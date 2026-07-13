package voice

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log"
	"sync/atomic"
	"time"

	"nhooyr.io/websocket"
)

// twilioEvent is a decoded message from the Twilio Media Streams WebSocket.
type twilioEvent struct {
	Event     string `json:"event"`
	StreamSID string `json:"streamSid"`
	Start     struct {
		CallSID   string `json:"callSid"`
		StreamSID string `json:"streamSid"`
	} `json:"start"`
	Media struct {
		Track     string `json:"track"`
		Payload   string `json:"payload"` // base64 mulaw
		Timestamp string `json:"timestamp"`
	} `json:"media"`
}

// Session manages one AI voice call end-to-end.
type Session struct {
	callSID   string
	streamSID string

	conn       *websocket.Conn
	history    []Message
	redirectFn func(callSID, path string)

	speaking atomic.Bool // true while TTS audio is playing
	cancel   context.CancelFunc
}

// Run is the main event loop for a Twilio Media Streams session.
// It returns when the call ends, the context is cancelled, or an unrecoverable error occurs.
// redirectFn is called mid-call (after callSID is known) to redirect the caller via Twilio REST API.
func Run(ctx context.Context, conn *websocket.Conn, redirectFn func(callSID, path string)) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	s := &Session{conn: conn, cancel: cancel, redirectFn: redirectFn}

	// audioIn carries raw mulaw chunks from the caller to Deepgram
	audioIn := make(chan []byte, 128)

	// Start Deepgram STT
	transcripts, err := DeepgramSTT(ctx, audioIn)
	if err != nil {
		log.Printf("level=ERROR msg=\"voice session: deepgram start failed\" err=%v", err)
		// No callSID yet — closing the WebSocket causes Twilio to fall through
		// to the <Say>/<Redirect> fallback already in the TwiML response.
		return
	}

	// greeting is sent as soon as we have the callSID (on "start" event)
	greeted := false

	// transcript processing loop
	go func() {
		for t := range transcripts {
			if !t.IsFinal {
				// Interim result: if AI is speaking, barge-in
				if s.speaking.Load() {
					s.clearAudio(ctx)
				}
				continue
			}
			if t.Text == "" {
				continue
			}
			log.Printf("level=INFO msg=\"voice transcript\" call=%s text=%q", s.callSID, t.Text)
			s.handleTranscript(ctx, t.Text)
		}
	}()

	// Main WebSocket read loop (Twilio → us)
	for {
		_, msg, err := conn.Read(ctx)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("level=WARN msg=\"voice session: read error\" call=%s err=%v", s.callSID, err)
			}
			close(audioIn)
			return
		}

		var event twilioEvent
		if err := json.Unmarshal(msg, &event); err != nil {
			continue
		}

		switch event.Event {
		case "start":
			s.callSID = event.Start.CallSID
			s.streamSID = event.Start.StreamSID
			log.Printf("level=INFO msg=\"voice session started\" call=%s stream=%s", s.callSID, s.streamSID)
			if !greeted {
				greeted = true
				go s.sendGreeting(ctx)
			}

		case "media":
			if event.Media.Track != "inbound" {
				continue
			}
			audio, err := base64.StdEncoding.DecodeString(event.Media.Payload)
			if err != nil {
				continue
			}
			select {
			case audioIn <- audio:
			default: // drop if buffer full (burst)
			}

		case "stop":
			log.Printf("level=INFO msg=\"voice session ended\" call=%s", s.callSID)
			close(audioIn)
			return
		}
	}
}

// sendGreeting plays the opening message when a call connects.
func (s *Session) sendGreeting(ctx context.Context) {
	greeting := "Thank you for calling Minnesota EquiVoice Partnership's free V I T A tax service. How can I help you today?"
	s.history = append(s.history, Message{Role: "assistant", Content: greeting})
	s.speakText(ctx, greeting)
}

// handleTranscript processes a final transcript from the caller.
func (s *Session) handleTranscript(ctx context.Context, text string) {
	// Stop any in-progress TTS
	if s.speaking.Load() {
		s.clearAudio(ctx)
	}

	phrases, err := RespondToTranscript(ctx, &s.history, text)
	if err != nil {
		log.Printf("level=WARN msg=\"voice: LLM error\" call=%s err=%v", s.callSID, err)
		s.speakText(ctx, "I'm having trouble connecting. Let me transfer you to our phone menu.")
		time.Sleep(2 * time.Second)
		s.redirectFn(s.callSID, "/voice/play")
		s.cancel()
		return
	}

	for phrase := range phrases {
		if ctx.Err() != nil {
			return
		}
		// Check for transfer/voicemail/SMS-consent intent
		if RequestsTransfer(phrase) {
			s.speakText(ctx, phrase)
			time.Sleep(500 * time.Millisecond)
			s.transferToQueue(ctx)
			return
		}
		if RequestsVoicemail(phrase) {
			s.speakText(ctx, phrase)
			time.Sleep(500 * time.Millisecond)
			s.redirectToVoicemail(ctx)
			return
		}
		if RequestsSMSLink(phrase) {
			s.speakText(ctx, phrase)
			time.Sleep(500 * time.Millisecond)
			s.redirectToSMSConsent(ctx)
			return
		}
		s.speakText(ctx, phrase)
	}
}

// speakText synthesizes text and streams the audio to the caller via Twilio.
func (s *Session) speakText(ctx context.Context, text string) {
	if text == "" || ctx.Err() != nil {
		return
	}

	audio, err := Synthesize(ctx, text)
	if err != nil {
		log.Printf("level=WARN msg=\"voice: TTS error\" call=%s err=%v", s.callSID, err)
		return
	}

	s.speaking.Store(true)
	defer s.speaking.Store(false)

	// Twilio expects audio in 20ms chunks (160 bytes of mulaw at 8000 Hz)
	const chunkSize = 160
	for i := 0; i < len(audio) && ctx.Err() == nil; i += chunkSize {
		end := i + chunkSize
		if end > len(audio) {
			end = len(audio)
		}
		chunk := audio[i:end]
		payload := base64.StdEncoding.EncodeToString(chunk)
		msg, _ := json.Marshal(map[string]any{
			"event":     "media",
			"streamSid": s.streamSID,
			"media":     map[string]string{"payload": payload},
		})
		if err := s.conn.Write(ctx, websocket.MessageText, msg); err != nil {
			return
		}
	}

	// Send mark so we know when playback finishes
	markMsg, _ := json.Marshal(map[string]any{
		"event":     "mark",
		"streamSid": s.streamSID,
		"mark":      map[string]string{"name": "done"},
	})
	_ = s.conn.Write(ctx, websocket.MessageText, markMsg)
}

// clearAudio sends a Twilio "clear" event to stop buffered audio (barge-in).
func (s *Session) clearAudio(ctx context.Context) {
	s.speaking.Store(false)
	msg, _ := json.Marshal(map[string]any{
		"event":     "clear",
		"streamSid": s.streamSID,
	})
	_ = s.conn.Write(ctx, websocket.MessageText, msg)
}

// transferToQueue redirects the active call to the volunteer queue via Twilio REST.
func (s *Session) transferToQueue(_ context.Context) {
	log.Printf("level=INFO msg=\"voice: transferring to queue\" call=%s", s.callSID)
	s.redirectFn(s.callSID, "/voice/transfer")
	s.cancel()
}

// redirectToVoicemail redirects the caller to the voicemail prompt.
func (s *Session) redirectToVoicemail(_ context.Context) {
	log.Printf("level=INFO msg=\"voice: redirecting to voicemail\" call=%s", s.callSID)
	s.redirectFn(s.callSID, "/voice/voicemail")
	s.cancel()
}

// redirectToSMSConsent hands the caller off to the A2P-compliant SMS consent IVR.
func (s *Session) redirectToSMSConsent(_ context.Context) {
	log.Printf("level=INFO msg=\"voice: redirecting to SMS consent\" call=%s", s.callSID)
	s.redirectFn(s.callSID, "/voice/sms-consent")
	s.cancel()
}
