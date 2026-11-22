package tools

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
)

// ErrApprovalNonInteractive may be returned by an Approver when an action cannot be
// prompted because the session is non-interactive (distinguishing it from an explicit user denial).
var ErrApprovalNonInteractive = errors.New("approval required in non-interactive mode")

// ApprovalDecision represents the human or policy decision for a gated tool action.
type ApprovalDecision string

const (
	ApprovalAllow        ApprovalDecision = "allow"
	ApprovalDeny         ApprovalDecision = "deny"
	ApprovalAllowSession ApprovalDecision = "allow_session"
)

// ApprovalKind identifies the category of gated action.
type ApprovalKind string

const (
	ApprovalKindCommand       ApprovalKind = "command"
	ApprovalKindFileCreate    ApprovalKind = "file_create"
	ApprovalKindFileOverwrite ApprovalKind = "file_overwrite"
)

// ApprovalRequest describes an action requiring user or policy approval (shared by command and write gates).
type ApprovalRequest struct {
	Kind              ApprovalKind
	BotID             string
	BotSlug           string
	BotName           string
	Tool              string
	Command           string
	Cwd               string
	WorkspaceID       int64
	WorkspaceName     string
	WorkspaceRoot     string
	SourcePath        string
	DestRelPath       string
	SizeBytes         int64
	ExistingSizeBytes int64
	Preview           string
	WillOverwrite     bool
}

// Approver evaluates whether a gated action (such as bot_run_command or deliver_file) is permitted.
type Approver func(ctx context.Context, req ApprovalRequest) (ApprovalDecision, error)

// Policy declares the intended OS sandbox boundaries for a child command.
// Enforced by OS-specific Sandbox implementations in Phase 3 (Landlock / Seatbelt / AppContainer).
type Policy struct {
	ReadOnly  []string
	ReadWrite []string
	Network   bool
}

// Sandbox configures OS-level isolation on an exec.Cmd before Start/Run.
type Sandbox interface {
	Prepare(cmd *exec.Cmd, p Policy) error
}

type noopSandbox struct{}

func (noopSandbox) Prepare(_ *exec.Cmd, _ Policy) error { return nil }

// RuntimeInfo describes the resolved Python / uv runtime available to bot commands.
// Today it resolves system binaries; in Phase 3 it will point at Faro's bundled runtime.
type RuntimeInfo struct {
	PythonPath string
	UVPath     string
	BinDir     string
	Managed    bool
}

// ResolvePython locates the Python and uv executables for bot commands.
func ResolvePython() RuntimeInfo {
	info := RuntimeInfo{}
	if p, err := exec.LookPath("python3"); err == nil {
		info.PythonPath = p
	} else if p, err := exec.LookPath("python"); err == nil {
		info.PythonPath = p
	}
	if u, err := exec.LookPath("uv"); err == nil {
		info.UVPath = u
	}
	if info.PythonPath != "" {
		info.BinDir = filepath.Dir(info.PythonPath)
	}
	return info
}
