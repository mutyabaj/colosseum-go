// Package mcp implements a Model Context Protocol client.
// It supports both stdio and HTTP transports and manages per-run MCP sessions.
package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"
	"unicode"

	"github.com/adevireddy/colosseum/internal/providers"
)

const protocolVersion = "2024-11-05"

// Prefix is the tool name prefix for all MCP-sourced tools.
const Prefix = "mcp__"

// ServerSlug converts a server name to a valid tool-name component
// (lowercase alphanumeric + underscores only).
func ServerSlug(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	return strings.Trim(b.String(), "_")
}

// ToolName builds the namespaced tool name: mcp__{serverSlug}__{localName}.
func ToolName(serverSlug, localName string) string {
	return Prefix + serverSlug + "__" + localName
}

// ParseToolName splits an mcp__-prefixed name into (serverSlug, localName, true).
// Returns ("", "", false) if the name is not an MCP tool.
func ParseToolName(name string) (serverSlug, localName string, ok bool) {
	if !strings.HasPrefix(name, Prefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(name, Prefix)
	idx := strings.Index(rest, "__")
	if idx < 0 {
		return "", "", false
	}
	return rest[:idx], rest[idx+2:], true
}

// ---- JSON-RPC 2.0 wire types ----

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
	ID      *int   `json:"id,omitempty"` // nil = notification (no response expected)
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
	ID      *int            `json:"id,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ---- MCP domain types ----

type initializeParams struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    clientCapabilities `json:"capabilities"`
	ClientInfo      clientInfo         `json:"clientInfo"`
}

type clientCapabilities struct {
	Tools map[string]any `json:"tools"`
}

type clientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type mcpTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

type toolsListResult struct {
	Tools []mcpTool `json:"tools"`
}

type toolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type contentItem struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type toolCallResult struct {
	Content []contentItem `json:"content"`
	IsError bool          `json:"isError"`
}

// extractText joins all text content items from a tool result into a single string.
func extractText(result toolCallResult) string {
	var parts []string
	for _, c := range result.Content {
		if c.Type == "text" && c.Text != "" {
			parts = append(parts, c.Text)
		}
	}
	text := strings.Join(parts, "\n")
	if result.IsError {
		return fmt.Sprintf("[mcp error] %s", text)
	}
	return text
}

// ---- Client interface ----

// Client is an active connection to one MCP server.
type Client interface {
	Connect(ctx context.Context) error
	ListTools(ctx context.Context, serverSlug string) ([]providers.Tool, error)
	// CallTool invokes a tool by its LOCAL name (without mcp__ prefix).
	CallTool(ctx context.Context, localName string, arguments json.RawMessage) (string, error)
	Close() error
}

// ---- ServerConfig mirrors one row from mcp_servers ----

type ServerConfig struct {
	ID             string
	Name           string
	Description    string
	Transport      string
	Command        string
	Args           []string
	Env            map[string]string
	URL            string
	Headers        map[string]string
	TimeoutSeconds int
}

// ---- Session: manages all MCP clients for one agent run ----

// Session holds the live MCP clients for a single run.
// It is created at run-start, used during the agent loop, and closed when the run ends.
type Session struct {
	// clientsBySlug maps server slug → connected Client
	clientsBySlug map[string]Client
	// slugByToolName maps full mcp__ tool name → server slug (for routing tool calls)
	slugByToolName map[string]string
}

// NewSession loads all MCP servers configured for the agent, connects them, and
// discovers their tools. Servers that fail to connect are logged and skipped.
func NewSession(ctx context.Context, db *sql.DB, agentID string) *Session {
	s := &Session{
		clientsBySlug:  make(map[string]Client),
		slugByToolName: make(map[string]string),
	}

	configs, err := loadAgentMCPServers(ctx, db, agentID)
	if err != nil {
		log.Printf("mcp: load servers for agent %s: %v", agentID, err)
		return s
	}

	for _, cfg := range configs {
		client := buildClient(cfg)
		if err := client.Connect(ctx); err != nil {
			log.Printf("mcp: connect to server %q: %v", cfg.Name, err)
			continue
		}
		slug := ServerSlug(cfg.Name)
		s.clientsBySlug[slug] = client
	}
	return s
}

// ListAllTools returns providers.Tool entries for every tool across all connected servers.
// Call this once at run-start to build the full tool list to pass to the LLM.
func (s *Session) ListAllTools(ctx context.Context) []providers.Tool {
	var out []providers.Tool
	for slug, client := range s.clientsBySlug {
		tools, err := client.ListTools(ctx, slug)
		if err != nil {
			log.Printf("mcp: list tools from %q: %v", slug, err)
			continue
		}
		for _, t := range tools {
			s.slugByToolName[t.Name] = slug
		}
		out = append(out, tools...)
	}
	return out
}

// CallTool routes a tool call (by full mcp__ name) to the correct server client.
func (s *Session) CallTool(ctx context.Context, fullName string, arguments json.RawMessage) (string, error) {
	slug, ok := s.slugByToolName[fullName]
	if !ok {
		return "", fmt.Errorf("mcp: no server found for tool %q", fullName)
	}
	client, ok := s.clientsBySlug[slug]
	if !ok {
		return "", fmt.Errorf("mcp: client for slug %q not connected", slug)
	}
	_, localName, _ := ParseToolName(fullName)
	return client.CallTool(ctx, localName, arguments)
}

// Close shuts down all connected MCP clients.
func (s *Session) Close() {
	for slug, client := range s.clientsBySlug {
		if err := client.Close(); err != nil {
			log.Printf("mcp: close client %q: %v", slug, err)
		}
	}
}

// ---- DB helpers ----

func loadAgentMCPServers(ctx context.Context, db *sql.DB, agentID string) ([]ServerConfig, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT s.id, s.name, s.description, s.transport,
		       s.command, s.args_json, s.env_json,
		       s.url, s.headers_json, s.timeout_seconds
		FROM mcp_servers s
		JOIN agent_mcp_servers a ON a.mcp_server_id = s.id
		WHERE a.agent_id = ? AND s.enabled = 1
	`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var configs []ServerConfig
	for rows.Next() {
		var cfg ServerConfig
		var argsJSON, envJSON, headersJSON string
		if err := rows.Scan(
			&cfg.ID, &cfg.Name, &cfg.Description, &cfg.Transport,
			&cfg.Command, &argsJSON, &envJSON,
			&cfg.URL, &headersJSON, &cfg.TimeoutSeconds,
		); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(argsJSON), &cfg.Args)
		_ = json.Unmarshal([]byte(envJSON), &cfg.Env)
		_ = json.Unmarshal([]byte(headersJSON), &cfg.Headers)
		if cfg.TimeoutSeconds <= 0 {
			cfg.TimeoutSeconds = 30
		}
		configs = append(configs, cfg)
	}
	return configs, rows.Err()
}

func buildClient(cfg ServerConfig) Client {
	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	switch cfg.Transport {
	case "http":
		return NewHTTPClient(cfg.URL, cfg.Headers, timeout)
	default: // "stdio"
		return NewStdioClient(cfg.Command, cfg.Args, cfg.Env)
	}
}
