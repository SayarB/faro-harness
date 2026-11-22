package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/SayarB/faro-harness/internal/tools"
)

type cliApproverConfig struct {
	interactive     bool
	allowCommands   bool
	allowWrites     bool
	allowOverwrites bool
	promptOut       io.Writer
	readLine        func(ctx context.Context) (string, error)
}

func newCLIApprover(cfg cliApproverConfig) tools.Approver {
	var mu sync.Mutex
	cmdSessionApproved := false
	writeApprovedWorkspaces := make(map[int64]bool)

	out := cfg.promptOut
	if out == nil {
		out = io.Discard
	}

	return func(callCtx context.Context, req tools.ApprovalRequest) (tools.ApprovalDecision, error) {
		actor := req.BotSlug
		if actor == "" {
			actor = "faro"
		}

		switch req.Kind {
		case tools.ApprovalKindFileCreate:
			if cfg.allowWrites {
				return tools.ApprovalAllowSession, nil
			}
			mu.Lock()
			wsApproved := writeApprovedWorkspaces[req.WorkspaceID]
			mu.Unlock()
			if wsApproved {
				return tools.ApprovalAllowSession, nil
			}
			if !cfg.interactive || cfg.readLine == nil {
				fmt.Fprintf(out, "[approval] denied saving %q in non-interactive mode (pass --allow-writes to permit)\n", req.DestRelPath)
				return tools.ApprovalDeny, tools.ErrApprovalNonInteractive
			}
			fmt.Fprintf(out, "\n[approval] %q wants to save a new file in %q (%q):\n  from: %q (%d bytes)\n    to: %q\n",
				actor, req.WorkspaceName, req.WorkspaceRoot, req.SourcePath, req.SizeBytes, req.DestRelPath)
			if req.Preview != "" {
				fmt.Fprintf(out, "  text: %q\n", req.Preview)
			}
			fmt.Fprint(out, "Allow? [y]es / [N]o / [a]llow new files in this folder for session: ")
			ans, err := cfg.readLine(callCtx)
			if err != nil {
				if errors.Is(err, io.EOF) {
					return tools.ApprovalDeny, nil
				}
				if errors.Is(err, context.Canceled) {
					fmt.Fprintln(out)
				}
				return tools.ApprovalDeny, err
			}
			switch strings.ToLower(strings.TrimSpace(ans)) {
			case "y", "yes":
				return tools.ApprovalAllow, nil
			case "a", "all", "session", "allow":
				mu.Lock()
				writeApprovedWorkspaces[req.WorkspaceID] = true
				mu.Unlock()
				return tools.ApprovalAllowSession, nil
			default:
				return tools.ApprovalDeny, nil
			}

		case tools.ApprovalKindFileOverwrite:
			if cfg.allowOverwrites {
				return tools.ApprovalAllow, nil
			}
			if !cfg.interactive || cfg.readLine == nil {
				fmt.Fprintf(out, "[approval] denied overwriting %q in non-interactive mode (pass --allow-overwrites to permit)\n", req.DestRelPath)
				return tools.ApprovalDeny, tools.ErrApprovalNonInteractive
			}
			fmt.Fprintf(out, "\n[approval] %q wants to OVERWRITE an existing file in %q (%q):\n  from: %q (%d bytes)\n    to: %q (existing: %d bytes; will be backed up first)\n",
				actor, req.WorkspaceName, req.WorkspaceRoot, req.SourcePath, req.SizeBytes, req.DestRelPath, req.ExistingSizeBytes)
			if req.Preview != "" {
				fmt.Fprintf(out, "  text: %q\n", req.Preview)
			}
			fmt.Fprint(out, "Allow overwrite? [y]es / [N]o: ")
			ans, err := cfg.readLine(callCtx)
			if err != nil {
				if errors.Is(err, io.EOF) {
					return tools.ApprovalDeny, nil
				}
				if errors.Is(err, context.Canceled) {
					fmt.Fprintln(out)
				}
				return tools.ApprovalDeny, err
			}
			switch strings.ToLower(strings.TrimSpace(ans)) {
			case "y", "yes":
				return tools.ApprovalAllow, nil
			default:
				return tools.ApprovalDeny, nil
			}

		default: // ApprovalKindCommand
			if cfg.allowCommands {
				return tools.ApprovalAllowSession, nil
			}
			if req.Sandboxed {
				mu.Lock()
				approved := cmdSessionApproved
				mu.Unlock()
				if approved {
					return tools.ApprovalAllowSession, nil
				}
			}
			if !cfg.interactive || cfg.readLine == nil {
				fmt.Fprintf(out, "[approval] denied command in non-interactive mode (pass --allow-commands to permit)\n")
				return tools.ApprovalDeny, tools.ErrApprovalNonInteractive
			}
			if req.Sandboxed {
				fmt.Fprintf(out, "\n[approval] %q wants to run a command in %q:\n  $ %s\nAllow? [y]es / [N]o / [a]llow for session: ",
					actor, req.Cwd, sanitizeTerminalCommand(req.Command))
			} else {
				fmt.Fprintf(out, "\n[approval] %q wants to run a command in %q:\n  $ %s\n  WARNING: runs unsandboxed with your full user permissions and network access.\nAllow? [y]es / [N]o: ",
					actor, req.Cwd, sanitizeTerminalCommand(req.Command))
			}
			ans, err := cfg.readLine(callCtx)
			if err != nil {
				if errors.Is(err, io.EOF) {
					return tools.ApprovalDeny, nil
				}
				if errors.Is(err, context.Canceled) {
					fmt.Fprintln(out)
				}
				return tools.ApprovalDeny, err
			}
			switch strings.ToLower(strings.TrimSpace(ans)) {
			case "y", "yes":
				return tools.ApprovalAllow, nil
			case "a", "all", "session", "allow":
				if req.Sandboxed {
					mu.Lock()
					cmdSessionApproved = true
					mu.Unlock()
					return tools.ApprovalAllowSession, nil
				}
				return tools.ApprovalDeny, nil
			default:
				return tools.ApprovalDeny, nil
			}
		}
	}
}

// sanitizeTerminalCommand escapes control characters (including ANSI ESC, CR, LF, C1,
// and Unicode bidi overrides) so a command string cannot spoof or redraw the approval banner.
func sanitizeTerminalCommand(s string) string {
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
