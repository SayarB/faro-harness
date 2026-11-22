package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/SayarB/faro-harness/internal/bot"
	"github.com/SayarB/faro-harness/internal/llm"
	"github.com/SayarB/faro-harness/internal/store"
)

const (
	maxBotWriteBytes      = 256 * 1024
	defaultCmdTimeoutSecs = 60
	maxCmdTimeoutSecs     = 180
	maxFilesChangedReport = 30
)

func botToolDefinitions() []llm.ToolDef {
	shellName := shellDisplayName()
	return []llm.ToolDef{
		{
			Name:        "bot_list",
			Description: "List files and subfolders inside your own bot folder (such as memory.md, bot.md, and scratch/). Never accesses other bots or user folders.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"path": {
						"type": "string",
						"description": "Relative subfolder inside your bot folder (omit or '' for root, or 'scratch')."
					}
				}
			}`),
		},
		{
			Name:        "bot_read",
			Description: "Read a file inside your own bot folder (e.g. 'memory.md', 'bot.md', or 'scratch/notes.md').",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"path": {
						"type": "string",
						"description": "Relative path inside your bot folder."
					},
					"offset": {
						"type": "integer",
						"description": "Byte offset to start reading from (default 0)."
					},
					"limit": {
						"type": "integer",
						"description": "Max bytes to return (default 12288, max 16384)."
					}
				},
				"required": ["path"]
			}`),
		},
		{
			Name:        "bot_write",
			Description: "Create, overwrite, or append to 'memory.md' or files inside 'scratch/' in your own bot folder. Cannot edit bot.md (user-editable only).",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"path": {
						"type": "string",
						"description": "Relative path: 'memory.md' or under 'scratch/' (e.g. 'scratch/out/summary.md')."
					},
					"content": {
						"type": "string",
						"description": "Text content to write."
					},
					"mode": {
						"type": "string",
						"enum": ["overwrite", "append"],
						"description": "Write mode: 'overwrite' (default) or 'append'."
					}
				},
				"required": ["path", "content"]
			}`),
		},
		{
			Name:        "bot_delete",
			Description: "Delete a file inside your own scratch/ folder to clean up working files. Cannot delete bot.md or memory.md.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"path": {
						"type": "string",
						"description": "Relative path under 'scratch/' (e.g. 'scratch/draft.md')."
					}
				},
				"required": ["path"]
			}`),
		},
		{
			Name: "bot_run_command",
			Description: fmt.Sprintf(
				"Run a terminal command using %s with working directory set to your own scratch/ folder (e.g. python3 or `uv run --with openpyxl/reportlab/pypdf/duckdb python ...`) to generate real Excel/PDF/Word files, analyze data, or inspect binary documents. Requires user approval unless pre-approved for the session. Environment variables FARO_BOT_DIR, FARO_SCRATCH_DIR, and FARO_WORKSPACES_READONLY (JSON map of workspace id to read-only path) are provided. Never modify files outside scratch/.",
				shellName,
			),
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"command": {
						"type": "string",
						"description": "Command string for the host shell, executed inside scratch/."
					},
					"timeout_seconds": {
						"type": "integer",
						"description": "Optional timeout in seconds (default 60, max 180)."
					}
				},
				"required": ["command"]
			}`),
		},
	}
}

type botListArgs struct {
	Path string `json:"path"`
}

type botListEntry struct {
	Path  string `json:"path"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size,omitempty"`
	Mtime string `json:"mtime,omitempty"`
}

type botListOut struct {
	Path    string         `json:"path"`
	Entries []botListEntry `json:"entries"`
}

func (r *Registry) execBotList(_ context.Context, rawArgs string) (botListOut, error) {
	if r.bot == nil {
		return botListOut{}, errors.New("no bot home folder is mounted in this session")
	}
	var args botListArgs
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		return botListOut{}, fmt.Errorf("invalid bot_list arguments: %w", err)
	}

	rel, err := cleanBotRelPath(args.Path, true)
	if err != nil {
		return botListOut{}, err
	}
	if isInternalBotPath(rel) {
		return botListOut{}, fmt.Errorf("path %q is internal to Faro and cannot be listed", rel)
	}

	targetDir := r.bot.HomePath
	if rel != "" {
		targetDir, err = confinePath(r.bot.HomePath, rel)
		if err != nil {
			return botListOut{}, err
		}
	}

	info, err := os.Lstat(targetDir)
	if err != nil {
		if os.IsNotExist(err) {
			return botListOut{Path: rel, Entries: []botListEntry{}}, nil
		}
		return botListOut{}, fmt.Errorf("cannot access folder %q", rel)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return botListOut{}, fmt.Errorf("path %q is not a regular folder", rel)
	}
	if err := verifyRealPathWithinRoot(r.bot.HomePath, targetDir); err != nil {
		return botListOut{}, err
	}

	dirEntries, err := os.ReadDir(targetDir)
	if err != nil {
		return botListOut{}, fmt.Errorf("cannot read folder %q", rel)
	}

	entries := make([]botListEntry, 0, len(dirEntries))
	for _, de := range dirEntries {
		name := de.Name()
		if isIgnoredScratchName(name, de.IsDir()) {
			continue
		}
		info, err := de.Info()
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		entryPath := name
		if rel != "" {
			entryPath = rel + "/" + name
		}
		e := botListEntry{
			Path:  entryPath,
			IsDir: de.IsDir(),
			Mtime: formatMtime(info.ModTime().UnixNano()),
		}
		if !de.IsDir() {
			e.Size = info.Size()
		}
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		return entries[i].Path < entries[j].Path
	})

	return botListOut{Path: rel, Entries: entries}, nil
}

type botReadArgs struct {
	Path   string `json:"path"`
	Offset int64  `json:"offset"`
	Limit  int    `json:"limit"`
}

type botReadOut struct {
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	Mtime      string `json:"mtime"`
	Content    string `json:"content"`
	Offset     int64  `json:"offset"`
	BytesRead  int    `json:"bytes_read"`
	HasMore    bool   `json:"has_more"`
	NextOffset int64  `json:"next_offset,omitempty"`
}

func (r *Registry) execBotRead(_ context.Context, rawArgs string) (botReadOut, error) {
	if r.bot == nil {
		return botReadOut{}, errors.New("no bot home folder is mounted in this session")
	}
	var args botReadArgs
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		return botReadOut{}, fmt.Errorf("invalid bot_read arguments: %w", err)
	}

	rel, err := cleanBotRelPath(args.Path, false)
	if err != nil {
		return botReadOut{}, err
	}
	if isInternalBotPath(rel) {
		return botReadOut{}, fmt.Errorf("path %q is internal to Faro and cannot be read", rel)
	}
	absPath, err := confinePath(r.bot.HomePath, rel)
	if err != nil {
		return botReadOut{}, err
	}

	info, err := os.Lstat(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return botReadOut{}, fmt.Errorf("file %q does not exist in your bot folder", rel)
		}
		return botReadOut{}, err
	}
	if info.IsDir() {
		return botReadOut{}, fmt.Errorf("%q is a folder; use bot_list instead", rel)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return botReadOut{}, fmt.Errorf("%q is a symlink and cannot be read", rel)
	}
	if err := verifyRealPathWithinRoot(r.bot.HomePath, absPath); err != nil {
		return botReadOut{}, err
	}

	offset := args.Offset
	if offset < 0 {
		offset = 0
	}
	limit := args.Limit
	if limit <= 0 {
		limit = defaultReadBytes
	}
	if limit > maxReadBytes {
		limit = maxReadBytes
	}

	data, hasMore, nextOffset, isBinary, err := readUTF8Slice(absPath, offset, limit, info.Size())
	if err != nil {
		return botReadOut{}, err
	}
	if isBinary {
		return botReadOut{}, fmt.Errorf("%q appears to be a binary file", rel)
	}

	out := botReadOut{
		Path:      rel,
		Size:      info.Size(),
		Mtime:     formatMtime(info.ModTime().UnixNano()),
		Content:   data,
		Offset:    offset,
		BytesRead: len(data),
		HasMore:   hasMore,
	}
	if hasMore {
		out.NextOffset = nextOffset
	}
	return out, nil
}

type botWriteArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Mode    string `json:"mode"`
}

type botWriteOut struct {
	Path         string `json:"path"`
	Mode         string `json:"mode"`
	BytesWritten int    `json:"bytes_written"`
	TotalSize    int64  `json:"total_size"`
}

func (r *Registry) execBotWrite(_ context.Context, rawArgs string) (botWriteOut, error) {
	if r.bot == nil {
		return botWriteOut{}, errors.New("no bot home folder is mounted in this session")
	}
	var args botWriteArgs
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		return botWriteOut{}, fmt.Errorf("invalid bot_write arguments: %w", err)
	}
	if len(args.Content) > maxBotWriteBytes {
		return botWriteOut{}, fmt.Errorf("content exceeds max single write size (%d bytes)", maxBotWriteBytes)
	}

	mode := strings.ToLower(strings.TrimSpace(args.Mode))
	if mode == "" {
		mode = "overwrite"
	}
	if mode != "overwrite" && mode != "append" {
		return botWriteOut{}, fmt.Errorf("invalid mode %q: expected 'overwrite' or 'append'", args.Mode)
	}

	rel, err := cleanBotRelPath(args.Path, false)
	if err != nil {
		return botWriteOut{}, err
	}
	if isInternalBotPath(rel) {
		return botWriteOut{}, fmt.Errorf("path %q is internal to Faro and cannot be written", rel)
	}
	if rel != bot.MemoryFileName && !strings.HasPrefix(rel, bot.ScratchDirName+"/") {
		if rel == bot.BotFileName {
			return botWriteOut{}, errors.New("bot.md is user-editable only; write to memory.md or scratch/ instead")
		}
		return botWriteOut{}, fmt.Errorf("bot_write only permits %q or paths under %q (got %q)", bot.MemoryFileName, bot.ScratchDirName+"/", rel)
	}

	absPath, err := confinePath(r.bot.HomePath, rel)
	if err != nil {
		return botWriteOut{}, err
	}

	// Reject symlinks anywhere in the existing ancestor chain before creating subdirs or writing.
	if err := verifyExistingAncestorsWithinRoot(r.bot.HomePath, absPath); err != nil {
		return botWriteOut{}, err
	}
	if info, err := os.Lstat(absPath); err == nil {
		if info.IsDir() {
			return botWriteOut{}, fmt.Errorf("%q is a directory", rel)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return botWriteOut{}, fmt.Errorf("%q is a symlink and cannot be written", rel)
		}
	} else if !os.IsNotExist(err) {
		return botWriteOut{}, err
	}

	parentDir := filepath.Dir(absPath)
	if err := os.MkdirAll(parentDir, 0o755); err != nil {
		return botWriteOut{}, err
	}
	if err := verifyRealPathWithinRoot(r.bot.HomePath, parentDir); err != nil {
		return botWriteOut{}, err
	}

	var finalBytes []byte
	if mode == "append" {
		existing, err := os.ReadFile(absPath)
		if err != nil && !os.IsNotExist(err) {
			return botWriteOut{}, err
		}
		if len(existing) > 0 && existing[len(existing)-1] != '\n' && len(args.Content) > 0 && args.Content[0] != '\n' {
			existing = append(existing, '\n')
		}
		finalBytes = append(existing, []byte(args.Content)...)
	} else {
		finalBytes = []byte(args.Content)
	}

	// Atomic write via temp file in parentDir + rename.
	tmp, err := os.CreateTemp(parentDir, ".faro-bot-write-*")
	if err != nil {
		return botWriteOut{}, err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(finalBytes); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return botWriteOut{}, err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return botWriteOut{}, err
	}
	if err := os.Rename(tmpName, absPath); err != nil {
		_ = os.Remove(tmpName)
		return botWriteOut{}, err
	}

	return botWriteOut{
		Path:         rel,
		Mode:         mode,
		BytesWritten: len(args.Content),
		TotalSize:    int64(len(finalBytes)),
	}, nil
}

type botDeleteArgs struct {
	Path string `json:"path"`
}

type botDeleteOut struct {
	Path    string `json:"path"`
	Deleted bool   `json:"deleted"`
}

func (r *Registry) execBotDelete(_ context.Context, rawArgs string) (botDeleteOut, error) {
	if r.bot == nil {
		return botDeleteOut{}, errors.New("no bot home folder is mounted in this session")
	}
	var args botDeleteArgs
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		return botDeleteOut{}, fmt.Errorf("invalid bot_delete arguments: %w", err)
	}

	rel, err := cleanBotRelPath(args.Path, false)
	if err != nil {
		return botDeleteOut{}, err
	}
	if isInternalBotPath(rel) || !strings.HasPrefix(rel, bot.ScratchDirName+"/") {
		return botDeleteOut{}, fmt.Errorf("bot_delete can only remove files inside %s/ (got %q)", bot.ScratchDirName, rel)
	}

	absPath, err := confinePath(r.bot.HomePath, rel)
	if err != nil {
		return botDeleteOut{}, err
	}
	info, err := os.Lstat(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return botDeleteOut{Path: rel, Deleted: false}, nil
		}
		return botDeleteOut{}, err
	}
	if info.IsDir() {
		return botDeleteOut{}, fmt.Errorf("%q is a directory", rel)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return botDeleteOut{}, fmt.Errorf("%q is a symlink and cannot be deleted", rel)
	}
	if err := verifyRealPathWithinRoot(r.bot.HomePath, absPath); err != nil {
		return botDeleteOut{}, err
	}
	if err := os.Remove(absPath); err != nil {
		return botDeleteOut{}, err
	}
	return botDeleteOut{Path: rel, Deleted: true}, nil
}

type botRunCommandArgs struct {
	Command        string `json:"command"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

type botRunCommandOut struct {
	Cwd                   string   `json:"cwd"`
	ExitCode              int      `json:"exit_code"`
	Signal                string   `json:"signal,omitempty"`
	Note                  string   `json:"note,omitempty"`
	Stdout                string   `json:"stdout,omitempty"`
	Stderr                string   `json:"stderr,omitempty"`
	StdoutBytesTotal      int64    `json:"stdout_bytes_total"`
	StderrBytesTotal      int64    `json:"stderr_bytes_total"`
	StdoutTruncated       bool     `json:"stdout_truncated,omitempty"`
	StderrTruncated       bool     `json:"stderr_truncated,omitempty"`
	TimedOut              bool     `json:"timed_out,omitempty"`
	DurationMS            int64    `json:"duration_ms"`
	FilesChanged          []string `json:"files_changed,omitempty"`
	FilesChangedTotal     int      `json:"files_changed_total,omitempty"`
	FilesChangedTruncated bool     `json:"files_changed_truncated,omitempty"`
}

type commandAuditEntry struct {
	Timestamp      string   `json:"timestamp"`
	BotID          string   `json:"bot_id"`
	BotSlug        string   `json:"bot_slug"`
	ConversationID string   `json:"conversation_id,omitempty"`
	Command        string   `json:"command"`
	Decision       string   `json:"decision"`
	ExitCode       int      `json:"exit_code"`
	Signal         string   `json:"signal,omitempty"`
	TimedOut       bool     `json:"timed_out,omitempty"`
	Error          string   `json:"error,omitempty"`
	DurationMS     int64    `json:"duration_ms"`
	FilesChanged   []string `json:"files_changed,omitempty"`
}

func (r *Registry) execBotRunCommand(ctx context.Context, rawArgs string) (botRunCommandOut, error) {
	if r.bot == nil {
		return botRunCommandOut{}, errors.New("no bot home folder is mounted in this session")
	}
	var args botRunCommandArgs
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		return botRunCommandOut{}, fmt.Errorf("invalid bot_run_command arguments: %w", err)
	}
	cmdStr := strings.TrimSpace(args.Command)
	if cmdStr == "" {
		return botRunCommandOut{}, errors.New("command is required")
	}

	decision, err := r.authorizeCommand(ctx, cmdStr)
	if err != nil {
		if errors.Is(err, ErrApprovalNonInteractive) {
			r.writeCommandAudit(commandAuditEntry{
				Timestamp:      time.Now().UTC().Format(time.RFC3339),
				BotID:          r.bot.ID,
				BotSlug:        r.bot.Slug,
				ConversationID: r.currentConversationID(),
				Command:        cmdStr,
				Decision:       "non_interactive_denied",
				ExitCode:       -1,
				Error:          err.Error(),
			})
			return botRunCommandOut{}, errors.New("command requires approval (run interactively or pass --allow-commands)")
		}
		r.writeCommandAudit(commandAuditEntry{
			Timestamp:      time.Now().UTC().Format(time.RFC3339),
			BotID:          r.bot.ID,
			BotSlug:        r.bot.Slug,
			ConversationID: r.currentConversationID(),
			Command:        cmdStr,
			Decision:       "approver_error",
			ExitCode:       -1,
			Error:          err.Error(),
		})
		return botRunCommandOut{}, err
	}
	if decision == ApprovalDeny {
		r.writeCommandAudit(commandAuditEntry{
			Timestamp:      time.Now().UTC().Format(time.RFC3339),
			BotID:          r.bot.ID,
			BotSlug:        r.bot.Slug,
			ConversationID: r.currentConversationID(),
			Command:        cmdStr,
			Decision:       string(ApprovalDeny),
			ExitCode:       -1,
		})
		if r.approver == nil {
			return botRunCommandOut{}, errors.New("command requires approval (run interactively or pass --allow-commands)")
		}
		return botRunCommandOut{}, errors.New("command denied by user")
	}

	timeoutSecs := args.TimeoutSeconds
	if timeoutSecs <= 0 {
		timeoutSecs = defaultCmdTimeoutSecs
	}
	if timeoutSecs > maxCmdTimeoutSecs {
		timeoutSecs = maxCmdTimeoutSecs
	}

	scratchDir := filepath.Join(r.bot.HomePath, bot.ScratchDirName)
	if err := os.MkdirAll(filepath.Join(scratchDir, bot.ScratchOutDirName), 0o755); err != nil {
		return botRunCommandOut{}, fmt.Errorf("ensure scratch dir: %w", err)
	}
	if err := verifyRealPathWithinRoot(r.bot.HomePath, scratchDir); err != nil {
		return botRunCommandOut{}, err
	}

	tmpDir := filepath.Join(r.bot.HomePath, ".tmp")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return botRunCommandOut{}, fmt.Errorf("ensure bot tmp dir: %w", err)
	}
	uvCacheDir := r.resolveUVCacheDir()
	_ = os.MkdirAll(uvCacheDir, 0o755)

	beforeSnap := snapshotScratch(scratchDir)

	runCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSecs)*time.Second)
	defer cancel()

	cmd := buildShellCommand(runCtx, cmdStr)
	cmd.Dir = scratchDir
	// Keep Stdin nil (/dev/null) so child commands never block on or hijack the interactive TTY.
	cmd.Stdin = nil
	cmd.Env = r.buildCommandEnv(scratchDir, tmpDir, uvCacheDir)

	policy := Policy{
		ReadWrite: []string{scratchDir, tmpDir, uvCacheDir},
		Network:   true, // Documented v0 exception for uv/PyPI until bundled Phase 3 runtime lands.
	}
	for _, wsID := range r.order {
		policy.ReadOnly = append(policy.ReadOnly, r.workspaces[wsID].RootPath)
	}
	if err := r.sandbox.Prepare(cmd, policy); err != nil {
		r.writeCommandAudit(commandAuditEntry{
			Timestamp:      time.Now().UTC().Format(time.RFC3339),
			BotID:          r.bot.ID,
			BotSlug:        r.bot.Slug,
			ConversationID: r.currentConversationID(),
			Command:        cmdStr,
			Decision:       string(decision),
			ExitCode:       -1,
			Error:          "prepare sandbox: " + err.Error(),
		})
		return botRunCommandOut{}, fmt.Errorf("prepare sandbox: %w", err)
	}

	stdoutBuf := newHeadTailBuffer(cmdOutputHeadBytes, cmdOutputTailBytes)
	stderrBuf := newHeadTailBuffer(cmdOutputHeadBytes, cmdOutputTailBytes)
	cmd.Stdout = stdoutBuf
	cmd.Stderr = stderrBuf

	start := time.Now()
	runErr := cmd.Run()
	// Always terminate any lingering background children in the process group even after normal exit.
	cleanupProcGroup(cmd)
	dur := time.Since(start)

	afterSnap := snapshotScratch(scratchDir)
	filesChanged, filesChangedTotal, filesChangedTrunc := diffScratchSnapshots(beforeSnap, afterSnap, maxFilesChangedReport)

	stdoutText, stdoutTotal, stdoutTrunc := stdoutBuf.Format()
	stderrText, stderrTotal, stderrTrunc := stderrBuf.Format()

	out := botRunCommandOut{
		Cwd:                   bot.ScratchDirName + "/",
		Stdout:                stdoutText,
		Stderr:                stderrText,
		StdoutBytesTotal:      stdoutTotal,
		StderrBytesTotal:      stderrTotal,
		StdoutTruncated:       stdoutTrunc,
		StderrTruncated:       stderrTrunc,
		DurationMS:            dur.Milliseconds(),
		FilesChanged:          filesChanged,
		FilesChangedTotal:     filesChangedTotal,
		FilesChangedTruncated: filesChangedTrunc,
	}

	audit := commandAuditEntry{
		Timestamp:      time.Now().UTC().Format(time.RFC3339),
		BotID:          r.bot.ID,
		BotSlug:        r.bot.Slug,
		ConversationID: r.currentConversationID(),
		Command:        cmdStr,
		Decision:       string(decision),
		DurationMS:     dur.Milliseconds(),
		FilesChanged:   filesChanged,
	}

	// Check parent context cancellation FIRST (e.g., Ctrl-C) before timeout or ExitError
	// so the agent stops the turn cleanly instead of misreporting exit_code=-1.
	if ctx.Err() != nil {
		audit.ExitCode = -1
		audit.Signal = "canceled"
		audit.Error = ctx.Err().Error()
		r.writeCommandAudit(audit)
		return botRunCommandOut{}, ctx.Err()
	}

	if runCtx.Err() == context.DeadlineExceeded {
		out.TimedOut = true
		out.ExitCode = -1
		out.Signal = extractSignal(runErr)
	} else if errors.Is(runErr, exec.ErrWaitDelay) {
		// The main shell exited, and lingering background processes holding stdout/stderr
		// were terminated by WaitDelay + cleanupProcGroup.
		if cmd.ProcessState != nil {
			out.ExitCode = cmd.ProcessState.ExitCode()
		}
		out.Note = "background processes holding output streams were terminated"
	} else if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			out.ExitCode = exitErr.ExitCode()
			out.Signal = extractSignal(runErr)
		} else {
			audit.ExitCode = -1
			audit.Error = runErr.Error()
			r.writeCommandAudit(audit)
			return botRunCommandOut{}, fmt.Errorf("run command: %w", runErr)
		}
	}

	audit.ExitCode = out.ExitCode
	audit.Signal = out.Signal
	audit.TimedOut = out.TimedOut
	r.writeCommandAudit(audit)
	return out, nil
}

func (r *Registry) authorizeCommand(ctx context.Context, cmdStr string) (ApprovalDecision, error) {
	r.mu.Lock()
	if r.allowCmdSession {
		r.mu.Unlock()
		return ApprovalAllowSession, nil
	}
	approver := r.approver
	r.mu.Unlock()

	if approver == nil {
		return ApprovalDeny, nil
	}

	dec, err := approver(ctx, ApprovalRequest{
		Kind:    ApprovalKindCommand,
		BotID:   r.bot.ID,
		BotSlug: r.bot.Slug,
		BotName: r.bot.Name,
		Tool:    "bot_run_command",
		Command: cmdStr,
		Cwd:     bot.ScratchDirName + "/",
	})
	if err != nil {
		return ApprovalDeny, err
	}
	if dec == ApprovalAllowSession {
		r.mu.Lock()
		r.allowCmdSession = true
		r.mu.Unlock()
		return ApprovalAllowSession, nil
	}
	if dec == ApprovalAllow {
		return ApprovalAllow, nil
	}
	return ApprovalDeny, nil
}

func (r *Registry) resolveUVCacheDir() string {
	if r.dataDir != "" {
		return filepath.Join(r.dataDir, "uv-cache")
	}
	if dbPath := r.store.Path(); dbPath != "" && dbPath != ":memory:" && !strings.HasPrefix(dbPath, "file::memory:") {
		return filepath.Join(filepath.Dir(dbPath), "uv-cache")
	}
	if dir, err := store.DefaultDataDir(); err == nil && dir != "" {
		return filepath.Join(dir, "uv-cache")
	}
	return filepath.Join(r.bot.HomePath, ".uv-cache")
}

func (r *Registry) buildCommandEnv(scratchDir, tmpDir, uvCacheDir string) []string {
	var env []string
	var pathVal string

	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if !isAllowedEnvKey(k) {
			continue
		}
		if strings.EqualFold(k, "PATH") {
			pathVal = v
			continue
		}
		// Override TMPDIR/TEMP/TMP below with the bot's own .tmp directory.
		if strings.EqualFold(k, "TMPDIR") || strings.EqualFold(k, "TEMP") || strings.EqualFold(k, "TMP") {
			continue
		}
		env = append(env, kv)
	}

	// Prepend managed Python bin dir when available (Phase 3 bundled runtime seam).
	rt := ResolvePython()
	if rt.Managed && rt.BinDir != "" {
		if pathVal != "" {
			pathVal = rt.BinDir + string(os.PathListSeparator) + pathVal
		} else {
			pathVal = rt.BinDir
		}
	}
	if pathVal != "" {
		env = append(env, "PATH="+pathVal)
	}

	env = append(env,
		"TMPDIR="+tmpDir,
		"TEMP="+tmpDir,
		"TMP="+tmpDir,
		"PYTHONNOUSERSITE=1",
		"PYTHONDONTWRITEBYTECODE=1",
		"PYTHONUNBUFFERED=1",
		"MPLBACKEND=Agg",
		"NO_COLOR=1",
		"UV_NO_PROGRESS=1",
		"UV_CACHE_DIR="+uvCacheDir,
		"FARO_BOT_DIR="+r.bot.HomePath,
		"FARO_SCRATCH_DIR="+scratchDir,
	)

	if len(r.order) > 0 {
		wsRO := make(map[string]string, len(r.order))
		for _, wsID := range r.order {
			ws := r.workspaces[wsID]
			wsRO[strconv.FormatInt(ws.ID, 10)] = ws.RootPath
		}
		if b, err := json.Marshal(wsRO); err == nil {
			env = append(env, "FARO_WORKSPACES_READONLY="+string(b))
		}
	}
	return env
}

func isAllowedEnvKey(k string) bool {
	ku := strings.ToUpper(k)
	// Allow standard proxy and TLS CA bundle variables on all platforms so uv works behind corporate proxies.
	switch ku {
	case "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "SSL_CERT_FILE", "SSL_CERT_DIR", "REQUESTS_CA_BUNDLE":
		return true
	}
	if runtime.GOOS == "windows" {
		switch ku {
		case "PATH", "SYSTEMROOT", "SYSTEMDRIVE", "COMSPEC", "PATHEXT", "WINDIR",
			"USERPROFILE", "USERNAME", "HOMEDRIVE", "HOMEPATH",
			"APPDATA", "LOCALAPPDATA", "TEMP", "TMP", "PROGRAMDATA",
			"PROCESSOR_ARCHITECTURE", "NUMBER_OF_PROCESSORS", "OS":
			return true
		default:
			return false
		}
	}
	switch k {
	case "PATH", "HOME", "USER", "LOGNAME", "LANG", "TZ", "TERM":
		return true
	default:
		return strings.HasPrefix(k, "LC_")
	}
}

func (r *Registry) writeCommandAudit(entry commandAuditEntry) {
	if r.bot == nil || r.bot.HomePath == "" {
		return
	}
	auditDir := filepath.Join(r.bot.HomePath, ".faro")
	if err := os.MkdirAll(auditDir, 0o755); err != nil {
		return
	}
	b, err := json.Marshal(entry)
	if err != nil {
		return
	}
	logPath := filepath.Join(auditDir, "commands.log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}

type scratchFileStat struct {
	size    int64
	mtimeNS int64
}

func snapshotScratch(scratchDir string) map[string]scratchFileStat {
	snap := make(map[string]scratchFileStat)
	_ = filepath.WalkDir(scratchDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if path == scratchDir {
				return nil
			}
			if isIgnoredScratchName(name, true) {
				return filepath.SkipDir
			}
			return nil
		}
		if isIgnoredScratchName(name, false) {
			return nil
		}
		rel, err := filepath.Rel(scratchDir, path)
		if err != nil {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		snap[filepath.ToSlash(rel)] = scratchFileStat{
			size:    info.Size(),
			mtimeNS: info.ModTime().UnixNano(),
		}
		return nil
	})
	return snap
}

func diffScratchSnapshots(before, after map[string]scratchFileStat, maxEntries int) (changed []string, total int, truncated bool) {
	for rel, stAfter := range after {
		stBefore, existed := before[rel]
		if !existed || stBefore.size != stAfter.size || stBefore.mtimeNS != stAfter.mtimeNS {
			changed = append(changed, bot.ScratchDirName+"/"+rel)
		}
	}
	for rel := range before {
		if _, exists := after[rel]; !exists {
			changed = append(changed, "deleted: "+bot.ScratchDirName+"/"+rel)
		}
	}
	sort.Strings(changed)
	total = len(changed)
	if total > maxEntries {
		changed = changed[:maxEntries]
		truncated = true
	}
	return changed, total, truncated
}

// cleanBotRelPath normalizes a bot-relative path using path.Clean and rejects any
// control characters, traversal ('..'), absolute paths, or Windows backslash escapes BEFORE allowlist checks.
func cleanBotRelPath(raw string, allowRoot bool) (string, error) {
	if err := validateSafePathRunes(raw); err != nil {
		return "", err
	}
	s := strings.TrimSpace(raw)
	if s == "" || s == "." || s == "./" {
		if allowRoot {
			return "", nil
		}
		return "", errors.New("path is required")
	}
	s = strings.ReplaceAll(s, "\\", "/")
	if strings.HasPrefix(s, "/") || filepath.IsAbs(s) {
		return "", fmt.Errorf("absolute path %q is not allowed", raw)
	}
	for _, seg := range strings.Split(s, "/") {
		if seg == ".." {
			return "", fmt.Errorf("path %q must not contain '..' traversal segments", raw)
		}
	}
	cleaned := path.Clean(s)
	if cleaned == "." || cleaned == "" {
		if allowRoot {
			return "", nil
		}
		return "", fmt.Errorf("invalid path %q", raw)
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("path %q escapes bot folder", raw)
	}
	return cleaned, nil
}

func isIgnoredScratchName(name string, isDir bool) bool {
	if strings.HasPrefix(name, ".") {
		return true
	}
	if isDir {
		switch name {
		case "__pycache__", "node_modules", "venv", "env":
			return true
		}
	}
	return false
}

func isInternalBotPath(rel string) bool {
	if rel == "" {
		return false
	}
	for _, seg := range strings.Split(rel, "/") {
		if strings.HasPrefix(seg, ".") {
			return true
		}
	}
	return false
}

// verifyExistingAncestorsWithinRoot walks up from targetPath until an existing directory is found
// and ensures that directory resolves inside rootPath (preventing symlink-directory writes).
func verifyExistingAncestorsWithinRoot(rootPath, targetPath string) error {
	cur := filepath.Dir(targetPath)
	for {
		if _, err := os.Lstat(cur); err == nil {
			return verifyRealPathWithinRoot(rootPath, cur)
		} else if !os.IsNotExist(err) {
			return err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return errors.New("could not resolve parent directory inside root directory")
		}
		cur = parent
	}
}
