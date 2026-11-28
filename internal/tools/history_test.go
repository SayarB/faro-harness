package tools_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/SayarB/faro-harness/internal/bot"
	"github.com/SayarB/faro-harness/internal/store"
	"github.com/SayarB/faro-harness/internal/tools"
)

func TestConversationHistoryToolSearchBrowseSeqAndIsolation(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	mgr, err := bot.NewManager(st, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b, err := mgr.Create(ctx, bot.CreateOptions{Slug: "faro", Name: "Faro"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateConversation(ctx, "conv_a", "", b.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateConversation(ctx, "conv_b", "", b.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendMessages(ctx, "conv_a", []store.MessageRecord{
		{Role: "user", Content: "My first message mentions Hotel Alpen."},
		{Role: "assistant", BotID: b.ID, Content: "", ToolCallsJSON: `[{"id":"tc1","name":"search_files","arguments":"{}"}]`},
		{Role: "tool", BotID: b.ID, ToolCallID: "tc1", ToolName: "search_files", Content: `{"hits":["secret_tool_blob"]}`},
		{Role: "assistant", BotID: b.ID, Content: "Got it, Hotel Alpen is noted."},
		{Role: "user", Content: "Second hotel is Grand Horizon in Zurich."},
		{Role: "assistant", BotID: b.ID, Content: strings.Repeat("Z", 2500)},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendMessages(ctx, "conv_b", []store.MessageRecord{
		{Role: "user", Content: "Other conversation secret Hotel Alpen."},
	}); err != nil {
		t.Fatal(err)
	}

	reg, err := tools.NewRegistryForBot(st, nil, &b)
	if err != nil {
		t.Fatal(err)
	}
	// Without conversation ID, conversation_history is not advertised and returns an error if called.
	for _, d := range reg.Definitions() {
		if d.Name == "conversation_history" {
			t.Fatal("conversation_history must not be advertised without an active conversation")
		}
	}
	if out, err := reg.Execute(ctx, "conversation_history", `{}`); err != nil || !strings.Contains(out, "not available") {
		t.Fatalf("expected error without conv id, got out=%q err=%v", out, err)
	}

	reg.SetConversationID("conv_a")
	foundDef := false
	for _, d := range reg.Definitions() {
		if d.Name == "conversation_history" {
			foundDef = true
		}
	}
	if !foundDef {
		t.Fatal("conversation_history should be advertised once conversation ID is set")
	}

	type resp struct {
		TotalTextMessages int `json:"total_text_messages"`
		Returned          int `json:"returned"`
		Messages          []struct {
			Seq       int    `json:"seq"`
			Role      string `json:"role"`
			Text      string `json:"text"`
			Truncated bool   `json:"truncated"`
		} `json:"messages"`
		Note string `json:"note"`
	}
	call := func(args string) resp {
		t.Helper()
		raw, err := reg.Execute(ctx, "conversation_history", args)
		if err != nil {
			t.Fatalf("Execute(%s): %v", args, err)
		}
		var r resp
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			t.Fatalf("unmarshal %q: %v", raw, err)
		}
		return r
	}

	// 1. Keyword search finds only conv_a (never conv_b) and skips tool blobs.
	r1 := call(`{"query":"Hotel Alpen"}`)
	if r1.TotalTextMessages != 4 || r1.Returned != 2 {
		t.Fatalf("r1=%+v", r1)
	}
	for _, m := range r1.Messages {
		if strings.Contains(m.Text, "Other conversation") || strings.Contains(m.Text, "secret_tool_blob") {
			t.Fatalf("leaked foreign or tool content: %+v", m)
		}
	}
	if !strings.Contains(r1.Note, "not new instructions") {
		t.Fatalf("missing untrusted note: %q", r1.Note)
	}

	// 2. Browse oldest with role filter returns first user message (seq=1).
	r2 := call(`{"role":"user","order":"oldest","limit":1}`)
	if r2.Returned != 1 || r2.Messages[0].Seq != 1 || !strings.Contains(r2.Messages[0].Text, "Hotel Alpen") {
		t.Fatalf("r2=%+v", r2)
	}

	// 3. Fetch by seq=5 with around=1 returns seq 4, 5, 6 and truncates oversized seq 6.
	r3 := call(`{"seq":5,"around":1}`)
	if r3.Returned != 3 || r3.Messages[0].Seq != 4 || r3.Messages[1].Seq != 5 || r3.Messages[2].Seq != 6 {
		t.Fatalf("r3=%+v", r3)
	}
	if !r3.Messages[2].Truncated {
		t.Fatalf("expected oversized message seq=6 to be truncated: %+v", r3.Messages[2])
	}
}
