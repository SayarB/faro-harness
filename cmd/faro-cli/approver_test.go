package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SayarB/faro-harness/internal/tools"
)

func TestCLIApproverNonInteractiveFlagsAndSeparation(t *testing.T) {
	ctx := context.Background()

	// --allow-writes in non-interactive mode allows NEW files only, NOT overwrites and NOT commands.
	var buf bytes.Buffer
	app := newCLIApprover(cliApproverConfig{
		interactive:   false,
		allowWrites:   true,
		allowCommands: false,
		promptOut:     &buf,
	})

	dec, err := app(ctx, tools.ApprovalRequest{
		Kind:        tools.ApprovalKindFileCreate,
		WorkspaceID: 1,
		DestRelPath: "report.xlsx",
	})
	if err != nil || dec != tools.ApprovalAllowSession {
		t.Fatalf("expected --allow-writes to allow FileCreate, got dec=%v err=%v", dec, err)
	}

	dec, err = app(ctx, tools.ApprovalRequest{
		Kind:          tools.ApprovalKindFileOverwrite,
		WorkspaceID:   1,
		DestRelPath:   "report.xlsx",
		WillOverwrite: true,
	})
	if !errors.Is(err, tools.ErrApprovalNonInteractive) || dec != tools.ApprovalDeny {
		t.Fatalf("expected --allow-writes to DENY overwrite with ErrApprovalNonInteractive, got dec=%v err=%v", dec, err)
	}

	dec, err = app(ctx, tools.ApprovalRequest{
		Kind:    tools.ApprovalKindCommand,
		Command: "echo hi",
	})
	if !errors.Is(err, tools.ErrApprovalNonInteractive) || dec != tools.ApprovalDeny {
		t.Fatalf("expected --allow-writes to DENY command with ErrApprovalNonInteractive, got dec=%v err=%v", dec, err)
	}

	// --allow-overwrites explicitly permits overwrites in non-interactive mode (but still not new files or commands).
	appOver := newCLIApprover(cliApproverConfig{
		interactive:     false,
		allowOverwrites: true,
		promptOut:       &buf,
	})
	dec, err = appOver(ctx, tools.ApprovalRequest{
		Kind:          tools.ApprovalKindFileOverwrite,
		WorkspaceID:   1,
		DestRelPath:   "report.xlsx",
		WillOverwrite: true,
	})
	if err != nil || dec != tools.ApprovalAllow {
		t.Fatalf("expected --allow-overwrites to allow FileOverwrite, got dec=%v err=%v", dec, err)
	}
	dec, err = appOver(ctx, tools.ApprovalRequest{
		Kind:        tools.ApprovalKindFileCreate,
		WorkspaceID: 1,
		DestRelPath: "new.xlsx",
	})
	if !errors.Is(err, tools.ErrApprovalNonInteractive) || dec != tools.ApprovalDeny {
		t.Fatalf("expected --allow-overwrites alone to deny FileCreate, got dec=%v err=%v", dec, err)
	}
}

func TestCLIApproverInteractivePerWorkspaceScopeAndSanitization(t *testing.T) {
	ctx := context.Background()
	answers := []string{"a", "n", "a", "y"}
	idx := 0
	var out bytes.Buffer

	app := newCLIApprover(cliApproverConfig{
		interactive: true,
		promptOut:   &out,
		readLine: func(_ context.Context) (string, error) {
			a := answers[idx]
			idx++
			return a, nil
		},
	})

	// 1. Grant session new-file approval on workspace 1.
	dec, err := app(ctx, tools.ApprovalRequest{
		Kind:          tools.ApprovalKindFileCreate,
		BotSlug:       "news-reporter",
		WorkspaceID:   1,
		WorkspaceName: "faro",
		WorkspaceRoot: "/home/user/faro",
		SourcePath:    "(inline text)",
		DestRelPath:   "a.md",
		SizeBytes:     10,
		Preview:       "# Hello",
	})
	if err != nil || dec != tools.ApprovalAllowSession {
		t.Fatalf("ws1 create 1: dec=%v err=%v", dec, err)
	}
	if !strings.Contains(out.String(), `"# Hello"`) {
		t.Fatalf("expected preview in prompt output, got:\n%s", out.String())
	}

	// 2. Second new file in workspace 1 must NOT prompt again.
	dec, err = app(ctx, tools.ApprovalRequest{
		Kind:        tools.ApprovalKindFileCreate,
		WorkspaceID: 1,
		DestRelPath: "b.md",
	})
	if err != nil || dec != tools.ApprovalAllowSession || idx != 1 {
		t.Fatalf("ws1 create 2 should not re-prompt: dec=%v err=%v idx=%d", dec, err, idx)
	}

	// 3. New file in a DIFFERENT workspace (id=2, e.g. mounted via /add-folder) MUST prompt!
	dec, err = app(ctx, tools.ApprovalRequest{
		Kind:        tools.ApprovalKindFileCreate,
		WorkspaceID: 2,
		DestRelPath: "other.md",
	})
	if err != nil || dec != tools.ApprovalDeny || idx != 2 {
		t.Fatalf("ws2 create must prompt independently (answered 'n'): dec=%v err=%v idx=%d", dec, err, idx)
	}

	// 4. Overwrite on workspace 1 MUST still prompt, and typing 'a' must NOT approve it!
	out.Reset()
	dec, err = app(ctx, tools.ApprovalRequest{
		Kind:              tools.ApprovalKindFileOverwrite,
		BotSlug:           "news-reporter",
		WorkspaceID:       1,
		WorkspaceName:     "faro",
		WorkspaceRoot:     "/home/user/faro",
		SourcePath:        "scratch/out/a.xlsx",
		DestRelPath:       "a.xlsx",
		SizeBytes:         200,
		ExistingSizeBytes: 150,
		WillOverwrite:     true,
	})
	if err != nil || dec != tools.ApprovalDeny || idx != 3 {
		t.Fatalf("overwrite answering 'a' must deny: dec=%v err=%v idx=%d", dec, err, idx)
	}
	if !strings.Contains(out.String(), "existing: 150 bytes") {
		t.Fatalf("expected existing size in overwrite prompt, got:\n%s", out.String())
	}

	// 5. Command containing ANSI escape sequences is escaped before printing.
	out.Reset()
	dec, err = app(ctx, tools.ApprovalRequest{
		Kind:    tools.ApprovalKindCommand,
		BotSlug: "news-reporter",
		Cwd:     "scratch/",
		Command: "echo \x1b[2Jfake\r\nrm -rf /",
	})
	if err != nil || dec != tools.ApprovalAllow || idx != 4 {
		t.Fatalf("command prompt: dec=%v err=%v idx=%d", dec, err, idx)
	}
	if strings.Contains(out.String(), "\x1b[") {
		t.Fatalf("raw ANSI escape leaked into terminal prompt:\n%q", out.String())
	}
	if !strings.Contains(out.String(), `\u001b[2Jfake\r\n`) {
		t.Fatalf("expected escaped control chars in command prompt, got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "WARNING: runs unsandboxed") {
		t.Fatalf("expected unsandboxed warning in command prompt, got:\n%s", out.String())
	}
}

func TestCLIApproverCommandSandboxGating(t *testing.T) {
	ctx := context.Background()

	// 1. Unsandboxed command prompt warns, omits [a]llow for session, denies 'a', and prompts every time.
	answers := []string{"a", "y", "y", "a", "n"}
	idx := 0
	var out bytes.Buffer
	app := newCLIApprover(cliApproverConfig{
		interactive: true,
		promptOut:   &out,
		readLine: func(_ context.Context) (string, error) {
			a := answers[idx]
			idx++
			return a, nil
		},
	})

	dec, err := app(ctx, tools.ApprovalRequest{
		Kind:      tools.ApprovalKindCommand,
		BotSlug:   "ledger",
		Cwd:       "scratch/",
		Command:   "echo one",
		Sandboxed: false,
	})
	if err != nil || dec != tools.ApprovalDeny || idx != 1 {
		t.Fatalf("unsandboxed 'a' must deny: dec=%v err=%v idx=%d", dec, err, idx)
	}
	if !strings.Contains(out.String(), "WARNING: runs unsandboxed") || strings.Contains(out.String(), "[a]llow for session") {
		t.Fatalf("unexpected unsandboxed prompt:\n%s", out.String())
	}

	// Answering 'y' allows once and does not suppress the next unsandboxed prompt.
	for wantIdx := 2; wantIdx <= 3; wantIdx++ {
		dec, err = app(ctx, tools.ApprovalRequest{
			Kind:      tools.ApprovalKindCommand,
			BotSlug:   "ledger",
			Cwd:       "scratch/",
			Command:   "echo repeat",
			Sandboxed: false,
		})
		if err != nil || dec != tools.ApprovalAllow || idx != wantIdx {
			t.Fatalf("unsandboxed call %d: dec=%v err=%v idx=%d", wantIdx, dec, err, idx)
		}
	}

	// 2. Sandboxed command prompt omits the unsandboxed warning, offers [a]llow for session, and caches it.
	out.Reset()
	dec, err = app(ctx, tools.ApprovalRequest{
		Kind:      tools.ApprovalKindCommand,
		BotSlug:   "ledger",
		Cwd:       "scratch/",
		Command:   "echo safe1",
		Sandboxed: true,
	})
	if err != nil || dec != tools.ApprovalAllowSession || idx != 4 {
		t.Fatalf("sandboxed 'a' must return AllowSession: dec=%v err=%v idx=%d", dec, err, idx)
	}
	if strings.Contains(out.String(), "WARNING: runs unsandboxed") || !strings.Contains(out.String(), "[a]llow for session") {
		t.Fatalf("unexpected sandboxed prompt:\n%s", out.String())
	}

	// Second sandboxed command must not re-prompt.
	dec, err = app(ctx, tools.ApprovalRequest{
		Kind:      tools.ApprovalKindCommand,
		BotSlug:   "ledger",
		Cwd:       "scratch/",
		Command:   "echo safe2",
		Sandboxed: true,
	})
	if err != nil || dec != tools.ApprovalAllowSession || idx != 4 {
		t.Fatalf("second sandboxed call should not re-prompt: dec=%v err=%v idx=%d", dec, err, idx)
	}

	// An unsandboxed command on the same approver must still prompt despite prior sandboxed session approval.
	dec, err = app(ctx, tools.ApprovalRequest{
		Kind:      tools.ApprovalKindCommand,
		BotSlug:   "ledger",
		Cwd:       "scratch/",
		Command:   "echo unsafe_after_safe",
		Sandboxed: false,
	})
	if err != nil || dec != tools.ApprovalDeny || idx != 5 {
		t.Fatalf("unsandboxed call after sandboxed session grant must still prompt: dec=%v err=%v idx=%d", dec, err, idx)
	}

	// 3. --allow-commands still auto-approves every call even when unsandboxed & non-interactive.
	appFlag := newCLIApprover(cliApproverConfig{
		interactive:   false,
		allowCommands: true,
		promptOut:     &out,
	})
	for i := 0; i < 2; i++ {
		dec, err = appFlag(ctx, tools.ApprovalRequest{
			Kind:      tools.ApprovalKindCommand,
			Command:   "echo auto",
			Sandboxed: false,
		})
		if err != nil || dec != tools.ApprovalAllowSession {
			t.Fatalf("--allow-commands call %d failed: dec=%v err=%v", i, dec, err)
		}
	}
}
