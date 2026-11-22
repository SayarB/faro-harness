package bot

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SayarB/faro-harness/internal/store"
)

func TestBotManagerCreateLoadAndIsolation(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	botsDir := t.TempDir()
	mgr, err := NewManager(st, botsDir)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	ctx := context.Background()

	ledger, err := mgr.Create(ctx, CreateOptions{
		Slug:         "ledger",
		Name:         "Ledger",
		Instructions: "You handle invoices and expenses.",
	})
	if err != nil {
		t.Fatalf("create ledger: %v", err)
	}
	counsel, err := mgr.Create(ctx, CreateOptions{
		Slug:         "counsel",
		Name:         "Counsel",
		Instructions: "You review contracts and NDAs.",
	})
	if err != nil {
		t.Fatalf("create counsel: %v", err)
	}

	if ledger.HomePath == counsel.HomePath {
		t.Fatalf("bots must have separate home paths")
	}
	// scratch/out/ is pre-created so command-generated deliverables never fail on missing dir.
	ledgerOutDir := filepath.Join(ledger.HomePath, ScratchDirName, ScratchOutDirName)
	if info, err := os.Stat(ledgerOutDir); err != nil || !info.IsDir() {
		t.Fatalf("expected scratch/out/ to be pre-created on Create: err=%v", err)
	}
	// LoadProfile also restores scratch/out/ if removed manually.
	if err := os.RemoveAll(ledgerOutDir); err != nil {
		t.Fatalf("remove scratch/out: %v", err)
	}
	if _, err := LoadProfile(ledger); err != nil {
		t.Fatalf("LoadProfile recreate scratch/out: %v", err)
	}
	if info, err := os.Stat(ledgerOutDir); err != nil || !info.IsDir() {
		t.Fatalf("expected scratch/out/ to be recreated by LoadProfile: err=%v", err)
	}

	// Duplicate slug rejected.
	if _, err := mgr.Create(ctx, CreateOptions{Slug: "ledger"}); err == nil {
		t.Fatalf("expected error on duplicate slug")
	}
	// Path-traversal slug rejected.
	if _, err := mgr.Create(ctx, CreateOptions{Slug: "../escape"}); err == nil {
		t.Fatalf("expected error on invalid slug")
	}

	// Edit ledger's memory and scratchpad on disk; Counsel's profile must remain completely clean.
	if err := os.WriteFile(filepath.Join(ledger.HomePath, MemoryFileName), []byte("# Memory\n- Fiscal year starts April 1.\n"), 0o644); err != nil {
		t.Fatalf("write ledger memory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ledger.HomePath, ScratchDirName, "q1_draft.md"), []byte("draft"), 0o644); err != nil {
		t.Fatalf("write ledger scratch: %v", err)
	}

	ledgerProf, err := LoadProfile(ledger)
	if err != nil {
		t.Fatalf("LoadProfile ledger: %v", err)
	}
	counselProf, err := LoadProfile(counsel)
	if err != nil {
		t.Fatalf("LoadProfile counsel: %v", err)
	}

	if !strings.Contains(ledgerProf.Memory, "Fiscal year starts April 1") {
		t.Fatalf("ledger memory missing expected note: %q", ledgerProf.Memory)
	}
	if len(ledgerProf.ScratchFiles) != 1 || ledgerProf.ScratchFiles[0] != "q1_draft.md" {
		t.Fatalf("unexpected ledger scratch files: %+v", ledgerProf.ScratchFiles)
	}
	if strings.Contains(counselProf.Memory, "Fiscal year") || len(counselProf.ScratchFiles) != 0 {
		t.Fatalf("counsel profile leaked ledger state: %+v", counselProf)
	}

	// Scratch listing skips .venv/, __pycache__/, node_modules/, and dotfiles.
	for _, noise := range []string{
		filepath.Join(ledger.HomePath, ScratchDirName, ".venv", "bin", "python"),
		filepath.Join(ledger.HomePath, ScratchDirName, "__pycache__", "mod.pyc"),
		filepath.Join(ledger.HomePath, ScratchDirName, "node_modules", "pkg", "index.js"),
		filepath.Join(ledger.HomePath, ScratchDirName, ".faro-bot-write-1234"),
	} {
		if err := os.MkdirAll(filepath.Dir(noise), 0o755); err != nil {
			t.Fatalf("mkdir noise: %v", err)
		}
		if err := os.WriteFile(noise, []byte("x"), 0o644); err != nil {
			t.Fatalf("write noise: %v", err)
		}
	}
	ledgerProf2, err := LoadProfile(ledger)
	if err != nil {
		t.Fatalf("LoadProfile ledger after noise: %v", err)
	}
	if len(ledgerProf2.ScratchFiles) != 1 || ledgerProf2.ScratchFiles[0] != "q1_draft.md" {
		t.Fatalf("expected noise dirs/dotfiles to be skipped, got %+v", ledgerProf2.ScratchFiles)
	}

	// Creating a bot whose folder already exists on disk (e.g. after DB reset) fails instead of inheriting stale files.
	staleDir := filepath.Join(botsDir, "stalebot")
	if err := os.MkdirAll(staleDir, 0o755); err != nil {
		t.Fatalf("mkdir staleDir: %v", err)
	}
	if _, err := mgr.Create(ctx, CreateOptions{Slug: "stalebot"}); err == nil {
		t.Fatalf("expected error when bot folder already exists on disk")
	}
}
