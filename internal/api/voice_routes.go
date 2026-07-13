package api

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
)

// vitaForceCloseUntil holds a Unix timestamp; when non-zero and in the future,
// all calls are treated as after-hours regardless of the schedule.
// Auto-expires at midnight CT so each day starts fresh independently.
var vitaForceCloseUntil atomic.Int64

const vitaBookingURLDefault = "https://outlook.office.com/book/MNEVPVITATCE@mnequivoicepartnership.org/"

func vitaBookingURL() string {
	if v := strings.TrimSpace(os.Getenv("VITA_BOOKING_URL")); v != "" {
		return v
	}
	return vitaBookingURLDefault
}

func registerVoiceRoutes(r chi.Router) {
	r.Post("/voice/inbound", vitaVoiceInboundHandler())
	r.Post("/voice/play", vitaVoicePlayHandler())
	r.Post("/voice/menu", vitaVoiceMenuHandler())
	r.Post("/voice/transfer", vitaVoiceTransferHandler())
	r.Post("/voice/voicemail", vitaVoiceVoicemailPromptHandler())
	r.Post("/voice/voicemail-done", vitaVoiceVoicemailDoneHandler())
	r.Post("/voice/queue-fallback", vitaVoiceQueueFallbackHandler())
	r.Get("/voice/vita/toggle", vitaQueueToggleHandler())
	// AI voice stream routes
	r.Post("/voice/stream", vitaStreamTwiMLHandler())
	r.Get("/voice/stream/ws", vitaStreamWSHandler())
}

func twiml(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	fmt.Fprintf(w, "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<Response>\n%s\n</Response>", body)
}

const vitaMessage = `Thank you for calling Minnesota EquiVoice Partnership's free V I T A tax preparation service. ` +
	`We have just sent a scheduling link to your phone. ` +
	`Our sites are open on Saturdays. ` +
	`The first and second Saturdays of each month we are at Saint Paul Public Library. ` +
	`The last two Saturdays of each month we are at Rondo Community Library. ` +
	`Virtual appointments and document drop-off are also available. ` +
	`Press 1 to hear this message again, press 2 to leave a voicemail, or press 3 to speak with a V I T A volunteer.`

// isFederalHoliday reports whether t falls on a US federal holiday (America/Chicago).
func isFederalHoliday(t time.Time) bool {
	loc, _ := time.LoadLocation("America/Chicago")
	ct := t.In(loc)
	m, d, wd := ct.Month(), ct.Day(), ct.Weekday()
	switch {
	case m == time.January && d == 1:                                     // New Year's Day
		return true
	case m == time.January && wd == time.Monday && d >= 15 && d <= 21:   // MLK Day (3rd Mon)
		return true
	case m == time.February && wd == time.Monday && d >= 15 && d <= 21:  // Presidents Day (3rd Mon)
		return true
	case m == time.May && wd == time.Monday && d >= 25:                   // Memorial Day (last Mon)
		return true
	case m == time.June && d == 19:                                        // Juneteenth
		return true
	case m == time.July && d == 4:                                         // Independence Day
		return true
	case m == time.September && wd == time.Monday && d <= 7:              // Labor Day (1st Mon)
		return true
	case m == time.October && wd == time.Monday && d >= 8 && d <= 14:    // Columbus Day (2nd Mon)
		return true
	case m == time.November && d == 11:                                    // Veterans Day
		return true
	case m == time.November && wd == time.Thursday && d >= 22 && d <= 28: // Thanksgiving (4th Thu)
		return true
	case m == time.December && d == 25:                                    // Christmas
		return true
	}
	return false
}

// isVITAHours reports whether t falls within VITA service hours: Sat–Sun 9:00–17:00 CT.
// Returns false if the queue has been manually force-closed (auto-expires at midnight CT).
func isVITAHours(t time.Time) bool {
	if until := vitaForceCloseUntil.Load(); until > 0 && t.Unix() < until {
		return false
	}
	loc, _ := time.LoadLocation("America/Chicago")
	ct := t.In(loc)
	wd := ct.Weekday()
	return (wd == time.Saturday || wd == time.Sunday) && ct.Hour() >= 9 && ct.Hour() < 17
}

// vitaVoiceInboundHandler handles the initial call: sends booking SMS then plays IVR.
func vitaVoiceInboundHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		from := strings.TrimSpace(r.FormValue("From"))
		if from != "" {
			tc := loadTwilioConfig()
			if tc.AccountSID != "" && tc.AuthToken != "" && tc.FromNumber != "" {
				smsBody := fmt.Sprintf(
					"EquiVoice free VITA tax prep — schedule your appointment:\n%s\n\n"+
						"Saturdays at St. Paul Public Library (1st & 2nd Sat) or Rondo Community Library (last 2 Sat). "+
						"Virtual & drop-off also available.",
					vitaBookingURL(),
				)
				if err := sendSMS(tc.AccountSID, tc.AuthToken, tc.FromNumber, from, smsBody); err != nil {
					log.Printf("level=WARN msg=\"VITA voice: failed to send booking SMS\" to=%s err=%v", from, err)
				}
			}
		}

		now := time.Now()

		// AI path: when enabled and during VITA hours, hand off to Media Stream
		if vitaAIEnabled.Load() && isVITAHours(now) {
			twiml(w, fmt.Sprintf(
				`  <Connect>`+
					`<Stream url="%s" track="inbound_track"/>`+
					`</Connect>`+
					`  <Say voice="Polly.Joanna">I'm having trouble with our assistant. Let me take you to our regular menu.</Say>`+
					`  <Redirect method="POST">/voice/play</Redirect>`,
				streamWSURL(r),
			))
			return
		}

		if isFederalHoliday(now) {
			twiml(w,
				`  <Say voice="Polly.Joanna">Thank you for calling Minnesota EquiVoice Partnership. `+
					`Our V I T A sites are closed today in observance of the holiday. `+
					`We are open Saturdays from 9 A M to 5 P M. `+
					`Please leave a message after the tone and a volunteer will follow up with you.</Say>`+
					`  <Record action="/voice/voicemail-done" maxLength="120" finishOnKey="#" playBeep="true"/>`,
			)
			return
		}

		if !isVITAHours(now) {
			twiml(w,
				`  <Say voice="Polly.Joanna">Thank you for calling Minnesota EquiVoice Partnership's `+
					`free V I T A tax preparation service. `+
					`Our phone lines are open Saturdays from 9 A M to 5 P M. `+
					`A scheduling link has been sent to your phone. `+
					`You may also visit minnesota equivoice partnership dot org to schedule a virtual appointment. `+
					`Please leave a message after the tone and a volunteer will follow up with you.</Say>`+
					`  <Record action="/voice/voicemail-done" maxLength="120" finishOnKey="#" playBeep="true"/>`,
			)
			return
		}

		twiml(w, fmt.Sprintf(
			`  <Gather numDigits="1" action="/voice/menu" timeout="15" method="POST">`+
				`<Say voice="Polly.Joanna">%s</Say></Gather>`+
				`  <Say voice="Polly.Joanna">We did not receive your selection. Thank you for calling, and have a great day.</Say>`,
			vitaMessage,
		))
	}
}

// vitaVoicePlayHandler replays the IVR message without re-sending the SMS.
func vitaVoicePlayHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		twiml(w, fmt.Sprintf(
			`  <Gather numDigits="1" action="/voice/menu" timeout="15" method="POST">`+
				`<Say voice="Polly.Joanna">%s</Say></Gather>`+
				`  <Say voice="Polly.Joanna">We did not receive your selection. Thank you for calling, and have a great day.</Say>`,
			vitaMessage,
		))
	}
}

// vitaVoiceMenuHandler handles digit presses: 1 = replay, 2 = voicemail.
func vitaVoiceMenuHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		switch strings.TrimSpace(r.FormValue("Digits")) {
		case "1":
			twiml(w, `  <Redirect method="POST">/voice/play</Redirect>`)
		case "2":
			twiml(w, `  <Redirect method="POST">/voice/voicemail</Redirect>`)
		case "3":
			twiml(w,
				`  <Say voice="Polly.Joanna">Please hold while we connect you with a volunteer.</Say>`+
					`  <Dial timeout="30" action="/voice/queue-fallback" method="POST">`+
					`<Sip>sip:650@voice.mnequivoicepartnership.org</Sip></Dial>`,
			)
		default:
			twiml(w, `  <Say voice="Polly.Joanna">Thank you for calling. Goodbye.</Say>`)
		}
	}
}

// vitaVoiceTransferHandler directly connects the caller to the volunteer SIP queue.
// Used by the AI session when the caller requests a human — avoids relying on POST body params.
func vitaVoiceTransferHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		twiml(w,
			`  <Say voice="Polly.Joanna">Please hold while we connect you with a volunteer.</Say>`+
				`  <Dial timeout="30" action="/voice/queue-fallback" method="POST">`+
				`<Sip>sip:650@voice.mnequivoicepartnership.org</Sip></Dial>`,
		)
	}
}

// vitaVoiceVoicemailPromptHandler plays the voicemail prompt and starts recording.
func vitaVoiceVoicemailPromptHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		twiml(w,
			`  <Say voice="Polly.Joanna">Please leave your name, phone number, and a brief message after the tone. Press the pound key when finished.</Say>`+
				`  <Record action="/voice/voicemail-done" maxLength="120" finishOnKey="#" playBeep="true"/>`,
		)
	}
}

// vitaVoiceVoicemailDoneHandler handles the completed recording and notifies the admin.
func vitaVoiceVoicemailDoneHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		from := strings.TrimSpace(r.FormValue("From"))
		recordingURL := strings.TrimSpace(r.FormValue("RecordingUrl"))
		duration := strings.TrimSpace(r.FormValue("RecordingDuration"))

		log.Printf("level=INFO msg=\"VITA voicemail received\" from=%s duration=%ss url=%s", from, duration, recordingURL)

		notifyNumber := strings.TrimSpace(os.Getenv("VITA_VOICEMAIL_NOTIFY_NUMBER"))
		if notifyNumber != "" && recordingURL != "" {
			tc := loadTwilioConfig()
			if tc.AccountSID != "" && tc.AuthToken != "" && tc.FromNumber != "" {
				msg := fmt.Sprintf("VITA voicemail from %s (%ss): %s.mp3", from, duration, recordingURL)
				if err := sendSMS(tc.AccountSID, tc.AuthToken, tc.FromNumber, notifyNumber, msg); err != nil {
					log.Printf("level=WARN msg=\"VITA voicemail: failed to notify admin\" err=%v", err)
				}
			}
		}

		twiml(w, `  <Say voice="Polly.Joanna">Thank you for your message. Our team will follow up with you soon. Goodbye.</Say>`)
	}
}

// vitaQueueToggleHandler lets authorized staff force-close or reopen the VITA queue.
// GET /voice/vita/toggle?token=TOKEN&state=close|open|status
func vitaQueueToggleHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimSpace(os.Getenv("VITA_TOGGLE_TOKEN"))
		if token == "" || strings.TrimSpace(r.URL.Query().Get("token")) != token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		loc, _ := time.LoadLocation("America/Chicago")
		switch strings.TrimSpace(r.URL.Query().Get("state")) {
		case "close":
			now := time.Now().In(loc)
			midnight := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, loc)
			vitaForceCloseUntil.Store(midnight.Unix())
			fmt.Fprintf(w, "VITA queue: FORCED CLOSED until midnight CT (%s). Opens fresh tomorrow.\n", midnight.Format("Mon Jan 2"))
		case "open":
			vitaForceCloseUntil.Store(0)
			fmt.Fprintln(w, "VITA queue: OPEN — normal Sat/Sun schedule resumed.")
		case "ai-on":
			vitaAIEnabled.Store(true)
			fmt.Fprintln(w, "VITA AI voice: ENABLED — callers will reach the AI assistant during VITA hours.")
		case "ai-off":
			vitaAIEnabled.Store(false)
			fmt.Fprintln(w, "VITA AI voice: DISABLED — callers will reach the standard IVR menu.")
		default:
			until := vitaForceCloseUntil.Load()
			if until > 0 && time.Now().Unix() < until {
				expiry := time.Unix(until, 0).In(loc)
				fmt.Fprintf(w, "VITA queue status: FORCED CLOSED until %s\n", expiry.Format("Mon Jan 2 12:00 AM CT"))
			} else {
				fmt.Fprintln(w, "VITA queue status: following normal Sat/Sun 9am-5pm schedule")
			}
			if vitaAIEnabled.Load() {
				fmt.Fprintln(w, "VITA AI voice: ENABLED")
			} else {
				fmt.Fprintln(w, "VITA AI voice: DISABLED (IVR fallback active)")
			}
		}
	}
}

// vitaVoiceQueueFallbackHandler handles the case where the queue SIP transfer fails or times out.
func vitaVoiceQueueFallbackHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		twiml(w,
			`  <Say voice="Polly.Joanna">We're sorry, all volunteers are currently unavailable. `+
				`Please leave a message after the tone and a volunteer will follow up with you.</Say>`+
				`  <Record action="/voice/voicemail-done" maxLength="120" finishOnKey="#" playBeep="true"/>`,
		)
	}
}
