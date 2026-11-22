package main

import (
	"context"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/SayarB/faro-harness/internal/bot"
	"github.com/SayarB/faro-harness/internal/desktop"
	"github.com/SayarB/faro-harness/internal/store"
)

// eventChannel is the single Wails event name carrying desktop.Event payloads to the UI.
const eventChannel = "faro:event"

// App is the thin Wails shell. All behaviour lives in internal/desktop; this type only adds
// the Wails-specific pieces (native dialogs, event emission) and exposes bound methods.
type App struct {
	ctx context.Context
	st  *store.Store
	ctl *desktop.Controller
}

// NewApp opens the local store and builds the controller.
func NewApp() (*App, error) {
	st, err := store.Open("")
	if err != nil {
		return nil, err
	}
	mgr, err := bot.NewManager(st, "")
	if err != nil {
		return nil, err
	}
	dataDir, err := store.DefaultDataDir()
	if err != nil {
		return nil, err
	}
	a := &App{st: st}
	a.ctl, err = desktop.New(desktop.Options{
		Store:   st,
		Bots:    mgr,
		DataDir: dataDir,
		Emit: func(ev desktop.Event) {
			if a.ctx != nil {
				runtime.EventsEmit(a.ctx, eventChannel, ev)
			}
		},
	})
	if err != nil {
		return nil, err
	}
	return a, nil
}

func (a *App) startup(ctx context.Context) { a.ctx = ctx }

func (a *App) shutdown(_ context.Context) {
	a.ctl.Shutdown()
	_ = a.st.Close()
}

// GetState returns the key/folders/bots snapshot.
func (a *App) GetState() (desktop.State, error) { return a.ctl.State(a.ctx) }

// SaveAPIKey stores the AI Gateway API key on this computer.
func (a *App) SaveAPIKey(key string) error { return a.ctl.SaveAPIKey(key) }

// PickFolder opens the native folder dialog and attaches the chosen folder. It returns nil if
// the user cancelled. Indexing continues in the background and reports index.progress events.
func (a *App) PickFolder() (*desktop.WorkspaceInfo, error) {
	dir, err := runtime.OpenDirectoryDialog(a.ctx, runtime.OpenDialogOptions{
		Title:                "Choose a folder for Faro to work with",
		CanCreateDirectories: true, // shows "New Folder" so people can make a home for Faro right here
	})
	if err != nil || dir == "" {
		return nil, err
	}
	ws, err := a.ctl.AddWorkspace(a.ctx, dir)
	if err != nil {
		return nil, err
	}
	return &ws, nil
}

// Rescan re-indexes an attached folder.
func (a *App) Rescan(workspaceID int64) error { return a.ctl.Rescan(a.ctx, workspaceID) }

// CreateBot creates a bot with a display name and optional instructions.
func (a *App) CreateBot(name, instructions string) (desktop.BotInfo, error) {
	return a.ctl.CreateBot(a.ctx, name, instructions)
}

// NewChat starts a fresh conversation (empty botSlug = plain Faro).
func (a *App) NewChat(botSlug string) (desktop.ChatInfo, error) {
	return a.ctl.NewChat(a.ctx, botSlug)
}

// Send starts a chat turn and returns its turn id; progress arrives on the event channel.
func (a *App) Send(text string) (string, error) { return a.ctl.Send(a.ctx, text) }

// Cancel stops the running turn.
func (a *App) Cancel() { a.ctl.Cancel() }
