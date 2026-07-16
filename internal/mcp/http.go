package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/adevireddy/colosseum/internal/providers"
)

// HTTPClient implements Client using the MCP Streamable HTTP transport.
// All JSON-RPC messages are sent as HTTP POST to a single endpoint URL.
type HTTPClient struct {
	url       string
	headers   map[string]string
	timeout   time.Duration
	nextID    atomic.Int32
	http      *http.Client
	sessionID atomic.Value // string
}

func NewHTTPClient(url string, headers map[string]string, timeout time.Duration) *HTTPClient {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &HTTPClient{
		url:     url,
		headers: headers,
		timeout: timeout,
		http:    &http.Client{Timeout: timeout + 5*time.Second},
	}
}

func (c *HTTPClient) doRPC(ctx context.Context, req rpcRequest) (rpcResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return rpcResponse{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return rpcResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range c.headers {
		httpReq.Header.Set(k, v)
	}
	if sid, ok := c.sessionID.Load().(string); ok && sid != "" {
		httpReq.Header.Set("Mcp-Session-Id", sid)
	}
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return rpcResponse{}, fmt.Errorf("http request failed: %w", err)
	}
	defer resp.Body.Close()
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.sessionID.Store(sid)
	}
	if resp.StatusCode >= 400 {
		return rpcResponse{}, fmt.Errorf("server returned HTTP %d", resp.StatusCode)
	}
	var rpcResp rpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		return rpcResponse{}, fmt.Errorf("decode response: %w", err)
	}
	return rpcResp, nil
}

func (c *HTTPClient) Connect(ctx context.Context) error {
	id := int(c.nextID.Add(1))
	resp, err := c.doRPC(ctx, rpcRequest{
		JSONRPC: "2.0",
		Method:  "initialize",
		Params: initializeParams{
			ProtocolVersion: protocolVersion,
			Capabilities:    clientCapabilities{Tools: map[string]any{}},
			ClientInfo:      clientInfo{Name: "colosseum", Version: "1.0"},
		},
		ID: &id,
	})
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	if resp.Error != nil {
		return fmt.Errorf("initialize error %d: %s", resp.Error.Code, resp.Error.Message)
	}
	// notifications/initialized — fire and forget
	nilID := (*int)(nil)
	_, _ = c.doRPC(ctx, rpcRequest{JSONRPC: "2.0", Method: "notifications/initialized", ID: nilID})
	return nil
}

func (c *HTTPClient) ListTools(ctx context.Context, serverSlug string) ([]providers.Tool, error) {
	id := int(c.nextID.Add(1))
	resp, err := c.doRPC(ctx, rpcRequest{JSONRPC: "2.0", Method: "tools/list", Params: map[string]any{}, ID: &id})
	if err != nil {
		return nil, fmt.Errorf("tools/list: %w", err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("tools/list error: %s", resp.Error.Message)
	}
	return parseToolList(resp.Result, serverSlug)
}

func (c *HTTPClient) CallTool(ctx context.Context, localName string, arguments json.RawMessage) (string, error) {
	if len(arguments) == 0 {
		arguments = json.RawMessage(`{}`)
	}
	id := int(c.nextID.Add(1))
	resp, err := c.doRPC(ctx, rpcRequest{
		JSONRPC: "2.0",
		Method:  "tools/call",
		Params:  toolCallParams{Name: localName, Arguments: arguments},
		ID:      &id,
	})
	if err != nil {
		return "", fmt.Errorf("tools/call: %w", err)
	}
	if resp.Error != nil {
		return "", fmt.Errorf("tools/call error: %s", resp.Error.Message)
	}
	return parseToolResult(resp.Result)
}

func (c *HTTPClient) Close() error { return nil }
