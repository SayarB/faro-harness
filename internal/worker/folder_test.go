package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/SayarB/faro-harness/internal/store"
)

func TestIndexFolderIndexesAndRegistersWorkspace(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.md"), []byte("# Hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ws, snap, err := IndexFolder(context.Background(), st, root, func(StatsSnapshot) {})
	if err != nil {
		t.Fatalf("IndexFolder: %v", err)
	}
	if ws.ID == 0 || snap.Indexed != 1 {
		t.Fatalf("ws=%+v snap=%+v", ws, snap)
	}
	n, err := st.WorkspaceFileCount(context.Background(), ws.ID)
	if err != nil || n != 1 {
		t.Fatalf("file count=%d err=%v", n, err)
	}
}
