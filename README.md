# faro-harness

Local-first native AI harness for non-technical users, built with **Go** + **React / TypeScript / Vite** via **Wails v2**.

Read the full product vision and roadmap in [docs/IDEA.md](docs/IDEA.md).

## Project Layout

```text
├── main.go                     # Wails v2 desktop entrypoint
├── app.go                      # Go <-> React bridge
├── wails.json                  # Wails configuration
├── Makefile                    # Dev & build targets
├── docs/
│   └── IDEA.md                 # Product vision, principles, and milestones
├── cmd/
│   └── faro-cli/               # Headless CLI entrypoint
├── internal/
│   ├── store/                  # SQLite + FTS5 card catalog
│   ├── peek/                   # Fast bounded file header peekers
│   ├── watcher/                # Workspace filesystem watcher
│   └── worker/                 # Crawl, hashing, and indexing workers
└── frontend/                   # React + TypeScript + Vite UI
    └── src/
        ├── App.tsx
        ├── main.tsx
        ├── styles.css
        └── lib/
            └── bridge.ts       # Wails frontend bridge
```

## Quickstart

```bash
# Run Go tests / build CLI
go test ./internal/... ./cmd/...
go build ./cmd/faro-cli

# Run React frontend standalone
cd frontend && npm install && npm run dev

# Run native desktop app via Wails
wails dev
```
