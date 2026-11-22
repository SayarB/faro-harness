package desktop

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SayarB/faro-harness/internal/llm"
)

func tempSocketPath(t *testing.T) string {
	t.Helper()
	// Use /tmp so the Unix socket path stays well below the 103-byte sun_path limit
	// regardless of how long t.TempDir() is in the test runner.
	dir, err := os.MkdirTemp("/tmp", "faro-sock-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "run", "faro.sock")
}

func TestSocketRoundTripAndApproval(t *testing.T) {
	h := newHarness(t, &fakeClient{scripts: []llm.ChatResponse{
		deliver("c1", "via-sock.md"),
		say("Saved via socket."),
	}})
	ctx := context.Background()
	_ = h.c.SaveAPIKey("k")
	ws := h.addFolder(t)

	sockPath := tempSocketPath(t)
	srv, err := StartServer(sockPath, h.c)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	// Directory mode must be 0700.
	fi, err := os.Stat(filepath.Dir(sockPath))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("socket dir perm=%o, want 0700", fi.Mode().Perm())
	}

	cli := NewClient(sockPath)

	// new_chat
	info, err := cli.NewChat(ctx, DefaultBotSlug)
	if err != nil || info.ConversationID == "" || info.BotSlug != DefaultBotSlug {
		t.Fatalf("NewChat=%+v err=%v", info, err)
	}

	// list_chats
	list, err := cli.ListChats(ctx)
	if err != nil || len(list) != 1 || list[0].ID != info.ConversationID {
		t.Fatalf("ListChats=%+v err=%v", list, err)
	}

	// attach before send
	snap, stream, err := cli.Attach(ctx, info.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if snap.Busy || snap.ConversationID != info.ConversationID || snap.BotSlug != DefaultBotSlug {
		t.Fatalf("snap=%+v", snap)
	}

	// send
	turnID, err := cli.Send(ctx, info.ConversationID, "save a file")
	if err != nil || turnID == "" {
		t.Fatalf("Send=(%q, %v)", turnID, err)
	}

	// Wait for approval.request on the socket stream, answer via respond_approval, wait for turn.done.
	var sawStarted, sawResolved, sawDone bool
	deadline := time.After(5 * time.Second)
	for !sawDone {
		select {
		case ev, ok := <-stream.Events():
			if !ok {
				t.Fatalf("stream closed early: err=%v", stream.Err())
			}
			switch ev.Type {
			case EventTurnStarted:
				if ev.TurnID != turnID || ev.Text != "save a file" {
					t.Fatalf("turn.started=%+v", ev)
				}
				sawStarted = true
			case EventApprovalRequest:
				if err := cli.RespondApproval(ctx, ev.ApprovalID, DecisionAllow); err != nil {
					t.Fatalf("RespondApproval: %v", err)
				}
				if err := cli.RespondApproval(ctx, ev.ApprovalID, DecisionDeny); err == nil || !strings.Contains(err.Error(), "no longer pending") {
					t.Fatalf("second RespondApproval err=%v", err)
				}
			case EventApprovalResolved:
				if ev.Decision != DecisionAllow {
					t.Fatalf("resolved=%+v", ev)
				}
				sawResolved = true
			case EventTurnDone:
				if ev.Reply != "Saved via socket." {
					t.Fatalf("turn.done=%+v", ev)
				}
				sawDone = true
			}
		case <-deadline:
			t.Fatal("timed out waiting for socket stream events")
		}
	}
	if !sawStarted || !sawResolved {
		t.Fatalf("missing events: started=%v resolved=%v", sawStarted, sawResolved)
	}
	if _, err := os.Stat(filepath.Join(ws.Path, "via-sock.md")); err != nil {
		t.Fatalf("delivered file missing: %v", err)
	}

	// UI callback also saw the same turn.done.
	h.waitFor(t, isTurnEnd)
}

func TestServerCleansStaleSocketAndRejectsLongPath(t *testing.T) {
	h := newHarness(t, &fakeClient{})

	// Too-long path rejected with a clear error.
	longPath := filepath.Join("/tmp", strings.Repeat("a", 110), "faro.sock")
	if _, err := StartServer(longPath, h.c); err == nil || !strings.Contains(err.Error(), "too long") {
		t.Fatalf("expected too-long error, got %v", err)
	}

	// Stale socket file (listener closed without removing file) is replaced cleanly.
	sockPath := tempSocketPath(t)
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate crash: unlink disabled by creating a dangling socket or closing after copying?
	// Go's UnixListener removes on Close by default unless SetUnlinkOnClose(false).
	if uln, ok := ln.(*net.UnixListener); ok {
		uln.SetUnlinkOnClose(false)
	}
	_ = ln.Close()
	if _, err := os.Lstat(sockPath); err != nil {
		t.Fatalf("expected stale socket file to exist: %v", err)
	}

	srv, err := StartServer(sockPath, h.c)
	if err != nil {
		t.Fatalf("StartServer over stale socket failed: %v", err)
	}
	defer srv.Close()

	// Client errors clearly when host is not running.
	downCli := NewClient(filepath.Join(filepath.Dir(sockPath), "missing.sock"))
	if _, err := downCli.ListChats(context.Background()); !errors.Is(err, ErrHostNotRunning) {
		t.Fatalf("expected ErrHostNotRunning, got %v", err)
	}
}
