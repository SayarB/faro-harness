# Faro Harness — File Indexer Design

This document locks the Milestone 1 architecture for Faro's local file indexer ("Card Catalog, Not Photocopy") across `internal/fsutil`, `internal/store`, `internal/peek`, `internal/worker`, and `internal/watcher`.

---

## 1. Storage & Database Location

- **Driver:** `modernc.org/sqlite` (pure Go, CGo-free, FTS5 enabled out of the box).
- **Mode:** SQLite WAL (`journal_mode=WAL`, `synchronous=NORMAL`, `foreign_keys=ON`, `busy_timeout=5000`).
- **Location:** Single global SQLite database in the OS app-data directory so Faro never drops dotfiles into user folders:
  - **Linux:** `$XDG_DATA_HOME/faro/catalog.db` (default `~/.local/share/faro/catalog.db`)
  - **macOS:** `~/Library/Application Support/faro/catalog.db`
  - **Windows:** `%LOCALAPPDATA%\faro\catalog.db`
  - **Override:** `FARO_DB_PATH` environment variable or CLI `--db <path>` flag.

---

## 2. Indexing Pipeline & `index_depth` Tiers

Every file flows through 5 stages:

1. **Filter & `Lstat`:** Skip ignored filenames/directories and symlinks. Check OS cloud-placeholder flags *before* opening the file.
2. **Fast-path skip:** Compare `(size, mtime_ns)` and `peeker_version` against the `files` table. If unchanged, skip hashing and peeking.
3. **Classify + Bounded BLAKE3 card fingerprint:**
   - Cloud placeholders and known binary/media formats record `metadata_only` with `blake3 = NULL` and zero content I/O.
   - Peekable files are fingerprinted with BLAKE3 (`github.com/zeebo/blake3`) over **only the bounded bytes the peeker consumes** (never streaming entire multi-hundred-MB files):
     - **Plain text / Markdown:** `blake3("head:v1\0" || truncated || first 256 KB or 64 KB)`
     - **CSV / TSV:** `blake3("head:v1\0" || truncated || first 64 KB)`
     - **ZIP / `.xlsx` / `.pptx`:** `blake3("zipcd:v1\0" || raw ZIP central directory bytes)` (zero decompression, capped at `16 MB` central directory; includes every member's name, size, timestamp, and CRC32).
   - Appending bytes past `64 KB` on a large `.log`/`.csv` keeps the exact same head fingerprint and hits the blob cache with zero re-parsing. Copies and renames still dedup instantly.
4. **Content-addressed blob lookup or Peek:**
   - Look up `(blake3, peeker_id, peeker_version)` in `content_blobs`. On hit, reuse the extracted card with zero re-parsing (copies and renames are free, while identical bytes under different peekers such as `.csv` vs `.txt` or `.xlsx` vs `.zip` keep independent cards).
   - On miss, run the matching peeker under a per-file timeout (`500 ms` default) and strict byte caps.
   - Re-`Lstat` after reading; if `(size, mtime_ns)` shifted mid-read, retry with backoff so partial writes are never committed.
5. **Batched atomic commit:** Single SQLite writer goroutine commits `content_blobs`, `files`, and `files_fts` in transactions (flushed every 50 items or 150 ms).

### Depth & Status Semantics

- `index_depth`:
  - `full`: Entire text content fit within upfront bounds.
  - `header_only`: Bounded card extracted (headings, sheet names, slide titles, ZIP entries, or truncated preview). Signals the agent to drill down on demand instead of assuming full coverage.
  - `metadata_only`: Path/filename/folder/ext/size/mtime only (placeholders, binaries, unreadable/corrupt files, or deferred formats).
- `index_status`: `ok`, `placeholder`, `unreadable`, `too_large`, `error`.

---

## 3. Milestone 1 Peekers & Bounds

Routing uses file extension verified by magic-byte / UTF-8 sanity checks (only *after* confirming the file is local, never on a placeholder).

| Format | Extraction Strategy | Bounds | Resulting `index_depth` |
|---|---|---|---|
| **Plain text / `.txt` / `.log` / `.json` / `.yaml` / code** | Read prefix; NUL-byte check rejects misnamed binaries. | Full if `≤ 256 KB`; else first `64 KB` | `full` or `header_only` |
| **Markdown (`.md`, `.markdown`)** | Extract `#` headings into `headings` & `outline_json`; bounded body text. | Full if `≤ 256 KB`; else first `64 KB` | `full` or `header_only` |
| **CSV (`.csv`, `.tsv`)** | Parse header row into `headings` & `outline_json`; capture sample rows into `body`. | Read up to `64 KB` / first 25 rows | `full` (small complete file) or `header_only` |
| **Excel (`.xlsx`)** | `archive/zip` random access: `docProps/core.xml` title, `xl/workbook.xml` sheet names into `headings` & `outline_json`, bounded peek of `xl/sharedStrings.xml` + first sheet rows into `body`. | `2 MB` uncompressed XML cap per part, `32 KB` body | `header_only` |
| **PowerPoint (`.pptx`)** | `archive/zip` random access: `docProps/core.xml` title, slides in presentation order (`ppt/presentation.xml` + rels), slide titles into `headings` & `outline_json`, slide text into `body`. | First 30 slides, `2 MB` XML cap per part, `64 KB` body | `full` (all slides fit) or `header_only` |
| **ZIP (`.zip`)** | Central directory only via `archive/zip` (zero decompression). Entry paths into `body` & `outline_json`. | Up to 200 entries | `header_only` |
| **Deferred / Binary** (`.docx`, `.pdf`, media, executables, unknown) | Stat only; zero content read. (`.docx` & `.pdf` peekers planned next.) | `0 B` | `metadata_only` |

---

## 4. SQLite + FTS5 Schema & Ranking

```sql
CREATE TABLE workspaces (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    root_path       TEXT NOT NULL UNIQUE,
    added_at        INTEGER NOT NULL,
    last_scan_at    INTEGER NOT NULL DEFAULT 0,
    current_scan_id INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE content_blobs (
    blake3         TEXT NOT NULL,
    peeker_id      TEXT NOT NULL,
    peeker_version INTEGER NOT NULL,
    index_depth    TEXT NOT NULL,
    title          TEXT NOT NULL DEFAULT '',
    headings       TEXT NOT NULL DEFAULT '',
    body_preview   TEXT NOT NULL DEFAULT '',
    outline_json   TEXT NOT NULL DEFAULT '',
    unit_count     INTEGER NOT NULL DEFAULT 0,
    extracted_at   INTEGER NOT NULL,
    PRIMARY KEY (blake3, peeker_id, peeker_version)
);

CREATE TABLE files (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    workspace_id      INTEGER NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    rel_path          TEXT NOT NULL,
    filename          TEXT NOT NULL,
    folder            TEXT NOT NULL,
    ext               TEXT NOT NULL,
    size              INTEGER NOT NULL,
    mtime_ns          INTEGER NOT NULL,
    blake3            TEXT,
    peeker_id         TEXT NOT NULL DEFAULT '',
    peeker_version    INTEGER NOT NULL DEFAULT 0,
    index_depth       TEXT NOT NULL,
    index_status      TEXT NOT NULL,
    error_msg         TEXT NOT NULL DEFAULT '',
    indexed_at        INTEGER NOT NULL,
    last_seen_scan_id INTEGER NOT NULL DEFAULT 0,
    UNIQUE(workspace_id, rel_path),
    FOREIGN KEY (blake3, peeker_id, peeker_version)
        REFERENCES content_blobs(blake3, peeker_id, peeker_version)
);

CREATE VIRTUAL TABLE files_fts USING fts5(
    filename,
    headings,
    folder,
    body,
    tokenize='porter unicode61 remove_diacritics 2',
    prefix='3'
);

CREATE VIRTUAL TABLE files_trigram USING fts5(
    filename,
    folder,
    tokenize='trigram'
);
```

- **FTS5 synchronization:** Both `files_fts` and `files_trigram` are keyed directly by `rowid = files.id` (O(log N) deletes/updates instead of full-table virtual scans) and managed explicitly inside Go `store` transactions. Schema migrations are tracked atomically via `PRAGMA user_version` (currently `v4`).
- **Identifier & separator normalization:** When indexing `filename` and `folder` into `files_fts`, `normalizeForFTS` splits separators (`_`, `-`, `.`, `/`, `\`), camelCase (`ClientInvoice`), acronyms (`HTTPServer`, preserving plural acronyms like `PDFs`/`IDs`/`URLs`), and letter/digit boundaries (`March2026`, `2026Report`, `FY2026`) while preserving compact codes (`Q4`, `v2`, `FY26`) and Unicode combining marks (e.g. Indic scripts). Raw `filename` and `folder` are also indexed into `files_trigram` for substring and CJK/non-space-language matching (`≥ 3` runes; note 2-character CJK terms are below SQLite's 3-rune trigram minimum).
- **Forgiving agent-oriented search (`SearchWithOptions`):**
  - Default **OR** semantics with English stopword stripping (falling back to original terms if all terms are stopwords), prefix `*` wildcards gated to `≥ 3`-rune tokens (`prefix='3'`), dual compound-token matching (`("ClientInvoice"* OR ("Client"* AND "Invoice"*))`) so camelCase/digit terms hit both raw body text and normalized filenames under `MatchAll`, plus opt-in `MatchAll` (strict AND) and `ExactPhrase`.
  - Weighted BM25 ranking: `bm25(files_fts, 10.0, 5.0, 3.0, 1.0)` (`filename > headings > folder > body`) paired with `snippet(files_fts, -1, '[', ']', '…', 16)`, merged with `files_trigram` (`bm25(files_trigram, 10.0, 3.0)`) and tagged with `MatchSource` (`fts`, `trigram`, `fts+trigram`, or `filter`).
  - Structured metadata filters (`Ext`, `Folder` prefix, `MtimeAfter`/`MtimeBefore`, `IndexDepth`), recency-sorted filter-only queries, and `Truncated` flag (`Limit + 1` fetch).
- **Orphan blob GC:** `GCOrphanBlobs(olderThan)` removes `content_blobs` unreferenced by any `files` row after a grace period so rename events still hit the blob cache.

---

## 5. Filesystem Hazards & Concurrency

- **Recursive watching + generation-based crawl:** `watcher` walks the workspace on startup (skipping ignored directories), registers `fsnotify` watches on all directories, dynamically watches newly created subdirectories, stamps seen files (and concurrent live upserts) with `last_seen_scan_id`, and prunes stale rows (`last_seen_scan_id < scan_id`) while refusing to prune missing roots or errored subtrees.
- **Directory renames & deletes:** `Remove`/`Rename` events dispatch `DeletePath`, which removes both an exact file match and any nested `dir/%` prefix so renamed or trashed directories never leave ghost rows.
- **Debouncing & coalescing:** `fsnotify` `Create | Write | Rename | Remove` events pass through a per-path debouncer (`300 ms` default quiet window for upserts; immediate stat check for deletes/renames). In-flight paths track a `dirty` map so concurrent workers never race on the same path.
- **Cloud placeholders & Windows reparse points:** Platform-specific checks (`SF_DATALESS` / `.icloud` on macOS, `FILE_ATTRIBUTE_OFFLINE | RECALL_ON_DATA_ACCESS | RECALL_ON_OPEN` on Windows, with `fsutil.IsIndexableFile` allowing Windows cloud reparse points through instead of dropping `ModeIrregular`) run before opening any file.
- **Bounded concurrency:**
  - Mutex + `sync.Cond` queue coordinator with a high-priority **Live** queue (never blocks workers on retries/dirty re-runs) and a bounded **Backfill** queue (natural walk backpressure), plus clean cancel-aware shutdown.
  - Fixed worker pool: `min(4, max(2, NumCPU/2))`.
  - Single batched SQLite writer (`SetMaxOpenConns(1)` for writes, separate `query_only` read pool for searches) with per-item fallback on batch errors.
