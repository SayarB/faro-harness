package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	// DefaultGatewayBaseURL points to Vercel AI Gateway's OpenAI-compatible API.
	DefaultGatewayBaseURL = "https://ai-gateway.vercel.sh/v1"
	// DefaultModel is a fast, low-cost default routed through Vercel AI Gateway.
	DefaultModel = "openai/gpt-6-luna"
)

// OpenAICompatClient calls any OpenAI Chat Completions-compatible endpoint
// (including Vercel AI Gateway at https://ai-gateway.vercel.sh/v1).
type OpenAICompatClient struct {
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client
	// EnableGatewayCaching sends Vercel AI Gateway providerOptions.gateway
	// (caching: "auto" + sessionId) and the x-session-id header.
	// NewVercelGatewayClient enables this automatically for ai-gateway.vercel.sh
	// (overridable via FARO_GATEWAY_CACHING=on|off); it is opt-in when constructing
	// OpenAICompatClient directly.
	EnableGatewayCaching bool
}

// NewVercelGatewayClient creates a client configured for Vercel AI Gateway.
// Falls back to AI_GATEWAY_API_KEY and FARO_LLM_BASE_URL environment variables when unset.
func NewVercelGatewayClient(apiKey, baseURL string) (*OpenAICompatClient, error) {
	if strings.TrimSpace(apiKey) == "" {
		apiKey = strings.TrimSpace(os.Getenv("AI_GATEWAY_API_KEY"))
	}
	if strings.TrimSpace(baseURL) == "" {
		baseURL = strings.TrimSpace(os.Getenv("FARO_LLM_BASE_URL"))
	}
	if baseURL == "" {
		baseURL = DefaultGatewayBaseURL
	}
	if apiKey == "" {
		return nil, errors.New("AI_GATEWAY_API_KEY is required (set env var or pass API key)")
	}
	trimmedBase := strings.TrimRight(baseURL, "/")
	enableCaching := isVercelGatewayURL(trimmedBase)
	if override, ok := parseGatewayCachingOverride(os.Getenv("FARO_GATEWAY_CACHING")); ok {
		enableCaching = override
	}
	return &OpenAICompatClient{
		BaseURL: trimmedBase,
		APIKey:  apiKey,
		HTTPClient: &http.Client{
			Timeout: 120 * time.Second,
		},
		EnableGatewayCaching: enableCaching,
	}, nil
}

func parseGatewayCachingOverride(raw string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "on", "yes":
		return true, true
	case "0", "false", "off", "no":
		return false, true
	default:
		return false, false
	}
}

func isVercelGatewayURL(rawURL string) bool {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Hostname(), "ai-gateway.vercel.sh")
}

func isHeaderSafeSessionID(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '_' || c == '-' || c == '.' || c == ':'
		if !ok {
			return false
		}
	}
	return true
}

type wireChatRequest struct {
	Model           string               `json:"model"`
	Messages        []wireMessage        `json:"messages"`
	Tools           []wireTool           `json:"tools,omitempty"`
	ToolChoice      string               `json:"tool_choice,omitempty"`
	ProviderOptions *wireProviderOptions `json:"providerOptions,omitempty"`
	Stream          bool                 `json:"stream,omitempty"`
	StreamOptions   *wireStreamOptions   `json:"stream_options,omitempty"`
}

type wireStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type wireProviderOptions struct {
	Gateway *wireGatewayOptions `json:"gateway,omitempty"`
}

type wireGatewayOptions struct {
	Caching   string `json:"caching,omitempty"`
	SessionID string `json:"sessionId,omitempty"`
}

type wireMessage struct {
	Role       string         `json:"role"`
	Content    any            `json:"content"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	Name       string         `json:"name,omitempty"`
}

type wireToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function wireFunctionCall `json:"function"`
}

type wireFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type wireTool struct {
	Type     string          `json:"type"`
	Function wireFunctionDef `json:"function"`
}

type wireFunctionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type wireChatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Role      string         `json:"role"`
			Content   string         `json:"content"`
			ToolCalls []wireToolCall `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage wireUsage     `json:"usage"`
	Error *wireAPIError `json:"error,omitempty"`
}

type wireUsage struct {
	PromptTokens             int `json:"prompt_tokens"`
	CompletionTokens         int `json:"completion_tokens"`
	TotalTokens              int `json:"total_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	PromptTokensDetails      struct {
		CachedTokens        int `json:"cached_tokens"`
		CacheWriteTokens    int `json:"cache_write_tokens"`
		CacheCreationTokens int `json:"cache_creation_tokens"`
	} `json:"prompt_tokens_details"`
}

func (u wireUsage) toUsage() Usage {
	cacheWrite := u.CacheCreationInputTokens
	if cacheWrite == 0 {
		cacheWrite = u.PromptTokensDetails.CacheWriteTokens
	}
	if cacheWrite == 0 {
		cacheWrite = u.PromptTokensDetails.CacheCreationTokens
	}
	return Usage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
		CachedTokens:     u.PromptTokensDetails.CachedTokens,
		CacheWriteTokens: cacheWrite,
	}
}

type wireStreamChunk struct {
	Model   string `json:"model"`
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Delta        struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *wireUsage    `json:"usage"`
	Error *wireAPIError `json:"error,omitempty"`
}

type wireAPIError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    any    `json:"code"`
}

// Chat sends a Chat Completions request to the configured OpenAI-compatible gateway.
func (c *OpenAICompatClient) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	if c == nil {
		return ChatResponse{}, errors.New("llm client is nil")
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = DefaultModel
	}

	wireMsgs := make([]wireMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		wm := wireMessage{
			Role:       string(m.Role),
			ToolCallID: m.ToolCallID,
			Name:       m.Name,
		}
		if len(m.ToolCalls) > 0 {
			if m.Content != "" {
				wm.Content = m.Content
			} else {
				wm.Content = nil
			}
			wm.ToolCalls = make([]wireToolCall, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				wm.ToolCalls = append(wm.ToolCalls, wireToolCall{
					ID:   tc.ID,
					Type: "function",
					Function: wireFunctionCall{
						Name:      tc.Name,
						Arguments: tc.Arguments,
					},
				})
			}
		} else {
			wm.Content = m.Content
		}
		wireMsgs = append(wireMsgs, wm)
	}

	sessionID := strings.TrimSpace(req.SessionID)
	payload := wireChatRequest{
		Model:    model,
		Messages: wireMsgs,
	}
	if c.EnableGatewayCaching {
		payload.ProviderOptions = &wireProviderOptions{
			Gateway: &wireGatewayOptions{
				Caching:   "auto",
				SessionID: sessionID,
			},
		}
	}
	if len(req.Tools) > 0 {
		payload.Tools = make([]wireTool, 0, len(req.Tools))
		for _, t := range req.Tools {
			payload.Tools = append(payload.Tools, wireTool{
				Type: "function",
				Function: wireFunctionDef{
					Name:        t.Name,
					Description: t.Description,
					Parameters:  t.Parameters,
				},
			})
		}
		payload.ToolChoice = "auto"
	}

	streaming := req.OnDelta != nil
	if streaming {
		payload.Stream = true
		payload.StreamOptions = &wireStreamOptions{IncludeUsage: true}
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("marshal chat request: %w", err)
	}

	endpoint := strings.TrimRight(c.BaseURL, "/") + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return ChatResponse{}, fmt.Errorf("create http request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	if c.EnableGatewayCaching && isHeaderSafeSessionID(sessionID) {
		httpReq.Header.Set("x-session-id", sessionID)
	}

	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("llm request failed: %w", err)
	}
	defer resp.Body.Close()

	if streaming && resp.StatusCode < 400 {
		return readChatStream(resp.Body, req.OnDelta)
	}

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxChatResponseBytes))
	if err != nil {
		return ChatResponse{}, fmt.Errorf("read llm response: %w", err)
	}

	var parsed wireChatResponse
	if err := json.Unmarshal(respBytes, &parsed); err != nil {
		if resp.StatusCode >= 400 {
			return ChatResponse{}, fmt.Errorf("llm gateway error (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(respBytes)))
		}
		return ChatResponse{}, fmt.Errorf("decode llm response: %w", err)
	}
	if resp.StatusCode >= 400 || parsed.Error != nil {
		msg := strings.TrimSpace(string(respBytes))
		if parsed.Error != nil && parsed.Error.Message != "" {
			msg = parsed.Error.Message
		}
		return ChatResponse{}, fmt.Errorf("llm gateway error (HTTP %d): %s", resp.StatusCode, msg)
	}
	if len(parsed.Choices) == 0 {
		return ChatResponse{}, errors.New("llm response contained no choices")
	}

	choice := parsed.Choices[0]
	outMsg := Message{
		Role:    RoleAssistant,
		Content: choice.Message.Content,
	}
	if len(choice.Message.ToolCalls) > 0 {
		outMsg.ToolCalls = make([]ToolCall, 0, len(choice.Message.ToolCalls))
		for _, tc := range choice.Message.ToolCalls {
			outMsg.ToolCalls = append(outMsg.ToolCalls, ToolCall{
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			})
		}
	}

	return ChatResponse{
		Message:      outMsg,
		FinishReason: choice.FinishReason,
		Model:        parsed.Model,
		Usage:        parsed.Usage.toUsage(),
	}, nil
}

const maxChatResponseBytes = 4 * 1024 * 1024

// readChatStream assembles a streamed (SSE) chat completion into a ChatResponse,
// calling onDelta for every text fragment. Tool-call fragments are merged by index.
func readChatStream(body io.Reader, onDelta func(string)) (ChatResponse, error) {
	sc := bufio.NewScanner(io.LimitReader(body, maxChatResponseBytes))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var (
		content strings.Builder
		calls   []ToolCall
		out     ChatResponse
	)
	out.Message.Role = RoleAssistant

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk wireStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return ChatResponse{}, fmt.Errorf("decode llm stream chunk: %w", err)
		}
		if chunk.Error != nil {
			return ChatResponse{}, fmt.Errorf("llm gateway error: %s", chunk.Error.Message)
		}
		if chunk.Model != "" {
			out.Model = chunk.Model
		}
		if chunk.Usage != nil {
			out.Usage = chunk.Usage.toUsage()
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		choice := chunk.Choices[0]
		if choice.FinishReason != "" {
			out.FinishReason = choice.FinishReason
		}
		if choice.Delta.Content != "" {
			content.WriteString(choice.Delta.Content)
			onDelta(choice.Delta.Content)
		}
		for _, tc := range choice.Delta.ToolCalls {
			if tc.Index < 0 || tc.Index > 64 {
				return ChatResponse{}, fmt.Errorf("llm stream: invalid tool call index %d", tc.Index)
			}
			for len(calls) <= tc.Index {
				calls = append(calls, ToolCall{})
			}
			if tc.ID != "" {
				calls[tc.Index].ID = tc.ID
			}
			if tc.Function.Name != "" {
				calls[tc.Index].Name = tc.Function.Name
			}
			calls[tc.Index].Arguments += tc.Function.Arguments
		}
	}
	if err := sc.Err(); err != nil {
		return ChatResponse{}, fmt.Errorf("read llm stream: %w", err)
	}
	if content.Len() == 0 && len(calls) == 0 && out.FinishReason == "" {
		return ChatResponse{}, errors.New("llm stream ended without a response")
	}
	out.Message.Content = content.String()
	out.Message.ToolCalls = calls
	return out, nil
}
