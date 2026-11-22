// Package approval holds the human-approval policy shared by the CLI and the desktop app:
// which requests are auto-approved by flags, what is remembered for the session, and the rule
// that an unsandboxed command can never be approved for a whole session. How a human is asked
// (terminal prompt, dialog) is injected as Ask.
package approval

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/SayarB/faro-harness/internal/tools"
)

// Choice is what a human answered when asked.
type Choice int

const (
	// ChoiceDeny refuses the action. It is the zero value so an unset answer fails closed.
	ChoiceDeny Choice = iota
	// ChoiceAllow permits this one action.
	ChoiceAllow
	// ChoiceSession permits this action and remembers it where the Gate allows that (see Approve).
	ChoiceSession
)

// Ask puts a request in front of a human. It must return ChoiceDeny (or an error) when the
// human cannot answer, for example when ctx is cancelled.
type Ask func(ctx context.Context, req tools.ApprovalRequest) (Choice, error)

// Config is the policy for a Gate.
type Config struct {
	AllowCommands   bool // auto-approve every command, sandboxed or not (explicit opt-in flag)
	AllowWrites     bool // auto-approve new files
	AllowOverwrites bool // auto-approve overwrites (the tools layer backs the original up first)
	// Ask is nil in non-interactive mode: anything not auto-approved is then denied with
	// tools.ErrApprovalNonInteractive.
	Ask Ask
}

// Gate decides approval requests from policy and session memory, asking a human only when needed.
// Safe for concurrent use.
type Gate struct {
	cfg Config

	mu                 sync.Mutex
	writeWorkspaces    map[int64]bool // workspaces where the human allowed new files for the session
	cmdSessionApproved bool           // a sandboxed command was approved for the session
}

// NewGate creates a Gate.
func NewGate(cfg Config) *Gate {
	return &Gate{cfg: cfg, writeWorkspaces: map[int64]bool{}}
}

// Approve implements tools.Approver.
//
// Hard rules enforced here regardless of what Ask returns:
//   - Session approval of new files is per workspace; it never covers overwrites.
//   - An overwrite is only ever approved once (ChoiceAllow).
//   - A command is only remembered for the session when req.Sandboxed. An unsandboxed command
//     answered with ChoiceSession is denied and is asked again every time.
func (g *Gate) Approve(ctx context.Context, req tools.ApprovalRequest) (tools.ApprovalDecision, error) {
	switch req.Kind {
	case tools.ApprovalKindFileCreate:
		if g.cfg.AllowWrites || g.writeApproved(req.WorkspaceID) {
			return tools.ApprovalAllowSession, nil
		}
		choice, err := g.ask(ctx, req)
		if err != nil {
			return tools.ApprovalDeny, err
		}
		switch choice {
		case ChoiceAllow:
			return tools.ApprovalAllow, nil
		case ChoiceSession:
			g.mu.Lock()
			g.writeWorkspaces[req.WorkspaceID] = true
			g.mu.Unlock()
			return tools.ApprovalAllowSession, nil
		}
		return tools.ApprovalDeny, nil

	case tools.ApprovalKindFileOverwrite:
		if g.cfg.AllowOverwrites {
			return tools.ApprovalAllow, nil
		}
		choice, err := g.ask(ctx, req)
		if err != nil {
			return tools.ApprovalDeny, err
		}
		if choice == ChoiceAllow {
			return tools.ApprovalAllow, nil
		}
		return tools.ApprovalDeny, nil

	default: // tools.ApprovalKindCommand and anything unknown: treated as a command (fail closed)
		if g.cfg.AllowCommands {
			return tools.ApprovalAllowSession, nil
		}
		if req.Sandboxed && g.commandApproved() {
			return tools.ApprovalAllowSession, nil
		}
		choice, err := g.ask(ctx, req)
		if err != nil {
			return tools.ApprovalDeny, err
		}
		switch choice {
		case ChoiceAllow:
			return tools.ApprovalAllow, nil
		case ChoiceSession:
			if !req.Sandboxed {
				return tools.ApprovalDeny, nil
			}
			g.mu.Lock()
			g.cmdSessionApproved = true
			g.mu.Unlock()
			return tools.ApprovalAllowSession, nil
		}
		return tools.ApprovalDeny, nil
	}
}

func (g *Gate) ask(ctx context.Context, req tools.ApprovalRequest) (Choice, error) {
	if g.cfg.Ask == nil {
		return ChoiceDeny, tools.ErrApprovalNonInteractive
	}
	return g.cfg.Ask(ctx, req)
}

func (g *Gate) writeApproved(workspaceID int64) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.writeWorkspaces[workspaceID]
}

func (g *Gate) commandApproved() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.cmdSessionApproved
}

// EscapeControl escapes control characters (including ANSI ESC, CR, LF, C1, and Unicode bidi
// overrides) so a model-authored command or path cannot spoof or redraw an approval prompt, in a
// terminal or in the desktop dialog.
func EscapeControl(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) ||
			r == 0x061C || r == 0x200E || r == 0x200F ||
			(r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069):
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
