package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/adevireddy/colosseum/internal/mcp"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type mcpServerRequest struct {
	Name           string            `json:"name"`
	Description    string            `json:"description"`
	Transport      string            `json:"transport"`
	Command        string            `json:"command"`
	Args           []string          `json:"args"`
	Env            map[string]string `json:"env"`
	URL            string            `json:"url"`
	Headers        map[string]string `json:"headers"`
	TimeoutSeconds int               `json:"timeout_seconds"`
	Enabled        bool              `json:"enabled"`
}

func registerMCPRoutes(r chi.Router, db *sql.DB) {
	r.Route("/api/mcp", func(r chi.Router) {
		r.Get("/servers", listMCPServersHandler(db))
		r.Post("/servers", createMCPServerHandler(db))
		r.Put("/servers/{id}", updateMCPServerHandler(db))
		r.Delete("/servers/{id}", deleteMCPServerHandler(db))
		r.Post("/servers/{id}/test", testMCPServerHandler(db))

		r.Get("/agents/{agentID}/servers", listAgentMCPServersHandler(db))
		r.Put("/agents/{agentID}/servers", setAgentMCPServersHandler(db))
	})
}

func listMCPServersHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := db.QueryContext(r.Context(), `
			SELECT id,name,description,transport,command,args_json,env_json,url,headers_json,timeout_seconds,enabled,created_at,updated_at
			FROM mcp_servers ORDER BY name ASC
		`)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		defer rows.Close()
		out := make([]map[string]any, 0)
		for rows.Next() {
			row, err := scanMCPServer(rows)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			out = append(out, row)
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func createMCPServerHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req mcpServerRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		if err := validateMCPServerRequest(req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		id := uuid.NewString()
		nowStr := time.Now().UTC().Format(time.RFC3339Nano)
		argsJSON, _ := json.Marshal(req.Args)
		envJSON, _ := json.Marshal(req.Env)
		headersJSON, _ := json.Marshal(req.Headers)
		if req.TimeoutSeconds <= 0 {
			req.TimeoutSeconds = 30
		}
		enabled := 0
		if req.Enabled {
			enabled = 1
		}
		_, err := db.ExecContext(r.Context(), `
			INSERT INTO mcp_servers(id,name,description,transport,command,args_json,env_json,url,headers_json,timeout_seconds,enabled,created_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
		`, id, req.Name, req.Description, req.Transport, req.Command,
			string(argsJSON), string(envJSON), req.URL, string(headersJSON),
			req.TimeoutSeconds, enabled, nowStr, nowStr)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"id": id})
	}
}

func updateMCPServerHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		var req mcpServerRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		if err := validateMCPServerRequest(req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		argsJSON, _ := json.Marshal(req.Args)
		envJSON, _ := json.Marshal(req.Env)
		headersJSON, _ := json.Marshal(req.Headers)
		if req.TimeoutSeconds <= 0 {
			req.TimeoutSeconds = 30
		}
		enabled := 0
		if req.Enabled {
			enabled = 1
		}
		res, err := db.ExecContext(r.Context(), `
			UPDATE mcp_servers
			SET name=?,description=?,transport=?,command=?,args_json=?,env_json=?,url=?,headers_json=?,timeout_seconds=?,enabled=?,updated_at=?
			WHERE id=?
		`, req.Name, req.Description, req.Transport, req.Command,
			string(argsJSON), string(envJSON), req.URL, string(headersJSON),
			req.TimeoutSeconds, enabled, time.Now().UTC().Format(time.RFC3339Nano), id)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "mcp server not found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"id": id})
	}
}

func deleteMCPServerHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		_, _ = db.ExecContext(r.Context(), `DELETE FROM agent_mcp_servers WHERE mcp_server_id=?`, id)
		res, err := db.ExecContext(r.Context(), `DELETE FROM mcp_servers WHERE id=?`, id)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "mcp server not found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
	}
}

func testMCPServerHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		cfg, err := loadMCPServerByID(r.Context(), db, id)
		if err != nil {
			if err == sql.ErrNoRows {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "mcp server not found"})
			} else {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			}
			return
		}
		client := buildClientFromCfg(cfg)
		ctx := r.Context()
		if err := client.Connect(ctx); err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		defer client.Close()
		tools, err := client.ListTools(ctx, mcp.ServerSlug(cfg.Name))
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		names := make([]string, 0, len(tools))
		for _, t := range tools {
			names = append(names, t.Name)
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "tool_count": len(tools), "tools": names})
	}
}

func listAgentMCPServersHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		agentID := chi.URLParam(r, "agentID")
		rows, err := db.QueryContext(r.Context(), `
			SELECT s.id,s.name,s.description,s.transport,s.command,s.args_json,s.env_json,s.url,s.headers_json,s.timeout_seconds,s.enabled,s.created_at,s.updated_at
			FROM mcp_servers s
			JOIN agent_mcp_servers a ON a.mcp_server_id = s.id
			WHERE a.agent_id=? ORDER BY s.name ASC
		`, agentID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		defer rows.Close()
		out := make([]map[string]any, 0)
		for rows.Next() {
			row, err := scanMCPServer(rows)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			out = append(out, row)
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func setAgentMCPServersHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		agentID := chi.URLParam(r, "agentID")
		var req struct {
			ServerIDs []string `json:"server_ids"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		tx, err := db.BeginTx(r.Context(), nil)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		_, _ = tx.ExecContext(r.Context(), `DELETE FROM agent_mcp_servers WHERE agent_id=?`, agentID)
		for _, sid := range req.ServerIDs {
			sid = strings.TrimSpace(sid)
			if sid == "" {
				continue
			}
			if _, err := tx.ExecContext(r.Context(), `INSERT OR IGNORE INTO agent_mcp_servers(agent_id,mcp_server_id) VALUES(?,?)`, agentID, sid); err != nil {
				_ = tx.Rollback()
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
		}
		if err := tx.Commit(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"agent_id": agentID, "server_ids": req.ServerIDs})
	}
}

// ---- helpers ----

func validateMCPServerRequest(req mcpServerRequest) error {
	if strings.TrimSpace(req.Name) == "" {
		return errors.New("name required")
	}
	switch req.Transport {
	case "stdio":
		if strings.TrimSpace(req.Command) == "" {
			return errors.New("command required for stdio transport")
		}
	case "http":
		if strings.TrimSpace(req.URL) == "" {
			return errors.New("url required for http transport")
		}
	default:
		return errors.New("transport must be 'stdio' or 'http'")
	}
	return nil
}

type scannerRows interface {
	Scan(dest ...any) error
}

func scanMCPServer(rows scannerRows) (map[string]any, error) {
	var id, name, desc, transport, command, argsJSON, envJSON, url, headersJSON, createdAt, updatedAt string
	var timeoutSecs, enabled int
	if err := rows.Scan(&id, &name, &desc, &transport, &command, &argsJSON, &envJSON, &url, &headersJSON, &timeoutSecs, &enabled, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	return map[string]any{
		"id": id, "name": name, "description": desc,
		"transport": transport, "command": command,
		"args":            json.RawMessage(argsJSON),
		"env":             json.RawMessage(envJSON),
		"url":             url,
		"headers":         json.RawMessage(headersJSON),
		"timeout_seconds": timeoutSecs,
		"enabled":         enabled == 1,
		"created_at":      createdAt, "updated_at": updatedAt,
	}, nil
}

func loadMCPServerByID(ctx context.Context, db *sql.DB, id string) (mcp.ServerConfig, error) {
	var cfg mcp.ServerConfig
	var argsJSON, envJSON, headersJSON string
	err := db.QueryRowContext(ctx, `
		SELECT id,name,description,transport,command,args_json,env_json,url,headers_json,timeout_seconds
		FROM mcp_servers WHERE id=?
	`, id).Scan(&cfg.ID, &cfg.Name, &cfg.Description, &cfg.Transport,
		&cfg.Command, &argsJSON, &envJSON, &cfg.URL, &headersJSON, &cfg.TimeoutSeconds)
	if err != nil {
		return cfg, err
	}
	_ = json.Unmarshal([]byte(argsJSON), &cfg.Args)
	_ = json.Unmarshal([]byte(envJSON), &cfg.Env)
	_ = json.Unmarshal([]byte(headersJSON), &cfg.Headers)
	return cfg, nil
}

func buildClientFromCfg(cfg mcp.ServerConfig) mcp.Client {
	t := time.Duration(cfg.TimeoutSeconds) * time.Second
	if t <= 0 {
		t = 30 * time.Second
	}
	switch cfg.Transport {
	case "http":
		return mcp.NewHTTPClient(cfg.URL, cfg.Headers, t)
	default:
		return mcp.NewStdioClient(cfg.Command, cfg.Args, cfg.Env)
	}
}

