package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SayarB/faro-harness/internal/bot"
	"github.com/SayarB/faro-harness/internal/llm"
	"github.com/SayarB/faro-harness/internal/store"
	"github.com/SayarB/faro-harness/internal/tools"
	"github.com/SayarB/faro-harness/internal/worker"
)

type fakeLLMClient struct {
	requests  []llm.ChatRequest
	responses []llm.ChatResponse
	errors    map[int]error
	idx       int
}

func (f *fakeLLMClient) Chat(_ context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
	msgsCopy := append([]llm.Message(nil), req.Messages...)
	req.Messages = msgsCopy
	f.requests = append(f.requests, req)
	cur := f.idx
	f.idx++
	if f.errors != nil {
		if err, ok := f.errors[cur]; ok && err != nil {
			return llm.ChatResponse{}, err
		}
	}
	return f.responses[cur], nil
}

func TestSessionToolLoopAndCacheStableSystemPrompt(t *testing.T) {
	root := t.TempDir()
	invPath := filepath.Join(root, "Clients", "Acme", "Invoice_2026.md")
	if err := os.MkdirAll(filepath.Dir(invPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(invPath, []byte("# Invoice 2026\n\nAmount: $4,500\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	ctx := context.Background()
	cat, err := worker.NewCatalogIndexer(ctx, st, root)
	if err != nil {
		t.Fatalf("NewCatalogIndexer: %v", err)
	}
	pipe := worker.NewPipeline(cat, worker.PipelineConfig{})
	defer pipe.Close()
	if _, err := pipe.IndexWorkspace(ctx); err != nil {
		t.Fatalf("IndexWorkspace: %v", err)
	}

	reg, err := tools.NewRegistry(st, []store.Workspace{cat.Workspace})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	fake := &fakeLLMClient{
		responses: []llm.ChatResponse{
			{
				Message: llm.Message{
					Role: llm.RoleAssistant,
					ToolCalls: []llm.ToolCall{{
						ID:        "tc_1",
						Name:      "search_files",
						Arguments: `{"query":"Acme invoice 2026"}`,
					}},
				},
				Usage: llm.Usage{PromptTokens: 200, CompletionTokens: 20, TotalTokens: 220, CacheWriteTokens: 190},
			},
			{
				Message: llm.Message{
					Role: llm.RoleAssistant,
					ToolCalls: []llm.ToolCall{{
						ID:        "tc_2",
						Name:      "read_file",
						Arguments: `{"rel_path":"Clients/Acme/Invoice_2026.md"}`,
					}},
				},
				Usage: llm.Usage{PromptTokens: 320, CompletionTokens: 18, TotalTokens: 338, CachedTokens: 180, CacheWriteTokens: 140},
			},
			{
				Message: llm.Message{
					Role:    llm.RoleAssistant,
					Content: "The 2026 invoice from Acme is $4,500 (see Clients/Acme/Invoice_2026.md).",
				},
				Usage: llm.Usage{PromptTokens: 410, CompletionTokens: 25, TotalTokens: 435, CachedTokens: 300},
			},
			{
				Message: llm.Message{
					Role:    llm.RoleAssistant,
					Content: "You're welcome!",
				},
				Usage: llm.Usage{PromptTokens: 450, CompletionTokens: 5, TotalTokens: 455, CachedTokens: 400},
			},
		},
	}

	var events []Event
	fixedNow := time.Date(2026, 11, 22, 11, 45, 30, 0, time.UTC)
	sess, err := NewSession(Config{
		Client:     fake,
		Tools:      reg,
		Workspaces: []store.Workspace{cat.Workspace},
		Model:      "anthropic/claude-sonnet-4.5",
		Now:        func() time.Time { return fixedNow },
		OnEvent:    func(ev Event) { events = append(events, ev) },
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	// Verify Layer 1 is static prefix and Layer 2 uses date-only (no HH:MM:SS).
	if !strings.HasPrefix(sess.SystemPrompt(), BaseSystemPrompt) {
		t.Fatalf("system prompt must begin with static BaseSystemPrompt")
	}
	if !strings.Contains(sess.SystemPrompt(), "2026-11-22 (Sunday)") || strings.Contains(sess.SystemPrompt(), "11:45") {
		t.Fatalf("system prompt should contain date-only anchor, got:\n%s", sess.SystemPrompt())
	}

	res1, err := sess.Turn(ctx, "Get me the 2026 invoice from Acme")
	if err != nil {
		t.Fatalf("Turn 1: %v", err)
	}
	if res1.Steps != 3 || res1.ToolCalls != 2 {
		t.Fatalf("unexpected turn 1 stats: %+v", res1)
	}
	if !strings.Contains(res1.Reply, "$4,500") {
		t.Fatalf("unexpected reply: %q", res1.Reply)
	}
	if res1.Usage.CachedTokens != 480 || res1.Usage.CacheWriteTokens != 330 {
		t.Fatalf("unexpected cached token sum: %+v", res1.Usage)
	}
	if fake.requests[0].SessionID == "" || fake.requests[0].SessionID != fake.requests[1].SessionID || fake.requests[1].SessionID != fake.requests[2].SessionID {
		t.Fatalf("expected stable non-empty SessionID across steps, got %q, %q, %q", fake.requests[0].SessionID, fake.requests[1].SessionID, fake.requests[2].SessionID)
	}

	// Verify tool results actually flowed back to the model.
	toolMsg := fake.requests[2].Messages[len(fake.requests[2].Messages)-1]
	if toolMsg.Role != llm.RoleTool || !strings.Contains(toolMsg.Content, "$4,500") {
		t.Fatalf("expected read_file tool output to contain $4,500, got %+v", toolMsg)
	}

	// Turn 2 must keep the exact byte-identical system prompt prefix.
	res2, err := sess.Turn(ctx, "Thanks!")
	if err != nil {
		t.Fatalf("Turn 2: %v", err)
	}
	if res2.Reply != "You're welcome!" {
		t.Fatalf("unexpected turn 2 reply: %q", res2.Reply)
	}
	if fake.requests[0].Messages[0].Content != fake.requests[3].Messages[0].Content {
		t.Fatalf("system prompt mutated between turns")
	}
	if fake.requests[0].SessionID != fake.requests[3].SessionID {
		t.Fatalf("SessionID changed between turns: %q vs %q", fake.requests[0].SessionID, fake.requests[3].SessionID)
	}
	// Reset without a store must rotate to a fresh sess_ ID.
	oldSessID := fake.requests[0].SessionID
	if err := sess.Reset(); err != nil {
		t.Fatalf("Reset (store-less): %v", err)
	}
	fake.responses = append(fake.responses, llm.ChatResponse{
		Message: llm.Message{Role: llm.RoleAssistant, Content: "Fresh session"},
	})
	if _, err := sess.Turn(ctx, "New chat"); err != nil {
		t.Fatalf("Turn after reset: %v", err)
	}
	newSessID := fake.requests[4].SessionID
	if !strings.HasPrefix(newSessID, "sess_") || newSessID == oldSessID {
		t.Fatalf("expected fresh sess_ ID after store-less Reset, old=%q new=%q", oldSessID, newSessID)
	}
	if len(events) == 0 {
		t.Fatalf("expected lifecycle events to be emitted")
	}
}

func TestTwoBotsIsolatedMemoryScratchpadAndPersistedConversations(t *testing.T) {
	root := t.TempDir()
	invPath := filepath.Join(root, "Invoices", "Acme_2026.md")
	if err := os.MkdirAll(filepath.Dir(invPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(invPath, []byte("# Acme 2026\nTotal: $4,500\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	ctx := context.Background()
	cat, err := worker.NewCatalogIndexer(ctx, st, root)
	if err != nil {
		t.Fatalf("NewCatalogIndexer: %v", err)
	}
	pipe := worker.NewPipeline(cat, worker.PipelineConfig{})
	defer pipe.Close()
	if _, err := pipe.IndexWorkspace(ctx); err != nil {
		t.Fatalf("IndexWorkspace: %v", err)
	}

	mgr, err := bot.NewManager(st, t.TempDir())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	ledger, err := mgr.Create(ctx, bot.CreateOptions{
		Slug:         "ledger",
		Name:         "Ledger",
		Instructions: "You track invoices and cashflow.",
	})
	if err != nil {
		t.Fatalf("create ledger: %v", err)
	}
	counsel, err := mgr.Create(ctx, bot.CreateOptions{
		Slug:         "counsel",
		Name:         "Counsel",
		Instructions: "You review legal contracts.",
	})
	if err != nil {
		t.Fatalf("create counsel: %v", err)
	}

	regLedger, err := tools.NewRegistryForBot(st, []store.Workspace{cat.Workspace}, &ledger)
	if err != nil {
		t.Fatalf("NewRegistryForBot ledger: %v", err)
	}
	regCounsel, err := tools.NewRegistryForBot(st, []store.Workspace{cat.Workspace}, &counsel)
	if err != nil {
		t.Fatalf("NewRegistryForBot counsel: %v", err)
	}

	// Session 1 for Ledger: writes to its own memory.md and scratch/acme_report.md.
	fakeLedger := &fakeLLMClient{
		responses: []llm.ChatResponse{
			{
				Message: llm.Message{
					Role: llm.RoleAssistant,
					ToolCalls: []llm.ToolCall{
						{
							ID:        "tc_mem",
							Name:      "bot_write",
							Arguments: `{"path":"memory.md","content":"- Acme 2026 invoice is $4,500 (Invoices/Acme_2026.md)","mode":"append"}`,
						},
						{
							ID:        "tc_scratch",
							Name:      "bot_write",
							Arguments: `{"path":"scratch/acme_report.md","content":"# Acme Report\nTotal: $4,500"}`,
						},
					},
				},
			},
			{
				Message: llm.Message{
					Role:    llm.RoleAssistant,
					Content: "Saved to my memory and drafted scratch/acme_report.md.",
				},
			},
		},
	}

	sessLedger1, err := NewSession(Config{
		Client:     fakeLedger,
		Tools:      regLedger,
		Workspaces: []store.Workspace{cat.Workspace},
		Bot:        &ledger,
		Store:      st,
	})
	if err != nil {
		t.Fatalf("NewSession ledger 1: %v", err)
	}
	if _, err := sessLedger1.Turn(ctx, "Note Acme's invoice and draft a report in your scratchpad"); err != nil {
		t.Fatalf("ledger turn 1: %v", err)
	}
	ledgerConvID := sessLedger1.ConversationID()

	// New session for Ledger reloads its updated memory.md and scratch/ listing in Layer 1b.
	fakeLedger2 := &fakeLLMClient{
		responses: []llm.ChatResponse{
			{Message: llm.Message{Role: llm.RoleAssistant, Content: "Yes, $4,500."}},
		},
	}
	sessLedger2, err := NewSession(Config{
		Client:         fakeLedger2,
		Tools:          regLedger,
		Workspaces:     []store.Workspace{cat.Workspace},
		Bot:            &ledger,
		Store:          st,
		ConversationID: ledgerConvID,
	})
	if err != nil {
		t.Fatalf("NewSession ledger 2 (resume): %v", err)
	}
	if !strings.Contains(sessLedger2.SystemPrompt(), "Acme 2026 invoice is $4,500") {
		t.Fatalf("ledger session 2 did not load updated memory.md:\n%s", sessLedger2.SystemPrompt())
	}
	if !strings.Contains(sessLedger2.SystemPrompt(), "scratch/acme_report.md") {
		t.Fatalf("ledger session 2 did not list scratch/acme_report.md:\n%s", sessLedger2.SystemPrompt())
	}
	if _, err := sessLedger2.Turn(ctx, "Remind me of the total?"); err != nil {
		t.Fatalf("ledger resumed turn: %v", err)
	}
	// Resumed request must include previous conversation messages from SQLite (system + 5 prior + 1 new = 7)
	// and reuse the resumed ConversationID as SessionID.
	if got := len(fakeLedger2.requests[0].Messages); got != 7 {
		t.Fatalf("expected 7 messages in resumed ledger conversation, got %d", got)
	}
	if fakeLedger2.requests[0].SessionID != ledgerConvID {
		t.Fatalf("expected resumed SessionID=%q, got %q", ledgerConvID, fakeLedger2.requests[0].SessionID)
	}
	// Reset with a store creates a new conversation ID and updates SessionID to match.
	if err := sessLedger2.Reset(); err != nil {
		t.Fatalf("sessLedger2.Reset: %v", err)
	}
	resetConvID := sessLedger2.ConversationID()
	if resetConvID == "" || resetConvID == ledgerConvID {
		t.Fatalf("expected new conv ID after Reset, old=%q new=%q", ledgerConvID, resetConvID)
	}
	fakeLedger2.responses = append(fakeLedger2.responses, llm.ChatResponse{
		Message: llm.Message{Role: llm.RoleAssistant, Content: "Reset ok."},
	})
	if _, err := sessLedger2.Turn(ctx, "Hello after reset"); err != nil {
		t.Fatalf("ledger turn after reset: %v", err)
	}
	if fakeLedger2.requests[1].SessionID != resetConvID {
		t.Fatalf("expected SessionID=%q after Reset, got %q", resetConvID, fakeLedger2.requests[1].SessionID)
	}

	// Counsel cannot resume Ledger's conversation.
	if _, err := NewSession(Config{
		Client:         fakeLedger2,
		Tools:          regCounsel,
		Workspaces:     []store.Workspace{cat.Workspace},
		Bot:            &counsel,
		Store:          st,
		ConversationID: ledgerConvID,
	}); err == nil {
		t.Fatalf("expected error when Counsel tries to resume Ledger's conversation")
	}

	// Finding G: A bot-less session also cannot resume Ledger's conversation.
	regPlain, err := tools.NewRegistry(st, []store.Workspace{cat.Workspace})
	if err != nil {
		t.Fatalf("NewRegistry plain: %v", err)
	}
	if _, err := NewSession(Config{
		Client:         fakeLedger2,
		Tools:          regPlain,
		Workspaces:     []store.Workspace{cat.Workspace},
		Store:          st,
		ConversationID: ledgerConvID,
	}); err == nil {
		t.Fatalf("expected error when bot-less session tries to resume Ledger's conversation")
	}

	// Finding G reverse: A bot session cannot resume a bot-less conversation.
	sessPlain, err := NewSession(Config{
		Client:     fakeLedger2,
		Tools:      regPlain,
		Workspaces: []store.Workspace{cat.Workspace},
		Store:      st,
	})
	if err != nil {
		t.Fatalf("NewSession plain: %v", err)
	}
	if _, err := NewSession(Config{
		Client:         fakeLedger2,
		Tools:          regCounsel,
		Workspaces:     []store.Workspace{cat.Workspace},
		Bot:            &counsel,
		Store:          st,
		ConversationID: sessPlain.ConversationID(),
	}); err == nil {
		t.Fatalf("expected error when bot session tries to resume a bot-less conversation")
	}

	// Finding H: Mid-turn LLM failure after a completed tool batch preserves the tool result in SQLite.
	fakeFail := &fakeLLMClient{
		responses: []llm.ChatResponse{
			{
				Message: llm.Message{
					Role: llm.RoleAssistant,
					ToolCalls: []llm.ToolCall{{
						ID:        "tc_partial",
						Name:      "bot_write",
						Arguments: `{"path":"scratch/partial.txt","content":"persisted side effect"}`,
					}},
				},
			},
			{}, // step 2 fails via errors[1]
		},
		errors: map[int]error{
			1: errors.New("gateway 502"),
		},
	}
	sessPartial, err := NewSession(Config{
		Client:     fakeFail,
		Tools:      regCounsel,
		Workspaces: []store.Workspace{cat.Workspace},
		Bot:        &counsel,
		Store:      st,
	})
	if err != nil {
		t.Fatalf("NewSession partial: %v", err)
	}
	if _, err := sessPartial.Turn(ctx, "Write partial.txt"); err == nil {
		t.Fatalf("expected step 2 gateway error")
	}
	persisted, err := st.ListMessages(ctx, sessPartial.ConversationID())
	if err != nil {
		t.Fatalf("ListMessages partial: %v", err)
	}
	// Expect: user, assistant(tool_call), tool(result), assistant([Turn interrupted...]) = 4 rows.
	if len(persisted) != 4 || persisted[2].Role != string(llm.RoleTool) || !strings.Contains(persisted[2].Content, "scratch/partial.txt") {
		t.Fatalf("expected completed tool side effect + interruption note persisted, got %+v", persisted)
	}
	if persisted[3].Role != string(llm.RoleAssistant) || !strings.Contains(persisted[3].Content, "Turn interrupted") {
		t.Fatalf("expected closing interruption assistant message, got %+v", persisted[3])
	}

	// Counsel's own session sees ONLY Counsel's identity/memory and ZERO trace of Ledger's memory or scratchpad.
	sessCounsel, err := NewSession(Config{
		Client:     fakeLedger2,
		Tools:      regCounsel,
		Workspaces: []store.Workspace{cat.Workspace},
		Bot:        &counsel,
		Store:      st,
	})
	if err != nil {
		t.Fatalf("NewSession counsel: %v", err)
	}
	sp := sessCounsel.SystemPrompt()
	if !strings.Contains(sp, "You review legal contracts.") {
		t.Fatalf("counsel system prompt missing its own bot.md:\n%s", sp)
	}
	if strings.Contains(sp, "Acme 2026 invoice") || strings.Contains(sp, "acme_report.md") || strings.Contains(sp, "invoices and cashflow") {
		t.Fatalf("counsel system prompt leaked ledger context:\n%s", sp)
	}
}

type deltaLLMClient struct{ chunks []string }

func (d *deltaLLMClient) Chat(_ context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
	if req.OnDelta != nil {
		for _, c := range d.chunks {
			req.OnDelta(c)
		}
	}
	return llm.ChatResponse{Message: llm.Message{Role: llm.RoleAssistant, Content: strings.Join(d.chunks, "")}}, nil
}

func TestSessionStreamTokensIsOptIn(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ws, err := st.EnsureWorkspace(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	reg, err := tools.NewRegistry(st, []store.Workspace{ws})
	if err != nil {
		t.Fatal(err)
	}

	run := func(stream bool) []Event {
		var evs []Event
		sess, err := NewSession(Config{
			Client:       &deltaLLMClient{chunks: []string{"Hel", "lo"}},
			Tools:        reg,
			Workspaces:   []store.Workspace{ws},
			StreamTokens: stream,
			OnEvent:      func(ev Event) { evs = append(evs, ev) },
		})
		if err != nil {
			t.Fatal(err)
		}
		res, err := sess.Turn(context.Background(), "hi")
		if err != nil || res.Reply != "Hello" {
			t.Fatalf("reply=%q err=%v", res.Reply, err)
		}
		return evs
	}

	var tokens []string
	for _, ev := range run(true) {
		if ev.Kind == EventToken {
			tokens = append(tokens, ev.Text)
		}
	}
	if strings.Join(tokens, "|") != "Hel|lo" {
		t.Fatalf("tokens=%v", tokens)
	}
	for _, ev := range run(false) {
		if ev.Kind == EventToken {
			t.Fatalf("token event emitted without StreamTokens")
		}
	}
}
