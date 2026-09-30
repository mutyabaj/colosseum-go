package api

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/go-chi/chi/v5"
)

// agentTriggerConfig holds the shared secret for the on-demand agent-run
// trigger endpoints, read from the environment.
type agentTriggerConfig struct {
	InternalToken string // AGENT_TRIGGER_TOKEN
	BasicAuthUser string // COLOSSEUM_BASIC_AUTH_USER (reused, not duplicated)
	BasicAuthPass string // COLOSSEUM_BASIC_AUTH_PASS
}

func loadAgentTriggerConfig() agentTriggerConfig {
	return agentTriggerConfig{
		InternalToken: os.Getenv("AGENT_TRIGGER_TOKEN"),
		BasicAuthUser: os.Getenv("COLOSSEUM_BASIC_AUTH_USER"),
		BasicAuthPass: os.Getenv("COLOSSEUM_BASIC_AUTH_PASS"),
	}
}

func checkAgentTriggerToken(w http.ResponseWriter, r *http.Request, token string) bool {
	if token == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "agent trigger endpoint not configured"})
		return false
	}
	got := r.Header.Get("X-Agent-Trigger-Token")
	if got == "" || got != token {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return false
	}
	return true
}

// triggerableAgents is an explicit allowlist. Deliberately excludes anything
// touching client PII (VITA-Intake-Assistant, VITA-tax-helper) -- Hermes,
// a personal assistant kept isolated from the nonprofit's other systems,
// should never be able to spin up a run against taxpayer data, even
// indirectly through a scoped trigger endpoint. New Colosseum agents are
// NOT triggerable by default -- they must be deliberately added here.
var triggerableAgents = map[string]string{
	"grant-researcher": "f50c5c59-acd2-43d4-a1f1-80a2405999fc",
	"jlm-prospecting":  "a2c01039-eda3-4fea-98e2-ef2065298cef",
}

// agentTriggerRunHandler handles POST /internal/agent-runs/trigger.
// Starts a run for an allowlisted agent only -- the caller names the agent
// by a fixed key (not a raw agent_id), so this can never be used to run an
// agent that isn't deliberately in triggerableAgents. Delegates the actual
// run creation to Colosseum's own /api/runs (same code path the JLM/grants
// weekly cron scripts already use), authenticating with the same Basic Auth
// credentials Colosseum already has configured -- no new credential needed.
func agentTriggerRunHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := loadAgentTriggerConfig()
		if !checkAgentTriggerToken(w, r, cfg.InternalToken) {
			return
		}
		if cfg.BasicAuthUser == "" || cfg.BasicAuthPass == "" {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "colosseum basic auth not configured"})
			return
		}
		var req struct {
			Agent    string `json:"agent"`
			Task     string `json:"task"`
			MaxSteps int    `json:"max_steps,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		agentID, ok := triggerableAgents[strings.TrimSpace(req.Agent)]
		if !ok {
			names := make([]string, 0, len(triggerableAgents))
			for k := range triggerableAgents {
				names = append(names, k)
			}
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unknown or disallowed agent", "allowed": names})
			return
		}
		if strings.TrimSpace(req.Task) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "task is required"})
			return
		}
		if req.MaxSteps <= 0 {
			req.MaxSteps = 120
		}

		body, _ := json.Marshal(map[string]any{"agent_id": agentID, "task": req.Task, "max_steps": req.MaxSteps})
		outReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, "http://localhost:8080/api/runs", bytes.NewReader(body))
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		outReq.SetBasicAuth(cfg.BasicAuthUser, cfg.BasicAuthPass)
		outReq.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(outReq)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(resp.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(respBody)
	}
}

// agentTriggerStatusHandler handles GET /internal/agent-runs/{id}/status.
// A narrow read-only view onto a run's status -- doesn't expose workspace
// paths, credential vault IDs, etc. the way the main getRunHandler does,
// just what a caller polling for completion actually needs.
func agentTriggerStatusHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := loadAgentTriggerConfig()
		if !checkAgentTriggerToken(w, r, cfg.InternalToken) {
			return
		}
		runID := chi.URLParam(r, "id")
		var status, errStr string
		var completedAt sql.NullString
		err := db.QueryRowContext(r.Context(), `SELECT status, error, completed_at FROM runs WHERE id = ?`, runID).
			Scan(&status, &errStr, &completedAt)
		if err == sql.ErrNoRows {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "run not found"})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id": runID, "status": status, "error": errStr, "completed_at": completedAt.String,
		})
	}
}
