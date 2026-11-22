package desktop

import (
	"context"
	"fmt"

	"github.com/SayarB/faro-harness/internal/approval"
	"github.com/SayarB/faro-harness/internal/bot"
	"github.com/SayarB/faro-harness/internal/tools"
)

// Decisions the UI may send to RespondApproval.
const (
	DecisionAllow        = "allow"
	DecisionAllowSession = "allow_session"
	DecisionDeny         = "deny"
	// DecisionCancelled is only ever reported by approval.resolved, never accepted from the UI.
	DecisionCancelled = "cancelled"
)

func choiceName(c approval.Choice) string {
	switch c {
	case approval.ChoiceAllow:
		return DecisionAllow
	case approval.ChoiceSession:
		return DecisionAllowSession
	}
	return DecisionDeny
}

// newGate builds the approval policy for a chat. There are no auto-approve flags in the GUI:
// every gated action is put in front of the user. Session memory lives as long as the Gate, i.e.
// until the next NewChat.
func (c *Controller) newGate() *approval.Gate {
	return approval.NewGate(approval.Config{Ask: c.askUser})
}

// askUser is the GUI implementation of approval.Ask. It emits approval.request and blocks until
// RespondApproval answers or ctx ends (Cancel / Shutdown), which counts as a denial so a pending
// request never leaves a goroutine blocked.
func (c *Controller) askUser(ctx context.Context, req tools.ApprovalRequest) (approval.Choice, error) {
	id, err := bot.NewID("appr")
	if err != nil {
		return approval.ChoiceDeny, err
	}
	answer := make(chan approval.Choice, 1)
	c.mu.Lock()
	c.pending[id] = answer
	turn := c.turnID
	c.mu.Unlock()

	decision := DecisionCancelled
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		c.emit(Event{Type: EventApprovalResolved, TurnID: turn, ApprovalID: id, Decision: decision})
	}()

	c.emit(Event{
		Type: EventApprovalRequest, TurnID: turn, ApprovalID: id,
		Kind:              string(req.Kind),
		Command:           approval.EscapeControl(req.Command),
		Cwd:               approval.EscapeControl(req.Cwd),
		WorkspaceName:     approval.EscapeControl(req.WorkspaceName),
		WorkspacePath:     approval.EscapeControl(req.WorkspaceRoot),
		DestRelPath:       approval.EscapeControl(req.DestRelPath),
		SizeBytes:         req.SizeBytes,
		ExistingSizeBytes: req.ExistingSizeBytes,
		Preview:           approval.EscapeControl(req.Preview),
		Sandboxed:         req.Sandboxed,
		BotName:           approval.EscapeControl(req.BotName),
	})

	select {
	case ch := <-answer:
		decision = choiceName(ch)
		return ch, nil
	case <-ctx.Done():
		return approval.ChoiceDeny, ctx.Err()
	}
}

// RespondApproval delivers the user's answer for a pending approval request. This is a UI-only
// entry point: it is never exposed to the model as a tool. An unrecognised decision denies.
// Whether "allow_session" is honoured is up to the Gate (it is not for unsandboxed commands).
func (c *Controller) RespondApproval(id, decision string) error {
	choice, known := approval.ChoiceDeny, true
	switch decision {
	case DecisionAllow:
		choice = approval.ChoiceAllow
	case DecisionAllowSession:
		choice = approval.ChoiceSession
	case DecisionDeny:
	default:
		known = false
	}
	c.mu.Lock()
	answer, ok := c.pending[id]
	delete(c.pending, id)
	c.mu.Unlock()
	if !ok {
		return fmt.Errorf("approval request %q is no longer pending", id)
	}
	answer <- choice // buffered; a single sender wins because the entry was removed under the lock
	if !known {
		return fmt.Errorf("unknown decision %q; denied", decision)
	}
	return nil
}
