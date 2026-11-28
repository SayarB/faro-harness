# OS Sandbox, Managed Python Runtime & First-Run Setup (Gap 1 Design)

Status: **Draft / Design Handover** (no implementation merged yet; `bot_run_command` currently runs with `noopSandbox{}` and `Network: true`).

This document captures the design for **Gap 1** from [`IDEA_ALIGNMENT_REVIEW.md`](IDEA_ALIGNMENT_REVIEW.md) (*Lock down bot scripts*) after product review and two independent architectural/empirical reviews. It supersedes the earlier Gap 1 suggestion of an interactive `bot_install_packages` tool and session-approved sandboxed commands.

---

## 1. Core Product & Approval Model

> **Guiding rule:** Let the OS sandbox enforce hard boundaries, and only ask the user when an action crosses a boundary a non-technical person can actually judge.

1. **Pre-installed packages only (no `bot_install_packages` tool or install prompts):**
   - Asking a non-technical user to approve PyPI package names (`openpyxl`, `python-docx`) or shell one-liners is approval theatre—they cannot evaluate safety, and it trains blind clicking on prompts that do matter.
   - Faro provisions a curated, hash-pinned Python 3.12 runtime once during first-run setup (via `uv` for now; `.app`/installer bundling comes later via `FARO_RUNTIME_DIR`).
2. **OS-sandboxed + network-blocked scripts run automatically (no approval prompt):**
   - Once the OS enforces read-only workspaces, write-only `scratch/` + `.tmp/`, blocked Faro internals/host files, and blocked network/IPC, a script has the same risk class as `read_file`.
   - Enforce this skip-approver rule in **one place** (`Registry.authorizeCommand` in `internal/tools/bot_tools.go`): skip `r.approver` when `r.SandboxEnforced()` is true.
   - Delete `allowCmdSession` from `Registry` and `cmdSessionApproved` from `internal/approval/gate.go`.
   - Drop unused `Policy.Network` and `ApprovalRequest.Network` (sandboxed scripts are always network-blocked).
   - Show sandboxed runs transparently in the chat activity feed (*"Ran an analysis script · Sandboxed · couldn't change your files or go online"*, expandable to view command/output).
3. **Unsandboxed fallback fails closed in Desktop:**
   - **Desktop (`internal/desktop/controller.go`):** `AllowUnsandboxedCommands: false` (default). When `!sandbox.Enforced()`, `bot_run_command` is omitted from `Definitions()` and rejected in `Execute()`. Startup verification retries once, fails cleanly, and surfaces actionable errors with a **Retry** option in the UI.
   - **CLI (`cmd/faro-cli`):** `AllowUnsandboxedCommands: true` — keeps per-command interactive prompts (never session-approved) warning explicitly that unsandboxed commands can modify or delete files *outside Faro's safety net (no backups or undo)* and reach the internet.
4. **Online-app framing:**
   - Faro is an online app (cloud LLM, online runtime bootstrap). Blocking network inside bot scripts is strictly an **anti-exfiltration lock** against prompt injection. UI and docs should say *"no internet access"* for scripts rather than *"offline"*.
5. **`deliver_file` prompts stay unchanged** until Gap 2 (job-level undo) lands.

---

## 2. OS Sandbox Architecture & Startup Self-Test Probe

### Common Contract (`internal/tools/sandbox.go`)
- Export `DefaultSandbox() Sandbox` and `NoopSandbox() Sandbox`. `NewRegistryWithOptions` defaults `opts.Sandbox == nil` to `DefaultSandbox()`.
- `Policy` struct:
  ```go
  type Policy struct {
      ReadOnly  []string // mounted user workspaces + read-only managed runtime
      ReadWrite []string // scratch/ and .tmp/ only
      Deny      []string // Faro internals (overrides ReadOnly; carved out by ReadWrite/runtime)
      ExecPaths []string // resolved runtime/toolchain dirs
  }
  ```
- **Wire `DataDir` everywhere:** pass `DataDir` into `tools.NewRegistryWithOptions` in **both** `internal/desktop/controller.go` and `cmd/faro-cli/main.go`.
- **Shared `protectedFaroPaths()`** (shared between `deliver.go` and `bot_run_command`): always deny `dataDir`, DB dir, `botsDir`, `botHome`, `.faro/`, `bot.md`, `memory.md`, other bots, and default data/bots directories—even if the user mounts `$HOME` as a workspace.
- **Path validation guard in `Prepare`:** canonicalize all paths (`resolvePathBestEffort`), reject NUL/control characters, escape profile literals, and reject any `ReadWrite` path equal to or an ancestor of any `Deny` path or `/`.

### Startup Self-Test Probe (Fail-Closed + Retryable)
Do **not** report `Enforced() == true` just because a sandbox binary or kernel syscall exists. Run a real probe verifying **6 invariants** before enabling sandboxed execution:
1. Write inside an allowed `ReadWrite` carve-out succeeds.
2. Write to a `ReadOnly` workspace fails (`EPERM`/`EROFS`/`EACCES`).
3. Read of a `Deny` file inside a `ReadOnly` parent fails.
4. TCP connect to a local `127.0.0.1` listener started by the probe fails (0 connections accepted).
5. Unix socket connect to a local listener fails (0 connections accepted).
6. Platform-specific network-proxy IPC check fails (on macOS: `trustd` lookup via `/usr/bin/security dump-trust-settings -d`).

- Retry once automatically on transient failure; if it still fails, fail closed (`Enforced() == false`) and return the exact failed invariant/error so Desktop can display it and allow manual **Retry** (re-running the probe rather than caching a permanent failure with `sync.Once`).

### Platform Implementations

#### macOS — Seatbelt (`internal/tools/sandbox_darwin.go`, PR 1)
Empirically verified on macOS 27.2:
1. Start with `(version 1)`, `(deny default)`, `(import "bsd.sb")`.
2. Immediately strip over-broad inherited rules from `bsd.sb` / `system.sb`:
   - `(deny file-write*)` (strips `.CFUserTextEncoding`, `/cores/`)
   - `(deny network*)` (strips `/private/var/run/syslog`; blocks TCP, UDP, DNS, and Unix sockets including `<dataDir>/run/faro.sock`)
   - `(deny mach-lookup (xpc-service-name-prefix ""))` (strips wildcard XPC lookup)
   - `(deny mach-lookup (global-name "com.apple.trustd" "com.apple.trustd.agent"))` *(closes OCSP/CRL network exfiltration via `trustd`; Keychain/`SecurityServer`, pasteboard, Apple Events, and LaunchServices are already denied by `(deny default)`)*
3. Minimal OS/toolchain read roots: `/System`, `/usr`, `/bin`, `/sbin`, `/Library/Frameworks`, `/Library/Apple`, `/Library/Developer/CommandLineTools`, `/opt/homebrew`, `/usr/local`, `/private/etc`, `/private/var/select`, `/private/var/db/timezone` *(narrowed from `/private/var/db`)*, `/dev`, plus resolved `ExecPaths`.
   - Shared `/tmp` and `/private/tmp` stay unmounted/blocked.
   - `/Library/Fonts` stays unmounted (`matplotlib`/`reportlab` use bundled fonts; document this in the tool description).
4. Last-match-wins filesystem rule ordering:
   1. `ReadOnly` user workspaces
   2. `Deny` Faro internals (wins even if `$HOME` is mounted)
   3. Strict `ReadOnly` carve-out for `<dataDir>/runtime`
   4. `ReadWrite` carve-outs for `scratch/` and `.tmp/` only

#### Linux — Landlock + Network Isolation (`internal/tools/sandbox_linux.go`, PR 2)
Plugs into the same `Sandbox` interface + 6-invariant startup probe:
- **Filesystem:** Landlock LSM (best available ABI) granting read-only to OS/toolchain roots, mounted workspaces, and `<dataDir>/runtime`; read-write only to `scratch/` and `.tmp/`; no access to `dataDir`, `.faro/`, `bot.md`, `memory.md`, other bots, or `/tmp`.
  - *Note:* Landlock is allowlist-only (no explicit deny rule inside an allowed parent), so if a user mounts `$HOME` as a workspace while `dataDir` lives under `~/.local/share/faro`, either reject/split overlapping mounts or use a mount namespace (`bwrap` / unprivileged user+mount ns) to mask `dataDir` and bot internals with tmpfs/empty binds. Verify this behavior explicitly on Linux!
- **Network & IPC:** Network namespace (`CLONE_NEWNET` via unprivileged user namespace / `bwrap`) or seccomp-bpf blocking `socket(AF_INET, ...)`, `socket(AF_INET6, ...)`, and `socket(AF_UNIX, ...)` + `PR_SET_NO_NEW_PRIVS`.
- **Process cleanup:** `PR_SET_PDEATHSIG` / PID namespace cleanly closes the `setsid()` orphan-process gap on Linux.
- Verify on Linux with the same adversarial test suite + GitHub Actions Linux CI.

#### Windows — AppContainer + Job Object (`internal/tools/sandbox_windows.go`, PR 3)
Plugs into the same `Sandbox` interface + 6-invariant startup probe:
- **Isolation:** AppContainer (or restricted token) with zero network capability SID, explicit ACLs only on RO workspaces + runtime and RW `scratch/`/`.tmp/`, and no access to `%APPDATA%\Faro`.
- **Process & resource cleanup:** Windows Job Object (`JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`, plus memory/process caps).

---

## 3. Curated, Hash-Pinned Managed Python 3.12 Runtime

- **Files:** `internal/tools/runtime.go`, embedded `internal/tools/runtime_requirements.txt`.
- **Curated package set (proposed 11 packages — see Open Questions §7):**
  - *Spreadsheets, CSV & SQL:* `pandas`, `duckdb`, `openpyxl`, `xlsxwriter`
  - *PDFs:* `pypdf`, `pdfplumber`, `reportlab`
  - *Word & PowerPoint:* `python-docx`, `python-pptx`
  - *Charts & Images:* `matplotlib`, `pillow`
- **Full transitive, hash-pinned lockfile:** generated with `uv pip compile --universal --generate-hashes --python-version 3.12`.
- **Self-contained interpreter + venv under `<dataDir>/runtime/`:**
  - `EnsureManagedRuntime(ctx, dataDir)` sets `UV_PYTHON_INSTALL_DIR=<dataDir>/runtime/python` (`--python 3.12 --managed-python`), builds the venv in a staging directory, installs with `--only-binary :all: --require-hashes`, writes a SHA-256 stamp covering `(lockfile + pythonVersion + GOOS + GOARCH)`, and swaps the whole venv into `<dataDir>/runtime/venv` via atomic rename.
  - Because `uv venv` symlinks `bin/python` to its base interpreter, keeping the base CPython inside `<dataDir>/runtime/python` ensures the sandbox can mount `<dataDir>/runtime` read-only without needing host `~/.pyenv` or `~/.local/share/uv`.
  - Resolution order:
    1. `FARO_RUNTIME_DIR` env override (seam for future bundled `.app` / installer builds).
    2. `<dataDir>/runtime/venv` when stamp matches and python binary exists.
    3. CLI-only host Python fallback (≥ 3.10 on `PATH`, resolving real `sys.executable` + `sys.base_prefix` into `ExecPaths`, avoiding macOS's `/usr/bin/python3` xcrun 3.9.6 shim).
  - Exposed via `faro-cli setup-runtime` and invoked during Desktop first-run setup (never during unit tests).
- **Script environment hygiene (`bot_run_command`):**
  - Point `HOME`, `XDG_CACHE_HOME`, `XDG_CONFIG_HOME`, `MPLCONFIGDIR`, and `UV_CACHE_DIR` under `.tmp/`.
  - Set `UV_OFFLINE=1`, `UV_PYTHON_DOWNLOADS=never`, `VIRTUAL_ENV=<venv>`, and prepend `<venv>/bin` to `PATH`.
  - Strip `HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY`, `SSL_CERT_FILE`, `SSL_CERT_DIR`, `REQUESTS_CA_BUNDLE`.

---

## 4. Resource Limits & Symlink Hardening

- **Wall-clock timeout:** 60s default, 180s max + process-group kill (`Setpgid: true` + `cleanupProcGroup`).
  - Drop `ulimit -t` (CPU time sums across multi-core threads in DuckDB/numpy and hits 60s in ~6–10s wall time).
  - Skip macOS `ulimit -u` (per-UID across all user apps) and `ulimit -v` (breaks Python/DuckDB mmap).
- **Single-file write cap:** `ulimit -f 204800` (100 MiB in 512-byte POSIX blocks in `internal/tools/proc_unix.go`).
- **Live 512 MiB disk quota across `scratch/` + `.tmp/`:** pre-flight check before launch + 500 ms background poller that cancels the command if exceeded, using `Lstat` / `os.DirEntry` so planted symlinks are never followed.
- **Output cap:** keep existing O(1)-memory `headTailBuffer` (no extra total-byte counter needed).
- **Symlink hardening:** scripts inside the sandbox can still create symlinks inside `scratch/` pointing outside; host tools already use `Lstat`, `O_NOFOLLOW`, and `verifyRealPathWithinRoot`. Add explicit planted-symlink regression tests for `bot_read`, `deliver_file`, `bot_delete`, `snapshotScratch`, and the quota walker.
- **Known limitation on macOS (`setsid()`):** a script calling `setsid()` can leave its process group and survive past timeout (still confined inside Seatbelt; pre-flight quota catches it on subsequent runs). Do not claim airtight background-process termination in UI/prompts on macOS unless mitigated.

---

## 5. Desktop First-Run "Setting Up" Page, Honest UI & Prompts

- **First-run setup lifecycle:**
  - Runs once when the app opens (or after a lockfile/runtime update). Subsequent launches where the runtime stamp already matches and the sandbox probe passes (~100–200 ms) skip directly to Onboarding/Chat.
  - **Backend (`internal/desktop/controller.go`, `events.go`, `app.go`):**
    - Add `SetupStatus` to `desktop.State` (`phase`: `"pending" | "running" | "ready" | "error"`, `step`, `sandboxReady`, `sandboxError`, `runtimeReady`, `runtimeError`).
    - Emit `setup.status` events as steps progress; expose `RetrySetup()` on `App`/`Controller`.
    - Trigger `StartSetup()` from `App.ServiceStartup` (auto-retrying transient sandbox probe / `uv` download errors once before surfacing an error).
  - **Frontend (`frontend/src/App.tsx`, `state/store.ts`, new `features/onboarding/SetupScreen.tsx`):**
    - Add `"setup"` view alongside `"loading" | "onboarding" | "chat"`.
    - Display calm step progress (*"Verifying script sandbox…"* → *"Setting up Python & analysis packages…"*).
    - On failure, display the exact actionable error (e.g. `uv` missing, download error, failed sandbox invariant, or unsupported OS) with a **Retry** button.
- **Honest UI & Prompts:**
  - `ApprovalDialog.tsx`: remove `"Allow commands for this chat"` and the sandboxed command branch; update unsandboxed warning to explicitly state it can modify/delete files *outside Faro's safety net (no backups or undo)* and reach the internet.
  - `toolLabels.ts` & `ToolCallCard.tsx`: show `"Running an analysis script"` / `"Ran an analysis script"` with an honest *"Sandboxed · couldn't change your files or go online"* note driven by `botRunCommandOut.Sandboxed`.
  - `defaultbot.go`, `prompt.go`, and dynamic `botToolDefinitions()`: remove stale *"Commands you run are not sandboxed"* and `uv run --with` instructions; dynamically reflect actual sandbox + runtime package state (including bundled-fonts note).

---

## 6. Verification Plan (When Implementing)

- **Adversarial sandbox tests (`sandbox_darwin_test.go`, and `sandbox_linux_test.go` for Linux):**
  - Workspace RO succeeds; workspace write, hardlink, and rename into `scratch/` blocked.
  - `dataDir`, `settings.json`, `catalog.db`, `.faro/`, `bot.md`, `memory.md`, and other bots blocked even when parent `$HOME` is mounted as a workspace.
  - `faro.sock` Unix socket blocked; TCP (`127.0.0.1`), UDP, and DNS (`getaddrinfo`) blocked.
  - macOS-specific: `trustd` lookup blocked; `pbpaste`, `security`, `osascript` blocked; `/tmp` blocked.
  - Profile literal injection blocked; `ReadWrite` ancestor of `Deny` rejected.
  - `ulimit -f` 100 MiB enforcement test; 512 MiB disk quota pre-flight & live watchdog tests.
  - Startup probe self-test (including fail-closed behavior and retry).
  - Planted-symlink regression tests across `bot_read`, `deliver_file`, `bot_delete`, `snapshotScratch`, and quota walker.
- **Runtime, gate, desktop, CLI & frontend tests:**
  - `runtime_test.go` (stamp validation, `FARO_RUNTIME_DIR`, atomic swap, host fallback).
  - Updated `internal/approval`, `internal/tools`, `internal/desktop`, `cmd/faro-cli`, and Vitest frontend tests (`go test -race ./internal/... ./cmd/...`, `go build ./cmd/faro-cli`, `npm test` / `npm run build` in `frontend/`).

---

## 7. Open Design Questions to Finalize Before Implementation

1. **Package list (11 packages):** Keep `pandas`, `duckdb`, `openpyxl`, `xlsxwriter`, `pypdf`, `pdfplumber`, `reportlab`, `python-docx`, `python-pptx`, `matplotlib`, `pillow` as-is, or add/drop any (e.g. `beautifulsoup4`/`lxml`, `odfpy`, `pymupdf`)?
2. **`setsid()` process-group escape on macOS:** Keep the minimal PR 1 approach (honest wording + pre-flight 512 MiB quota, since Seatbelt still traps any orphan) or invest in stronger macOS process tracking? *(Linux PID namespace / `PR_SET_PDEATHSIG` and Windows Job Objects solve this natively.)*
3. **Platform implementation order:** Build **Linux** first (now that a Linux machine is available for live testing, and Landlock + netns/bwrap is fast to verify), **macOS** first, or both behind the shared `Sandbox` interface + probe?
4. **Setup screen failure UX:** If setup fails (e.g. `uv` missing or sandbox unavailable), should the Setup screen:
   - *(a)* **Strict gate** — stay on Setup with the error + **Retry** until both pass, or
   - *(b)* **Degraded escape hatch** — show the error + **Retry** plus a secondary *"Continue without analysis scripts"* button that lets the user chat/search/read files while keeping `bot_run_command` hard-disabled (with a subtle banner linking back to Retry)?
