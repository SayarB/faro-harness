package approval

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SayarB/faro-harness/internal/tools"
)

// scripted returns an Ask that replays answers and counts calls.
func scripted(answers ...Choice) (Ask, *int) {
	n := 0
	return func(context.Context, tools.ApprovalRequest) (Choice, error) {
		c := answers[n]
		n++
		return c, nil
	}, &n
}

func cmdReq(sandboxed bool) tools.ApprovalRequest {
	return tools.ApprovalRequest{Kind: tools.ApprovalKindCommand, Command: "echo hi", Sandboxed: sandboxed}
}

func TestNonInteractiveFlagsAreSeparate(t *testing.T) {
	ctx := context.Background()
	g := NewGate(Config{AllowWrites: true})
	create := tools.ApprovalRequest{Kind: tools.ApprovalKindFileCreate, WorkspaceID: 1}
	if dec, err := g.Approve(ctx, create); err != nil || dec != tools.ApprovalAllowSession {
		t.Fatalf("AllowWrites must allow create: %v %v", dec, err)
	}
	for _, req := range []tools.ApprovalRequest{
		{Kind: tools.ApprovalKindFileOverwrite, WorkspaceID: 1},
		cmdReq(true),
		cmdReq(false),
	} {
		if dec, err := g.Approve(ctx, req); !errors.Is(err, tools.ErrApprovalNonInteractive) || dec != tools.ApprovalDeny {
			t.Fatalf("AllowWrites must not cover %s: %v %v", req.Kind, dec, err)
		}
	}

	g = NewGate(Config{AllowOverwrites: true})
	if dec, err := g.Approve(ctx, tools.ApprovalRequest{Kind: tools.ApprovalKindFileOverwrite}); err != nil || dec != tools.ApprovalAllow {
		t.Fatalf("AllowOverwrites must allow overwrite once: %v %v", dec, err)
	}
	if dec, err := g.Approve(ctx, create); !errors.Is(err, tools.ErrApprovalNonInteractive) || dec != tools.ApprovalDeny {
		t.Fatalf("AllowOverwrites must not cover create: %v %v", dec, err)
	}

	g = NewGate(Config{AllowCommands: true})
	for _, sandboxed := range []bool{true, false} {
		if dec, err := g.Approve(ctx, cmdReq(sandboxed)); err != nil || dec != tools.ApprovalAllowSession {
			t.Fatalf("AllowCommands must allow commands (sandboxed=%v): %v %v", sandboxed, dec, err)
		}
	}
}

func TestFileCreateSessionIsPerWorkspaceAndNeverCoversOverwrite(t *testing.T) {
	ctx := context.Background()
	ask, calls := scripted(ChoiceSession, ChoiceDeny, ChoiceSession, ChoiceAllow)
	g := NewGate(Config{Ask: ask})

	create := func(ws int64) tools.ApprovalRequest {
		return tools.ApprovalRequest{Kind: tools.ApprovalKindFileCreate, WorkspaceID: ws}
	}
	if dec, _ := g.Approve(ctx, create(1)); dec != tools.ApprovalAllowSession {
		t.Fatalf("ws1 first create: %v", dec)
	}
	if dec, _ := g.Approve(ctx, create(1)); dec != tools.ApprovalAllowSession || *calls != 1 {
		t.Fatalf("ws1 second create must not ask again: dec=%v calls=%d", dec, *calls)
	}
	if dec, _ := g.Approve(ctx, create(2)); dec != tools.ApprovalDeny || *calls != 2 {
		t.Fatalf("ws2 must ask independently: dec=%v calls=%d", dec, *calls)
	}
	// Session answer on an overwrite is not honoured: denied, and never remembered.
	over := tools.ApprovalRequest{Kind: tools.ApprovalKindFileOverwrite, WorkspaceID: 1}
	if dec, _ := g.Approve(ctx, over); dec != tools.ApprovalDeny || *calls != 3 {
		t.Fatalf("overwrite answered 'session' must deny: dec=%v calls=%d", dec, *calls)
	}
	if dec, _ := g.Approve(ctx, over); dec != tools.ApprovalAllow || *calls != 4 {
		t.Fatalf("overwrite must ask every time: dec=%v calls=%d", dec, *calls)
	}
}

func TestUnsandboxedCommandIsNeverSessionApproved(t *testing.T) {
	ctx := context.Background()
	ask, calls := scripted(ChoiceSession, ChoiceAllow, ChoiceAllow, ChoiceSession, ChoiceDeny)
	g := NewGate(Config{Ask: ask})

	// 'session' on an unsandboxed command is denied by the Gate, not just hidden by a UI.
	if dec, err := g.Approve(ctx, cmdReq(false)); err != nil || dec != tools.ApprovalDeny {
		t.Fatalf("unsandboxed session answer must deny: %v %v", dec, err)
	}
	// 'allow once' works and is not remembered.
	for i := 2; i <= 3; i++ {
		if dec, _ := g.Approve(ctx, cmdReq(false)); dec != tools.ApprovalAllow || *calls != i {
			t.Fatalf("unsandboxed once #%d: dec=%v calls=%d", i, dec, *calls)
		}
	}
	// A sandboxed session grant is remembered for sandboxed commands...
	if dec, _ := g.Approve(ctx, cmdReq(true)); dec != tools.ApprovalAllowSession || *calls != 4 {
		t.Fatalf("sandboxed session grant: dec=%v calls=%d", dec, *calls)
	}
	if dec, _ := g.Approve(ctx, cmdReq(true)); dec != tools.ApprovalAllowSession || *calls != 4 {
		t.Fatalf("sandboxed second call must not ask: dec=%v calls=%d", dec, *calls)
	}
	// ...but never extends to an unsandboxed one.
	if dec, _ := g.Approve(ctx, cmdReq(false)); dec != tools.ApprovalDeny || *calls != 5 {
		t.Fatalf("unsandboxed after sandboxed grant must still ask: dec=%v calls=%d", dec, *calls)
	}
}

func TestAskErrorDeniesAndPropagates(t *testing.T) {
	boom := errors.New("boom")
	g := NewGate(Config{Ask: func(context.Context, tools.ApprovalRequest) (Choice, error) { return ChoiceAllow, boom }})
	for _, kind := range []tools.ApprovalKind{tools.ApprovalKindFileCreate, tools.ApprovalKindFileOverwrite, tools.ApprovalKindCommand} {
		dec, err := g.Approve(context.Background(), tools.ApprovalRequest{Kind: kind})
		if !errors.Is(err, boom) || dec != tools.ApprovalDeny {
			t.Fatalf("%s: ask error must deny and propagate: %v %v", kind, dec, err)
		}
	}
}

func TestEscapeControl(t *testing.T) {
	got := EscapeControl("echo \x1b[2Jfake\r\nrm\t\u202e-rf \u0085/\u2066")
	if strings.ContainsAny(got, "\x1b\r\n\t\u202e\u0085\u2066") {
		t.Fatalf("control or bidi characters leaked: %q", got)
	}
	for _, want := range []string{`\u001b[2Jfake`, `\r\n`, `\t`, `\u202e`, `\u0085`, `\u2066`} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
	if EscapeControl("plain ü text") != "plain ü text" {
		t.Fatal("printable text must be unchanged")
	}
}
