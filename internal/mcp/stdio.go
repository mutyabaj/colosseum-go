package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/adevireddy/colosseum/internal/providers"
)

// StdioClient implements Client by spawning a subprocess and communicating
// via newline-delimited JSON-RPC 2.0 over stdin/stdout.
type StdioClient struct {
	command string
	args    []string
	env     map[string]string

	mu     sync.Mutex
	cmd    *exec.Cmd
	enc    *json.Encoder
	dec    *json.Decoder
	nextID atomic.Int32
}

func NewStdioClient(command string, args []string, env map[string]string) *StdioClient {
	return &StdioClient{command: command, args: args, env: env}
}

func (c *StdioClient) Connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	cmd := exec.CommandContext(ctx, c.command, c.args...)
	cmd.Env = os.Environ()
	for k, v := range c.env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %q: %w", c.command, err)
	}
	c.cmd = cmd
	c.enc = json.NewEncoder(stdin)
	c.dec = json.NewDecoder(bufio.NewReader(stdout))

	return c.initialize(ctx)
}

func (c *StdioClient) initialize(ctx context.Context) error {
	id := int(c.nextID.Add(1))
	if err := c.enc.Encode(rpcRequest{
		JSONRPC: "2.0",
		Method:  "initialize",
		Params: initializeParams{
			ProtocolVersion: protocolVersion,
			Capabilities:    clientCapabilities{Tools: map[string]any{}},
			ClientInfo:      clientInfo{Name: "colosseum", Version: "1.0"},
		},
		ID: &id,
	}); err != nil {
		return fmt.Errorf("send initialize: %w", err)
	}

	var resp rpcResponse
	if err := c.decodeWithTimeout(30 * time.Second, &resp); err != nil {
		return fmt.Errorf("read initialize response: %w", err)
	}
	if resp.Error != nil {
		return fmt.Errorf("initialize error %d: %s", resp.Error.Code, resp.Error.Message)
	}

	// notifications/initialized has no response
	nilID := (*int)(nil)
	_ = c.enc.Encode(rpcRequest{JSONRPC: "2.0", Method: "notifications/initialized", ID: nilID})
	return nil
}

// decodeWithTimeout decodes the next JSON value from the decoder with a deadline.
func (c *StdioClient) decodeWithTimeout(d time.Duration, v any) error {
	type result struct {
		err error
	}
	done := make(chan result, 1)
	go func() { done <- result{c.dec.Decode(v)} }()
	select {
	case r := <-done:
		return r.err
	case <-time.After(d):
		return fmt.Errorf("timed out waiting for MCP server response")
	}
}

func (c *StdioClient) ListTools(ctx context.Context, serverSlug string) ([]providers.Tool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	id := int(c.nextID.Add(1))
	if err := c.enc.Encode(rpcRequest{JSONRPC: "2.0", Method: "tools/list", Params: map[string]any{}, ID: &id}); err != nil {
		return nil, fmt.Errorf("send tools/list: %w", err)
	}
	var resp rpcResponse
	if err := c.decodeWithTimeout(30*time.Second, &resp); err != nil {
		return nil, fmt.Errorf("read tools/list: %w", err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("tools/list error: %s", resp.Error.Message)
	}
	return parseToolList(resp.Result, serverSlug)
}

func (c *StdioClient) CallTool(ctx context.Context, localName string, arguments json.RawMessage) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(arguments) == 0 {
		arguments = json.RawMessage(`{}`)
	}
	id := int(c.nextID.Add(1))
	if err := c.enc.Encode(rpcRequest{
		JSONRPC: "2.0",
		Method:  "tools/call",
		Params:  toolCallParams{Name: localName, Arguments: arguments},
		ID:      &id,
	}); err != nil {
		return "", fmt.Errorf("send tools/call: %w", err)
	}
	var resp rpcResponse
	if err := c.decodeWithTimeout(60*time.Second, &resp); err != nil {
		return "", fmt.Errorf("read tools/call response: %w", err)
	}
	if resp.Error != nil {
		return "", fmt.Errorf("tools/call error: %s", resp.Error.Message)
	}
	return parseToolResult(resp.Result)
}

func (c *StdioClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
		_ = c.cmd.Wait()
	}
	return nil
}

// ---- shared parse helpers ----

func parseToolList(raw json.RawMessage, serverSlug string) ([]providers.Tool, error) {
	var result toolsListResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("parse tools/list result: %w", err)
	}
	out := make([]providers.Tool, 0, len(result.Tools))
	for _, t := range result.Tools {
		schema := t.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, providers.Tool{
			Name:        ToolName(serverSlug, t.Name),
			Description: t.Description,
			InputSchema: schema,
		})
	}
	return out, nil
}

func parseToolResult(raw json.RawMessage) (string, error) {
	var result toolCallResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", fmt.Errorf("parse tools/call result: %w", err)
	}
	return extractText(result), nil
}
