package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/SayarB/faro-harness/internal/desktop"
	"github.com/SayarB/faro-harness/internal/store"
)

func resolveSocketPath(override string) (string, error) {
	if s := strings.TrimSpace(override); s != "" {
		return expandHomeDir(s), nil
	}
	dir, err := store.DefaultDataDir()
	if err != nil {
		return "", err
	}
	return desktop.DefaultSocketPath(dir), nil
}

func runChats(args []string) error {
	fs := flag.NewFlagSet("chats", flag.ExitOnError)
	sockFlag := fs.String("socket", "", "path to Faro desktop socket (defaults to <dataDir>/run/faro.sock)")
	_ = fs.Parse(reorderFlagsFirst(args, nil))

	sockPath, err := resolveSocketPath(*sockFlag)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cli := desktop.NewClient(sockPath)
	chats, err := cli.ListChats(ctx)
	if err != nil {
		return err
	}
	if len(chats) == 0 {
		fmt.Println("(no chats yet)")
		return nil
	}
	for _, ch := range chats {
		ts := "-"
		if ch.UpdatedAt > 0 {
			ts = time.Unix(ch.UpdatedAt, 0).Local().Format("2006-01-02 15:04")
		}
		preview := ch.Preview
		if preview == "" {
			preview = "(empty)"
		}
		fmt.Printf("%s  %-14s  %s  %s\n", ch.ID, ch.BotSlug, ts, preview)
	}
	return nil
}

func runAttach(args []string) error {
	fs := flag.NewFlagSet("attach", flag.ExitOnError)
	sockFlag := fs.String("socket", "", "path to Faro desktop socket (defaults to <dataDir>/run/faro.sock)")
	botSlug := fs.String("bot", "", "start a new chat with this bot slug on the desktop host")
	_ = fs.Parse(reorderFlagsFirst(args, nil))

	if *botSlug != "" && fs.NArg() > 0 {
		return errors.New("pass either <conv-id> or --bot <slug>, not both")
	}
	if *botSlug == "" && fs.NArg() < 1 {
		return errors.New("conversation id or --bot <slug> is required (run `faro-cli chats` to list conversations)")
	}

	sockPath, err := resolveSocketPath(*sockFlag)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cli := desktop.NewClient(sockPath)
	convID := ""
	if *botSlug != "" {
		info, err := cli.NewChat(ctx, strings.TrimSpace(*botSlug))
		if err != nil {
			return err
		}
		convID = info.ConversationID
	} else {
		convID = strings.TrimSpace(fs.Arg(0))
	}

	return attachInteractive(ctx, cli, convID, os.Stdin, os.Stdout, os.Stderr)
}

func attachInteractive(ctx context.Context, cli *desktop.Client, convID string, in io.Reader, out, errOut io.Writer) error {
	snap, stream, err := cli.Attach(ctx, convID)
	if err != nil {
		return err
	}
	defer stream.Close()

	fmt.Fprintf(errOut, "Attached to %s (bot=%s) | commands: /cancel, /exit\n", snap.ConversationID, snap.BotSlug)

	r := &attachRenderer{out: out, errOut: errOut}
	var pendingReq *desktop.Event
	for i := range snap.Events {
		ev := snap.Events[i]
		r.renderEvent(ev)
		switch ev.Type {
		case desktop.EventApprovalRequest:
			cp := ev
			pendingReq = &cp
		case desktop.EventApprovalResolved:
			if pendingReq != nil && pendingReq.ApprovalID == ev.ApprovalID {
				pendingReq = nil
			}
		case desktop.EventTurnDone, desktop.EventTurnError, desktop.EventTurnCanceled:
			if pendingReq != nil && pendingReq.TurnID == ev.TurnID {
				pendingReq = nil
			}
		}
	}

	pendingID := ""
	answeredLocally := map[string]bool{}
	if pendingReq != nil {
		pendingID = pendingReq.ApprovalID
		r.printApproval(*pendingReq, true)
	}

	stdinReader := newStdinLineReader(in)
	for {
		select {
		case <-ctx.Done():
			fmt.Fprintln(errOut)
			return ctx.Err()

		case ev, ok := <-stream.Events():
			if !ok {
				r.flushTokenLine()
				fmt.Fprintln(errOut, "\n[disconnected]")
				return stream.Err()
			}
			r.renderEvent(ev)
			switch ev.Type {
			case desktop.EventApprovalRequest:
				pendingID = ev.ApprovalID
				r.printApproval(ev, true)
			case desktop.EventApprovalResolved:
				if answeredLocally[ev.ApprovalID] {
					delete(answeredLocally, ev.ApprovalID)
				} else if pendingID == ev.ApprovalID {
					pendingID = ""
					if ev.Decision == desktop.DecisionCancelled {
						fmt.Fprintln(errOut, "\n[approval cancelled]")
					} else {
						fmt.Fprintln(errOut, "\n[answered in the app]")
					}
				}
			case desktop.EventTurnDone, desktop.EventTurnError, desktop.EventTurnCanceled:
				if pendingID != "" {
					pendingID = ""
				}
			}

		case line, ok := <-stdinReader.lines:
			if !ok {
				fmt.Fprintln(errOut)
				return nil
			}
			if line.err != nil && !errors.Is(line.err, io.EOF) {
				return line.err
			}
			if pendingID != "" {
				id := pendingID
				pendingID = ""
				answeredLocally[id] = true
				decision := parseApprovalDecision(line.text)
				if err := cli.RespondApproval(ctx, id, decision); err != nil {
					delete(answeredLocally, id)
					fmt.Fprintf(errOut, "[approval] %v\n", err)
				}
				if errors.Is(line.err, io.EOF) {
					return nil
				}
				continue
			}

			input := strings.TrimSpace(line.text)
			if input == "" {
				if errors.Is(line.err, io.EOF) {
					return nil
				}
				continue
			}
			switch input {
			case "/exit", "/quit":
				return nil
			case "/cancel":
				if err := cli.Cancel(ctx); err != nil {
					fmt.Fprintf(errOut, "cancel error: %v\n", err)
				}
			default:
				if _, err := cli.Send(ctx, snap.ConversationID, input); err != nil {
					fmt.Fprintf(errOut, "error: %v\n", err)
				}
			}
			if errors.Is(line.err, io.EOF) {
				return nil
			}
		}
	}
}

func runSend(args []string) error {
	fs := flag.NewFlagSet("send", flag.ExitOnError)
	sockFlag := fs.String("socket", "", "path to Faro desktop socket (defaults to <dataDir>/run/faro.sock)")
	_ = fs.Parse(args)

	if fs.NArg() < 2 {
		return errors.New("usage: faro-cli send [--socket <path>] <conv-id> <message...>")
	}
	convID := strings.TrimSpace(fs.Arg(0))
	msg := strings.TrimSpace(strings.Join(fs.Args()[1:], " "))
	if convID == "" || msg == "" {
		return errors.New("conversation id and message are required")
	}

	sockPath, err := resolveSocketPath(*sockFlag)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cli := desktop.NewClient(sockPath)
	return sendOneShot(ctx, cli, convID, msg, os.Stdout, os.Stderr)
}

func sendOneShot(ctx context.Context, cli *desktop.Client, convID, msg string, out, errOut io.Writer) error {
	_, stream, err := cli.Attach(ctx, convID)
	if err != nil {
		return err
	}
	defer stream.Close()

	turnID, err := cli.Send(ctx, convID, msg)
	if err != nil {
		return err
	}

	r := &attachRenderer{out: out, errOut: errOut}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-stream.Events():
			if !ok {
				r.flushTokenLine()
				fmt.Fprintln(errOut, "\n[disconnected]")
				if sErr := stream.Err(); sErr != nil {
					return sErr
				}
				return errors.New("disconnected before turn finished")
			}
			if ev.TurnID != turnID {
				continue
			}
			r.renderEvent(ev)
			switch ev.Type {
			case desktop.EventApprovalRequest:
				r.printApproval(ev, false)
			case desktop.EventApprovalResolved:
				if ev.Decision != "" {
					fmt.Fprintf(errOut, "[approval resolved: %s]\n", ev.Decision)
				}
			case desktop.EventTurnDone:
				return nil
			case desktop.EventTurnError:
				return errors.New(ev.Error)
			case desktop.EventTurnCanceled:
				return errors.New("turn cancelled")
			}
		}
	}
}

func parseApprovalDecision(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "y", "yes":
		return desktop.DecisionAllow
	case "a", "all", "session", "allow":
		return desktop.DecisionAllowSession
	default:
		return desktop.DecisionDeny
	}
}

type attachRenderer struct {
	out      io.Writer
	errOut   io.Writer
	inTokens bool
}

func (r *attachRenderer) flushTokenLine() {
	if r.inTokens {
		fmt.Fprintln(r.out)
		r.inTokens = false
	}
}

func (r *attachRenderer) renderEvent(ev desktop.Event) {
	switch ev.Type {
	case desktop.EventTurnStarted:
		r.flushTokenLine()
		if ev.Text != "" {
			fmt.Fprintf(r.errOut, "\nuser> %s\n", ev.Text)
		}
	case desktop.EventToken:
		fmt.Fprint(r.out, ev.Text)
		r.inTokens = true
	case desktop.EventToolCall:
		r.flushTokenLine()
		if ev.Step > 0 {
			fmt.Fprintf(r.errOut, "  [step %d] -> %s(%s)\n", ev.Step, ev.ToolName, ev.ToolArgs)
		} else {
			fmt.Fprintf(r.errOut, "  [tool] -> %s(%s)\n", ev.ToolName, ev.ToolArgs)
		}
	case desktop.EventToolResult:
		r.flushTokenLine()
		if ev.Step > 0 {
			fmt.Fprintf(r.errOut, "  [step %d] <- %s (%d bytes, %dms)\n", ev.Step, ev.ToolName, len(ev.ToolResult), ev.DurationMs)
		} else {
			fmt.Fprintf(r.errOut, "  [tool] <- %s (%d bytes)\n", ev.ToolName, len(ev.ToolResult))
		}
	case desktop.EventTurnDone:
		if !r.inTokens && ev.Reply != "" {
			fmt.Fprint(r.out, ev.Reply)
			r.inTokens = true
		}
		r.flushTokenLine()
		if ev.Usage != nil {
			fmt.Fprintf(r.errOut, "[done] tokens(prompt=%d cached=%d cache_write=%d completion=%d)\n",
				ev.Usage.PromptTokens, ev.Usage.CachedTokens, ev.Usage.CacheWriteTokens, ev.Usage.CompletionTokens)
		}
	case desktop.EventTurnError:
		r.flushTokenLine()
		fmt.Fprintf(r.errOut, "[error] %s\n", ev.Error)
	case desktop.EventTurnCanceled:
		r.flushTokenLine()
		fmt.Fprintln(r.errOut, "[cancelled]")
	}
}

// printApproval formats an approval.request event using the escaped fields already prepared by Go.
func (r *attachRenderer) printApproval(ev desktop.Event, interactive bool) {
	r.flushTokenLine()
	actor := ev.BotName
	if actor == "" {
		actor = "Faro"
	}

	switch ev.Kind {
	case "file_create":
		fmt.Fprintf(r.errOut, "\n[approval] %q wants to save a new file in %q (%q):\n    to: %q (%d bytes)\n",
			actor, ev.WorkspaceName, ev.WorkspacePath, ev.DestRelPath, ev.SizeBytes)
		if ev.Preview != "" {
			fmt.Fprintf(r.errOut, "  text: %q\n", ev.Preview)
		}
		if interactive {
			fmt.Fprint(r.errOut, "Allow? [y]es / [N]o / [a]llow new files in this folder for session: ")
		}
	case "file_overwrite":
		fmt.Fprintf(r.errOut, "\n[approval] %q wants to OVERWRITE an existing file in %q (%q):\n    to: %q (%d bytes; existing: %d bytes; will be backed up first)\n",
			actor, ev.WorkspaceName, ev.WorkspacePath, ev.DestRelPath, ev.SizeBytes, ev.ExistingSizeBytes)
		if ev.Preview != "" {
			fmt.Fprintf(r.errOut, "  text: %q\n", ev.Preview)
		}
		if interactive {
			fmt.Fprint(r.errOut, "Allow overwrite? [y]es / [N]o: ")
		}
	default: // command
		fmt.Fprintf(r.errOut, "\n[approval] %q wants to run a command in %q:\n  $ %s\n",
			actor, ev.Cwd, ev.Command)
		if !ev.Sandboxed {
			fmt.Fprintln(r.errOut, "  WARNING: runs unsandboxed with your full user permissions and network access.")
		}
		if interactive {
			if ev.Sandboxed {
				fmt.Fprint(r.errOut, "Allow? [y]es / [N]o / [a]llow for session: ")
			} else {
				fmt.Fprint(r.errOut, "Allow? [y]es / [N]o: ")
			}
		}
	}

	if !interactive {
		fmt.Fprintln(r.errOut, "  [approval required: answer this request in the Faro desktop app to continue]")
	}
}
