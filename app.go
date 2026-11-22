package main

import (
	"context"
	"log"

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
	ctx     context.Context
	st      *store.Store
	ctl     *desktop.Controller
	dataDir string
	srv     *desktop.Server
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
	a := &App{st: st, dataDir: dataDir}
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

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	if srv, err := desktop.StartServer(desktop.DefaultSocketPath(a.dataDir), a.ctl); err != nil {
		log.Printf("faro socket server disabled: %v", err)
	} else {
		a.srv = srv
	}
}

func (a *App) shutdown(_ context.Context) {
	if a.srv != nil {
		_ = a.srv.Close()
	}
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

// NewChat starts a fresh conversation with the given bot (the UI passes the default "faro" bot unless the user picked another).
func (a *App) NewChat(botSlug string) (desktop.ChatInfo, error) {
	return a.ctl.NewChat(a.ctx, botSlug)
}

// OpenChat loads an existing conversation on demand and returns its replayable event snapshot.
func (a *App) OpenChat(convID string) (desktop.Snapshot, error) {
	return a.ctl.OpenChat(a.ctx, convID)
}

// ListChats returns recent single-bot conversations ordered newest first.
func (a *App) ListChats() ([]desktop.ChatSummary, error) {
	return a.ctl.ListChats(a.ctx)
}

// Send starts a chat turn in convID and returns its turn id; progress arrives on the event channel.
func (a *App) Send(convID, text string) (string, error) {
	return a.ctl.Send(a.ctx, convID, text)
}

// Cancel stops the running turn.
func (a *App) Cancel() { a.ctl.Cancel() }

// RespondApproval answers a pending approval dialog. decision is "allow", "allow_session" or
// "deny". Only the human using the window can call this; it is not an agent tool.
func (a *App) RespondApproval(id, decision string) error { return a.ctl.RespondApproval(id, decision) }
