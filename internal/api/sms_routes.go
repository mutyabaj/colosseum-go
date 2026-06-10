package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// twilioConfig holds credentials read from environment variables.
type twilioConfig struct {
	AccountSID string // TWILIO_ACCOUNT_SID
	AuthToken  string // TWILIO_AUTH_TOKEN
	FromNumber string // TWILIO_FROM_NUMBER  e.g. +16513027258
	AgentID    string // TWILIO_SMS_AGENT_ID — which Colosseum agent handles SMS
}

func loadTwilioConfig() twilioConfig {
	return twilioConfig{
		AccountSID: os.Getenv("TWILIO_ACCOUNT_SID"),
		AuthToken:  os.Getenv("TWILIO_AUTH_TOKEN"),
		FromNumber: os.Getenv("TWILIO_FROM_NUMBER"),
		AgentID:    os.Getenv("TWILIO_SMS_AGENT_ID"),
	}
}

func (tc twilioConfig) ready() bool {
	return tc.AccountSID != "" && tc.AuthToken != "" && tc.FromNumber != "" && tc.AgentID != ""
}

// validateTwilioSignature verifies the X-Twilio-Signature header.
// See: https://www.twilio.com/docs/usage/webhooks/webhooks-security
func validateTwilioSignature(authToken, fullURL, signature string, params map[string]string) bool {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	s := fullURL
	for _, k := range keys {
		s += k + params[k]
	}
	mac := hmac.New(sha1.New, []byte(authToken))
	mac.Write([]byte(s))
	expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(signature))
}

// sendSMS sends an outbound SMS via the Twilio Messages REST API.
func sendSMS(accountSID, authToken, from, to, body string) error {
	// Twilio SMS has a 1600-character limit.
	if len(body) > 1600 {
		body = body[:1597] + "..."
	}
	form := url.Values{}
	form.Set("From", from)
	form.Set("To", to)
	form.Set("Body", body)

	apiURL := fmt.Sprintf("https://api.twilio.com/2010-04-01/Accounts/%s/Messages.json", accountSID)
	req, err := http.NewRequest(http.MethodPost, apiURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.SetBasicAuth(accountSID, authToken)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("twilio send failed status=%d body=%s", resp.StatusCode, string(b))
	}
	return nil
}

// getOrCreateSMSSession returns an existing chat session for (phone, agentID) or creates one.
func getOrCreateSMSSession(ctx context.Context, db *sql.DB, phone, agentID string) (string, error) {
	var sessionID string
	err := db.QueryRowContext(ctx,
		`SELECT session_id FROM sms_sessions WHERE phone_number=? AND agent_id=?`,
		phone, agentID,
	).Scan(&sessionID)
	if err == nil {
		return sessionID, nil
	}
	if err != sql.ErrNoRows {
		return "", err
	}

	// Create new chat session.
	sessionID = uuid.NewString()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO chat_sessions(id,title,agent_id,status,created_at,updated_at)
		VALUES(?,?,?,?,?,?)
	`, sessionID, "SMS: "+phone, agentID, "active", now, now); err != nil {
		return "", err
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO sms_sessions(phone_number,agent_id,session_id,created_at,updated_at)
		VALUES(?,?,?,?,?)
	`, phone, agentID, sessionID, now, now); err != nil {
		return "", err
	}
	return sessionID, nil
}

// waitForRunResult polls until the run completes and returns the agent's reply text.
func waitForRunResult(ctx context.Context, db *sql.DB, runID string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(2 * time.Second):
		}

		var payload string
		err := db.QueryRowContext(ctx,
			`SELECT payload_json FROM events WHERE run_id=? AND event_type='run.completed' LIMIT 1`,
			runID,
		).Scan(&payload)
		if err == sql.ErrNoRows {
			// Also check for failure.
			var failPayload string
			if ferr := db.QueryRowContext(ctx,
				`SELECT payload_json FROM events WHERE run_id=? AND event_type='run.failed' LIMIT 1`,
				runID,
			).Scan(&failPayload); ferr == nil {
				var p struct{ Error string `json:"error"` }
				_ = json.Unmarshal([]byte(failPayload), &p)
				if p.Error != "" {
					return p.Error, nil
				}
				return "Sorry, something went wrong. Please try again.", nil
			}
			continue
		}
		if err != nil {
			return "", err
		}
		var p struct{ Result string `json:"result"` }
		if err := json.Unmarshal([]byte(payload), &p); err != nil {
			return "", err
		}
		return p.Result, nil
	}
	return "Your request is taking longer than expected. Please try again in a moment.", nil
}

// smsInboundHandler handles POST /sms/inbound from Twilio.
func smsInboundHandler(db *sql.DB, workspaceRoot string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tc := loadTwilioConfig()
		if !tc.ready() {
			log.Printf("level=WARN msg=\"SMS inbound: Twilio not configured, dropping request\"")
			w.WriteHeader(http.StatusNoContent)
			return
		}

		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		// Validate Twilio signature.
		scheme := "https"
		if r.TLS == nil && r.Header.Get("X-Forwarded-Proto") != "https" {
			scheme = "http"
		}
		fullURL := fmt.Sprintf("%s://%s%s", scheme, r.Host, r.URL.Path)
		params := map[string]string{}
		for k, v := range r.PostForm {
			if len(v) > 0 {
				params[k] = v[0]
			}
		}
		sig := r.Header.Get("X-Twilio-Signature")
		if sig != "" && !validateTwilioSignature(tc.AuthToken, fullURL, sig, params) {
			log.Printf("level=WARN msg=\"SMS inbound: invalid Twilio signature from %s\"", r.RemoteAddr)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		from := strings.TrimSpace(r.FormValue("From"))
		body := strings.TrimSpace(r.FormValue("Body"))

		if from == "" || body == "" {
			w.Header().Set("Content-Type", "text/xml")
			fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?><Response/>`)
			return
		}

		log.Printf("level=INFO msg=\"SMS inbound\" from=%s body_len=%d", from, len(body))

		// Respond immediately so Twilio doesn't time out — reply comes async.
		w.Header().Set("Content-Type", "text/xml")
		fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?><Response/>`)

		// Process and reply asynchronously.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()

			sessionID, err := getOrCreateSMSSession(ctx, db, from, tc.AgentID)
			if err != nil {
				log.Printf("level=ERROR msg=\"SMS session error\" from=%s err=%v", from, err)
				_ = sendSMS(tc.AccountSID, tc.AuthToken, tc.FromNumber, from,
					"Sorry, we encountered an error. Please try again.")
				return
			}

			runID, _, err := createRunForSessionMessage(ctx, db, workspaceRoot, sessionID, body, "queued")
			if err != nil {
				log.Printf("level=ERROR msg=\"SMS run error\" from=%s err=%v", from, err)
				_ = sendSMS(tc.AccountSID, tc.AuthToken, tc.FromNumber, from,
					"Sorry, we encountered an error. Please try again.")
				return
			}

			reply, err := waitForRunResult(ctx, db, runID, 4*time.Minute)
			if err != nil {
				log.Printf("level=ERROR msg=\"SMS wait error\" from=%s run=%s err=%v", from, runID, err)
				reply = "Sorry, the request timed out. Please try again."
			}

			if err := sendSMS(tc.AccountSID, tc.AuthToken, tc.FromNumber, from, reply); err != nil {
				log.Printf("level=ERROR msg=\"SMS send error\" from=%s err=%v", from, err)
			} else {
				log.Printf("level=INFO msg=\"SMS reply sent\" from=%s run=%s", from, runID)
			}
		}()
	}
}
