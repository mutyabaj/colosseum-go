package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
)

// grantsConfig holds credentials for the EquiVoice grant pipeline, read from
// the environment. The internal token and Graph client secret never leave
// this server -- they are never passed to an agent or included in a system
// prompt. Agents reach these endpoints instead, with the shared secret baked
// into the calling tool's config_json, invisible to the agent.
type grantsConfig struct {
	InternalToken string // GRANTS_INTERNAL_TOKEN
	MSGraphTenant string // MSGRAPH_TENANT_ID (reused from the Hermes O365 mail setup)
	MSGraphClient string // MSGRAPH_CLIENT_ID
	MSGraphSecret string // MSGRAPH_CLIENT_SECRET
	MSGraphMailbox string // MSGRAPH_MAILBOX (john.mutyaba@mnequivoicepartnership.org)
}

func loadGrantsConfig() grantsConfig {
	return grantsConfig{
		InternalToken:  os.Getenv("GRANTS_INTERNAL_TOKEN"),
		MSGraphTenant:  os.Getenv("MSGRAPH_TENANT_ID"),
		MSGraphClient:  os.Getenv("MSGRAPH_CLIENT_ID"),
		MSGraphSecret:  os.Getenv("MSGRAPH_CLIENT_SECRET"),
		MSGraphMailbox: os.Getenv("MSGRAPH_MAILBOX"),
	}
}

func checkGrantsToken(w http.ResponseWriter, r *http.Request, token string) bool {
	if token == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "grants endpoint not configured"})
		return false
	}
	got := r.Header.Get("X-Grants-Internal-Token")
	if got == "" || got != token {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return false
	}
	return true
}

var validGrantCategories = map[string]bool{
	"vita_tax":         true,
	"healthcare":       true,
	"youth_engagement": true,
	"civic_engagement": true,
	"general":          true,
}

type grantRequest struct {
	RunID                        string `json:"run_id"`
	FunderName                   string `json:"funder_name"`
	ProgramName                  string `json:"program_name,omitempty"`
	Category                     string `json:"category,omitempty"`
	Source                       string `json:"source,omitempty"`
	Amount                       string `json:"amount,omitempty"`
	Deadline                     string `json:"deadline,omitempty"`
	EligibilityKeywords          string `json:"eligibility_keywords,omitempty"`
	AlignmentScore               int    `json:"alignment_score,omitempty"`
	Link                         string `json:"link,omitempty"`
	ContactName                  string `json:"contact_name,omitempty"`
	ContactEmail                 string `json:"contact_email,omitempty"`
	Notes                        string `json:"notes,omitempty"`
	DraftNarrative               string `json:"draft_narrative,omitempty"`
	ApplicationDraftArtifactPath string `json:"application_draft_artifact_path,omitempty"`
	Status                       string `json:"status,omitempty"`
}

// grantSaveHandler handles POST /internal/grants.
// Called by Grant-researcher's dedicated http_tool after it has found and
// scored one grant opportunity.
func grantSaveHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := loadGrantsConfig()
		if !checkGrantsToken(w, r, cfg.InternalToken) {
			return
		}
		var req grantRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		if strings.TrimSpace(req.RunID) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "run_id is required"})
			return
		}
		if strings.TrimSpace(req.FunderName) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "funder_name is required"})
			return
		}
		if req.Category == "" {
			req.Category = "general"
		}
		if !validGrantCategories[req.Category] {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid category: must be one of vita_tax, healthcare, youth_engagement, civic_engagement, general"})
			return
		}
		if req.Status == "" {
			req.Status = "new"
		}

		id := uuid.NewString()
		now := time.Now().UTC().Format(time.RFC3339Nano)
		_, err := db.ExecContext(r.Context(), `
			INSERT INTO grants(
				id, run_id, funder_name, program_name, category, source, amount, deadline,
				eligibility_keywords, alignment_score, link, contact_name, contact_email, notes,
				draft_narrative, application_draft_artifact_path, status, created_at, updated_at
			) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(funder_name, program_name) DO UPDATE SET
				run_id=excluded.run_id, category=excluded.category, source=excluded.source,
				amount=excluded.amount, deadline=excluded.deadline,
				eligibility_keywords=excluded.eligibility_keywords, alignment_score=excluded.alignment_score,
				link=excluded.link, contact_name=excluded.contact_name, contact_email=excluded.contact_email,
				notes=excluded.notes, draft_narrative=excluded.draft_narrative,
				application_draft_artifact_path=excluded.application_draft_artifact_path,
				status=excluded.status, updated_at=excluded.updated_at
		`, id, req.RunID, req.FunderName, req.ProgramName, req.Category, req.Source, req.Amount, req.Deadline,
			req.EligibilityKeywords, req.AlignmentScore, req.Link, req.ContactName, req.ContactEmail, req.Notes,
			req.DraftNarrative, req.ApplicationDraftArtifactPath, req.Status, now, now)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "saved", "funder_name": req.FunderName})
	}
}

// grantCheckExistsHandler handles POST /internal/grants/search.
func grantCheckExistsHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := loadGrantsConfig()
		if !checkGrantsToken(w, r, cfg.InternalToken) {
			return
		}
		var req struct {
			FunderName  string `json:"funder_name"`
			ProgramName string `json:"program_name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		if strings.TrimSpace(req.FunderName) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "funder_name is required"})
			return
		}
		var count int
		err := db.QueryRowContext(r.Context(),
			`SELECT COUNT(1) FROM grants WHERE funder_name = ? COLLATE NOCASE AND program_name IS ? COLLATE NOCASE`,
			req.FunderName, req.ProgramName).Scan(&count)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"exists": count > 0})
	}
}

// grantListNamesHandler handles POST /internal/grants/list-names.
func grantListNamesHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := loadGrantsConfig()
		if !checkGrantsToken(w, r, cfg.InternalToken) {
			return
		}
		rows, err := db.QueryContext(r.Context(), `SELECT funder_name, COALESCE(program_name, '') FROM grants ORDER BY funder_name`)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		defer rows.Close()
		type entry struct {
			Funder  string `json:"funder_name"`
			Program string `json:"program_name"`
		}
		entries := []entry{}
		for rows.Next() {
			var e entry
			if err := rows.Scan(&e.Funder, &e.Program); err == nil {
				entries = append(entries, e)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"grants": entries})
	}
}

var grantsCSVHeader = []string{"Category", "Funder", "Program", "Source", "Amount", "Deadline",
	"Alignment Score", "Eligibility Keywords", "Link", "Contact Name", "Contact Email", "Notes",
	"Draft Narrative", "Application Draft", "LOI Draft Status", "Application Draft Status", "Status", "Created At"}

// buildGrantsCSV renders grants as CSV bytes, optionally filtered by run and/or category.
func buildGrantsCSV(ctx context.Context, db *sql.DB, runID, category string) ([]byte, error) {
	cols := `category, funder_name, program_name, source, amount, deadline, alignment_score,
		eligibility_keywords, link, contact_name, contact_email, notes, draft_narrative,
		application_draft_artifact_path, loi_draft_status, application_draft_status, status, created_at`
	query := `SELECT ` + cols + ` FROM grants WHERE 1=1`
	args := []any{}
	if runID != "" {
		query += ` AND run_id = ?`
		args = append(args, runID)
	}
	if category != "" {
		query += ` AND category = ?`
		args = append(args, category)
	}
	query += ` ORDER BY alignment_score DESC, funder_name`

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var buf bytes.Buffer
	buf.Write([]byte{0xEF, 0xBB, 0xBF}) // UTF-8 BOM, see jlm_routes.go buildProspectsCSV
	cw := csv.NewWriter(&buf)
	_ = cw.Write(grantsCSVHeader)
	for rows.Next() {
		const n = 18
		var vals [n]sql.NullString
		ptrs := make([]any, n)
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			continue
		}
		record := make([]string, n)
		for i, v := range vals {
			record[i] = v.String
		}
		_ = cw.Write(record)
	}
	cw.Flush()
	return buf.Bytes(), nil
}

// grantExportHandler handles GET /internal/grants/export?run_id=...&category=...
func grantExportHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := loadGrantsConfig()
		if !checkGrantsToken(w, r, cfg.InternalToken) {
			return
		}
		csvBytes, err := buildGrantsCSV(r.Context(), db, r.URL.Query().Get("run_id"), r.URL.Query().Get("category"))
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		w.Header().Set("Content-Type", "text/csv")
		w.Header().Set("Content-Disposition", `attachment; filename="grants.csv"`)
		_, _ = w.Write(csvBytes)
	}
}

var grantCategoryFilenames = map[string]string{
	"vita_tax":         "VITA-Tax-Assistance.csv",
	"healthcare":       "Healthcare-Outreach.csv",
	"youth_engagement": "Youth-Engagement.csv",
	"civic_engagement": "Civic-Engagement.csv",
	"general":          "General.csv",
}

const grantsOneDriveFolderPath = "Documents/Clients/EquiVoice Partnership/Grants"

// grantPushToOneDriveHandler handles POST /internal/grants/push-to-onedrive?run_id=...
// Pushes one CSV per category to the EquiVoice Grants OneDrive folder, using
// the same personal-OneDrive refresh token mechanism as the JLM pipeline
// (see jlm_routes.go: refreshOneDriveToken, uploadCSVToOneDrive, system_settings storage).
func grantPushToOneDriveHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := loadGrantsConfig()
		if !checkGrantsToken(w, r, cfg.InternalToken) {
			return
		}
		runID := r.URL.Query().Get("run_id")
		if strings.TrimSpace(runID) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "run_id is required"})
			return
		}

		accessToken, err := getOneDriveAccessToken(r.Context(), db)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}

		pushed := map[string]string{}
		dateStamp := time.Now().UTC().Format("2006-01-02")
		for category, baseName := range grantCategoryFilenames {
			csvBytes, err := buildGrantsCSV(r.Context(), db, "", category)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			filename := fmt.Sprintf("%s-%s", dateStamp, baseName)
			if err := uploadCSVToFolder(r.Context(), accessToken, grantsOneDriveFolderPath, filename, csvBytes); err != nil {
				writeJSON(w, http.StatusBadGateway, map[string]string{"error": fmt.Sprintf("upload failed for %s: %s", category, err.Error())})
				return
			}
			pushed[category] = filename
		}

		writeJSON(w, http.StatusOK, map[string]any{"status": "pushed", "path": grantsOneDriveFolderPath, "files": pushed})
	}
}

// getGraphAppToken acquires an app-only Graph token for the EquiVoice work
// tenant (client-credentials flow) -- the same mechanism used for Hermes'
// O365 mail integration, reusing the same app registration's credentials.
func getGraphAppToken(ctx context.Context, cfg grantsConfig) (string, error) {
	body := url.Values{
		"client_id":     {cfg.MSGraphClient},
		"client_secret": {cfg.MSGraphSecret},
		"scope":         {"https://graph.microsoft.com/.default"},
		"grant_type":    {"client_credentials"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/token", cfg.MSGraphTenant),
		strings.NewReader(body.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("graph token request failed: %d: %s", resp.StatusCode, string(respBody))
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", err
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("graph token response missing access_token")
	}
	return out.AccessToken, nil
}

// grantCreateLOIDraftHandler handles POST /internal/grants/loi-draft.
// Creates a REAL draft (never sends) in the mnequivoicepartnership.org
// mailbox via Microsoft Graph -- POSTing to /messages without calling /send
// creates the message directly in the Drafts folder. Safe to expose to the
// agent's own allowed_tools since a draft can't reach anyone until John
// reviews and sends it himself.
func grantCreateLOIDraftHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := loadGrantsConfig()
		if !checkGrantsToken(w, r, cfg.InternalToken) {
			return
		}
		if cfg.MSGraphTenant == "" || cfg.MSGraphClient == "" || cfg.MSGraphSecret == "" || cfg.MSGraphMailbox == "" {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "graph mail not configured"})
			return
		}
		var req struct {
			FunderName  string `json:"funder_name"`
			ProgramName string `json:"program_name"`
			To          string `json:"to"`
			Subject     string `json:"subject"`
			Body        string `json:"body"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		if strings.TrimSpace(req.To) == "" || strings.TrimSpace(req.Subject) == "" || strings.TrimSpace(req.Body) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "to, subject, and body are required"})
			return
		}

		token, err := getGraphAppToken(r.Context(), cfg)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "graph auth failed: " + err.Error()})
			return
		}

		payload := map[string]any{
			"subject": req.Subject,
			"body":    map[string]string{"contentType": "Text", "content": req.Body},
			"toRecipients": []map[string]any{
				{"emailAddress": map[string]string{"address": req.To}},
			},
		}
		body, _ := json.Marshal(payload)
		createURL := fmt.Sprintf("https://graph.microsoft.com/v1.0/users/%s/messages", cfg.MSGraphMailbox)
		greq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, createURL, bytes.NewReader(body))
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		greq.Header.Set("Authorization", "Bearer "+token)
		greq.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(greq)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= 300 {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "graph draft creation failed: " + string(respBody)})
			return
		}

		if req.FunderName != "" {
			_, _ = db.ExecContext(r.Context(), `UPDATE grants SET loi_draft_status = 'created' WHERE funder_name = ? COLLATE NOCASE AND program_name IS ? COLLATE NOCASE`,
				req.FunderName, req.ProgramName)
		}

		writeJSON(w, http.StatusOK, map[string]string{"status": "draft created", "mailbox": cfg.MSGraphMailbox})
	}
}
