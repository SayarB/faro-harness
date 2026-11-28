package desktop

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SayarB/faro-harness/internal/llm"
	"github.com/SayarB/faro-harness/internal/store"
)

// scriptedClient lets tests pause before a specific Chat call and release it deterministically.
type scriptedClient struct {
	mu      sync.Mutex
	calls   int
	scripts []func(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error)
}

func (s *scriptedClient) Chat(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
	s.mu.Lock()
	i := s.calls
	s.calls++
	s.mu.Unlock()
	return s.scripts[i](ctx, req)
}

func drainUntil(t *testing.T, ch <-chan Event, match func(Event) bool) []Event {
	t.Helper()
	var out []Event
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("subscriber channel closed early after %d events: %+v", len(out), out)
			}
			out = append(out, ev)
			if match(ev) {
				return out
			}
		case <-deadline:
			t.Fatalf("timed out waiting on subscriber; saw %d events: %+v", len(out), out)
		}
	}
}

func TestTwoSubscribersSeeIdenticalOrderedEventsAndTurnStartedBeforeSendReturns(t *testing.T) {
	gate := make(chan struct{})
	sc := &scriptedClient{scripts: []func(context.Context, llm.ChatRequest) (llm.ChatResponse, error){
		func(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
			<-gate
			if req.OnDelta != nil {
				req.OnDelta("Hel")
				req.OnDelta("lo")
			}
			return llm.ChatResponse{Message: llm.Message{Role: llm.RoleAssistant, Content: "Hello"}}, nil
		},
	}}
	h := newHarness(t, sc)
	ctx := context.Background()
	_ = h.c.SaveAPIKey("k")
	chat, err := h.c.BotChat(ctx, DefaultBotSlug)
	if err != nil {
		t.Fatal(err)
	}

	snap1, ch1, unsub1, err := h.c.Attach(ctx, chat.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	defer unsub1()
	snap2, ch2, unsub2, err := h.c.Attach(ctx, chat.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	defer unsub2()
	if snap1.Busy || snap2.Busy || len(snap1.Events) != 0 || len(snap2.Events) != 0 {
		t.Fatalf("unexpected initial snapshots: %+v / %+v", snap1, snap2)
	}

	turnID, err := h.c.Send(ctx, chat.ConversationID, "say hello")
	if err != nil {
		t.Fatal(err)
	}

	// Both subscribers AND the Wails UI callback must already have turn.started synchronously
	// before Send returned (non-blocking channel receive must succeed immediately).
	checkImmediateStart := func(name string, ch <-chan Event) Event {
		t.Helper()
		select {
		case ev := <-ch:
			if ev.Type != EventTurnStarted || ev.TurnID != turnID || ev.ConversationID != chat.ConversationID || ev.Text != "say hello" {
				t.Fatalf("%s got unexpected first event: %+v", name, ev)
			}
			return ev
		default:
			t.Fatalf("%s did not receive turn.started before Send returned", name)
			return Event{}
		}
	}
	s1Start := checkImmediateStart("sub1", ch1)
	s2Start := checkImmediateStart("sub2", ch2)
	uiStart := checkImmediateStart("ui", h.events)
	if s1Start.Seq != s2Start.Seq || s1Start.Seq != uiStart.Seq || s1Start.Seq <= snap1.LastSeq {
		t.Fatalf("seq mismatch: sub1=%d sub2=%d ui=%d lastSeq=%d", s1Start.Seq, s2Start.Seq, uiStart.Seq, snap1.LastSeq)
	}

	close(gate)
	evs1 := append([]Event{s1Start}, drainUntil(t, ch1, isTurnEnd)...)
	evs2 := append([]Event{s2Start}, drainUntil(t, ch2, isTurnEnd)...)
	evsUI := append([]Event{uiStart}, h.waitFor(t, isTurnEnd)...)

	if !reflect.DeepEqual(evs1, evs2) {
		t.Fatalf("sub1 != sub2:\n1=%+v\n2=%+v", evs1, evs2)
	}
	if !reflect.DeepEqual(evs1, evsUI) {
		t.Fatalf("sub1 != ui:\n1=%+v\nui=%+v", evs1, evsUI)
	}
	for i := 1; i < len(evs1); i++ {
		if evs1[i].Seq <= evs1[i-1].Seq {
			t.Fatalf("non-monotonic seq at %d: %+v", i, evs1)
		}
	}
}

func TestAttachMidTurnAfterPersistedToolBatchHasNoDuplicates(t *testing.T) {
	step2Entered := make(chan struct{})
	releaseStep2 := make(chan struct{})

	sc := &scriptedClient{scripts: []func(context.Context, llm.ChatRequest) (llm.ChatResponse, error){
		// Turn 1: completed prior history.
		func(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
			return say("First answer."), nil
		},
		// Turn 2 step 1: tool call (persisted to SQLite right after tool finishes!).
		func(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
			return toolCall("tc1", "list_folder", `{}`), nil
		},
		// Turn 2 step 2: pause here while step 1's rows are already in SQLite.
		func(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
			close(step2Entered)
			<-releaseStep2
			if req.OnDelta != nil {
				req.OnDelta("Found ")
				req.OnDelta("notes.")
			}
			return say("Found notes."), nil
		},
	}}

	h := newHarness(t, sc)
	ctx := context.Background()
	_ = h.c.SaveAPIKey("k")
	h.addFolder(t)
	chat, err := h.c.BotChat(ctx, DefaultBotSlug)
	if err != nil {
		t.Fatal(err)
	}

	// Complete Turn 1 so DB has persisted prior history.
	if _, err := h.c.Send(ctx, chat.ConversationID, "turn one"); err != nil {
		t.Fatal(err)
	}
	h.waitFor(t, isTurnEnd)

	// Start Turn 2; wait until step 1 tool batch has been persisted and step 2 begins.
	turn2ID, err := h.c.Send(ctx, chat.ConversationID, "turn two")
	if err != nil {
		t.Fatal(err)
	}
	<-step2Entered

	// Verify DB already contains Turn 2's persisted tool batch rows (seq > startMsgSeq).
	recs, err := h.c.st.ListMessages(ctx, chat.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) <= 2 {
		t.Fatalf("expected Turn 2 tool batch persisted in DB, got %d rows", len(recs))
	}

	// Attach mid-turn: startMsgSeq must cut Turn 2's DB rows so they only appear once (from log).
	snap, ch, unsub, err := h.c.Attach(ctx, chat.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()

	if !snap.Busy || snap.ConversationID != chat.ConversationID || snap.BotSlug != DefaultBotSlug {
		t.Fatalf("snap header=%+v", snap)
	}
	var startedTexts []string
	toolCalls := 0
	toolResults := 0
	for _, e := range snap.Events {
		switch e.Type {
		case EventTurnStarted:
			startedTexts = append(startedTexts, e.Text)
		case EventToolCall:
			toolCalls++
		case EventToolResult:
			toolResults++
		}
	}
	if strings.Join(startedTexts, "|") != "turn one|turn two" {
		t.Fatalf("startedTexts=%v events=%+v", startedTexts, snap.Events)
	}
	if toolCalls != 1 || toolResults != 1 {
		t.Fatalf("expected exactly 1 tool_call and 1 tool_result (no duplicates), got %d/%d", toolCalls, toolResults)
	}

	// Release step 2 and verify live events have Seq > LastSeq and complete Turn 2 cleanly.
	close(releaseStep2)
	live := drainUntil(t, ch, isTurnEnd)
	for _, e := range live {
		if e.Seq <= snap.LastSeq {
			t.Fatalf("live event seq %d <= snapshot lastSeq %d: %+v", e.Seq, snap.LastSeq, e)
		}
		if e.TurnID != turn2ID {
			t.Fatalf("unexpected turn id in live stream: %+v", e)
		}
	}
	if live[len(live)-1].Type != EventTurnDone || live[len(live)-1].Reply != "Found notes." {
		t.Fatalf("last live=%+v", live[len(live)-1])
	}
}

func TestSnapshotBetweenFinalPersistAndTurnDoneHasNoDuplicates(t *testing.T) {
	// Simulate the exact window after agent.Turn persists the final reply to SQLite while
	// hub.running is still active (before finishTurn emits turn.done and clears running).
	h := newHarness(t, &fakeClient{})
	ctx := context.Background()
	_ = h.c.SaveAPIKey("k")
	chat, err := h.c.BotChat(ctx, DefaultBotSlug)
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.c.bots.Get(ctx, DefaultBotSlug)
	if err != nil || b == nil {
		t.Fatalf("bot=%v err=%v", b, err)
	}

	// Start turn on hub at startMsgSeq=0, stream two tokens (merged into one in running.log).
	h.c.hub.startTurn(chat.ConversationID, "turn_1", 0, "hi")
	h.c.hub.emitForChat(chat.ConversationID, Event{Type: EventToken, Text: "Hel"})
	h.c.hub.emitForChat(chat.ConversationID, Event{Type: EventToken, Text: "lo"})

	// Persist final user + assistant rows to SQLite BEFORE finishTurn runs.
	if err := h.c.st.AppendMessages(ctx, chat.ConversationID, []store.MessageRecord{
		{Role: "user", Content: "hi"},
		{Role: "assistant", BotID: b.ID, Content: "Hello"},
	}); err != nil {
		t.Fatal(err)
	}

	// SubscribeWithSnapshot during the window: must see running log only (no DB duplicate!).
	snap, ch, unsub, err := h.c.Attach(ctx, chat.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()

	if !snap.Busy {
		t.Fatal("expected snap.Busy == true before finishTurn")
	}
	var types []string
	var tokens strings.Builder
	for _, e := range snap.Events {
		types = append(types, e.Type)
		if e.Type == EventToken {
			tokens.WriteString(e.Text)
		}
	}
	if got := strings.Join(types, ","); got != "turn.started,token" {
		t.Fatalf("snapshot types=%s events=%+v", got, snap.Events)
	}
	if tokens.String() != "Hello" {
		t.Fatalf("coalesced token=%q", tokens.String())
	}

	// Now finishTurn runs; subscriber receives turn.done with Seq > LastSeq.
	doneEv := h.c.hub.finishTurn(Event{
		Type: EventTurnDone, ConversationID: chat.ConversationID, TurnID: "turn_1", Reply: "Hello",
	})
	live := drainUntil(t, ch, isTurnEnd)
	if len(live) != 1 || live[0].Seq != doneEv.Seq || live[0].Seq <= snap.LastSeq {
		t.Fatalf("live=%+v done=%+v lastSeq=%d", live, doneEv, snap.LastSeq)
	}

	// Snapshot taken AFTER finishTurn reads the persisted DB rows and has Busy=false.
	after, err := h.c.OpenChat(ctx, chat.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Busy {
		t.Fatal("expected Busy == false after finishTurn")
	}
	types = nil
	for _, e := range after.Events {
		types = append(types, e.Type)
	}
	if got := strings.Join(types, ","); got != "turn.started,token,turn.done" {
		t.Fatalf("post-turn snapshot types=%s events=%+v", got, after.Events)
	}
}

func TestSnapshotDuringPendingApprovalContainsRequestAndFirstAnswerWins(t *testing.T) {
	h := newHarness(t, &fakeClient{scripts: []llm.ChatResponse{deliver("c1", "shared.md"), say("Done.")}})
	ctx := context.Background()
	root, convID := startChat(t, h, DefaultBotSlug)

	// Wait for UI to see the pending approval request.
	uiReq := h.nextApproval(t)

	// Late subscriber attaches while approval is pending.
	snap, ch, unsub, err := h.c.Attach(ctx, convID)
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()

	if !snap.Busy {
		t.Fatal("expected Busy=true while waiting on approval")
	}
	var snapReq *Event
	for i := range snap.Events {
		if snap.Events[i].Type == EventApprovalRequest {
			snapReq = &snap.Events[i]
		}
	}
	if snapReq == nil || snapReq.ApprovalID != uiReq.ApprovalID || snapReq.DestRelPath != "shared.md" {
		t.Fatalf("pending approval missing from snapshot: %+v", snap.Events)
	}

	// Second client answers first; first client's answer must fail with "no longer pending".
	if err := h.c.RespondApproval(snapReq.ApprovalID, DecisionAllow); err != nil {
		t.Fatalf("first answer failed: %v", err)
	}
	if err := h.c.RespondApproval(snapReq.ApprovalID, DecisionDeny); err == nil || !strings.Contains(err.Error(), "no longer pending") {
		t.Fatalf("second answer err=%v, want 'no longer pending'", err)
	}

	// Both UI and subscriber receive approval.resolved and turn.done.
	subEvs := drainUntil(t, ch, isTurnEnd)
	uiEvs := h.waitFor(t, isTurnEnd)

	hasResolved := func(evs []Event) bool {
		for _, e := range evs {
			if e.Type == EventApprovalResolved && e.ApprovalID == uiReq.ApprovalID && e.Decision == DecisionAllow {
				return true
			}
		}
		return false
	}
	if !hasResolved(subEvs) || !hasResolved(uiEvs) {
		t.Fatalf("resolved missing: sub=%+v ui=%+v", subEvs, uiEvs)
	}
	if _, err := os.Stat(filepath.Join(root, "shared.md")); err != nil {
		t.Fatalf("approved file not created: %v", err)
	}
}

func TestOpenChatErrorsOnNoParticipantAndLoadsUncachedConversation(t *testing.T) {
	h := newHarness(t, &fakeClient{scripts: []llm.ChatResponse{say("Welcome back.")}})
	ctx := context.Background()
	_ = h.c.SaveAPIKey("k")

	// Conversation with no participant -> clear error.
	if _, err := h.c.st.CreateConversation(ctx, "conv_legacy_nobot", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.c.OpenChat(ctx, "conv_legacy_nobot"); err == nil || !strings.Contains(err.Error(), "no bot participant") {
		t.Fatalf("expected clear error for bot-less conversation, got %v", err)
	}

	// Conversation stored directly in DB (not in h.c.chats yet) -> OpenChat loads on demand.
	b, err := h.c.bots.Get(ctx, DefaultBotSlug)
	if err != nil || b == nil {
		t.Fatalf("default bot missing: %v", err)
	}
	if _, err := h.c.st.CreateConversation(ctx, "conv_existing", "", b.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.c.st.AppendMessages(ctx, "conv_existing", []store.MessageRecord{
		{Role: "user", Content: "earlier question"},
		{Role: "assistant", BotID: b.ID, Content: "earlier answer"},
	}); err != nil {
		t.Fatal(err)
	}

	snap, err := h.c.OpenChat(ctx, "conv_existing")
	if err != nil {
		t.Fatal(err)
	}
	if snap.BotSlug != DefaultBotSlug || snap.Busy || len(snap.Events) != 3 {
		t.Fatalf("snap=%+v", snap)
	}
	if snap.Events[0].Type != EventTurnStarted || snap.Events[0].Text != "earlier question" ||
		snap.Events[1].Type != EventToken || snap.Events[1].Text != "earlier answer" ||
		snap.Events[2].Type != EventTurnDone || snap.Events[2].Reply != "earlier answer" {
		t.Fatalf("unexpected replayed events: %+v", snap.Events)
	}

	// Sending on the loaded conversation works.
	if _, err := h.c.Send(ctx, "conv_existing", "follow up"); err != nil {
		t.Fatal(err)
	}
	evs := h.waitFor(t, isTurnEnd)
	if evs[len(evs)-1].Reply != "Welcome back." {
		t.Fatalf("reply=%+v", evs[len(evs)-1])
	}
}

func TestStaleRebuildAfterAddWorkspaceKeepsSessionApprovals(t *testing.T) {
	h := newHarness(t, &fakeClient{scripts: []llm.ChatResponse{
		deliver("c1", "first.md"), say("saved first"),
		toolCall("c2", "deliver_file", `{"workspace_id":1,"dest_rel_path":"second.md","content":"# Second"}`), say("saved second"),
	}})
	ctx := context.Background()
	root, convID := startChat(t, h, DefaultBotSlug)

	req := h.nextApproval(t)
	if err := h.c.RespondApproval(req.ApprovalID, DecisionAllowSession); err != nil {
		t.Fatal(err)
	}
	h.waitFor(t, isTurnEnd)

	// Adding a second folder marks every cached chat stale.
	h.addFolder(t)

	// Next turn rebuilds the session but MUST reuse the existing Gate so first folder's
	// session approval for new files is preserved (no second approval prompt).
	if _, err := h.c.Send(ctx, convID, "save another"); err != nil {
		t.Fatal(err)
	}
	evs := h.waitFor(t, isTurnEnd)
	for _, e := range evs {
		if e.Type == EventApprovalRequest {
			t.Fatalf("stale rebuild lost session approval: %+v", e)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "second.md")); err != nil {
		t.Fatalf("second.md not created: %v", err)
	}
}

func TestListChatsReturnsNewestFirstWithPreview(t *testing.T) {
	h := newHarness(t, &fakeClient{scripts: []llm.ChatResponse{say("ok")}})
	ctx := context.Background()
	_ = h.c.SaveAPIKey("k")

	chat, err := h.c.BotChat(ctx, DefaultBotSlug)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.c.Send(ctx, chat.ConversationID, "hello world\nsecond line"); err != nil {
		t.Fatal(err)
	}
	h.waitFor(t, isTurnEnd)

	list, err := h.c.ListChats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != chat.ConversationID || list[0].BotSlug != DefaultBotSlug || list[0].BotName != DefaultBotName || list[0].Preview != "hello world" {
		t.Fatalf("list=%+v", list)
	}
}
