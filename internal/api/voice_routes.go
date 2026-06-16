package api

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/go-chi/chi/v5"
)

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
	r.Post("/voice/voicemail", vitaVoiceVoicemailPromptHandler())
	r.Post("/voice/voicemail-done", vitaVoiceVoicemailDoneHandler())
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
	`Press 1 to hear this message again, or press 2 to leave a voicemail for our team.`

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

		twiml(w, fmt.Sprintf(
			`  <Gather numDigits="1" action="/voice/menu" timeout="15" method="POST">`+
				`<Say voice="alice">%s</Say></Gather>`+
				`  <Say voice="alice">We did not receive your selection. Thank you for calling, and have a great day.</Say>`,
			vitaMessage,
		))
	}
}

// vitaVoicePlayHandler replays the IVR message without re-sending the SMS.
func vitaVoicePlayHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		twiml(w, fmt.Sprintf(
			`  <Gather numDigits="1" action="/voice/menu" timeout="15" method="POST">`+
				`<Say voice="alice">%s</Say></Gather>`+
				`  <Say voice="alice">We did not receive your selection. Thank you for calling, and have a great day.</Say>`,
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
		default:
			twiml(w, `  <Say voice="alice">Thank you for calling. Goodbye.</Say>`)
		}
	}
}

// vitaVoiceVoicemailPromptHandler plays the voicemail prompt and starts recording.
func vitaVoiceVoicemailPromptHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		twiml(w,
			`  <Say voice="alice">Please leave your name, phone number, and a brief message after the tone. Press the pound key when finished.</Say>`+
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

		twiml(w, `  <Say voice="alice">Thank you for your message. Our team will follow up with you soon. Goodbye.</Say>`)
	}
}
