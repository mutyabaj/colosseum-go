package api

import (
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"net/smtp"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
)

// jlmConfig holds credentials for the JLM (bookkeeping prospecting) integration,
// read from environment variables. The internal token and Zoho SMTP password
// never leave this server — they are never passed to an agent or included in
// a system prompt. Agents reach these endpoints instead, with the shared
// secret baked into the calling tool's config_json, invisible to the agent.
type jlmConfig struct {
	InternalToken string // JLM_INTERNAL_TOKEN — shared secret for /internal/jlm-* endpoints
	ZohoUser      string // ZOHO_SMTP_USER
	ZohoPassword  string // ZOHO_SMTP_PASSWORD (app-specific password)
	ZohoFrom      string // ZOHO_SMTP_FROM (e.g. outreach@jlmriskmanagement.com)
}

func loadJLMConfig() jlmConfig {
	return jlmConfig{
		InternalToken: os.Getenv("JLM_INTERNAL_TOKEN"),
		ZohoUser:      os.Getenv("ZOHO_SMTP_USER"),
		ZohoPassword:  os.Getenv("ZOHO_SMTP_PASSWORD"),
		ZohoFrom:      os.Getenv("ZOHO_SMTP_FROM"),
	}
}

func checkJLMToken(w http.ResponseWriter, r *http.Request, token string) bool {
	if token == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "jlm endpoint not configured"})
		return false
	}
	got := r.Header.Get("X-Jlm-Internal-Token")
	if got == "" || got != token {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return false
	}
	return true
}

// jlmProspectRequest mirrors the jlm_prospects table columns.
type jlmProspectRequest struct {
	RunID                 string `json:"run_id"`
	OrganizationName      string `json:"organization_name"`
	EIN                   string `json:"ein,omitempty"`
	Location              string `json:"location,omitempty"`
	Mission               string `json:"mission,omitempty"`
	AnnualRevenue         string `json:"annual_revenue,omitempty"`
	Latest990Year         string `json:"latest_990_year,omitempty"`
	Latest990Summary      string `json:"latest_990_summary,omitempty"`
	Website               string `json:"website,omitempty"`
	ExecutiveDirector     string `json:"executive_director,omitempty"`
	ContactEmail          string `json:"contact_email,omitempty"`
	ContactPhone          string `json:"contact_phone,omitempty"`
	AccountingIndicators  string `json:"accounting_indicators,omitempty"`
	WhyProspect           string `json:"why_prospect,omitempty"`
	DraftOutreachSubject  string `json:"draft_outreach_subject,omitempty"`
	DraftOutreachBody     string `json:"draft_outreach_body,omitempty"`
	Status                string `json:"status,omitempty"`
}

// jlmSaveProspectHandler handles POST /internal/jlm-prospects.
// Called by the JLM prospecting agent's dedicated http_tool after it has
// researched, qualified, and drafted outreach for one organization.
func jlmSaveProspectHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := loadJLMConfig()
		if !checkJLMToken(w, r, cfg.InternalToken) {
			return
		}

		var req jlmProspectRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		if strings.TrimSpace(req.RunID) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "run_id is required"})
			return
		}
		if strings.TrimSpace(req.OrganizationName) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "organization_name is required"})
			return
		}
		if req.Status == "" {
			req.Status = "researched"
		}

		id := uuid.NewString()
		now := time.Now().UTC().Format(time.RFC3339Nano)
		_, err := db.ExecContext(r.Context(), `
			INSERT INTO jlm_prospects(
				id, run_id, organization_name, ein, location, mission, annual_revenue,
				latest_990_year, latest_990_summary, website, executive_director,
				contact_email, contact_phone, accounting_indicators, why_prospect,
				draft_outreach_subject, draft_outreach_body, status, created_at, updated_at
			) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(organization_name) DO UPDATE SET
				run_id=excluded.run_id, ein=excluded.ein, location=excluded.location,
				mission=excluded.mission, annual_revenue=excluded.annual_revenue,
				latest_990_year=excluded.latest_990_year, latest_990_summary=excluded.latest_990_summary,
				website=excluded.website, executive_director=excluded.executive_director,
				contact_email=excluded.contact_email, contact_phone=excluded.contact_phone,
				accounting_indicators=excluded.accounting_indicators, why_prospect=excluded.why_prospect,
				draft_outreach_subject=excluded.draft_outreach_subject, draft_outreach_body=excluded.draft_outreach_body,
				status=excluded.status, updated_at=excluded.updated_at
		`, id, req.RunID, req.OrganizationName, req.EIN, req.Location, req.Mission, req.AnnualRevenue,
			req.Latest990Year, req.Latest990Summary, req.Website, req.ExecutiveDirector,
			req.ContactEmail, req.ContactPhone, req.AccountingIndicators, req.WhyProspect,
			req.DraftOutreachSubject, req.DraftOutreachBody, req.Status, now, now)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "saved", "organization_name": req.OrganizationName})
	}
}

// jlmCheckProspectHandler handles POST /internal/jlm-prospects/search.
// Called by the agent before researching an organization, to honor
// "exclude organizations already researched."
func jlmCheckProspectHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := loadJLMConfig()
		if !checkJLMToken(w, r, cfg.InternalToken) {
			return
		}
		var req struct {
			OrganizationName string `json:"organization_name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		if strings.TrimSpace(req.OrganizationName) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "organization_name is required"})
			return
		}
		var count int
		err := db.QueryRowContext(r.Context(),
			`SELECT COUNT(1) FROM jlm_prospects WHERE organization_name = ? COLLATE NOCASE`,
			req.OrganizationName).Scan(&count)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"exists": count > 0})
	}
}

// jlmListOrgNamesHandler handles POST /internal/jlm-prospects/list-names.
// Returns every already-researched organization name in one call, so the
// agent can check its whole candidate list against the dataset up front
// instead of one lookup per organization.
func jlmListOrgNamesHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := loadJLMConfig()
		if !checkJLMToken(w, r, cfg.InternalToken) {
			return
		}
		rows, err := db.QueryContext(r.Context(), `SELECT organization_name FROM jlm_prospects ORDER BY organization_name`)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		defer rows.Close()
		names := []string{}
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err == nil {
				names = append(names, n)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"organization_names": names})
	}
}

// jlmExportProspectsHandler handles GET /internal/jlm-prospects/export?run_id=...
// Returns a CSV of the given run (or, with no run_id, everything), fetched
// by the OneDrive-delivery step after a scheduled run completes.
func jlmExportProspectsHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := loadJLMConfig()
		if !checkJLMToken(w, r, cfg.InternalToken) {
			return
		}
		runID := r.URL.Query().Get("run_id")
		var rows *sql.Rows
		var err error
		cols := `organization_name, ein, location, mission, annual_revenue, latest_990_year,
			latest_990_summary, website, executive_director, contact_email, contact_phone,
			accounting_indicators, why_prospect, draft_outreach_subject, draft_outreach_body, status, created_at`
		if runID != "" {
			rows, err = db.QueryContext(r.Context(), `SELECT `+cols+` FROM jlm_prospects WHERE run_id = ? ORDER BY created_at`, runID)
		} else {
			rows, err = db.QueryContext(r.Context(), `SELECT `+cols+` FROM jlm_prospects ORDER BY created_at`)
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		defer rows.Close()

		w.Header().Set("Content-Type", "text/csv")
		w.Header().Set("Content-Disposition", `attachment; filename="jlm-prospects.csv"`)
		cw := csv.NewWriter(w)
		_ = cw.Write([]string{"Organization", "EIN", "Location", "Mission", "Annual Revenue",
			"Latest 990 Year", "Latest 990 Summary", "Website", "Executive Director", "Contact Email",
			"Contact Phone", "Accounting Indicators", "Why Prospect", "Draft Subject", "Draft Body",
			"Status", "Created At"})
		for rows.Next() {
			var vals [17]sql.NullString
			ptrs := make([]any, 17)
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				continue
			}
			record := make([]string, 17)
			for i, v := range vals {
				record[i] = v.String
			}
			_ = cw.Write(record)
		}
		cw.Flush()
	}
}

// jlmOutreachEmailHandler handles POST /internal/jlm-outreach-email.
// Sends via Zoho SMTP using the app-specific password read from the
// environment here and never exposed to an agent. NOT wired into the
// autonomous prospecting agent's allowed_tools -- outreach is draft-only
// there. This exists for a deliberate, manually-triggered send once a
// draft has been reviewed.
func jlmOutreachEmailHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := loadJLMConfig()
		if !checkJLMToken(w, r, cfg.InternalToken) {
			return
		}
		if cfg.ZohoUser == "" || cfg.ZohoPassword == "" || cfg.ZohoFrom == "" {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "zoho smtp not configured"})
			return
		}
		var req struct {
			To      string `json:"to"`
			Subject string `json:"subject"`
			Body    string `json:"body"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		if strings.TrimSpace(req.To) == "" || strings.TrimSpace(req.Subject) == "" || strings.TrimSpace(req.Body) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "to, subject, and body are required"})
			return
		}

		msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n%s",
			cfg.ZohoFrom, req.To, req.Subject, req.Body)

		auth := smtp.PlainAuth("", cfg.ZohoUser, cfg.ZohoPassword, "smtp.zoho.com")
		err := smtp.SendMail("smtp.zoho.com:587", auth, cfg.ZohoFrom, []string{req.To}, []byte(msg))
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "smtp send failed: " + err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "sent", "to": req.To})
	}
}
