package voice

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync"

	"nhooyr.io/websocket"
)

// Transcript is a finalized or interim STT result from Deepgram.
type Transcript struct {
	Text     string
	IsFinal  bool
}

// DeepgramSTT streams audio to Deepgram and returns a channel of transcripts.
// The caller sends mulaw 8000 Hz audio chunks to audioIn; close audioIn to end the session.
// Transcripts arrive on the returned channel until the session ends.
func DeepgramSTT(ctx context.Context, audioIn <-chan []byte) (<-chan Transcript, error) {
	apiKey := os.Getenv("DEEPGRAM_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("DEEPGRAM_API_KEY not set")
	}

	url := "wss://api.deepgram.com/v1/listen" +
		"?encoding=mulaw" +
		"&sample_rate=8000" +
		"&channels=1" +
		"&model=nova-2" +
		"&endpointing=300" +
		"&utterance_end_ms=1000" +
		"&interim_results=true" +
		"&smart_format=true"

	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader: map[string][]string{
			"Authorization": {"Token " + apiKey},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("deepgram dial: %w", err)
	}

	out := make(chan Transcript, 32)

	var wg sync.WaitGroup

	// Sender: push audio chunks to Deepgram
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			// Send close message to Deepgram
			_ = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"CloseStream"}`))
		}()
		for chunk := range audioIn {
			if err := conn.Write(ctx, websocket.MessageBinary, chunk); err != nil {
				if ctx.Err() == nil {
					log.Printf("level=WARN msg=\"deepgram write error\" err=%v", err)
				}
				return
			}
		}
	}()

	// Receiver: read transcripts from Deepgram
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(out)
		for {
			_, msg, err := conn.Read(ctx)
			if err != nil {
				if ctx.Err() == nil {
					log.Printf("level=WARN msg=\"deepgram read error\" err=%v", err)
				}
				return
			}
			var result deepgramResult
			if err := json.Unmarshal(msg, &result); err != nil {
				continue
			}
			if result.Type == "Results" && len(result.Channel.Alternatives) > 0 {
				text := result.Channel.Alternatives[0].Transcript
				if text == "" {
					continue
				}
				select {
				case out <- Transcript{Text: text, IsFinal: result.IsFinal}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	// Closer: close connection when both goroutines finish
	go func() {
		wg.Wait()
		conn.Close(websocket.StatusNormalClosure, "done")
	}()

	return out, nil
}

type deepgramResult struct {
	Type    string `json:"type"`
	IsFinal bool   `json:"is_final"`
	Channel struct {
		Alternatives []struct {
			Transcript string  `json:"transcript"`
			Confidence float64 `json:"confidence"`
		} `json:"alternatives"`
	} `json:"channel"`
}
