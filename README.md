# faro-harness

Local-first native AI harness for non-technical users, built with **Go** workers (SQLite WAL + FTS5 + BLAKE3 + fast header peeks) and a **React + TypeScript** UI via **Wails v2**.

## Architecture

Instead of eagerly parsing giant PDFs, 300k-row spreadsheets, and `.zip` archives into a heavy vector DB, `faro-harness` treats the local index like a **fast library card catalog**:

1. **Tier 0 (Instant metadata):** `path`, `filename`, `folder`, `ext`, `size`, `mtime`, `inode` stored in SQLite (`WAL` mode).
2. **Fast Bounded Peeks (`internal/peek`):**
   - **Plain text / Markdown / code / JSON / CSV (`≤ 5 MB`):** bounded text excerpt (capped at 64 KB) into FTS5.
   - **`.xlsx`:** reads `xl/workbook.xml` directly from the ZIP container to index sheet names in ~1ms without loading 200k rows.
   - **`.zip`:** reads only the central directory header (zero decompression) to index internal filenames.
   - **Large / binary files:** marked `metadata_only` so the agent knows to inspect them on demand (e.g., via a sandboxed Python/DuckDB script).
3. **Content-Addressed Deduplication (BLAKE3):** `blobs` are keyed by `(content_hash, extractor_version)`. Renames, moves, and duplicate copies skip re-extraction completely.
4. **Live Watching (`internal/watcher`):** Debounced `fsnotify` watcher with sensible noise filters (`~$*` Office lock files, `.git`, `node_modules`, `.DS_Store`).
5. **Wails Desktop + Headless CLI:** All core engine code lives in `internal/`, shared by the Wails desktop app (`main.go` + `frontend/`) and a headless CLI (`cmd/faro-cli`).

## Project Layout

```text
├── main.go                     # Wails v2 desktop entrypoint
├── app.go                      # Go <-> React bridge methods
├── wails.json                  # Wails configuration
├── cmd/faro-cli/               # Headless CLI (index + FTS5 search without GUI)
├── internal/
│   ├── store/                  # SQLite (WAL) + FTS5 (porter unicode61, weighted BM25)
│   ├── peek/                   # Fast bounded header/card peekers (.txt/.md/.zip/.xlsx)
│   ├── watcher/                # Debounced fsnotify workspace watcher
│   └── worker/                 # Crawl + BLAKE3 hashing + bounded goroutine worker pool
└── frontend/                   # React + TypeScript + Vite UI
```

## Quickstart

### 1. Run tests & headless CLI (works anywhere with Go 1.22+)

```bash
go test ./internal/... ./cmd/...
go run ./cmd/faro-cli index ~/Documents/MyFolder
go run ./cmd/faro-cli search "quarterly budget"
```

### 2. Run the React frontend standalone

```bash
cd frontend && npm install && npm run dev
```

### 3. Run the native desktop app with Wails

Install the Wails CLI once:

```bash
go install github.com/wailsapp/wails/v2/cmd/wails@latest
```

> **Linux prerequisites:** Wails requires GTK3 and WebKit2GTK headers (`sudo apt install libgtk-3-dev libwebkit2gtk-4.1-dev` on Ubuntu/Debian; use `-tags webkit2_41` on Ubuntu 24.04+).
> **macOS / Windows:** Works natively with Xcode Command Line Tools / WebView2.

```bash
wails dev
```
