// Package llm provides a minimal provider-agnostic chat & tool-calling client interface.
package llm

import (
	"context"
	"encoding/json"
)

// Role identifies the speaker in a chat message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message represents a single chat turn or tool result.
type Message struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

// ToolCall represents a model-requested function invocation.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolDef describes a tool exposed to the model using JSON Schema.
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// Usage captures token accounting returned by the provider.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	CachedTokens     int `json:"cached_tokens,omitempty"`
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
}

// ChatRequest is a single model completion request.
type ChatRequest struct {
	Model     string
	SessionID string
	Messages  []Message
	Tools     []ToolDef
}

// ChatResponse is a normalized model completion response.
type ChatResponse struct {
	Message      Message
	FinishReason string
	Usage        Usage
	Model        string
}

// Client executes chat completions against an LLM provider or gateway.
type Client interface {
	Chat(ctx context.Context, req ChatRequest) (ChatResponse, error)
}
