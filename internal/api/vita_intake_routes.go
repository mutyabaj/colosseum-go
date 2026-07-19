package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
)

// vitaIntakeConfig holds credentials read from environment variables.
// The Supabase service role key never leaves this server — it is never
// passed to an agent or included in a system prompt. Agents reach this
// endpoint instead, authenticated by a much lower-privilege shared token.
type vitaIntakeConfig struct {
	SupabaseURL        string // SUPABASE_URL
	SupabaseServiceKey string // SUPABASE_SERVICE_ROLE_KEY
	InternalToken      string // VITA_INTAKE_TOKEN — shared secret for this endpoint only
}

func loadVitaIntakeConfig() vitaIntakeConfig {
	return vitaIntakeConfig{
		SupabaseURL:        strings.TrimRight(os.Getenv("SUPABASE_URL"), "/"),
		SupabaseServiceKey: os.Getenv("SUPABASE_SERVICE_ROLE_KEY"),
		InternalToken:      os.Getenv("VITA_INTAKE_TOKEN"),
	}
}

func (vc vitaIntakeConfig) ready() bool {
	return vc.SupabaseURL != "" && vc.SupabaseServiceKey != ""
}

// validAnswerValues are the only accepted values for each income/expenses/
// life_events entry. Plain booleans can't distinguish "client said no" from
// "conversation was interrupted before this was asked" - both would
// otherwise silently save as false.
var validAnswerValues = map[string]bool{
	"yes":          true,
	"no":           true,
	"unsure":       true,
	"not_answered": true,
}

var validStatusValues = map[string]bool{
	"started":     true,
	"partial":     true,
	"completed":   true,
	"save_failed": true,
	"reviewed":    true,
}

// vitaIntakeRequest mirrors the client_intake table columns.
type vitaIntakeRequest struct {
	Phone                string            `json:"phone"`
	FirstName            string            `json:"first_name"`
	AppointmentDate      string            `json:"appointment_date"` // YYYY-MM-DD
	SiteLocation         string            `json:"site_location,omitempty"`
	TaxYear              int               `json:"tax_year,omitempty"`
	QuestionnaireVersion string            `json:"questionnaire_version,omitempty"`
	Status               string            `json:"status,omitempty"`
	Income               map[string]string `json:"income,omitempty"`
	Expenses             map[string]string `json:"expenses,omitempty"`
	LifeEvents           map[string]string `json:"life_events,omitempty"`
}

func validateAnswerMap(name string, m map[string]string) error {
	for k, v := range m {
		if !validAnswerValues[v] {
			return fmt.Errorf("%s.%s: invalid value %q (must be yes, no, unsure, or not_answered)", name, k, v)
		}
	}
	return nil
}

func (req vitaIntakeRequest) validate() error {
	if strings.TrimSpace(req.Phone) == "" {
		return fmt.Errorf("phone is required")
	}
	if strings.TrimSpace(req.FirstName) == "" {
		return fmt.Errorf("first_name is required")
	}
	if strings.TrimSpace(req.AppointmentDate) == "" {
		return fmt.Errorf("appointment_date is required (YYYY-MM-DD)")
	}
	if req.Status != "" && !validStatusValues[req.Status] {
		return fmt.Errorf("status: invalid value %q", req.Status)
	}
	if err := validateAnswerMap("income", req.Income); err != nil {
		return err
	}
	if err := validateAnswerMap("expenses", req.Expenses); err != nil {
		return err
	}
	if err := validateAnswerMap("life_events", req.LifeEvents); err != nil {
		return err
	}
	return nil
}

// vitaIntakeHandler handles POST /internal/vita-intake.
// Called by the VITA-Intake-Assistant agent's http.request tool after it
// finishes walking a client through the Form 13614-C Parts III-V
// pre-appointment questions. Writes one row into the client_intake table
// in the MEP Volunteer App's Supabase project via the service role key,
// which is read from the environment here and never exposed to the agent.
func vitaIntakeHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vc := loadVitaIntakeConfig()
		if !vc.ready() {
			log.Printf("level=WARN msg=\"vita-intake: not configured (supabase_url=%v supabase_key=%v), rejecting\"",
				vc.SupabaseURL != "", vc.SupabaseServiceKey != "")
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "vita intake not configured"})
			return
		}

		if vc.InternalToken != "" {
			got := r.Header.Get("X-Vita-Intake-Token")
			if got == "" || got != vc.InternalToken {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
				return
			}
		}

		var req vitaIntakeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		if err := req.validate(); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if req.Income == nil {
			req.Income = map[string]string{}
		}
		if req.Expenses == nil {
			req.Expenses = map[string]string{}
		}
		if req.LifeEvents == nil {
			req.LifeEvents = map[string]string{}
		}
		if req.Status == "" {
			req.Status = "completed"
		}

		body, err := json.Marshal(req)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "encode failed"})
			return
		}

		supabaseURL := vc.SupabaseURL + "/rest/v1/client_intake"
		sbReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, supabaseURL, bytes.NewReader(body))
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "request build failed"})
			return
		}
		sbReq.Header.Set("Content-Type", "application/json")
		sbReq.Header.Set("apikey", vc.SupabaseServiceKey)
		sbReq.Header.Set("Authorization", "Bearer "+vc.SupabaseServiceKey)
		sbReq.Header.Set("Prefer", "return=minimal")

		resp, err := http.DefaultClient.Do(sbReq)
		if err != nil {
			log.Printf("level=ERROR msg=\"vita-intake: supabase request failed\" err=%v", err)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "supabase request failed"})
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode >= 300 {
			respBody, _ := io.ReadAll(resp.Body)
			log.Printf("level=ERROR msg=\"vita-intake: supabase insert failed\" status=%d body=%s", resp.StatusCode, string(respBody))
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "supabase insert failed"})
			return
		}

		log.Printf("level=INFO msg=\"vita-intake: saved\" phone=%s appointment_date=%s site=%s",
			req.Phone, req.AppointmentDate, req.SiteLocation)
		writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
	}
}
