package desktop

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SayarB/faro-harness/internal/bot"
	"github.com/SayarB/faro-harness/internal/llm"
	"github.com/SayarB/faro-harness/internal/store"
)

type fakeClient struct {
	mu      sync.Mutex
	calls   int
	block   chan struct{} // when non-nil, Chat waits for ctx cancel
	scripts []llm.ChatResponse
}

func (f *fakeClient) Chat(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
	f.mu.Lock()
	i := f.calls
	f.calls++
	f.mu.Unlock()
	if f.block != nil {
		close(f.block)
		<-ctx.Done()
		return llm.ChatResponse{}, ctx.Err()
	}
	resp := f.scripts[i]
	if req.OnDelta != nil && resp.Message.Content != "" {
		for _, p := range strings.SplitAfter(resp.Message.Content, " ") {
			req.OnDelta(p)
		}
	}
	return resp, nil
}

type harness struct {
	c      *Controller
	events chan Event
	dir    string
}

func newHarness(t *testing.T, client llm.Client) *harness {
	t.Helper()
	t.Setenv("AI_GATEWAY_API_KEY", "")
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	mgr, err := bot.NewManager(st, filepath.Join(dir, "bots"))
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan Event, 256)
	c, err := New(Options{
		Store: st, Bots: mgr, DataDir: dir,
		Emit:      func(e Event) { events <- e },
		NewClient: func(string) (llm.Client, error) { return client, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Shutdown)
	return &harness{c: c, events: events, dir: dir}
}

func (h *harness) waitFor(t *testing.T, match func(Event) bool) []Event {
	t.Helper()
	var seen []Event
	deadline := time.After(5 * time.Second)
	for {
		select {
		case e := <-h.events:
			seen = append(seen, e)
			if match(e) {
				return seen
			}
		case <-deadline:
			t.Fatalf("timed out; saw %d events: %+v", len(seen), seen)
		}
	}
}

func isTurnEnd(e Event) bool {
	return e.Type == EventTurnDone || e.Type == EventTurnError || e.Type == EventTurnCanceled
}

func (h *harness) addFolder(t *testing.T) WorkspaceInfo {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "notes.md"), []byte("# Notes\nhello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ws, err := h.c.AddWorkspace(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	h.waitFor(t, func(e Event) bool { return e.Type == EventIndex && e.Done })
	return ws
}

func TestAPIKeySavedAndReportedInState(t *testing.T) {
	h := newHarness(t, &fakeClient{})
	st, _ := h.c.State(context.Background())
	if st.HasKey {
		t.Fatal("unexpected key")
	}
	if err := h.c.SaveAPIKey("  "); err == nil {
		t.Fatal("empty key must be rejected")
	}
	if err := h.c.SaveAPIKey("sk-test"); err != nil {
		t.Fatal(err)
	}
	st, _ = h.c.State(context.Background())
	if !st.HasKey {
		t.Fatal("key not reported")
	}
	if fi, err := os.Stat(settingsPath(h.dir)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("settings file perms: %v %v", fi, err)
	}
}

func TestAddWorkspaceIndexesAndReports(t *testing.T) {
	h := newHarness(t, &fakeClient{})
	ws := h.addFolder(t)
	if _, err := h.c.AddWorkspace(context.Background(), filepath.Join(h.dir, "catalog.db")); err == nil {
		t.Fatal("a file must be rejected as a folder")
	}
	st, _ := h.c.State(context.Background())
	if len(st.Workspaces) != 1 || st.Workspaces[0].ID != ws.ID || st.Workspaces[0].FileCount != 1 {
		t.Fatalf("state=%+v", st)
	}
}

func TestCreateBotDerivesUniqueSlugs(t *testing.T) {
	h := newHarness(t, &fakeClient{})
	a, err := h.c.CreateBot(context.Background(), "Ledger Bot!", "Track invoices.")
	if err != nil || a.Slug != "ledger-bot" || a.Name != "Ledger Bot!" {
		t.Fatalf("a=%+v err=%v", a, err)
	}
	b, err := h.c.CreateBot(context.Background(), "ledger bot", "")
	if err != nil || b.Slug != "ledger-bot-2" {
		t.Fatalf("b=%+v err=%v", b, err)
	}
	if _, err := h.c.CreateBot(context.Background(), "!!!", ""); err == nil {
		t.Fatal("name without letters must fail")
	}
	st, _ := h.c.State(context.Background())
	if len(st.Bots) != 2 {
		t.Fatalf("bots=%+v", st.Bots)
	}
}

func TestChatRequiresKeyAndFolder(t *testing.T) {
	h := newHarness(t, &fakeClient{})
	if _, err := h.c.NewChat(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "API key") {
		t.Fatalf("err=%v", err)
	}
	_ = h.c.SaveAPIKey("k")
	if _, err := h.c.NewChat(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "folder") {
		t.Fatalf("err=%v", err)
	}
	// A bot alone is enough to chat.
	b, _ := h.c.CreateBot(context.Background(), "Solo", "")
	if _, err := h.c.NewChat(context.Background(), b.Slug); err != nil {
		t.Fatalf("bot-only chat: %v", err)
	}
}

func TestSendStreamsTokensAndToolEventsThenDone(t *testing.T) {
	fc := &fakeClient{scripts: []llm.ChatResponse{
		{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "tc1", Name: "list_folder", Arguments: `{}`}}}},
		{Message: llm.Message{Role: llm.RoleAssistant, Content: "You have one note."}},
	}}
	h := newHarness(t, fc)
	_ = h.c.SaveAPIKey("k")
	h.addFolder(t)
	if _, err := h.c.NewChat(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	id, err := h.c.Send(context.Background(), "what's in my folder?")
	if err != nil {
		t.Fatal(err)
	}
	evs := h.waitFor(t, isTurnEnd)

	var types []string
	var text strings.Builder
	for _, e := range evs {
		if e.TurnID != "" && e.TurnID != id {
			t.Fatalf("event with wrong turn id: %+v", e)
		}
		types = append(types, e.Type)
		if e.Type == EventToken {
			text.WriteString(e.Text)
		}
	}
	got := strings.Join(types, ",")
	if !strings.Contains(got, "tool_call,tool_result") || !strings.HasSuffix(got, "turn.done") {
		t.Fatalf("types=%s", got)
	}
	if text.String() != "You have one note." {
		t.Fatalf("streamed text=%q", text.String())
	}
	last := evs[len(evs)-1]
	if last.Reply != "You have one note." {
		t.Fatalf("done=%+v", last)
	}
	var call, result Event
	for _, e := range evs {
		if e.Type == EventToolCall {
			call = e
		}
		if e.Type == EventToolResult {
			result = e
		}
	}
	if call.ToolCallID != "tc1" || result.ToolCallID != "tc1" || result.ToolName != "list_folder" {
		t.Fatalf("call=%+v result=%+v", call, result)
	}
}

func TestSendRejectsConcurrentTurnAndCancelStopsIt(t *testing.T) {
	fc := &fakeClient{block: make(chan struct{})}
	h := newHarness(t, fc)
	_ = h.c.SaveAPIKey("k")
	h.addFolder(t)
	if _, err := h.c.NewChat(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.c.Send(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	<-fc.block // Chat is in flight
	if _, err := h.c.Send(context.Background(), "again"); err == nil {
		t.Fatal("second concurrent turn must be rejected")
	}
	if _, err := h.c.NewChat(context.Background(), ""); err == nil {
		t.Fatal("NewChat during a turn must be rejected")
	}
	h.c.Cancel()
	evs := h.waitFor(t, isTurnEnd)
	if evs[len(evs)-1].Type != EventTurnCanceled {
		t.Fatalf("last=%+v", evs[len(evs)-1])
	}
}

func TestFolderAddedMidChatIsMountedOnNextTurn(t *testing.T) {
	fc := &fakeClient{scripts: []llm.ChatResponse{
		{Message: llm.Message{Role: llm.RoleAssistant, Content: "one"}},
		{Message: llm.Message{Role: llm.RoleAssistant, Content: "two"}},
	}}
	h := newHarness(t, fc)
	_ = h.c.SaveAPIKey("k")
	h.addFolder(t)
	chat, _ := h.c.NewChat(context.Background(), "")
	_, _ = h.c.Send(context.Background(), "q1")
	h.waitFor(t, isTurnEnd)
	h.addFolder(t)
	if _, err := h.c.Send(context.Background(), "q2"); err != nil {
		t.Fatal(err)
	}
	h.waitFor(t, isTurnEnd)
	if h.c.sess.ConversationID() != chat.ConversationID {
		t.Fatal("rebuilding the session must keep the same conversation")
	}
	if n := strings.Count(h.c.sess.SystemPrompt(), "- id="); n != 2 {
		t.Fatalf("expected 2 mounted folders, got %d", n)
	}
}

func TestTruncateIsRuneSafe(t *testing.T) {
	s := strings.Repeat("é", 10)
	out := truncate(s, 5)
	if !strings.HasSuffix(out, "…") || strings.ContainsRune(out, '�') {
		t.Fatalf("out=%q", out)
	}
}
