package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/SayarB/faro-harness/internal/bot"
	"github.com/SayarB/faro-harness/internal/llm"
	"github.com/SayarB/faro-harness/internal/store"
	"github.com/SayarB/faro-harness/internal/tools"
)

const DefaultMaxSteps = 10

// EventKind categorizes live lifecycle events emitted during a turn.
type EventKind string

const (
	EventToolCall   EventKind = "tool_call"
	EventToolResult EventKind = "tool_result"
	EventUsage      EventKind = "usage"
	EventToken      EventKind = "token" // streamed assistant text fragment (only when Config.StreamTokens)
)

// Event is emitted to OnEvent observers (CLI logger, future UI stream).
type Event struct {
	Kind       EventKind
	Step       int
	Text       string // EventToken: the text fragment
	ToolCallID string // EventToolCall / EventToolResult: pairs a result with its call
	ToolName   string
	ToolArgs   string
	ToolResult string
	Duration   time.Duration
	Usage      llm.Usage
}

// Config configures a chat Session.
type Config struct {
	Client         llm.Client
	Tools          *tools.Registry
	Workspaces     []store.Workspace
	Bot            *store.Bot
	BotProfile     *bot.Profile
	Store          *store.Store // Optional: enables SQLite conversation persistence
	ConversationID string       // Optional: resumes an existing conversation when Store is set
	Model          string
	MaxSteps       int
	History        HistoryConfig // Optional: tunes geometric compaction (n, k, g) and replay byte cap
	Now            func() time.Time
	OnEvent        func(Event)
	StreamTokens   bool // when true, request streamed replies and emit EventToken fragments
}

// TurnResult summarizes the outcome of a single user turn.
type TurnResult struct {
	Reply     string
	Steps     int
	ToolCalls int
	Usage     llm.Usage
}

// Session holds the stable system prompt, conversation transcript, and cached compaction summaries.
type Session struct {
	client         llm.Client
	tools          *tools.Registry
	store          *store.Store
	bot            *store.Bot
	conversationID string
	sessionID      string
	model          string
	maxSteps       int
	historyCfg     HistoryConfig
	onEvent        func(Event)
	streamTokens   bool
	systemPrompt   string

	entries []historyEntry
	nextSeq int

	summariesMu sync.Mutex
	summaries   map[summaryKey]store.ChunkSummary
	bgCtx       context.Context
	bgCancel    context.CancelFunc
	bgRunning   bool
	bgQueued    []compactionJob
	bgWg        sync.WaitGroup
}

// NewSession initializes a chat session with a cache-stable Layer 1 + Layer 2 system prompt.
// When Config.Store is set, conversations and messages are persisted in SQLite and isolated per bot.
func NewSession(cfg Config) (*Session, error) {
	if cfg.Client == nil {
		return nil, errors.New("llm client is required")
	}
	if cfg.Tools == nil {
		return nil, errors.New("tools registry is required")
	}
	if len(cfg.Workspaces) == 0 && cfg.Bot == nil {
		return nil, errors.New("at least one workspace or a bot is required")
	}
	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	maxSteps := cfg.MaxSteps
	if maxSteps <= 0 {
		maxSteps = DefaultMaxSteps
	}
	model := cfg.Model
	if model == "" && cfg.Bot != nil && cfg.Bot.Model != "" {
		model = cfg.Bot.Model
	}
	if model == "" {
		model = llm.DefaultModel
	}

	profile := cfg.BotProfile
	if profile == nil && cfg.Bot != nil {
		loaded, err := bot.LoadProfile(*cfg.Bot)
		if err != nil {
			return nil, fmt.Errorf("load bot profile: %w", err)
		}
		profile = &loaded
	}

	var botCopy *store.Bot
	if cfg.Bot != nil {
		c := *cfg.Bot
		botCopy = &c
	}

	sysPrompt := BuildSystemPromptForBot(nowFn(), cfg.Workspaces, profile)
	var entries []historyEntry
	nextSeq := 0
	summaries := map[summaryKey]store.ChunkSummary{}

	convID := cfg.ConversationID
	if cfg.Store != nil {
		ctx := context.Background()
		if convID == "" {
			var err error
			convID, err = bot.NewID("conv")
			if err != nil {
				return nil, err
			}
			var botIDs []string
			if botCopy != nil {
				botIDs = append(botIDs, botCopy.ID)
			}
			if _, err := cfg.Store.CreateConversation(ctx, convID, "", botIDs...); err != nil {
				return nil, err
			}
		} else {
			conv, err := cfg.Store.GetConversation(ctx, convID)
			if err != nil {
				return nil, err
			}
			if conv == nil {
				return nil, fmt.Errorf("conversation %q not found", convID)
			}
			participants, err := cfg.Store.ConversationParticipants(ctx, convID)
			if err != nil {
				return nil, err
			}
			if botCopy == nil {
				if len(participants) > 0 {
					return nil, fmt.Errorf("conversation %q belongs to a bot; pass --bot to resume it", convID)
				}
			} else if len(participants) != 1 || participants[0] != botCopy.ID {
				return nil, fmt.Errorf("conversation %q does not belong to bot %q", convID, botCopy.Slug)
			}
			records, err := cfg.Store.ListMessages(ctx, convID)
			if err != nil {
				return nil, err
			}
			for _, rec := range records {
				msg, err := recordToLLMMessage(rec)
				if err != nil {
					return nil, err
				}
				entries = append(entries, historyEntry{seq: rec.Seq, msg: msg})
				if rec.Seq > nextSeq {
					nextSeq = rec.Seq
				}
			}
			savedSummaries, err := cfg.Store.ListChunkSummaries(ctx, convID)
			if err != nil {
				return nil, err
			}
			for _, rec := range savedSummaries {
				summaries[summaryKey{chunkIndex: rec.ChunkIndex, level: rec.Level}] = rec
			}
		}
	}

	sessionID := convID
	if sessionID == "" {
		var err error
		sessionID, err = bot.NewID("sess")
		if err != nil {
			return nil, err
		}
	}

	bgCtx, bgCancel := context.WithCancel(context.Background())
	sess := &Session{
		client:         cfg.Client,
		tools:          cfg.Tools,
		store:          cfg.Store,
		bot:            botCopy,
		conversationID: convID,
		sessionID:      sessionID,
		model:          model,
		maxSteps:       maxSteps,
		historyCfg:     cfg.History.withDefaults(),
		onEvent:        cfg.OnEvent,
		streamTokens:   cfg.StreamTokens,
		systemPrompt:   sysPrompt,
		entries:        entries,
		nextSeq:        nextSeq,
		summaries:      summaries,
		bgCtx:          bgCtx,
		bgCancel:       bgCancel,
	}
	cfg.Tools.SetConversationID(convID)
	return sess, nil
}

// SystemPrompt returns the exact Layer 1 + Layer 2 system prompt for inspection/debugging.
func (s *Session) SystemPrompt() string {
	return s.systemPrompt
}

// ConversationID returns the persisted SQLite conversation ID (empty if persistence is disabled).
func (s *Session) ConversationID() string {
	return s.conversationID
}

// Close cancels any in-flight background compaction work and waits for it to exit.
func (s *Session) Close() {
	s.summariesMu.Lock()
	cancel := s.bgCancel
	s.bgQueued = nil
	s.summariesMu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.bgWg.Wait()
}

// WaitCompaction blocks until any currently scheduled background compaction jobs finish.
func (s *Session) WaitCompaction() {
	s.bgWg.Wait()
}

// Reset starts a fresh conversation while keeping the bot's Layer 1 + Layer 2 system prompt.
// When Store is configured, creates a new conversation ID so previous history remains intact in SQLite.
func (s *Session) Reset() error {
	s.Close()

	var newConvID string
	var newSessionID string
	if s.store != nil {
		id, err := bot.NewID("conv")
		if err != nil {
			return err
		}
		var botIDs []string
		if s.bot != nil {
			botIDs = append(botIDs, s.bot.ID)
		}
		if _, err := s.store.CreateConversation(context.Background(), id, "", botIDs...); err != nil {
			return err
		}
		newConvID = id
		newSessionID = id
	} else {
		id, err := bot.NewID("sess")
		if err != nil {
			return err
		}
		newSessionID = id
	}

	s.entries = nil
	s.nextSeq = 0
	s.sessionID = newSessionID
	if s.store != nil {
		s.conversationID = newConvID
		s.tools.SetConversationID(newConvID)
	}
	bgCtx, bgCancel := context.WithCancel(context.Background())
	s.summariesMu.Lock()
	s.summaries = map[summaryKey]store.ChunkSummary{}
	s.bgCtx = bgCtx
	s.bgCancel = bgCancel
	s.bgQueued = nil
	s.summariesMu.Unlock()
	return nil
}

// Turn appends userText (Layer 4, raw with zero bloat) and runs the tool loop until
// the model produces a final reply or hits MaxSteps.
// Completed tool batches are persisted immediately so side effects are never lost if a
// subsequent step fails.
func (s *Session) Turn(ctx context.Context, userText string) (TurnResult, error) {
	// Snapshot completed-turn plan and summaries once per turn so the prompt prefix stays
	// byte-identical across every tool step inside this turn (preserving intra-turn KV cache).
	priorPlan := planHistory(groupTurns(s.entries), s.historyCfg)
	summariesSnap := s.snapshotSummaries()

	startLen := len(s.entries)
	persistedLen := startLen
	s.entries = append(s.entries, historyEntry{
		msg: llm.Message{
			Role:    llm.RoleUser,
			Content: userText,
		},
	})

	toolDefs := s.tools.Definitions()
	var totalUsage llm.Usage
	toolCallCount := 0

	abortWithPartialSave := func(cause error) (TurnResult, error) {
		s.entries = s.entries[:persistedLen]
		if persistedLen > startLen {
			// Append a closing assistant note so persisted history never ends on a dangling tool_result.
			note := llm.Message{
				Role:    llm.RoleAssistant,
				Content: fmt.Sprintf("[Turn interrupted before finishing: %v]", cause),
			}
			s.entries = append(s.entries, historyEntry{msg: note})
			_ = s.persistEntries(context.Background(), persistedLen)
		}
		return TurnResult{}, cause
	}

	for step := 1; step <= s.maxSteps; step++ {
		reqMsgs := buildTurnMessages(
			s.systemPrompt,
			priorPlan,
			summariesSnap,
			s.entries[startLen:],
			s.historyCfg.MaxPromptHistoryBytes,
		)
		chatReq := llm.ChatRequest{
			Model:     s.model,
			SessionID: s.sessionID,
			Messages:  reqMsgs,
			Tools:     toolDefs,
		}
		if s.streamTokens {
			chatReq.OnDelta = func(text string) {
				s.emit(Event{Kind: EventToken, Step: step, Text: text})
			}
		}
		resp, err := s.client.Chat(ctx, chatReq)
		if err != nil {
			return abortWithPartialSave(err)
		}

		totalUsage.PromptTokens += resp.Usage.PromptTokens
		totalUsage.CompletionTokens += resp.Usage.CompletionTokens
		totalUsage.TotalTokens += resp.Usage.TotalTokens
		totalUsage.CachedTokens += resp.Usage.CachedTokens
		totalUsage.CacheWriteTokens += resp.Usage.CacheWriteTokens

		s.emit(Event{
			Kind:  EventUsage,
			Step:  step,
			Usage: resp.Usage,
		})

		s.entries = append(s.entries, historyEntry{msg: resp.Message})

		if len(resp.Message.ToolCalls) == 0 {
			if err := s.persistEntries(ctx, persistedLen); err != nil {
				return abortWithPartialSave(err)
			}
			// Trigger non-blocking background compaction if this turn crossed a k-turn stride
			// (or if previously compacted chunks are still missing LLM summaries).
			postPlan := planHistory(groupTurns(s.entries), s.historyCfg)
			s.scheduleCompaction(postPlan)
			return TurnResult{
				Reply:     resp.Message.Content,
				Steps:     step,
				ToolCalls: toolCallCount,
				Usage:     totalUsage,
			}, nil
		}

		for _, tc := range resp.Message.ToolCalls {
			toolCallCount++
			s.emit(Event{
				Kind:       EventToolCall,
				Step:       step,
				ToolCallID: tc.ID,
				ToolName:   tc.Name,
				ToolArgs:   tc.Arguments,
			})

			t0 := time.Now()
			out, execErr := s.tools.Execute(ctx, tc.Name, tc.Arguments)
			dur := time.Since(t0)
			if execErr != nil {
				// Record the interruption as a tool result so the already-completed tool calls
				// in this batch remain paired with tool_result messages before persisting.
				s.entries = append(s.entries, historyEntry{
					msg: llm.Message{
						Role:       llm.RoleTool,
						ToolCallID: tc.ID,
						Name:       tc.Name,
						Content:    fmt.Sprintf(`{"error":%q}`, execErr.Error()),
					},
				})
				// Fill any remaining unexecuted calls in this batch so history stays schema-valid.
				for _, rem := range resp.Message.ToolCalls {
					if !hasToolResultSince(s.entries[persistedLen:], rem.ID) {
						s.entries = append(s.entries, historyEntry{
							msg: llm.Message{
								Role:       llm.RoleTool,
								ToolCallID: rem.ID,
								Name:       rem.Name,
								Content:    `{"error":"skipped due to turn interruption"}`,
							},
						})
					}
				}
				if pErr := s.persistEntries(context.Background(), persistedLen); pErr == nil {
					persistedLen = len(s.entries)
				}
				return abortWithPartialSave(execErr)
			}

			s.emit(Event{
				Kind:       EventToolResult,
				Step:       step,
				ToolCallID: tc.ID,
				ToolName:   tc.Name,
				ToolResult: out,
				Duration:   dur,
			})

			s.entries = append(s.entries, historyEntry{
				msg: llm.Message{
					Role:       llm.RoleTool,
					ToolCallID: tc.ID,
					Name:       tc.Name,
					Content:    out,
				},
			})
		}

		// Persist completed tool batch immediately so side effects survive a later LLM failure.
		if err := s.persistEntries(ctx, persistedLen); err != nil {
			return abortWithPartialSave(err)
		}
		persistedLen = len(s.entries)
	}

	return abortWithPartialSave(fmt.Errorf("agent exceeded max steps (%d) without producing a final answer", s.maxSteps))
}

func (s *Session) snapshotSummaries() map[summaryKey]store.ChunkSummary {
	s.summariesMu.Lock()
	defer s.summariesMu.Unlock()
	out := make(map[summaryKey]store.ChunkSummary, len(s.summaries))
	for k, v := range s.summaries {
		out[k] = v
	}
	return out
}

func (s *Session) scheduleCompaction(plan historyPlan) {
	if len(plan.chunks) == 0 && len(plan.epochChunks) == 0 {
		return
	}
	s.summariesMu.Lock()
	if s.bgCtx == nil || s.bgCtx.Err() != nil {
		s.summariesMu.Unlock()
		return
	}
	jobs := collectMissingCompactionJobs(plan, s.summaries)
	if len(jobs) == 0 {
		s.summariesMu.Unlock()
		return
	}
	if s.bgRunning {
		s.bgQueued = jobs
		s.summariesMu.Unlock()
		return
	}
	s.bgRunning = true
	bgCtx := s.bgCtx
	convID := s.conversationID
	model := s.model
	s.bgWg.Add(1)
	s.summariesMu.Unlock()

	go func(initial []compactionJob) {
		defer s.bgWg.Done()
		batch := initial
		for {
			for _, job := range batch {
				if bgCtx.Err() != nil {
					break
				}
				key := summaryKey{chunkIndex: job.chunkIndex, level: job.level}
				s.summariesMu.Lock()
				existing, ok := s.summaries[key]
				s.summariesMu.Unlock()
				if ok && existing.SourceHash == job.sourceHash && existing.Summary != "" {
					continue
				}
				summaryText, err := runCompactionJob(bgCtx, s.client, model, job)
				if err != nil || bgCtx.Err() != nil {
					continue
				}
				rec := store.ChunkSummary{
					ConversationID: convID,
					ChunkIndex:     job.chunkIndex,
					Level:          job.level,
					StartSeq:       job.startSeq,
					EndSeq:         job.endSeq,
					SourceHash:     job.sourceHash,
					Summary:        summaryText,
					UpdatedAt:      time.Now().Unix(),
				}
				if s.store != nil && convID != "" {
					if err := s.store.UpsertChunkSummary(bgCtx, rec); err != nil {
						continue
					}
				}
				s.summariesMu.Lock()
				if bgCtx.Err() == nil {
					s.summaries[key] = rec
				}
				s.summariesMu.Unlock()
			}
			s.summariesMu.Lock()
			if len(s.bgQueued) == 0 || bgCtx.Err() != nil {
				s.bgQueued = nil
				s.bgRunning = false
				s.summariesMu.Unlock()
				return
			}
			batch = s.bgQueued
			s.bgQueued = nil
			s.summariesMu.Unlock()
		}
	}(jobs)
}

func hasToolResultSince(entries []historyEntry, toolCallID string) bool {
	for _, e := range entries {
		if e.msg.Role == llm.RoleTool && e.msg.ToolCallID == toolCallID {
			return true
		}
	}
	return false
}

// persistEntries writes s.entries[from:] to SQLite (when Store is set) and stamps each entry's
// monotonic 1-based seq so compaction pointers and chunk hashes stay deterministic.
func (s *Session) persistEntries(ctx context.Context, from int) error {
	if from >= len(s.entries) {
		return nil
	}
	delta := s.entries[from:]
	if s.store == nil || s.conversationID == "" {
		for i := range delta {
			s.nextSeq++
			delta[i].seq = s.nextSeq
		}
		return nil
	}

	botID := ""
	if s.bot != nil {
		botID = s.bot.ID
	}
	recs := make([]store.MessageRecord, 0, len(delta))
	for _, e := range delta {
		m := e.msg
		rec := store.MessageRecord{
			Role:       string(m.Role),
			Content:    m.Content,
			ToolCallID: m.ToolCallID,
			ToolName:   m.Name,
		}
		if m.Role != llm.RoleUser {
			rec.BotID = botID
		}
		if len(m.ToolCalls) > 0 {
			b, err := json.Marshal(m.ToolCalls)
			if err != nil {
				return err
			}
			rec.ToolCallsJSON = string(b)
		}
		recs = append(recs, rec)
	}
	if err := s.store.AppendMessages(ctx, s.conversationID, recs); err != nil {
		return err
	}
	maxSeq, err := s.store.MaxMessageSeq(ctx, s.conversationID)
	if err != nil || maxSeq < len(delta) {
		for i := range delta {
			s.nextSeq++
			delta[i].seq = s.nextSeq
		}
		return nil
	}
	firstSeq := maxSeq - len(delta) + 1
	for i := range delta {
		delta[i].seq = firstSeq + i
	}
	s.nextSeq = maxSeq
	return nil
}

func recordToLLMMessage(rec store.MessageRecord) (llm.Message, error) {
	msg := llm.Message{
		Role:       llm.Role(rec.Role),
		Content:    rec.Content,
		ToolCallID: rec.ToolCallID,
		Name:       rec.ToolName,
	}
	if rec.ToolCallsJSON != "" {
		if err := json.Unmarshal([]byte(rec.ToolCallsJSON), &msg.ToolCalls); err != nil {
			return llm.Message{}, fmt.Errorf("decode persisted tool calls: %w", err)
		}
	}
	return msg, nil
}

func (s *Session) emit(ev Event) {
	if s.onEvent != nil {
		s.onEvent(ev)
	}
}
