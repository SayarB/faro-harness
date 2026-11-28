package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/SayarB/faro-harness/internal/bot"
	"github.com/SayarB/faro-harness/internal/llm"
	"github.com/SayarB/faro-harness/internal/store"
	"github.com/SayarB/faro-harness/internal/tools"
)

func makeSyntheticTurns(n int) []turnGroup {
	turns := make([]turnGroup, 0, n)
	seq := 1
	for i := 1; i <= n; i++ {
		u := historyEntry{seq: seq, msg: llm.Message{Role: llm.RoleUser, Content: fmt.Sprintf("user turn %d", i)}}
		seq++
		a := historyEntry{seq: seq, msg: llm.Message{Role: llm.RoleAssistant, Content: fmt.Sprintf("assistant reply %d", i)}}
		seq++
		turns = append(turns, turnGroup{
			entries:  []historyEntry{u, a},
			startSeq: u.seq,
			endSeq:   a.seq,
		})
	}
	return turns
}

func TestPlanHistoryBoundaryMovesOnlyEveryKTurnsAndAgesGeometrically(t *testing.T) {
	cfg := HistoryConfig{KeepRecentTurns: 3, StrideTurns: 4, TierGrowth: 2}

	// Below n+k (0..6 turns): boundary stays 0, everything verbatim.
	for nTurns := 0; nTurns <= 6; nTurns++ {
		p := planHistory(makeSyntheticTurns(nTurns), cfg)
		if p.boundary != 0 || len(p.chunks) != 0 || len(p.epochChunks) != 0 || len(p.verbatim) != nTurns {
			t.Fatalf("turns=%d: boundary=%d chunks=%d epoch=%d verbatim=%d", nTurns, p.boundary, len(p.chunks), len(p.epochChunks), len(p.verbatim))
		}
	}

	// Turns 7, 8, 9, 10: boundary stays pinned at 4 for all k=4 turns (cache-stable prefix!).
	var prevHash string
	for nTurns := 7; nTurns <= 10; nTurns++ {
		p := planHistory(makeSyntheticTurns(nTurns), cfg)
		if p.boundary != 4 || len(p.chunks) != 1 || p.chunks[0].level != LevelDetail || len(p.verbatim) != nTurns-4 {
			t.Fatalf("turns=%d: unexpected plan %+v", nTurns, p)
		}
		if prevHash != "" && p.chunks[0].sourceHash != prevHash {
			t.Fatalf("chunk 0 hash drifted between turns %d and %d", nTurns-1, nTurns)
		}
		prevHash = p.chunks[0].sourceHash
	}

	// At 35 turns: boundary = floor((35-3)/4)*4 = 32 -> 8 chunks (0..7).
	// With g=2:
	//   age 0 (chunk 7) -> detail (1 chunk)
	//   age 1..2 (chunks 5, 6) -> short (2 chunks)
	//   age 3..6 (chunks 1, 2, 3, 4) -> gist (4 chunks)
	//   age 7 (chunk 0) -> epoch
	p := planHistory(makeSyntheticTurns(35), cfg)
	if p.boundary != 32 || len(p.verbatim) != 3 {
		t.Fatalf("boundary=%d verbatim=%d", p.boundary, len(p.verbatim))
	}
	if len(p.epochChunks) != 1 || p.epochChunks[0].index != 0 {
		t.Fatalf("epochChunks=%+v", p.epochChunks)
	}
	wantLevels := []string{LevelGist, LevelGist, LevelGist, LevelGist, LevelShort, LevelShort, LevelDetail}
	if len(p.chunks) != len(wantLevels) {
		t.Fatalf("len(chunks)=%d want %d", len(p.chunks), len(wantLevels))
	}
	for i, want := range wantLevels {
		if p.chunks[i].index != i+1 || p.chunks[i].level != want {
			t.Fatalf("chunk[%d]=(idx=%d, level=%s) want (idx=%d, level=%s)", i, p.chunks[i].index, p.chunks[i].level, i+1, want)
		}
	}
}

func TestBuildTurnMessagesHardByteCapNeverSplitsToolCalls(t *testing.T) {
	// Turn 1: large tool call + result; Turn 2: small turn; Current turn: user prompt.
	entries := []historyEntry{
		{seq: 1, msg: llm.Message{Role: llm.RoleUser, Content: "read big file"}},
		{seq: 2, msg: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "tc1", Name: "read_file", Arguments: `{}`}}}},
		{seq: 3, msg: llm.Message{Role: llm.RoleTool, ToolCallID: "tc1", Name: "read_file", Content: strings.Repeat("A", 4000)}},
		{seq: 4, msg: llm.Message{Role: llm.RoleAssistant, Content: "done reading"}},
		{seq: 5, msg: llm.Message{Role: llm.RoleUser, Content: "short follow-up"}},
		{seq: 6, msg: llm.Message{Role: llm.RoleAssistant, Content: "short answer"}},
	}
	plan := planHistory(groupTurns(entries), HistoryConfig{KeepRecentTurns: 3, StrideTurns: 4})
	cur := []historyEntry{{seq: 7, msg: llm.Message{Role: llm.RoleUser, Content: "now?"}}}

	// Cap allows Turn 2 + Current, but NOT Turn 1. Turn 1 must be dropped as a whole unit.
	msgs := buildTurnMessages("SYS", plan, nil, cur, 500)
	// Expect: system, turn2.user, turn2.assistant, cur.user.
	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages after dropping whole oversized turn 1, got %d: %+v", len(msgs), msgs)
	}
	if msgs[1].Role != llm.RoleUser || msgs[1].Content != "short follow-up" {
		t.Fatalf("unexpected first kept message: %+v", msgs[1])
	}
	for _, m := range msgs {
		if m.Role == llm.RoleTool {
			t.Fatalf("orphaned tool message leaked after trimming: %+v", msgs)
		}
	}
}

type compactionTestClient struct {
	mu            sync.Mutex
	chatReqs      []llm.ChatRequest
	compactReqs   []llm.ChatRequest
	failCompact   bool
	compactCount  int
}

func (c *compactionTestClient) Chat(_ context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	msgsCopy := append([]llm.Message(nil), req.Messages...)
	reqCopy := req
	reqCopy.Messages = msgsCopy

	// Compaction calls have no tools and use the compaction system prompt.
	if len(req.Tools) == 0 && len(req.Messages) > 0 && strings.Contains(req.Messages[0].Content, "compact conversation history") {
		c.compactReqs = append(c.compactReqs, reqCopy)
		if c.failCompact {
			return llm.ChatResponse{}, errors.New("summarizer unavailable")
		}
		c.compactCount++
		return llm.ChatResponse{
			Message: llm.Message{
				Role:    llm.RoleAssistant,
				Content: fmt.Sprintf("LLM summary #%d", c.compactCount),
			},
		}, nil
	}

	c.chatReqs = append(c.chatReqs, reqCopy)
	userMsg := req.Messages[len(req.Messages)-1].Content
	return llm.ChatResponse{
		Message: llm.Message{
			Role:    llm.RoleAssistant,
			Content: "reply to: " + userMsg,
		},
	}, nil
}

func TestSessionGeometricCompactionPersistsAndKeepsCacheStablePrefix(t *testing.T) {
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
	reg, err := tools.NewRegistryForBot(st, nil, &b)
	if err != nil {
		t.Fatal(err)
	}

	client := &compactionTestClient{}
	hCfg := HistoryConfig{KeepRecentTurns: 2, StrideTurns: 2, TierGrowth: 2}
	sess, err := NewSession(Config{
		Client:  client,
		Tools:   reg,
		Bot:     &b,
		Store:   st,
		History: hCfg,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	// Turns 1..3: below n+k=4, no compaction triggered.
	for i := 1; i <= 3; i++ {
		if _, err := sess.Turn(ctx, fmt.Sprintf("msg %d", i)); err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
	}
	sess.WaitCompaction()
	client.mu.Lock()
	if len(client.compactReqs) != 0 {
		t.Fatalf("expected 0 compactions before 4 turns, got %d", len(client.compactReqs))
	}
	client.mu.Unlock()

	// Turn 4 completes -> 4 completed turns -> boundary jumps to 2 (chunk 0 at Detail) -> background job runs once.
	if _, err := sess.Turn(ctx, "msg 4"); err != nil {
		t.Fatalf("turn 4: %v", err)
	}
	sess.WaitCompaction()

	// Turn 5 uses the persisted LLM summary for chunk 0 + verbatim turns 3, 4 + msg 5.
	if _, err := sess.Turn(ctx, "msg 5"); err != nil {
		t.Fatalf("turn 5: %v", err)
	}
	sess.WaitCompaction()

	// Turn 6 stays on the same boundary=2 (stride k=2), so the summary block must be byte-identical to Turn 5
	// and NO extra compaction job should run between Turn 4 and Turn 5.
	if _, err := sess.Turn(ctx, "msg 6"); err != nil {
		t.Fatalf("turn 6: %v", err)
	}
	sess.WaitCompaction()

	client.mu.Lock()
	turn5Req := client.chatReqs[4]
	turn6Req := client.chatReqs[5]
	compactCallsAfterTurn6 := len(client.compactReqs)
	client.mu.Unlock()

	if !strings.Contains(turn5Req.Messages[1].Content, "LLM summary #1") || !strings.Contains(turn5Req.Messages[1].Content, "[Detail: messages 1-4]") {
		t.Fatalf("turn 5 missing expected Detail summary:\n%s", turn5Req.Messages[1].Content)
	}
	if turn5Req.Messages[0].Content != turn6Req.Messages[0].Content {
		t.Fatal("system prompt mutated across turns")
	}
	if turn5Req.Messages[1].Content != turn6Req.Messages[1].Content {
		t.Fatalf("summary block mutated within the same k-turn stride:\nT5=%s\nT6=%s", turn5Req.Messages[1].Content, turn6Req.Messages[1].Content)
	}
	// Turn 6 completed -> 6 completed turns -> boundary jumps to 4 -> chunk 0 ages to Short + chunk 1 gets Detail (2 more calls = 3 total).
	if compactCallsAfterTurn6 != 3 {
		t.Fatalf("expected 3 total compaction calls (chunk0:detail, chunk0:short, chunk1:detail), got %d", compactCallsAfterTurn6)
	}

	// Verify summaries persisted in SQLite and survive session restart without re-summarizing.
	saved, err := st.ListChunkSummaries(ctx, sess.ConversationID())
	if err != nil {
		t.Fatal(err)
	}
	if len(saved) != 3 {
		t.Fatalf("expected 3 persisted chunk summaries, got %+v", saved)
	}
	// Raw messages in SQLite must remain completely untouched (6 turns * 2 = 12 messages).
	rawMsgs, err := st.ListMessages(ctx, sess.ConversationID())
	if err != nil || len(rawMsgs) != 12 {
		t.Fatalf("raw transcript must stay intact: len=%d err=%v", len(rawMsgs), err)
	}

	// Reload session from SQLite: Turn 7 immediately sees [Short] + [Detail] with zero extra summarization calls.
	sess2, err := NewSession(Config{
		Client:         client,
		Tools:          reg,
		Bot:            &b,
		Store:          st,
		ConversationID: sess.ConversationID(),
		History:        hCfg,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sess2.Close()

	if _, err := sess2.Turn(ctx, "msg 7"); err != nil {
		t.Fatalf("turn 7: %v", err)
	}
	sess2.WaitCompaction()

	client.mu.Lock()
	turn7Req := client.chatReqs[6]
	totalCompacts := len(client.compactReqs)
	client.mu.Unlock()

	if totalCompacts != 3 {
		t.Fatalf("reloaded session re-ran compaction unnecessarily: got %d calls", totalCompacts)
	}
	summaryBlock := turn7Req.Messages[1].Content
	if !strings.Contains(summaryBlock, "[Short: messages 1-4]") || !strings.Contains(summaryBlock, "[Detail: messages 5-8]") {
		t.Fatalf("reloaded summary block missing aged tiers:\n%s", summaryBlock)
	}
}

func TestSessionFallsBackDeterministicallyWhenSummarizerFails(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	mgr, _ := bot.NewManager(st, t.TempDir())
	b, _ := mgr.Create(ctx, bot.CreateOptions{Slug: "faro", Name: "Faro"})
	reg, _ := tools.NewRegistryForBot(st, nil, &b)

	client := &compactionTestClient{failCompact: true}
	sess, err := NewSession(Config{
		Client:  client,
		Tools:   reg,
		Bot:     &b,
		Store:   st,
		History: HistoryConfig{KeepRecentTurns: 2, StrideTurns: 2, TierGrowth: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	for i := 1; i <= 5; i++ {
		if _, err := sess.Turn(ctx, fmt.Sprintf("alpha %d", i)); err != nil {
			t.Fatalf("turn %d failed despite fallback: %v", i, err)
		}
		sess.WaitCompaction()
	}

	client.mu.Lock()
	turn5Req := client.chatReqs[4]
	client.mu.Unlock()
	if !strings.Contains(turn5Req.Messages[1].Content, "alpha 1") || !strings.Contains(turn5Req.Messages[1].Content, "[Detail: messages 1-4]") {
		t.Fatalf("expected deterministic fallback digest for chunk 0, got:\n%s", turn5Req.Messages[1].Content)
	}
}

func TestSessionEpochMergeFoldsOldestChunksIncrementally(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	mgr, _ := bot.NewManager(st, t.TempDir())
	b, _ := mgr.Create(ctx, bot.CreateOptions{Slug: "faro", Name: "Faro"})
	reg, _ := tools.NewRegistryForBot(st, nil, &b)

	client := &compactionTestClient{}
	// n=1, k=2, g=2 -> detail(1) + short(2) + gist(4) = 7 chunks (14 turns); chunk 0 enters epoch at 17 completed turns,
	// and chunk 1 folds into epoch at 19 completed turns.
	hCfg := HistoryConfig{KeepRecentTurns: 1, StrideTurns: 2, TierGrowth: 2}
	sess, err := NewSession(Config{
		Client:  client,
		Tools:   reg,
		Bot:     &b,
		Store:   st,
		History: hCfg,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	for i := 1; i <= 20; i++ {
		if _, err := sess.Turn(ctx, fmt.Sprintf("turn %d", i)); err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
		sess.WaitCompaction()
	}

	client.mu.Lock()
	lastReq := client.chatReqs[len(client.chatReqs)-1]
	client.mu.Unlock()

	summaryBlock := lastReq.Messages[1].Content
	for _, want := range []string{"[Epoch: messages 1-8]", "[Gist:", "[Short:", "[Detail:"} {
		if !strings.Contains(summaryBlock, want) {
			t.Fatalf("expected %q in summary block after 20 turns, got:\n%s", want, summaryBlock)
		}
	}
	// Verify epoch record persisted in SQLite with chunk_index = -1 and end_seq = 8.
	saved, err := st.ListChunkSummaries(ctx, sess.ConversationID())
	if err != nil {
		t.Fatal(err)
	}
	foundEpoch := false
	for _, rec := range saved {
		if rec.ChunkIndex == epochChunkIndex && rec.Level == LevelEpoch {
			foundEpoch = true
			if rec.StartSeq != 1 || rec.EndSeq != 8 {
				t.Fatalf("unexpected epoch seq range: %+v", rec)
			}
		}
	}
	if !foundEpoch {
		t.Fatalf("epoch summary not persisted in SQLite: %+v", saved)
	}
}
