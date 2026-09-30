package api

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/smtp"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/adevireddy/colosseum/internal/secrets"
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
		draftStatus := ""
		if strings.TrimSpace(req.ContactEmail) != "" && strings.TrimSpace(req.DraftOutreachSubject) != "" && strings.TrimSpace(req.DraftOutreachBody) != "" {
			if err := appendZohoDraft(r.Context(), cfg, req.ContactEmail, req.DraftOutreachSubject, req.DraftOutreachBody); err != nil {
				draftStatus = "failed: " + err.Error()
			} else {
				draftStatus = "created"
			}
			_, _ = db.ExecContext(r.Context(), `UPDATE jlm_prospects SET zoho_draft_status = ? WHERE organization_name = ?`, draftStatus, req.OrganizationName)
		}

		writeJSON(w, http.StatusOK, map[string]string{"status": "saved", "organization_name": req.OrganizationName, "zoho_draft_status": draftStatus})
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

var jlmCSVHeader = []string{"Organization", "EIN", "Location", "Mission", "Annual Revenue",
	"Latest 990 Year", "Latest 990 Summary", "Website", "Executive Director", "Contact Email",
	"Contact Phone", "Accounting Indicators", "Why Prospect", "Draft Subject", "Draft Body",
	"Zoho Draft Status", "Status", "Created At"}

// buildProspectsCSV renders the given run (or, with an empty runID, every
// prospect) as CSV bytes. Shared by the export endpoint and the OneDrive
// push step so the two never drift.
func buildProspectsCSV(ctx context.Context, db *sql.DB, runID string) ([]byte, error) {
	cols := `organization_name, ein, location, mission, annual_revenue, latest_990_year,
		latest_990_summary, website, executive_director, contact_email, contact_phone,
		accounting_indicators, why_prospect, draft_outreach_subject, draft_outreach_body, zoho_draft_status, status, created_at`
	var rows *sql.Rows
	var err error
	if runID != "" {
		rows, err = db.QueryContext(ctx, `SELECT `+cols+` FROM jlm_prospects WHERE run_id = ? ORDER BY created_at`, runID)
	} else {
		rows, err = db.QueryContext(ctx, `SELECT `+cols+` FROM jlm_prospects ORDER BY created_at`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var buf bytes.Buffer
	// UTF-8 BOM: without it, Excel on Windows opens this as Windows-1252 and
	// mangles anything non-ASCII (em-dashes, curly quotes, accented names)
	// into mojibake like "a-euro-quote" instead of the real character.
	buf.Write([]byte{0xEF, 0xBB, 0xBF})
	cw := csv.NewWriter(&buf)
	_ = cw.Write(jlmCSVHeader)
	for rows.Next() {
		var vals [18]sql.NullString
		ptrs := make([]any, 18)
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			continue
		}
		record := make([]string, 18)
		for i, v := range vals {
			record[i] = v.String
		}
		_ = cw.Write(record)
	}
	cw.Flush()
	return buf.Bytes(), nil
}

// jlmExportProspectsHandler handles GET /internal/jlm-prospects/export?run_id=...
// Returns a CSV of the given run (or, with no run_id, everything).
func jlmExportProspectsHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := loadJLMConfig()
		if !checkJLMToken(w, r, cfg.InternalToken) {
			return
		}
		csvBytes, err := buildProspectsCSV(r.Context(), db, r.URL.Query().Get("run_id"))
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		w.Header().Set("Content-Type", "text/csv")
		w.Header().Set("Content-Disposition", `attachment; filename="jlm-prospects.csv"`)
		_, _ = w.Write(csvBytes)
	}
}

// appendZohoDraft writes a draft message directly into the Drafts folder of
// the Zoho mailbox via raw IMAP APPEND (RFC 3501). This creates a real draft
// John can open, edit, and send from Zoho itself -- it never transmits
// anything. Reuses the same app-specific password as the SMTP send path;
// Zoho app passwords aren't protocol-scoped.
func appendZohoDraft(ctx context.Context, cfg jlmConfig, to, subject, body string) error {
	if cfg.ZohoUser == "" || cfg.ZohoPassword == "" || cfg.ZohoFrom == "" {
		return fmt.Errorf("zoho not configured")
	}

	dialer := &net.Dialer{Timeout: 15 * time.Second}
	conn, err := tls.DialWithDialer(dialer, "tcp", "imap.zoho.com:993", &tls.Config{ServerName: "imap.zoho.com"})
	if err != nil {
		return fmt.Errorf("imap connect failed: %w", err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	}

	reader := bufio.NewReader(conn)
	// Server greeting.
	if _, err := reader.ReadString('\n'); err != nil {
		return fmt.Errorf("imap greeting failed: %w", err)
	}

	readUntilTagged := func(tag string) (string, error) {
		var lines strings.Builder
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return lines.String(), err
			}
			lines.WriteString(line)
			if strings.HasPrefix(line, tag+" ") {
				if !strings.Contains(line, "OK") {
					return lines.String(), fmt.Errorf("imap command failed: %s", strings.TrimSpace(line))
				}
				return lines.String(), nil
			}
		}
	}

	// LOGIN
	if _, err := fmt.Fprintf(conn, "a1 LOGIN %s %s\r\n", imapQuote(cfg.ZohoUser), imapQuote(cfg.ZohoPassword)); err != nil {
		return fmt.Errorf("imap login write failed: %w", err)
	}
	if _, err := readUntilTagged("a1"); err != nil {
		return fmt.Errorf("imap login failed: %w", err)
	}

	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n%s",
		cfg.ZohoFrom, to, subject, body)

	// APPEND -- announce the literal length, wait for the "+" continuation,
	// then send the raw message bytes.
	if _, err := fmt.Fprintf(conn, "a2 APPEND \"Drafts\" (\\Draft) {%d}\r\n", len(msg)); err != nil {
		return fmt.Errorf("imap append header write failed: %w", err)
	}
	cont, err := reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("imap append continuation read failed: %w", err)
	}
	if !strings.HasPrefix(cont, "+") {
		return fmt.Errorf("imap server rejected append literal: %s", strings.TrimSpace(cont))
	}
	if _, err := conn.Write([]byte(msg + "\r\n")); err != nil {
		return fmt.Errorf("imap append body write failed: %w", err)
	}
	if _, err := readUntilTagged("a2"); err != nil {
		return fmt.Errorf("imap append failed: %w", err)
	}

	_, _ = fmt.Fprintf(conn, "a3 LOGOUT\r\n")
	return nil
}

// imapQuote wraps a value in IMAP quoted-string syntax. Zoho credentials
// don't contain quotes or backslashes in practice, but escape defensively
// rather than assume that.
func imapQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
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

// oneDriveClientID is the public-client Azure AD app registration created for
// this integration (personal Microsoft accounts don't support the app-only
// client-credentials flow used for the work O365 mailbox, so this uses a
// delegated refresh token obtained once via device-code consent instead).
const oneDriveClientID = "4c4736de-906c-4c48-9ceb-75217983e3a3"

const jlmOneDriveFolderPath = "Documents/Clients/JLM Risk/Customer/Leads"

func getSystemSettingValue(ctx context.Context, db *sql.DB, key string) (string, error) {
	var value string
	err := db.QueryRowContext(ctx, `SELECT value FROM system_settings WHERE key = ?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return value, err
}

func setSystemSettingValue(ctx context.Context, db *sql.DB, key, value string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := db.ExecContext(ctx, `
		INSERT INTO system_settings(key, value, updated_at) VALUES(?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
	`, key, value, now)
	return err
}

// refreshOneDriveToken redeems the stored refresh token for a fresh access
// token. Personal Microsoft account refresh tokens rotate on every
// redemption -- callers MUST persist the returned refresh token, or the next
// call will fail once the old one is invalidated.
func refreshOneDriveToken(ctx context.Context, refreshToken string) (accessToken, newRefreshToken string, err error) {
	body := url.Values{
		"client_id":     {oneDriveClientID},
		"refresh_token": {refreshToken},
		"grant_type":    {"refresh_token"},
		"scope":         {"Files.ReadWrite offline_access"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://login.microsoftonline.com/consumers/oauth2/v2.0/token", strings.NewReader(body.Encode()))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("token refresh failed: %d: %s", resp.StatusCode, string(respBody))
	}
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", "", err
	}
	if out.AccessToken == "" || out.RefreshToken == "" {
		return "", "", fmt.Errorf("token refresh response missing tokens")
	}
	return out.AccessToken, out.RefreshToken, nil
}

// getOneDriveAccessToken returns a fresh OneDrive access token for the
// connected personal account, transparently handling the refresh-token
// rotation personal Microsoft accounts require on every redemption. Shared
// by every pipeline that pushes files to OneDrive (JLM, grants, ...).
func getOneDriveAccessToken(ctx context.Context, db *sql.DB) (string, error) {
	secretKey := os.Getenv("COLOSSEUM_SECRET_KEY")
	encRefreshToken, err := getSystemSettingValue(ctx, db, "jlm_onedrive_refresh_token")
	if err != nil || encRefreshToken == "" {
		return "", fmt.Errorf("onedrive not connected (no refresh token stored)")
	}
	refreshToken, err := decryptSecret(encRefreshToken, secretKey)
	if err != nil {
		return "", fmt.Errorf("failed to decrypt stored refresh token: %w", err)
	}
	accessToken, newRefreshToken, err := refreshOneDriveToken(ctx, refreshToken)
	if err != nil {
		return "", fmt.Errorf("onedrive token refresh failed: %w", err)
	}
	// Persist the rotated refresh token immediately -- if this fails we'd
	// rather error loudly now than silently lose access on the next run.
	if err := encryptAndStoreSecret(ctx, db, "jlm_onedrive_refresh_token", newRefreshToken, secretKey); err != nil {
		return "", fmt.Errorf("failed to store refreshed token: %w", err)
	}
	return accessToken, nil
}

func decryptSecret(cipherText, keyMaterial string) (string, error) {
	return secrets.Decrypt(cipherText, keyMaterial)
}

func encryptAndStoreSecret(ctx context.Context, db *sql.DB, key, value, keyMaterial string) error {
	enc, err := secrets.Encrypt(value, keyMaterial)
	if err != nil {
		return err
	}
	return setSystemSettingValue(ctx, db, key, enc)
}

// uploadCSVToOneDrive writes data to <jlmOneDriveFolderPath>/filename in the
// signed-in personal OneDrive account, overwriting any existing file of the
// same name.
func uploadCSVToOneDrive(ctx context.Context, accessToken, filename string, data []byte) error {
	return uploadCSVToFolder(ctx, accessToken, jlmOneDriveFolderPath, filename, data)
}

// uploadCSVToFolder writes data to <folderPath>/filename in the signed-in
// personal OneDrive account, overwriting any existing file of the same name.
func uploadCSVToFolder(ctx context.Context, accessToken, folderPath, filename string, data []byte) error {
	target := fmt.Sprintf("%s/%s", folderPath, filename)
	// Path segments (incl. spaces) must be percent-encoded individually --
	// naive url.PathEscape would also escape the "/" separators.
	segments := strings.Split(target, "/")
	for i, seg := range segments {
		segments[i] = url.PathEscape(seg)
	}
	encodedPath := strings.Join(segments, "/")
	uploadURL := fmt.Sprintf("https://graph.microsoft.com/v1.0/me/drive/root:/%s:/content", encodedPath)

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "text/csv")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("onedrive upload failed: %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// jlmPushToOneDriveHandler handles POST /internal/jlm-prospects/push-to-onedrive?run_id=...
// Builds the CSV for the given run and pushes it straight to the personal
// OneDrive folder via Microsoft Graph -- called by the weekly cron after a
// research run completes. No local machine is involved.
func jlmPushToOneDriveHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := loadJLMConfig()
		if !checkJLMToken(w, r, cfg.InternalToken) {
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

		csvBytes, err := buildProspectsCSV(r.Context(), db, runID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}

		filename := fmt.Sprintf("jlm-prospects-%s.csv", time.Now().UTC().Format("2006-01-02"))
		if err := uploadCSVToOneDrive(r.Context(), accessToken, filename, csvBytes); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}

		writeJSON(w, http.StatusOK, map[string]string{"status": "pushed", "filename": filename, "path": jlmOneDriveFolderPath})
	}
}
