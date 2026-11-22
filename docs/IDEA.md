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
   - Any file edits or creations go through explicit, user-approved tools with automatic pre-edit backups/undo (see *Coworking on Files* below).

---

## Coworking on Files

Faro's job is to cowork with the user, and that includes changing their files. The aim is not to make Faro timid about editing. It is to make every change **impossible to regret**.

> **The promise:** Faro never destroys anything. Every change it makes can be undone, and you can always see what it did.

### Turf vs. handed-over files

Faro knows about a file in one of two ways:

- **Turf:** files in watched folders. The bot lives there and keeps up with changes. What it knows is always current.
- **Handed-over:** a file the user drops into a chat from somewhere unwatched, such as `Downloads`. The user showed it once. That is not permission to watch the folder.

When a file is handed over:

1. **Answer first.** Faro reads the file where it sits and answers with citations. It asks the user nothing before the answer.
2. **Only that file.** Dropping one file grants access to that file, not to its folder.
3. **Remembered as a snapshot.** The bot notes that it was shown the file and what it learned from it. Later it speaks accordingly ("as of when you showed it to me").
4. **One gentle offer, afterwards.** For example: "This looks like Acme's contract. Want me to file it in Clients/Acme?" If the user declines, the bot remembers and stops asking.

Handed-over files make good moments:

- **Duplicate:** "This is the same file as Clients/Acme/NDA.pdf."
- **Newer version:** "This looks like a newer version of the Acme contract. The payment terms changed from 30 to 45 days."
- **Orphan:** the file fits no bot's turf. That can be a reason to suggest a new bot.

**Faro does not watch `Downloads` by default.** It's a junk drawer, and absorbing it would make the bots' picture of the user's world noisier. But `Downloads` is where new things arrive first (invoices, contracts, statements). Later, it can become a **doorway**: a bot notices new arrivals and offers to file them, without treating the folder as knowledge. Filing things for the user is the kind of help non-technical people want most.

### The safety net

- **The original is always kept.** Before Faro first changes a file, it saves the "before" version somewhere safe. The user never has to think about backups.
- **Nothing is truly deleted.** Faro's deletes, overwrites and renames all go through a holding area and can be reversed.
- **One undo per job.** "Rename and file these 40 receipts" is one action that one tap reverses. People think in tasks, not file operations.
- **Each bot has a change log.** "This week, Ledger filed 12 invoices, renamed 3 receipts, and updated the 2026 expenses sheet." Every entry can be undone. Bots answer for what they've done.
- **Changes are explained in plain language.** "Changed the payment terms from 30 to 45 days on page 2." Show before and after side by side where it helps.
- **Edit in place by default.** The original stays safe in the background, so folders don't fill up with `Contract (Faro edit).docx` copies. Faro creates a new file only when the user is clearly asking for something new.

### How the safety net fits autonomy

Undo is what lets bots earn more freedom. If every change can be reversed, bots can ask for approval less often. As trust grows, the safety net can go from obvious ("Ledger changed 3 files. Review?") to quiet. Actions that **cannot** be undone, such as sending anything or anything that leaves the machine, always need explicit approval.

### Tensions

- **The user keeps editing after Faro.** Undoing Faro's change can't silently erase the user's later work. Undo has to say so ("You've changed this since. I'll restore my Tuesday version and keep your current one too").
- **Cloud-synced folders.** Safe copies stored inside Dropbox or iCloud would sync and clutter other devices. They probably belong in Faro's own storage instead, which means they don't follow the user to other machines.
- **How long originals are kept.** The policy should fit in one sentence the user understands (e.g. "originals kept for 90 days").
- **The safety net itself is fragile.** If the user deletes Faro's storage, the history is gone. Faro should handle that gracefully.
- **Knowing vs. having.** If a handed-over file is deleted, the bot may still remember what it learned. That's acceptable as long as its memory shows where each fact came from and can be told to forget.

### Open questions

- Should the safety net eventually cover the user's **own** edits too ("what did this contract say before I changed it last month?"), making it part of Faro's identity rather than a feature?
- Does what a bot learns from a handed-over file go into its **long-term memory**, or stay in that conversation unless the file is filed into the bot's turf?

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
