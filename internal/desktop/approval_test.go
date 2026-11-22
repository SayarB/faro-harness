package desktop

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SayarB/faro-harness/internal/llm"
)

func toolCall(id, name, args string) llm.ChatResponse {
	return llm.ChatResponse{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: id, Name: name, Arguments: args}}}}
}

func say(text string) llm.ChatResponse {
	return llm.ChatResponse{Message: llm.Message{Role: llm.RoleAssistant, Content: text}}
}

const deliverArgs = `{"dest_rel_path":"%s","content":"# Questionnaire\nHello"}`

func deliver(id, name string) llm.ChatResponse {
	return toolCall(id, "deliver_file", strings.Replace(deliverArgs, "%s", name, 1))
}

// startChat adds a folder, opens a chat and sends one message. It returns the folder root.
func startChat(t *testing.T, h *harness, botSlug string) string {
	t.Helper()
	_ = h.c.SaveAPIKey("k")
	ws := h.addFolder(t)
	if _, err := h.c.NewChat(context.Background(), botSlug); err != nil {
		t.Fatal(err)
	}
	if _, err := h.c.Send(context.Background(), "create a document out of this"); err != nil {
		t.Fatal(err)
	}
	return ws.Path
}

func (h *harness) nextApproval(t *testing.T) Event {
	t.Helper()
	evs := h.waitFor(t, func(e Event) bool { return e.Type == EventApprovalRequest })
	return evs[len(evs)-1]
}

func toolResultFor(evs []Event, id string) string {
	for _, e := range evs {
		if e.Type == EventToolResult && e.ToolCallID == id {
			return e.ToolResult
		}
	}
	return ""
}

func TestApprovalAllowCreatesTheFile(t *testing.T) {
	h := newHarness(t, &fakeClient{scripts: []llm.ChatResponse{deliver("c1", "Questionnaire.md"), say("Saved.")}})
	root := startChat(t, h, DefaultBotSlug)

	req := h.nextApproval(t)
	if req.Kind != "file_create" || req.DestRelPath != "Questionnaire.md" || req.SizeBytes == 0 ||
		!strings.Contains(req.Preview, "# Questionnaire") || req.WorkspacePath != root || req.ApprovalID == "" || req.WorkspaceName == "" || req.Sandboxed {
		t.Fatalf("request=%+v", req)
	}
	if _, err := os.Stat(filepath.Join(root, "Questionnaire.md")); err == nil {
		t.Fatal("file must not exist before approval")
	}
	if err := h.c.RespondApproval(req.ApprovalID, DecisionAllow); err != nil {
		t.Fatal(err)
	}
	evs := h.waitFor(t, isTurnEnd)
	if evs[len(evs)-1].Type != EventTurnDone {
		t.Fatalf("last=%+v", evs[len(evs)-1])
	}
	if _, err := os.Stat(filepath.Join(root, "Questionnaire.md")); err != nil {
		t.Fatalf("file not created: %v", err)
	}
	var resolved Event
	for _, e := range evs {
		if e.Type == EventApprovalResolved {
			resolved = e
		}
	}
	if resolved.ApprovalID != req.ApprovalID || resolved.Decision != DecisionAllow {
		t.Fatalf("resolved=%+v", resolved)
	}
	if err := h.c.RespondApproval(req.ApprovalID, DecisionAllow); err == nil {
		t.Fatal("answering a resolved request must fail")
	}
}

func TestApprovalDenyWritesNothingAndSaysDenied(t *testing.T) {
	h := newHarness(t, &fakeClient{scripts: []llm.ChatResponse{deliver("c1", "no.md"), say("Okay, not saved.")}})
	root := startChat(t, h, DefaultBotSlug)
	req := h.nextApproval(t)
	if err := h.c.RespondApproval(req.ApprovalID, DecisionDeny); err != nil {
		t.Fatal(err)
	}
	h.waitFor(t, isTurnEnd)
	if got := toolResultFor(h.log, "c1"); !strings.Contains(got, "file delivery denied by user") {
		t.Fatalf("tool result=%q", got)
	}
	if _, err := os.Stat(filepath.Join(root, "no.md")); err == nil {
		t.Fatal("denied file was written")
	}
}

func TestApprovalSessionAllowCoversNextNewFileInThatChatOnly(t *testing.T) {
	h := newHarness(t, &fakeClient{scripts: []llm.ChatResponse{deliver("c1", "a.md"), deliver("c2", "b.md"), say("done")}})
	root := startChat(t, h, DefaultBotSlug)
	req := h.nextApproval(t)
	if err := h.c.RespondApproval(req.ApprovalID, DecisionAllowSession); err != nil {
		t.Fatal(err)
	}
	evs := h.waitFor(t, isTurnEnd)
	for _, e := range evs {
		if e.Type == EventApprovalRequest {
			t.Fatalf("second file must not prompt again: %+v", e)
		}
	}
	for _, f := range []string{"a.md", "b.md"} {
		if _, err := os.Stat(filepath.Join(root, f)); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
}

func TestApprovalOverwriteNeverSessionApproved(t *testing.T) {
	h := newHarness(t, &fakeClient{scripts: []llm.ChatResponse{
		toolCall("c1", "deliver_file", `{"dest_rel_path":"notes.md","content":"new","overwrite":true}`), say("done"),
	}})
	root := startChat(t, h, DefaultBotSlug)
	req := h.nextApproval(t)
	if req.Kind != "file_overwrite" || req.ExistingSizeBytes == 0 {
		t.Fatalf("request=%+v", req)
	}
	// Even if a client sends allow_session for an overwrite, the Gate refuses it.
	_ = h.c.RespondApproval(req.ApprovalID, DecisionAllowSession)
	h.waitFor(t, isTurnEnd)
	if got, _ := os.ReadFile(filepath.Join(root, "notes.md")); string(got) != "# Notes\nhello\n" {
		t.Fatalf("overwrite happened without a plain allow: %q", got)
	}
	if !strings.Contains(toolResultFor(h.log, "c1"), "denied") {
		t.Fatalf("result=%q", toolResultFor(h.log, "c1"))
	}
}

func TestCancelWhilePendingResolvesAsCancelledAndWritesNothing(t *testing.T) {
	h := newHarness(t, &fakeClient{scripts: []llm.ChatResponse{deliver("c1", "x.md"), say("unused")}})
	root := startChat(t, h, DefaultBotSlug)
	req := h.nextApproval(t)
	h.c.Cancel()
	evs := h.waitFor(t, isTurnEnd)
	if evs[len(evs)-1].Type != EventTurnCanceled {
		t.Fatalf("last=%+v", evs[len(evs)-1])
	}
	var resolved *Event
	for i := range evs {
		if evs[i].Type == EventApprovalResolved {
			resolved = &evs[i]
		}
	}
	if resolved == nil || resolved.ApprovalID != req.ApprovalID || resolved.Decision != DecisionCancelled {
		t.Fatalf("resolved=%+v", resolved)
	}
	if _, err := os.Stat(filepath.Join(root, "x.md")); err == nil {
		t.Fatal("cancelled request wrote a file")
	}
	h.c.mu.Lock()
	n := len(h.c.pending)
	h.c.mu.Unlock()
	if n != 0 {
		t.Fatalf("pending approvals left behind: %d", n)
	}
}

func TestShutdownWhilePendingDoesNotBlock(t *testing.T) {
	h := newHarness(t, &fakeClient{scripts: []llm.ChatResponse{deliver("c1", "x.md"), say("unused")}})
	startChat(t, h, DefaultBotSlug)
	h.nextApproval(t)
	done := make(chan struct{})
	go func() { h.c.Shutdown(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown blocked on a pending approval")
	}
}

func TestUnknownDecisionDeniesAndUnknownIDErrors(t *testing.T) {
	h := newHarness(t, &fakeClient{scripts: []llm.ChatResponse{deliver("c1", "x.md"), say("ok")}})
	root := startChat(t, h, DefaultBotSlug)
	if err := h.c.RespondApproval("appr_nope", DecisionAllow); err == nil {
		t.Fatal("unknown id must error")
	}
	req := h.nextApproval(t)
	if err := h.c.RespondApproval(req.ApprovalID, "yes please"); err == nil {
		t.Fatal("unknown decision must error")
	}
	h.waitFor(t, isTurnEnd)
	if _, err := os.Stat(filepath.Join(root, "x.md")); err == nil {
		t.Fatal("unknown decision must deny")
	}
}

func TestUnsandboxedCommandIsNeverSessionApproved(t *testing.T) {
	h := newHarness(t, &fakeClient{scripts: []llm.ChatResponse{
		toolCall("c1", "bot_run_command", `{"command":"echo one"}`),
		toolCall("c2", "bot_run_command", `{"command":"echo two"}`),
		toolCall("c3", "bot_run_command", `{"command":"echo three"}`),
		say("done"),
	}})
	_ = h.c.SaveAPIKey("k")
	b, err := h.c.CreateBot(context.Background(), "Runner", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.c.NewChat(context.Background(), b.Slug); err != nil {
		t.Fatal(err)
	}
	if _, err := h.c.Send(context.Background(), "run things"); err != nil {
		t.Fatal(err)
	}

	r1 := h.nextApproval(t)
	if r1.Kind != "command" || r1.Command != "echo one" || r1.Sandboxed || r1.Cwd == "" {
		t.Fatalf("request=%+v", r1)
	}
	// 'allow for session' on an unsandboxed command is refused by Go...
	_ = h.c.RespondApproval(r1.ApprovalID, DecisionAllowSession)
	// ...so the next command asks again, and so does the one after an allow-once.
	r2 := h.nextApproval(t)
	if r2.Command != "echo two" {
		t.Fatalf("second request=%+v", r2)
	}
	_ = h.c.RespondApproval(r2.ApprovalID, DecisionAllow)
	r3 := h.nextApproval(t)
	if r3.Command != "echo three" {
		t.Fatalf("third request=%+v", r3)
	}
	_ = h.c.RespondApproval(r3.ApprovalID, DecisionDeny)
	h.waitFor(t, isTurnEnd)
	if got := toolResultFor(h.log, "c1"); !strings.Contains(got, "denied") {
		t.Fatalf("c1 result=%q", got)
	}
}

func TestApprovalRequestEscapesControlCharacters(t *testing.T) {
	h := newHarness(t, &fakeClient{scripts: []llm.ChatResponse{
		toolCall("c1", "bot_run_command", `{"command":"echo ok\u001b[2J\r\nrm -rf ~ \u202e"}`), say("done"),
	}})
	_ = h.c.SaveAPIKey("k")
	b, _ := h.c.CreateBot(context.Background(), "Esc", "")
	_, _ = h.c.NewChat(context.Background(), b.Slug)
	_, _ = h.c.Send(context.Background(), "go")
	req := h.nextApproval(t)
	if strings.ContainsAny(req.Command, "\x1b\r\n\u202e") || !strings.Contains(req.Command, `\u001b[2J\r\n`) || !strings.Contains(req.Command, `\u202e`) {
		t.Fatalf("command not escaped: %q", req.Command)
	}
	_ = h.c.RespondApproval(req.ApprovalID, DecisionDeny)
	h.waitFor(t, isTurnEnd)
}

// A plain chat (default bot) can produce a binary document: it builds the file in scratch/ with
// bot_run_command, then promotes it with deliver_file scratch_path. Each step asks for approval.
// The command is faked with printf; the real flow would run python-docx.
func TestDefaultBotWordRequestRunsCommandThenDeliversScratchFile(t *testing.T) {
	h := newHarness(t, &fakeClient{scripts: []llm.ChatResponse{
		toolCall("c1", "bot_run_command", `{"command":"mkdir -p out && printf 'PK-fake-docx' > out/report.docx"}`),
		toolCall("c2", "deliver_file", `{"dest_rel_path":"report.docx","scratch_path":"scratch/out/report.docx"}`),
		say("Saved report.docx."),
	}})
	root := startChat(t, h, DefaultBotSlug)

	cmd := h.nextApproval(t)
	if cmd.Kind != "command" || cmd.Sandboxed || cmd.BotName != "Faro" || !strings.Contains(cmd.Command, "report.docx") {
		t.Fatalf("command request=%+v", cmd)
	}
	if err := h.c.RespondApproval(cmd.ApprovalID, DecisionAllow); err != nil {
		t.Fatal(err)
	}
	file := h.nextApproval(t)
	if file.Kind != "file_create" || file.DestRelPath != "report.docx" || file.SizeBytes != int64(len("PK-fake-docx")) {
		t.Fatalf("delivery request=%+v", file)
	}
	if _, err := os.Stat(filepath.Join(root, "report.docx")); err == nil {
		t.Fatal("file must not exist before the delivery is approved")
	}
	if err := h.c.RespondApproval(file.ApprovalID, DecisionAllow); err != nil {
		t.Fatal(err)
	}
	evs := h.waitFor(t, isTurnEnd)
	if evs[len(evs)-1].Type != EventTurnDone {
		t.Fatalf("last=%+v", evs[len(evs)-1])
	}
	got, err := os.ReadFile(filepath.Join(root, "report.docx"))
	if err != nil || string(got) != "PK-fake-docx" {
		t.Fatalf("delivered file: %q err=%v", got, err)
	}
	// The order matters: the command ran first, then the delivery.
	var order []string
	for _, e := range h.log {
		if e.Type == EventToolCall {
			order = append(order, e.ToolName)
		}
	}
	if strings.Join(order, ",") != "bot_run_command,deliver_file" {
		t.Fatalf("tool order=%v", order)
	}
}
