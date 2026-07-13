package api

import (
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/adevireddy/colosseum/internal/voice"
	"nhooyr.io/websocket"
)

// vitaAIEnabled gates the AI voice path. Off by default; enabled via toggle endpoint.
var vitaAIEnabled atomic.Bool

// vitaStreamTwiMLHandler returns TwiML that connects the call to our WebSocket AI.
func vitaStreamTwiMLHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		wsURL := streamWSURL(r)
		twiml(w, fmt.Sprintf(
			`  <Connect>`+
				`<Stream url="%s" track="inbound_track"/>`+
				`</Connect>`+
				`  <Say voice="Polly.Joanna">I'm having trouble connecting our assistant. Let me take you to our regular menu.</Say>`+
				`  <Redirect method="POST">/voice/play</Redirect>`,
			wsURL,
		))
	}
}

// vitaStreamWSHandler accepts the Twilio WebSocket and runs the AI session.
func vitaStreamWSHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			InsecureSkipVerify: false,
			OriginPatterns:     []string{"*.twilio.com"},
		})
		if err != nil {
			log.Printf("level=ERROR msg=\"voice stream: websocket accept failed\" err=%v", err)
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "done")

		ctx := r.Context()
		voice.Run(ctx, conn, redirectActiveCall)
	}
}

// streamWSURL builds the wss:// URL for the Twilio Media Stream connection.
func streamWSURL(r *http.Request) string {
	if v := strings.TrimSpace(os.Getenv("VITA_STREAM_WS_URL")); v != "" {
		return v
	}
	host := r.Host
	return "wss://" + host + "/voice/stream/ws"
}

// redirectActiveCall uses the Twilio REST API to redirect an active call to a new TwiML URL.
func redirectActiveCall(callSID, path string) {
	tc := loadTwilioConfig()
	if tc.AccountSID == "" || tc.AuthToken == "" {
		return
	}

	baseURL := strings.TrimSpace(os.Getenv("TWILIO_VOICE_WEBHOOK_BASE"))
	if baseURL == "" {
		baseURL = "https://colosseum.mnequivoicepartnership.org"
	}
	redirectURL := baseURL + path

	apiURL := fmt.Sprintf("https://api.twilio.com/2010-04-01/Accounts/%s/Calls/%s.json",
		tc.AccountSID, callSID)

	form := url.Values{}
	form.Set("Url", redirectURL)
	form.Set("Method", "POST")

	req, err := http.NewRequest(http.MethodPost, apiURL, strings.NewReader(form.Encode()))
	if err != nil {
		log.Printf("level=WARN msg=\"redirectActiveCall: build request failed\" err=%v", err)
		return
	}
	req.SetBasicAuth(tc.AccountSID, tc.AuthToken)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("level=WARN msg=\"redirectActiveCall: request failed\" call=%s err=%v", callSID, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		log.Printf("level=WARN msg=\"redirectActiveCall: unexpected status\" call=%s status=%d", callSID, resp.StatusCode)
	}
}
