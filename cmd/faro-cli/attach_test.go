package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SayarB/faro-harness/internal/bot"
	"github.com/SayarB/faro-harness/internal/desktop"
	"github.com/SayarB/faro-harness/internal/llm"
	"github.com/SayarB/faro-harness/internal/store"
)

type cliFakeLLM struct {
	mu      sync.Mutex
	calls   int
	scripts []llm.ChatResponse
}

func (f *cliFakeLLM) Chat(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
	f.mu.Lock()
	i := f.calls
	f.calls++
	f.mu.Unlock()
	resp := f.scripts[i]
	if req.OnDelta != nil && resp.Message.Content != "" {
		req.OnDelta(resp.Message.Content)
	}
	return resp, nil
}

func startTestDesktopHost(t *testing.T, client llm.Client) (*desktop.Controller, *desktop.Client, string) {
	t.Helper()
	t.Setenv("AI_GATEWAY_API_KEY", "")
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	mgr, err := bot.NewManager(st, filepath.Join(dir, "bots"))
	if err != nil {
		t.Fatal(err)
	}
	ctl, err := desktop.New(desktop.Options{
		Store:     st,
		Bots:      mgr,
		DataDir:   dir,
		NewClient: func(string) (llm.Client, error) { return client, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ctl.Shutdown)
	_ = ctl.SaveAPIKey("k")

	sockDir, err := os.MkdirTemp("/tmp", "faro-cli-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sockPath := filepath.Join(sockDir, "run", "faro.sock")
	srv, err := desktop.StartServer(sockPath, ctl)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return ctl, desktop.NewClient(sockPath), sockPath
}

func TestAttachInteractiveApprovalAndAppResolution(t *testing.T) {
	ctl, cli, _ := startTestDesktopHost(t, &cliFakeLLM{scripts: []llm.ChatResponse{
		{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{
			ID: "c1", Name: "deliver_file", Arguments: `{"dest_rel_path":"cli.md","content":"hello"}`,
		}}}},
		{Message: llm.Message{Role: llm.RoleAssistant, Content: "Saved cli.md."}},
		{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{
			ID: "c2", Name: "deliver_file", Arguments: `{"dest_rel_path":"app.md","content":"hello2"}`,
		}}}},
		{Message: llm.Message{Role: llm.RoleAssistant, Content: "Saved app.md."}},
	}})
	ctx := context.Background()
	root := t.TempDir()
	if _, err := ctl.AddWorkspace(ctx, root); err != nil {
		t.Fatal(err)
	}
	chat, err := cli.BotChat(ctx, desktop.DefaultBotSlug)
	if err != nil {
		t.Fatal(err)
	}

	pr, pw := io.Pipe()
	var out, errOut syncBuffer
	done := make(chan error, 1)
	go func() {
		done <- attachInteractive(ctx, cli, chat.ConversationID, pr, &out, &errOut)
	}()

	waitContains := func(buf *syncBuffer, substr string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if strings.Contains(buf.String(), substr) {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %q in:\n%s", substr, buf.String())
	}

	waitContains(&errOut, "Attached to ")

	// Turn 1: send from CLI and approve via typed "y\n".
	_, _ = io.WriteString(pw, "write cli.md\n")
	waitContains(&errOut, "Allow? [y]es")
	_, _ = io.WriteString(pw, "y\n")
	waitContains(&out, "Saved cli.md.")

	// Turn 2: send from host/app and answer approval from host -> CLI prints [answered in the app].
	snapStream, s, err := cli.Attach(ctx, chat.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	_ = snapStream
	defer s.Close()

	if _, err := ctl.Send(ctx, chat.ConversationID, "write app.md"); err != nil {
		t.Fatal(err)
	}
	waitContains(&errOut, `app.md`)
	// Pull the approval id from our observer stream and answer via controller (simulating UI).
	var apprID string
	deadline := time.After(5 * time.Second)
	for apprID == "" {
		select {
		case ev := <-s.Events():
			if ev.Type == desktop.EventApprovalRequest {
				apprID = ev.ApprovalID
			}
		case <-deadline:
			t.Fatal("timed out waiting for approval id")
		}
	}
	if err := ctl.RespondApproval(apprID, desktop.DecisionAllow); err != nil {
		t.Fatal(err)
	}
	waitContains(&errOut, "[answered in the app]")
	waitContains(&out, "Saved app.md.")

	_, _ = io.WriteString(pw, "/exit\n")
	_ = pw.Close()
	if err := <-done; err != nil {
		t.Fatalf("attachInteractive err=%v", err)
	}
}

func TestSendOneShotPrintsWaitingForAppApprovalAndExitsOnTurnDone(t *testing.T) {
	ctl, cli, _ := startTestDesktopHost(t, &cliFakeLLM{scripts: []llm.ChatResponse{
		{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{
			ID: "c1", Name: "deliver_file", Arguments: `{"dest_rel_path":"oneshot.md","content":"hi"}`,
		}}}},
		{Message: llm.Message{Role: llm.RoleAssistant, Content: "All set."}},
	}})
	ctx := context.Background()
	root := t.TempDir()
	if _, err := ctl.AddWorkspace(ctx, root); err != nil {
		t.Fatal(err)
	}
	chat, err := cli.BotChat(ctx, desktop.DefaultBotSlug)
	if err != nil {
		t.Fatal(err)
	}

	_, obs, err := cli.Attach(ctx, chat.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	defer obs.Close()

	var out, errOut syncBuffer
	done := make(chan error, 1)
	go func() {
		done <- sendOneShot(ctx, cli, chat.ConversationID, "make oneshot", &out, &errOut)
	}()

	// Wait for approval request on observer and approve via controller.
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-obs.Events():
			if ev.Type == desktop.EventApprovalRequest {
				if err := ctl.RespondApproval(ev.ApprovalID, desktop.DecisionAllow); err != nil {
					t.Fatal(err)
				}
				goto waitDone
			}
		case <-deadline:
			t.Fatal("timed out waiting for approval request")
		}
	}
waitDone:
	if err := <-done; err != nil {
		t.Fatalf("sendOneShot err=%v", err)
	}
	if !strings.Contains(out.String(), "All set.") {
		t.Fatalf("out=%q", out.String())
	}
	if !strings.Contains(errOut.String(), "answer this request in the Faro desktop app") {
		t.Fatalf("errOut=%q", errOut.String())
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
