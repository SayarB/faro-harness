package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/SayarB/faro-harness/internal/approval"
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

// newCLIApprover wires the shared approval policy to a terminal prompt. The policy (flags,
// session memory, "unsandboxed commands are never session-approved") lives in internal/approval;
// this file only owns the terminal wording.
func newCLIApprover(cfg cliApproverConfig) tools.Approver {
	out := cfg.promptOut
	if out == nil {
		out = io.Discard
	}

	gateCfg := approval.Config{
		AllowCommands:   cfg.allowCommands,
		AllowWrites:     cfg.allowWrites,
		AllowOverwrites: cfg.allowOverwrites,
	}
	if cfg.interactive && cfg.readLine != nil {
		gateCfg.Ask = func(ctx context.Context, req tools.ApprovalRequest) (approval.Choice, error) {
			return askTerminal(ctx, out, cfg.readLine, req)
		}
	}
	gate := approval.NewGate(gateCfg)

	return func(ctx context.Context, req tools.ApprovalRequest) (tools.ApprovalDecision, error) {
		dec, err := gate.Approve(ctx, req)
		if errors.Is(err, tools.ErrApprovalNonInteractive) {
			switch req.Kind {
			case tools.ApprovalKindFileCreate:
				fmt.Fprintf(out, "[approval] denied saving %q in non-interactive mode (pass --allow-writes to permit)\n", req.DestRelPath)
			case tools.ApprovalKindFileOverwrite:
				fmt.Fprintf(out, "[approval] denied overwriting %q in non-interactive mode (pass --allow-overwrites to permit)\n", req.DestRelPath)
			default:
				fmt.Fprintf(out, "[approval] denied command in non-interactive mode (pass --allow-commands to permit)\n")
			}
		}
		return dec, err
	}
}

// askTerminal prints the request and reads y / n / a. 'a' is always reported as a session
// choice; the Gate decides whether that is honoured (it is not for overwrites or unsandboxed commands).
func askTerminal(ctx context.Context, out io.Writer, readLine func(context.Context) (string, error), req tools.ApprovalRequest) (approval.Choice, error) {
	actor := req.BotSlug
	if actor == "" {
		actor = "faro"
	}

	switch req.Kind {
	case tools.ApprovalKindFileCreate:
		fmt.Fprintf(out, "\n[approval] %q wants to save a new file in %q (%q):\n  from: %q (%d bytes)\n    to: %q\n",
			actor, req.WorkspaceName, req.WorkspaceRoot, req.SourcePath, req.SizeBytes, req.DestRelPath)
		if req.Preview != "" {
			fmt.Fprintf(out, "  text: %q\n", req.Preview)
		}
		fmt.Fprint(out, "Allow? [y]es / [N]o / [a]llow new files in this folder for session: ")
	case tools.ApprovalKindFileOverwrite:
		fmt.Fprintf(out, "\n[approval] %q wants to OVERWRITE an existing file in %q (%q):\n  from: %q (%d bytes)\n    to: %q (existing: %d bytes; will be backed up first)\n",
			actor, req.WorkspaceName, req.WorkspaceRoot, req.SourcePath, req.SizeBytes, req.DestRelPath, req.ExistingSizeBytes)
		if req.Preview != "" {
			fmt.Fprintf(out, "  text: %q\n", req.Preview)
		}
		fmt.Fprint(out, "Allow overwrite? [y]es / [N]o: ")
	default: // command
		if req.Sandboxed {
			fmt.Fprintf(out, "\n[approval] %q wants to run a command in %q:\n  $ %s\nAllow? [y]es / [N]o / [a]llow for session: ",
				actor, req.Cwd, approval.EscapeControl(req.Command))
		} else {
			fmt.Fprintf(out, "\n[approval] %q wants to run a command in %q:\n  $ %s\n  WARNING: runs unsandboxed with your full user permissions and network access.\nAllow? [y]es / [N]o: ",
				actor, req.Cwd, approval.EscapeControl(req.Command))
		}
	}

	ans, err := readLine(ctx)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return approval.ChoiceDeny, nil
		}
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(out)
		}
		return approval.ChoiceDeny, err
	}
	switch strings.ToLower(strings.TrimSpace(ans)) {
	case "y", "yes":
		return approval.ChoiceAllow, nil
	case "a", "all", "session", "allow":
		return approval.ChoiceSession, nil
	}
	return approval.ChoiceDeny, nil
}
