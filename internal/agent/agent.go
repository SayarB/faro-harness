package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// Session holds the stable system prompt and growing conversation history.
type Session struct {
	client         llm.Client
	tools          *tools.Registry
	store          *store.Store
	bot            *store.Bot
	conversationID string
	sessionID      string
	model          string
	maxSteps       int
	onEvent        func(Event)
	streamTokens   bool
	systemPrompt   string
	history        []llm.Message
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
	history := []llm.Message{
		{Role: llm.RoleSystem, Content: sysPrompt},
	}

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
				history = append(history, msg)
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

	sess := &Session{
		client:         cfg.Client,
		tools:          cfg.Tools,
		store:          cfg.Store,
		bot:            botCopy,
		conversationID: convID,
		sessionID:      sessionID,
		model:          model,
		maxSteps:       maxSteps,
		onEvent:        cfg.OnEvent,
		streamTokens:   cfg.StreamTokens,
		systemPrompt:   sysPrompt,
		history:        history,
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

// Reset starts a fresh conversation while keeping the bot's Layer 1 + Layer 2 system prompt.
// When Store is configured, creates a new conversation ID so previous history remains intact in SQLite.
func (s *Session) Reset() error {
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

	s.history = []llm.Message{
		{Role: llm.RoleSystem, Content: s.systemPrompt},
	}
	s.sessionID = newSessionID
	if s.store != nil {
		s.conversationID = newConvID
		s.tools.SetConversationID(newConvID)
	}
	return nil
}

// Turn appends userText (Layer 4, raw with zero bloat) and runs the tool loop until
// the model produces a final reply or hits MaxSteps.
// Completed tool batches are persisted immediately so side effects are never lost if a
// subsequent step fails.
func (s *Session) Turn(ctx context.Context, userText string) (TurnResult, error) {
	startLen := len(s.history)
	persistedLen := startLen
	s.history = append(s.history, llm.Message{
		Role:    llm.RoleUser,
		Content: userText,
	})

	toolDefs := s.tools.Definitions()
	var totalUsage llm.Usage
	toolCallCount := 0

	abortWithPartialSave := func(cause error) (TurnResult, error) {
		s.history = s.history[:persistedLen]
		if persistedLen > startLen {
			// Append a closing assistant note so persisted history never ends on a dangling tool_result.
			note := llm.Message{
				Role:    llm.RoleAssistant,
				Content: fmt.Sprintf("[Turn interrupted before finishing: %v]", cause),
			}
			s.history = append(s.history, note)
			_ = s.persistDelta(context.Background(), []llm.Message{note})
		}
		return TurnResult{}, cause
	}

	for step := 1; step <= s.maxSteps; step++ {
		chatReq := llm.ChatRequest{
			Model:     s.model,
			SessionID: s.sessionID,
			Messages:  s.history,
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

		s.history = append(s.history, resp.Message)

		if len(resp.Message.ToolCalls) == 0 {
			if err := s.persistDelta(ctx, s.history[persistedLen:]); err != nil {
				return abortWithPartialSave(err)
			}
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
				s.history = append(s.history, llm.Message{
					Role:       llm.RoleTool,
					ToolCallID: tc.ID,
					Name:       tc.Name,
					Content:    fmt.Sprintf(`{"error":%q}`, execErr.Error()),
				})
				// Fill any remaining unexecuted calls in this batch so history stays schema-valid.
				for _, rem := range resp.Message.ToolCalls {
					if !hasToolResultSince(s.history[persistedLen:], rem.ID) {
						s.history = append(s.history, llm.Message{
							Role:       llm.RoleTool,
							ToolCallID: rem.ID,
							Name:       rem.Name,
							Content:    `{"error":"skipped due to turn interruption"}`,
						})
					}
				}
				if pErr := s.persistDelta(context.Background(), s.history[persistedLen:]); pErr == nil {
					persistedLen = len(s.history)
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

			s.history = append(s.history, llm.Message{
				Role:       llm.RoleTool,
				ToolCallID: tc.ID,
				Name:       tc.Name,
				Content:    out,
			})
		}

		// Persist completed tool batch immediately so side effects survive a later LLM failure.
		if err := s.persistDelta(ctx, s.history[persistedLen:]); err != nil {
			return abortWithPartialSave(err)
		}
		persistedLen = len(s.history)
	}

	return abortWithPartialSave(fmt.Errorf("agent exceeded max steps (%d) without producing a final answer", s.maxSteps))
}

func hasToolResultSince(msgs []llm.Message, toolCallID string) bool {
	for _, m := range msgs {
		if m.Role == llm.RoleTool && m.ToolCallID == toolCallID {
			return true
		}
	}
	return false
}

func (s *Session) persistDelta(ctx context.Context, delta []llm.Message) error {
	if s.store == nil || s.conversationID == "" || len(delta) == 0 {
		return nil
	}
	botID := ""
	if s.bot != nil {
		botID = s.bot.ID
	}
	recs := make([]store.MessageRecord, 0, len(delta))
	for _, m := range delta {
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
	return s.store.AppendMessages(ctx, s.conversationID, recs)
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
