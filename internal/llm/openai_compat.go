package llm

import (
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
	Usage struct {
		PromptTokens             int `json:"prompt_tokens"`
		CompletionTokens         int `json:"completion_tokens"`
		TotalTokens              int `json:"total_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		PromptTokensDetails      struct {
			CachedTokens        int `json:"cached_tokens"`
			CacheWriteTokens    int `json:"cache_write_tokens"`
			CacheCreationTokens int `json:"cache_creation_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
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

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
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

	cacheWriteTokens := parsed.Usage.CacheCreationInputTokens
	if cacheWriteTokens == 0 {
		cacheWriteTokens = parsed.Usage.PromptTokensDetails.CacheWriteTokens
	}
	if cacheWriteTokens == 0 {
		cacheWriteTokens = parsed.Usage.PromptTokensDetails.CacheCreationTokens
	}

	return ChatResponse{
		Message:      outMsg,
		FinishReason: choice.FinishReason,
		Model:        parsed.Model,
		Usage: Usage{
			PromptTokens:     parsed.Usage.PromptTokens,
			CompletionTokens: parsed.Usage.CompletionTokens,
			TotalTokens:      parsed.Usage.TotalTokens,
			CachedTokens:     parsed.Usage.PromptTokensDetails.CachedTokens,
			CacheWriteTokens: cacheWriteTokens,
		},
	}, nil
}
