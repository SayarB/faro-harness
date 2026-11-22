package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAICompatClientChatAndToolCalls(t *testing.T) {
	var gotAuth string
	var gotPath string
	var gotReq wireChatRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"model": "anthropic/claude-sonnet-4.5",
			"choices": [{
				"finish_reason": "tool_calls",
				"message": {
					"role": "assistant",
					"content": "",
					"tool_calls": [{
						"id": "call_123",
						"type": "function",
						"function": {
							"name": "search_files",
							"arguments": "{\"query\":\"Acme invoice\"}"
						}
					}]
				}
			}],
			"usage": {
				"prompt_tokens": 210,
				"completion_tokens": 24,
				"total_tokens": 234,
				"prompt_tokens_details": {"cached_tokens": 120}
			}
		}`))
	}))
	defer srv.Close()

	client, err := NewVercelGatewayClient("test-key-xyz", srv.URL+"/v1")
	if err != nil {
		t.Fatalf("NewVercelGatewayClient: %v", err)
	}

	resp, err := client.Chat(context.Background(), ChatRequest{
		Model: "anthropic/claude-sonnet-4.5",
		Messages: []Message{
			{Role: RoleSystem, Content: "You are Faro."},
			{Role: RoleUser, Content: "Find Acme invoice"},
		},
		Tools: []ToolDef{
			{
				Name:        "search_files",
				Description: "Search files",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}}}`),
			},
		},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	if gotAuth != "Bearer test-key-xyz" {
		t.Fatalf("unexpected auth header %q", gotAuth)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("unexpected path %q", gotPath)
	}
	if gotReq.ToolChoice != "auto" || len(gotReq.Tools) != 1 || gotReq.Tools[0].Function.Name != "search_files" {
		t.Fatalf("unexpected tools payload: %+v", gotReq)
	}
	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(resp.Message.ToolCalls))
	}
	tc := resp.Message.ToolCalls[0]
	if tc.ID != "call_123" || tc.Name != "search_files" || tc.Arguments != `{"query":"Acme invoice"}` {
		t.Fatalf("unexpected tool call: %+v", tc)
	}
	if resp.Usage.PromptTokens != 210 || resp.Usage.CachedTokens != 120 {
		t.Fatalf("unexpected usage: %+v", resp.Usage)
	}
	if client.EnableGatewayCaching {
		t.Fatalf("expected EnableGatewayCaching=false for non-Vercel test server URL")
	}
	if gotReq.ProviderOptions != nil {
		t.Fatalf("expected ProviderOptions omitted for non-Vercel base URL, got %+v", gotReq.ProviderOptions)
	}
}

func TestOpenAICompatClientGatewayCachingAndSessionAffinity(t *testing.T) {
	var gotSessionHeader string
	var gotReq wireChatRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSessionHeader = r.Header.Get("x-session-id")
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"model": "anthropic/claude-haiku-5.5",
			"choices": [{
				"finish_reason": "stop",
				"message": {
					"role": "assistant",
					"content": "ok"
				}
			}],
			"usage": {
				"prompt_tokens": 1125,
				"completion_tokens": 5,
				"total_tokens": 1130,
				"cache_creation_input_tokens": 14,
				"prompt_tokens_details": {"cached_tokens": 1107}
			}
		}`))
	}))
	defer srv.Close()

	client, err := NewVercelGatewayClient("test-key-xyz", srv.URL+"/v1")
	if err != nil {
		t.Fatalf("NewVercelGatewayClient: %v", err)
	}
	client.EnableGatewayCaching = true

	resp, err := client.Chat(context.Background(), ChatRequest{
		Model:     "anthropic/claude-haiku-5.5",
		SessionID: "conv_abc123",
		Messages:  []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	if gotSessionHeader != "conv_abc123" {
		t.Fatalf("expected x-session-id=conv_abc123, got %q", gotSessionHeader)
	}
	if gotReq.ProviderOptions == nil || gotReq.ProviderOptions.Gateway == nil {
		t.Fatalf("expected providerOptions.gateway to be set, got %+v", gotReq.ProviderOptions)
	}
	if gotReq.ProviderOptions.Gateway.Caching != "auto" || gotReq.ProviderOptions.Gateway.SessionID != "conv_abc123" {
		t.Fatalf("unexpected gateway options: %+v", gotReq.ProviderOptions.Gateway)
	}
	if resp.Usage.CachedTokens != 1107 || resp.Usage.CacheWriteTokens != 14 {
		t.Fatalf("unexpected cache usage: %+v", resp.Usage)
	}
}

func TestOpenAICompatClientGatewayCachingEdgeCases(t *testing.T) {
	var gotHeader string
	var rawBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("x-session-id")
		rawBody = nil
		if err := json.NewDecoder(r.Body).Decode(&rawBody); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"model": "anthropic/claude-haiku-5.5",
			"choices": [{"finish_reason": "stop", "message": {"role": "assistant", "content": "ok"}}],
			"usage": {
				"prompt_tokens": 500,
				"completion_tokens": 5,
				"total_tokens": 505,
				"cache_creation_input_tokens": null,
				"prompt_tokens_details": {"cached_tokens": 200, "cache_write_tokens": 42}
			}
		}`))
	}))
	defer srv.Close()

	// 1. Caching enabled with empty/whitespace SessionID: caching:"auto" sent, sessionId & x-session-id omitted,
	//    null cache_creation_input_tokens falls back to prompt_tokens_details.cache_write_tokens.
	client := &OpenAICompatClient{BaseURL: srv.URL + "/v1", EnableGatewayCaching: true}
	resp, err := client.Chat(context.Background(), ChatRequest{
		Model:     "anthropic/claude-haiku-5.5",
		SessionID: "   ",
		Messages:  []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat (blank session): %v", err)
	}
	if gotHeader != "" {
		t.Fatalf("expected empty x-session-id header, got %q", gotHeader)
	}
	gw := rawBody["providerOptions"].(map[string]any)["gateway"].(map[string]any)
	if gw["caching"] != "auto" {
		t.Fatalf("expected caching=auto, got %+v", gw)
	}
	if _, exists := gw["sessionId"]; exists {
		t.Fatalf("expected sessionId omitted for blank SessionID, got %+v", gw)
	}
	if resp.Usage.CacheWriteTokens != 42 || resp.Usage.CachedTokens != 200 {
		t.Fatalf("expected fallback cache_write_tokens=42 and cached_tokens=200, got %+v", resp.Usage)
	}

	// 2. Whitespace around SessionID is trimmed in both header and body.
	if _, err := client.Chat(context.Background(), ChatRequest{
		SessionID: "  conv_trim_1  ",
		Messages:  []Message{{Role: RoleUser, Content: "hi"}},
	}); err != nil {
		t.Fatalf("Chat (trimmed session): %v", err)
	}
	if gotHeader != "conv_trim_1" {
		t.Fatalf("expected trimmed x-session-id=conv_trim_1, got %q", gotHeader)
	}
	gw = rawBody["providerOptions"].(map[string]any)["gateway"].(map[string]any)
	if gw["sessionId"] != "conv_trim_1" {
		t.Fatalf("expected trimmed body sessionId=conv_trim_1, got %+v", gw)
	}

	// 3. Header-unsafe SessionID (CR/LF) drops x-session-id without failing the request.
	if _, err := client.Chat(context.Background(), ChatRequest{
		SessionID: "bad\r\nid",
		Messages:  []Message{{Role: RoleUser, Content: "hi"}},
	}); err != nil {
		t.Fatalf("Chat (unsafe session): %v", err)
	}
	if gotHeader != "" {
		t.Fatalf("expected unsafe x-session-id header dropped, got %q", gotHeader)
	}

	// 4. Zero-value OpenAICompatClient omits providerOptions and x-session-id.
	plain := &OpenAICompatClient{BaseURL: srv.URL + "/v1"}
	if _, err := plain.Chat(context.Background(), ChatRequest{
		SessionID: "conv_plain",
		Messages:  []Message{{Role: RoleUser, Content: "hi"}},
	}); err != nil {
		t.Fatalf("Chat (zero-value client): %v", err)
	}
	if gotHeader != "" {
		t.Fatalf("expected no x-session-id on zero-value client, got %q", gotHeader)
	}
	if _, exists := rawBody["providerOptions"]; exists {
		t.Fatalf("expected no providerOptions on zero-value client, got %+v", rawBody["providerOptions"])
	}
}

func TestIsVercelGatewayURLAndEnvOverride(t *testing.T) {
	if !isVercelGatewayURL(DefaultGatewayBaseURL) {
		t.Fatalf("expected DefaultGatewayBaseURL to be recognized as Vercel AI Gateway")
	}
	if !isVercelGatewayURL("https://AI-GATEWAY.VERCEL.SH/v1") {
		t.Fatalf("expected case-insensitive Vercel AI Gateway host match")
	}
	for _, nonMatch := range []string{
		"",
		"ai-gateway.vercel.sh/v1",
		"https://ai-gateway.vercel.sh.evil.com/v1",
		"http://localhost:11434/v1",
		"https://api.openai.com/v1",
	} {
		if isVercelGatewayURL(nonMatch) {
			t.Fatalf("unexpected match for non-Vercel gateway URL %q", nonMatch)
		}
	}

	t.Setenv("FARO_GATEWAY_CACHING", "")
	client, err := NewVercelGatewayClient("test-key", DefaultGatewayBaseURL)
	if err != nil {
		t.Fatalf("NewVercelGatewayClient: %v", err)
	}
	if !client.EnableGatewayCaching {
		t.Fatalf("expected EnableGatewayCaching=true for DefaultGatewayBaseURL")
	}

	t.Setenv("FARO_GATEWAY_CACHING", "off")
	clientOff, err := NewVercelGatewayClient("test-key", DefaultGatewayBaseURL)
	if err != nil {
		t.Fatalf("NewVercelGatewayClient (off): %v", err)
	}
	if clientOff.EnableGatewayCaching {
		t.Fatalf("expected FARO_GATEWAY_CACHING=off to disable caching")
	}

	t.Setenv("FARO_GATEWAY_CACHING", "on")
	clientOn, err := NewVercelGatewayClient("test-key", "https://proxy.internal/v1")
	if err != nil {
		t.Fatalf("NewVercelGatewayClient (on): %v", err)
	}
	if !clientOn.EnableGatewayCaching {
		t.Fatalf("expected FARO_GATEWAY_CACHING=on to enable caching on custom proxy URL")
	}
}

func TestOpenAICompatClientGatewayError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Invalid AI Gateway key","type":"authentication_error"}}`))
	}))
	defer srv.Close()

	client, err := NewVercelGatewayClient("bad-key", srv.URL+"/v1")
	if err != nil {
		t.Fatalf("NewVercelGatewayClient: %v", err)
	}

	_, err = client.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err == nil || !strings.Contains(err.Error(), "Invalid AI Gateway key") {
		t.Fatalf("expected gateway error message, got %v", err)
	}
}
