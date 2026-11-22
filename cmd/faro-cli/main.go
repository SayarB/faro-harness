package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/SayarB/faro-harness/internal/store"
	"github.com/SayarB/faro-harness/internal/watcher"
	"github.com/SayarB/faro-harness/internal/worker"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	var err error
	switch cmd {
	case "index":
		err = runIndex(args)
	case "watch":
		err = runWatch(args)
	case "list":
		err = runList(args)
	case "search":
		err = runSearch(args)
	default:
		usage()
		os.Exit(1)
	}

	if err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `Usage:
  faro-cli index  [--db <path>] <folder>
  faro-cli watch  [--db <path>] <folder>
  faro-cli list   [--db <path>] [--json] [folder]
  faro-cli search [--db <path>] [--folder <workspace>] [--subdir <rel>] [--ext <ext>] [--depth <depth>] [--phrase <exact>] [--match-all] [--limit N] [query]`)
}

func runIndex(args []string) error {
	fs := flag.NewFlagSet("index", flag.ExitOnError)
	dbPath := fs.String("db", "", "path to catalog SQLite DB (defaults to OS app-data)")
	_ = fs.Parse(args)
	if fs.NArg() < 1 {
		return errors.New("folder path is required")
	}
	folder := fs.Arg(0)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	cat, err := worker.NewCatalogIndexer(ctx, st, folder)
	if err != nil {
		return err
	}
	pipe := worker.NewPipeline(cat, worker.PipelineConfig{
		Logger: log.New(os.Stderr, "", log.LstdFlags),
	})
	defer pipe.Close()

	snap, err := pipe.IndexWorkspace(ctx)
	if err != nil {
		return err
	}
	blobs, _ := st.BlobCount(ctx)
	fmt.Printf("Indexed %s -> db=%s\n", cat.Workspace.RootPath, st.Path())
	fmt.Printf("  indexed=%d skipped=%d blob_cache_hits=%d placeholders=%d deleted=%d errors=%d unique_blobs=%d\n",
		snap.Indexed, snap.Skipped, snap.BlobCacheHits, snap.Placeholders, snap.Deleted, snap.Errors, blobs)
	return nil
}

func runWatch(args []string) error {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	dbPath := fs.String("db", "", "path to catalog SQLite DB (defaults to OS app-data)")
	_ = fs.Parse(args)
	if fs.NArg() < 1 {
		return errors.New("folder path is required")
	}
	folder := fs.Arg(0)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger := log.New(os.Stdout, "", log.LstdFlags)
	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	cat, err := worker.NewCatalogIndexer(ctx, st, folder)
	if err != nil {
		return err
	}
	pipe := worker.NewPipeline(cat, worker.PipelineConfig{Logger: logger})
	pipe.Start(ctx)
	defer pipe.Close()

	w, err := watcher.New(folder, pipe, watcher.WithLogger(logger))
	if err != nil {
		return err
	}

	logger.Printf("watching %s -> db=%s (press Ctrl+C to stop)", w.Folder(), st.Path())
	return w.Run(ctx)
}

func runList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	dbPath := fs.String("db", "", "path to catalog SQLite DB (defaults to OS app-data)")
	asJSON := fs.Bool("json", false, "output JSON")
	_ = fs.Parse(args)

	ctx := context.Background()
	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	var wsID int64
	if fs.NArg() > 0 {
		ws, err := st.EnsureWorkspace(ctx, fs.Arg(0))
		if err != nil {
			return err
		}
		wsID = ws.ID
	}

	entries, err := st.ListFiles(ctx, wsID)
	if err != nil {
		return err
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(entries)
	}

	for _, e := range entries {
		f := e.File
		hashShort := "-"
		if len(f.BLAKE3) >= 12 {
			hashShort = f.BLAKE3[:12]
		}
		fmt.Printf("=== %s ===\n", f.RelPath)
		fmt.Printf("  ext=%s size=%dB depth=%s status=%s blake3=%s\n", f.Ext, f.Size, f.IndexDepth, f.IndexStatus, hashShort)
		if e.Blob != nil {
			if e.Blob.Title != "" {
				fmt.Printf("  title:    %s\n", e.Blob.Title)
			}
			if e.Blob.Headings != "" {
				fmt.Printf("  headings: %s\n", strings.ReplaceAll(e.Blob.Headings, "\n", " | "))
			}
			if e.Blob.OutlineJSON != "" {
				fmt.Printf("  outline:  %s\n", e.Blob.OutlineJSON)
			}
			if e.Blob.BodyPreview != "" {
				preview := strings.ReplaceAll(e.Blob.BodyPreview, "\n", " ↵ ")
				if len(preview) > 220 {
					preview = preview[:220] + "…"
				}
				fmt.Printf("  preview:  %s\n", preview)
			}
		}
		fmt.Println()
	}
	return nil
}

func runSearch(args []string) error {
	fs := flag.NewFlagSet("search", flag.ExitOnError)
	dbPath := fs.String("db", "", "path to catalog SQLite DB (defaults to OS app-data)")
	folder := fs.String("folder", "", "optional workspace root folder filter")
	subdir := fs.String("subdir", "", "optional relative folder prefix filter inside workspace")
	ext := fs.String("ext", "", "optional file extension filter (e.g. .xlsx or xlsx)")
	depth := fs.String("depth", "", "optional index_depth filter (full, header_only, metadata_only)")
	phrase := fs.String("phrase", "", "optional exact phrase match")
	matchAll := fs.Bool("match-all", false, "require all query terms (AND) instead of OR-by-default")
	limit := fs.Int("limit", 10, "max search hits")
	_ = fs.Parse(args)

	query := strings.Join(fs.Args(), " ")
	if query == "" && *phrase == "" && *ext == "" && *subdir == "" && *depth == "" && *folder == "" {
		return errors.New("search query or at least one filter flag is required")
	}

	ctx := context.Background()
	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	var wsID int64
	if *folder != "" {
		ws, err := st.EnsureWorkspace(ctx, *folder)
		if err != nil {
			return err
		}
		wsID = ws.ID
	}

	res, err := st.SearchWithOptions(ctx, store.SearchOptions{
		WorkspaceID: wsID,
		Query:       query,
		ExactPhrase: *phrase,
		MatchAll:    *matchAll,
		Ext:         *ext,
		Folder:      *subdir,
		IndexDepth:  *depth,
		Limit:       *limit,
	})
	if err != nil {
		return err
	}
	if len(res.Hits) == 0 {
		fmt.Println("(no matches)")
		return nil
	}
	for i, h := range res.Hits {
		if h.MatchSource == "filter" {
			fmt.Printf("%d. %s  [%s | filter]\n", i+1, h.RelPath, h.IndexDepth)
		} else {
			fmt.Printf("%d. %s  [%s | %s | rank=%.3f]\n", i+1, h.RelPath, h.IndexDepth, h.MatchSource, h.Rank)
		}
		if h.Title != "" {
			fmt.Printf("   title:   %s\n", h.Title)
		}
		if h.Snippet != "" {
			fmt.Printf("   snippet: %s\n", strings.ReplaceAll(h.Snippet, "\n", " "))
		}
	}
	if res.Truncated {
		fmt.Printf("(showing top %d; more results matched)\n", len(res.Hits))
	}
	return nil
}
