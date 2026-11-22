package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zeebo/blake3"

	"github.com/SayarB/faro-harness/internal/bot"
	"github.com/SayarB/faro-harness/internal/fsutil"
	"github.com/SayarB/faro-harness/internal/llm"
	"github.com/SayarB/faro-harness/internal/store"
	"github.com/SayarB/faro-harness/internal/worker"
)

const (
	maxDeliverFileBytes = 100 * 1024 * 1024 // 100 MB safety ceiling for staged binary deliverables
	maxBackupFileBytes  = 250 * 1024 * 1024 // 250 MB safety ceiling for pre-overwrite shadow backups
	maxApprovalPreview  = 160
)

var windowsReservedNames = map[string]struct{}{
	"CON": {}, "PRN": {}, "AUX": {}, "NUL": {},
	"COM1": {}, "COM2": {}, "COM3": {}, "COM4": {}, "COM5": {}, "COM6": {}, "COM7": {}, "COM8": {}, "COM9": {},
	"LPT1": {}, "LPT2": {}, "LPT3": {}, "LPT4": {}, "LPT5": {}, "LPT6": {}, "LPT7": {}, "LPT8": {}, "LPT9": {},
}

func deliverFileToolDefinition(hasBot bool) llm.ToolDef {
	if hasBot {
		return llm.ToolDef{
			Name:        "deliver_file",
			Description: "Save a deliverable file into a mounted user workspace folder (requires user approval). Provide EITHER scratch_path (must start with 'scratch/', e.g. 'scratch/out/employee_data.xlsx', to promote a generated file such as .xlsx, .pdf, or .docx) OR content (UTF-8 text up to 256 KB for Markdown, CSV, JSON, etc.). Existing files are never overwritten unless overwrite=true; ask the user before overwriting, and Faro will automatically back up the previous file first.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"workspace_id": {
						"type": "integer",
						"description": "Target user workspace ID (optional when only one workspace is mounted)."
					},
					"dest_rel_path": {
						"type": "string",
						"description": "Relative destination path inside the user workspace (e.g. 'employee_data.xlsx' or 'Reports/summary.md'). Subfolders are created automatically."
					},
					"scratch_path": {
						"type": "string",
						"description": "Relative path starting with 'scratch/' to copy into the workspace (e.g. 'scratch/out/employee_data.xlsx'). Mutually exclusive with content."
					},
					"content": {
						"type": "string",
						"description": "UTF-8 text content to write directly (up to 256 KB). Mutually exclusive with scratch_path."
					},
					"overwrite": {
						"type": "boolean",
						"description": "Set to true to replace an existing file at dest_rel_path (default false). Ask the user first; Faro backs up the original before overwriting."
					}
				},
				"required": ["dest_rel_path"]
			}`),
		}
	}
	return llm.ToolDef{
		Name:        "deliver_file",
		Description: "Create or update a UTF-8 text file (Markdown, CSV, JSON, plain text, up to 256 KB) in a mounted user workspace folder (requires user approval). Existing files are never overwritten unless overwrite=true; ask the user before overwriting, and Faro will automatically back up the previous file first.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"workspace_id": {
					"type": "integer",
					"description": "Target user workspace ID (optional when only one workspace is mounted)."
				},
				"dest_rel_path": {
					"type": "string",
					"description": "Relative destination path inside the user workspace (e.g. 'summary.md' or 'Reports/notes.csv'). Subfolders are created automatically."
				},
				"content": {
					"type": "string",
					"description": "UTF-8 text content to write (up to 256 KB)."
				},
				"overwrite": {
					"type": "boolean",
					"description": "Set to true to replace an existing file at dest_rel_path (default false). Ask the user first; Faro backs up the original before overwriting."
				}
			},
			"required": ["dest_rel_path", "content"]
		}`),
	}
}

type deliverFileArgs struct {
	WorkspaceID int64   `json:"workspace_id"`
	DestRelPath string  `json:"dest_rel_path"`
	ScratchPath string  `json:"scratch_path"`
	Content     *string `json:"content"`
	Overwrite   bool    `json:"overwrite"`
}

type deliverFileOut struct {
	WorkspaceID   int64  `json:"workspace_id"`
	WorkspaceName string `json:"workspace_name"`
	RelPath       string `json:"rel_path"`
	AbsPath       string `json:"abs_path"`
	FileID        int64  `json:"file_id,omitempty"`
	SizeBytes     int64  `json:"size_bytes"`
	Overwrote     bool   `json:"overwrote"`
	BackupPath    string `json:"backup_path,omitempty"`
	Indexed       bool   `json:"indexed"`
	IndexNote     string `json:"index_note,omitempty"`
}

type fileChangeAuditEntry struct {
	Timestamp      string `json:"timestamp"`
	BotID          string `json:"bot_id,omitempty"`
	BotSlug        string `json:"bot_slug,omitempty"`
	ConversationID string `json:"conversation_id,omitempty"`
	Op             string `json:"op"`
	Decision       string `json:"decision"`
	WorkspaceID    int64  `json:"workspace_id"`
	WorkspaceRoot  string `json:"workspace_root"`
	DestRelPath    string `json:"dest_rel_path"`
	SourcePath     string `json:"source_path,omitempty"`
	SizeBytes      int64  `json:"size_bytes,omitempty"`
	BackupPath     string `json:"backup_path,omitempty"`
	Error          string `json:"error,omitempty"`
}

func (r *Registry) execDeliverFile(ctx context.Context, rawArgs string) (deliverFileOut, error) {
	if len(r.workspaces) == 0 {
		return deliverFileOut{}, errors.New("no user workspaces are mounted in this session")
	}

	var args deliverFileArgs
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		return deliverFileOut{}, fmt.Errorf("invalid deliver_file arguments: %w", err)
	}

	ws, err := r.resolveWorkspace(args.WorkspaceID)
	if err != nil {
		return deliverFileOut{}, err
	}
	wsName := workspaceDisplayName(ws.RootPath)

	hasScratch := strings.TrimSpace(args.ScratchPath) != ""
	hasContent := args.Content != nil
	if hasScratch && hasContent {
		return deliverFileOut{}, errors.New("provide either scratch_path or content, not both")
	}
	if !hasScratch && !hasContent {
		if r.bot != nil {
			return deliverFileOut{}, errors.New("either scratch_path or content is required")
		}
		return deliverFileOut{}, errors.New("content is required")
	}

	destRel, err := cleanWorkspaceDestPath(args.DestRelPath)
	if err != nil {
		return deliverFileOut{}, err
	}
	destAbs, err := confinePath(ws.RootPath, destRel)
	if err != nil {
		return deliverFileOut{}, err
	}
	if err := verifyExistingAncestorsWithinRoot(ws.RootPath, destAbs); err != nil {
		return deliverFileOut{}, err
	}
	if err := r.verifyNotInsideFaroState(destAbs); err != nil {
		return deliverFileOut{}, err
	}

	// Inspect and validate source before prompting.
	var srcRel string
	var srcAbs string
	var preApprovalSrcInfo os.FileInfo
	var sourceLabel string
	var preview string
	var sizeBytes int64

	if hasScratch {
		if r.bot == nil {
			return deliverFileOut{}, errors.New("scratch_path requires a bot session; pass content instead")
		}
		srcRel, err = cleanBotRelPath(args.ScratchPath, false)
		if err != nil {
			return deliverFileOut{}, err
		}
		if isInternalBotPath(srcRel) || !strings.HasPrefix(srcRel, bot.ScratchDirName+"/") {
			return deliverFileOut{}, fmt.Errorf("scratch_path must start with %s/ (got %q)", bot.ScratchDirName, srcRel)
		}
		srcAbs, err = confinePath(r.bot.HomePath, srcRel)
		if err != nil {
			return deliverFileOut{}, err
		}
		srcInfo, err := os.Lstat(srcAbs)
		if err != nil {
			if os.IsNotExist(err) {
				return deliverFileOut{}, fmt.Errorf("staged file %q does not exist in your bot folder", srcRel)
			}
			return deliverFileOut{}, fmt.Errorf("stat staged file %q: %w", srcRel, err)
		}
		if srcInfo.IsDir() {
			return deliverFileOut{}, fmt.Errorf("staged path %q is a directory, not a file", srcRel)
		}
		if srcInfo.Mode()&os.ModeSymlink != 0 {
			return deliverFileOut{}, fmt.Errorf("staged path %q is a symlink and cannot be delivered", srcRel)
		}
		if err := verifyRealPathWithinRoot(r.bot.HomePath, srcAbs); err != nil {
			return deliverFileOut{}, err
		}
		if srcInfo.Size() > maxDeliverFileBytes {
			return deliverFileOut{}, fmt.Errorf("staged file %q exceeds max delivery size (%d bytes)", srcRel, maxDeliverFileBytes)
		}
		preApprovalSrcInfo = srcInfo
		sizeBytes = srcInfo.Size()
		sourceLabel = srcRel
	} else {
		if len(*args.Content) > maxBotWriteBytes {
			return deliverFileOut{}, fmt.Errorf("content exceeds max single write size (%d bytes)", maxBotWriteBytes)
		}
		if !utf8.ValidString(*args.Content) {
			return deliverFileOut{}, errors.New("content must be valid UTF-8 text (use scratch_path for binary files)")
		}
		sizeBytes = int64(len(*args.Content))
		sourceLabel = "(inline text)"
		preview = buildContentPreview(*args.Content, maxApprovalPreview)
	}

	// Inspect destination before prompting so missing overwrite=true fails fast without nagging the user.
	willOverwrite := false
	var existingSize int64
	if destInfo, err := os.Lstat(destAbs); err == nil {
		if destInfo.IsDir() {
			return deliverFileOut{}, fmt.Errorf("destination %q is an existing folder", destRel)
		}
		if destInfo.Mode()&os.ModeSymlink != 0 {
			return deliverFileOut{}, fmt.Errorf("destination %q is a symlink and cannot be overwritten", destRel)
		}
		if fsutil.IsCloudPlaceholder(destAbs, destInfo) {
			return deliverFileOut{}, fmt.Errorf("destination %q is a cloud-only placeholder and cannot be overwritten until downloaded", destRel)
		}
		willOverwrite = true
		existingSize = destInfo.Size()
		if !args.Overwrite {
			return deliverFileOut{}, fmt.Errorf(
				"file %q already exists in workspace %q (%d bytes); ask the user for confirmation before retrying with overwrite=true (Faro will back up the existing file first), or choose a different filename",
				destRel, wsName, destInfo.Size(),
			)
		}
	} else if !os.IsNotExist(err) {
		return deliverFileOut{}, fmt.Errorf("stat destination %q: %w", destRel, err)
	}

	op := "create"
	kind := ApprovalKindFileCreate
	if willOverwrite {
		op = "overwrite"
		kind = ApprovalKindFileOverwrite
	}

	botID, botSlug, botName := r.actorIdentity()
	req := ApprovalRequest{
		Kind:              kind,
		BotID:             botID,
		BotSlug:           botSlug,
		BotName:           botName,
		Tool:              "deliver_file",
		WorkspaceID:       ws.ID,
		WorkspaceName:     wsName,
		WorkspaceRoot:     ws.RootPath,
		SourcePath:        sourceLabel,
		DestRelPath:       destRel,
		SizeBytes:         sizeBytes,
		ExistingSizeBytes: existingSize,
		Preview:           preview,
		WillOverwrite:     willOverwrite,
	}

	decision, err := r.authorizeDelivery(ctx, req)
	if err != nil {
		if errors.Is(err, ErrApprovalNonInteractive) {
			r.writeChangeAudit(fileChangeAuditEntry{
				Timestamp:      time.Now().UTC().Format(time.RFC3339),
				BotID:          botID,
				BotSlug:        botSlug,
				ConversationID: r.currentConversationID(),
				Op:             op,
				Decision:       "non_interactive_denied",
				WorkspaceID:    ws.ID,
				WorkspaceRoot:  ws.RootPath,
				DestRelPath:    destRel,
				SourcePath:     sourceLabel,
				SizeBytes:      sizeBytes,
				Error:          err.Error(),
			})
			if willOverwrite {
				return deliverFileOut{}, errors.New("file overwrite requires approval (run interactively or pass --allow-overwrites)")
			}
			return deliverFileOut{}, errors.New("file delivery requires approval (run interactively or pass --allow-writes)")
		}
		r.writeChangeAudit(fileChangeAuditEntry{
			Timestamp:      time.Now().UTC().Format(time.RFC3339),
			BotID:          botID,
			BotSlug:        botSlug,
			ConversationID: r.currentConversationID(),
			Op:             op,
			Decision:       "approver_error",
			WorkspaceID:    ws.ID,
			WorkspaceRoot:  ws.RootPath,
			DestRelPath:    destRel,
			SourcePath:     sourceLabel,
			SizeBytes:      sizeBytes,
			Error:          err.Error(),
		})
		return deliverFileOut{}, err
	}
	if decision == ApprovalDeny {
		r.writeChangeAudit(fileChangeAuditEntry{
			Timestamp:      time.Now().UTC().Format(time.RFC3339),
			BotID:          botID,
			BotSlug:        botSlug,
			ConversationID: r.currentConversationID(),
			Op:             op,
			Decision:       string(ApprovalDeny),
			WorkspaceID:    ws.ID,
			WorkspaceRoot:  ws.RootPath,
			DestRelPath:    destRel,
			SourcePath:     sourceLabel,
			SizeBytes:      sizeBytes,
		})
		if r.approver == nil {
			if willOverwrite {
				return deliverFileOut{}, errors.New("file overwrite requires approval (run interactively or pass --allow-overwrites)")
			}
			return deliverFileOut{}, errors.New("file delivery requires approval (run interactively or pass --allow-writes)")
		}
		return deliverFileOut{}, errors.New("file delivery denied by user")
	}

	// From here on, the action was approved: audit any failure or success uniformly.
	var backupPath string
	var written int64
	auditErr := func(opErr error) (deliverFileOut, error) {
		r.writeChangeAudit(fileChangeAuditEntry{
			Timestamp:      time.Now().UTC().Format(time.RFC3339),
			BotID:          botID,
			BotSlug:        botSlug,
			ConversationID: r.currentConversationID(),
			Op:             op,
			Decision:       string(decision),
			WorkspaceID:    ws.ID,
			WorkspaceRoot:  ws.RootPath,
			DestRelPath:    destRel,
			SourcePath:     sourceLabel,
			SizeBytes:      written,
			BackupPath:     backupPath,
			Error:          opErr.Error(),
		})
		return deliverFileOut{}, opErr
	}

	// Re-check destination state right before committing to prevent TOCTOU clobbering.
	if err := verifyExistingAncestorsWithinRoot(ws.RootPath, destAbs); err != nil {
		return auditErr(err)
	}
	if err := r.verifyNotInsideFaroState(destAbs); err != nil {
		return auditErr(err)
	}

	var origPerm os.FileMode
	var preBackupSize int64
	var preBackupMtime time.Time
	hasOrigPerm := false

	if destInfo, err := os.Lstat(destAbs); err == nil {
		if destInfo.IsDir() {
			return auditErr(fmt.Errorf("destination %q is an existing folder", destRel))
		}
		if destInfo.Mode()&os.ModeSymlink != 0 {
			return auditErr(fmt.Errorf("destination %q is a symlink and cannot be overwritten", destRel))
		}
		if fsutil.IsCloudPlaceholder(destAbs, destInfo) {
			return auditErr(fmt.Errorf("destination %q is a cloud-only placeholder and cannot be overwritten", destRel))
		}
		if !willOverwrite {
			return auditErr(fmt.Errorf("destination %q was created while awaiting approval; ask the user before retrying with overwrite=true", destRel))
		}
		origPerm = destInfo.Mode().Perm()
		hasOrigPerm = true
		preBackupSize = destInfo.Size()
		preBackupMtime = destInfo.ModTime()
		if preBackupSize > maxBackupFileBytes {
			return auditErr(fmt.Errorf("existing file %q (%d bytes) exceeds max automatic backup size (%d bytes); refusing to overwrite without backup", destRel, preBackupSize, maxBackupFileBytes))
		}
		backupPath, err = r.backupExistingFile(destAbs, botSlug)
		if err != nil {
			return auditErr(fmt.Errorf("backup existing file before overwrite: %w", err))
		}
	} else if !os.IsNotExist(err) {
		return auditErr(fmt.Errorf("stat destination %q: %w", destRel, err))
	} else {
		// File was removed between prompt and write; treat as a clean create.
		willOverwrite = false
		op = "create"
	}

	parentDir := filepath.Dir(destAbs)
	if err := os.MkdirAll(parentDir, 0o755); err != nil {
		return auditErr(fmt.Errorf("create destination folder: %w", err))
	}
	if err := verifyRealPathWithinRoot(ws.RootPath, parentDir); err != nil {
		return auditErr(err)
	}
	if err := r.verifyNotInsideFaroState(parentDir); err != nil {
		return auditErr(err)
	}

	// Write atomically via a temp file whose name is ignored by fsutil.IsIgnoredFile (*.tmp).
	// On new files, open with 0o666 so the user's process umask applies naturally.
	// On overwrites, open with origPerm (and chmod to preserve exact mode bits).
	createMode := os.FileMode(0o666)
	if hasOrigPerm {
		createMode = origPerm
	}
	tmpFile, tmpName, err := createTempWithMode(parentDir, ".faro-deliver-", ".tmp", createMode)
	if err != nil {
		return auditErr(fmt.Errorf("create temp file: %w", err))
	}
	cleanupTmp := true
	defer func() {
		if cleanupTmp {
			_ = os.Remove(tmpName)
		}
	}()

	if hasScratch {
		if err := verifyRealPathWithinRoot(r.bot.HomePath, srcAbs); err != nil {
			_ = tmpFile.Close()
			return auditErr(err)
		}
		srcFile, openedInfo, err := openRegularNoFollow(srcAbs)
		if err != nil {
			_ = tmpFile.Close()
			return auditErr(fmt.Errorf("open staged file %q: %w", srcRel, err))
		}
		if !os.SameFile(preApprovalSrcInfo, openedInfo) || openedInfo.Size() != sizeBytes {
			_ = srcFile.Close()
			_ = tmpFile.Close()
			return auditErr(fmt.Errorf("staged file %q changed while awaiting approval; retry delivery", srcRel))
		}
		n, copyErr := io.Copy(tmpFile, io.LimitReader(srcFile, maxDeliverFileBytes+1))
		_ = srcFile.Close()
		if copyErr != nil {
			_ = tmpFile.Close()
			return auditErr(fmt.Errorf("copy staged file %q: %w", srcRel, copyErr))
		}
		if n > maxDeliverFileBytes {
			_ = tmpFile.Close()
			return auditErr(fmt.Errorf("staged file %q exceeds max delivery size (%d bytes)", srcRel, maxDeliverFileBytes))
		}
		written = n
	} else {
		n, writeErr := tmpFile.WriteString(*args.Content)
		if writeErr != nil {
			_ = tmpFile.Close()
			return auditErr(fmt.Errorf("write content: %w", writeErr))
		}
		written = int64(n)
	}

	if err := tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		return auditErr(fmt.Errorf("sync temp file: %w", err))
	}
	if err := tmpFile.Close(); err != nil {
		return auditErr(fmt.Errorf("close temp file: %w", err))
	}
	if hasOrigPerm {
		if err := os.Chmod(tmpName, origPerm); err != nil {
			return auditErr(fmt.Errorf("preserve file permissions: %w", err))
		}
	}

	// Re-verify parent dir, temp file, and target state immediately before rename to close TOCTOU windows.
	if err := verifyRealPathWithinRoot(ws.RootPath, parentDir); err != nil {
		return auditErr(err)
	}
	if err := verifyRealPathWithinRoot(ws.RootPath, tmpName); err != nil {
		return auditErr(err)
	}
	if err := r.verifyNotInsideFaroState(parentDir); err != nil {
		return auditErr(err)
	}
	if willOverwrite {
		afterInfo, err := os.Lstat(destAbs)
		if err != nil {
			return auditErr(fmt.Errorf("destination %q changed before overwrite commit: %w", destRel, err))
		}
		if afterInfo.IsDir() || afterInfo.Mode()&os.ModeSymlink != 0 {
			return auditErr(fmt.Errorf("destination %q changed type before overwrite commit", destRel))
		}
		if afterInfo.Size() != preBackupSize || !afterInfo.ModTime().Equal(preBackupMtime) {
			return auditErr(fmt.Errorf("destination %q was modified on disk while preparing overwrite; aborting to preserve your edits", destRel))
		}
	} else {
		if _, err := os.Lstat(destAbs); err == nil {
			return auditErr(fmt.Errorf("destination %q appeared on disk before commit; ask the user before retrying with overwrite=true", destRel))
		} else if !os.IsNotExist(err) {
			return auditErr(fmt.Errorf("stat destination %q before commit: %w", destRel, err))
		}
	}

	if err := os.Rename(tmpName, destAbs); err != nil {
		if runtime.GOOS == "windows" {
			return auditErr(fmt.Errorf("move delivered file into place (if %q is open in Excel or another app, close it and retry): %w", destRel, err))
		}
		return auditErr(fmt.Errorf("move delivered file into place: %w", err))
	}
	cleanupTmp = false

	r.writeChangeAudit(fileChangeAuditEntry{
		Timestamp:      time.Now().UTC().Format(time.RFC3339),
		BotID:          botID,
		BotSlug:        botSlug,
		ConversationID: r.currentConversationID(),
		Op:             op,
		Decision:       string(decision),
		WorkspaceID:    ws.ID,
		WorkspaceRoot:  ws.RootPath,
		DestRelPath:    destRel,
		SourcePath:     sourceLabel,
		SizeBytes:      written,
		BackupPath:     backupPath,
	})

	out := deliverFileOut{
		WorkspaceID:   ws.ID,
		WorkspaceName: wsName,
		RelPath:       destRel,
		AbsPath:       destAbs,
		SizeBytes:     written,
		Overwrote:     willOverwrite,
		BackupPath:    backupPath,
	}

	// Immediately index the delivered file using the existing workspace record (no EnsureWorkspace upsert).
	if cat, idxErr := worker.NewCatalogIndexerForWorkspace(r.store, ws); idxErr != nil {
		out.IndexNote = "file saved on disk, but catalog refresh failed: " + idxErr.Error()
	} else if idxErr := cat.IndexFile(ctx, destAbs); idxErr != nil {
		out.IndexNote = "file saved on disk, but catalog refresh failed: " + idxErr.Error()
	} else {
		out.Indexed = true
		if entry, getErr := r.store.GetFileByPath(ctx, ws.ID, destRel); getErr == nil && entry != nil {
			out.FileID = entry.File.ID
		}
	}

	return out, nil
}

func (r *Registry) authorizeDelivery(ctx context.Context, req ApprovalRequest) (ApprovalDecision, error) {
	r.mu.Lock()
	// Session-wide write approval covers NEW file creation in this workspace ONLY; overwrites always prompt.
	if req.Kind == ApprovalKindFileCreate && r.allowWriteWorkspaces[req.WorkspaceID] {
		r.mu.Unlock()
		return ApprovalAllowSession, nil
	}
	approver := r.approver
	r.mu.Unlock()

	if approver == nil {
		return ApprovalDeny, nil
	}

	dec, err := approver(ctx, req)
	if err != nil {
		return ApprovalDeny, err
	}
	if dec == ApprovalAllowSession {
		if req.Kind == ApprovalKindFileCreate {
			r.mu.Lock()
			if r.allowWriteWorkspaces == nil {
				r.allowWriteWorkspaces = make(map[int64]bool)
			}
			r.allowWriteWorkspaces[req.WorkspaceID] = true
			r.mu.Unlock()
			return ApprovalAllowSession, nil
		}
		// Never upgrade an overwrite approval into a session-wide grant.
		return ApprovalAllow, nil
	}
	if dec == ApprovalAllow {
		return ApprovalAllow, nil
	}
	return ApprovalDeny, nil
}

func (r *Registry) actorIdentity() (id, slug, name string) {
	if r.bot != nil {
		return r.bot.ID, r.bot.Slug, r.bot.Name
	}
	return "", "faro", "Faro"
}

func (r *Registry) resolveBackupsDir(actorSlug string) (string, error) {
	if actorSlug == "" {
		actorSlug = "faro"
	}
	var base string
	switch {
	case r.dataDir != "":
		base = filepath.Join(r.dataDir, "backups")
	case r.store != nil && r.store.Path() != "" && r.store.Path() != ":memory:" && !strings.HasPrefix(r.store.Path(), "file::memory:"):
		base = filepath.Join(filepath.Dir(r.store.Path()), "backups")
	case r.bot != nil && r.bot.HomePath != "":
		base = filepath.Join(r.bot.HomePath, ".faro", "backups")
	default:
		dir, err := store.DefaultDataDir()
		if err != nil || strings.TrimSpace(dir) == "" {
			return "", errors.New("cannot resolve private backup directory")
		}
		base = filepath.Join(dir, "backups")
	}
	return filepath.Join(base, actorSlug), nil
}

func (r *Registry) backupExistingFile(srcAbs string, actorSlug string) (string, error) {
	backupDir, err := r.resolveBackupsDir(actorSlug)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		return "", err
	}
	_ = os.Chmod(backupDir, 0o700)

	in, _, err := openRegularNoFollow(srcAbs)
	if err != nil {
		return "", err
	}
	defer in.Close()

	tmp, tmpName, err := createTempWithMode(backupDir, ".faro-backup-", ".tmp", 0o600)
	if err != nil {
		return "", err
	}
	cleanupTmp := true
	defer func() {
		if cleanupTmp {
			_ = os.Remove(tmpName)
		}
	}()

	hasher := blake3.New()
	n, err := io.Copy(io.MultiWriter(tmp, hasher), io.LimitReader(in, maxBackupFileBytes+1))
	if err != nil {
		_ = tmp.Close()
		return "", err
	}
	if n > maxBackupFileBytes {
		_ = tmp.Close()
		return "", fmt.Errorf("existing file exceeds max automatic backup size (%d bytes)", maxBackupFileBytes)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}

	sum := hex.EncodeToString(hasher.Sum(nil))
	shortHash := sum
	if len(shortHash) > 12 {
		shortHash = shortHash[:12]
	}
	ts := time.Now().UTC().Format("20060102-150405.000000Z")
	baseName := sanitizeBackupFileName(filepath.Base(srcAbs))
	destPath := filepath.Join(backupDir, fmt.Sprintf("%s_%s_%s", ts, shortHash, baseName))
	if err := os.Rename(tmpName, destPath); err != nil {
		return "", err
	}
	cleanupTmp = false
	return destPath, nil
}

func sanitizeBackupFileName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return "file"
	}
	replacer := strings.NewReplacer("/", "_", "\\", "_", "\x00", "_")
	return replacer.Replace(name)
}

func (r *Registry) writeChangeAudit(entry fileChangeAuditEntry) {
	var logPath string
	switch {
	case r.bot != nil && r.bot.HomePath != "":
		logPath = filepath.Join(r.bot.HomePath, ".faro", "changes.log")
	case r.dataDir != "":
		logPath = filepath.Join(r.dataDir, "changes.log")
	case r.store != nil && r.store.Path() != "" && r.store.Path() != ":memory:" && !strings.HasPrefix(r.store.Path(), "file::memory:"):
		logPath = filepath.Join(filepath.Dir(r.store.Path()), "changes.log")
	default:
		return
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return
	}
	b, err := json.Marshal(entry)
	if err != nil {
		return
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}

// verifyNotInsideFaroState rejects destinations that fall inside Faro's own data directory,
// bots directory, or any bot home folder (e.g. if a user mounts $HOME).
func (r *Registry) verifyNotInsideFaroState(targetPath string) error {
	resolvedTarget, err := resolvePathBestEffort(targetPath)
	if err != nil {
		return err
	}

	var protected []string
	if r.dataDir != "" {
		protected = append(protected, r.dataDir)
	}
	if r.store != nil && r.store.Path() != "" && r.store.Path() != ":memory:" && !strings.HasPrefix(r.store.Path(), "file::memory:") {
		protected = append(protected, filepath.Dir(r.store.Path()))
	}
	if r.bot != nil && r.bot.HomePath != "" {
		protected = append(protected, r.bot.HomePath, filepath.Dir(r.bot.HomePath))
	}
	if d, err := store.DefaultDataDir(); err == nil && d != "" {
		protected = append(protected, d)
	}
	if d, err := store.DefaultBotsDir(); err == nil && d != "" {
		protected = append(protected, d)
	}

	for _, root := range protected {
		resolvedRoot, err := resolvePathBestEffort(root)
		if err != nil || resolvedRoot == "" {
			continue
		}
		rel, err := filepath.Rel(resolvedRoot, resolvedTarget)
		if err != nil {
			continue
		}
		if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
			return errors.New("destination resolves inside Faro's internal state directory and cannot be modified")
		}
	}
	return nil
}

// resolvePathBestEffort resolves symlinks on the deepest existing ancestor of p
// and re-attaches any non-existent trailing path components.
func resolvePathBestEffort(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real, nil
	}
	var tail []string
	cur := abs
	for {
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs, nil
		}
		tail = append([]string{filepath.Base(cur)}, tail...)
		cur = parent
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(append([]string{real}, tail...)...), nil
		}
	}
}

func createTempWithMode(dir, prefix, suffix string, mode os.FileMode) (*os.File, string, error) {
	var buf [8]byte
	for i := 0; i < 16; i++ {
		if _, err := rand.Read(buf[:]); err != nil {
			return nil, "", err
		}
		name := filepath.Join(dir, prefix+hex.EncodeToString(buf[:])+suffix)
		f, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, mode)
		if err == nil {
			return f, name, nil
		}
		if !os.IsExist(err) {
			return nil, "", err
		}
	}
	return nil, "", errors.New("could not allocate unique temp file name")
}

// validateSafePathRunes rejects ASCII control characters, DEL, C1 controls, and Unicode
// bidirectional override/isolate marks that could spoof terminal or UI approval prompts.
func validateSafePathRunes(raw string) error {
	for _, r := range raw {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return fmt.Errorf("path contains forbidden control character (U+%04X)", r)
		}
		if r == 0x061C || r == 0x200E || r == 0x200F ||
			(r >= 0x202A && r <= 0x202E) ||
			(r >= 0x2066 && r <= 0x2069) {
			return fmt.Errorf("path contains forbidden bidirectional control character (U+%04X)", r)
		}
	}
	return nil
}

func cleanWorkspaceDestPath(raw string) (string, error) {
	if err := validateSafePathRunes(raw); err != nil {
		return "", err
	}
	if strings.TrimSpace(raw) == "" {
		return "", errors.New("dest_rel_path is required")
	}
	s := raw
	if s == "." || s == "./" {
		return "", errors.New("dest_rel_path is required")
	}
	s = strings.ReplaceAll(s, "\\", "/")
	if strings.HasPrefix(s, "/") || filepath.IsAbs(s) {
		return "", fmt.Errorf("dest_rel_path must be relative to the workspace (got %q)", raw)
	}
	if strings.ContainsAny(s, ":<>\"|?*") {
		return "", fmt.Errorf("dest_rel_path %q contains characters disallowed across filesystems", raw)
	}
	for _, seg := range strings.Split(s, "/") {
		if seg == ".." {
			return "", fmt.Errorf("dest_rel_path %q must not contain '..' traversal segments", raw)
		}
	}
	cleaned := path.Clean(s)
	if cleaned == "." || cleaned == "" || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("invalid dest_rel_path %q", raw)
	}
	if !filepath.IsLocal(filepath.FromSlash(cleaned)) {
		return "", fmt.Errorf("dest_rel_path %q is not a valid local relative path", raw)
	}
	segs := strings.Split(cleaned, "/")
	for i, seg := range segs {
		if strings.EqualFold(seg, ".faro") {
			return "", fmt.Errorf("cannot write inside internal Faro folder %q", seg)
		}
		if strings.HasSuffix(seg, ".") || strings.HasSuffix(seg, " ") {
			return "", fmt.Errorf("path segment %q cannot end with a dot or space", seg)
		}
		stem := seg
		if dot := strings.IndexByte(stem, '.'); dot >= 0 {
			stem = stem[:dot]
		}
		if _, reserved := windowsReservedNames[strings.ToUpper(stem)]; reserved {
			return "", fmt.Errorf("path segment %q uses a reserved filename", seg)
		}
		if i < len(segs)-1 && fsutil.IsIgnoredDir(seg) {
			return "", fmt.Errorf("cannot write inside ignored/system folder %q", seg)
		}
	}
	base := segs[len(segs)-1]
	if fsutil.IsIgnoredFile(base) || fsutil.IsICloudStubName(base) {
		return "", fmt.Errorf("cannot write temporary/system filename %q", base)
	}
	return cleaned, nil
}

func buildContentPreview(content string, maxBytes int) string {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return "(empty)"
	}
	var b strings.Builder
	b.Grow(min(len(trimmed), maxBytes))
	for _, r := range trimmed {
		if b.Len() >= maxBytes {
			b.WriteString("...")
			break
		}
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteByte(' ')
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f):
			continue
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func workspaceDisplayName(rootPath string) string {
	name := filepath.Base(rootPath)
	if name == "" || name == "." || name == string(filepath.Separator) {
		return rootPath
	}
	return name
}
