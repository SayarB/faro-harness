package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/SayarB/faro-harness/internal/store"
	"github.com/SayarB/faro-harness/internal/worker"
)

func main() {
	defaultDB := filepath.Join(os.TempDir(), "faro-harness", "index.db")
	if cfg, err := os.UserConfigDir(); err == nil {
		defaultDB = filepath.Join(cfg, "faro-harness", "index.db")
	}

	dbPath := flag.String("db", defaultDB, "path to SQLite index database")
	limit := flag.Int("limit", 20, "max search results")
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		usage()
		os.Exit(1)
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		fatalf("open store: %v", err)
	}
	defer st.Close()

	ctx := context.Background()

	switch args[0] {
	case "index":
		if len(args) < 2 {
			fatalf("usage: faro-cli index <directory>")
		}
		idx := worker.NewIndexer(st, 0, nil)
		prog, err := idx.IndexWorkspace(ctx, args[1])
		if err != nil {
			fatalf("index workspace: %v", err)
		}
		fmt.Printf("Indexed %s -> scanned=%d indexed=%d skipped=%d errors=%d\n",
			prog.Workspace, prog.Scanned, prog.Indexed, prog.Skipped, prog.Errors)

	case "search":
		query := strings.Join(args[1:], " ")
		hits, err := st.Search(ctx, query, *limit)
		if err != nil {
			fatalf("search: %v", err)
		}
		if len(hits) == 0 {
			fmt.Println("No matches.")
			return
		}
		for _, h := range hits {
			fmt.Printf("[%s] %s (%d bytes)\n", h.IndexDepth, h.AbsPath, h.Size)
			if h.Snippet != "" {
				fmt.Printf("  %s\n", strings.ReplaceAll(h.Snippet, "\n", " "))
			}
		}

	case "stats":
		stats, err := st.Stats(ctx)
		if err != nil {
			fatalf("stats: %v", err)
		}
		fmt.Printf("Files: %d | Deduplicated Blobs: %d\n", stats.TotalFiles, stats.TotalBlobs)
		for depth, count := range stats.ByDepth {
			fmt.Printf("  %-14s %d\n", depth, count)
		}

	default:
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "Usage: faro-cli [--db path] <index <dir> | search <query> | stats>")
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}
