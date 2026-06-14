package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/adevireddy/colosseum/internal/mcp"
	"github.com/adevireddy/colosseum/internal/policy"
	"github.com/adevireddy/colosseum/internal/providers"
	"github.com/adevireddy/colosseum/internal/secrets"
	"github.com/adevireddy/colosseum/internal/tools"
	"github.com/google/uuid"
)

type RunEventStore interface {
	AppendEvent(ctx context.Context, runID, stepID, eventType string, payload map[string]any) error
	GetEvents(ctx context.Context, runID string, afterSeq, limit int) ([]map[string]any, error)
	GetCheckpoint(ctx context.Context, runID string) (int, error)
}

type DBRunEventStore struct{ DB *sql.DB }

func (s *DBRunEventStore) AppendEvent(ctx context.Context, runID, stepID, eventType string, payload map[string]any) error {
	body, _ := json.Marshal(payload)
	const maxAttempts = 3
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		tx, err := s.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		var seq int
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq),0)+1 FROM events WHERE run_id=?`, runID).Scan(&seq); err != nil {
			_ = tx.Rollback()
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO events(id,run_id,step_id,event_type,seq,payload_json,created_at) VALUES(?,?,?,?,?,?,?)`, uuid.NewString(), runID, stepID, eventType, seq, string(body), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			_ = tx.Rollback()
			if isEventSequenceConflict(err) {
				continue
			}
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return nil
	}
	return fmt.Errorf("append event retries exceeded")
}

func (s *DBRunEventStore) GetEvents(ctx context.Context, runID string, afterSeq, limit int) ([]map[string]any, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT id,step_id,event_type,seq,payload_json,created_at FROM events WHERE run_id=? AND seq>? ORDER BY seq ASC LIMIT ?`, runID, afterSeq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]map[string]any, 0)
	for rows.Next() {
		var id, stepID, typ, payload, created string
		var seq int
		if err := rows.Scan(&id, &stepID, &typ, &seq, &payload, &created); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"id": id, "step_id": stepID, "event_type": typ, "seq": seq, "payload": json.RawMessage(payload), "created_at": created})
	}
	return out, nil
}

func (s *DBRunEventStore) GetCheckpoint(ctx context.Context, runID string) (int, error) {
	var max int
	if err := s.DB.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq),0) FROM events WHERE run_id=?`, runID).Scan(&max); err != nil {
		return 0, err
	}
	return max, nil
}

type Manager struct {
	DB           *sql.DB
	Store        RunEventStore
	Providers    map[string]providers.Client
	Tools        *tools.Executor
	SecretKey    string
	SandboxImage string
	wg           sync.WaitGroup
}

func NewManager(db *sql.DB, providerMap map[string]providers.Client, toolExec *tools.Executor, secretKey string, sandboxImage string) *Manager {
	store := &DBRunEventStore{DB: db}
	if toolExec != nil && toolExec.EventSink == nil {
		toolExec.EventSink = store
	}
	return &Manager{DB: db, Store: store, Providers: providerMap, Tools: toolExec, SecretKey: secretKey, SandboxImage: sandboxImage}
}

func (m *Manager) Start(ctx context.Context) {
	_ = m.RecoverInFlight(ctx)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.processOne(ctx)
		}
	}
}

func (m *Manager) RecoverInFlight(ctx context.Context) error {
	_, err := m.DB.ExecContext(ctx, `UPDATE runs SET status='queued', updated_at=? WHERE status='running'`, now())
	return err
}

// Wake allows any stateless harness instance to reattach to a run.
func (m *Manager) Wake(ctx context.Context, runID string) error {
	var count int
	if err := m.DB.QueryRowContext(ctx, `SELECT COUNT(1) FROM runs WHERE id=?`, runID).Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		return fmt.Errorf("run not found: %s", runID)
	}
	return nil
}

func (m *Manager) Interrupt(ctx context.Context, runID string) error {
	_, err := m.DB.ExecContext(ctx, `UPDATE runs SET status='interrupted', updated_at=? WHERE id=?`, now(), runID)
	return err
}

func (m *Manager) Resume(ctx context.Context, runID string) error {
	_, err := m.DB.ExecContext(ctx, `UPDATE runs SET status='queued', updated_at=? WHERE id=?`, now(), runID)
	return err
}

func (m *Manager) processOne(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	var runID, agentID, task, workspace, providerName, model, environmentID, vaultID string
	var maxSteps int
	err := m.DB.QueryRowContext(ctx, `
		SELECT r.id, r.agent_id, r.task, r.workspace_path, r.provider, r.model, r.max_steps, r.environment_id, r.credential_vault_id
		FROM runs r
		WHERE r.status='queued'
		  AND NOT EXISTS (
			SELECT 1
			FROM session_runs sr
			JOIN session_runs sr2 ON sr2.session_id = sr.session_id AND sr2.turn_index < sr.turn_index
			JOIN runs r2 ON r2.id = sr2.run_id
			WHERE sr.run_id = r.id
			  AND r2.status IN ('queued','running','interrupted')
		  )
		ORDER BY r.created_at ASC
		LIMIT 1
	`).Scan(&runID, &agentID, &task, &workspace, &providerName, &model, &maxSteps, &environmentID, &vaultID)
	if err == sql.ErrNoRows {
		return
	}
	if err != nil {
		log.Printf("runtime query queued run failed: %v", err)
		return
	}
	res, err := m.DB.ExecContext(ctx, `UPDATE runs SET status='running', started_at=?, updated_at=? WHERE id=? AND status='queued'`, now(), now(), runID)
	if err != nil {
		log.Printf("runtime claim run failed: %v", err)
		return
	}
	rowsAffected, err := res.RowsAffected()
	if err != nil {
		log.Printf("runtime claim run rows affected failed run_id=%s err=%v", runID, err)
		return
	}
	if rowsAffected == 0 {
		return
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		if err := m.run(ctx, runID, agentID, task, workspace, providerName, model, maxSteps, environmentID, vaultID); err != nil && ctx.Err() == nil {
			log.Printf("run %s failed: %v", runID, err)
		}
	}()
}

func (m *Manager) run(ctx context.Context, runID, agentID, task, workspace, providerName, model string, maxSteps int, environmentID, vaultID string) error {
	if ctx.Err() != nil {
		return nil
	}
	provider := m.Providers[providerName]
	if provider == nil {
		return m.failRun(ctx, runID, fmt.Errorf("provider %s not configured", providerName))
	}
	var systemPrompt, allowedToolsJSON string
	if err := m.DB.QueryRowContext(ctx, `SELECT system_prompt, allowed_tools FROM agents WHERE id=?`, agentID).Scan(&systemPrompt, &allowedToolsJSON); err != nil {
		return m.failRun(ctx, runID, err)
	}
	var allowedTools []string
	_ = json.Unmarshal([]byte(allowedToolsJSON), &allowedTools)
	systemPrompt = buildEffectiveSystemPrompt(systemPrompt, allowedTools)
	resources, err := m.loadSessionResources(ctx, runID, environmentID, vaultID)
	if err != nil {
		return m.failRun(ctx, runID, err)
	}
	if err := m.Store.AppendEvent(ctx, runID, "", "run.started", map[string]any{
		"provider": providerName, "model": model,
		"environment_id": resources.EnvironmentID, "environment_name": resources.EnvironmentName,
		"credential_vault_id": resources.VaultID, "injected_env_count": len(resources.EnvVars),
	}); err != nil {
		return m.failRun(ctx, runID, err)
	}
	mediaPolicy := buildMediaInputPolicy(providerName, model)

	messages := []providers.Message{{Role: "user", Content: task}}
	lastEventSeq := 0
	if err := m.appendPendingUserMessages(ctx, runID, &messages, &lastEventSeq, mediaPolicy); err != nil {
		return m.failRun(ctx, runID, fmt.Errorf("load user events: %w", err))
	}
	var replaySourceRunID string
	var replayFromStep int
	var outputContractType, outputContractPayload string
	_ = m.DB.QueryRowContext(ctx, `SELECT COALESCE(replay_source_run_id,''), COALESCE(replay_from_step,1), COALESCE(output_contract_type,'none'), COALESCE(output_contract_payload,'') FROM runs WHERE id=?`, runID).Scan(&replaySourceRunID, &replayFromStep, &outputContractType, &outputContractPayload)
	if replayFromStep <= 0 {
		replayFromStep = 1
	}
	if packed, ok := m.buildChatSessionContext(ctx, runID, task, workspace); ok {
		messages = packed.Messages
		// Re-apply any user.events that arrived before the run started,
		// so attachments uploaded just before sending still get inlined.
		lastEventSeq = 0
		if err := m.appendPendingUserMessages(ctx, runID, &messages, &lastEventSeq, mediaPolicy); err != nil {
			return m.failRun(ctx, runID, fmt.Errorf("load user events: %w", err))
		}
		_ = m.Store.AppendEvent(ctx, runID, "", "session.context_packed", packed.PackSummary)
	} else if replaySourceRunID != "" {
		var replayed int
		var replayErr error
		messages, replayed, replayErr = m.rebuildReplayContext(ctx, runID, replaySourceRunID, replayFromStep, task)
		if replayErr != nil {
			return m.failRun(ctx, runID, fmt.Errorf("replay bootstrap failed: %w", replayErr))
		}
		_ = m.Store.AppendEvent(ctx, runID, "", "replay.bootstrap", map[string]any{
			"source_run_id":    replaySourceRunID,
			"resume_from_step": replayFromStep,
			"replayed_steps":   replayed,
		})
	}
	var existingStepMax int
	_ = m.DB.QueryRowContext(ctx, `SELECT COALESCE(MAX(idx),0) FROM run_steps WHERE run_id=?`, runID).Scan(&existingStepMax)
	toolDefs, err := tools.ListDefinitions(ctx, m.DB, false)
	if err != nil {
		return m.failRun(ctx, runID, fmt.Errorf("list tools: %w", err))
	}
	providerTools := filterProviderTools(toolDefs, allowedTools)

	mcpSession := mcp.NewSession(ctx, m.DB, agentID)
	defer mcpSession.Close()
	if mcpTools := mcpSession.ListAllTools(ctx); len(mcpTools) > 0 {
		providerTools = append(providerTools, mcpTools...)
	}

	for step := 1; step <= maxSteps; step++ {
		if ctx.Err() != nil {
			_ = m.Store.AppendEvent(context.Background(), runID, "", "run.cancelled_by_shutdown", map[string]any{"reason": "context_cancelled"})
			return nil
		}
		status, _ := m.currentStatus(runID)
		if status == "cancelled" || status == "interrupted" {
			_ = m.Store.AppendEvent(ctx, runID, "", "run.stopped", map[string]any{"status": status})
			return nil
		}
		if err := m.appendPendingUserMessages(ctx, runID, &messages, &lastEventSeq, mediaPolicy); err != nil {
			return m.failRun(ctx, runID, fmt.Errorf("load user events: %w", err))
		}
		stepIdx := existingStepMax + step
		stepID := uuid.NewString()
		modelSpanID := uuid.NewString()
		inputJSON := buildLLMRequestSnapshotJSON(model, systemPrompt, messages, providerTools)
		if _, err := m.DB.ExecContext(ctx, `INSERT INTO run_steps(id,run_id,idx,step_type,status,input_json,created_at,started_at) VALUES(?,?,?,?,?,?,?,?)`, stepID, runID, stepIdx, "model", "running", inputJSON, now(), now()); err != nil {
			return m.failRun(ctx, runID, err)
		}
		m.execDB(ctx, "insert model trace span", `INSERT INTO trace_spans(id,run_id,parent_id,name,kind,status,started_at,attrs_json) VALUES(?,?,?,?,?,?,?,?)`, modelSpanID, runID, "", "model.step", "model", "running", now(), `{"step_id":"`+stepID+`"}`)

		resp, err := m.completeWithRetry(ctx, provider, providers.CompletionRequest{Model: model, System: systemPrompt, Messages: messages, Tools: providerTools, Timeout: 120 * time.Second})
		if err != nil {
			m.execDB(ctx, "mark run step failed", `UPDATE run_steps SET status='failed', error=?, ended_at=? WHERE id=?`, err.Error(), now(), stepID)
			m.execDB(ctx, "mark model span failed", `UPDATE trace_spans SET status='failed', ended_at=?, attrs_json=? WHERE id=?`, now(), `{"error":`+strconv.Quote(err.Error())+`}`, modelSpanID)
			return m.failRun(ctx, runID, err)
		}

		outJSON, _ := json.Marshal(map[string]any{"text": resp.Text, "tool_calls": resp.ToolCalls, "usage": resp.Usage})
		m.execDB(ctx, "mark run step completed", `UPDATE run_steps SET status='completed', output_json=?, ended_at=? WHERE id=?`, string(outJSON), now(), stepID)
		m.execDB(ctx, "mark model span completed", `UPDATE trace_spans SET status='completed', ended_at=? WHERE id=?`, now(), modelSpanID)
		_ = m.Store.AppendEvent(ctx, runID, stepID, "model.response", map[string]any{"text": resp.Text, "tool_calls": resp.ToolCalls, "usage": resp.Usage})

		if len(resp.ToolCalls) == 0 {
			if strings.TrimSpace(resp.Text) == "" {
				continue
			}
			// If new user events (for example, late file attachment notes) arrived during this model step,
			// defer finalization and let the next step answer with the freshest user context.
			msgCountBefore := len(messages)
			if err := m.appendPendingUserMessages(ctx, runID, &messages, &lastEventSeq, mediaPolicy); err != nil {
				return m.failRun(ctx, runID, fmt.Errorf("load user events: %w", err))
			}
			if len(messages) > msgCountBefore {
				_ = m.Store.AppendEvent(ctx, runID, stepID, "run.deferred_for_user_event", map[string]any{
					"new_user_messages": len(messages) - msgCountBefore,
				})
				continue
			}
			dispatchCtx, dispatchCtxErr := m.buildDispatchContext(ctx, runID)
			if dispatchCtxErr != nil {
				return m.failRun(ctx, runID, fmt.Errorf("build dispatch context failed: %w", dispatchCtxErr))
			}
			chatText, dispatchMeta := m.prepareChatAssistantText(
				ctx,
				provider,
				model,
				runID,
				task,
				resp.Text,
				dispatchCtx.ProvenanceSummary,
				dispatchCtx.Artifacts,
			)
			if dispatchMeta.Applied {
				_ = m.Store.AppendEvent(ctx, runID, stepID, "response.dispatch.succeeded", map[string]any{
					"reason":        dispatchMeta.Reason,
					"input_length":  len(resp.Text),
					"output_length": len(chatText),
				})
			} else if dispatchMeta.Error != "" {
				_ = m.Store.AppendEvent(ctx, runID, stepID, "response.dispatch.failed", map[string]any{
					"reason": dispatchMeta.Reason,
					"error":  truncateForEvent(dispatchMeta.Error, 220),
				})
			} else if dispatchMeta.Reason != "" {
				_ = m.Store.AppendEvent(ctx, runID, stepID, "response.dispatch.skipped", map[string]any{
					"reason": dispatchMeta.Reason,
				})
			}
			chatText = ensureAttachmentReferences(runID, chatText, resp.Text, dispatchCtx.Artifacts)
			provenanceValid, provenanceDetail := validateProvenanceOutputContract(chatText, dispatchCtx.Media)
			_ = m.Store.AppendEvent(ctx, runID, stepID, "output_contract.validated", map[string]any{
				"type":    "provenance_media",
				"passed":  provenanceValid,
				"details": truncateForEvent(provenanceDetail, 280),
			})
			if !provenanceValid {
				_ = m.Store.AppendEvent(ctx, runID, stepID, "output_contract.failed", map[string]any{
					"type":   "provenance_media",
					"reason": truncateForEvent(provenanceDetail, 280),
				})
				return m.failRun(ctx, runID, fmt.Errorf("output contract validation failed: %s", provenanceDetail))
			}
			valid, contractDetail := validateOutputContract(outputContractType, outputContractPayload, chatText)
			_ = m.Store.AppendEvent(ctx, runID, stepID, "output_contract.validated", map[string]any{
				"type":    normalizeContractType(outputContractType),
				"passed":  valid,
				"details": truncateForEvent(contractDetail, 280),
			})
			if !valid {
				_ = m.Store.AppendEvent(ctx, runID, stepID, "output_contract.failed", map[string]any{
					"type":   normalizeContractType(outputContractType),
					"reason": truncateForEvent(contractDetail, 280),
				})
				return m.failRun(ctx, runID, fmt.Errorf("output contract validation failed: %s", contractDetail))
			}
			m.appendChatMessageForRun(ctx, runID, "assistant", chatText, "model.response")
			m.execDB(ctx, "mark run completed", `UPDATE runs SET status='completed', completed_at=?, updated_at=? WHERE id=?`, now(), now(), runID)
			_ = m.Store.AppendEvent(ctx, runID, stepID, "run.completed", map[string]any{"result": chatText})
			m.recordChatTurnSummary(ctx, runID, task, chatText)
			return nil
		}

		m.appendChatMessageForRun(ctx, runID, "assistant", resp.Text, "model.response")

		messages = append(messages, providers.Message{Role: "assistant", Content: resp.Text, ToolCalls: resp.ToolCalls})
		for _, tc := range resp.ToolCalls {
			toolCallID := uuid.NewString()
			decision := policy.EvaluateTool(tc.Name, tc.Arguments, allowedTools)
			if !decision.Allow {
				_ = m.Store.AppendEvent(ctx, runID, stepID, "policy.denied", map[string]any{"tool": tc.Name, "reason": decision.Reason})
				return m.failRun(ctx, runID, fmt.Errorf("policy denied tool %s: %s", tc.Name, decision.Reason))
			}
			if decision.RequireApproval {
				approvalID := uuid.NewString()
				m.execDB(ctx, "insert approval", `INSERT INTO approvals(id,run_id,step_id,reason,status,requested_at) VALUES(?,?,?,?,?,?)`, approvalID, runID, stepID, decision.Reason, "pending", now())
				m.execDB(ctx, "mark run interrupted for approval", `UPDATE runs SET status='interrupted', updated_at=? WHERE id=?`, now(), runID)
				_ = m.Store.AppendEvent(ctx, runID, stepID, "approval.requested", map[string]any{"approval_id": approvalID, "reason": decision.Reason, "tool": tc.Name})
				return nil
			}
			m.execDB(ctx, "insert tool call", `INSERT INTO tool_calls(id,run_id,step_id,tool_name,input_json,status,started_at) VALUES(?,?,?,?,?,?,?)`, toolCallID, runID, stepID, tc.Name, string(tc.Arguments), "running", now())
			toolSpanID := uuid.NewString()
			m.execDB(ctx, "insert tool trace span", `INSERT INTO trace_spans(id,run_id,parent_id,name,kind,status,started_at,attrs_json) VALUES(?,?,?,?,?,?,?,?)`, toolSpanID, runID, modelSpanID, "tool."+tc.Name, "tool", "running", now(), `{"step_id":"`+stepID+`","tool_call_id":"`+toolCallID+`"}`)
			var result tools.Result
			var execErr error
			if _, _, isMCP := mcp.ParseToolName(tc.Name); isMCP {
				var text string
				text, execErr = mcpSession.CallTool(ctx, tc.Name, tc.Arguments)
				result = tools.Result{Output: map[string]any{"text": text}}
			} else {
				result, execErr = m.Tools.Execute(ctx, tools.Context{
					RunID: runID, StepID: stepID, Workspace: workspace,
					EnvironmentID: resources.EnvironmentID, CredentialVaultID: resources.VaultID,
					EnvVars: resources.EnvVars,
				}, tc.Name, tc.Arguments)
			}
			outputJSON, _ := json.Marshal(result.Output)
			status := "completed"
			errClass := ""
			errMsg := ""
			if execErr != nil {
				status = "failed"
				errClass = "tool_error"
				errMsg = execErr.Error()
			}
			m.execDB(ctx, "update tool call result", `UPDATE tool_calls SET output_json=?, status=?, ended_at=?, error_class=?, error_message=?, logs_path=? WHERE id=?`, string(outputJSON), status, now(), errClass, errMsg, "", toolCallID)
			m.execDB(ctx, "update tool trace span", `UPDATE trace_spans SET status=?, ended_at=?, attrs_json=? WHERE id=?`, status, now(), `{"error":`+strconv.Quote(errMsg)+`}`, toolSpanID)
			_ = m.Store.AppendEvent(ctx, runID, stepID, "tool.result", map[string]any{"tool": tc.Name, "status": status, "output": result.Output, "log": result.Log, "error": errMsg})
			if execErr != nil {
				messages = append(messages, providers.Message{Role: "tool", ToolCallID: tc.ID, Name: tc.Name, Content: `{"error":` + strconv.Quote(execErr.Error()) + `}`})
				continue
			}
			body, _ := json.Marshal(result.Output)
			messages = append(messages, providers.Message{Role: "tool", ToolCallID: tc.ID, Name: tc.Name, Content: string(body)})
		}
	}
	return m.failRun(ctx, runID, fmt.Errorf("max steps reached"))
}

func (m *Manager) completeWithRetry(ctx context.Context, provider providers.Client, req providers.CompletionRequest) (providers.CompletionResponse, error) {
	var lastErr error
	backoff := 1 * time.Second
	for attempt := 1; attempt <= 3; attempt++ {
		resp, err := provider.Complete(ctx, req)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if attempt < 3 {
			select {
			case <-ctx.Done():
				return providers.CompletionResponse{}, ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
		}
	}
	return providers.CompletionResponse{}, lastErr
}

func buildLLMRequestSnapshotJSON(model, systemPrompt string, messages []providers.Message, tools []providers.Tool) string {
	type messageSummary struct {
		Role               string   `json:"role"`
		Name               string   `json:"name,omitempty"`
		ToolCallID         string   `json:"tool_call_id,omitempty"`
		ContentPreview     string   `json:"content_preview,omitempty"`
		ContentLength      int      `json:"content_length,omitempty"`
		ContentParts       int      `json:"content_parts,omitempty"`
		ContentPartTypes   []string `json:"content_part_types,omitempty"`
		ContentPartPreview []string `json:"content_part_preview,omitempty"`
	}
	payload := map[string]any{
		"model":               model,
		"system_prompt":       truncateForEvent(systemPrompt, 1800),
		"system_prompt_len":   len(systemPrompt),
		"message_count":       len(messages),
		"tool_count":          len(tools),
		"tool_names":          toolNames(tools),
		"message_role_counts": map[string]int{},
	}
	summaries := make([]messageSummary, 0, len(messages))
	roleCounts := map[string]int{}
	for _, msg := range messages {
		role := strings.TrimSpace(msg.Role)
		if role == "" {
			role = "unknown"
		}
		roleCounts[role]++
		summary := messageSummary{
			Role:           role,
			Name:           msg.Name,
			ToolCallID:     msg.ToolCallID,
			ContentPreview: truncateForEvent(msg.Content, 420),
			ContentLength:  len(msg.Content),
			ContentParts:   len(msg.ContentParts),
		}
		for _, part := range msg.ContentParts {
			partType := strings.TrimSpace(part.Type)
			if partType == "" {
				partType = "unknown"
			}
			summary.ContentPartTypes = append(summary.ContentPartTypes, partType)
			switch strings.ToLower(partType) {
			case "text":
				if snippet := truncateForEvent(part.Text, 220); snippet != "" {
					summary.ContentPartPreview = append(summary.ContentPartPreview, snippet)
				}
			case "image_url":
				if snippet := truncateForEvent(part.URL, 220); snippet != "" {
					summary.ContentPartPreview = append(summary.ContentPartPreview, snippet)
				}
			}
		}
		summaries = append(summaries, summary)
	}
	payload["messages"] = summaries
	payload["message_role_counts"] = roleCounts
	encoded, err := json.Marshal(payload)
	if err != nil {
		fallback, _ := json.Marshal(map[string]any{"message_count": len(messages), "tool_count": len(tools)})
		return string(fallback)
	}
	return string(encoded)
}

func toolNames(tools []providers.Tool) []string {
	if len(tools) == 0 {
		return nil
	}
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		name := strings.TrimSpace(t.Name)
		if name == "" {
			continue
		}
		out = append(out, name)
	}
	return out
}

func (m *Manager) rebuildReplayContext(ctx context.Context, runID, sourceRunID string, resumeFromStep int, task string) ([]providers.Message, int, error) {
	msgs := []providers.Message{}
	var sourceTask string
	_ = m.DB.QueryRowContext(ctx, `SELECT COALESCE(task,'') FROM runs WHERE id=?`, sourceRunID).Scan(&sourceTask)
	if strings.TrimSpace(sourceTask) != "" {
		msgs = append(msgs, providers.Message{Role: "user", Content: strings.TrimSpace(sourceTask)})
	}
	rows, err := m.DB.QueryContext(ctx, `
		SELECT id,idx,output_json FROM run_steps
		WHERE run_id=? AND step_type IN ('model','replay') AND status='completed' AND idx<?
		ORDER BY idx ASC
	`, sourceRunID, resumeFromStep)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	replayed := 0
	for rows.Next() {
		var stepID, outputJSON string
		var idx int
		if err := rows.Scan(&stepID, &idx, &outputJSON); err != nil {
			return nil, replayed, err
		}
		var modelOut struct {
			Text      string               `json:"text"`
			ToolCalls []providers.ToolCall `json:"tool_calls"`
		}
		if err := json.Unmarshal([]byte(outputJSON), &modelOut); err != nil {
			return nil, replayed, err
		}
		msgs = append(msgs, providers.Message{Role: "assistant", Content: modelOut.Text, ToolCalls: modelOut.ToolCalls})
		tcRows, err := m.DB.QueryContext(ctx, `
			SELECT status,output_json,error_message FROM tool_calls
			WHERE run_id=? AND step_id=?
			ORDER BY started_at ASC
		`, sourceRunID, stepID)
		if err != nil {
			return nil, replayed, err
		}
		type replayToolCall struct {
			Status string
			Output string
			ErrMsg string
		}
		recorded := make([]replayToolCall, 0)
		for tcRows.Next() {
			var row replayToolCall
			if err := tcRows.Scan(&row.Status, &row.Output, &row.ErrMsg); err != nil {
				_ = tcRows.Close()
				return nil, replayed, err
			}
			recorded = append(recorded, row)
		}
		_ = tcRows.Close()
		for i, tc := range modelOut.ToolCalls {
			content := `{}`
			if i < len(recorded) {
				if recorded[i].Status == "failed" {
					content = `{"error":` + strconv.Quote(recorded[i].ErrMsg) + `}`
				} else if strings.TrimSpace(recorded[i].Output) != "" {
					content = recorded[i].Output
				}
			}
			msgs = append(msgs, providers.Message{Role: "tool", ToolCallID: tc.ID, Name: tc.Name, Content: content})
		}
		replayed++
		m.execDB(ctx, "insert replay step", `
			INSERT INTO run_steps(id,run_id,idx,step_type,status,input_json,output_json,created_at,started_at,ended_at)
			VALUES(?,?,?,?,?,?,?,?,?,?)
		`, uuid.NewString(), runID, idx, "replay", "completed",
			fmt.Sprintf(`{"source_run_id":"%s","source_step_id":"%s"}`, sourceRunID, stepID),
			outputJSON, now(), now(), now())
	}
	if strings.TrimSpace(task) != "" {
		msgs = append(msgs, providers.Message{Role: "user", Content: strings.TrimSpace(task)})
	}
	return msgs, replayed, nil
}

func (m *Manager) Wait() {
	m.wg.Wait()
}

func (m *Manager) failRun(ctx context.Context, runID string, err error) error {
	m.execDB(ctx, "mark run failed", `UPDATE runs SET status='failed', error=?, completed_at=?, updated_at=? WHERE id=?`, err.Error(), now(), now(), runID)
	_ = m.Store.AppendEvent(ctx, runID, "", "run.failed", map[string]any{"error": err.Error()})
	m.appendChatMessageForRun(ctx, runID, "system", "Run failed: "+truncateForEvent(err.Error(), 220), "run.failed")
	return err
}

func (m *Manager) execDB(ctx context.Context, op, query string, args ...any) {
	if _, err := m.DB.ExecContext(ctx, query, args...); err != nil {
		log.Printf("runtime %s failed: %v", op, err)
	}
}

func (m *Manager) appendChatMessageForRun(ctx context.Context, runID, role, content, source string) {
	content = strings.TrimSpace(content)
	if content == "" {
		return
	}
	var sessionID string
	var turnIndex int
	if err := m.DB.QueryRowContext(ctx, `SELECT session_id,turn_index FROM session_runs WHERE run_id=? LIMIT 1`, runID).Scan(&sessionID, &turnIndex); err != nil {
		return
	}
	nowTs := now()
	m.execDB(ctx, "append chat message", `
		INSERT INTO chat_messages(id,session_id,turn_index,role,content,source,run_id,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?)
	`, uuid.NewString(), sessionID, turnIndex, role, content, source, runID, nowTs, nowTs)
	m.execDB(ctx, "update chat session updated_at", `UPDATE chat_sessions SET updated_at=? WHERE id=?`, nowTs, sessionID)
}

func (m *Manager) currentStatus(runID string) (string, error) {
	var s string
	err := m.DB.QueryRow(`SELECT status FROM runs WHERE id=?`, runID).Scan(&s)
	return s, err
}

func (m *Manager) appendPendingUserMessages(ctx context.Context, runID string, messages *[]providers.Message, lastSeq *int, mediaPolicy mediaInputPolicy) error {
	for {
		events, err := m.Store.GetEvents(ctx, runID, *lastSeq, 200)
		if err != nil {
			return err
		}
		if len(events) == 0 {
			return nil
		}
		for _, ev := range events {
			seq := asInt(ev["seq"])
			if seq > *lastSeq {
				*lastSeq = seq
			}
			eventType, _ := ev["event_type"].(string)
			if eventType != "user.event" {
				continue
			}
			payloadSource := eventPayloadSource(ev["payload"])
			userMsg, ok := m.buildUserMessageFromEventPayload(ctx, runID, ev["payload"], mediaPolicy)
			if !ok {
				continue
			}
			userMsg.Source = payloadSource
			if payloadSource == "chat.attachments" {
				*messages = dropPriorAttachmentContextMessages(*messages)
			}
			*messages = append(*messages, userMsg)
		}
		if len(events) < 200 {
			return nil
		}
	}
}

func asInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	default:
		return 0
	}
}

func extractUserMessage(payload any) string {
	switch raw := payload.(type) {
	case json.RawMessage:
		return extractUserMessageFromJSON(raw)
	case []byte:
		return extractUserMessageFromJSON(raw)
	case string:
		return strings.TrimSpace(raw)
	default:
		return ""
	}
}

func (m *Manager) buildUserMessageFromEventPayload(ctx context.Context, runID string, payload any, mediaPolicy mediaInputPolicy) (providers.Message, bool) {
	var raw map[string]any
	switch v := payload.(type) {
	case json.RawMessage:
		_ = json.Unmarshal(v, &raw)
	case []byte:
		_ = json.Unmarshal(v, &raw)
	}
	text := extractUserMessage(payload)
	msg := providers.Message{Role: "user", Content: text}
	if raw == nil {
		return msg, strings.TrimSpace(text) != ""
	}
	idsRaw, _ := raw["attachments"].([]any)
	if len(idsRaw) == 0 {
		return msg, strings.TrimSpace(text) != ""
	}
	parts := make([]providers.ContentPart, 0, len(idsRaw)+1)
	if strings.TrimSpace(text) != "" {
		parts = append(parts, providers.ContentPart{Type: "text", Text: text})
	}
	for _, item := range idsRaw {
		artifactID := strings.TrimSpace(fmt.Sprint(item))
		if artifactID == "" {
			continue
		}
		var path, mime string
		if err := m.DB.QueryRowContext(ctx, `SELECT path,mime_type FROM artifacts WHERE run_id=? AND id=?`, runID, artifactID).Scan(&path, &mime); err != nil {
			continue
		}
		partSlice := buildAttachmentContentParts(mediaPolicy, artifactID, path, mime)
		if len(partSlice) == 0 {
			continue
		}
		parts = append(parts, partSlice...)
		if len(parts) >= mediaPolicy.MaxParts {
			break
		}
	}
	if len(parts) > 0 {
		msg.ContentParts = parts
	}
	return msg, strings.TrimSpace(text) != "" || len(msg.ContentParts) > 0
}

func eventPayloadSource(payload any) string {
	raw := map[string]any{}
	switch v := payload.(type) {
	case json.RawMessage:
		_ = json.Unmarshal(v, &raw)
	case []byte:
		_ = json.Unmarshal(v, &raw)
	}
	return strings.ToLower(strings.TrimSpace(fmt.Sprint(raw["source"])))
}

func dropPriorAttachmentContextMessages(messages []providers.Message) []providers.Message {
	if len(messages) == 0 {
		return messages
	}
	out := make([]providers.Message, 0, len(messages))
	for _, msg := range messages {
		if msg.Role == "user" && isAttachmentContextUserMessage(msg) {
			continue
		}
		out = append(out, msg)
	}
	return out
}

func isAttachmentContextUserMessage(msg providers.Message) bool {
	if strings.ToLower(strings.TrimSpace(msg.Source)) == "chat.attachments" {
		return true
	}
	content := strings.ToLower(strings.TrimSpace(msg.Content))
	if strings.HasPrefix(content, "user attached file(s) for this run:") {
		return true
	}
	for _, part := range msg.ContentParts {
		if strings.ToLower(strings.TrimSpace(part.Type)) == "image_url" && strings.TrimSpace(part.URL) != "" {
			return true
		}
	}
	return false
}

func extractUserMessageFromJSON(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return strings.TrimSpace(string(raw))
	}
	for _, key := range []string{"message", "text", "content", "instruction"} {
		if v, ok := obj[key]; ok {
			if s := strings.TrimSpace(fmt.Sprint(v)); s != "" {
				return s
			}
		}
	}
	return ""
}

// filterProviderTools narrows the tool set we advertise to the LLM to the
// agent's allowed_tools list. Empty allowed list means "no restriction" — same
// semantics as policy.EvaluateTool, so we don't silently muzzle agents that
// haven't configured an allowlist.
func filterProviderTools(defs []tools.Definition, allowed []string) []providers.Tool {
	allowSet := make(map[string]struct{}, len(allowed))
	for _, t := range allowed {
		if n := strings.TrimSpace(t); n != "" {
			allowSet[n] = struct{}{}
		}
	}
	out := make([]providers.Tool, 0, len(defs))
	for _, t := range defs {
		if len(allowSet) > 0 {
			if _, ok := allowSet[t.Name]; !ok {
				continue
			}
		}
		out = append(out, providers.Tool{Name: t.Name, Description: t.Description, InputSchema: t.Schema})
	}
	return out
}

func buildEffectiveSystemPrompt(base string, allowedTools []string) string {
	base = strings.TrimSpace(base)
	if len(allowedTools) == 0 {
		return base
	}
	toolPolicy := []string{
		"Operational tool policy:",
		"- Prefer tool-backed execution when tools are available.",
		"- If a task depends on external, current, or environment-specific facts, verify using tools before concluding.",
		"- Do not claim inability to access information when an allowed tool can fetch or verify it.",
		"- Ground final answers in observed tool output; summarize key evidence succinctly.",
		"- Prefer user-facing summaries over raw telemetry dumps; avoid listing raw UUIDs or artifact IDs unless the user explicitly asks for them.",
		"- If artifacts are produced, describe what was produced in plain language and where to view it, rather than enumerating internal identifiers.",
		"- When the task asks for media output (screenshots/images/video/audio), end with a clear attachment confirmation sentence (e.g., 'Screenshot attached below.').",
		"- Never return internal filesystem paths (e.g., /home/... or /workspace/...) unless explicitly requested for debugging.",
		"- If a tool attempt fails, briefly report the failure and next best attempt.",
		"- Treat each run as a one-shot execution whenever feasible: complete the requested deliverable in this run.",
		"- Do not ask follow-up permission questions like 'want me to continue?' after completing the requested action.",
		"- If the user requested a concrete output (file, patch, screenshot, report), produce it directly and reference the produced output in the final answer.",
	}
	if hasAllowedTool(allowedTools, "shell.exec") {
		toolPolicy = append(toolPolicy,
			"- shell.exec is available: use terminal commands to retrieve and validate external data when needed.")
	}
	if hasAllowedToolPrefix(allowedTools, "browser.") {
		toolPolicy = append(toolPolicy,
			"- browser.* tools are available: when asked for a screenshot or page capture, capture it in-run and confirm it is attached; avoid exposing raw artifact IDs unless requested.")
	}
	policyBlock := strings.Join(toolPolicy, "\n")
	if base == "" {
		return policyBlock
	}
	return base + "\n\n" + policyBlock
}

type sessionResources struct {
	EnvironmentID   string
	EnvironmentName string
	VaultID         string
	EnvVars         map[string]string
}

func (m *Manager) loadSessionResources(ctx context.Context, runID, environmentID, vaultID string) (sessionResources, error) {
	res := sessionResources{
		EnvironmentID: strings.TrimSpace(environmentID),
		VaultID:       strings.TrimSpace(vaultID),
		EnvVars:       map[string]string{},
	}
	res.EnvVars["COLOSSEUM_SESSION_ID"] = runID
	if res.EnvironmentID != "" {
		var name, configJSON string
		if err := m.DB.QueryRowContext(ctx, `SELECT name, config_json FROM environments WHERE id=?`, res.EnvironmentID).Scan(&name, &configJSON); err != nil {
			if err == sql.ErrNoRows {
				return res, fmt.Errorf("environment not found: %s", res.EnvironmentID)
			}
			return res, fmt.Errorf("load environment: %w", err)
		}
		res.EnvironmentName = name
		var cfg map[string]any
		if err := json.Unmarshal([]byte(configJSON), &cfg); err == nil {
			for k, v := range parseEnvironmentVars(cfg) {
				res.EnvVars[k] = v
			}
		}
	}
	if res.VaultID != "" {
		rows, err := m.DB.QueryContext(ctx, `
			SELECT i.secret_name, i.alias, s.cipher_text
			FROM credential_vault_items i
			JOIN secrets s ON s.name=i.secret_name
			WHERE i.vault_id=?
		`, res.VaultID)
		if err != nil {
			return res, fmt.Errorf("load vault secrets: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var secretName, alias, cipher string
			if err := rows.Scan(&secretName, &alias, &cipher); err != nil {
				return res, fmt.Errorf("scan vault secret: %w", err)
			}
			value, decErr := secrets.Decrypt(cipher, m.SecretKey)
			if decErr != nil {
				return res, fmt.Errorf("decrypt vault secret %s: %w", secretName, decErr)
			}
			envName := strings.TrimSpace(alias)
			if envName == "" {
				envName = strings.TrimSpace(secretName)
			}
			if envName != "" {
				res.EnvVars[envName] = value
			}
		}
	}
	return res, nil
}

func parseEnvironmentVars(cfg map[string]any) map[string]string {
	out := map[string]string{}
	if cfg == nil {
		return out
	}
	raw := cfg["env_vars"]
	envObj, ok := raw.(map[string]any)
	if !ok {
		return out
	}
	for k, v := range envObj {
		key := strings.TrimSpace(k)
		if key == "" {
			continue
		}
		out[key] = strings.TrimSpace(fmt.Sprint(v))
	}
	return out
}

func isEventSequenceConflict(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique constraint failed") && strings.Contains(msg, "events.run_id") && strings.Contains(msg, "events.seq")
}

func hasAllowedTool(allowedTools []string, name string) bool {
	for _, t := range allowedTools {
		if strings.TrimSpace(t) == name {
			return true
		}
	}
	return false
}

func hasAllowedToolPrefix(allowedTools []string, prefix string) bool {
	for _, t := range allowedTools {
		if strings.HasPrefix(strings.TrimSpace(t), prefix) {
			return true
		}
	}
	return false
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }
