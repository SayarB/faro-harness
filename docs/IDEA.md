# Faro Harness — Idea & Architecture

## Vision

**Faro** is a local-first native AI harness—"Cursor for non-technical people."

Instead of forcing users to upload documents into a web chat or manage databases and Docker containers, Faro runs as a lightweight native desktop app pointed directly at the folders people already live in (`Documents`, `Desktop`, client folders, receipts, contracts, spreadsheets, notes).

*(Note: Faro is a clean-slate desktop rewrite built from lessons learned in an earlier web chat prototype. It is intentionally decoupled from any prior web/Docker/server stack.)*

---

## Core Design Principles

1. **Card Catalog, Not Photocopy**
   Most files on a person's laptop are never asked about. Never waste CPU/battery eagerly parsing 50 MB spreadsheets, 500-page scanned PDFs, or gigabyte `.zip` archives into a heavy vector store.
   - **Upfront (milliseconds per file):** Index Tier 0 metadata (`path`, `filename`, `folder`, `ext`, `size`, `mtime`) plus **fast bounded header peeks** into SQLite FTS5 so files are discoverable even when filenames are terrible (`Document (4).docx`, `Export_final_v2.xlsx`).
   - **On demand:** When a user actually asks about a big spreadsheet, huge PDF, or archive, let the agent inspect it dynamically via a sandboxed local runtime (Python + DuckDB).

2. **Explicit `index_depth` Awareness**
   Every file tracks how deeply it was indexed (`full`, `header_only`, or `metadata_only`). The agent always knows whether an FTS5 search saw the full document or only its card (e.g., Excel sheet names or ZIP file list), so it knows when to drill down with a script instead of guessing.

3. **Outline & Snippet First (Never Raw Context Dumps)**
   Borrowing the best lessons from modern coding harnesses and hierarchical document trees:
   - Return weighted BM25 hits (`filename > headings > folder > body`) with compact snippets before loading full documents.
   - Inspect document outlines (sections, pages, sheets, slides) and read paginated slices rather than dumping entire files into the prompt.

4. **Real-World Filesystem Hygiene**
   Non-technical workspaces have hazards coding repos don't:
   - **Cloud placeholders (iCloud / OneDrive / Dropbox Smart Sync):** Detect dataless/offline stubs before opening files so indexing never silently triggers gigabytes of downloads.
   - **Duplicates & renames:** Content-address extracted blobs with **BLAKE3** so copying or moving `Contract.pdf` across 5 folders costs zero re-extraction.
   - **Temp/lock noise:** Ignore Office lock files (`~$*.xlsx`), `.DS_Store`, `Thumbs.db`, `.Trash`, and developer trees (`.git`, `node_modules`).
   - **Calm permissions:** Use native OS folder pickers (workspace-scoped grants) instead of asking for invasive Full Disk Access.

5. **Safe by Default**
   - Derived indexes and cached Markdown views are strictly **read-only**.
   - On-demand inspection scripts run in a **read-only sandbox** scoped to the chosen workspace with **no network access**.
   - Any file edits or creations go through explicit, user-approved tools with automatic pre-edit backups/undo.

---

## Stack

- **Desktop shell & workers:** **Wails v2** (Go backend + native OS WebView)
- **UI:** **React + TypeScript + Vite**
- **Storage & Search:** Embedded **SQLite (WAL mode)** + **FTS5** (`porter unicode61`, weighted BM25, `snippet()`)
- **Deduplication & Watching:** **BLAKE3** streaming hashes + debounced **`fsnotify`**
- **On-Demand Heavy Inspection (Planned):** Bundled, sandboxed **Python + DuckDB** runtime for large `.xlsx`/`.csv`, big PDFs, `.zip` archives, and exotic formats

---

## Planned Milestones

### Phase 1 — Solid Local Card Catalog (Current Foundation)
- [x] SQLite WAL + FTS5 store, BLAKE3 deduplicated blobs, debounced watcher, worker pool
- [x] Fast bounded peekers for plain text/Markdown, `.zip` central directory, and `.xlsx` sheet names
- [ ] Add `.docx` / `.pptx` fast XML peeks and `.pdf` metadata + TOC + first-pages peek
- [ ] Add macOS/Windows cloud-placeholder detection (`SF_DATALESS` / `FILE_ATTRIBUTE_OFFLINE`)
- [ ] Add `cache gc` for orphaned blobs and persisted workspace list

### Phase 2 — Agent Harness & Chat UI
- [ ] Chat interface + provider abstraction in Go (streaming tool calls & citations to local files)
- [ ] Core read-only agent tools: `search_files`, `list_folder`, `read_document` (paginated), `inspect_outline`
- [ ] Lazy "cache on read" Markdown mirror for extracted binary docs

### Phase 3 — On-Demand Sandboxed Inspection
- [ ] Bundled read-only Python + DuckDB tool (`run_inspection`) scoped to workspace, no network
- [ ] Lazy `.parquet` caching for repeated SQL queries on large `.xlsx` / `.csv` files

### Phase 4 — Memory, Editing & Polish
- [ ] Local durable memory in SQLite (user preferences, recurring rules, folder notes)
- [ ] Safe file creation/editing tools with shadow backup & one-click undo
- [ ] Native packaging & signing (macOS, Windows, Linux)
