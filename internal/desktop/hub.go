package desktop

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/SayarB/faro-harness/internal/llm"
	"github.com/SayarB/faro-harness/internal/store"
)

const defaultSubBuffer = 256

type runningTurn struct {
	convID      string
	turnID      string
	startMsgSeq int
	log         []Event
}

type subscriber struct {
	convID string
	ch     chan Event
}

// hub fans out desktop Events in strict global sequence order to the Wails UI callback and any
// attached conversation subscribers, and tracks the single in-flight turn's coalesced event log
// so snapshots never duplicate or miss events.
//
// Lock ordering: always c.mu -> hub.mu; hub never acquires c.mu.
type hub struct {
	mu      sync.Mutex
	seq     uint64
	emitUI  func(Event)
	subs    map[*subscriber]struct{}
	running *runningTurn
}

func newHub(emitUI func(Event)) *hub {
	if emitUI == nil {
		emitUI = func(Event) {}
	}
	return &hub{
		emitUI: emitUI,
		subs:   map[*subscriber]struct{}{},
	}
}

// emit stamps a monotonic seq, appends to the running turn log if matching, notifies the Wails
// UI callback, and pushes non-blockingly to conversation subscribers.
func (h *hub) emit(ev Event) Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.emitLocked(ev)
}

// emitForChat stamps convID and, when unset, the running turn's TurnID for convID, then emits.
func (h *hub) emitForChat(convID string, ev Event) Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	ev.ConversationID = convID
	if ev.TurnID == "" && h.running != nil && h.running.convID == convID {
		ev.TurnID = h.running.turnID
	}
	return h.emitLocked(ev)
}

func (h *hub) runningTurnID(convID string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.running != nil && h.running.convID == convID {
		return h.running.turnID
	}
	return ""
}

// startTurn sets the active running turn and emits turn.started in one atomic critical section.
func (h *hub) startTurn(convID, turnID string, startMsgSeq int, text string) Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.running = &runningTurn{
		convID:      convID,
		turnID:      turnID,
		startMsgSeq: startMsgSeq,
	}
	return h.emitLocked(Event{
		Type:           EventTurnStarted,
		ConversationID: convID,
		TurnID:         turnID,
		Text:           text,
	})
}

// finishTurn emits the terminal turn event (turn.done/error/cancelled) and clears the running
// turn in one atomic critical section so any subsequent snapshot reads the persisted rows.
func (h *hub) finishTurn(ev Event) Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := h.emitLocked(ev)
	if h.running != nil && h.running.convID == ev.ConversationID && h.running.turnID == ev.TurnID {
		h.running = nil
	}
	return out
}

func (h *hub) emitLocked(ev Event) Event {
	h.seq++
	ev.Seq = h.seq
	if h.running != nil && ev.ConversationID != "" && ev.ConversationID == h.running.convID {
		if ev.Type == EventToken && len(h.running.log) > 0 {
			last := &h.running.log[len(h.running.log)-1]
			if last.Type == EventToken && last.TurnID == ev.TurnID {
				last.Text += ev.Text
				last.Seq = ev.Seq
			} else {
				h.running.log = append(h.running.log, ev)
			}
		} else {
			h.running.log = append(h.running.log, ev)
		}
	}
	h.emitUI(ev)
	if ev.ConversationID != "" && ev.Type != EventIndex {
		for sub := range h.subs {
			if sub.convID != ev.ConversationID {
				continue
			}
			select {
			case sub.ch <- ev:
			default:
				delete(h.subs, sub)
				close(sub.ch)
			}
		}
	}
	return ev
}

// snapshot builds a point-in-time event snapshot for convID under hub.mu.
func (h *hub) snapshot(ctx context.Context, st *store.Store, convID, botSlug string) (Snapshot, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.snapshotLocked(ctx, st, convID, botSlug)
}

// subscribeWithSnapshot atomically registers a subscriber for convID and builds its initial
// snapshot under a single hub.mu hold so no live event can arrive with Seq <= LastSeq.
func (h *hub) subscribeWithSnapshot(ctx context.Context, st *store.Store, convID, botSlug string) (Snapshot, *subscriber, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	snap, err := h.snapshotLocked(ctx, st, convID, botSlug)
	if err != nil {
		return Snapshot{}, nil, err
	}
	sub := &subscriber{
		convID: convID,
		ch:     make(chan Event, defaultSubBuffer),
	}
	h.subs[sub] = struct{}{}
	return snap, sub, nil
}

func (h *hub) unsubscribe(sub *subscriber) {
	h.mu.Lock()
	if _, ok := h.subs[sub]; ok {
		delete(h.subs, sub)
		close(sub.ch)
	}
	h.mu.Unlock()
}

func (h *hub) closeAll() {
	h.mu.Lock()
	for sub := range h.subs {
		delete(h.subs, sub)
		close(sub.ch)
	}
	h.mu.Unlock()
}

func (h *hub) snapshotLocked(ctx context.Context, st *store.Store, convID, botSlug string) (Snapshot, error) {
	records, err := st.ListMessages(ctx, convID)
	if err != nil {
		return Snapshot{}, err
	}
	runningThis := h.running != nil && h.running.convID == convID
	limitSeq := -1
	if runningThis {
		limitSeq = h.running.startMsgSeq
	}
	events := recordsToEvents(convID, records, limitSeq)
	if runningThis {
		events = append(events, h.running.log...)
	}
	return Snapshot{
		ConversationID: convID,
		BotSlug:        botSlug,
		Busy:           runningThis,
		LastSeq:        h.seq,
		Events:         events,
	}, nil
}

// recordsToEvents converts persisted conversation messages (up to limitSeq when >= 0) into
// synthetic replay events with Seq 0 and TurnIDs "hist-1", "hist-2", etc.
func recordsToEvents(convID string, records []store.MessageRecord, limitSeq int) []Event {
	events := []Event{}
	turnIdx := 0
	turnID := ""
	inTurn := false
	lastReply := ""

	closeTurn := func() {
		if !inTurn {
			return
		}
		events = append(events, Event{
			Type:           EventTurnDone,
			ConversationID: convID,
			TurnID:         turnID,
			Reply:          lastReply,
		})
		inTurn = false
		lastReply = ""
	}

	for _, rec := range records {
		if limitSeq >= 0 && rec.Seq > limitSeq {
			break
		}
		switch llm.Role(rec.Role) {
		case llm.RoleUser:
			closeTurn()
			turnIdx++
			turnID = fmt.Sprintf("hist-%d", turnIdx)
			inTurn = true
			lastReply = ""
			events = append(events, Event{
				Type:           EventTurnStarted,
				ConversationID: convID,
				TurnID:         turnID,
				Text:           rec.Content,
			})
		case llm.RoleAssistant:
			if !inTurn {
				turnIdx++
				turnID = fmt.Sprintf("hist-%d", turnIdx)
				inTurn = true
				lastReply = ""
				events = append(events, Event{
					Type:           EventTurnStarted,
					ConversationID: convID,
					TurnID:         turnID,
				})
			}
			if rec.Content != "" {
				events = append(events, Event{
					Type:           EventToken,
					ConversationID: convID,
					TurnID:         turnID,
					Text:           rec.Content,
				})
				lastReply = rec.Content
			}
			if rec.ToolCallsJSON != "" {
				var calls []llm.ToolCall
				if err := json.Unmarshal([]byte(rec.ToolCallsJSON), &calls); err == nil {
					for _, tc := range calls {
						events = append(events, Event{
							Type:           EventToolCall,
							ConversationID: convID,
							TurnID:         turnID,
							ToolCallID:     tc.ID,
							ToolName:       tc.Name,
							ToolArgs:       tc.Arguments,
						})
					}
				}
			}
		case llm.RoleTool:
			if !inTurn {
				continue
			}
			events = append(events, Event{
				Type:           EventToolResult,
				ConversationID: convID,
				TurnID:         turnID,
				ToolCallID:     rec.ToolCallID,
				ToolName:       rec.ToolName,
				ToolResult:     truncate(rec.Content, maxToolResultDisplay),
			})
		}
	}
	closeTurn()
	return events
}
