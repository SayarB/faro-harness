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
	"github.com/SayarB/faro-harness/internal/bot"
	"github.com/SayarB/faro-harness/internal/llm"
	"github.com/SayarB/faro-harness/internal/store"
	"github.com/SayarB/faro-harness/internal/tools"
	"github.com/SayarB/faro-harness/internal/worker"
)

// maxToolResultDisplay bounds the tool output forwarded to the UI (display only; the model
// still receives the full result).
const maxToolResultDisplay = 4 * 1024

// Options configures a Controller.
type Options struct {
	Store   *store.Store
	Bots    *bot.Manager
	DataDir string
	Emit    func(Event)
	// NewClient builds the LLM client from an API key. Defaults to the Vercel AI Gateway client.
	NewClient func(apiKey string) (llm.Client, error)
}

// Controller owns all GUI state. Methods are safe for concurrent use.
type Controller struct {
	st      *store.Store
	bots    *bot.Manager
	dataDir string
	emit    func(Event)
	newCli  func(string) (llm.Client, error)

	rootCtx context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	mu       sync.Mutex
	sess     *agent.Session
	botSlug  string
	stale    bool // a folder was added since the session was built; rebuild before the next turn
	busy     bool
	turnID   string
	turnStop context.CancelFunc
	indexing map[int64]bool
}

// New creates a Controller.
func New(opts Options) (*Controller, error) {
	if opts.Store == nil || opts.Bots == nil {
		return nil, errors.New("store and bots manager are required")
	}
	if opts.Emit == nil {
		opts.Emit = func(Event) {}
	}
	if opts.NewClient == nil {
		opts.NewClient = func(key string) (llm.Client, error) { return llm.NewVercelGatewayClient(key, "") }
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Controller{
		st: opts.Store, bots: opts.Bots, dataDir: opts.DataDir, emit: opts.Emit, newCli: opts.NewClient,
		rootCtx: ctx, cancel: cancel, indexing: map[int64]bool{},
	}, nil
}

// Shutdown cancels running turns and indexing and waits for them to stop.
func (c *Controller) Shutdown() {
	c.cancel()
	c.wg.Wait()
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
	out := State{HasKey: c.apiKey() != "", Workspaces: []WorkspaceInfo{}, Bots: []BotInfo{}}
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
		out.Bots = append(out.Bots, BotInfo{ID: b.ID, Slug: b.Slug, Name: b.Name})
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
	c.stale = true
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
			c.emit(Event{Type: EventIndex, WorkspaceID: ws.ID, Indexed: s.Indexed, Skipped: s.Skipped, IndexErrors: s.Errors})
		}
		progress(worker.StatsSnapshot{})
		_, snap, err := worker.IndexFolder(c.rootCtx, c.st, ws.RootPath, progress)
		ev := Event{Type: EventIndex, WorkspaceID: ws.ID, Indexed: snap.Indexed, Skipped: snap.Skipped, IndexErrors: snap.Errors, Done: true}
		if err != nil && c.rootCtx.Err() == nil {
			ev.Error = err.Error()
		}
		c.emit(ev)
	}()
}

// CreateBot creates a bot from a display name (slug derived) and optional instructions.
func (c *Controller) CreateBot(ctx context.Context, name, instructions string) (BotInfo, error) {
	name = strings.TrimSpace(name)
	base := slugify(name)
	if base == "" {
		return BotInfo{}, errors.New("give the bot a name using letters or numbers")
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

// NewChat starts a fresh conversation, optionally as a bot (empty botSlug = plain Faro).
func (c *Controller) NewChat(ctx context.Context, botSlug string) (ChatInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.busy {
		return ChatInfo{}, errors.New("a reply is still in progress")
	}
	sess, err := c.buildSession(ctx, botSlug, "")
	if err != nil {
		return ChatInfo{}, err
	}
	c.sess, c.botSlug, c.stale = sess, botSlug, false
	return ChatInfo{ConversationID: sess.ConversationID(), BotSlug: botSlug}, nil
}

// buildSession must be called with c.mu held. No approver is installed: deliver_file and
// bot_run_command are denied by the tools layer until an approval UI exists.
func (c *Controller) buildSession(ctx context.Context, botSlug, convID string) (*agent.Session, error) {
	key := c.apiKey()
	if key == "" {
		return nil, errors.New("add your API key first")
	}
	client, err := c.newCli(key)
	if err != nil {
		return nil, err
	}
	wss, err := c.userWorkspaces(ctx)
	if err != nil {
		return nil, err
	}
	var b *store.Bot
	if botSlug != "" {
		if b, err = c.bots.Get(ctx, botSlug); err != nil {
			return nil, err
		}
		if b == nil {
			return nil, fmt.Errorf("bot %q not found", botSlug)
		}
	}
	if len(wss) == 0 && b == nil {
		return nil, errors.New("add a folder first (or choose a bot)")
	}
	reg, err := tools.NewRegistryWithOptions(c.st, wss, b, tools.RegistryOptions{})
	if err != nil {
		return nil, err
	}
	return agent.NewSession(agent.Config{
		Client: client, Tools: reg, Workspaces: wss, Bot: b, Store: c.st,
		ConversationID: convID, StreamTokens: true, OnEvent: c.onAgentEvent,
	})
}

func (c *Controller) onAgentEvent(ev agent.Event) {
	c.mu.Lock()
	turn := c.turnID
	c.mu.Unlock()
	out := Event{TurnID: turn, Step: ev.Step}
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
	c.emit(out)
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

// Send starts one chat turn and returns its id immediately; progress arrives as events and the
// turn ends with exactly one of turn.done, turn.error or turn.cancelled. Only one turn runs at a time.
func (c *Controller) Send(ctx context.Context, text string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", errors.New("message is empty")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sess == nil {
		return "", errors.New("no active chat; start one first")
	}
	if c.busy {
		return "", errors.New("a reply is still in progress")
	}
	if c.stale {
		rebuilt, err := c.buildSession(ctx, c.botSlug, c.sess.ConversationID())
		if err != nil {
			return "", err
		}
		c.sess, c.stale = rebuilt, false
	}
	id, err := bot.NewID("turn")
	if err != nil {
		return "", err
	}
	turnCtx, stop := context.WithCancel(c.rootCtx)
	c.busy, c.turnID, c.turnStop = true, id, stop
	sess := c.sess

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		start := time.Now()
		res, err := sess.Turn(turnCtx, text)
		canceled := turnCtx.Err() != nil
		stop()

		c.mu.Lock()
		c.busy, c.turnStop = false, nil
		c.mu.Unlock()

		switch {
		case err != nil && canceled:
			c.emit(Event{Type: EventTurnCanceled, TurnID: id})
		case err != nil:
			c.emit(Event{Type: EventTurnError, TurnID: id, Error: err.Error()})
		default:
			u := res.Usage
			c.emit(Event{Type: EventTurnDone, TurnID: id, Reply: res.Reply, Usage: &u, DurationMs: time.Since(start).Milliseconds()})
		}
	}()
	return id, nil
}

// Cancel stops the running turn, if any.
func (c *Controller) Cancel() {
	c.mu.Lock()
	stop := c.turnStop
	c.mu.Unlock()
	if stop != nil {
		stop()
	}
}
