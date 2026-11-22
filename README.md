# faro-harness

Local-first native AI harness for non-technical users, built with **Go** + **React / TypeScript / Vite** via **Wails v3**.

Read the full product vision and roadmap in [docs/IDEA.md](docs/IDEA.md).

## Project Layout

```text
├── main.go                     # Wails v3 desktop entrypoint (app + window setup)
├── app.go                      # Wails service: the Go <-> React bridge
├── Taskfile.yml, build/        # Wails v3 build config (config.yml holds app name/version/identifier)
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
            └── bridge.ts       # Wails frontend bridge (only importer of frontend/bindings)
```

## Quickstart

```bash
# Run Go tests / build CLI
go test ./internal/... ./cmd/...
go build ./cmd/faro-cli

# Run React frontend standalone
cd frontend && npm install && npm run dev

# Run native desktop app via Wails v3 (one-time: install the CLI)
go install github.com/wailsapp/wails/v3/cmd/wails3@v3.1.0
wails3 dev      # hot-reload dev mode
wails3 build    # production binary at bin/faro-harness
```

`frontend/bindings/` (TypeScript bindings for `app.go`) is generated and gitignored. `wails3 dev|build`
regenerate it; run `make bindings` to generate it for a bare `npm run build` / `tsc`.

### Tray and updater

Neither is built yet; both are small additions to `main.go`. Wails v3 ships them: a tray is
`app.SystemTray.New()`, and the updater is `app.Updater.Init(...)` before `app.Run()`. For a tray that
keeps indexing with no window open, also set `ApplicationShouldTerminateAfterLastWindowClosed` to false
and hide the window on close. The updater needs a signed-release feed and a version string; the
version lives in `build/config.yml`.
