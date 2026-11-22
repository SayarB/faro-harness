package tools

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/SayarB/faro-harness/internal/bot"
	"github.com/SayarB/faro-harness/internal/store"
	"github.com/SayarB/faro-harness/internal/worker"
)

func setupIndexedWorkspace(t *testing.T) (*store.Store, store.Workspace, string) {
	t.Helper()
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "Clients", "Acme", "Invoice_2026.md"), "# Acme Invoice 2026\n\nTotal due: $4,500 on 2026-05-01.\n")
	mustWriteFile(t, filepath.Join(root, "Clients", "Acme", "notes.txt"), strings.Repeat("Acme contract line — café.\n", 600))
	mustWriteFile(t, filepath.Join(root, "README.md"), "# Work Folder\n\nTop-level notes.\n")
	createMinimalZip(t, filepath.Join(root, "Archive", "bundle.zip"), map[string]string{
		"receipts/jan.txt": "jan",
		"receipts/feb.txt": "feb",
	})

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ctx := context.Background()
	cat, err := worker.NewCatalogIndexer(ctx, st, root)
	if err != nil {
		t.Fatalf("NewCatalogIndexer: %v", err)
	}
	pipe := worker.NewPipeline(cat, worker.PipelineConfig{})
	defer pipe.Close()
	if _, err := pipe.IndexWorkspace(ctx); err != nil {
		t.Fatalf("IndexWorkspace: %v", err)
	}
	return st, cat.Workspace, root
}

func TestToolsSearchListAndRead(t *testing.T) {
	st, ws, _ := setupIndexedWorkspace(t)
	reg, err := NewRegistry(st, []store.Workspace{ws})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	ctx := context.Background()

	// 1. search_files finds Acme invoice.
	raw, err := reg.Execute(ctx, "search_files", `{"query":"Acme invoice 2026"}`)
	if err != nil {
		t.Fatalf("search_files: %v", err)
	}
	var searchRes searchFilesOut
	if err := json.Unmarshal([]byte(raw), &searchRes); err != nil {
		t.Fatalf("unmarshal search_files: %v (%s)", err, raw)
	}
	if len(searchRes.Hits) == 0 || searchRes.Hits[0].RelPath != "Clients/Acme/Invoice_2026.md" {
		t.Fatalf("expected Invoice_2026.md top hit, got %+v", searchRes.Hits)
	}
	invoiceID := searchRes.Hits[0].FileID

	// 2. list_folder at root returns immediate subfolders + root file.
	raw, err = reg.Execute(ctx, "list_folder", `{}`)
	if err != nil {
		t.Fatalf("list_folder root: %v", err)
	}
	var listRes listFolderOut
	if err := json.Unmarshal([]byte(raw), &listRes); err != nil {
		t.Fatalf("unmarshal list_folder: %v", err)
	}
	if len(listRes.Folders) != 2 || listRes.Folders[0].Path != "Archive" || listRes.Folders[1].Path != "Clients" || listRes.Folders[1].FileCount != 2 {
		t.Fatalf("unexpected root folders: %+v", listRes.Folders)
	}
	if len(listRes.Files) != 1 || listRes.Files[0].RelPath != "README.md" {
		t.Fatalf("unexpected root files: %+v", listRes.Files)
	}

	// 3. read_file by file_id reads markdown text.
	raw, err = reg.Execute(ctx, "read_file", `{"file_id":`+itoa(invoiceID)+`}`)
	if err != nil {
		t.Fatalf("read_file invoice: %v", err)
	}
	var readRes readFileOut
	if err := json.Unmarshal([]byte(raw), &readRes); err != nil {
		t.Fatalf("unmarshal read_file: %v", err)
	}
	if readRes.Mode != "text" || !strings.Contains(readRes.Content, "$4,500") || readRes.HasMore {
		t.Fatalf("unexpected read_file invoice result: %+v", readRes)
	}

	// 4. read_file pagination over larger UTF-8 file aligns cleanly.
	raw, err = reg.Execute(ctx, "read_file", `{"rel_path":"Clients/Acme/notes.txt","offset":0,"limit":1024}`)
	if err != nil {
		t.Fatalf("read_file page 1: %v", err)
	}
	var page1 readFileOut
	if err := json.Unmarshal([]byte(raw), &page1); err != nil {
		t.Fatalf("unmarshal page1: %v", err)
	}
	if !page1.HasMore || page1.NextOffset <= 0 || page1.NextOffset > 1024 {
		t.Fatalf("unexpected page1 pagination: %+v", page1)
	}
	raw, err = reg.Execute(ctx, "read_file", `{"rel_path":"Clients/Acme/notes.txt","offset":`+itoa(page1.NextOffset)+`,"limit":1024}`)
	if err != nil {
		t.Fatalf("read_file page 2: %v", err)
	}
	var page2 readFileOut
	if err := json.Unmarshal([]byte(raw), &page2); err != nil {
		t.Fatalf("unmarshal page2: %v", err)
	}
	if page2.Offset != page1.NextOffset || page2.BytesRead == 0 {
		t.Fatalf("unexpected page2: %+v", page2)
	}

	// 5. read_file on .zip returns indexed card + outline instead of binary garbage.
	raw, err = reg.Execute(ctx, "read_file", `{"rel_path":"Archive/bundle.zip"}`)
	if err != nil {
		t.Fatalf("read_file zip: %v", err)
	}
	var zipRes readFileOut
	if err := json.Unmarshal([]byte(raw), &zipRes); err != nil {
		t.Fatalf("unmarshal zip: %v", err)
	}
	if zipRes.Mode != "card" || !strings.Contains(zipRes.BodyPreview, "receipts/jan.txt") || len(zipRes.Outline) == 0 {
		t.Fatalf("expected zip card + outline, got %+v", zipRes)
	}
}

func TestReadFileConfinementSymlinkAndMovedHint(t *testing.T) {
	st, ws, root := setupIndexedWorkspace(t)
	reg, err := NewRegistry(st, []store.Workspace{ws})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	ctx := context.Background()

	// Path traversal rejected.
	raw, err := reg.Execute(ctx, "read_file", `{"rel_path":"../etc/passwd"}`)
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if !strings.Contains(raw, `"error"`) {
		t.Fatalf("expected error JSON for traversal, got %s", raw)
	}

	// Symlink escaping workspace rejected.
	outsideDir := t.TempDir()
	secretFile := filepath.Join(outsideDir, "secret.txt")
	mustWriteFile(t, secretFile, "top secret")
	linkPath := filepath.Join(root, "escape.txt")
	if err := os.Symlink(secretFile, linkPath); err == nil {
		raw, err = reg.Execute(ctx, "read_file", `{"rel_path":"escape.txt"}`)
		if err != nil {
			t.Fatalf("unexpected Go error: %v", err)
		}
		if !strings.Contains(raw, `"error"`) || strings.Contains(raw, "top secret") {
			t.Fatalf("expected symlink read to be blocked, got %s", raw)
		}
	}

	// Moved file hint via BLAKE3 when old file_id path is removed on disk after a copy is indexed.
	oldEntry, err := st.GetFileByPath(ctx, ws.ID, "Clients/Acme/Invoice_2026.md")
	if err != nil || oldEntry == nil {
		t.Fatalf("GetFileByPath: %v", err)
	}
	// Copy to new location and index new location without pruning old row yet (simulating debounce window).
	newAbs := filepath.Join(root, "Invoices", "Acme_2026_Moved.md")
	origBytes, err := os.ReadFile(filepath.Join(root, "Clients", "Acme", "Invoice_2026.md"))
	if err != nil {
		t.Fatalf("read orig: %v", err)
	}
	mustWriteFile(t, newAbs, string(origBytes))
	cat, err := worker.NewCatalogIndexer(ctx, st, root)
	if err != nil {
		t.Fatalf("NewCatalogIndexer: %v", err)
	}
	if err := cat.IndexFile(ctx, newAbs); err != nil {
		t.Fatalf("IndexFile moved copy: %v", err)
	}
	if err := os.Remove(filepath.Join(root, "Clients", "Acme", "Invoice_2026.md")); err != nil {
		t.Fatalf("remove old path: %v", err)
	}

	raw, err = reg.Execute(ctx, "read_file", `{"file_id":`+itoa(oldEntry.File.ID)+`}`)
	if err != nil {
		t.Fatalf("read_file moved: %v", err)
	}
	if !strings.Contains(raw, "Invoices/Acme_2026_Moved.md") {
		t.Fatalf("expected moved-file BLAKE3 hint, got %s", raw)
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func createMinimalZip(t *testing.T, path string, entries map[string]string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create zip: %v", err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	for name, body := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %s: %v", name, err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
}

func itoa(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestBotToolsIsolationAndScratchpad(t *testing.T) {
	st, ws, _ := setupIndexedWorkspace(t)
	ctx := context.Background()

	mgr, err := bot.NewManager(st, t.TempDir())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	ledger, err := mgr.Create(ctx, bot.CreateOptions{Slug: "ledger", Name: "Ledger"})
	if err != nil {
		t.Fatalf("create ledger: %v", err)
	}
	counsel, err := mgr.Create(ctx, bot.CreateOptions{Slug: "counsel", Name: "Counsel"})
	if err != nil {
		t.Fatalf("create counsel: %v", err)
	}

	regLedger, err := NewRegistryForBot(st, []store.Workspace{ws}, &ledger)
	if err != nil {
		t.Fatalf("NewRegistryForBot ledger: %v", err)
	}
	regCounsel, err := NewRegistryForBot(st, []store.Workspace{ws}, &counsel)
	if err != nil {
		t.Fatalf("NewRegistryForBot counsel: %v", err)
	}

	// Both bots get 9 tools (3 user read + 1 deliver_file + 5 bot workshop).
	if len(regLedger.Definitions()) != 9 {
		t.Fatalf("expected 9 tool defs for bot registry, got %d", len(regLedger.Definitions()))
	}

	// Ledger writes its own memory and a scratch file.
	raw, err := regLedger.Execute(ctx, "bot_write", `{"path":"memory.md","content":"- Acme pays net-45 (Invoice_2026.md)","mode":"append"}`)
	if err != nil || strings.Contains(raw, `"error"`) {
		t.Fatalf("ledger bot_write memory failed: err=%v raw=%s", err, raw)
	}
	raw, err = regLedger.Execute(ctx, "bot_write", `{"path":"scratch/acme_summary.md","content":"Draft summary for Acme"}`)
	if err != nil || strings.Contains(raw, `"error"`) {
		t.Fatalf("ledger bot_write scratch failed: err=%v raw=%s", err, raw)
	}

	// Ledger can read and list its own files.
	raw, err = regLedger.Execute(ctx, "bot_read", `{"path":"memory.md"}`)
	if err != nil || !strings.Contains(raw, "Acme pays net-45") {
		t.Fatalf("ledger bot_read memory failed: err=%v raw=%s", err, raw)
	}
	raw, err = regLedger.Execute(ctx, "bot_list", `{"path":"scratch"}`)
	if err != nil || !strings.Contains(raw, "scratch/acme_summary.md") {
		t.Fatalf("ledger bot_list scratch failed: err=%v raw=%s", err, raw)
	}

	// Counsel sees NONE of Ledger's memory or scratch files.
	raw, err = regCounsel.Execute(ctx, "bot_read", `{"path":"memory.md"}`)
	if err != nil || strings.Contains(raw, "Acme pays net-45") {
		t.Fatalf("counsel saw ledger memory: %s", raw)
	}
	raw, err = regCounsel.Execute(ctx, "bot_list", `{"path":"scratch"}`)
	if err != nil || strings.Contains(raw, "acme_summary.md") {
		t.Fatalf("counsel saw ledger scratch: %s", raw)
	}

	// Cross-bot path traversal is blocked on read, write, and delete.
	for _, tc := range []struct {
		tool string
		args string
	}{
		{"bot_read", `{"path":"../ledger/memory.md"}`},
		{"bot_write", `{"path":"../ledger/memory.md","content":"pwned"}`},
		{"bot_delete", `{"path":"../ledger/scratch/acme_summary.md"}`},
		{"bot_delete", `{"path":"memory.md"}`},
	} {
		raw, err := regCounsel.Execute(ctx, tc.tool, tc.args)
		if err != nil {
			t.Fatalf("%s returned Go error: %v", tc.tool, err)
		}
		if !strings.Contains(raw, `"error"`) {
			t.Fatalf("expected error JSON for %s(%s), got %s", tc.tool, tc.args, raw)
		}
	}

	// Cross-bot symlink escape inside Counsel's scratch is also blocked.
	linkPath := filepath.Join(counsel.HomePath, bot.ScratchDirName, "leak.md")
	if err := os.Symlink(filepath.Join(ledger.HomePath, bot.MemoryFileName), linkPath); err == nil {
		raw, err := regCounsel.Execute(ctx, "bot_read", `{"path":"scratch/leak.md"}`)
		if err != nil || !strings.Contains(raw, `"error"`) || strings.Contains(raw, "Acme pays net-45") {
			t.Fatalf("expected symlink escape to be blocked, got err=%v raw=%s", err, raw)
		}
	}

	// Ledger can delete its own scratch file.
	raw, err = regLedger.Execute(ctx, "bot_delete", `{"path":"scratch/acme_summary.md"}`)
	if err != nil || !strings.Contains(raw, `"deleted":true`) {
		t.Fatalf("expected scratch delete to succeed, got err=%v raw=%s", err, raw)
	}

	// A bot can also run with zero user workspaces (bot tools only).
	regNoWS, err := NewRegistryForBot(st, nil, &ledger)
	if err != nil {
		t.Fatalf("NewRegistryForBot with zero workspaces: %v", err)
	}
	if len(regNoWS.Definitions()) != 5 {
		t.Fatalf("expected 5 bot-only tool defs with zero workspaces, got %d", len(regNoWS.Definitions()))
	}
	raw, err = regNoWS.Execute(ctx, "search_files", `{"query":"Acme"}`)
	if err != nil || !strings.Contains(raw, `"error"`) {
		t.Fatalf("expected error JSON when searching with zero workspaces, got err=%v raw=%s", err, raw)
	}

	// bot_write rejects modifying bot.md (user-editable only), including via scratch/.. traversal.
	for _, tc := range []struct {
		tool string
		args string
	}{
		{"bot_write", `{"path":"bot.md","content":"hijacked"}`},
		{"bot_write", `{"path":"scratch/../bot.md","content":"hijacked"}`},
		{"bot_write", `{"path":"scratch/../.faro/commands.log","content":"wiped"}`},
		{"bot_read", `{"path":"scratch/../.faro/commands.log"}`},
		{"bot_list", `{"path":"scratch/../.faro"}`},
		{"bot_delete", `{"path":"scratch/../memory.md"}`},
	} {
		raw, err = regLedger.Execute(ctx, tc.tool, tc.args)
		if err != nil || !strings.Contains(raw, `"error"`) {
			t.Fatalf("expected %s(%s) to be rejected, got err=%v raw=%s", tc.tool, tc.args, err, raw)
		}
	}
	botMDBytes, err := os.ReadFile(filepath.Join(ledger.HomePath, bot.BotFileName))
	if err != nil || strings.Contains(string(botMDBytes), "hijacked") {
		t.Fatalf("bot.md was unexpectedly modified: err=%v content=%q", err, string(botMDBytes))
	}

	// bot_run_command without an approver denies execution and logs the denial.
	raw, err = regLedger.Execute(ctx, "bot_run_command", `{"command":"echo denied"}`)
	if err != nil || !strings.Contains(raw, "requires approval") {
		t.Fatalf("expected approval denial without approver, got err=%v raw=%s", err, raw)
	}

	// Approval seam: Allow once, Deny, and AllowSession + allowlist env + files_changed + audit log.
	var calls int
	decisions := []ApprovalDecision{ApprovalDeny, ApprovalAllow, ApprovalAllowSession}
	regApproved, err := NewRegistryWithOptions(st, []store.Workspace{ws}, &ledger, RegistryOptions{
		Approver: func(_ context.Context, _ ApprovalRequest) (ApprovalDecision, error) {
			d := decisions[calls]
			calls++
			return d, nil
		},
	})
	if err != nil {
		t.Fatalf("NewRegistryWithOptions: %v", err)
	}
	regApproved.SetConversationID("conv_test_audit")

	// 1st call: denied.
	raw, err = regApproved.Execute(ctx, "bot_run_command", `{"command":"echo nope"}`)
	if err != nil || !strings.Contains(raw, "denied by user") {
		t.Fatalf("expected user denial, got err=%v raw=%s", err, raw)
	}

	// 2nd call: allowed once; verify allowlist strips arbitrary FOO_SECRET and AI_GATEWAY_API_KEY,
	// sets FARO_WORKSPACES_READONLY, returns cwd="scratch/", and reports files_changed.
	t.Setenv("AI_GATEWAY_API_KEY", "super-secret-do-not-leak")
	t.Setenv("FOO_SECRET", "random-secret-do-not-leak")
	raw, err = regApproved.Execute(ctx, "bot_run_command", `{"command":"printf 'hello-from-cmd' > created.txt && printf 'A=%s|F=%s|RO=%s' \"$AI_GATEWAY_API_KEY\" \"$FOO_SECRET\" \"$FARO_WORKSPACES_READONLY\""}`)
	if err != nil {
		t.Fatalf("bot_run_command allow: %v", err)
	}
	var cmdOut botRunCommandOut
	if err := json.Unmarshal([]byte(raw), &cmdOut); err != nil {
		t.Fatalf("unmarshal bot_run_command: %v (%s)", err, raw)
	}
	if cmdOut.ExitCode != 0 || cmdOut.Cwd != "scratch/" {
		t.Fatalf("unexpected bot_run_command result: %+v", cmdOut)
	}
	if strings.Contains(cmdOut.Stdout, "super-secret") || strings.Contains(cmdOut.Stdout, "random-secret") {
		t.Fatalf("secret env leaked into child command: %q", cmdOut.Stdout)
	}
	if !strings.Contains(cmdOut.Stdout, "A=|F=|RO={") {
		t.Fatalf("expected allowlisted env with FARO_WORKSPACES_READONLY, got %q", cmdOut.Stdout)
	}
	if len(cmdOut.FilesChanged) != 1 || cmdOut.FilesChanged[0] != "scratch/created.txt" {
		t.Fatalf("expected files_changed=[scratch/created.txt], got %+v", cmdOut.FilesChanged)
	}
	// Counsel's scratch must not contain Ledger's created.txt.
	if _, err := os.Stat(filepath.Join(counsel.HomePath, bot.ScratchDirName, "created.txt")); !os.IsNotExist(err) {
		t.Fatalf("counsel scratch should not have ledger's created.txt")
	}

	// 3rd call: AllowSession; 4th call must NOT invoke approver again.
	for i := 0; i < 2; i++ {
		raw, err = regApproved.Execute(ctx, "bot_run_command", `{"command":"echo ok"}`)
		if err != nil || !strings.Contains(raw, `"exit_code":0`) {
			t.Fatalf("session-approved call %d failed: err=%v raw=%s", i, err, raw)
		}
	}
	if calls != 3 {
		t.Fatalf("expected approver to be called 3 times (4th covered by AllowSession), got %d", calls)
	}

	// Audit log exists and contains conversation_id and entries.
	auditBytes, err := os.ReadFile(filepath.Join(ledger.HomePath, ".faro", "commands.log"))
	if err != nil {
		t.Fatalf("read commands.log: %v", err)
	}
	if !strings.Contains(string(auditBytes), `"conv_test_audit"`) || !strings.Contains(string(auditBytes), `"scratch/created.txt"`) {
		t.Fatalf("unexpected audit log contents:\n%s", string(auditBytes))
	}
}

func TestBotRunCommandProcessTreeKillOutputShapingAndCancel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix pipeline repro test")
	}
	st, ws, _ := setupIndexedWorkspace(t)
	ctx := context.Background()

	mgr, err := bot.NewManager(st, t.TempDir())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	ledger, err := mgr.Create(ctx, bot.CreateOptions{Slug: "ledger", Name: "Ledger"})
	if err != nil {
		t.Fatalf("create ledger: %v", err)
	}
	reg, err := NewRegistryWithOptions(st, []store.Workspace{ws}, &ledger, RegistryOptions{
		Approver: func(_ context.Context, _ ApprovalRequest) (ApprovalDecision, error) {
			return ApprovalAllowSession, nil
		},
	})
	if err != nil {
		t.Fatalf("NewRegistryWithOptions: %v", err)
	}

	// Finding A repro: grandchild holding stdout pipe must be killed promptly on 1s timeout (< 5s).
	t0 := time.Now()
	raw, err := reg.Execute(ctx, "bot_run_command", `{"command":"sleep 30 | cat","timeout_seconds":1}`)
	elapsed := time.Since(t0)
	if err != nil {
		t.Fatalf("timeout command returned Go error: %v", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("process group kill took too long (%s); grandchild likely orphaned", elapsed)
	}
	var timeoutOut botRunCommandOut
	if err := json.Unmarshal([]byte(raw), &timeoutOut); err != nil {
		t.Fatalf("unmarshal timeout out: %v (%s)", err, raw)
	}
	if !timeoutOut.TimedOut || timeoutOut.ExitCode != -1 {
		t.Fatalf("expected timed_out=true exit_code=-1, got %+v", timeoutOut)
	}

	// Finding C: head (4KB) + tail (12KB) output keeps tail error + ANSI/CR stripping + binary detection.
	raw, err = reg.Execute(ctx, "bot_run_command", `{"command":"python3 -c \"print('HEAD_MARKER'); print('x'*25000); print('\\x1b[31mTAIL_ERROR_MARKER\\x1b[0m')\""}`)
	if err != nil {
		t.Fatalf("head-tail command: %v", err)
	}
	var htOut botRunCommandOut
	if err := json.Unmarshal([]byte(raw), &htOut); err != nil {
		t.Fatalf("unmarshal head-tail: %v", err)
	}
	if !htOut.StdoutTruncated || htOut.StdoutBytesTotal < 25000 {
		t.Fatalf("expected truncated head+tail output, got %+v", htOut)
	}
	if !strings.Contains(htOut.Stdout, "HEAD_MARKER") || !strings.Contains(htOut.Stdout, "TAIL_ERROR_MARKER") || !strings.Contains(htOut.Stdout, "bytes omitted") {
		t.Fatalf("expected HEAD_MARKER, omitted marker, and TAIL_ERROR_MARKER, got %q", htOut.Stdout)
	}
	if strings.Contains(htOut.Stdout, "\x1b[") {
		t.Fatalf("expected ANSI escape codes to be stripped, got %q", htOut.Stdout)
	}

	// Binary stdout is replaced cleanly.
	raw, err = reg.Execute(ctx, "bot_run_command", `{"command":"printf '\\x00\\x01\\x02\\xff\\xfe\\xfd'"}`)
	if err != nil {
		t.Fatalf("binary command: %v", err)
	}
	var binOut botRunCommandOut
	if err := json.Unmarshal([]byte(raw), &binOut); err != nil {
		t.Fatalf("unmarshal binary out: %v", err)
	}
	if !strings.Contains(binOut.Stdout, "[binary output") || !binOut.StdoutTruncated {
		t.Fatalf("expected binary output summary, got %+v", binOut)
	}

	// Finding D: parent context cancellation (Ctrl-C) propagates as context.Canceled error.
	cancelCtx, cancel := context.WithCancel(ctx)
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	_, err = reg.Execute(cancelCtx, "bot_run_command", `{"command":"sleep 10","timeout_seconds":30}`)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled on parent cancel, got %v", err)
	}

	// Bug 2 regression: background child holding stdout is terminated after normal exit,
	// stdout is preserved, child PID is dead, and the run is audited.
	t1 := time.Now()
	raw, err = reg.Execute(ctx, "bot_run_command", `{"command":"sleep 77 & echo $! > bg.pid; echo started","timeout_seconds":20}`)
	bgElapsed := time.Since(t1)
	if err != nil {
		t.Fatalf("background child command returned Go error: %v", err)
	}
	if bgElapsed > 6*time.Second {
		t.Fatalf("background child command took too long (%s)", bgElapsed)
	}
	var bgOut botRunCommandOut
	if err := json.Unmarshal([]byte(raw), &bgOut); err != nil {
		t.Fatalf("unmarshal bgOut: %v (%s)", err, raw)
	}
	if bgOut.ExitCode != 0 || !strings.Contains(bgOut.Stdout, "started") || bgOut.Note == "" {
		t.Fatalf("unexpected background child output: %+v", bgOut)
	}
	pidBytes, err := os.ReadFile(filepath.Join(ledger.HomePath, bot.ScratchDirName, "bg.pid"))
	if err != nil {
		t.Fatalf("read bg.pid: %v", err)
	}
	bgPID := strings.TrimSpace(string(pidBytes))
	// Give the OS a tiny moment to reap the SIGKILLed background sleep, then confirm it's gone.
	time.Sleep(100 * time.Millisecond)
	checkRaw, _ := reg.Execute(ctx, "bot_run_command", `{"command":"kill -0 `+bgPID+` 2>/dev/null && echo ALIVE || echo DEAD"}`)
	if !strings.Contains(checkRaw, "DEAD") {
		t.Fatalf("expected background sleep PID %s to be dead, got %s", bgPID, checkRaw)
	}
	auditLog, err := os.ReadFile(filepath.Join(ledger.HomePath, ".faro", "commands.log"))
	if err != nil || !strings.Contains(string(auditLog), "sleep 77") {
		t.Fatalf("expected background command to be recorded in commands.log, err=%v", err)
	}

	// Bug 3 regression: stray invalid Latin-1 byte earlier in stdout must NOT drop trailing ASCII.
	raw, err = reg.Execute(ctx, "bot_run_command", `{"command":"printf 'caf\\xe9 result=1234'"}`)
	if err != nil {
		t.Fatalf("stray byte command: %v", err)
	}
	var strayOut botRunCommandOut
	if err := json.Unmarshal([]byte(raw), &strayOut); err != nil {
		t.Fatalf("unmarshal strayOut: %v (%s)", err, raw)
	}
	if !strings.Contains(strayOut.Stdout, "result=1234") {
		t.Fatalf("expected trailing result=1234 to be preserved despite stray byte, got %q", strayOut.Stdout)
	}

	// Nit (a): >30 changed files sets files_changed_truncated and files_changed_total.
	raw, err = reg.Execute(ctx, "bot_run_command", `{"command":"mkdir -p many && for i in $(seq 1 35); do echo x > many/f_$i.txt; done"}`)
	if err != nil {
		t.Fatalf("many files command: %v", err)
	}
	var manyOut botRunCommandOut
	if err := json.Unmarshal([]byte(raw), &manyOut); err != nil {
		t.Fatalf("unmarshal manyOut: %v (%s)", err, raw)
	}
	if !manyOut.FilesChangedTruncated || manyOut.FilesChangedTotal != 35 || len(manyOut.FilesChanged) != maxFilesChangedReport {
		t.Fatalf("expected truncated=true total=35 len=%d, got %+v", maxFilesChangedReport, manyOut)
	}

	// Approver error is also recorded in commands.log.
	regErrApprover, err := NewRegistryWithOptions(st, []store.Workspace{ws}, &ledger, RegistryOptions{
		Approver: func(_ context.Context, _ ApprovalRequest) (ApprovalDecision, error) {
			return ApprovalDeny, errors.New("approval ui disconnected")
		},
	})
	if err != nil {
		t.Fatalf("NewRegistryWithOptions err approver: %v", err)
	}
	raw, err = regErrApprover.Execute(ctx, "bot_run_command", `{"command":"echo approver_err_probe"}`)
	if err != nil || !strings.Contains(raw, "approval ui disconnected") {
		t.Fatalf("expected approver error JSON, got err=%v raw=%s", err, raw)
	}
	auditLog, err = os.ReadFile(filepath.Join(ledger.HomePath, ".faro", "commands.log"))
	if err != nil || !strings.Contains(string(auditLog), `"approver_error"`) || !strings.Contains(string(auditLog), "approver_err_probe") {
		t.Fatalf("expected approver_error entry in commands.log, err=%v", err)
	}
}

func TestDeliverFileStagingApprovalBackupAndIndexing(t *testing.T) {
	st, ws, root := setupIndexedWorkspace(t)
	ctx := context.Background()

	mgr, err := bot.NewManager(st, t.TempDir())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	reporter, err := mgr.Create(ctx, bot.CreateOptions{Slug: "news-reporter", Name: "News Reporter"})
	if err != nil {
		t.Fatalf("create bot: %v", err)
	}

	dataDir := t.TempDir()
	var seenReqs []ApprovalRequest
	var nextDecision ApprovalDecision = ApprovalAllow

	reg, err := NewRegistryWithOptions(st, []store.Workspace{ws}, &reporter, RegistryOptions{
		DataDir: dataDir,
		Approver: func(_ context.Context, req ApprovalRequest) (ApprovalDecision, error) {
			seenReqs = append(seenReqs, req)
			return nextDecision, nil
		},
	})
	if err != nil {
		t.Fatalf("NewRegistryWithOptions: %v", err)
	}
	reg.SetConversationID("conv_deliver_test")

	// 1. Granting AllowSession to bot_run_command must NOT auto-approve deliver_file.
	// Stage the binary payload portably with os.WriteFile (and still exercise bot_run_command approval isolation).
	nextDecision = ApprovalAllowSession
	raw, err := reg.Execute(ctx, "bot_run_command", `{"command":"echo staged_check"}`)
	if err != nil || !strings.Contains(raw, `"exit_code":0`) {
		t.Fatalf("bot_run_command check failed: err=%v raw=%s", err, raw)
	}
	stagedXLSX := filepath.Join(reporter.HomePath, bot.ScratchDirName, bot.ScratchOutDirName, "employee_data.xlsx")
	if err := os.WriteFile(stagedXLSX, []byte("PK\x03\x04fake-xlsx-payload"), 0o644); err != nil {
		t.Fatalf("stage xlsx: %v", err)
	}
	if len(seenReqs) != 1 || seenReqs[0].Kind != ApprovalKindCommand {
		t.Fatalf("expected 1 command approval req, got %+v", seenReqs)
	}

	// 2. Denying deliver_file leaves user workspace untouched and records denial in changes.log.
	nextDecision = ApprovalDeny
	raw, err = reg.Execute(ctx, "deliver_file", `{"scratch_path":"scratch/out/employee_data.xlsx","dest_rel_path":"Reports/employee_data.xlsx"}`)
	if err != nil || !strings.Contains(raw, "denied by user") {
		t.Fatalf("expected deliver_file denial, got err=%v raw=%s", err, raw)
	}
	if len(seenReqs) != 2 || seenReqs[1].Kind != ApprovalKindFileCreate || seenReqs[1].WillOverwrite {
		t.Fatalf("expected file_create approval req (not bypassed by command session), got %+v", seenReqs)
	}
	if _, err := os.Stat(filepath.Join(root, "Reports", "employee_data.xlsx")); !os.IsNotExist(err) {
		t.Fatalf("denied delivery must not create file on disk")
	}

	// 3. Approving deliver_file with AllowSession delivers staged binary file & indexes immediately.
	nextDecision = ApprovalAllowSession
	raw, err = reg.Execute(ctx, "deliver_file", `{"scratch_path":"scratch/out/employee_data.xlsx","dest_rel_path":"Reports/employee_data.xlsx"}`)
	if err != nil || strings.Contains(raw, `"error"`) {
		t.Fatalf("deliver_file staged xlsx failed: err=%v raw=%s", err, raw)
	}
	var delOut deliverFileOut
	if err := json.Unmarshal([]byte(raw), &delOut); err != nil {
		t.Fatalf("unmarshal deliver_file: %v (%s)", err, raw)
	}
	if !delOut.Indexed || delOut.FileID == 0 || delOut.Overwrote || delOut.RelPath != "Reports/employee_data.xlsx" {
		t.Fatalf("unexpected deliver_file output: %+v", delOut)
	}
	gotBytes, err := os.ReadFile(filepath.Join(root, "Reports", "employee_data.xlsx"))
	if err != nil || string(gotBytes) != "PK\x03\x04fake-xlsx-payload" {
		t.Fatalf("unexpected delivered binary bytes: err=%v got=%q", err, string(gotBytes))
	}
	// Immediately visible via list_folder.
	listRaw, err := reg.Execute(ctx, "list_folder", `{"folder":"Reports"}`)
	if err != nil || !strings.Contains(listRaw, "Reports/employee_data.xlsx") {
		t.Fatalf("expected delivered file in list_folder immediately, got err=%v raw=%s", err, listRaw)
	}

	// 4. Subsequent NEW file creation via inline content is covered by allowWriteSession (no extra prompt)
	// and is immediately searchable via search_files.
	reqsBefore := len(seenReqs)
	raw, err = reg.Execute(ctx, "deliver_file", `{"dest_rel_path":"Q4_Summary.md","content":"# Q4 Summary\n\nUniqueDeliverKeyword9942\n"}`)
	if err != nil || strings.Contains(raw, `"error"`) {
		t.Fatalf("deliver_file inline content failed: err=%v raw=%s", err, raw)
	}
	if len(seenReqs) != reqsBefore {
		t.Fatalf("expected allowWriteSession to cover second new file create, got %d new approval calls", len(seenReqs)-reqsBefore)
	}
	searchRaw, err := reg.Execute(ctx, "search_files", `{"query":"UniqueDeliverKeyword9942"}`)
	if err != nil || !strings.Contains(searchRaw, "Q4_Summary.md") {
		t.Fatalf("expected newly delivered markdown file to be searchable immediately, got err=%v raw=%s", err, searchRaw)
	}

	// 5. Overwriting WITHOUT overwrite=true fails fast WITHOUT invoking approver.
	reqsBefore = len(seenReqs)
	raw, err = reg.Execute(ctx, "deliver_file", `{"dest_rel_path":"Q4_Summary.md","content":"replacement"}`)
	if err != nil || !strings.Contains(raw, "already exists") {
		t.Fatalf("expected already exists error without overwrite=true, got err=%v raw=%s", err, raw)
	}
	if len(seenReqs) != reqsBefore {
		t.Fatalf("missing overwrite=true should fail before prompting approver")
	}

	// 6. Overwriting WITH overwrite=true MUST still prompt even though allowWriteSession is true,
	// and MUST create a shadow backup outside the workspace before replacing.
	nextDecision = ApprovalAllowSession // even if approver returns AllowSession, overwrites never auto-approve later overwrites
	raw, err = reg.Execute(ctx, "deliver_file", `{"dest_rel_path":"Q4_Summary.md","content":"# Q4 Summary v2\n\nUpdatedDeliverKeyword7711\n","overwrite":true}`)
	if err != nil || strings.Contains(raw, `"error"`) {
		t.Fatalf("deliver_file overwrite failed: err=%v raw=%s", err, raw)
	}
	if len(seenReqs) != reqsBefore+1 || seenReqs[len(seenReqs)-1].Kind != ApprovalKindFileOverwrite || !seenReqs[len(seenReqs)-1].WillOverwrite {
		t.Fatalf("expected overwrite approval prompt despite allowWriteSession, got %+v", seenReqs[reqsBefore:])
	}
	var overOut deliverFileOut
	if err := json.Unmarshal([]byte(raw), &overOut); err != nil {
		t.Fatalf("unmarshal overwrite out: %v (%s)", err, raw)
	}
	if !overOut.Overwrote || overOut.BackupPath == "" || !overOut.Indexed {
		t.Fatalf("expected overwrote=true, non-empty backup_path, indexed=true; got %+v", overOut)
	}
	// Backup must live under dataDir/backups/news-reporter (NOT inside user workspace root) and hold original bytes.
	if !strings.HasPrefix(overOut.BackupPath, filepath.Join(dataDir, "backups", "news-reporter")) {
		t.Fatalf("expected backup inside dataDir/backups/news-reporter, got %q", overOut.BackupPath)
	}
	if strings.HasPrefix(overOut.BackupPath, root) {
		t.Fatalf("backup must not be stored inside user workspace: %q", overOut.BackupPath)
	}
	backupBytes, err := os.ReadFile(overOut.BackupPath)
	if err != nil || !strings.Contains(string(backupBytes), "UniqueDeliverKeyword9942") {
		t.Fatalf("backup missing original content: err=%v content=%q", err, string(backupBytes))
	}
	// Updated content is live on disk and in search index.
	searchRaw2, err := reg.Execute(ctx, "search_files", `{"query":"UpdatedDeliverKeyword7711"}`)
	if err != nil || !strings.Contains(searchRaw2, "Q4_Summary.md") {
		t.Fatalf("expected overwritten content to be indexed, got err=%v raw=%s", err, searchRaw2)
	}

	// Second overwrite MUST prompt again (AllowSession on overwrite never sticks).
	reqsBefore = len(seenReqs)
	nextDecision = ApprovalDeny
	raw, err = reg.Execute(ctx, "deliver_file", `{"dest_rel_path":"Q4_Summary.md","content":"v3","overwrite":true}`)
	if err != nil || !strings.Contains(raw, "denied by user") {
		t.Fatalf("expected second overwrite to prompt and be denied, got err=%v raw=%s", err, raw)
	}
	if len(seenReqs) != reqsBefore+1 {
		t.Fatalf("expected second overwrite to invoke approver")
	}

	// 7. Validation & confinement edge cases (including Windows reserved names, control chars, bidi, .faro, invalid UTF-8, size limits).
	for _, tc := range []struct {
		name string
		args string
	}{
		{"both scratch and content", `{"scratch_path":"scratch/out/employee_data.xlsx","content":"hi","dest_rel_path":"a.txt"}`},
		{"neither scratch nor content", `{"dest_rel_path":"a.txt"}`},
		{"scratch outside scratch/", `{"scratch_path":"memory.md","dest_rel_path":"mem.md"}`},
		{"dest traversal", `{"content":"x","dest_rel_path":"../escape.txt"}`},
		{"dest absolute", `{"content":"x","dest_rel_path":"/tmp/escape.txt"}`},
		{"dest ignored dir", `{"content":"x","dest_rel_path":".git/config"}`},
		{"dest .faro dir", `{"content":"x","dest_rel_path":".faro/changes.log"}`},
		{"dest office lockfile", `{"content":"x","dest_rel_path":"~$Book1.xlsx"}`},
		{"dest existing dir", `{"content":"x","dest_rel_path":"Reports"}`},
		{"dest ANSI escape", `{"content":"x","dest_rel_path":"ok\u001b[2Jfake.md"}`},
		{"dest newline", `{"content":"x","dest_rel_path":"a\nb.md"}`},
		{"dest bidi override", `{"content":"x","dest_rel_path":"doc\u202Efdp.exe"}`},
		{"dest colon ADS", `{"content":"x","dest_rel_path":"report.md:hidden"}`},
		{"dest drive relative", `{"content":"x","dest_rel_path":"C:foo.txt"}`},
		{"dest reserved name NUL", `{"content":"x","dest_rel_path":"NUL.txt"}`},
		{"dest reserved name CON", `{"content":"x","dest_rel_path":"sub/con"}`},
		{"dest trailing dot", `{"content":"x","dest_rel_path":"report."}`},
		{"dest trailing space", `{"content":"x","dest_rel_path":"report "}`},
		{"dest dir trailing space", `{"content":"x","dest_rel_path":"Reports /file.txt"}`},
		{"content over 256KB", `{"content":"` + strings.Repeat("a", maxBotWriteBytes+1) + `","dest_rel_path":"big.txt"}`},
	} {
		raw, err := reg.Execute(ctx, "deliver_file", tc.args)
		if err != nil || !strings.Contains(raw, `"error"`) {
			t.Fatalf("case %q: expected error JSON, got err=%v raw=%s", tc.name, err, raw)
		}
	}

	// Content exactly at 256 KB succeeds.
	exactArgs, _ := json.Marshal(map[string]any{
		"dest_rel_path": "exact_256k.txt",
		"content":       strings.Repeat("b", maxBotWriteBytes),
	})
	raw, err = reg.Execute(ctx, "deliver_file", string(exactArgs))
	if err != nil || strings.Contains(raw, `"error"`) {
		t.Fatalf("content at exact 256KB limit should succeed, got err=%v raw=%s", err, raw)
	}

	// Staged file over 100 MB (sparse) is rejected.
	bigStaged := filepath.Join(reporter.HomePath, bot.ScratchDirName, bot.ScratchOutDirName, "huge.bin")
	if bf, berr := os.Create(bigStaged); berr == nil {
		_ = bf.Truncate(maxDeliverFileBytes + 1)
		_ = bf.Close()
		raw, err = reg.Execute(ctx, "deliver_file", `{"scratch_path":"scratch/out/huge.bin","dest_rel_path":"huge.bin"}`)
		if err != nil || !strings.Contains(raw, "exceeds max delivery size") {
			t.Fatalf("expected >100MB staged file rejection, got err=%v raw=%s", err, raw)
		}
		_ = os.Remove(bigStaged)
	}

	// Overwrite preserves original file permissions (e.g. 0600 private file stays 0600 on Unix).
	if runtime.GOOS != "windows" {
		privPath := filepath.Join(root, "private.txt")
		if err := os.WriteFile(privPath, []byte("secret v1"), 0o600); err != nil {
			t.Fatalf("write private.txt: %v", err)
		}
		nextDecision = ApprovalAllow
		raw, err = reg.Execute(ctx, "deliver_file", `{"dest_rel_path":"private.txt","content":"secret v2","overwrite":true}`)
		if err != nil || strings.Contains(raw, `"error"`) {
			t.Fatalf("overwrite private.txt failed: err=%v raw=%s", err, raw)
		}
		info, err := os.Stat(privPath)
		if err != nil {
			t.Fatalf("stat private.txt: %v", err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("expected overwritten private.txt to preserve 0600 perm, got %#o", info.Mode().Perm())
		}
	}

	// Symlink destination, symlink source, and symlink parent directory escaping workspace are rejected.
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "outside.txt")
	mustWriteFile(t, outsideFile, "untouched")
	symDest := filepath.Join(root, "link_dest.txt")
	if err := os.Symlink(outsideFile, symDest); err != nil {
		t.Logf("skipping symlink subchecks (os.Symlink unsupported: %v)", err)
	} else {
		nextDecision = ApprovalAllow
		raw, err := reg.Execute(ctx, "deliver_file", `{"content":"pwned","dest_rel_path":"link_dest.txt","overwrite":true}`)
		if err != nil || !strings.Contains(raw, `"error"`) {
			t.Fatalf("expected symlink overwrite to be rejected, got err=%v raw=%s", err, raw)
		}
		if b, _ := os.ReadFile(outsideFile); string(b) != "untouched" {
			t.Fatalf("outside file was modified via symlink: %q", string(b))
		}
		// Symlink parent dir inside workspace.
		symDir := filepath.Join(root, "EscapeDir")
		if err := os.Symlink(outsideDir, symDir); err == nil {
			raw, err = reg.Execute(ctx, "deliver_file", `{"content":"pwned","dest_rel_path":"EscapeDir/evil.txt"}`)
			if err != nil || !strings.Contains(raw, `"error"`) {
				t.Fatalf("expected symlink parent dir write to be rejected, got err=%v raw=%s", err, raw)
			}
		}
		// Symlink source inside scratch/out.
		symSrc := filepath.Join(reporter.HomePath, bot.ScratchDirName, bot.ScratchOutDirName, "leak.txt")
		if err := os.Symlink(outsideFile, symSrc); err == nil {
			raw, err = reg.Execute(ctx, "deliver_file", `{"scratch_path":"scratch/out/leak.txt","dest_rel_path":"leaked.txt"}`)
			if err != nil || !strings.Contains(raw, `"error"`) {
				t.Fatalf("expected symlink source delivery to be rejected, got err=%v raw=%s", err, raw)
			}
		}
	}

	// Destination inside Faro's own dataDir or bot home (when a parent folder is mounted) is rejected.
	parentMount := t.TempDir()
	nestedDataDir := filepath.Join(parentMount, "faro-data")
	if err := os.MkdirAll(nestedDataDir, 0o755); err != nil {
		t.Fatalf("mkdir nestedDataDir: %v", err)
	}
	parentWS, err := st.EnsureWorkspace(ctx, parentMount)
	if err != nil {
		t.Fatalf("EnsureWorkspace parentMount: %v", err)
	}
	regNested, err := NewRegistryWithOptions(st, []store.Workspace{parentWS}, &reporter, RegistryOptions{
		DataDir: nestedDataDir,
		Approver: func(_ context.Context, _ ApprovalRequest) (ApprovalDecision, error) {
			return ApprovalAllow, nil
		},
	})
	if err != nil {
		t.Fatalf("NewRegistryWithOptions nested: %v", err)
	}
	raw, err = regNested.Execute(ctx, "deliver_file", `{"dest_rel_path":"faro-data/catalog.db","content":"corrupt"}`)
	if err != nil || !strings.Contains(raw, "internal state") {
		t.Fatalf("expected Faro state destination to be rejected, got err=%v raw=%s", err, raw)
	}

	// File created while approval is pending triggers TOCTOU guard and refuses silent overwrite.
	regTOCTOU, err := NewRegistryWithOptions(st, []store.Workspace{ws}, &reporter, RegistryOptions{
		DataDir: dataDir,
		Approver: func(_ context.Context, req ApprovalRequest) (ApprovalDecision, error) {
			// Simulate concurrent file creation during user prompt.
			if req.DestRelPath == "raced.txt" {
				mustWriteFile(t, filepath.Join(root, "raced.txt"), "user wrote this concurrently")
			}
			return ApprovalAllow, nil
		},
	})
	if err != nil {
		t.Fatalf("NewRegistryWithOptions TOCTOU: %v", err)
	}
	raw, err = regTOCTOU.Execute(ctx, "deliver_file", `{"dest_rel_path":"raced.txt","content":"bot overwrite"}`)
	if err != nil || !strings.Contains(raw, "created while awaiting approval") {
		t.Fatalf("expected TOCTOU create race to abort, got err=%v raw=%s", err, raw)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "raced.txt")); string(b) != "user wrote this concurrently" {
		t.Fatalf("TOCTOU race clobbered concurrent user file: %q", string(b))
	}

	// Approver returning ErrApprovalNonInteractive surfaces non-interactive message (not "denied by user"),
	// and generic approver errors are recorded in changes.log.
	regNonInt, err := NewRegistryWithOptions(st, []store.Workspace{ws}, &reporter, RegistryOptions{
		DataDir: dataDir,
		Approver: func(_ context.Context, _ ApprovalRequest) (ApprovalDecision, error) {
			return ApprovalDeny, ErrApprovalNonInteractive
		},
	})
	if err != nil {
		t.Fatalf("NewRegistryWithOptions non-interactive: %v", err)
	}
	raw, err = regNonInt.Execute(ctx, "deliver_file", `{"dest_rel_path":"ni.txt","content":"hi"}`)
	if err != nil || !strings.Contains(raw, "--allow-writes") || strings.Contains(raw, "denied by user") {
		t.Fatalf("expected non-interactive message with --allow-writes, got err=%v raw=%s", err, raw)
	}

	// Multiple workspaces: omitting workspace_id fails cleanly, and per-workspace allowWriteWorkspaces does not spill across workspaces.
	ws2Root := t.TempDir()
	ws2, err := st.EnsureWorkspace(ctx, ws2Root)
	if err != nil {
		t.Fatalf("EnsureWorkspace ws2: %v", err)
	}
	multiCalls := 0
	regMulti, err := NewRegistryWithOptions(st, []store.Workspace{ws, ws2}, &reporter, RegistryOptions{
		DataDir: dataDir,
		Approver: func(_ context.Context, _ ApprovalRequest) (ApprovalDecision, error) {
			multiCalls++
			return ApprovalAllowSession, nil
		},
	})
	if err != nil {
		t.Fatalf("NewRegistryWithOptions multi: %v", err)
	}
	raw, err = regMulti.Execute(ctx, "deliver_file", `{"dest_rel_path":"ambiguous.txt","content":"x"}`)
	if err != nil || !strings.Contains(raw, "workspace_id is required") {
		t.Fatalf("expected workspace_id required error, got err=%v raw=%s", err, raw)
	}
	raw, err = regMulti.Execute(ctx, "deliver_file", `{"workspace_id":`+itoa(ws.ID)+`,"dest_rel_path":"m1.txt","content":"1"}`)
	if err != nil || strings.Contains(raw, `"error"`) {
		t.Fatalf("multi ws1 create: err=%v raw=%s", err, raw)
	}
	raw, err = regMulti.Execute(ctx, "deliver_file", `{"workspace_id":`+itoa(ws.ID)+`,"dest_rel_path":"m2.txt","content":"2"}`)
	if err != nil || strings.Contains(raw, `"error"`) {
		t.Fatalf("multi ws1 create 2: err=%v raw=%s", err, raw)
	}
	if multiCalls != 1 {
		t.Fatalf("expected ws1 session grant to cover second create on ws1, calls=%d", multiCalls)
	}
	raw, err = regMulti.Execute(ctx, "deliver_file", `{"workspace_id":`+itoa(ws2.ID)+`,"dest_rel_path":"m3.txt","content":"3"}`)
	if err != nil || strings.Contains(raw, `"error"`) {
		t.Fatalf("multi ws2 create: err=%v raw=%s", err, raw)
	}
	if multiCalls != 2 {
		t.Fatalf("expected ws1 session grant NOT to cover ws2, calls=%d", multiCalls)
	}

	// 8. Changes audit log exists and records creates, overwrites, backup_path, denials, and conversation_id.
	changesBytes, err := os.ReadFile(filepath.Join(reporter.HomePath, ".faro", "changes.log"))
	if err != nil {
		t.Fatalf("read changes.log: %v", err)
	}
	changesText := string(changesBytes)
	for _, want := range []string{`"conv_deliver_test"`, `"Reports/employee_data.xlsx"`, `"overwrite"`, overOut.BackupPath, `"deny"`} {
		if !strings.Contains(changesText, want) {
			t.Fatalf("changes.log missing %q:\n%s", want, changesText)
		}
	}

	// 9. Bot-less registry with a workspace gets 4 tools (3 read + deliver_file) and supports inline content only.
	regPlain, err := NewRegistryWithOptions(st, []store.Workspace{ws}, nil, RegistryOptions{
		DataDir: dataDir,
		Approver: func(_ context.Context, req ApprovalRequest) (ApprovalDecision, error) {
			if req.BotSlug != "faro" {
				t.Errorf("expected bot-less actor slug 'faro', got %q", req.BotSlug)
			}
			return ApprovalAllow, nil
		},
	})
	if err != nil {
		t.Fatalf("NewRegistryWithOptions plain: %v", err)
	}
	if len(regPlain.Definitions()) != 4 {
		t.Fatalf("expected 4 tool defs for bot-less workspace registry, got %d", len(regPlain.Definitions()))
	}
	raw, err = regPlain.Execute(ctx, "deliver_file", `{"scratch_path":"scratch/out/x.txt","dest_rel_path":"x.txt"}`)
	if err != nil || !strings.Contains(raw, "requires a bot session") {
		t.Fatalf("expected scratch_path error in bot-less session, got err=%v raw=%s", err, raw)
	}
	raw, err = regPlain.Execute(ctx, "deliver_file", `{"content":"plain hello","dest_rel_path":"plain_note.md"}`)
	if err != nil || strings.Contains(raw, `"error"`) {
		t.Fatalf("bot-less inline deliver_file failed: err=%v raw=%s", err, raw)
	}
}
