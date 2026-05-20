package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type Message struct {
	Role         string        `json:"role"`
	Content      string        `json:"content"`
	Source       string        `json:"source,omitempty"`
	ContentParts []ContentPart `json:"content_parts,omitempty"`
	ToolCallID   string        `json:"tool_call_id,omitempty"`
	Name         string        `json:"name,omitempty"`
	ToolCalls    []ToolCall    `json:"tool_calls,omitempty"`
}

type ContentPart struct {
	Type string `json:"type"` // text | image_url
	Text string `json:"text,omitempty"`
	URL  string `json:"url,omitempty"` // data URL or remote URL
}

type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type CompletionRequest struct {
	Model    string    `json:"model"`
	System   string    `json:"system"`
	Messages []Message `json:"messages"`
	Tools    []Tool    `json:"tools"`
	Timeout  time.Duration
}

type CompletionResponse struct {
	Text      string     `json:"text"`
	ToolCalls []ToolCall `json:"tool_calls"`
	Usage     Usage      `json:"usage"`
	Raw       string     `json:"raw"`
}

type Client interface {
	Complete(ctx context.Context, req CompletionRequest) (CompletionResponse, error)
	ProviderName() string
}

type AnthropicClient struct {
	APIKey     string
	HTTPClient *http.Client
	BaseURL    string
}

func (c *AnthropicClient) ProviderName() string { return "anthropic" }

func (c *AnthropicClient) Complete(ctx context.Context, req CompletionRequest) (CompletionResponse, error) {
	if c.HTTPClient == nil {
		c.HTTPClient = &http.Client{Timeout: 90 * time.Second}
	}
	messages := make([]map[string]any, 0, len(req.Messages))
	for i := 0; i < len(req.Messages); i++ {
		m := req.Messages[i]
		if m.Role == "tool" {
			// Batch all consecutive tool results into a single user message
			toolResults := []map[string]any{{
				"type":        "tool_result",
				"tool_use_id": m.ToolCallID,
				"content":     m.Content,
			}}
			for i+1 < len(req.Messages) && req.Messages[i+1].Role == "tool" {
				i++
				toolResults = append(toolResults, map[string]any{
					"type":        "tool_result",
					"tool_use_id": req.Messages[i].ToolCallID,
					"content":     req.Messages[i].Content,
				})
			}
			messages = append(messages, map[string]any{"role": "user", "content": toolResults})
			continue
		}
		if len(m.ContentParts) > 0 {
			blocks := make([]map[string]any, 0, len(m.ContentParts))
			for _, part := range m.ContentParts {
				switch strings.ToLower(strings.TrimSpace(part.Type)) {
				case "text":
					if strings.TrimSpace(part.Text) == "" {
						continue
					}
					blocks = append(blocks, map[string]any{"type": "text", "text": part.Text})
				case "image_url":
					source, ok := dataURLToAnthropicImageSource(part.URL)
					if !ok {
						continue
					}
					blocks = append(blocks, map[string]any{"type": "image", "source": source})
				}
			}
			if len(blocks) == 0 {
				messages = append(messages, map[string]any{"role": m.Role, "content": m.Content})
			} else {
				messages = append(messages, map[string]any{"role": m.Role, "content": blocks})
			}
			continue
		}
		// Assistant messages with tool calls need tool_use content blocks
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			blocks := []map[string]any{}
			if strings.TrimSpace(m.Content) != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": m.Content})
			}
			for _, tc := range m.ToolCalls {
				sanitizedName := sanitizeToolNameForOpenAI(tc.Name)
				var input any
				if err := json.Unmarshal(tc.Arguments, &input); err != nil {
					input = map[string]any{}
				}
				blocks = append(blocks, map[string]any{
					"type":  "tool_use",
					"id":    tc.ID,
					"name":  sanitizedName,
					"input": input,
				})
			}
			messages = append(messages, map[string]any{"role": "assistant", "content": blocks})
			continue
		}
		messages = append(messages, map[string]any{"role": m.Role, "content": m.Content})
	}
	tools := make([]map[string]any, 0, len(req.Tools))
	anthropicToInternalToolName := map[string]string{}
	for _, t := range req.Tools {
		var schema map[string]any
		_ = json.Unmarshal(t.InputSchema, &schema)
		sanitizedName := sanitizeToolNameForOpenAI(t.Name)
		anthropicToInternalToolName[sanitizedName] = t.Name
		tools = append(tools, map[string]any{
			"name":         sanitizedName,
			"description":  t.Description,
			"input_schema": schema,
		})
	}
	payload := map[string]any{
		"model":      req.Model,
		"max_tokens": 16000,
		"system":     req.System,
		"messages":   messages,
	}
	if len(tools) > 0 {
		payload["tools"] = tools
	}
	buf, _ := json.Marshal(payload)
	baseURL := c.BaseURL
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	httpReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/messages", bytes.NewReader(buf))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", c.APIKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return CompletionResponse{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return CompletionResponse{}, fmt.Errorf("anthropic status=%d body=%s", resp.StatusCode, string(body))
	}
	var parsed struct {
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return CompletionResponse{}, err
	}
	out := CompletionResponse{Usage: Usage(parsed.Usage), Raw: string(body)}
	var texts []string
	for _, c := range parsed.Content {
		if c.Type == "text" {
			texts = append(texts, c.Text)
		}
		if c.Type == "tool_use" {
			args := c.Input
			if len(args) == 0 {
				args = json.RawMessage(`{}`)
			}
			internalName := c.Name
			if mapped, ok := anthropicToInternalToolName[c.Name]; ok {
				internalName = mapped
			}
			out.ToolCalls = append(out.ToolCalls, ToolCall{ID: c.ID, Name: internalName, Arguments: args})
		}
	}
	out.Text = strings.TrimSpace(strings.Join(texts, "\n"))
	return out, nil
}

type OpenAIClient struct {
	APIKey     string
	HTTPClient *http.Client
	BaseURL    string
}

func (c *OpenAIClient) ProviderName() string { return "openai" }

func (c *OpenAIClient) Complete(ctx context.Context, req CompletionRequest) (CompletionResponse, error) {
	if c.HTTPClient == nil {
		c.HTTPClient = &http.Client{Timeout: 90 * time.Second}
	}
	messages := make([]map[string]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		row := map[string]any{"role": m.Role}
		if len(m.ContentParts) > 0 {
			parts := make([]map[string]any, 0, len(m.ContentParts))
			for _, part := range m.ContentParts {
				switch strings.ToLower(strings.TrimSpace(part.Type)) {
				case "text":
					if strings.TrimSpace(part.Text) == "" {
						continue
					}
					parts = append(parts, map[string]any{"type": "text", "text": part.Text})
				case "image_url":
					if strings.TrimSpace(part.URL) == "" {
						continue
					}
					parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": part.URL}})
				}
			}
			if len(parts) > 0 {
				row["content"] = parts
			} else {
				row["content"] = m.Content
			}
		} else {
			row["content"] = m.Content
		}
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			toolCalls := make([]map[string]any, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				toolCalls = append(toolCalls, map[string]any{
					"id":   tc.ID,
					"type": "function",
					"function": map[string]any{
						"name":      sanitizeToolNameForOpenAI(tc.Name),
						"arguments": formatOpenAIFunctionArguments(tc.Arguments),
					},
				})
			}
			row["tool_calls"] = toolCalls
		}
		if m.Role == "tool" {
			row["tool_call_id"] = m.ToolCallID
		}
		if m.Name != "" {
			row["name"] = m.Name
		}
		messages = append(messages, row)
	}
	tools := make([]map[string]any, 0, len(req.Tools))
	openAIToInternalToolName := map[string]string{}
	usedToolNames := map[string]bool{}
	for i, t := range req.Tools {
		var schema map[string]any
		_ = json.Unmarshal(t.InputSchema, &schema)
		openAIName := sanitizeToolNameForOpenAI(t.Name)
		if openAIName == "" {
			openAIName = fmt.Sprintf("tool_%d", i+1)
		}
		for usedToolNames[openAIName] {
			openAIName = fmt.Sprintf("%s_%d", openAIName, i+1)
		}
		usedToolNames[openAIName] = true
		openAIToInternalToolName[openAIName] = t.Name
		tools = append(tools, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        openAIName,
				"description": t.Description,
				"parameters":  schema,
			},
		})
	}
	payload := map[string]any{
		"model":    req.Model,
		"messages": messages,
	}
	if req.System != "" {
		payload["messages"] = append([]map[string]any{{"role": "system", "content": req.System}}, messages...)
	}
	if len(tools) > 0 {
		payload["tools"] = tools
		payload["tool_choice"] = "auto"
	}
	buf, _ := json.Marshal(payload)
	baseURL := c.BaseURL
	if baseURL == "" {
		baseURL = "https://api.openai.com"
	}
	var body []byte
	retryBackoff := 15 * time.Second
	for attempt := 0; attempt < 5; attempt++ {
		httpReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/chat/completions", bytes.NewReader(buf))
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
		resp, err := c.HTTPClient.Do(httpReq)
		if err != nil {
			return CompletionResponse{}, err
		}
		body, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == 429 {
			if attempt == 4 {
				return CompletionResponse{}, fmt.Errorf("openai status=%d body=%s", resp.StatusCode, string(body))
			}
			wait := retryBackoff
			if ra := resp.Header.Get("Retry-After"); ra != "" {
				if secs, err2 := strconv.Atoi(ra); err2 == nil {
					wait = time.Duration(secs) * time.Second
				}
			}
			select {
			case <-ctx.Done():
				return CompletionResponse{}, ctx.Err()
			case <-time.After(wait):
			}
			retryBackoff *= 2
			continue
		}
		if resp.StatusCode >= 300 {
			return CompletionResponse{}, fmt.Errorf("openai status=%d body=%s", resp.StatusCode, string(body))
		}
		break
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string          `json:"name"`
						Arguments json.RawMessage `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return CompletionResponse{}, err
	}
	if len(parsed.Choices) == 0 {
		return CompletionResponse{}, fmt.Errorf("openai returned no choices")
	}
	choice := parsed.Choices[0].Message
	out := CompletionResponse{Text: strings.TrimSpace(choice.Content), Usage: Usage{InputTokens: parsed.Usage.PromptTokens, OutputTokens: parsed.Usage.CompletionTokens}, Raw: string(body)}
	for _, t := range choice.ToolCalls {
		internalName := t.Function.Name
		if mapped, ok := openAIToInternalToolName[t.Function.Name]; ok {
			internalName = mapped
		}
		args := normalizeOpenAIArguments(t.Function.Arguments)
		if len(args) == 0 {
			args = json.RawMessage(`{}`)
		}
		out.ToolCalls = append(out.ToolCalls, ToolCall{ID: t.ID, Name: internalName, Arguments: args})
	}
	return out, nil
}

func sanitizeToolNameForOpenAI(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

func normalizeOpenAIArguments(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`{}`)
	}

	// OpenAI usually returns function.arguments as a JSON string.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		trimmed := strings.TrimSpace(s)
		if trimmed == "" {
			return json.RawMessage(`{}`)
		}
		return json.RawMessage(trimmed)
	}

	// Some compatible providers may already return an object.
	return raw
}

func formatOpenAIFunctionArguments(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	// If already quoted JSON string, pass it through as plain string.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if strings.TrimSpace(s) == "" {
			return "{}"
		}
		return s
	}
	// Otherwise compact raw JSON object/array into string form.
	var v any
	if err := json.Unmarshal(raw, &v); err == nil {
		b, err := json.Marshal(v)
		if err == nil {
			return string(b)
		}
	}
	return "{}"
}

func dataURLToAnthropicImageSource(raw string) (map[string]any, bool) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "data:") {
		return nil, false
	}
	comma := strings.Index(raw, ",")
	if comma <= 5 || comma >= len(raw)-1 {
		return nil, false
	}
	meta := raw[5:comma]
	data := raw[comma+1:]
	if !strings.Contains(strings.ToLower(meta), ";base64") {
		return nil, false
	}
	mediaType := strings.TrimSpace(strings.Split(meta, ";")[0])
	if mediaType == "" || !strings.HasPrefix(strings.ToLower(mediaType), "image/") {
		return nil, false
	}
	return map[string]any{
		"type":       "base64",
		"media_type": mediaType,
		"data":       data,
	}, true
}
