# Faro desktop UI: minimal MVP plan

Based on `main @ 4264259`. Written as a plan first; implementation status is tracked below.

## Status

**PR A (first implementation, branch `feat/desktop-chat-ui`): chat shell only. Implemented.**

Included:
- Extensible frontend scaffold exactly as in section 3: `lib/bridge.ts` (sole Wails importer), `lib/events.ts` (event union), `state/chat.ts` (pure reducer, vitest), `state/store.ts` (Zustand), `styles/index.css` (design tokens + Tailwind v4), `ui/` primitives (Radix dialog), `layout/AppShell` (with an empty right-panel slot), `features/*`.
- Chat with **token streaming**, a **tool-call card UI** (running/done/failed, expandable request/result), **create a bot** (name + optional instructions), **attach a folder** via the native dialog with indexing counters, minimal API key entry (0600 file in the data dir; `AI_GATEWAY_API_KEY` env wins).
- Backend: `llm.ChatRequest.OnDelta` SSE streaming (opt-in, existing callers unchanged), `agent.EventToken` + `Event.ToolCallID` (opt-in via `Config.StreamTokens`), `worker.IndexFolder` with progress, `internal/desktop` controller (no Wails import; tested with a fake LLM, `-race`), thin `app.go`.

Deliberately not included yet (each its own PR):
- **Approval dialog.** The GUI installs **no approver**, so `deliver_file` and `bot_run_command` are denied by `internal/tools` (fails closed). The tool card shows these as "didn't work".
- File citations / sources panel, conversation history, file watcher, keychain storage, OS sandbox.
- The shared `internal/approval` extraction and refactoring `cmd/faro-cli` onto the new helpers (the CLI is untouched; it keeps its own approver and indexing code).

Decisions taken against the original recommendations:
- `frontend/wailsjs/` is **not committed**: the repo's `.gitignore` already excludes it. Run `make bindings` (needs the `wails` CLI) before `npm run build`/`tsc` outside of `wails dev|build`.

How it was verified: see the PR description. Real LLM calls were not made (no gateway key in Infisical); the app was driven end to end in `wails dev -browser` against a local fake OpenAI-compatible SSE server. The native folder dialog was not exercised.

## Context

The backend is done and tested (store, worker, watcher, agent, llm, tools, bot), but the only way in is `cmd/faro-cli`. `app.go` is an empty Wails stub and `frontend/src` is ~19 lines. The goal is the smallest desktop UI that makes the existing harness usable by a non-technical person. It has to leave real seams so a professional UI can be layered on without rewrites. AGENTS.md applies throughout: approvals, path confinement and limits stay in Go, everything else stays in prompts, and nothing speculative gets built.

## Findings that change the plan

1. **There is no token streaming today.** `llm.Client` only has `Chat()`, which is one-shot, and `agent.Turn` emits only `tool_call`, `tool_result` and `usage` events per step. "Streaming" in the MVP therefore means live step events (searching, reading, saving) followed by the final reply. Token streaming is a separate backend PR (see PR5). I recommend shipping without it first.
2. **There is no `dev` branch**, only `main` and feature branches. The target branch needs your decision (Q1).
3. **The approver is already a clean seam.** `tools.Approver` is `func(ctx, ApprovalRequest) (ApprovalDecision, error)`, and `ApprovalRequest.Sandboxed` is the honest truth source for the UI. The policy inside `cmd/faro-cli/approver.go` is a hard boundary. It holds session-allow memory and refuses session-allow for unsandboxed commands. It must be shared with the GUI, not re-implemented.
4. **No API key store exists.** The key is env-only, and `loadDotEnv` is a CLI-only hack. A Finder-launched app has no env, so first-run needs a place to put the key. `NewVercelGatewayClient(apiKey, baseURL)` already accepts a key.
5. **Indexing progress needs no new instrumentation.** `CatalogIndexer.Stats` holds atomic counters and `Snapshot()` is cheap to poll. There is no total file count, so the UI shows counters, not a percentage.
6. **`Session` is not concurrency-safe** because it mutates `history` with no lock. Go must enforce one turn at a time.
7. **`sandbox.go` only has `noopSandbox`.** `bot_run_command` is not OS-sandboxed today, and `req.Sandboxed` is false in practice.
8. `wails` CLI is not installed on this machine. `frontend/dist` must exist for the root `package main` to build because of `//go:embed all:frontend/dist`.

## 1. MVP scope (challenged)

| Item | Decision | Notes |
|---|---|---|
| Chat view | **Keep** | Composer, message list, Stop button, "New chat". Live tool-activity rows ("Searched files for 'invoice'", "Read Contracts/Acme.pdf"), then a markdown reply. |
| File citations | **Keep, minimal** | The prompt already makes the model cite paths in its reply. The UI also shows a "Sources" list of paths parsed from `search_files` and `read_file` tool results. **No click-to-open in the MVP**, because it needs a new path-confined `RevealFile` hard boundary (Phase 6). |
| Workspace picker | **Keep** | Native `runtime.OpenDirectoryDialog` and a list from `store.ListWorkspaces`. Plain wording: "Faro can read every file in folders you add." **Cut:** remove-folder (no store method), per-folder settings. |
| Indexing status | **Keep, minimal** | Indeterminate bar plus `indexed / skipped / errors` counters from the polled Stats. Scan on add, scan on app launch, and a "Rescan" button. **Cut:** live watcher (see Phase 6), per-file progress. |
| Approval dialog | **Keep** | Wired through the shared Gate (see §2). |
| Bot selector and creator | **Keep, simplest** | A dropdown ("Faro" plus bots) and a "New bot" dialog with name and instructions only. The slug comes from `NormalizeSlug`, the model stays at the default. **Cut:** edit, delete, memory/scratch viewer, model picker, per-bot history. |
| First-run key and settings | **Keep, minimal** | One screen: paste the AI Gateway key, with a plain-language note about what gets sent (see §4). **Cut:** model picker, base-URL field, theme switch, a settings "page" beyond this. |
| Conversation history sidebar | **Cut** | Persistence already works in SQLite, but bot-less conversations have no list query. The MVP has "New chat" only. |
| Multi-chat tabs, file preview, undo/changelog UI | **Cut** | Roadmap items in `docs/IDEA.md`, not MVP. |

## 2. Go <-> React bridge

### Reuse, don't duplicate (a small refactor PR first)

- **`internal/approval`** (new, extracted from `cmd/faro-cli/approver.go`). Contents:
  - `Gate` holds the policy and session memory: the `allowCommands`/`allowWrites`/`allowOverwrites` flags, `writeApprovedWorkspaces`, `cmdSessionApproved`, and the rule "unsandboxed command → never allow-session".
  - It takes an injected `Ask func(ctx, tools.ApprovalRequest) (Choice, error)`.
  - Two real implementations justify the seam. The CLI's `Ask` is the readLine prompt. The GUI's `Ask` emits an event and waits on a channel.
  - `EscapeControl` (renamed from `sanitizeTerminalCommand`) also moves here, because bidi/control characters in a model-authored command can spoof the dialog in React too.
  - `approver_test.go` moves with it. Terminal formatting stays in the CLI.
- **`worker.IndexFolder(ctx, st, root, onProgress)`**. It takes the logic of `runChat`'s `indexWorkspace`/`addOrCreateFolder` (NewCatalogIndexer → NewPipeline → IndexWorkspace) and adds a 500ms Stats poll. The CLI calls it too.
- **`agent.NewChatSession(...)`**. It takes the logic of `buildSession` (`tools.NewRegistryWithOptions` + `agent.NewSession`) and is used by both the CLI and the GUI.
- **Stays in the CLI:** flag parsing, the REPL, `/commands`, `promptForInitialFolder`, `loadDotEnv`, terminal prompt formatting.

### GUI controller: `internal/desktop` (no Wails import)

It holds all UI state and takes an `emit func(Event)` callback. `app.go` becomes thin shims that add the Wails-only parts (the native dialog and `EventsEmit`). Because it has no Wails import, it can be tested with a fake `llm.Client`.

It owns:
- the store, the bots manager and the settings (key)
- the current session
- the `turn` state: `busy` flag, a `cancel` func, and a `pending map[approvalID]chan decision`

### Bound `App` methods (Wails generates the TS types)

| Method | Purpose |
|---|---|
| `GetState() State` | `{hasKey, workspaces[], bots[], sandboxEnforced, model}`, where `sandboxEnforced` comes from `Registry.SandboxEnforced()`. |
| `SaveAPIKey(key)` | Stores the key (see Q2). |
| `AddWorkspace() *Workspace` | Opens the native dialog, calls `EnsureWorkspace`, and starts indexing in a goroutine. |
| `Rescan(workspaceID)` | Re-runs indexing for one workspace. |
| `CreateBot(name, instructions) Bot` | Calls `bot.Manager.Create`. |
| `NewChat(botSlug) chatID` | Builds a session with all user workspaces. It is rebuilt on workspace add, as the CLI does, and disabled while a turn is running. |
| `Send(text) turnID` | Returns immediately and errors if `busy`. |
| `Cancel()` | Cancels the turn context. |
| `RespondApproval(id, decision)` | Delivers the user's choice to the waiting Gate. |

### One event channel: `faro:event`

The payload is a discriminated union `{type, turnId?, ...}`, so ordering is preserved and the frontend reduces over a single stream. The types are:
- `step.tool_call`
- `step.tool_result` (display payload truncated to ~4KB)
- `usage`
- `turn.done {reply}`
- `turn.error {message}`
- `turn.cancelled`
- `approval.request {id, kind, command, cwd, workspaceName, destRelPath, sizeBytes, existingSizeBytes, preview, sandboxed, botName}`
- `approval.resolved`
- `index.progress {workspaceId, indexed, skipped, errors, done}`

A `token.delta` type is added later in PR5.

### Typed bindings: `frontend/src/lib/bridge.ts`

- It is the only file that imports `wailsjs/*`.
- It exports typed async functions and `onEvent(cb) → unsubscribe`.
- The `FaroEvent` union is hand-written, because Wails does not type event payloads.
- Recommendation: **commit the generated `frontend/wailsjs/`**, so `tsc` and CI work without the `wails` CLI.

### Cancellation

- `Cancel()` cancels the turn ctx. `Chat` honours ctx, and `Turn` already persists a "[Turn interrupted…]" note.
- A pending approval's `Ask` selects on `ctx.Done()` and returns Deny, so cancel and window close never leave a goroutine blocked.
- Index goroutines hang off an app-level ctx cancelled in `shutdown`, and each has its own cancel for Rescan.
- The single-turn invariant is enforced in Go, not just by a disabled button.

## 3. Extensibility seams

### Frontend structure

```
frontend/src/
  lib/bridge.ts          // sole wailsjs importer, typed events
  state/store.ts         // Zustand: settings, workspaces, bots, chat, approval, index slices
  state/events.ts        // PURE reducer: FaroEvent -> state (the one place with logic; vitest)
  ui/                    // primitives: Button, Dialog (Radix), Input, Select
  styles/tokens.css      // design tokens as CSS variables (+ Tailwind v4 @theme), light/dark via data-theme
  features/{chat,workspaces,bots,approval,onboarding}/
  App.tsx                // Sidebar + Main (+ slot for a right panel)
```

### Library recommendations (not a survey)

- **Zustand** for state. It is tiny, has no provider, and `bridge.onEvent` can write to it outside React.
- **Tailwind v4 + `@radix-ui/react-dialog`**. Radix gives the approval dialog a focus trap and accessibility. Esc and click-outside must map to Deny, never Allow.
- **`react-markdown`** with no raw HTML plugin.
- **vitest**, for the reducer only.
- No router. The view is `'chat' | 'onboarding'` in the store.

### Seams to leave (cheap now, costly to retrofit)

1. The pure event reducer plus the single-channel discriminated union.
2. `bridge.ts` as the sole Wails touchpoint.
3. A message model of `Message{id, role, parts[]}` with `parts` of `text | tool | sources | approval`, not flat strings. Rich parts, artifacts and citations later are just new part kinds.
4. Design tokens and a `ui/` primitives folder, so a design pass restyles rather than rewrites.
5. A layout with a right-panel slot (for Sources or a file preview later).
6. `turnId` on every event (enables concurrent chats later).
7. The Go `Gate.Ask` and `desktop.Controller` seams.

### Deliberately NOT building yet

- Router
- i18n
- Plugin system
- Theme switcher UI
- Conversation history
- Multi-chat or multi-window
- Virtualized lists
- Markdown plugins
- File preview
- Undo/changelog UI
- Multi-provider settings
- Auto-update
- Window-state persistence
- TanStack Query
- A generic event bus
- Provider or backend interfaces

## 4. Honest safety UX

All statements below come from Go truth (`req.Sandboxed`, `SandboxEnforced()`), never from UI copy that assumes it.

- **Command approval.**
  - It shows the exact command, escaped via `EscapeControl`, plus the cwd. This is Go-sourced, not model prose.
  - If `!Sandboxed`, there is a red banner: "Runs with your full user permissions and network access. Faro does not restrict this." There is no "Allow for session" button, because Gate also refuses it.
  - A "sandboxed" badge appears only when `Sandboxed` is true. Today it never is, and the UI never claims otherwise.
  - Approving a command means trusting it fully. Workspaces are read-only by convention only (`FARO_WORKSPACES_READONLY`), not enforced when unsandboxed.
- **File create.** It shows the destination in the workspace, size, and a preview. "Allow once" and "Allow new files in this folder this session" are offered.
- **File overwrite.** It names the existing file and size. Say "original is backed up first" only if I confirm `deliver.go` fails closed when the backup fails. I will verify this during PR1.
- **What Faro reads.** On adding a folder: "Faro can read every file in this folder." Reading needs no approval and is confined to workspaces by `resolveWorkspace`.
- **What leaves the machine.** First-run copy states that file excerpts the assistant reads are sent to the AI provider via Vercel AI Gateway to answer. "Local-first" applies to the index, not to prompts.
- **Prompt-injection hygiene.**
  - Reply markdown is rendered with no raw HTML.
  - Links render as plain text.
  - Dialog content comes from the Go `ApprovalRequest`, not from the model's description.

## 5. PR-sized phases and touch list

**Target branch:** see Q1. Never push to `main`.

**PR0: Refactor, no behaviour change (Go only)**
- New: `internal/approval/{gate.go,gate_test.go}`
- Move: `cmd/faro-cli/approver_test.go` content into the new package
- Edit: `cmd/faro-cli/{approver.go,main.go}`, `internal/worker/` (add `IndexFolder`), `internal/agent/` (add `NewChatSession`)
- Check: the existing tests pass unchanged.

**PR1: Desktop controller and bridge (Go only)**
- New: `internal/desktop/{controller.go,events.go,settings.go}` plus tests
- Edit: `app.go`, `main.go`
- Tests: fake `llm.Client` driving `Send` → tool events → `turn.done`; cancel mid-turn; approval allow/deny/cancel; busy rejection; unsandboxed command never gets session-allow.
- Includes `go test -race` on `internal/desktop`.

**PR2: Frontend foundation**
- Edit: `frontend/package.json`
- New: `frontend/wailsjs/*` (generated), `lib/bridge.ts`, `state/{store,events}.ts` (+ vitest), `styles/tokens.css`, `ui/*`, `App.tsx`
- New: `features/onboarding/` (API key) and `features/workspaces/` (picker + index status)
- Check: `npm run build` (tsc + vite) and the reducer tests.

**PR3: Chat and approvals**
- New: `features/chat/{ChatView,Message,ToolRow,Sources,Composer}.tsx`, `features/approval/ApprovalDialog.tsx`
- Includes markdown rendering, Stop, and the honest-safety copy from §4.

**PR4: Bots**
- New: `features/bots/{BotSelect,NewBotDialog}.tsx`
- Wire `CreateBot` and `NewChat(botSlug)`.

**PR5 (separate, optional): Token streaming**
- `llm.ChatRequest.OnDelta func(string)`. When set, `OpenAICompatClient` uses `stream:true` and `stream_options.include_usage`, assembles the same `ChatResponse` including tool-call deltas, and keeps the gateway caching `providerOptions`.
- Fakes ignore the field, so no interface change.
- `agent` emits `EventToken`, the controller maps it to `token.delta`, and the reducer appends.
- Tests use an `httptest` SSE server.
- The main risk is cached-token usage reporting and SSE tool-call assembly. This is why it ships separately.

**Phase 6+ (not now):** file watcher per workspace, click-to-open/reveal (`RevealFile` with path confinement), conversation history list, keychain key storage, an OS sandbox for `bot_run_command`.

## 6. Verification

- Go (every PR): `go test ./internal/... ./cmd/...`, `go build ./cmd/faro-cli`, `go vet ./internal/... ./cmd/...`, and `go test -race ./internal/desktop/...`.
- Frontend (PR2 on): `cd frontend && npm ci && npm run build` (tsc and vite), plus vitest for the reducer.
- Wails build: needs `wails` installed (`go install github.com/wailsapp/wails/v2/cmd/wails@v2.17.1`, then `wails doctor`). Then `wails build`. If it is unavailable, I will say it was skipped rather than imply it passed.
- Manual smoke (PR3 and PR4): `wails dev` with the key loaded via `infisical-run --env=personal`, never printed. Cover a first-run flow, add a folder, ask a question, and cancel mid-turn. Cover deliver_file allow/deny and a `bot_run_command` approval showing the unsandboxed banner.
- Tests use fake LLMs only. No network and no production anything.
- CI: `go vet ./...` at the repo root fails without `frontend/dist`, so CI runs the frontend build first or limits Go checks to `./internal/... ./cmd/...`, as AGENTS.md does today.

## 7. Risks

- **Wails v2 + WebKit/macOS quirks** (the native dialog, event ordering). Mitigation: a single event channel and a manual smoke test.
- **PR0 refactors a 1000-line `runChat`.** Mitigation: extract only the three duplicated blocks, keep CLI behaviour byte-for-byte, and rely on the existing tests.
- **Plaintext key file.** It is mitigated by 0600 permissions and honest copy, and a keychain upgrade is queued.
- **Large tool results** flooding the UI. Mitigation: truncate to ~4KB for display only.
- **Index/scan on launch** can be heavy on big folders. Mitigation: it runs in the background with counters, and Stop is available through app-ctx cancel.
- **Gateway key for non-technical users.** The product premise is "bring your own AI Gateway key", which is not realistic beyond early users.

## 8. Open questions (each with my recommendation)

1. **Target branch.** `dev` doesn't exist on the remote. **Recommend:** create `dev` from `main` and target PRs there. Or tell me to use `main` via PR.
2. **API key storage.** **Recommend:** MVP uses a 0600 file in the data dir, with `AI_GATEWAY_API_KEY` env taking precedence for dev, and a keychain upgrade in Phase 6. Alternative: do the OS keychain now.
3. **Streaming.** **Recommend:** ship PR0–PR4 with step-level live events, then PR5 for token streaming. Alternative: block the MVP on PR5.
4. **Scan on launch plus manual Rescan versus a live watcher.** **Recommend:** scan on launch and Rescan for the MVP, with the watcher as a follow-up.
5. **Component kit.** **Recommend:** Tailwind v4 + Radix Dialog, with no full component library. Say if you want something heavier (e.g. shadcn/ui copy-ins) from the start.
6. **Commit `frontend/wailsjs/`?** **Recommend:** yes.
7. **Distribution.** Is this for you and early testers only, or should the first-run flow already look like end-user onboarding? This affects the API key story.
