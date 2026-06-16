package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/adevireddy/colosseum/internal/providers"
	"github.com/adevireddy/colosseum/internal/secrets"
	"github.com/adevireddy/colosseum/internal/tools"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type createAgentRequest struct {
	Name                     string   `json:"name"`
	Description              string   `json:"description"`
	Provider                 string   `json:"provider"`
	Model                    string   `json:"model"`
	SystemPrompt             string   `json:"system_prompt"`
	AllowedTools             []string `json:"allowed_tools"`
	StarterPrompts           []string `json:"starter_prompts"`
	DefaultTask              string   `json:"default_task"`
	DefaultMaxSteps          int      `json:"default_max_steps"`
	DefaultWorkspacePath     string   `json:"default_workspace_path"`
	DefaultEnvironmentID     string   `json:"default_environment_id"`
	DefaultCredentialVaultID string   `json:"default_credential_vault_id"`
	OutputContractType       string   `json:"output_contract_type"`
	OutputContractPayload    string   `json:"output_contract_payload"`
	PlanningMode             string   `json:"planning_mode"`
}

func normalizePlanningMode(mode string) (string, error) {
	switch strings.TrimSpace(strings.ToLower(mode)) {
	case "", "off":
		return "off", nil
	case "suggest":
		return "suggest", nil
	case "required":
		return "required", nil
	default:
		return "", fmt.Errorf("invalid planning_mode %q (must be off, suggest, or required)", mode)
	}
}

type createRunRequest struct {
	AgentID               string `json:"agent_id"`
	Task                  string `json:"task"`
	WorkspacePath         string `json:"workspace_path"`
	SourceWorkspacePath   string `json:"source_workspace_path"`
	ReplaySourceRunID     string `json:"replay_source_run_id"`
	ReplayFromStep        int    `json:"replay_from_step"`
	Provider              string `json:"provider"`
	Model                 string `json:"model"`
	MaxSteps              int    `json:"max_steps"`
	EnvironmentID         string `json:"environment_id"`
	CredentialVaultID     string `json:"credential_vault_id"`
	OutputContractType    string `json:"output_contract_type"`
	OutputContractPayload string `json:"output_contract_payload"`
}

type toolUpsertRequest struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
	Kind        string          `json:"kind"`
	ConfigJSON  json.RawMessage `json:"config_json"`
	Enabled     bool            `json:"enabled"`
}

type enhancePromptRequest struct {
	Prompt   string `json:"prompt"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

func registerAPIRoutes(
	r chi.Router,
	db *sql.DB,
	workspaceRoot string,
	availableProviders map[string]bool,
	openAIKey string,
	anthropicKey string,
	secretKey string,
	providerMap map[string]providers.Client,
) {
	r.Route("/api", func(r chi.Router) {
		r.Post("/agents", createAgentHandler(db))
		r.Get("/agents", listAgentsHandler(db))
		r.Put("/agents/{id}", updateAgentHandler(db))
		r.Delete("/agents/{id}", deleteAgentHandler(db))
		r.Get("/tools", listToolsHandler(db))
		r.Post("/tools", createToolHandler(db))
		r.Put("/tools/{id}", updateToolHandler(db))
		r.Delete("/tools/{id}", deleteToolHandler(db))
		r.Post("/tools/{id}/test", testToolHandler(db))
		r.Post("/runs", createRunHandler(db, workspaceRoot))
		r.Get("/runs", listRunsHandler(db))
		r.Get("/runs/{id}", getRunHandler(db))
		r.Post("/runs/{id}/replay", replayRunHandler(db, workspaceRoot))
		r.Get("/runs/{id}/trace", getRunTraceHandler(db))
		r.Get("/runs/{id}/telemetry", getRunTelemetryHandler(db))
		r.Get("/runs/{id}/artifacts", getRunArtifactsHandler(db))
		r.Get("/runs/{id}/artifacts/{artifactID}/content", getRunArtifactContentHandler(db))
		r.Post("/runs/{id}/files", uploadRunFilesHandler(db))
		r.Get("/runs/{id}/export", exportRunBundleHandler(db))
		r.Post("/runs/{id}/cancel", updateRunStatusHandler(db, "cancelled"))
		r.Post("/runs/{id}/approve", approveLatestHandler(db))
		r.Post("/runs/{id}/interrupt", updateRunStatusHandler(db, "interrupted"))
		r.Post("/runs/{id}/resume", updateRunStatusHandler(db, "queued"))
		r.Post("/runs/{id}/events", appendRunEventHandler(db))
		r.Get("/chat/sessions", listChatSessionsHandler(db))
		r.Post("/chat/sessions", createChatSessionHandler(db))
		r.Get("/chat/sessions/{id}", getChatSessionHandler(db))
		r.Patch("/chat/sessions/{id}", patchChatSessionHandler(db))
		r.Get("/chat/sessions/{id}/messages", listChatSessionMessagesHandler(db))
		r.Post("/chat/sessions/{id}/messages", createChatSessionMessageHandler(db, workspaceRoot, providerMap))
		r.Post("/chat/sessions/{id}/attachments", uploadChatSessionAttachmentsHandler(db))
		r.Get("/providers", providersHandler(availableProviders))
		r.Get("/providers/openai/models", openAIModelsHandler(openAIKey))
		r.Post("/prompts/enhance", enhancePromptHandler(providerMap))
		r.Get("/policies", listPoliciesHandler(db))
		r.Post("/policies", createPolicyHandler(db))
		r.Put("/policies/{id}", updatePolicyHandler(db))
		r.Delete("/policies/{id}", deletePolicyHandler(db))
		r.Get("/secrets", listSecretsHandler(db))
		r.Post("/secrets", createSecretHandler(db, secretKey))
		r.Delete("/secrets/{name}", deleteSecretHandler(db))
		r.Get("/provider-configs", listProviderConfigsHandler(db))
		r.Post("/provider-configs", createProviderConfigHandler(db))
		r.Put("/provider-configs/{id}", updateProviderConfigHandler(db))
		r.Delete("/provider-configs/{id}", deleteProviderConfigHandler(db))
		r.Post("/provider-configs/{id}/test", testProviderConfigHandler(db, openAIKey, anthropicKey))
		r.Get("/environments", listEnvironmentsHandler(db))
		r.Post("/environments", createEnvironmentHandler(db))
		r.Put("/environments/{id}", updateEnvironmentHandler(db))
		r.Delete("/environments/{id}", deleteEnvironmentHandler(db))
		r.Get("/credential-vaults", listCredentialVaultsHandler(db))
		r.Post("/credential-vaults", createCredentialVaultHandler(db))
		r.Put("/credential-vaults/{id}", updateCredentialVaultHandler(db))
		r.Delete("/credential-vaults/{id}", deleteCredentialVaultHandler(db))
		r.Get("/credential-vaults/{id}/items", listCredentialVaultItemsHandler(db))
		r.Post("/credential-vaults/{id}/items", upsertCredentialVaultItemHandler(db))
		r.Delete("/credential-vaults/{id}/items/{secretName}", deleteCredentialVaultItemHandler(db))
		r.Get("/stream/runs/{id}", streamRunEventsHandler(db))

		// Public chat API — no auth required (validated by per-agent public_token)
		r.Get("/public/agents/{agentID}", publicGetAgentHandler(db))
		r.Post("/public/chat/sessions", publicCreateSessionHandler(db))
		r.Get("/public/chat/sessions/{sessionID}", publicGetSessionHandler(db))
		r.Get("/public/chat/sessions/{sessionID}/messages", publicListMessagesHandler(db))
		r.Post("/public/chat/sessions/{sessionID}/messages", publicSendMessageHandler(db, workspaceRoot, nil))
		r.Get("/public/stream/runs/{runID}", publicStreamRunHandler(db))
	})

	// Public chat page — served outside /api, no auth required
	r.Get("/c/{agentID}", publicChatPageHandler(db))

	// Twilio SMS webhook — auth bypassed, validated by Twilio signature instead
	r.Post("/sms/inbound", smsInboundHandler(db, workspaceRoot))

	// Twilio WhatsApp webhook — same pattern as SMS, whatsapp: prefix on numbers
	r.Post("/whatsapp/inbound", whatsappInboundHandler(db, workspaceRoot))

	// Twilio Voice webhook — VITA IVR with auto-SMS booking link
	registerVoiceRoutes(r)

	// MCP server management
	registerMCPRoutes(r, db)
}

func enhancePromptHandler(providerMap map[string]providers.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req enhancePromptRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		selectedProvider, client := pickEnhancerProvider(providerMap, req.Provider)
		if client == nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no AI provider configured for prompt enhancement"})
			return
		}
		model := strings.TrimSpace(req.Model)
		candidateModels := buildEnhancerModelCandidates(selectedProvider, model)
		basePrompt := strings.TrimSpace(req.Prompt)
		if basePrompt == "" {
			basePrompt = "General-purpose autonomous coding/system harness."
		}

		systemInstruction := strings.TrimSpace(`
You are a senior prompt engineer. Rewrite the user's system prompt into a robust production-grade harness prompt for autonomous agents.

Requirements:
- Preserve user intent but make it clearer and more complete.
- Add clear safety boundaries and guardrails.
- Define planning and execution behavior.
- Include explicit input assumptions, output expectations, and failure handling.
- Include decision rules for when to ask clarifying questions versus acting directly.
- Bias strongly toward one-shot execution: complete the requested task in one run when feasible.
- Avoid conversational follow-ups after completion (e.g. "want me to also...") unless the task is blocked or ambiguous.
- When outputs are requested (artifacts, files, screenshots, patches), require generating them directly and citing the produced output.
- Include a strong "tool-first for external/current facts" rule when tools are available.
- Forbid "I cannot access X" responses when an allowed tool can retrieve or verify X.
- Require concise evidence from tool outputs in final answers whenever tools are used.
- Include concise formatting guidelines for agent responses.
- Keep it practical and actionable for real software tasks.
- Return only the final system prompt text (no markdown fences, no commentary). Don't include any prefixes.
`)
		userInstruction := fmt.Sprintf("Original system prompt:\n%s\n\nRewrite it now.", basePrompt)
		var attemptErrors []string
		for _, attemptModel := range candidateModels {
			resp, err := client.Complete(r.Context(), providers.CompletionRequest{
				Model:    attemptModel,
				System:   systemInstruction,
				Messages: []providers.Message{{Role: "user", Content: userInstruction}},
				Tools:    []providers.Tool{},
				Timeout:  45 * time.Second,
			})
			if err != nil {
				attemptErrors = append(attemptErrors, fmt.Sprintf("%s: %s", attemptModel, err.Error()))
				continue
			}
			enhanced := strings.TrimSpace(resp.Text)
			if enhanced == "" {
				attemptErrors = append(attemptErrors, fmt.Sprintf("%s: empty response text", attemptModel))
				continue
			}
			writeJSON(w, http.StatusOK, map[string]string{
				"provider": selectedProvider,
				"model":    attemptModel,
				"prompt":   enhanced,
			})
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":            "provider enhancement request failed",
			"provider":         selectedProvider,
			"attempted_models": candidateModels,
			"details":          attemptErrors,
		})
	}
}

func pickEnhancerProvider(providerMap map[string]providers.Client, requested string) (string, providers.Client) {
	want := strings.TrimSpace(strings.ToLower(requested))
	if want != "" {
		if client, ok := providerMap[want]; ok {
			return want, client
		}
	}
	if client, ok := providerMap["openai"]; ok {
		return "openai", client
	}
	if client, ok := providerMap["anthropic"]; ok {
		return "anthropic", client
	}
	for name, client := range providerMap {
		return name, client
	}
	return "", nil
}

func defaultEnhancerModel(provider string) string {
	switch provider {
	case "openai":
		return "gpt-5.4"
	case "anthropic":
		return "claude-3-5-sonnet-latest"
	default:
		return "gpt-5.4"
	}
}

func buildEnhancerModelCandidates(provider, requested string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, 4)
	add := func(model string) {
		m := strings.TrimSpace(model)
		if m == "" || seen[m] {
			return
		}
		seen[m] = true
		out = append(out, m)
	}
	add(requested)
	add(defaultEnhancerModel(provider))
	switch provider {
	case "openai":
		add("gpt-5.4")
		add("gpt-4.1-mini")
		add("gpt-4o-mini")
		add("gpt-4.1")
	case "anthropic":
		add("claude-3-5-sonnet-latest")
		add("claude-3-5-haiku-latest")
	}
	return out
}

func createAgentHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req createAgentRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		if req.Name == "" || req.Provider == "" || req.Model == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name, provider, model required"})
			return
		}
		if req.DefaultMaxSteps < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "default_max_steps cannot be negative"})
			return
		}
		if req.DefaultMaxSteps == 0 {
			req.DefaultMaxSteps = 30
		}
		if err := validateOutputContractDefinition(req.OutputContractType, req.OutputContractPayload); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		planningMode, err := normalizePlanningMode(req.PlanningMode)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		req.StarterPrompts = normalizeStarterPrompts(req.StarterPrompts)
		id := uuid.NewString()
		now := time.Now().UTC().Format(time.RFC3339Nano)
		toolsJSON, _ := json.Marshal(req.AllowedTools)
		starterPromptsJSON, _ := json.Marshal(req.StarterPrompts)
		_, err = db.ExecContext(r.Context(), `
			INSERT INTO agents(
				id,name,description,provider,model,system_prompt,allowed_tools,starter_prompts,
				default_task,default_max_steps,default_workspace_path,default_environment_id,default_credential_vault_id,
				output_contract_type,output_contract_payload,planning_mode,created_at,updated_at
			)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		`, id, req.Name, req.Description, req.Provider, req.Model, req.SystemPrompt, string(toolsJSON), string(starterPromptsJSON), req.DefaultTask, req.DefaultMaxSteps, req.DefaultWorkspacePath, strings.TrimSpace(req.DefaultEnvironmentID), strings.TrimSpace(req.DefaultCredentialVaultID), normalizeOutputContractType(req.OutputContractType), strings.TrimSpace(req.OutputContractPayload), planningMode, now, now)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"id": id})
	}
}

func listAgentsHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := db.QueryContext(r.Context(), `
			SELECT id,name,description,provider,model,system_prompt,allowed_tools,starter_prompts,default_task,default_max_steps,default_workspace_path,default_environment_id,default_credential_vault_id,output_contract_type,output_contract_payload,planning_mode,created_at,updated_at
			FROM agents
			ORDER BY created_at DESC
		`)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		defer rows.Close()
		out := make([]map[string]any, 0)
		for rows.Next() {
			var id, name, desc, provider, model, prompt, tools, starterPrompts, defaultTask, defaultWorkspacePath, defaultEnvID, defaultVaultID, outputContractType, outputContractPayload, planningMode, createdAt, updatedAt string
			var defaultMaxSteps int
			if err := rows.Scan(&id, &name, &desc, &provider, &model, &prompt, &tools, &starterPrompts, &defaultTask, &defaultMaxSteps, &defaultWorkspacePath, &defaultEnvID, &defaultVaultID, &outputContractType, &outputContractPayload, &planningMode, &createdAt, &updatedAt); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			out = append(out, map[string]any{
				"id": id, "name": name, "description": desc, "provider": provider, "model": model,
				"system_prompt": prompt, "allowed_tools": json.RawMessage(tools), "starter_prompts": json.RawMessage(starterPrompts),
				"default_task": defaultTask, "default_max_steps": defaultMaxSteps, "default_workspace_path": defaultWorkspacePath,
				"default_environment_id": defaultEnvID, "default_credential_vault_id": defaultVaultID,
				"output_contract_type": outputContractType, "output_contract_payload": outputContractPayload,
				"planning_mode": planningMode,
				"created_at":    createdAt, "updated_at": updatedAt,
			})
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func updateAgentHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		agentID := chi.URLParam(r, "id")
		var req createAgentRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		if req.Name == "" || req.Provider == "" || req.Model == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name, provider, model required"})
			return
		}
		if req.DefaultMaxSteps < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "default_max_steps cannot be negative"})
			return
		}
		if req.DefaultMaxSteps == 0 {
			req.DefaultMaxSteps = 30
		}
		if err := validateOutputContractDefinition(req.OutputContractType, req.OutputContractPayload); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		planningMode, err := normalizePlanningMode(req.PlanningMode)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		req.StarterPrompts = normalizeStarterPrompts(req.StarterPrompts)
		toolsJSON, _ := json.Marshal(req.AllowedTools)
		starterPromptsJSON, _ := json.Marshal(req.StarterPrompts)
		now := time.Now().UTC().Format(time.RFC3339Nano)
		res, err := db.ExecContext(r.Context(), `
			UPDATE agents
			SET name=?, description=?, provider=?, model=?, system_prompt=?, allowed_tools=?, starter_prompts=?, default_task=?, default_max_steps=?, default_workspace_path=?, default_environment_id=?, default_credential_vault_id=?, output_contract_type=?, output_contract_payload=?, planning_mode=?, updated_at=?
			WHERE id=?
		`, req.Name, req.Description, req.Provider, req.Model, req.SystemPrompt, string(toolsJSON), string(starterPromptsJSON), req.DefaultTask, req.DefaultMaxSteps, req.DefaultWorkspacePath, strings.TrimSpace(req.DefaultEnvironmentID), strings.TrimSpace(req.DefaultCredentialVaultID), normalizeOutputContractType(req.OutputContractType), strings.TrimSpace(req.OutputContractPayload), planningMode, now, agentID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		affected, _ := res.RowsAffected()
		if affected == 0 {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "agent not found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"id": agentID})
	}
}

func deleteAgentHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		agentID := chi.URLParam(r, "id")
		forceDelete := parseTruthy(r.URL.Query().Get("force"))
		var deletedRuns int64
		var runCount int
		if err := db.QueryRowContext(r.Context(), `SELECT COUNT(1) FROM runs WHERE agent_id=?`, agentID).Scan(&runCount); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if runCount > 0 && !forceDelete {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "agent has runs; cannot delete"})
			return
		}
		if runCount > 0 && forceDelete {
			n, err := deleteRunsForAgent(r.Context(), db, agentID)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			deletedRuns = n
		}
		res, err := db.ExecContext(r.Context(), `DELETE FROM agents WHERE id=?`, agentID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		affected, _ := res.RowsAffected()
		if affected == 0 {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "agent not found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "deleted_runs": deletedRuns})
	}
}

func deleteRunsForAgent(ctx context.Context, db *sql.DB, agentID string) (int64, error) {
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT path FROM artifacts WHERE run_id IN (SELECT id FROM runs WHERE agent_id=?)`, agentID)
	if err != nil {
		return 0, fmt.Errorf("query artifact paths: %w", err)
	}
	artifactPaths := make([]string, 0)
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scan artifact path: %w", err)
		}
		if path != "" {
			artifactPaths = append(artifactPaths, path)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("iterate artifact paths: %w", err)
	}
	_ = rows.Close()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	rollBack := true
	defer func() {
		if rollBack {
			_ = tx.Rollback()
		}
	}()

	deleteStatements := []string{
		`DELETE FROM chat_messages WHERE run_id IN (SELECT id FROM runs WHERE agent_id=?)`,
		`DELETE FROM session_runs WHERE run_id IN (SELECT id FROM runs WHERE agent_id=?)`,
		`DELETE FROM approvals WHERE run_id IN (SELECT id FROM runs WHERE agent_id=?)`,
		`DELETE FROM containers WHERE run_id IN (SELECT id FROM runs WHERE agent_id=?)`,
		`DELETE FROM artifacts WHERE run_id IN (SELECT id FROM runs WHERE agent_id=?)`,
		`DELETE FROM events WHERE run_id IN (SELECT id FROM runs WHERE agent_id=?)`,
		`DELETE FROM trace_spans WHERE run_id IN (SELECT id FROM runs WHERE agent_id=?)`,
		`DELETE FROM tool_calls WHERE run_id IN (SELECT id FROM runs WHERE agent_id=?)`,
		`DELETE FROM run_steps WHERE run_id IN (SELECT id FROM runs WHERE agent_id=?)`,
	}
	for _, stmt := range deleteStatements {
		if _, err := tx.ExecContext(ctx, stmt, agentID); err != nil {
			return 0, fmt.Errorf("delete run dependencies: %w", err)
		}
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM runs WHERE agent_id=?`, agentID)
	if err != nil {
		return 0, fmt.Errorf("delete runs: %w", err)
	}
	deletedRuns, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit tx: %w", err)
	}
	rollBack = false

	for _, artifactPath := range artifactPaths {
		cleanPath := filepath.Clean(artifactPath)
		if strings.HasPrefix(cleanPath, "inline://") {
			continue
		}
		_ = os.Remove(cleanPath)
	}
	return deletedRuns, nil
}

func listToolsHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defs, err := tools.ListDefinitions(r.Context(), db, true)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, defs)
	}
}

func createToolHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req toolUpsertRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		if req.Name == "" || req.Description == "" || len(req.InputSchema) == 0 || req.Kind == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name, description, kind, input_schema required"})
			return
		}
		id := uuid.NewString()
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if len(req.ConfigJSON) == 0 {
			req.ConfigJSON = json.RawMessage(`{}`)
		}
		enabled := 0
		if req.Enabled {
			enabled = 1
		}
		_, err := db.ExecContext(r.Context(), `INSERT INTO tool_defs(id,name,description,input_schema_json,kind,config_json,enabled,is_builtin,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			id, req.Name, req.Description, string(req.InputSchema), req.Kind, string(req.ConfigJSON), enabled, 0, now, now)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"id": id})
	}
}

func updateToolHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		var req toolUpsertRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		enabled := 0
		if req.Enabled {
			enabled = 1
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if len(req.ConfigJSON) == 0 {
			req.ConfigJSON = json.RawMessage(`{}`)
		}
		res, err := db.ExecContext(r.Context(), `
			UPDATE tool_defs SET
			 name=?, description=?, input_schema_json=?, kind=?, config_json=?, enabled=?, updated_at=?
			WHERE id=? AND is_builtin=0
		`, req.Name, req.Description, string(req.InputSchema), req.Kind, string(req.ConfigJSON), enabled, now, id)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		affected, _ := res.RowsAffected()
		if affected == 0 {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "tool not found or immutable"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"id": id})
	}
}

func deleteToolHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		res, err := db.ExecContext(r.Context(), `DELETE FROM tool_defs WHERE id=? AND is_builtin=0`, id)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		affected, _ := res.RowsAffected()
		if affected == 0 {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "tool not found or immutable"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
	}
}

func testToolHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		var req struct {
			WorkspacePath string          `json:"workspace_path"`
			Input         json.RawMessage `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		if req.WorkspacePath == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "workspace_path required"})
			return
		}
		var name string
		if err := db.QueryRowContext(r.Context(), `SELECT name FROM tool_defs WHERE id=?`, id).Scan(&name); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "tool not found"})
			return
		}
		exec := &tools.Executor{DB: db, ArtifactsDir: "./artifacts"}
		res, err := exec.Execute(r.Context(), tools.Context{RunID: "tool-test", StepID: "tool-test", Workspace: req.WorkspacePath}, name, req.Input)
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error(), "output": res.Output, "log": res.Log})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "output": res.Output, "log": res.Log})
	}
}

func createRunHandler(db *sql.DB, workspaceRoot string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req createRunRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		if req.AgentID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "agent_id required"})
			return
		}
		if req.ReplayFromStep <= 0 {
			req.ReplayFromStep = 1
		}
		var defaultProvider, defaultModel, defaultTask, defaultWorkspacePath, defaultOutputContractType, defaultOutputContractPayload string
		var defaultMaxSteps int
		err := db.QueryRowContext(
			r.Context(),
			`SELECT provider, model, default_task, default_max_steps, default_workspace_path, output_contract_type, output_contract_payload FROM agents WHERE id=?`,
			req.AgentID,
		).Scan(&defaultProvider, &defaultModel, &defaultTask, &defaultMaxSteps, &defaultWorkspacePath, &defaultOutputContractType, &defaultOutputContractPayload)
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "agent not found"})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if req.Provider == "" {
			req.Provider = defaultProvider
		}
		if req.Model == "" {
			req.Model = defaultModel
		}
		if req.Task == "" {
			req.Task = defaultTask
		}
		if strings.TrimSpace(req.Task) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "task required (or configure agent default_task)"})
			return
		}
		if req.MaxSteps <= 0 {
			req.MaxSteps = defaultMaxSteps
		}
		if req.MaxSteps <= 0 {
			req.MaxSteps = 30
		}
		if strings.TrimSpace(req.OutputContractType) == "" {
			req.OutputContractType = defaultOutputContractType
			req.OutputContractPayload = defaultOutputContractPayload
		}
		if err := validateOutputContractDefinition(req.OutputContractType, req.OutputContractPayload); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if err := validateEnvironmentID(r.Context(), db, req.EnvironmentID); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if err := validateCredentialVaultID(r.Context(), db, req.CredentialVaultID); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if req.ReplaySourceRunID != "" {
			var replayCount int
			if err := db.QueryRowContext(r.Context(), `SELECT COUNT(1) FROM runs WHERE id=?`, req.ReplaySourceRunID).Scan(&replayCount); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			if replayCount == 0 {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "replay source run not found"})
				return
			}
		}
		id := uuid.NewString()
		now := time.Now().UTC().Format(time.RFC3339Nano)
		workspacePath := req.WorkspacePath
		if workspacePath == "" {
			workspacePath = resolveWorkspacePath(defaultWorkspacePath, workspaceRoot, id)
		}
		workspacePath, err = filepath.Abs(workspacePath)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid workspace_path"})
			return
		}
		workspacePath, err = ensurePathWithinRoot(workspaceRoot, workspacePath)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "workspace_path must be inside workspace root"})
			return
		}
		if err := os.MkdirAll(workspacePath, 0o755); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create workspace"})
			return
		}
		if req.SourceWorkspacePath != "" {
			sourcePath, err := filepath.Abs(req.SourceWorkspacePath)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid source_workspace_path"})
				return
			}
			sourcePath, err = ensurePathWithinRoot(workspaceRoot, sourcePath)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "source_workspace_path must be inside workspace root"})
				return
			}
			if err := copyDirectory(sourcePath, workspacePath); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to seed workspace: " + err.Error()})
				return
			}
		}
		_, err = db.ExecContext(r.Context(), `
			INSERT INTO runs(id,agent_id,status,task,workspace_path,provider,model,max_steps,replay_source_run_id,replay_from_step,environment_id,credential_vault_id,output_contract_type,output_contract_payload,created_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		`, id, req.AgentID, "queued", req.Task, workspacePath, req.Provider, req.Model, req.MaxSteps, req.ReplaySourceRunID, req.ReplayFromStep, req.EnvironmentID, req.CredentialVaultID, normalizeOutputContractType(req.OutputContractType), strings.TrimSpace(req.OutputContractPayload), now, now)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if _, err := db.ExecContext(r.Context(), `INSERT INTO events(id,run_id,event_type,seq,payload_json,created_at) VALUES(?,?,?,?,?,?)`, uuid.NewString(), id, "run.created", 1, `{"status":"queued"}`, now); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "run created but failed to append run.created event"})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"id": id, "status": "queued", "workspace_path": workspacePath})
	}
}

func normalizeStarterPrompts(prompts []string) []string {
	if len(prompts) == 0 {
		return []string{}
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(prompts))
	for _, prompt := range prompts {
		cleaned := strings.TrimSpace(prompt)
		if cleaned == "" || seen[cleaned] {
			continue
		}
		if len(cleaned) > 280 {
			cleaned = cleaned[:280]
		}
		seen[cleaned] = true
		out = append(out, cleaned)
		if len(out) >= 16 {
			break
		}
	}
	return out
}

func resolveWorkspacePath(defaultPath, workspaceRoot, runID string) string {
	candidate := strings.TrimSpace(defaultPath)
	if candidate == "" {
		return filepath.Join(workspaceRoot, runID)
	}
	return strings.ReplaceAll(candidate, "{{run_id}}", runID)
}

func validateEnvironmentID(ctx context.Context, db *sql.DB, environmentID string) error {
	if strings.TrimSpace(environmentID) == "" {
		return nil
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(1) FROM environments WHERE id=?`, environmentID).Scan(&count); err != nil {
		return fmt.Errorf("validate environment: %w", err)
	}
	if count == 0 {
		return errors.New("environment not found")
	}
	return nil
}

func validateCredentialVaultID(ctx context.Context, db *sql.DB, vaultID string) error {
	if strings.TrimSpace(vaultID) == "" {
		return nil
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(1) FROM credential_vaults WHERE id=?`, vaultID).Scan(&count); err != nil {
		return fmt.Errorf("validate credential vault: %w", err)
	}
	if count == 0 {
		return errors.New("credential vault not found")
	}
	return nil
}

func replayRunHandler(db *sql.DB, workspaceRoot string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sourceRunID := chi.URLParam(r, "id")
		var req struct {
			ResumeFromStep        int    `json:"resume_from_step"`
			Provider              string `json:"provider"`
			Model                 string `json:"model"`
			MaxSteps              int    `json:"max_steps"`
			EnvironmentID         string `json:"environment_id"`
			VaultID               string `json:"credential_vault_id"`
			OutputContractType    string `json:"output_contract_type"`
			OutputContractPayload string `json:"output_contract_payload"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		if req.ResumeFromStep <= 0 {
			req.ResumeFromStep = 1
		}
		var src struct {
			AgentID               string
			Task                  string
			WorkspacePath         string
			Provider              string
			Model                 string
			MaxSteps              int
			EnvironmentID         string
			VaultID               string
			OutputContractType    string
			OutputContractPayload string
		}
		err := db.QueryRowContext(r.Context(), `SELECT agent_id,task,workspace_path,provider,model,max_steps,environment_id,credential_vault_id,output_contract_type,output_contract_payload FROM runs WHERE id=?`, sourceRunID).
			Scan(&src.AgentID, &src.Task, &src.WorkspacePath, &src.Provider, &src.Model, &src.MaxSteps, &src.EnvironmentID, &src.VaultID, &src.OutputContractType, &src.OutputContractPayload)
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "source run not found"})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if req.Provider == "" {
			req.Provider = src.Provider
		}
		if req.Model == "" {
			req.Model = src.Model
		}
		if req.MaxSteps <= 0 {
			req.MaxSteps = src.MaxSteps
		}
		if req.MaxSteps <= 0 {
			req.MaxSteps = 30
		}
		if req.EnvironmentID == "" {
			req.EnvironmentID = src.EnvironmentID
		}
		if req.VaultID == "" {
			req.VaultID = src.VaultID
		}
		if strings.TrimSpace(req.OutputContractType) == "" {
			req.OutputContractType = src.OutputContractType
			req.OutputContractPayload = src.OutputContractPayload
		}
		if err := validateOutputContractDefinition(req.OutputContractType, req.OutputContractPayload); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if err := validateEnvironmentID(r.Context(), db, req.EnvironmentID); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if err := validateCredentialVaultID(r.Context(), db, req.VaultID); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}

		id := uuid.NewString()
		now := time.Now().UTC().Format(time.RFC3339Nano)
		workspacePath := filepath.Join(workspaceRoot, id)
		workspacePath, err = filepath.Abs(workspacePath)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid workspace_path"})
			return
		}
		if err := os.MkdirAll(workspacePath, 0o755); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create workspace"})
			return
		}
		if src.WorkspacePath != "" {
			if err := copyDirectory(src.WorkspacePath, workspacePath); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to copy source workspace: " + err.Error()})
				return
			}
		}
		_, err = db.ExecContext(r.Context(), `
			INSERT INTO runs(id,agent_id,status,task,workspace_path,provider,model,max_steps,replay_source_run_id,replay_from_step,environment_id,credential_vault_id,output_contract_type,output_contract_payload,created_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		`, id, src.AgentID, "queued", src.Task, workspacePath, req.Provider, req.Model, req.MaxSteps, sourceRunID, req.ResumeFromStep, req.EnvironmentID, req.VaultID, normalizeOutputContractType(req.OutputContractType), strings.TrimSpace(req.OutputContractPayload), now, now)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if _, err := appendEventWithRetry(r.Context(), db, id, "", "run.created", map[string]any{
			"status":               "queued",
			"replay_source_run_id": sourceRunID,
			"resume_from_step":     req.ResumeFromStep,
		}); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "run created but failed to append run.created event"})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"id": id, "status": "queued", "workspace_path": workspacePath})
	}
}

func listRunsHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := db.QueryContext(r.Context(), `SELECT id,agent_id,status,task,workspace_path,provider,model,max_steps,replay_source_run_id,replay_from_step,environment_id,credential_vault_id,output_contract_type,output_contract_payload,created_at,updated_at,started_at,completed_at,error FROM runs ORDER BY created_at DESC`)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		defer rows.Close()
		out := make([]map[string]any, 0)
		for rows.Next() {
			var id, agentID, status, task, ws, provider, model, createdAt, updatedAt, runErr, replaySource, environmentID, vaultID, outputContractType, outputContractPayload string
			var startedAt, completedAt sql.NullString
			var maxSteps, replayFromStep int
			if err := rows.Scan(&id, &agentID, &status, &task, &ws, &provider, &model, &maxSteps, &replaySource, &replayFromStep, &environmentID, &vaultID, &outputContractType, &outputContractPayload, &createdAt, &updatedAt, &startedAt, &completedAt, &runErr); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			out = append(out, map[string]any{
				"id": id, "agent_id": agentID, "status": status, "task": task, "workspace_path": ws,
				"provider": provider, "model": model, "max_steps": maxSteps,
				"replay_source_run_id": replaySource, "replay_from_step": replayFromStep,
				"environment_id": environmentID, "credential_vault_id": vaultID,
				"output_contract_type": outputContractType, "output_contract_payload": outputContractPayload,
				"created_at": createdAt, "updated_at": updatedAt, "started_at": startedAt.String, "completed_at": completedAt.String, "error": runErr,
			})
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func getRunHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		runID := chi.URLParam(r, "id")
		var id, agentID, status, task, ws, provider, model, createdAt, updatedAt, runErr, replaySource, environmentID, vaultID, outputContractType, outputContractPayload string
		var startedAt, completedAt sql.NullString
		var maxSteps, replayFromStep int
		err := db.QueryRowContext(r.Context(), `SELECT id,agent_id,status,task,workspace_path,provider,model,max_steps,replay_source_run_id,replay_from_step,environment_id,credential_vault_id,output_contract_type,output_contract_payload,created_at,updated_at,started_at,completed_at,error FROM runs WHERE id=?`, runID).
			Scan(&id, &agentID, &status, &task, &ws, &provider, &model, &maxSteps, &replaySource, &replayFromStep, &environmentID, &vaultID, &outputContractType, &outputContractPayload, &createdAt, &updatedAt, &startedAt, &completedAt, &runErr)
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "run not found"})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id": id, "agent_id": agentID, "status": status, "task": task, "workspace_path": ws,
			"provider": provider, "model": model, "max_steps": maxSteps,
			"replay_source_run_id": replaySource, "replay_from_step": replayFromStep,
			"environment_id": environmentID, "credential_vault_id": vaultID,
			"output_contract_type": outputContractType, "output_contract_payload": outputContractPayload,
			"created_at": createdAt, "updated_at": updatedAt, "started_at": startedAt, "completed_at": completedAt, "error": runErr,
		})
	}
}

func getRunTraceHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		runID := chi.URLParam(r, "id")
		rows, err := db.QueryContext(r.Context(), `SELECT id,step_id,event_type,seq,payload_json,created_at FROM events WHERE run_id=? ORDER BY seq ASC`, runID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		defer rows.Close()
		out := make([]map[string]any, 0)
		for rows.Next() {
			var id, stepID, typ, payload, createdAt string
			var seq int
			if err := rows.Scan(&id, &stepID, &typ, &seq, &payload, &createdAt); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			out = append(out, map[string]any{"id": id, "step_id": stepID, "event_type": typ, "seq": seq, "payload": json.RawMessage(payload), "created_at": createdAt})
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func getRunTelemetryHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		runID := chi.URLParam(r, "id")
		var runCount int
		if err := db.QueryRowContext(r.Context(), `SELECT COUNT(1) FROM runs WHERE id=?`, runID).Scan(&runCount); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if runCount == 0 {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "run not found"})
			return
		}

		steps, err := fetchRows(r.Context(), db, `SELECT id,idx,step_type,status,input_json,output_json,error,created_at,started_at,ended_at FROM run_steps WHERE run_id=? ORDER BY idx ASC`, runID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		toolCalls, err := fetchRows(r.Context(), db, `SELECT id,step_id,tool_name,tool_version,input_json,output_json,status,started_at,ended_at,error_class,error_message FROM tool_calls WHERE run_id=? ORDER BY started_at ASC`, runID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		spans, err := fetchRows(r.Context(), db, `SELECT id,parent_id,name,kind,status,started_at,ended_at,attrs_json FROM trace_spans WHERE run_id=? ORDER BY started_at ASC`, runID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		events, err := fetchRows(r.Context(), db, `SELECT id,step_id,event_type,seq,payload_json,created_at FROM events WHERE run_id=? ORDER BY seq ASC`, runID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		payload := map[string]any{
			"steps":      steps,
			"tool_calls": toolCalls,
			"spans":      spans,
			"events":     events,
		}
		writeJSON(w, http.StatusOK, payload)
	}
}

func getRunArtifactsHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		runID := chi.URLParam(r, "id")
		rows, err := db.QueryContext(r.Context(), `SELECT id,step_id,kind,path,mime_type,size_bytes,created_at FROM artifacts WHERE run_id=? ORDER BY created_at DESC`, runID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		defer rows.Close()
		out := make([]map[string]any, 0)
		for rows.Next() {
			var id, stepID, kind, path, mime, createdAt string
			var size int64
			if err := rows.Scan(&id, &stepID, &kind, &path, &mime, &size, &createdAt); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			out = append(out, map[string]any{"id": id, "step_id": stepID, "kind": kind, "path": path, "mime_type": mime, "size_bytes": size, "created_at": createdAt})
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func getRunArtifactContentHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		runID := chi.URLParam(r, "id")
		artifactID := chi.URLParam(r, "artifactID")
		var path, mime string
		err := db.QueryRowContext(r.Context(), `SELECT path,mime_type FROM artifacts WHERE run_id=? AND id=?`, runID, artifactID).Scan(&path, &mime)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "artifact not found"})
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if strings.HasPrefix(path, "inline://") {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "inline artifacts do not have file content"})
			return
		}
		cleanPath := filepath.Clean(path)
		info, err := os.Stat(cleanPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "artifact file not found"})
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if info.IsDir() {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "artifact path is a directory"})
			return
		}
		content, err := os.ReadFile(cleanPath)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if strings.TrimSpace(mime) == "" {
			mime = http.DetectContentType(content)
		}
		if mime != "" {
			w.Header().Set("Content-Type", mime)
		}
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(content)
	}
}

func updateRunStatusHandler(db *sql.DB, status string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		runID := chi.URLParam(r, "id")
		var currentStatus string
		if err := db.QueryRowContext(r.Context(), `SELECT status FROM runs WHERE id=?`, runID).Scan(&currentStatus); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "run not found"})
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if !canTransitionRunStatus(currentStatus, status) {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": fmt.Sprintf("invalid run status transition: %s -> %s", currentStatus, status),
			})
			return
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		res, err := db.ExecContext(r.Context(), `UPDATE runs SET status=?, updated_at=? WHERE id=? AND status=?`, status, now, runID, currentStatus)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		affected, _ := res.RowsAffected()
		if affected == 0 {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "run status changed concurrently; retry"})
			return
		}
		if _, err := appendEventWithRetry(r.Context(), db, runID, "", "run.status", map[string]any{"status": status}); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": fmt.Sprintf("status updated but failed to append run.status event: %v", err)})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": status})
	}
}

func approveLatestHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		runID := chi.URLParam(r, "id")
		now := time.Now().UTC().Format(time.RFC3339Nano)
		tx, err := db.BeginTx(r.Context(), nil)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		var hasPolicyPending int
		_ = tx.QueryRowContext(r.Context(),
			`SELECT COUNT(1) FROM approvals WHERE run_id=? AND status='pending' AND source='policy'`, runID).Scan(&hasPolicyPending)
		resApproval, err := tx.ExecContext(r.Context(), `UPDATE approvals SET status='approved', decided_at=?, decided_by='operator' WHERE run_id=? AND status='pending'`, now, runID)
		if err != nil {
			_ = tx.Rollback()
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		approvalAffected, _ := resApproval.RowsAffected()
		if approvalAffected == 0 {
			_ = tx.Rollback()
			writeJSON(w, http.StatusConflict, map[string]string{"error": "no pending approvals for this run"})
			return
		}
		// Only policy-sourced approvals interrupt the run; model-sourced
		// approvals keep the run running while the tool polls for a decision.
		if hasPolicyPending > 0 {
			resRun, err := tx.ExecContext(r.Context(), `UPDATE runs SET status='queued', updated_at=? WHERE id=? AND status='interrupted'`, now, runID)
			if err != nil {
				_ = tx.Rollback()
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			runAffected, _ := resRun.RowsAffected()
			if runAffected == 0 {
				_ = tx.Rollback()
				writeJSON(w, http.StatusConflict, map[string]string{"error": "run is not interrupted"})
				return
			}
		}
		if err := tx.Commit(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if _, err := appendEventWithRetry(r.Context(), db, runID, "", "approval.approved", map[string]any{"by": "operator"}); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "approved"})
	}
}

func canTransitionRunStatus(from, to string) bool {
	from = strings.TrimSpace(strings.ToLower(from))
	to = strings.TrimSpace(strings.ToLower(to))
	switch to {
	case "queued":
		return from == "interrupted"
	case "interrupted":
		return from == "queued" || from == "running"
	case "cancelled":
		return from == "queued" || from == "running" || from == "interrupted"
	default:
		return false
	}
}

func appendRunEventHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		runID := chi.URLParam(r, "id")
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		message := extractSteeringMessage(payload)
		if message == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "message required"})
			return
		}
		normalized := map[string]any{"message": message}
		for key, value := range payload {
			normalized[key] = value
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		seq, err := appendEventWithRetry(r.Context(), db, runID, "", "user.event", normalized)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if _, err := db.ExecContext(
			r.Context(),
			`UPDATE runs SET status='queued', completed_at=NULL, error='', updated_at=? WHERE id=? AND status IN ('interrupted','completed','failed','cancelled')`,
			now,
			runID,
		); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		var status string
		_ = db.QueryRowContext(r.Context(), `SELECT status FROM runs WHERE id=?`, runID).Scan(&status)
		writeJSON(w, http.StatusCreated, map[string]any{"seq": seq, "status": status})
	}
}

func extractSteeringMessage(payload map[string]any) string {
	for _, key := range []string{"message", "text", "content", "instruction"} {
		value, ok := payload[key]
		if !ok {
			continue
		}
		msg := strings.TrimSpace(fmt.Sprint(value))
		if msg != "" {
			return msg
		}
	}
	return ""
}

func providersHandler(availableProviders map[string]bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		out := make([]map[string]any, 0, 2)
		if availableProviders["anthropic"] {
			out = append(out, map[string]any{
				"provider":           "anthropic",
				"supports_tools":     true,
				"supports_streaming": true,
			})
		}
		if availableProviders["openai"] {
			out = append(out, map[string]any{
				"provider":           "openai",
				"supports_tools":     true,
				"supports_streaming": true,
			})
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func openAIModelsHandler(openAIKey string) http.HandlerFunc {
	type model struct {
		ID string `json:"id"`
	}
	type modelListResponse struct {
		Data []model `json:"data"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.TrimSpace(openAIKey) == "" {
			writeJSON(w, http.StatusOK, []string{})
			return
		}
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, "https://api.openai.com/v1/models", nil)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to build openai models request"})
			return
		}
		req.Header.Set("Authorization", "Bearer "+openAIKey)
		client := &http.Client{Timeout: 15 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "openai models request failed"})
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": fmt.Sprintf("openai models request failed with status %d", resp.StatusCode)})
			return
		}
		var parsed modelListResponse
		if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "failed to parse openai models response"})
			return
		}
		seen := map[string]bool{}
		models := make([]string, 0, len(parsed.Data))
		for _, row := range parsed.Data {
			id := strings.TrimSpace(row.ID)
			if id == "" || !isUsableOpenAIModel(id) || seen[id] {
				continue
			}
			seen[id] = true
			models = append(models, id)
		}
		sort.Strings(models)
		writeJSON(w, http.StatusOK, models)
	}
}

func isUsableOpenAIModel(id string) bool {
	lower := strings.ToLower(id)
	if strings.Contains(lower, "audio") ||
		strings.Contains(lower, "tts") ||
		strings.Contains(lower, "transcribe") ||
		strings.Contains(lower, "whisper") ||
		strings.Contains(lower, "embedding") ||
		strings.Contains(lower, "moderation") ||
		strings.Contains(lower, "image") ||
		strings.Contains(lower, "realtime") {
		return false
	}
	return strings.HasPrefix(lower, "gpt-") ||
		strings.HasPrefix(lower, "o1") ||
		strings.HasPrefix(lower, "o3") ||
		strings.HasPrefix(lower, "o4")
}

func listPoliciesHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := fetchRows(r.Context(), db, `SELECT id,name,definition_json,enabled,created_at,updated_at FROM policies ORDER BY created_at DESC`)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, rows)
	}
}

func createPolicyHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name       string          `json:"name"`
			Definition json.RawMessage `json:"definition"`
			Enabled    bool            `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		if req.Name == "" || len(req.Definition) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name and definition required"})
			return
		}
		id := uuid.NewString()
		now := time.Now().UTC().Format(time.RFC3339Nano)
		enabled := 0
		if req.Enabled {
			enabled = 1
		}
		_, err := db.ExecContext(r.Context(), `INSERT INTO policies(id,name,definition_json,enabled,created_at,updated_at) VALUES(?,?,?,?,?,?)`, id, req.Name, string(req.Definition), enabled, now, now)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"id": id})
	}
}

func updatePolicyHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		var req struct {
			Name       string          `json:"name"`
			Definition json.RawMessage `json:"definition"`
			Enabled    bool            `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		enabled := 0
		if req.Enabled {
			enabled = 1
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		res, err := db.ExecContext(r.Context(), `UPDATE policies SET name=?, definition_json=?, enabled=?, updated_at=? WHERE id=?`, req.Name, string(req.Definition), enabled, now, id)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "policy not found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"id": id})
	}
}

func deletePolicyHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		res, err := db.ExecContext(r.Context(), `DELETE FROM policies WHERE id=?`, id)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "policy not found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
	}
}

func listSecretsHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := fetchRows(r.Context(), db, `SELECT name,created_at,updated_at FROM secrets ORDER BY updated_at DESC`)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, rows)
	}
}

func deleteSecretHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := chi.URLParam(r, "name")
		res, err := db.ExecContext(r.Context(), `DELETE FROM secrets WHERE name=?`, name)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "secret not found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
	}
}

func listProviderConfigsHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := fetchRows(r.Context(), db, `SELECT id,provider,name,config_json,created_at,updated_at FROM provider_configs ORDER BY updated_at DESC`)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, rows)
	}
}

func createProviderConfigHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Provider string          `json:"provider"`
			Name     string          `json:"name"`
			Config   json.RawMessage `json:"config"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		if req.Provider == "" || req.Name == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "provider and name required"})
			return
		}
		if len(req.Config) == 0 {
			req.Config = json.RawMessage(`{}`)
		}
		id := uuid.NewString()
		now := time.Now().UTC().Format(time.RFC3339Nano)
		_, err := db.ExecContext(r.Context(), `INSERT INTO provider_configs(id,provider,name,config_json,created_at,updated_at) VALUES(?,?,?,?,?,?)`, id, req.Provider, req.Name, string(req.Config), now, now)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"id": id})
	}
}

func updateProviderConfigHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		var req struct {
			Provider string          `json:"provider"`
			Name     string          `json:"name"`
			Config   json.RawMessage `json:"config"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		if len(req.Config) == 0 {
			req.Config = json.RawMessage(`{}`)
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		res, err := db.ExecContext(r.Context(), `UPDATE provider_configs SET provider=?, name=?, config_json=?, updated_at=? WHERE id=?`, req.Provider, req.Name, string(req.Config), now, id)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "provider config not found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"id": id})
	}
}

func deleteProviderConfigHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		res, err := db.ExecContext(r.Context(), `DELETE FROM provider_configs WHERE id=?`, id)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "provider config not found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
	}
}

func testProviderConfigHandler(db *sql.DB, openAIKey, anthropicKey string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		var provider, configJSON string
		row := db.QueryRowContext(r.Context(), `SELECT provider, config_json FROM provider_configs WHERE id=?`, id)
		if err := row.Scan(&provider, &configJSON); err != nil {
			if err == sql.ErrNoRows {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "provider config not found"})
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		cfg := map[string]any{}
		if strings.TrimSpace(configJSON) != "" {
			if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "config_json is not valid JSON"})
				return
			}
		}
		apiKey := strings.TrimSpace(stringFromAny(cfg["api_key"]))
		baseURL := strings.TrimSpace(stringFromAny(cfg["base_url"]))

		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()

		providerLower := strings.ToLower(strings.TrimSpace(provider))
		var url, effectiveKey, authHeader string
		switch providerLower {
		case "openai":
			if baseURL == "" {
				baseURL = "https://api.openai.com"
			}
			url = strings.TrimRight(baseURL, "/") + "/v1/models"
			if apiKey == "" {
				apiKey = openAIKey
			}
			if apiKey == "" {
				writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "no api_key configured and OPENAI_API_KEY is not set", "latency_ms": 0})
				return
			}
			effectiveKey = apiKey
			authHeader = "Bearer " + effectiveKey
		case "anthropic":
			if baseURL == "" {
				baseURL = "https://api.anthropic.com"
			}
			url = strings.TrimRight(baseURL, "/") + "/v1/models"
			if apiKey == "" {
				apiKey = anthropicKey
			}
			if apiKey == "" {
				writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "no api_key configured and ANTHROPIC_API_KEY is not set", "latency_ms": 0})
				return
			}
			effectiveKey = apiKey
		default:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("unsupported provider %q", provider)})
			return
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": err.Error(), "latency_ms": 0})
			return
		}
		if providerLower == "anthropic" {
			req.Header.Set("x-api-key", effectiveKey)
			req.Header.Set("anthropic-version", "2023-06-01")
		} else {
			req.Header.Set("Authorization", authHeader)
		}

		start := time.Now()
		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Do(req)
		elapsed := time.Since(start).Milliseconds()
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": err.Error(), "latency_ms": elapsed})
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			msg := fmt.Sprintf("HTTP %d", resp.StatusCode)
			if len(body) > 0 {
				msg = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
			}
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": msg, "latency_ms": elapsed})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "reachable", "latency_ms": elapsed})
	}
}

func stringFromAny(v any) string {
	if v == nil {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}

func listEnvironmentsHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := fetchRows(r.Context(), db, `SELECT id,name,description,config_json,enabled,created_at,updated_at FROM environments ORDER BY updated_at DESC`)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, rows)
	}
}

func createEnvironmentHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Config      json.RawMessage `json:"config"`
			Enabled     bool            `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		if strings.TrimSpace(req.Name) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name required"})
			return
		}
		if len(req.Config) == 0 {
			req.Config = json.RawMessage(`{}`)
		}
		id := uuid.NewString()
		now := time.Now().UTC().Format(time.RFC3339Nano)
		enabled := 0
		if req.Enabled {
			enabled = 1
		}
		_, err := db.ExecContext(r.Context(), `INSERT INTO environments(id,name,description,config_json,enabled,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`,
			id, strings.TrimSpace(req.Name), req.Description, string(req.Config), enabled, now, now)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"id": id})
	}
}

func updateEnvironmentHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		var req struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Config      json.RawMessage `json:"config"`
			Enabled     bool            `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		if len(req.Config) == 0 {
			req.Config = json.RawMessage(`{}`)
		}
		enabled := 0
		if req.Enabled {
			enabled = 1
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		res, err := db.ExecContext(r.Context(), `UPDATE environments SET name=?,description=?,config_json=?,enabled=?,updated_at=? WHERE id=?`,
			strings.TrimSpace(req.Name), req.Description, string(req.Config), enabled, now, id)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "environment not found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"id": id})
	}
}

func deleteEnvironmentHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		var runCount int
		if err := db.QueryRowContext(r.Context(), `SELECT COUNT(1) FROM runs WHERE environment_id=?`, id).Scan(&runCount); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if runCount > 0 {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "environment is referenced by sessions"})
			return
		}
		res, err := db.ExecContext(r.Context(), `DELETE FROM environments WHERE id=?`, id)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "environment not found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
	}
}

func listCredentialVaultsHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := fetchRows(r.Context(), db, `
			SELECT v.id,v.name,v.description,v.created_at,v.updated_at,COUNT(i.id) AS item_count
			FROM credential_vaults v
			LEFT JOIN credential_vault_items i ON i.vault_id=v.id
			GROUP BY v.id
			ORDER BY v.updated_at DESC
		`)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, rows)
	}
}

func createCredentialVaultHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		if strings.TrimSpace(req.Name) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name required"})
			return
		}
		id := uuid.NewString()
		now := time.Now().UTC().Format(time.RFC3339Nano)
		_, err := db.ExecContext(r.Context(), `INSERT INTO credential_vaults(id,name,description,created_at,updated_at) VALUES(?,?,?,?,?)`,
			id, strings.TrimSpace(req.Name), req.Description, now, now)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"id": id})
	}
}

func updateCredentialVaultHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		var req struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		res, err := db.ExecContext(r.Context(), `UPDATE credential_vaults SET name=?,description=?,updated_at=? WHERE id=?`,
			strings.TrimSpace(req.Name), req.Description, now, id)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "credential vault not found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"id": id})
	}
}

func deleteCredentialVaultHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		var runCount int
		if err := db.QueryRowContext(r.Context(), `SELECT COUNT(1) FROM runs WHERE credential_vault_id=?`, id).Scan(&runCount); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if runCount > 0 {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "credential vault is referenced by sessions"})
			return
		}
		res, err := db.ExecContext(r.Context(), `DELETE FROM credential_vaults WHERE id=?`, id)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "credential vault not found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
	}
}

func listCredentialVaultItemsHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vaultID := chi.URLParam(r, "id")
		rows, err := fetchRows(r.Context(), db, `
			SELECT id,vault_id,secret_name,alias,created_at,updated_at
			FROM credential_vault_items
			WHERE vault_id=?
			ORDER BY updated_at DESC
		`, vaultID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, rows)
	}
}

func upsertCredentialVaultItemHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vaultID := chi.URLParam(r, "id")
		var req struct {
			SecretName string `json:"secret_name"`
			Alias      string `json:"alias"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		if strings.TrimSpace(req.SecretName) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "secret_name required"})
			return
		}
		var exists int
		if err := db.QueryRowContext(r.Context(), `SELECT COUNT(1) FROM credential_vaults WHERE id=?`, vaultID).Scan(&exists); err != nil || exists == 0 {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "credential vault not found"})
			return
		}
		if err := db.QueryRowContext(r.Context(), `SELECT COUNT(1) FROM secrets WHERE name=?`, req.SecretName).Scan(&exists); err != nil || exists == 0 {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "secret not found"})
			return
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		id := uuid.NewString()
		_, err := db.ExecContext(r.Context(), `
			INSERT INTO credential_vault_items(id,vault_id,secret_name,alias,created_at,updated_at)
			VALUES(?,?,?,?,?,?)
			ON CONFLICT(vault_id,secret_name) DO UPDATE SET alias=excluded.alias, updated_at=excluded.updated_at
		`, id, vaultID, strings.TrimSpace(req.SecretName), strings.TrimSpace(req.Alias), now, now)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"vault_id": vaultID, "secret_name": req.SecretName})
	}
}

func deleteCredentialVaultItemHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vaultID := chi.URLParam(r, "id")
		secretName := chi.URLParam(r, "secretName")
		res, err := db.ExecContext(r.Context(), `DELETE FROM credential_vault_items WHERE vault_id=? AND secret_name=?`, vaultID, secretName)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "vault secret binding not found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
	}
}

func createSecretHandler(db *sql.DB, secretKey string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		if req.Name == "" || req.Value == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name and value required"})
			return
		}
		if strings.TrimSpace(secretKey) == "" {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "COLOSSEUM_SECRET_KEY is required before storing secrets"})
			return
		}
		id := uuid.NewString()
		now := time.Now().UTC().Format(time.RFC3339Nano)
		cipher, err := secrets.Encrypt(req.Value, secretKey)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to encrypt secret"})
			return
		}
		_, err = db.ExecContext(r.Context(), `INSERT INTO secrets(id,name,cipher_text,created_at,updated_at) VALUES(?,?,?,?,?) ON CONFLICT(name) DO UPDATE SET cipher_text=excluded.cipher_text, updated_at=excluded.updated_at`, id, req.Name, cipher, now, now)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"name": req.Name})
	}
}

func streamRunEventsHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		runID := chi.URLParam(r, "id")
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "stream unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		lastSeq := 0
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		send := func(ctx context.Context) error {
			rows, err := db.QueryContext(ctx, `SELECT id,step_id,event_type,seq,payload_json,created_at FROM events WHERE run_id=? AND seq>? ORDER BY seq ASC LIMIT 200`, runID, lastSeq)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var id, stepID, typ, payload, createdAt string
				var seq int
				if err := rows.Scan(&id, &stepID, &typ, &seq, &payload, &createdAt); err != nil {
					return err
				}
				lastSeq = seq
				evt := map[string]any{"id": id, "step_id": stepID, "event_type": typ, "seq": seq, "payload": json.RawMessage(payload), "created_at": createdAt}
				b, _ := json.Marshal(evt)
				if _, err := fmt.Fprintf(w, "event: run_event\ndata: %s\n\n", string(b)); err != nil {
					return err
				}
				flusher.Flush()
			}
			return nil
		}

		if err := send(r.Context()); err != nil {
			return
		}
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				if err := send(r.Context()); err != nil {
					return
				}
				_, _ = fmt.Fprint(w, ": ping\n\n")
				flusher.Flush()
			}
		}
	}
}

func uploadRunFilesHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		runID := chi.URLParam(r, "id")
		var workspacePath string
		var runStatus string
		err := db.QueryRowContext(r.Context(), `SELECT workspace_path,status FROM runs WHERE id=?`, runID).Scan(&workspacePath, &runStatus)
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "run not found"})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		switch runStatus {
		case "completed", "failed", "cancelled":
			writeJSON(w, http.StatusConflict, map[string]string{"error": "run is closed; cannot upload files"})
			return
		}

		if err := r.ParseMultipartForm(64 << 20); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid multipart payload"})
			return
		}
		files := r.MultipartForm.File["files"]
		if len(files) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no files uploaded"})
			return
		}
		nowTs := time.Now().UTC().Format(time.RFC3339Nano)
		uploadRoot := filepath.Join(workspacePath, "uploads")
		if err := os.MkdirAll(uploadRoot, 0o755); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create upload directory"})
			return
		}
		uploaded := make([]map[string]any, 0, len(files))
		for _, fileHeader := range files {
			if fileHeader == nil {
				continue
			}
			src, err := fileHeader.Open()
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to read uploaded file"})
				return
			}
			baseName := sanitizeUploadFilename(fileHeader.Filename)
			dstPath := uniqueFilePath(uploadRoot, baseName)
			dst, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				_ = src.Close()
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to write uploaded file"})
				return
			}
			written, copyErr := io.Copy(dst, src)
			_ = dst.Close()
			_ = src.Close()
			if copyErr != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to copy uploaded file"})
				return
			}
			if _, err := db.ExecContext(r.Context(), `
				INSERT INTO artifacts(id,run_id,step_id,kind,path,mime_type,size_bytes,created_at)
				VALUES(?,?,?,?,?,?,?,?)
			`, uuid.NewString(), runID, "", "uploaded_file", dstPath, fileHeader.Header.Get("Content-Type"), written, nowTs); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to register uploaded artifact"})
				return
			}
			relPath := strings.TrimPrefix(strings.ReplaceAll(dstPath, filepath.Clean(workspacePath), ""), string(filepath.Separator))
			uploaded = append(uploaded, map[string]any{
				"name":       baseName,
				"path":       relPath,
				"size_bytes": written,
			})
		}
		if len(uploaded) > 0 {
			if _, err := appendEventWithRetry(r.Context(), db, runID, "", "run.files_uploaded", map[string]any{"count": len(uploaded), "files": uploaded}); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to append upload event: " + err.Error()})
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"uploaded": uploaded, "count": len(uploaded)})
	}
}

func sanitizeUploadFilename(name string) string {
	clean := strings.TrimSpace(filepath.Base(name))
	if clean == "" || clean == "." || clean == string(filepath.Separator) {
		return "upload.bin"
	}
	clean = strings.ReplaceAll(clean, "..", "_")
	return clean
}

func uniqueFilePath(rootDir, fileName string) string {
	target := filepath.Join(rootDir, fileName)
	if _, err := os.Stat(target); errors.Is(err, os.ErrNotExist) {
		return target
	}
	ext := filepath.Ext(fileName)
	base := strings.TrimSuffix(fileName, ext)
	for i := 1; i <= 9999; i++ {
		candidate := filepath.Join(rootDir, fmt.Sprintf("%s-%d%s", base, i, ext))
		if _, err := os.Stat(candidate); errors.Is(err, os.ErrNotExist) {
			return candidate
		}
	}
	return filepath.Join(rootDir, fmt.Sprintf("%s-%d%s", base, time.Now().UnixNano(), ext))
}

func exportRunBundleHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		runID := chi.URLParam(r, "id")
		var id, agentID, status, task, workspace, provider, model, createdAt, updatedAt, runErr string
		var startedAt, completedAt sql.NullString
		var maxSteps int
		err := db.QueryRowContext(r.Context(), `SELECT id,agent_id,status,task,workspace_path,provider,model,max_steps,created_at,updated_at,started_at,completed_at,error FROM runs WHERE id=?`, runID).
			Scan(&id, &agentID, &status, &task, &workspace, &provider, &model, &maxSteps, &createdAt, &updatedAt, &startedAt, &completedAt, &runErr)
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "run not found"})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}

		events, err := fetchRows(r.Context(), db, `SELECT id,step_id,event_type,seq,payload_json,created_at FROM events WHERE run_id=? ORDER BY seq ASC`, runID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		steps, err := fetchRows(r.Context(), db, `SELECT id,idx,step_type,status,input_json,output_json,error,created_at,started_at,ended_at FROM run_steps WHERE run_id=? ORDER BY idx ASC`, runID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		toolCalls, err := fetchRows(r.Context(), db, `SELECT id,step_id,tool_name,input_json,output_json,status,started_at,ended_at,error_class,error_message FROM tool_calls WHERE run_id=? ORDER BY started_at ASC`, runID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		traceSpans, err := fetchRows(r.Context(), db, `SELECT id,parent_id,name,kind,status,started_at,ended_at,attrs_json FROM trace_spans WHERE run_id=? ORDER BY started_at ASC`, runID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		artifacts, err := fetchRows(r.Context(), db, `SELECT id,step_id,kind,path,mime_type,size_bytes,created_at FROM artifacts WHERE run_id=? ORDER BY created_at ASC`, runID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}

		bundle := map[string]any{
			"run": map[string]any{
				"id": id, "agent_id": agentID, "status": status, "task": task,
				"workspace_path": workspace, "provider": provider, "model": model,
				"max_steps": maxSteps, "created_at": createdAt, "updated_at": updatedAt,
				"started_at": startedAt.String, "completed_at": completedAt.String, "error": runErr,
			},
			"events":      events,
			"steps":       steps,
			"tool_calls":  toolCalls,
			"trace_spans": traceSpans,
			"artifacts":   artifacts,
		}
		w.Header().Set("Content-Disposition", "attachment; filename=run-"+runID+".json")
		writeJSON(w, http.StatusOK, bundle)
	}
}

func dbRow(ctx context.Context, db *sql.DB, query string, args ...any) map[string]any {
	rows, err := fetchRows(ctx, db, query, args...)
	if err != nil {
		return map[string]any{}
	}
	if len(rows) == 0 {
		return map[string]any{}
	}
	return rows[0]
}

func fetchRows(ctx context.Context, db *sql.DB, query string, args ...any) ([]map[string]any, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	out := make([]map[string]any, 0)
	for rows.Next() {
		values := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := map[string]any{}
		for i, c := range cols {
			switch v := values[i].(type) {
			case []byte:
				row[c] = string(v)
			default:
				row[c] = v
			}
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func ensurePathWithinRoot(rootPath, targetPath string) (string, error) {
	absRoot, err := filepath.Abs(rootPath)
	if err != nil {
		return "", err
	}
	absTarget, err := filepath.Abs(targetPath)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(absRoot, absTarget)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", errors.New("path escapes workspace root")
	}
	return absTarget, nil
}

func appendEventWithRetry(ctx context.Context, db *sql.DB, runID, stepID, eventType string, payload map[string]any) (int, error) {
	const maxAttempts = 3
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		seq, appendErr := appendEventOnce(ctx, db, runID, stepID, eventType, string(body))
		if appendErr == nil {
			return seq, nil
		}
		if !isEventSequenceConflict(appendErr) {
			return 0, appendErr
		}
	}
	return 0, fmt.Errorf("failed to append event after retries")
}

func appendEventOnce(ctx context.Context, db *sql.DB, runID, stepID, eventType, payload string) (int, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var seq int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq),0)+1 FROM events WHERE run_id=?`, runID).Scan(&seq); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO events(id,run_id,step_id,event_type,seq,payload_json,created_at) VALUES(?,?,?,?,?,?,?)`,
		uuid.NewString(), runID, stepID, eventType, seq, payload, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return seq, nil
}

func isEventSequenceConflict(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique constraint failed") && strings.Contains(msg, "events.run_id") && strings.Contains(msg, "events.seq")
}

func parseTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func copyDirectory(source, target string) error {
	return filepath.WalkDir(source, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		dstPath := filepath.Join(target, rel)
		if d.IsDir() {
			return os.MkdirAll(dstPath, 0o755)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		srcFile, err := os.Open(path)
		if err != nil {
			return err
		}
		defer srcFile.Close()
		if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
			return err
		}
		dstFile, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
		if err != nil {
			return err
		}
		defer dstFile.Close()
		_, err = io.Copy(dstFile, srcFile)
		return err
	})
}
