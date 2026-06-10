package tools

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

type Definition struct {
	ID          string          `json:"id,omitempty"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"input_schema"`
	Kind        string          `json:"kind"`
	Config      json.RawMessage `json:"config_json,omitempty"`
	Enabled     bool            `json:"enabled"`
	IsBuiltin   bool            `json:"is_builtin"`
	CreatedAt   string          `json:"created_at,omitempty"`
	UpdatedAt   string          `json:"updated_at,omitempty"`
}

type Executor struct {
	DB           *sql.DB
	ArtifactsDir string
	Docker       DockerExecutor
	Browser      *BrowserRuntime
	EventSink    EventSink
}

// EventSink lets tools append events (notably synthetic user.event records
// for tool-driven attachment recall) through the same transactional, retry-
// safe path the runtime uses. Provided by the runtime Manager at wiring time.
type EventSink interface {
	AppendEvent(ctx context.Context, runID, stepID, eventType string, payload map[string]any) error
}

type DockerExecutor interface {
	Exec(ctx context.Context, runID, workspace string, envVars map[string]string, command string, timeout time.Duration) (string, string, int, error)
}

type Context struct {
	RunID             string
	StepID            string
	Workspace         string
	EnvironmentID     string
	CredentialVaultID string
	EnvVars           map[string]string
}

type Result struct {
	Output    map[string]any
	Log       string
	Artifacts []string
}

func Builtins() []Definition {
	return []Definition{
		{Name: "shell.exec", Description: "Execute shell command in workspace", Schema: rawSchema(`{"type":"object","properties":{"command":{"type":"string"},"timeout_seconds":{"type":"integer","minimum":1,"maximum":600}},"required":["command"]}`), Kind: "builtin", Enabled: true, IsBuiltin: true},
		{Name: "file.read", Description: "Read a file from workspace", Schema: rawSchema(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`), Kind: "builtin", Enabled: true, IsBuiltin: true},
		{Name: "file.read_range", Description: "Read a line range from file in workspace", Schema: rawSchema(`{"type":"object","properties":{"path":{"type":"string"},"start_line":{"type":"integer","minimum":1},"end_line":{"type":"integer","minimum":1}},"required":["path","start_line","end_line"]}`), Kind: "builtin", Enabled: true, IsBuiltin: true},
		{Name: "file.write", Description: "Write text to file in workspace", Schema: rawSchema(`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}`), Kind: "builtin", Enabled: true, IsBuiltin: true},
		{Name: "file.search", Description: "Search file contents using regex pattern", Schema: rawSchema(`{"type":"object","properties":{"pattern":{"type":"string"},"glob":{"type":"string"}},"required":["pattern"]}`), Kind: "builtin", Enabled: true, IsBuiltin: true},
		{Name: "file.exists", Description: "Check whether a file path exists in workspace", Schema: rawSchema(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`), Kind: "builtin", Enabled: true, IsBuiltin: true},
		{Name: "file.stat", Description: "Get file metadata for a path in workspace", Schema: rawSchema(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`), Kind: "builtin", Enabled: true, IsBuiltin: true},
		{Name: "file.list", Description: "List files and directories under a workspace path", Schema: rawSchema(`{"type":"object","properties":{"path":{"type":"string"},"recursive":{"type":"boolean"},"max_entries":{"type":"integer","minimum":1,"maximum":5000}}}`), Kind: "builtin", Enabled: true, IsBuiltin: true},
		{Name: "path.glob", Description: "Find paths in workspace matching a glob pattern", Schema: rawSchema(`{"type":"object","properties":{"pattern":{"type":"string"}},"required":["pattern"]}`), Kind: "builtin", Enabled: true, IsBuiltin: true},
		{Name: "file.publish", Description: "Register a workspace file as a downloadable artifact. Returns artifact_id and download_path.", Schema: rawSchema(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`), Kind: "builtin", Enabled: true, IsBuiltin: true},
		{Name: "apply.patch", Description: "Apply unified diff patch text in workspace", Schema: rawSchema(`{"type":"object","properties":{"patch":{"type":"string"}},"required":["patch"]}`), Kind: "builtin", Enabled: true, IsBuiltin: true},
		{Name: "web.fetch", Description: "Fetch HTTP(S) content from a URL", Schema: rawSchema(`{"type":"object","properties":{"url":{"type":"string"},"timeout_seconds":{"type":"integer","minimum":1,"maximum":60},"max_bytes":{"type":"integer","minimum":1024,"maximum":1048576}},"required":["url"]}`), Kind: "builtin", Enabled: true, IsBuiltin: true},
		{Name: "json.parse", Description: "Parse JSON string and return normalized object", Schema: rawSchema(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`), Kind: "builtin", Enabled: true, IsBuiltin: true},
		{Name: "json.query", Description: "Query JSON payload using a dot path expression", Schema: rawSchema(`{"type":"object","properties":{"input":{"description":"JSON object/array or stringified JSON"},"path":{"type":"string"}},"required":["input","path"]}`), Kind: "builtin", Enabled: true, IsBuiltin: true},
		{Name: "browser.open", Description: "Open URL in browser session", Schema: rawSchema(`{"type":"object","properties":{"url":{"type":"string"}},"required":["url"]}`), Kind: "builtin", Enabled: true, IsBuiltin: true},
		{Name: "browser.snapshot", Description: "Capture current page snapshot from browser session", Schema: rawSchema(`{"type":"object","properties":{"screenshot":{"type":"boolean"}}}`), Kind: "builtin", Enabled: true, IsBuiltin: true},
		{Name: "browser.action", Description: "Perform browser action on current page", Schema: rawSchema(`{"type":"object","properties":{"action":{"type":"string","enum":["click","type","press","select","scroll"]},"selector":{"type":"string"},"text":{"type":"string"},"key":{"type":"string"},"value":{"type":"string"},"delta_y":{"type":"integer"}},"required":["action"]}`), Kind: "builtin", Enabled: true, IsBuiltin: true},
		{Name: "browser.wait", Description: "Wait in browser session", Schema: rawSchema(`{"type":"object","properties":{"milliseconds":{"type":"integer","minimum":1,"maximum":30000}}}`), Kind: "builtin", Enabled: true, IsBuiltin: true},
		{Name: "browser.close", Description: "Close browser session for current run", Schema: rawSchema(`{"type":"object","properties":{}}`), Kind: "builtin", Enabled: true, IsBuiltin: true},
		{Name: "test.run", Description: "Run test command in workspace", Schema: rawSchema(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`), Kind: "builtin", Enabled: true, IsBuiltin: true},
		{Name: "artifact.list", Description: "List artifacts generated for run", Schema: rawSchema(`{"type":"object","properties":{}}`), Kind: "builtin", Enabled: true, IsBuiltin: true},
		{Name: "artifact.get", Description: "Get artifact metadata by id", Schema: rawSchema(`{"type":"object","properties":{"artifact_id":{"type":"string"}},"required":["artifact_id"]}`), Kind: "builtin", Enabled: true, IsBuiltin: true},
		{Name: "recall_artifact", Description: "Re-attach an image or file from an earlier turn of this chat session so the next model step can see it. Accepts artifact_id or a workspace-relative path.", Schema: rawSchema(`{"type":"object","properties":{"artifact_id":{"type":"string"},"path":{"type":"string"},"note":{"type":"string"}}}`), Kind: "builtin", Enabled: true, IsBuiltin: true},
		{Name: "clock.now", Description: "Get the current wall-clock time, date, and timezone.", Schema: rawSchema(`{"type":"object","properties":{}}`), Kind: "builtin", Enabled: true, IsBuiltin: true},
		{Name: "env.inspect", Description: "Read env vars injected into this run by the environment + credential vault. Never reads host env.", Schema: rawSchema(`{"type":"object","properties":{"keys":{"type":"array","items":{"type":"string"}}}}`), Kind: "builtin", Enabled: true, IsBuiltin: true},
		{Name: "http.request", Description: "Make an HTTP(S) request (GET/POST/PUT/PATCH/DELETE/HEAD/OPTIONS) with optional headers, JSON body, or text body.", Schema: rawSchema(`{"type":"object","properties":{"url":{"type":"string"},"method":{"type":"string","enum":["GET","POST","PUT","PATCH","DELETE","HEAD","OPTIONS"]},"headers":{"type":"object","additionalProperties":{"type":"string"}},"body":{"type":"string"},"json":{"description":"If set, serialized as JSON and sent with Content-Type: application/json"},"timeout_seconds":{"type":"integer","minimum":1,"maximum":120},"max_bytes":{"type":"integer","minimum":1024,"maximum":4194304}},"required":["url"]}`), Kind: "builtin", Enabled: true, IsBuiltin: true},
		{Name: "browser.screenshot", Description: "Capture a PNG screenshot of the current browser page and register it as an artifact.", Schema: rawSchema(`{"type":"object","properties":{"full_page":{"type":"boolean"}}}`), Kind: "builtin", Enabled: true, IsBuiltin: true},
		{Name: "scratchpad.write", Description: "Store a session-scoped note-to-self under a key. Persists across turns of this chat session.", Schema: rawSchema(`{"type":"object","properties":{"key":{"type":"string"},"value":{"type":"string"},"note":{"type":"string"}},"required":["key","value"]}`), Kind: "builtin", Enabled: true, IsBuiltin: true},
		{Name: "scratchpad.read", Description: "Read one scratchpad entry by key, or list all entries in this session when key is omitted.", Schema: rawSchema(`{"type":"object","properties":{"key":{"type":"string"}}}`), Kind: "builtin", Enabled: true, IsBuiltin: true},
		{Name: "scratchpad.delete", Description: "Remove a scratchpad entry by key for this session.", Schema: rawSchema(`{"type":"object","properties":{"key":{"type":"string"}},"required":["key"]}`), Kind: "builtin", Enabled: true, IsBuiltin: true},
		{Name: "process.run_background", Description: "Launch a shell command as a background process in the run's workspace. Returns a process_id and log path.", Schema: rawSchema(`{"type":"object","properties":{"command":{"type":"string"},"label":{"type":"string"},"timeout_seconds":{"type":"integer","minimum":1,"maximum":60}},"required":["command"]}`), Kind: "builtin", Enabled: true, IsBuiltin: true},
		{Name: "process.logs", Description: "Fetch output and status for a background process by process_id.", Schema: rawSchema(`{"type":"object","properties":{"process_id":{"type":"string"},"max_bytes":{"type":"integer","minimum":512,"maximum":1048576},"tail_lines":{"type":"integer","minimum":1,"maximum":5000}},"required":["process_id"]}`), Kind: "builtin", Enabled: true, IsBuiltin: true},
		{Name: "process.kill", Description: "Send a signal (default TERM) to a background process.", Schema: rawSchema(`{"type":"object","properties":{"process_id":{"type":"string"},"signal":{"type":"string"}},"required":["process_id"]}`), Kind: "builtin", Enabled: true, IsBuiltin: true},
		{Name: "subagent.spawn", Description: "Queue a child agent run to work on a subtask. Returns immediately with a run_id. Cap of 5 active children per parent.", Schema: rawSchema(`{"type":"object","properties":{"agent_id":{"type":"string"},"task":{"type":"string"},"seed_workspace":{"type":"boolean"},"max_steps":{"type":"integer","minimum":1,"maximum":200}},"required":["agent_id","task"]}`), Kind: "builtin", Enabled: true, IsBuiltin: true},
		{Name: "subagent.status", Description: "Check the status of a subagent run by run_id.", Schema: rawSchema(`{"type":"object","properties":{"run_id":{"type":"string"}},"required":["run_id"]}`), Kind: "builtin", Enabled: true, IsBuiltin: true},
		{Name: "subagent.wait", Description: "Block until a subagent run reaches a terminal status or a timeout elapses (default 300s, max 1800s).", Schema: rawSchema(`{"type":"object","properties":{"run_id":{"type":"string"},"timeout_seconds":{"type":"integer","minimum":1,"maximum":1800}},"required":["run_id"]}`), Kind: "builtin", Enabled: true, IsBuiltin: true},
		{Name: "approval.request", Description: "Request operator approval before proceeding. Blocks (with timeout) until a human approves or rejects in the UI.", Schema: rawSchema(`{"type":"object","properties":{"reason":{"type":"string"},"details":{"type":"string"},"risk":{"type":"string"},"timeout_seconds":{"type":"integer","minimum":10,"maximum":3600},"plan_step_id":{"type":"string","description":"Optional session plan step this approval belongs to; enables step-linked audit."}},"required":["reason"]}`), Kind: "builtin", Enabled: true, IsBuiltin: true},
		{Name: "plan.set", Description: "Create or replace the session plan as an ordered list of steps. Call this before executing multi-step work; revising it later is fine and preferred over ignoring a stale plan.", Schema: rawSchema(`{"type":"object","properties":{"title":{"type":"string"},"steps":{"type":"array","minItems":1,"maxItems":100,"items":{"type":"object","properties":{"title":{"type":"string"},"detail":{"type":"string"}},"required":["title"]}}},"required":["steps"]}`), Kind: "builtin", Enabled: true, IsBuiltin: true},
		{Name: "plan.update_step", Description: "Update the status/notes/blocker of a single plan step. Claim a step by setting status to in_progress; release it with completed/skipped/blocked.", Schema: rawSchema(`{"type":"object","properties":{"step_id":{"type":"string"},"status":{"type":"string","enum":["pending","in_progress","completed","skipped","blocked"]},"notes":{"type":"string"},"blocker":{"type":"string","description":"Required when status=blocked; short reason."}},"required":["step_id"]}`), Kind: "builtin", Enabled: true, IsBuiltin: true},
		{Name: "plan.update_steps", Description: "Batch variant of plan.update_step. Use to update several steps in one call (e.g., skip a range of obsolete steps).", Schema: rawSchema(`{"type":"object","properties":{"updates":{"type":"array","minItems":1,"maxItems":100,"items":{"type":"object","properties":{"step_id":{"type":"string"},"status":{"type":"string","enum":["pending","in_progress","completed","skipped","blocked"]},"notes":{"type":"string"},"blocker":{"type":"string"}},"required":["step_id"]}}},"required":["updates"]}`), Kind: "builtin", Enabled: true, IsBuiltin: true},
		{Name: "plan.add_step", Description: "Insert a new step into the current plan. If after_step_id is omitted, the step is appended.", Schema: rawSchema(`{"type":"object","properties":{"title":{"type":"string"},"detail":{"type":"string"},"after_step_id":{"type":"string"}},"required":["title"]}`), Kind: "builtin", Enabled: true, IsBuiltin: true},
		{Name: "plan.read", Description: "Read the current session plan with full step details. The session primer already summarizes the plan; use this to re-align when you need exact status or to resolve a concurrent-update error.", Schema: rawSchema(`{"type":"object","properties":{}}`), Kind: "builtin", Enabled: true, IsBuiltin: true},
	}
}

func rawSchema(s string) json.RawMessage { return json.RawMessage(s) }

func (e *Executor) Execute(ctx context.Context, runCtx Context, name string, input json.RawMessage) (Result, error) {
	switch name {
	case "shell.exec":
		return e.shellExec(ctx, runCtx, input)
	case "file.read":
		return e.fileRead(runCtx, input)
	case "file.read_range":
		return e.fileReadRange(runCtx, input)
	case "file.write":
		return e.fileWrite(runCtx, input)
	case "file.search":
		return e.fileSearch(ctx, runCtx, input)
	case "file.exists":
		return e.fileExists(runCtx, input)
	case "file.stat":
		return e.fileStat(runCtx, input)
	case "file.list":
		return e.fileList(runCtx, input)
	case "path.glob":
		return e.pathGlob(runCtx, input)
	case "file.publish":
		return e.filePublish(ctx, runCtx, input)
	case "apply.patch":
		return e.applyPatch(ctx, runCtx, input)
	case "web.fetch":
		return e.webFetch(ctx, input)
	case "json.parse":
		return e.jsonParse(input)
	case "json.query":
		return e.jsonQuery(input)
	case "browser.open":
		return e.browserOpen(ctx, runCtx, input)
	case "browser.snapshot":
		return e.browserSnapshot(ctx, runCtx, input)
	case "browser.action":
		return e.browserAction(ctx, runCtx, input)
	case "browser.wait":
		return e.browserWait(ctx, runCtx, input)
	case "browser.close":
		return e.browserClose(ctx, runCtx)
	case "test.run":
		return e.testRun(ctx, runCtx, input)
	case "artifact.list":
		return e.artifactList(ctx, runCtx)
	case "artifact.get":
		return e.artifactGet(ctx, runCtx, input)
	case "recall_artifact":
		return e.recallArtifact(ctx, runCtx, input)
	case "clock.now":
		return e.clockNow(input)
	case "env.inspect":
		return e.envInspect(runCtx, input)
	case "http.request":
		return e.httpRequest(ctx, input)
	case "browser.screenshot":
		return e.browserScreenshot(ctx, runCtx, input)
	case "scratchpad.write":
		return e.scratchpadWrite(ctx, runCtx, input)
	case "scratchpad.read":
		return e.scratchpadRead(ctx, runCtx, input)
	case "scratchpad.delete":
		return e.scratchpadDelete(ctx, runCtx, input)
	case "process.run_background":
		return e.processRunBackground(ctx, runCtx, input)
	case "process.logs":
		return e.processLogs(ctx, runCtx, input)
	case "process.kill":
		return e.processKill(ctx, runCtx, input)
	case "subagent.spawn":
		return e.subagentSpawn(ctx, runCtx, input)
	case "subagent.status":
		return e.subagentStatus(ctx, runCtx, input)
	case "subagent.wait":
		return e.subagentWait(ctx, runCtx, input)
	case "approval.request":
		return e.approvalRequest(ctx, runCtx, input)
	case "plan.set":
		return e.planSet(ctx, runCtx, input)
	case "plan.update_step":
		return e.planUpdateStep(ctx, runCtx, input)
	case "plan.update_steps":
		return e.planUpdateSteps(ctx, runCtx, input)
	case "plan.add_step":
		return e.planAddStep(ctx, runCtx, input)
	case "plan.read":
		return e.planRead(ctx, runCtx, input)
	default:
		return e.customTool(ctx, runCtx, name, input)
	}
}

func EnsureBuiltinDefinitions(ctx context.Context, db *sql.DB) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, tool := range Builtins() {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO tool_defs(id,name,description,input_schema_json,kind,config_json,enabled,is_builtin,created_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(name) DO UPDATE SET
			  description=excluded.description,
			  input_schema_json=excluded.input_schema_json,
			  kind=excluded.kind,
			  enabled=excluded.enabled,
			  is_builtin=excluded.is_builtin,
			  updated_at=excluded.updated_at
		`, uuid.NewString(), tool.Name, tool.Description, string(tool.Schema), "builtin", "{}", 1, 1, now, now); err != nil {
			return err
		}
	}
	return nil
}

func ListDefinitions(ctx context.Context, db *sql.DB, includeDisabled bool) ([]Definition, error) {
	query := `SELECT id,name,description,input_schema_json,kind,config_json,enabled,is_builtin,created_at,updated_at FROM tool_defs`
	if !includeDisabled {
		query += ` WHERE enabled=1`
	}
	query += ` ORDER BY is_builtin DESC, name ASC`
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Definition, 0)
	for rows.Next() {
		var d Definition
		var enabledInt, builtinInt int
		var schema, cfg string
		if err := rows.Scan(&d.ID, &d.Name, &d.Description, &schema, &d.Kind, &cfg, &enabledInt, &builtinInt, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, err
		}
		d.Schema = json.RawMessage(schema)
		d.Config = json.RawMessage(cfg)
		d.Enabled = enabledInt == 1
		d.IsBuiltin = builtinInt == 1
		out = append(out, d)
	}
	return out, nil
}

func (e *Executor) customTool(ctx context.Context, runCtx Context, name string, input json.RawMessage) (Result, error) {
	var kind, configJSON string
	err := e.DB.QueryRowContext(ctx, `SELECT kind, config_json FROM tool_defs WHERE name=? AND enabled=1`, name).Scan(&kind, &configJSON)
	if err != nil {
		if err == sql.ErrNoRows {
			return Result{}, fmt.Errorf("unknown tool: %s", name)
		}
		return Result{}, err
	}

	switch kind {
	case "shell_command":
		var cfg struct {
			CommandTemplate string `json:"command_template"`
			TimeoutSeconds  int    `json:"timeout_seconds"`
		}
		if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
			return Result{}, err
		}
		if cfg.CommandTemplate == "" {
			return Result{}, fmt.Errorf("custom shell_command missing command_template")
		}
		if cfg.TimeoutSeconds <= 0 {
			cfg.TimeoutSeconds = 120
		}
		rendered := cfg.CommandTemplate
		var args map[string]any
		_ = json.Unmarshal(input, &args)
		re := regexp.MustCompile(`\{\{([a-zA-Z0-9_]+)\}\}`)
		rendered = re.ReplaceAllStringFunc(rendered, func(match string) string {
			key := strings.TrimSuffix(strings.TrimPrefix(match, "{{"), "}}")
			if v, ok := args[key]; ok {
				return fmt.Sprintf("%v", v)
			}
			return ""
		})
		return e.shellExec(ctx, runCtx, json.RawMessage(fmt.Sprintf(`{"command":%q,"timeout_seconds":%d}`, rendered, cfg.TimeoutSeconds)))

	case "http_tool":
		var cfg struct {
			URL            string            `json:"url"`
			Method         string            `json:"method"`
			Headers        map[string]string `json:"headers"`
			TimeoutSeconds int               `json:"timeout_seconds"`
		}
		if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
			return Result{}, err
		}
		if cfg.URL == "" {
			return Result{}, fmt.Errorf("http_tool missing url")
		}
		if cfg.Method == "" {
			cfg.Method = "POST"
		}
		if cfg.TimeoutSeconds <= 0 {
			cfg.TimeoutSeconds = 30
		}
		reqCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.TimeoutSeconds)*time.Second)
		defer cancel()
		httpReq, err := http.NewRequestWithContext(reqCtx, cfg.Method, cfg.URL, bytes.NewReader(input))
		if err != nil {
			return Result{}, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		for k, v := range cfg.Headers {
			httpReq.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(httpReq)
		if err != nil {
			return Result{}, err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= 300 {
			return Result{}, fmt.Errorf("http_tool %s returned status %d: %s", cfg.URL, resp.StatusCode, string(body))
		}
		var out any
		if err := json.Unmarshal(body, &out); err != nil {
			return Result{Output: map[string]any{"result": string(body)}}, nil
		}
		return Result{Output: map[string]any{"result": out}}, nil

	default:
		return Result{}, fmt.Errorf("unsupported custom tool kind: %s", kind)
	}
}

func safePath(workspace, p string) (string, error) {
	joined := filepath.Join(workspace, p)
	clean := filepath.Clean(joined)
	workspaceClean := filepath.Clean(workspace)
	if clean != workspaceClean && !strings.HasPrefix(clean, workspaceClean+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes workspace")
	}
	rel, err := filepath.Rel(workspaceClean, clean)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes workspace")
	}
	return clean, nil
}

func truncateOutput(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n...[output truncated]..."
}

func ioReadAllLimited(r io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		limit = 256 * 1024
	}
	lr := io.LimitReader(r, limit+1)
	b, err := io.ReadAll(lr)
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		b = b[:limit]
	}
	return b, nil
}

func mergeEnv(base []string, injected map[string]string) []string {
	if len(injected) == 0 {
		return base
	}
	out := append([]string{}, base...)
	for key, value := range injected {
		k := strings.TrimSpace(key)
		if k == "" || !isValidEnvName(k) {
			continue
		}
		out = append(out, k+"="+value)
	}
	return out
}

func sanitizedBaseEnv() []string {
	base := os.Environ()
	out := make([]string, 0, len(base))
	for _, entry := range base {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if shouldFilterHostEnvKey(key) {
			continue
		}
		out = append(out, entry)
	}
	return out
}

func shouldFilterHostEnvKey(key string) bool {
	k := strings.ToUpper(strings.TrimSpace(key))
	if k == "" {
		return true
	}
	if strings.Contains(k, "SECRET") || strings.Contains(k, "TOKEN") || strings.Contains(k, "PASSWORD") || strings.HasSuffix(k, "_KEY") {
		return true
	}
	switch k {
	case "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "GITHUB_TOKEN":
		return true
	default:
		return false
	}
}

func isValidEnvName(name string) bool {
	for i, r := range name {
		if i == 0 {
			if !((r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || r == '_') {
				return false
			}
			continue
		}
		if !((r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_') {
			return false
		}
	}
	return name != ""
}

func (e *Executor) writeRunArtifact(runCtx Context, prefix, ext string, content []byte) (string, error) {
	if e.ArtifactsDir == "" {
		return "", nil
	}
	runDir := filepath.Join(e.ArtifactsDir, runCtx.RunID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(runDir, fmt.Sprintf("%s-%d.%s", prefix, time.Now().UnixNano(), ext))
	if err := os.WriteFile(path, content, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func (e *Executor) insertArtifactRecord(runCtx Context, kind, path, mime string, size int64) error {
	if e.DB == nil {
		return nil
	}
	_, err := e.DB.Exec(`INSERT INTO artifacts(id,run_id,step_id,kind,path,mime_type,size_bytes,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		uuid.NewString(), runCtx.RunID, runCtx.StepID, kind, path, mime, size, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}
