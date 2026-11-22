package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/SayarB/faro-harness/internal/agent"
	"github.com/SayarB/faro-harness/internal/bot"
	"github.com/SayarB/faro-harness/internal/llm"
	"github.com/SayarB/faro-harness/internal/store"
	"github.com/SayarB/faro-harness/internal/tools"
	"github.com/SayarB/faro-harness/internal/watcher"
	"github.com/SayarB/faro-harness/internal/worker"
)

func main() {
	loadDotEnv(".env")

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
	case "bot":
		err = runBot(args)
	case "chat":
		err = runChat(args)
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
  faro-cli search [--db <path>] [--folder <workspace>] [--subdir <rel>] [--ext <ext>] [--depth <depth>] [--phrase <exact>] [--match-all] [--limit N] [query]
  faro-cli bot    <create|list|show> [flags]
  faro-cli chat   [--db <path>] [--bots-dir <dir>] [--bot <slug>] [--conv <id>] [--model <provider/model>] [--base-url <url>] [--index] [--allow-commands] [--allow-writes] [--allow-overwrites] [--verbose=true|false] [--debug-prompt] [-q <question>] [folder...]`)
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
	_ = fs.Parse(reorderFlagsFirst(args, map[string]bool{"match-all": true}))

	if *depth != "" {
		switch *depth {
		case store.DepthFull, store.DepthHeaderOnly, store.DepthMetadataOnly:
		default:
			return fmt.Errorf("invalid --depth %q (want full, header_only, or metadata_only)", *depth)
		}
	}

	query := strings.Join(fs.Args(), " ")
	if strings.TrimSpace(query) == "" && strings.TrimSpace(*phrase) == "" && *ext == "" && *subdir == "" && *depth == "" && *folder == "" {
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

func runBot(args []string) error {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, `Usage:
  faro-cli bot create [--db <path>] [--bots-dir <dir>] [--name <display>] [--instructions <text>] [--model <provider/model>] <slug>
  faro-cli bot list   [--db <path>]
  faro-cli bot show   [--db <path>] <slug>`)
		return errors.New("bot subcommand is required (create, list, show)")
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "create":
		return runBotCreate(rest)
	case "list":
		return runBotList(rest)
	case "show":
		return runBotShow(rest)
	default:
		return fmt.Errorf("unknown bot subcommand %q (expected create, list, show)", sub)
	}
}

func runBotCreate(args []string) error {
	fs := flag.NewFlagSet("bot create", flag.ExitOnError)
	dbPath := fs.String("db", "", "path to catalog SQLite DB (defaults to OS app-data)")
	botsDir := fs.String("bots-dir", "", "directory where bot home folders are stored")
	name := fs.String("name", "", "display name for the bot (defaults to capitalized slug)")
	instructions := fs.String("instructions", "", "initial bot.md instructions/role")
	model := fs.String("model", "", "optional default model slug for this bot")
	_ = fs.Parse(reorderFlagsFirst(args, nil))

	if fs.NArg() < 1 {
		return errors.New("bot slug is required (e.g. faro-cli bot create ledger)")
	}
	slug := fs.Arg(0)

	ctx := context.Background()
	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	mgr, err := bot.NewManager(st, *botsDir)
	if err != nil {
		return err
	}
	created, err := mgr.Create(ctx, bot.CreateOptions{
		Slug:         slug,
		Name:         *name,
		Instructions: *instructions,
		Model:        *model,
	})
	if err != nil {
		return err
	}
	fmt.Printf("Created bot %q (%s)\n", created.Name, created.Slug)
	fmt.Printf("  id:        %s\n", created.ID)
	fmt.Printf("  home:      %s\n", created.HomePath)
	fmt.Printf("  bot.md:    %s/%s\n", created.HomePath, bot.BotFileName)
	fmt.Printf("  memory.md: %s/%s\n", created.HomePath, bot.MemoryFileName)
	fmt.Printf("  scratch:   %s/%s\n", created.HomePath, bot.ScratchDirName)
	return nil
}

func runBotList(args []string) error {
	fs := flag.NewFlagSet("bot list", flag.ExitOnError)
	dbPath := fs.String("db", "", "path to catalog SQLite DB (defaults to OS app-data)")
	_ = fs.Parse(args)

	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	bots, err := st.ListBots(context.Background())
	if err != nil {
		return err
	}
	if len(bots) == 0 {
		fmt.Println("(no bots yet — run `faro-cli bot create <slug>`)")
		return nil
	}
	for _, b := range bots {
		model := b.Model
		if model == "" {
			model = "(default)"
		}
		fmt.Printf("- %s (%s)  id=%s  model=%s\n  home: %s\n", b.Slug, b.Name, b.ID, model, b.HomePath)
	}
	return nil
}

func runBotShow(args []string) error {
	fs := flag.NewFlagSet("bot show", flag.ExitOnError)
	dbPath := fs.String("db", "", "path to catalog SQLite DB (defaults to OS app-data)")
	_ = fs.Parse(reorderFlagsFirst(args, nil))

	if fs.NArg() < 1 {
		return errors.New("bot slug or id is required")
	}

	ctx := context.Background()
	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	mgr, err := bot.NewManager(st, "")
	if err != nil {
		return err
	}
	b, err := mgr.Get(ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	if b == nil {
		return fmt.Errorf("bot %q not found", fs.Arg(0))
	}
	prof, err := bot.LoadProfile(*b)
	if err != nil {
		return err
	}
	convs, _ := st.ListConversationsForBot(ctx, b.ID)

	fmt.Printf("=== %s (%s) ===\n", b.Name, b.Slug)
	fmt.Printf("id:   %s\n", b.ID)
	fmt.Printf("home: %s\n\n", b.HomePath)
	fmt.Printf("--- bot.md ---\n%s\n\n", prof.Instructions)
	fmt.Printf("--- memory.md ---\n%s\n\n", prof.Memory)
	fmt.Printf("--- scratch/ (%d files) ---\n", len(prof.ScratchFiles))
	for _, f := range prof.ScratchFiles {
		fmt.Printf("  scratch/%s\n", f)
	}
	if len(convs) > 0 {
		fmt.Printf("\n--- conversations (%d) ---\n", len(convs))
		for _, c := range convs {
			fmt.Printf("  %s\n", c.ID)
		}
	}
	return nil
}

func runChat(args []string) error {
	fs := flag.NewFlagSet("chat", flag.ExitOnError)
	dbPath := fs.String("db", "", "path to catalog SQLite DB (defaults to OS app-data)")
	botsDir := fs.String("bots-dir", "", "directory where bot home folders are stored")
	botSlug := fs.String("bot", "", "optional bot slug or id (mounts that bot's isolated memory.md and scratch/)")
	convID := fs.String("conv", "", "optional conversation id to resume")
	defaultModel := strings.TrimSpace(os.Getenv("FARO_MODEL"))
	if defaultModel == "" {
		defaultModel = llm.DefaultModel
	}
	model := fs.String("model", defaultModel, "model slug for Vercel AI Gateway (e.g. openai/gpt-6-luna, anthropic/claude-haiku-5.5)")
	baseURL := fs.String("base-url", "", "optional OpenAI-compatible gateway base URL (defaults to Vercel AI Gateway)")
	reindex := fs.Bool("index", false, "run an index pass on <folder> before chatting")
	allowCommands := fs.Bool("allow-commands", false, "auto-approve bot_run_command without prompting (runs with full user permissions when unsandboxed)")
	allowWrites := fs.Bool("allow-writes", false, "auto-approve creating NEW workspace files via deliver_file without prompting (does not allow overwrites)")
	allowOverwrites := fs.Bool("allow-overwrites", false, "auto-approve overwriting existing workspace files via deliver_file without prompting (originals are still backed up)")
	verbose := fs.Bool("verbose", true, "print tool calls and token usage to stderr")
	debugPrompt := fs.Bool("debug-prompt", false, "print the composed system prompt at startup")
	question := fs.String("q", "", "optional one-shot question (non-interactive)")

	reordered := reorderFlagsFirst(args, map[string]bool{
		"index":            true,
		"allow-commands":   true,
		"allow-writes":     true,
		"allow-overwrites": true,
		"verbose":          true,
		"debug-prompt":     true,
	})
	_ = fs.Parse(reordered)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client, err := llm.NewVercelGatewayClient("", *baseURL)
	if err != nil {
		return err
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	var activeBot *store.Bot
	if strings.TrimSpace(*botSlug) != "" {
		mgr, err := bot.NewManager(st, *botsDir)
		if err != nil {
			return err
		}
		b, err := mgr.Get(ctx, *botSlug)
		if err != nil {
			return err
		}
		if b == nil {
			return fmt.Errorf("bot %q not found (create one with `faro-cli bot create %s`)", *botSlug, *botSlug)
		}
		activeBot = b
	}

	isInteractive := strings.TrimSpace(*question) == ""
	var stdinReader *stdinLineReader
	if isInteractive {
		stdinReader = newStdinLineReader(os.Stdin)
	}

	indexWorkspace := func(root string) error {
		cat, err := worker.NewCatalogIndexer(ctx, st, root)
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
		fmt.Fprintf(os.Stderr, "[index] %s: indexed=%d skipped=%d blob_hits=%d deleted=%d errors=%d\n",
			cat.Workspace.RootPath, snap.Indexed, snap.Skipped, snap.BlobCacheHits, snap.Deleted, snap.Errors)
		return nil
	}

	addOrCreateFolder := func(rawPath string, createIfMissing bool) (store.Workspace, error) {
		resolved := expandHomeDir(rawPath)
		info, err := os.Stat(resolved)
		if err != nil {
			if os.IsNotExist(err) {
				if !createIfMissing {
					return store.Workspace{}, fmt.Errorf("folder %q does not exist", resolved)
				}
				if err := os.MkdirAll(resolved, 0o755); err != nil {
					return store.Workspace{}, fmt.Errorf("create folder %q: %w", resolved, err)
				}
				fmt.Fprintf(os.Stderr, "[folder] created %s\n", resolved)
			} else {
				return store.Workspace{}, err
			}
		} else if !info.IsDir() {
			return store.Workspace{}, fmt.Errorf("%q is a file, not a folder", resolved)
		}

		ws, err := st.EnsureWorkspace(ctx, resolved)
		if err != nil {
			return store.Workspace{}, err
		}
		fileCount, err := st.WorkspaceFileCount(ctx, ws.ID)
		if err != nil {
			return store.Workspace{}, err
		}
		if *reindex || fileCount == 0 {
			if err := indexWorkspace(ws.RootPath); err != nil {
				return store.Workspace{}, err
			}
		}
		return ws, nil
	}

	var workspaces []store.Workspace
	if fs.NArg() > 0 {
		for _, arg := range fs.Args() {
			resolved := expandHomeDir(arg)
			createMissing := false
			if _, err := os.Stat(resolved); os.IsNotExist(err) && isInteractive {
				fmt.Fprintf(os.Stderr, "Folder %q does not exist. Create it? [y/N]: ", resolved)
				ans, err := stdinReader.ReadLine(ctx)
				if err != nil {
					return err
				}
				if strings.HasPrefix(strings.ToLower(strings.TrimSpace(ans)), "y") {
					createMissing = true
				}
			}
			ws, err := addOrCreateFolder(resolved, createMissing)
			if err != nil {
				return err
			}
			workspaces = append(workspaces, ws)
		}
	} else {
		allWS, err := st.ListWorkspaces(ctx)
		if err != nil {
			return err
		}
		for _, ws := range allWS {
			if ws.Kind == store.WorkspaceKindUser || ws.Kind == "" {
				workspaces = append(workspaces, ws)
			}
		}
		if len(workspaces) > 0 && *reindex {
			for _, ws := range workspaces {
				if err := indexWorkspace(ws.RootPath); err != nil {
					return err
				}
			}
		}
		if len(workspaces) == 0 && isInteractive {
			ws, err := promptForInitialFolder(ctx, stdinReader, activeBot != nil, addOrCreateFolder)
			if err != nil {
				return err
			}
			if ws != nil {
				workspaces = append(workspaces, *ws)
			}
		}
	}

	if len(workspaces) == 0 && activeBot == nil {
		return errors.New("no folders are added yet; pass a folder path, add one interactively, or use --bot <slug>")
	}

	effectiveModel := *model
	if activeBot != nil && activeBot.Model != "" && !flagPassed(fs, "model") {
		effectiveModel = activeBot.Model
	}

	var toolApprover tools.Approver
	if isInteractive || *allowCommands || *allowWrites || *allowOverwrites {
		toolApprover = newCLIApprover(cliApproverConfig{
			interactive:     isInteractive,
			allowCommands:   *allowCommands,
			allowWrites:     *allowWrites,
			allowOverwrites: *allowOverwrites,
			promptOut:       os.Stderr,
			readLine: func(callCtx context.Context) (string, error) {
				return stdinReader.ReadLine(callCtx)
			},
		})
	}

	warnedUnsandboxedCommands := false
	buildSession := func(resumeConvID string) (*agent.Session, error) {
		reg, err := tools.NewRegistryWithOptions(st, workspaces, activeBot, tools.RegistryOptions{
			Approver: toolApprover,
		})
		if err != nil {
			return nil, err
		}
		if *allowCommands && !reg.SandboxEnforced() && !warnedUnsandboxedCommands {
			warnedUnsandboxedCommands = true
			fmt.Fprintln(os.Stderr, "[warning] --allow-commands is active without an OS sandbox: commands run with your full user permissions and network access")
		}
		return agent.NewSession(agent.Config{
			Client:         client,
			Tools:          reg,
			Workspaces:     workspaces,
			Bot:            activeBot,
			Store:          st,
			ConversationID: resumeConvID,
			Model:          effectiveModel,
			OnEvent: func(ev agent.Event) {
				if !*verbose {
					return
				}
				switch ev.Kind {
				case agent.EventToolCall:
					fmt.Fprintf(os.Stderr, "  [step %d] -> %s(%s)\n", ev.Step, ev.ToolName, ev.ToolArgs)
				case agent.EventToolResult:
					fmt.Fprintf(os.Stderr, "  [step %d] <- %s (%d bytes, %s)\n", ev.Step, ev.ToolName, len(ev.ToolResult), ev.Duration.Round(1e5))
				}
			},
		})
	}

	sess, err := buildSession(strings.TrimSpace(*convID))
	if err != nil {
		return err
	}

	if *debugPrompt {
		fmt.Fprintf(os.Stderr, "=== SYSTEM PROMPT ===\n%s\n=====================\n", sess.SystemPrompt())
	}

	runTurn := func(q string) error {
		res, err := sess.Turn(ctx, q)
		if err != nil {
			return err
		}
		fmt.Printf("\n%s\n\n", strings.TrimSpace(res.Reply))
		if *verbose {
			fmt.Fprintf(os.Stderr, "[done] steps=%d tools=%d tokens(prompt=%d cached=%d cache_write=%d completion=%d) conv=%s\n",
				res.Steps, res.ToolCalls, res.Usage.PromptTokens, res.Usage.CachedTokens, res.Usage.CacheWriteTokens, res.Usage.CompletionTokens, sess.ConversationID())
		}
		return nil
	}

	if q := strings.TrimSpace(*question); q != "" {
		return runTurn(q)
	}

	wsSummary := formatWorkspaceSummary(workspaces)
	if activeBot != nil {
		fmt.Fprintf(os.Stderr, "Faro chat | bot=%s (%s) | folders=%s | model=%s | conv=%s\n",
			activeBot.Name, activeBot.Slug, wsSummary, effectiveModel, sess.ConversationID())
		fmt.Fprintln(os.Stderr, "Commands: /add-folder <path>, /prompt, /memory, /scratch, /reindex, /reset, /exit")
	} else {
		fmt.Fprintf(os.Stderr, "Faro chat | folders=%s | model=%s | conv=%s\n",
			wsSummary, effectiveModel, sess.ConversationID())
		fmt.Fprintln(os.Stderr, "Commands: /add-folder <path>, /prompt, /reindex, /reset, /exit")
	}

	for {
		promptLabel := "\nyou> "
		if activeBot != nil {
			promptLabel = fmt.Sprintf("\nyou -> %s> ", activeBot.Slug)
		}
		fmt.Fprint(os.Stderr, promptLabel)
		line, err := stdinReader.ReadLine(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				fmt.Fprintln(os.Stderr)
				return nil
			}
			return err
		}
		input := strings.TrimSpace(line)
		if input == "" {
			continue
		}
		if strings.HasPrefix(input, "/add-folder") {
			target := strings.TrimSpace(strings.TrimPrefix(input, "/add-folder"))
			if target == "" {
				fmt.Fprint(os.Stderr, "Folder path (existing or new to create): ")
				p, err := stdinReader.ReadLine(ctx)
				if err != nil {
					if errors.Is(err, context.Canceled) {
						return err
					}
					continue
				}
				target = strings.TrimSpace(p)
			}
			if target == "" {
				fmt.Fprintln(os.Stderr, "(cancelled)")
				continue
			}
			ws, err := addOrCreateFolder(target, true)
			if err != nil {
				fmt.Fprintf(os.Stderr, "add-folder error: %v\n", err)
				continue
			}
			workspaces = appendUniqueWorkspace(workspaces, ws)
			rebuilt, err := buildSession(sess.ConversationID())
			if err != nil {
				fmt.Fprintf(os.Stderr, "session reload error: %v\n", err)
				continue
			}
			sess = rebuilt
			fmt.Fprintf(os.Stderr, "[mounted] %s (id=%d)\n", ws.RootPath, ws.ID)
			continue
		}
		switch input {
		case "/exit", "/quit":
			return nil
		case "/reset":
			if err := sess.Reset(); err != nil {
				fmt.Fprintf(os.Stderr, "reset error: %v\n", err)
			} else {
				fmt.Fprintf(os.Stderr, "[session reset; conv=%s]\n", sess.ConversationID())
			}
		case "/prompt":
			fmt.Fprintf(os.Stderr, "=== SYSTEM PROMPT ===\n%s\n=====================\n", sess.SystemPrompt())
		case "/memory":
			if activeBot == nil {
				fmt.Fprintln(os.Stderr, "(no bot active; pass --bot <slug>)")
			} else if prof, err := bot.LoadProfile(*activeBot); err != nil {
				fmt.Fprintf(os.Stderr, "memory error: %v\n", err)
			} else {
				fmt.Fprintf(os.Stderr, "=== %s/memory.md ===\n%s\n", activeBot.Slug, prof.Memory)
			}
		case "/scratch":
			if activeBot == nil {
				fmt.Fprintln(os.Stderr, "(no bot active; pass --bot <slug>)")
			} else if prof, err := bot.LoadProfile(*activeBot); err != nil {
				fmt.Fprintf(os.Stderr, "scratch error: %v\n", err)
			} else if len(prof.ScratchFiles) == 0 {
				fmt.Fprintf(os.Stderr, "(%s/scratch/ is empty)\n", activeBot.Slug)
			} else {
				fmt.Fprintf(os.Stderr, "=== %s/scratch/ ===\n", activeBot.Slug)
				for _, f := range prof.ScratchFiles {
					fmt.Fprintf(os.Stderr, "  scratch/%s\n", f)
				}
			}
		case "/reindex":
			if len(workspaces) == 0 {
				fmt.Fprintln(os.Stderr, "(no folders mounted yet; use /add-folder <path>)")
			} else {
				for _, ws := range workspaces {
					if err := indexWorkspace(ws.RootPath); err != nil {
						fmt.Fprintf(os.Stderr, "reindex error (%s): %v\n", ws.RootPath, err)
					}
				}
			}
		default:
			if err := runTurn(input); err != nil {
				if errors.Is(err, context.Canceled) {
					return err
				}
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
			}
		}
	}
}

type stdinLine struct {
	text string
	err  error
}

// stdinLineReader owns a single goroutine reading lines from r so multiple prompts
// (REPL + approval prompt) never race on one bufio.Reader after cancellation.
type stdinLineReader struct {
	lines chan stdinLine
}

func newStdinLineReader(r io.Reader) *stdinLineReader {
	sr := &stdinLineReader{lines: make(chan stdinLine)}
	br := bufio.NewReader(r)
	go func() {
		defer close(sr.lines)
		for {
			line, err := br.ReadString('\n')
			if line != "" || err != nil {
				sr.lines <- stdinLine{text: line, err: err}
			}
			if err != nil {
				return
			}
		}
	}()
	return sr
}

func (sr *stdinLineReader) ReadLine(ctx context.Context) (string, error) {
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case item, ok := <-sr.lines:
		if !ok {
			return "", io.EOF
		}
		return item.text, item.err
	}
}

func promptForInitialFolder(
	ctx context.Context,
	reader *stdinLineReader,
	hasBot bool,
	addOrCreate func(rawPath string, createIfMissing bool) (store.Workspace, error),
) (*store.Workspace, error) {
	for {
		fmt.Fprintln(os.Stderr, "No folders are added to Faro yet:")
		fmt.Fprintln(os.Stderr, "  1) Add an existing folder")
		fmt.Fprintln(os.Stderr, "  2) Create a new folder")
		if hasBot {
			fmt.Fprintln(os.Stderr, "  3) Continue without a folder (bot memory & scratchpad only)")
			fmt.Fprint(os.Stderr, "Choose [1/2/3]: ")
		} else {
			fmt.Fprint(os.Stderr, "Choose [1/2]: ")
		}
		choiceLine, err := reader.ReadLine(ctx)
		if err != nil {
			return nil, err
		}
		choice := strings.TrimSpace(choiceLine)
		switch choice {
		case "1":
			fmt.Fprint(os.Stderr, "Path to existing folder: ")
			p, err := reader.ReadLine(ctx)
			if err != nil {
				return nil, err
			}
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			ws, err := addOrCreate(p, false)
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n\n", err)
				continue
			}
			return &ws, nil
		case "2":
			fmt.Fprint(os.Stderr, "Path for new folder to create: ")
			p, err := reader.ReadLine(ctx)
			if err != nil {
				return nil, err
			}
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			ws, err := addOrCreate(p, true)
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n\n", err)
				continue
			}
			return &ws, nil
		case "3":
			if hasBot {
				return nil, nil
			}
			fmt.Fprintln(os.Stderr, "Please choose 1 or 2.")
		default:
			fmt.Fprintln(os.Stderr, "Invalid choice.")
		}
	}
}

func expandHomeDir(p string) string {
	p = strings.TrimSpace(p)
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			if p == "~" {
				return home
			}
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

func appendUniqueWorkspace(list []store.Workspace, ws store.Workspace) []store.Workspace {
	for _, existing := range list {
		if existing.ID == ws.ID {
			return list
		}
	}
	return append(list, ws)
}

func formatWorkspaceSummary(workspaces []store.Workspace) string {
	if len(workspaces) == 0 {
		return "(none)"
	}
	if len(workspaces) == 1 {
		return workspaces[0].RootPath
	}
	names := make([]string, 0, len(workspaces))
	for _, ws := range workspaces {
		names = append(names, filepath.Base(ws.RootPath))
	}
	return fmt.Sprintf("%d (%s)", len(workspaces), strings.Join(names, ", "))
}

func flagPassed(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

// loadDotEnv loads KEY=VALUE pairs from path into the process environment without
// overwriting variables already set in the shell.
func loadDotEnv(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key := strings.TrimSpace(k)
		if key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		val := strings.TrimSpace(v)
		if len(val) >= 2 && ((val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'')) {
			val = val[1 : len(val)-1]
		}
		_ = os.Setenv(key, val)
	}
}

// reorderFlagsFirst moves CLI flags appearing after positional query terms ahead of
// positional args so `faro-cli search invoice --ext pdf` parses identically to
// `faro-cli search --ext pdf invoice`.
func reorderFlagsFirst(args []string, boolFlags map[string]bool) []string {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if strings.HasPrefix(arg, "-") && len(arg) > 1 {
			flags = append(flags, arg)
			name := strings.TrimLeft(arg, "-")
			if !strings.Contains(name, "=") && !boolFlags[name] && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		positional = append(positional, arg)
	}
	return append(flags, positional...)
}
