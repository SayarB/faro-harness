package desktop

import "github.com/SayarB/faro-harness/internal/llm"

// Event type names sent to the frontend on a single stream. Keep in sync with
// frontend/src/lib/events.ts.
const (
	EventToken        = "token"
	EventToolCall     = "tool_call"
	EventToolResult   = "tool_result"
	EventUsage        = "usage"
	EventTurnDone     = "turn.done"
	EventTurnError    = "turn.error"
	EventTurnCanceled = "turn.cancelled"
	EventIndex        = "index.progress"

	EventApprovalRequest  = "approval.request"
	EventApprovalResolved = "approval.resolved"
)

// Event is the single JSON payload shape streamed to the UI; which fields are set depends on Type.
type Event struct {
	Type   string `json:"type"`
	TurnID string `json:"turnId,omitempty"`
	Step   int    `json:"step,omitempty"`

	Text string `json:"text,omitempty"` // token

	ToolCallID string `json:"toolCallId,omitempty"` // tool_call, tool_result
	ToolName   string `json:"toolName,omitempty"`
	ToolArgs   string `json:"toolArgs,omitempty"`
	ToolResult string `json:"toolResult,omitempty"` // truncated for display
	DurationMs int64  `json:"durationMs,omitempty"`

	Usage *llm.Usage `json:"usage,omitempty"` // usage, turn.done

	Reply string `json:"reply,omitempty"` // turn.done
	Error string `json:"error,omitempty"` // turn.error, index.progress

	WorkspaceID int64 `json:"workspaceId,omitempty"` // index.progress
	Indexed     int64 `json:"indexed,omitempty"`
	Skipped     int64 `json:"skipped,omitempty"`
	IndexErrors int64 `json:"indexErrors,omitempty"`
	Done        bool  `json:"done,omitempty"`

	// approval.request / approval.resolved. Every string here comes from the Go ApprovalRequest,
	// not from model prose, and is passed through approval.EscapeControl. Sandboxed is omitted
	// when false, and the UI must treat "absent" as unsandboxed.
	ApprovalID        string `json:"approvalId,omitempty"`
	Kind              string `json:"kind,omitempty"` // command | file_create | file_overwrite
	Command           string `json:"command,omitempty"`
	Cwd               string `json:"cwd,omitempty"`
	WorkspaceName     string `json:"workspaceName,omitempty"`
	WorkspacePath     string `json:"workspacePath,omitempty"` // absolute folder the file goes into
	DestRelPath       string `json:"destRelPath,omitempty"`
	SizeBytes         int64  `json:"sizeBytes,omitempty"`
	ExistingSizeBytes int64  `json:"existingSizeBytes,omitempty"`
	Preview           string `json:"preview,omitempty"`
	Sandboxed         bool   `json:"sandboxed,omitempty"`
	BotName           string `json:"botName,omitempty"`
	// Decision is set on approval.resolved: "allow", "allow_session", "deny", or "cancelled" when
	// the turn was stopped (or the app closed) before the user answered.
	Decision string `json:"decision,omitempty"`
}

// WorkspaceInfo is a folder the user has attached.
type WorkspaceInfo struct {
	ID        int64  `json:"id"`
	Path      string `json:"path"`
	Name      string `json:"name"`
	FileCount int64  `json:"fileCount"`
}

// BotInfo is a created bot.
type BotInfo struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// State is the app snapshot the UI loads at startup and after changes.
type State struct {
	HasKey     bool            `json:"hasKey"`
	Workspaces []WorkspaceInfo `json:"workspaces"`
	Bots       []BotInfo       `json:"bots"`
}

// ChatInfo identifies the active chat session.
type ChatInfo struct {
	ConversationID string `json:"conversationId"`
	BotSlug        string `json:"botSlug"`
}
