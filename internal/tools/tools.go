// Package tools implements Faro's minimal read-only agent tools over the local catalog and workspaces.
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/SayarB/faro-harness/internal/fsutil"
	"github.com/SayarB/faro-harness/internal/llm"
	"github.com/SayarB/faro-harness/internal/peek"
	"github.com/SayarB/faro-harness/internal/store"
)

const (
	defaultSearchLimit = 10
	maxSearchLimit     = 25
	maxSnippetBytes    = 280

	defaultListLimit = 100
	maxListLimit     = 200

	defaultReadBytes = 12 * 1024
	maxReadBytes     = 16 * 1024
)

// RegistryOptions configures optional approval, sandbox, and data directory settings.
type RegistryOptions struct {
	Approver Approver
	Sandbox  Sandbox
	DataDir  string
}

// Registry exposes workspace-scoped read-only tools (plus optional isolated bot folder tools) to the agent.
type Registry struct {
	store      *store.Store
	workspaces map[int64]store.Workspace
	order      []int64
	bot        *store.Bot

	approver Approver
	sandbox  Sandbox
	dataDir  string

	mu                   sync.Mutex
	allowCmdSession      bool
	allowWriteWorkspaces map[int64]bool
	conversationID       string
}

// NewRegistry creates a tool registry restricted strictly to the provided user workspaces.
func NewRegistry(st *store.Store, workspaces []store.Workspace) (*Registry, error) {
	return NewRegistryForBot(st, workspaces, nil)
}

// NewRegistryForBot creates a tool registry scoped to user workspaces and optionally a single bot's home folder.
// A bot may start with zero user workspaces (operating only on its own memory.md and scratch/).
func NewRegistryForBot(st *store.Store, workspaces []store.Workspace, b *store.Bot) (*Registry, error) {
	return NewRegistryWithOptions(st, workspaces, b, RegistryOptions{})
}

// NewRegistryWithOptions creates a tool registry with explicit approval, sandbox, and data dir options.
func NewRegistryWithOptions(st *store.Store, workspaces []store.Workspace, b *store.Bot, opts RegistryOptions) (*Registry, error) {
	if st == nil {
		return nil, errors.New("store is required")
	}
	if len(workspaces) == 0 && b == nil {
		return nil, errors.New("at least one workspace or a bot is required")
	}
	wsMap := make(map[int64]store.Workspace, len(workspaces))
	order := make([]int64, 0, len(workspaces))
	for _, ws := range workspaces {
		wsMap[ws.ID] = ws
		order = append(order, ws.ID)
	}
	var botCopy *store.Bot
	if b != nil {
		c := *b
		botCopy = &c
	}
	sb := opts.Sandbox
	if sb == nil {
		sb = noopSandbox{}
	}
	return &Registry{
		store:      st,
		workspaces: wsMap,
		order:      order,
		bot:        botCopy,
		approver:   opts.Approver,
		sandbox:    sb,
		dataDir:    strings.TrimSpace(opts.DataDir),
	}, nil
}

// SetConversationID records the active conversation ID for command audit logs.
func (r *Registry) SetConversationID(id string) {
	r.mu.Lock()
	r.conversationID = id
	r.mu.Unlock()
}

func (r *Registry) currentConversationID() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.conversationID
}

// Definitions returns the static tool JSON schemas for the LLM.
func (r *Registry) Definitions() []llm.ToolDef {
	var defs []llm.ToolDef
	if len(r.workspaces) > 0 {
		defs = append(defs,
			llm.ToolDef{
				Name:        "search_files",
				Description: "Search indexed files across allowed workspaces by keywords, phrase, filename/folder, extension, or folder prefix. Returns compact ranked cards with file_id, rel_path, index_depth, headings, and snippets.",
				Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"query": {
						"type": "string",
						"description": "Free-form search keywords (forgiving OR matching by default)."
					},
					"workspace_id": {
						"type": "integer",
						"description": "Optional workspace ID to restrict search. Omit to search all allowed workspaces."
					},
					"folder": {
						"type": "string",
						"description": "Optional relative folder prefix filter (e.g. 'Clients/Acme')."
					},
					"ext": {
						"type": "string",
						"description": "Optional file extension filter (e.g. '.xlsx', '.pdf', 'md')."
					},
					"exact_phrase": {
						"type": "string",
						"description": "Optional exact phrase that must appear in the match."
					},
					"match_all": {
						"type": "boolean",
						"description": "If true, require all query terms (AND) instead of OR."
					},
					"limit": {
						"type": "integer",
						"description": "Max hits to return (default 10, max 25)."
					}
				}
			}`),
			},
			llm.ToolDef{
				Name:        "list_folder",
				Description: "List immediate subfolders (with file counts) and files inside a workspace folder. Use to browse when exploring or when search misses.",
				Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"workspace_id": {
						"type": "integer",
						"description": "Workspace ID. Optional when only one workspace is mounted."
					},
					"folder": {
						"type": "string",
						"description": "Relative folder path inside the workspace (omit or '' for root)."
					},
					"limit": {
						"type": "integer",
						"description": "Max entries to return (default 100, max 200)."
					}
				}
			}`),
			},
			llm.ToolDef{
				Name:        "read_file",
				Description: "Read a file by file_id (from search_files or list_folder) or by (workspace_id, rel_path). Returns paginated UTF-8 text slices for text/Markdown/CSV/code files, or the indexed card and outline for structured/binary files.",
				Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"file_id": {
						"type": "integer",
						"description": "File ID returned by search_files or list_folder."
					},
					"workspace_id": {
						"type": "integer",
						"description": "Workspace ID when addressing by rel_path (optional if only one workspace is mounted)."
					},
					"rel_path": {
						"type": "string",
						"description": "Workspace-relative path (e.g. 'Clients/Acme/notes.md')."
					},
					"offset": {
						"type": "integer",
						"description": "Byte offset to start reading from (default 0). Use next_offset from a previous call to paginate."
					},
					"limit": {
						"type": "integer",
						"description": "Max bytes to return (default 12288, max 16384)."
					}
				}
			}`),
			},
			deliverFileToolDefinition(r.bot != nil),
		)
	}
	if r.bot != nil {
		defs = append(defs, botToolDefinitions()...)
	}
	return defs
}

// Execute dispatches a tool call by name and returns a JSON payload string for the model.
// Recoverable tool errors (bad args, missing file, out-of-scope workspace) are encoded as
// JSON {"error": "..."} with a nil Go error so the agent loop can recover gracefully.
func (r *Registry) Execute(ctx context.Context, name string, rawArgs string) (string, error) {
	if strings.TrimSpace(rawArgs) == "" {
		rawArgs = "{}"
	}
	var res any
	var err error
	switch name {
	case "search_files":
		res, err = r.execSearchFiles(ctx, rawArgs)
	case "list_folder":
		res, err = r.execListFolder(ctx, rawArgs)
	case "read_file":
		res, err = r.execReadFile(ctx, rawArgs)
	case "deliver_file":
		res, err = r.execDeliverFile(ctx, rawArgs)
	case "bot_list":
		res, err = r.execBotList(ctx, rawArgs)
	case "bot_read":
		res, err = r.execBotRead(ctx, rawArgs)
	case "bot_write":
		res, err = r.execBotWrite(ctx, rawArgs)
	case "bot_delete":
		res, err = r.execBotDelete(ctx, rawArgs)
	case "bot_run_command":
		res, err = r.execBotRunCommand(ctx, rawArgs)
	default:
		return marshalToolError(fmt.Sprintf("unknown tool %q", name)), nil
	}
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return marshalToolError(err.Error()), nil
	}
	b, merr := json.Marshal(res)
	if merr != nil {
		return "", merr
	}
	return string(b), nil
}

type searchFilesArgs struct {
	Query       string `json:"query"`
	WorkspaceID int64  `json:"workspace_id"`
	Folder      string `json:"folder"`
	Ext         string `json:"ext"`
	ExactPhrase string `json:"exact_phrase"`
	MatchAll    bool   `json:"match_all"`
	Limit       int    `json:"limit"`
}

type searchHitOut struct {
	FileID      int64  `json:"file_id"`
	WorkspaceID int64  `json:"workspace_id"`
	RelPath     string `json:"rel_path"`
	Size        int64  `json:"size"`
	Mtime       string `json:"mtime"`
	IndexDepth  string `json:"index_depth"`
	IndexStatus string `json:"index_status"`
	Title       string `json:"title,omitempty"`
	Headings    string `json:"headings,omitempty"`
	Snippet     string `json:"snippet,omitempty"`
	MatchSource string `json:"match_source,omitempty"`
}

type searchFilesOut struct {
	Hits      []searchHitOut `json:"hits"`
	Truncated bool           `json:"truncated"`
}

func (r *Registry) execSearchFiles(ctx context.Context, rawArgs string) (searchFilesOut, error) {
	if len(r.workspaces) == 0 {
		return searchFilesOut{}, errors.New("no user workspaces are mounted in this session")
	}
	var args searchFilesArgs
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		return searchFilesOut{}, fmt.Errorf("invalid search_files arguments: %w", err)
	}

	limit := args.Limit
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	if limit > maxSearchLimit {
		limit = maxSearchLimit
	}

	wsID := args.WorkspaceID
	var wsIDs []int64
	if wsID > 0 {
		if _, ok := r.workspaces[wsID]; !ok {
			return searchFilesOut{}, fmt.Errorf("workspace_id %d is not mounted in this session", wsID)
		}
	} else if len(r.order) == 1 {
		wsID = r.order[0]
	} else {
		wsIDs = append([]int64(nil), r.order...)
	}

	res, err := r.store.SearchWithOptions(ctx, store.SearchOptions{
		WorkspaceID:   wsID,
		WorkspaceIDs:  wsIDs,
		WorkspaceKind: store.WorkspaceKindUser,
		Query:         args.Query,
		ExactPhrase:   args.ExactPhrase,
		MatchAll:      args.MatchAll,
		Ext:           args.Ext,
		Folder:        args.Folder,
		Limit:         limit,
	})
	if err != nil {
		return searchFilesOut{}, err
	}

	out := searchFilesOut{
		Hits:      make([]searchHitOut, 0, len(res.Hits)),
		Truncated: res.Truncated,
	}
	for _, h := range res.Hits {
		if _, ok := r.workspaces[h.WorkspaceID]; !ok {
			continue
		}
		if len(out.Hits) >= limit {
			out.Truncated = true
			break
		}
		out.Hits = append(out.Hits, searchHitOut{
			FileID:      h.FileID,
			WorkspaceID: h.WorkspaceID,
			RelPath:     h.RelPath,
			Size:        h.Size,
			Mtime:       formatMtime(h.MtimeNS),
			IndexDepth:  h.IndexDepth,
			IndexStatus: h.IndexStatus,
			Title:       h.Title,
			Headings:    truncateUTF8(h.Headings, 200),
			Snippet:     truncateUTF8(h.Snippet, maxSnippetBytes),
			MatchSource: h.MatchSource,
		})
	}
	return out, nil
}

type listFolderArgs struct {
	WorkspaceID int64  `json:"workspace_id"`
	Folder      string `json:"folder"`
	Limit       int    `json:"limit"`
}

type listFileOut struct {
	FileID      int64  `json:"file_id"`
	RelPath     string `json:"rel_path"`
	Size        int64  `json:"size"`
	Mtime       string `json:"mtime"`
	IndexDepth  string `json:"index_depth"`
	IndexStatus string `json:"index_status"`
}

type listFolderOut struct {
	WorkspaceID int64                 `json:"workspace_id"`
	Folder      string                `json:"folder"`
	Folders     []store.FolderSummary `json:"folders"`
	Files       []listFileOut         `json:"files"`
	Truncated   bool                  `json:"truncated"`
}

func (r *Registry) execListFolder(ctx context.Context, rawArgs string) (listFolderOut, error) {
	var args listFolderArgs
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		return listFolderOut{}, fmt.Errorf("invalid list_folder arguments: %w", err)
	}

	ws, err := r.resolveWorkspace(args.WorkspaceID)
	if err != nil {
		return listFolderOut{}, err
	}

	limit := args.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}

	listing, err := r.store.ListFolder(ctx, ws.ID, args.Folder, limit)
	if err != nil {
		return listFolderOut{}, err
	}

	files := make([]listFileOut, 0, len(listing.Files))
	for _, f := range listing.Files {
		files = append(files, listFileOut{
			FileID:      f.ID,
			RelPath:     f.RelPath,
			Size:        f.Size,
			Mtime:       formatMtime(f.MtimeNS),
			IndexDepth:  f.IndexDepth,
			IndexStatus: f.IndexStatus,
		})
	}

	folders := listing.Folders
	if folders == nil {
		folders = []store.FolderSummary{}
	}
	return listFolderOut{
		WorkspaceID: listing.WorkspaceID,
		Folder:      listing.Folder,
		Folders:     folders,
		Files:       files,
		Truncated:   listing.Truncated,
	}, nil
}

type readFileArgs struct {
	FileID      int64  `json:"file_id"`
	WorkspaceID int64  `json:"workspace_id"`
	RelPath     string `json:"rel_path"`
	Offset      int64  `json:"offset"`
	Limit       int    `json:"limit"`
}

type readFileOut struct {
	FileID      int64           `json:"file_id,omitempty"`
	WorkspaceID int64           `json:"workspace_id"`
	RelPath     string          `json:"rel_path"`
	Size        int64           `json:"size"`
	Mtime       string          `json:"mtime"`
	IndexDepth  string          `json:"index_depth,omitempty"`
	IndexStatus string          `json:"index_status,omitempty"`
	Mode        string          `json:"mode"` // "text" | "card"
	Content     string          `json:"content,omitempty"`
	Offset      int64           `json:"offset,omitempty"`
	BytesRead   int             `json:"bytes_read,omitempty"`
	HasMore     bool            `json:"has_more,omitempty"`
	NextOffset  int64           `json:"next_offset,omitempty"`
	Title       string          `json:"title,omitempty"`
	Headings    string          `json:"headings,omitempty"`
	Outline     json.RawMessage `json:"outline,omitempty"`
	BodyPreview string          `json:"body_preview,omitempty"`
	Note        string          `json:"note,omitempty"`
}

func (r *Registry) execReadFile(ctx context.Context, rawArgs string) (readFileOut, error) {
	var args readFileArgs
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		return readFileOut{}, fmt.Errorf("invalid read_file arguments: %w", err)
	}

	var entry *store.FileEntry
	var ws store.Workspace
	var relPath string
	var err error

	if args.FileID > 0 {
		entry, err = r.store.GetFileByID(ctx, args.FileID)
		if err != nil {
			return readFileOut{}, err
		}
		if entry == nil {
			return readFileOut{}, fmt.Errorf("file_id %d was not found in the catalog; re-run search_files", args.FileID)
		}
		var ok bool
		ws, ok = r.workspaces[entry.File.WorkspaceID]
		if !ok {
			return readFileOut{}, fmt.Errorf("file_id %d belongs to an unmounted workspace", args.FileID)
		}
		relPath = entry.File.RelPath
	} else if strings.TrimSpace(args.RelPath) != "" {
		ws, err = r.resolveWorkspace(args.WorkspaceID)
		if err != nil {
			return readFileOut{}, err
		}
		relPath = strings.TrimPrefix(strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(args.RelPath)), "./"), "/")
		entry, err = r.store.GetFileByPath(ctx, ws.ID, relPath)
		if err != nil {
			return readFileOut{}, err
		}
	} else {
		return readFileOut{}, errors.New("either file_id or rel_path is required")
	}

	absPath, err := confinePath(ws.RootPath, relPath)
	if err != nil {
		return readFileOut{}, err
	}

	info, err := os.Lstat(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return readFileOut{}, r.notFoundHint(ctx, ws.ID, relPath, entry)
		}
		return readFileOut{}, fmt.Errorf("stat %q: %w", relPath, err)
	}
	if info.IsDir() {
		return readFileOut{}, fmt.Errorf("%q is a folder, not a file; use list_folder instead", relPath)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return readFileOut{}, fmt.Errorf("%q is a symlink and cannot be read", relPath)
	}
	if fsutil.IsCloudPlaceholder(absPath, info) {
		return readFileOut{}, fmt.Errorf("%q is a cloud-only placeholder not downloaded locally", relPath)
	}
	// Verify no parent directory along absPath is a symlink escaping ws.RootPath.
	if err := verifyRealPathWithinRoot(ws.RootPath, absPath); err != nil {
		return readFileOut{}, err
	}

	out := readFileOut{
		WorkspaceID: ws.ID,
		RelPath:     relPath,
		Size:        info.Size(),
		Mtime:       formatMtime(info.ModTime().UnixNano()),
	}
	if entry != nil {
		out.FileID = entry.File.ID
		out.IndexDepth = entry.File.IndexDepth
		out.IndexStatus = entry.File.IndexStatus
	}

	ext := strings.ToLower(filepath.Ext(relPath))
	if isDirectTextReadable(ext) {
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
			return readFileOut{}, err
		}
		if !isBinary {
			out.Mode = "text"
			out.Content = data
			out.Offset = offset
			out.BytesRead = len(data)
			out.HasMore = hasMore
			if hasMore {
				out.NextOffset = nextOffset
			}
			return out, nil
		}
	}

	// Non-text or structured file: return indexed card + outline_json if available.
	out.Mode = "card"
	if entry != nil && entry.Blob != nil {
		out.Title = entry.Blob.Title
		out.Headings = entry.Blob.Headings
		out.BodyPreview = truncateUTF8(entry.Blob.BodyPreview, defaultReadBytes)
		if entry.Blob.OutlineJSON != "" && json.Valid([]byte(entry.Blob.OutlineJSON)) {
			out.Outline = json.RawMessage(entry.Blob.OutlineJSON)
		}
		out.Note = fmt.Sprintf("Structured/binary file (%s): returning indexed card (%s). Deep full-file extraction for this format is not enabled yet.", ext, entry.File.IndexDepth)
	} else {
		out.Note = fmt.Sprintf("Binary or metadata-only file (%s): no extracted text is available in this version.", ext)
	}
	return out, nil
}

func (r *Registry) notFoundHint(ctx context.Context, workspaceID int64, relPath string, entry *store.FileEntry) error {
	if entry != nil && entry.File.BLAKE3 != "" {
		matches, err := r.store.FindFilesByBLAKE3(ctx, workspaceID, entry.File.BLAKE3)
		if err == nil {
			var moved []string
			for _, m := range matches {
				if m.RelPath != relPath {
					moved = append(moved, fmt.Sprintf("%s (file_id=%d)", m.RelPath, m.ID))
				}
			}
			if len(moved) > 0 {
				return fmt.Errorf("file %q no longer exists at that path; identical content found at: %s", relPath, strings.Join(moved, ", "))
			}
		}
	}
	return fmt.Errorf("file %q no longer exists on disk (it may have been moved or deleted); re-run search_files", relPath)
}

func (r *Registry) resolveWorkspace(wsID int64) (store.Workspace, error) {
	if len(r.workspaces) == 0 {
		return store.Workspace{}, errors.New("no user workspaces are mounted in this session")
	}
	if wsID > 0 {
		ws, ok := r.workspaces[wsID]
		if !ok {
			return store.Workspace{}, fmt.Errorf("workspace_id %d is not mounted in this session", wsID)
		}
		return ws, nil
	}
	if len(r.order) == 1 {
		return r.workspaces[r.order[0]], nil
	}
	return store.Workspace{}, errors.New("workspace_id is required when multiple workspaces are mounted")
}

func isDirectTextReadable(ext string) bool {
	switch peek.PeekerIDForExt(ext) {
	case "text", "md", "csv", "tsv":
		return true
	default:
		return false
	}
}

func confinePath(rootPath, relPath string) (string, error) {
	cleanRel := filepath.Clean(filepath.FromSlash(relPath))
	if cleanRel == "." || cleanRel == "" || filepath.IsAbs(cleanRel) || cleanRel == ".." || strings.HasPrefix(cleanRel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("invalid relative path %q", relPath)
	}
	absRoot, err := filepath.Abs(rootPath)
	if err != nil {
		return "", err
	}
	joined := filepath.Join(absRoot, cleanRel)
	rel, err := filepath.Rel(absRoot, joined)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes workspace root", relPath)
	}
	return joined, nil
}

func verifyRealPathWithinRoot(rootPath, targetPath string) error {
	realRoot, err := filepath.EvalSymlinks(rootPath)
	if err != nil {
		return fmt.Errorf("resolve workspace root: %w", err)
	}
	realTarget, err := filepath.EvalSymlinks(targetPath)
	if err != nil {
		return fmt.Errorf("resolve file path: %w", err)
	}
	rel, err := filepath.Rel(realRoot, realTarget)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("file path resolves outside workspace root")
	}
	return nil
}

func readUTF8Slice(path string, offset int64, limit int, totalSize int64) (content string, hasMore bool, nextOffset int64, isBinary bool, err error) {
	if offset >= totalSize {
		return "", false, totalSize, false, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", false, 0, false, err
	}
	defer f.Close()

	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return "", false, 0, false, err
		}
	}

	// Read 1 extra byte to know accurately if more data remains.
	buf := make([]byte, limit+1)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", false, 0, false, err
	}
	buf = buf[:n]

	if bytes.IndexByte(buf, 0) >= 0 {
		return "", false, 0, true, nil
	}

	more := false
	if len(buf) > limit {
		more = true
		buf = buf[:limit]
	} else if offset+int64(len(buf)) < totalSize {
		more = true
	}

	// Trim any trailing incomplete multi-byte UTF-8 rune so pagination aligns on rune boundaries.
	if more && len(buf) > 0 {
		cut := len(buf)
		for cut > 0 && cut > len(buf)-utf8.UTFMax && !utf8.Valid(buf[:cut]) {
			cut--
		}
		if cut > 0 {
			buf = buf[:cut]
		}
	}

	next := offset + int64(len(buf))
	return string(buf), more, next, false, nil
}

func formatMtime(ns int64) string {
	if ns <= 0 {
		return ""
	}
	return time.Unix(0, ns).UTC().Format("2006-01-02 15:04 UTC")
}

func truncateUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return strings.TrimSpace(s[:cut]) + "…"
}

func marshalToolError(msg string) string {
	b, _ := json.Marshal(struct {
		Error string `json:"error"`
	}{Error: msg})
	return string(b)
}
