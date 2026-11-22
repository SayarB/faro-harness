// Package desktop holds the GUI-facing state machine (settings, folders, bots, one chat turn at a
// time). It has no Wails dependency: the shell injects an Emit callback and calls its methods.
package desktop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/SayarB/faro-harness/internal/agent"
	"github.com/SayarB/faro-harness/internal/approval"
	"github.com/SayarB/faro-harness/internal/bot"
	"github.com/SayarB/faro-harness/internal/llm"
	"github.com/SayarB/faro-harness/internal/store"
	"github.com/SayarB/faro-harness/internal/tools"
	"github.com/SayarB/faro-harness/internal/worker"
)

// maxToolResultDisplay bounds the tool output forwarded to the UI (display only; the model
// still receives the full result).
const maxToolResultDisplay = 4 * 1024

const (
	maxListedChats = 50
	maxChatPreview = 80
)

// Options configures a Controller.
type Options struct {
	Store   *store.Store
	Bots    *bot.Manager
	DataDir string
	Emit    func(Event)
	// NewClient builds the LLM client from an API key. Defaults to the Vercel AI Gateway client.
	NewClient func(apiKey string) (llm.Client, error)
}

type chatEntry struct {
	convID  string
	sess    *agent.Session
	gate    *approval.Gate
	stale   bool
	botSlug string
}

// Controller owns all desktop host state. Methods are safe for concurrent use.
type Controller struct {
	st      *store.Store
	bots    *bot.Manager
	dataDir string
	hub     *hub
	newCli  func(string) (llm.Client, error)

	rootCtx context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	mu         sync.Mutex
	chats      map[string]*chatEntry // conversationID -> cached session + approval gate
	busy       bool
	turnID     string
	turnConvID string
	turnStop   context.CancelFunc
	indexing   map[int64]bool
	pending    map[string]chan approval.Choice // approval id -> answer channel
}

// New creates a Controller.
func New(opts Options) (*Controller, error) {
	if opts.Store == nil || opts.Bots == nil {
		return nil, errors.New("store and bots manager are required")
	}
	if opts.NewClient == nil {
		opts.NewClient = func(key string) (llm.Client, error) { return llm.NewVercelGatewayClient(key, "") }
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &Controller{
		st:       opts.Store,
		bots:     opts.Bots,
		dataDir:  opts.DataDir,
		hub:      newHub(opts.Emit),
		newCli:   opts.NewClient,
		rootCtx:  ctx,
		cancel:   cancel,
		chats:    map[string]*chatEntry{},
		indexing: map[int64]bool{},
		pending:  map[string]chan approval.Choice{},
	}
	if err := ensureDefaultBot(ctx, opts.Bots); err != nil {
		cancel()
		return nil, err
	}
	return c, nil
}

// Shutdown cancels running turns and indexing, waits for them to stop, and closes subscribers.
func (c *Controller) Shutdown() {
	c.cancel()
	c.wg.Wait()
	c.hub.closeAll()
}

// apiKey returns the AI Gateway key. AI_GATEWAY_API_KEY in the environment wins over the saved key.
func (c *Controller) apiKey() string {
	if k := strings.TrimSpace(os.Getenv("AI_GATEWAY_API_KEY")); k != "" {
		return k
	}
	s, _ := loadSettings(c.dataDir)
	return s.APIKey
}

// SaveAPIKey stores the key (plain text, 0600 file in the data dir).
func (c *Controller) SaveAPIKey(key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return errors.New("API key is empty")
	}
	return saveSettings(c.dataDir, settings{APIKey: key})
}

// State returns the current keys/folders/bots snapshot.
func (c *Controller) State(ctx context.Context) (State, error) {
	out := State{HasKey: c.apiKey() != "", DefaultBot: DefaultBotSlug, Workspaces: []WorkspaceInfo{}, Bots: []BotInfo{}}
	wss, err := c.userWorkspaces(ctx)
	if err != nil {
		return out, err
	}
	for _, ws := range wss {
		n, err := c.st.WorkspaceFileCount(ctx, ws.ID)
		if err != nil {
			return out, err
		}
		out.Workspaces = append(out.Workspaces, workspaceInfo(ws, n))
	}
	bs, err := c.bots.List(ctx)
	if err != nil {
		return out, err
	}
	for _, b := range bs {
		info := BotInfo{ID: b.ID, Slug: b.Slug, Name: b.Name}
		if b.Slug == DefaultBotSlug {
			out.Bots = append([]BotInfo{info}, out.Bots...) // the default bot is listed first
		} else {
			out.Bots = append(out.Bots, info)
		}
	}
	return out, nil
}

func workspaceInfo(ws store.Workspace, files int64) WorkspaceInfo {
	name := filepath.Base(ws.RootPath)
	if name == "" || name == "." || name == string(filepath.Separator) {
		name = ws.RootPath
	}
	return WorkspaceInfo{ID: ws.ID, Path: ws.RootPath, Name: name, FileCount: files}
}

func (c *Controller) userWorkspaces(ctx context.Context) ([]store.Workspace, error) {
	all, err := c.st.ListWorkspaces(ctx)
	if err != nil {
		return nil, err
	}
	var out []store.Workspace
	for _, ws := range all {
		if ws.Kind == store.WorkspaceKindUser || ws.Kind == "" {
			out = append(out, ws)
		}
	}
	return out, nil
}

// AddWorkspace attaches an existing folder and indexes it in the background, reporting
// index.progress events. The next chat turn will see the new folder.
func (c *Controller) AddWorkspace(ctx context.Context, path string) (WorkspaceInfo, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return WorkspaceInfo{}, errors.New("folder path is empty")
	}
	info, err := os.Stat(path)
	if err != nil {
		return WorkspaceInfo{}, err
	}
	if !info.IsDir() {
		return WorkspaceInfo{}, fmt.Errorf("%q is a file, not a folder", path)
	}
	ws, err := c.st.EnsureWorkspace(ctx, path)
	if err != nil {
		return WorkspaceInfo{}, err
	}
	c.mu.Lock()
	for _, ch := range c.chats {
		ch.stale = true
	}
	c.mu.Unlock()
	c.startIndex(ws)
	n, _ := c.st.WorkspaceFileCount(ctx, ws.ID)
	return workspaceInfo(ws, n), nil
}

// Rescan re-runs indexing for an attached folder.
func (c *Controller) Rescan(ctx context.Context, id int64) error {
	wss, err := c.userWorkspaces(ctx)
	if err != nil {
		return err
	}
	for _, ws := range wss {
		if ws.ID == id {
			c.startIndex(ws)
			return nil
		}
	}
	return fmt.Errorf("folder %d not found", id)
}

func (c *Controller) startIndex(ws store.Workspace) {
	c.mu.Lock()
	if c.indexing[ws.ID] {
		c.mu.Unlock()
		return
	}
	c.indexing[ws.ID] = true
	c.mu.Unlock()

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer func() {
			c.mu.Lock()
			delete(c.indexing, ws.ID)
			c.mu.Unlock()
		}()
		progress := func(s worker.StatsSnapshot) {
			c.hub.emit(Event{Type: EventIndex, WorkspaceID: ws.ID, Indexed: s.Indexed, Skipped: s.Skipped, IndexErrors: s.Errors})
		}
		progress(worker.StatsSnapshot{})
		_, snap, err := worker.IndexFolder(c.rootCtx, c.st, ws.RootPath, progress)
		ev := Event{Type: EventIndex, WorkspaceID: ws.ID, Indexed: snap.Indexed, Skipped: snap.Skipped, IndexErrors: snap.Errors, Done: true}
		if err != nil && c.rootCtx.Err() == nil {
			ev.Error = err.Error()
		}
		c.hub.emit(ev)
	}()
}

// CreateBot creates a bot from a display name (slug derived) and optional instructions.
func (c *Controller) CreateBot(ctx context.Context, name, instructions string) (BotInfo, error) {
	name = strings.TrimSpace(name)
	base := slugify(name)
	if base == "" {
		return BotInfo{}, errors.New("give the bot a name using letters or numbers")
	}
	if base == DefaultBotSlug {
		return BotInfo{}, fmt.Errorf("%q is the name of the built-in assistant; choose a different name", DefaultBotName)
	}
	for i := 1; i <= 20; i++ {
		slug := base
		if i > 1 {
			slug = fmt.Sprintf("%s-%d", base, i)
		}
		if existing, err := c.bots.Get(ctx, slug); err != nil {
			return BotInfo{}, err
		} else if existing != nil {
			continue
		}
		b, err := c.bots.Create(ctx, bot.CreateOptions{Slug: slug, Name: name, Instructions: instructions})
		if err != nil {
			return BotInfo{}, err
		}
		return BotInfo{ID: b.ID, Slug: b.Slug, Name: b.Name}, nil
	}
	return BotInfo{}, errors.New("could not find a free bot id; try a different name")
}

func slugify(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			dash = false
		case b.Len() > 0 && !dash:
			b.WriteByte('-')
			dash = true
		}
	}
	s := strings.TrimRight(b.String(), "-")
	if len(s) > 40 {
		s = strings.TrimRight(s[:40], "-")
	}
	return s
}

// NewChat starts a fresh conversation with the bot botSlug. Every chat has a bot; the UI
// selects the default one ("faro") unless the user picks another.
func (c *Controller) NewChat(ctx context.Context, botSlug string) (ChatInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.busy {
		return ChatInfo{}, errors.New("a reply is still in progress")
	}
	entry := &chatEntry{}
	entry.gate = c.newGate(func() string { return entry.convID })
	sess, b, err := c.buildSession(ctx, botSlug, "", entry.gate, func(ev agent.Event) {
		c.onAgentEvent(entry.convID, ev)
	})
	if err != nil {
		return ChatInfo{}, err
	}
	entry.convID = sess.ConversationID()
	entry.sess = sess
	entry.botSlug = b.Slug
	c.chats[entry.convID] = entry
	return ChatInfo{ConversationID: entry.convID, BotSlug: entry.botSlug}, nil
}

// OpenChat loads an existing conversation on demand and returns its replayable event snapshot.
func (c *Controller) OpenChat(ctx context.Context, convID string) (Snapshot, error) {
	botSlug, err := c.ensureChat(ctx, convID)
	if err != nil {
		return Snapshot{}, err
	}
	return c.hub.snapshot(ctx, c.st, strings.TrimSpace(convID), botSlug)
}

// Attach loads an existing conversation on demand and atomically subscribes to its live events
// while capturing a consistent initial snapshot (no live event can have Seq <= Snapshot.LastSeq).
func (c *Controller) Attach(ctx context.Context, convID string) (Snapshot, <-chan Event, func(), error) {
	convID = strings.TrimSpace(convID)
	botSlug, err := c.ensureChat(ctx, convID)
	if err != nil {
		return Snapshot{}, nil, nil, err
	}
	snap, sub, err := c.hub.subscribeWithSnapshot(ctx, c.st, convID, botSlug)
	if err != nil {
		return Snapshot{}, nil, nil, err
	}
	return snap, sub.ch, func() { c.hub.unsubscribe(sub) }, nil
}

func (c *Controller) ensureChat(ctx context.Context, convID string) (string, error) {
	convID = strings.TrimSpace(convID)
	if convID == "" {
		return "", errors.New("conversation id is required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, err := c.loadChatLocked(ctx, convID)
	if err != nil {
		return "", err
	}
	return entry.botSlug, nil
}

// loadChatLocked returns the cached chatEntry for convID, loading and caching it from SQLite on
// demand. Caller must hold c.mu.
func (c *Controller) loadChatLocked(ctx context.Context, convID string) (*chatEntry, error) {
	if entry, ok := c.chats[convID]; ok {
		return entry, nil
	}
	conv, err := c.st.GetConversation(ctx, convID)
	if err != nil {
		return nil, err
	}
	if conv == nil {
		return nil, fmt.Errorf("conversation %q not found", convID)
	}
	participants, err := c.st.ConversationParticipants(ctx, convID)
	if err != nil {
		return nil, err
	}
	if len(participants) == 0 {
		return nil, fmt.Errorf("conversation %q has no bot participant and cannot be opened", convID)
	}
	if len(participants) > 1 {
		return nil, fmt.Errorf("conversation %q has %d participants; only single-bot conversations are supported", convID, len(participants))
	}
	entry := &chatEntry{convID: convID}
	entry.gate = c.newGate(func() string { return entry.convID })
	sess, b, err := c.buildSession(ctx, participants[0], convID, entry.gate, func(ev agent.Event) {
		c.onAgentEvent(entry.convID, ev)
	})
	if err != nil {
		return nil, err
	}
	entry.sess = sess
	entry.botSlug = b.Slug
	c.chats[convID] = entry
	return entry, nil
}

// ListChats returns up to 50 recent single-bot conversations ordered newest first.
func (c *Controller) ListChats(ctx context.Context) ([]ChatSummary, error) {
	rows, err := c.st.ListConversationsWithBot(ctx, maxListedChats)
	if err != nil {
		return nil, err
	}
	out := make([]ChatSummary, 0, len(rows))
	for _, r := range rows {
		out = append(out, ChatSummary{
			ID:        r.ID,
			BotSlug:   r.BotSlug,
			BotName:   r.BotName,
			UpdatedAt: r.UpdatedAt,
			Preview:   firstLinePreview(r.FirstUserMessage, maxChatPreview),
		})
	}
	return out, nil
}

func firstLinePreview(s string, max int) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return truncate(line, max)
		}
	}
	return ""
}

// buildSession must be called with c.mu held. Gated tools (deliver_file, bot_run_command) go
// through the chat's approval Gate, which asks the user via the approval event stream.
func (c *Controller) buildSession(
	ctx context.Context,
	botSlugOrID, convID string,
	gate *approval.Gate,
	onEvent func(agent.Event),
) (*agent.Session, *store.Bot, error) {
	key := c.apiKey()
	if key == "" {
		return nil, nil, errors.New("add your API key first")
	}
	client, err := c.newCli(key)
	if err != nil {
		return nil, nil, err
	}
	wss, err := c.userWorkspaces(ctx)
	if err != nil {
		return nil, nil, err
	}
	if strings.TrimSpace(botSlugOrID) == "" {
		return nil, nil, errors.New("choose an assistant first")
	}
	b, err := c.bots.Get(ctx, botSlugOrID)
	if err != nil {
		return nil, nil, err
	}
	if b == nil {
		return nil, nil, fmt.Errorf("bot %q not found", botSlugOrID)
	}
	reg, err := tools.NewRegistryWithOptions(c.st, wss, b, tools.RegistryOptions{Approver: gate.Approve})
	if err != nil {
		return nil, nil, err
	}
	sess, err := agent.NewSession(agent.Config{
		Client: client, Tools: reg, Workspaces: wss, Bot: b, Store: c.st,
		ConversationID: convID, StreamTokens: true, OnEvent: onEvent,
	})
	if err != nil {
		return nil, nil, err
	}
	return sess, b, nil
}

func (c *Controller) onAgentEvent(convID string, ev agent.Event) {
	out := Event{Step: ev.Step}
	switch ev.Kind {
	case agent.EventToken:
		out.Type, out.Text = EventToken, ev.Text
	case agent.EventToolCall:
		out.Type, out.ToolCallID, out.ToolName, out.ToolArgs = EventToolCall, ev.ToolCallID, ev.ToolName, ev.ToolArgs
	case agent.EventToolResult:
		out.Type, out.ToolCallID, out.ToolName = EventToolResult, ev.ToolCallID, ev.ToolName
		out.ToolResult, out.DurationMs = truncate(ev.ToolResult, maxToolResultDisplay), ev.Duration.Milliseconds()
	case agent.EventUsage:
		u := ev.Usage
		out.Type, out.Usage = EventUsage, &u
	default:
		return
	}
	c.hub.emitForChat(convID, out)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// Send starts one chat turn in convID and returns its turn id immediately. turn.started is
// emitted synchronously before Send returns; progress arrives as events and the turn ends with
// exactly one of turn.done, turn.error or turn.cancelled. Only one turn runs across the app.
func (c *Controller) Send(ctx context.Context, convID, text string) (string, error) {
	convID = strings.TrimSpace(convID)
	if convID == "" {
		return "", errors.New("conversation id is required")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", errors.New("message is empty")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.busy {
		return "", errors.New("a reply is still in progress")
	}
	entry, err := c.loadChatLocked(ctx, convID)
	if err != nil {
		return "", err
	}
	if entry.stale {
		rebuilt, _, err := c.buildSession(ctx, entry.botSlug, entry.convID, entry.gate, func(ev agent.Event) {
			c.onAgentEvent(entry.convID, ev)
		})
		if err != nil {
			return "", err
		}
		entry.sess, entry.stale = rebuilt, false
	}
	startMsgSeq, err := c.st.MaxMessageSeq(ctx, entry.convID)
	if err != nil {
		return "", err
	}
	id, err := bot.NewID("turn")
	if err != nil {
		return "", err
	}
	turnCtx, stop := context.WithCancel(c.rootCtx)
	c.busy, c.turnID, c.turnConvID, c.turnStop = true, id, entry.convID, stop
	c.hub.startTurn(entry.convID, id, startMsgSeq, text)
	sess := entry.sess

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		start := time.Now()
		res, err := sess.Turn(turnCtx, text)
		canceled := turnCtx.Err() != nil
		stop()

		var endEv Event
		switch {
		case err != nil && canceled:
			endEv = Event{Type: EventTurnCanceled, ConversationID: convID, TurnID: id}
		case err != nil:
			endEv = Event{Type: EventTurnError, ConversationID: convID, TurnID: id, Error: err.Error()}
		default:
			u := res.Usage
			endEv = Event{Type: EventTurnDone, ConversationID: convID, TurnID: id, Reply: res.Reply, Usage: &u, DurationMs: time.Since(start).Milliseconds()}
		}

		c.mu.Lock()
		c.busy, c.turnStop, c.turnID, c.turnConvID = false, nil, "", ""
		c.hub.finishTurn(endEv)
		c.mu.Unlock()
	}()
	return id, nil
}

// Cancel stops the running turn if it belongs to convID. A turn running in another conversation
// is left alone, so a client attached to an idle thread cannot stop someone else's turn by accident.
func (c *Controller) Cancel(convID string) {
	c.mu.Lock()
	stop := c.turnStop
	if c.turnConvID != strings.TrimSpace(convID) {
		stop = nil
	}
	c.mu.Unlock()
	if stop != nil {
		stop()
	}
}
