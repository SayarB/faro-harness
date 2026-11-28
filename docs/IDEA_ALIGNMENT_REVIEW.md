# Does `main` follow the idea? A review

Reviewed: `main` at `9adcc9e` (after PR #8, the Wails v3 migration).
Measured against: [`IDEA.md`](IDEA.md) (*Coworking on Files*) plus the bot and permission decisions from the product brainstorm, which are listed below because they are not written down anywhere else yet.

**Short answer:** the bot foundation follows the idea closely. Bots are folders, chats are stored in the database, search keeps the user's files apart from Faro's own, and the bot can't edit its own identity. The safety story does **not** hold yet. The main reason is that bot scripts run unsandboxed, so the "one gate for every write" is a convention, not a boundary. On top of that there's no undo, no permission modes, no live watching, and no way to hand over a single file.

---

## The idea, as decided

1. **A bot is a folder of plain files**: identity, notebook (memory), skills, scripts and schedules. The database indexes them but is not the source of truth.
2. **A bot's home is separate from what it works on.** One global bots directory. The bot's own folder is its turf and scratchpad. Bots can change files anywhere when the user tells them to.
3. **Every bot run starts from a trigger.** A chat message is the first trigger. File events, schedules and other bots come later.
4. **Every write passes through one gate**, which checks permission, keeps the original, logs the change and attributes it to a bot. Scripts work in the scratchpad, and only the gate touches the user's files.
5. **The safety net:** originals are always kept, nothing is truly deleted, one undo per job, a per-bot change log, and plain-language explanations of changes.
6. **Permission modes on every write:** *supervised*, *whitelisted*, *auto approvals*, *full access*. Some actions always ask. A bot can never change its own permissions.
7. **Every memory records where it came from** (a file, a handed-over file, or the user said it).
8. **Transcripts live in the database.**
9. **A conversation is the unit, and bots are participants in it.**
10. **Search tells the user's files apart from Faro's own files.**
11. **Handed-over files:** a file dropped into a chat is readable for that chat only. It is not indexed, and its folder is not watched.
12. **Turf is always current:** watched folders stay up to date without a manual rescan.

---

## Scorecard

| # | Decision | Status on `main` |
|---|---|---|
| 1 | Bot = folder of files | ✅ Follows: `bot.md`, `memory.md`, `scratch/` ([`internal/bot/bot.go`](../internal/bot/bot.go)). Skills and schedules don't exist yet, which is fine for now |
| 2 | Global home, separate from turf | ✅ Follows: app-data `bots/` dir ([`internal/store/paths.go`](../internal/store/paths.go)) |
| 3 | Trigger → bot run | ⚪ Not needed yet: runs are chat-only (`Session.Turn(userText)`), but the CLI already runs bots without a UI |
| 4 | Single write gate | ❌ Broken by unsandboxed scripts (Gap 1) and by audit logs the bot can write to (Gap 3) |
| 5 | Safety net / undo | ❌ Partial: overwrites are backed up, but there is no undo, no grouping into jobs, no change log UI, and bot memory has no history (Gap 2) |
| 6 | Permission modes | ❌ Not built: three CLI flags and "allow for this chat" (Gap 4) |
| 7 | Memory provenance | ⚠️ Asked for in the prompt only, with no format (Gap 6) |
| 8 | Transcripts in DB | ✅ Follows: `messages` table |
| 9 | Conversation with participants | ✅ Follows: `conversation_participants`, `CreateConversation(..., botIDs...)` |
| 10 | Your files vs. Faro's files in search | ✅ Follows: search defaults to `WorkspaceKindUser`, and bot homes are not indexed |
| 11 | Handed-over files | ❌ Missing (Gap 5) |
| 12 | Turf always current | ❌ The desktop app indexes once, with no watcher (Gap 7) |

What `main` already does well and must keep:
- **The bot cannot change its own identity.** `bot_write` refuses `bot.md`.
- **Overwrites are careful.** `deliver_file` backs up the original to app data before overwriting, refuses if the backup would be too large, writes atomically, and aborts if the user edited the file while approval was pending.
- **Approval wording is honest.** It never claims a sandbox that isn't there, and unsandboxed commands can't be approved for a whole session.
- **The approval dialog can't be spoofed.** Control characters in commands and paths are escaped before they're shown.
- **Search is scoped.** It only covers user workspaces unless asked otherwise.

---

## Gaps and fixes

Ordered by importance. Each fix follows `AGENTS.md`: hard boundaries go in code, habits go in prompts, and nothing speculative gets built.

### Gap 1: Scripts get around the write gate (critical)

**What the idea says:** scripts only write inside the scratchpad, and only the gate touches the user's files.

**What `main` does:** `bot_run_command` runs with `noopSandbox{}` ([`internal/tools/tools.go:92`](../internal/tools/tools.go)). The read-only workspaces in its policy ([`bot_tools.go:620`](../internal/tools/bot_tools.go)) and the "never write outside scratch/" instruction in the prompt are conventions. A script can therefore:
- overwrite, move or delete any user file with no backup and no log entry;
- edit `catalog.db` and `settings.json` in the app-data dir, including any future permission store, and read the saved API key;
- reach the network (`Network: true`, [`bot_tools.go:617`](../internal/tools/bot_tools.go)).

Asking for approval on every command keeps this honest today. But it means the whole safety net depends on the user reading every shell command, which a non-technical user cannot do.

**Fix:**
*(Updated design in [`OS_SANDBOX_DESIGN.md`](OS_SANDBOX_DESIGN.md): instead of an interactive `bot_install_packages` prompt or session-approved commands, Faro provisions a curated, hash-pinned Python 3.12 runtime during first-run setup, runs OS-sandboxed network-blocked scripts automatically without prompting, and hard-disables `bot_run_command` in Desktop whenever no OS sandbox is verified active.)*

1. **Ship a real OS sandbox before anything else in this list.** Landlock on Linux, Seatbelt on macOS, AppContainer or a restricted token on Windows. The policy should be: read-only access to workspaces, read-write only to `scratch/` and `.tmp/`, **no access to the app-data dir or to the bot's own `.faro/`**, and no network.
2. **Pre-install a curated runtime instead of opening network in scripts.** Provision a hash-pinned Python 3.12 runtime mounted read-only into `bot_run_command` during first-run setup (via `uv` for now, bundled later), so scripts themselves never have network access and non-technical users are never asked to approve PyPI package names.
3. **Until the sandbox ships on a platform,** hard-disable `bot_run_command` in the desktop app (surfacing the setup/sandbox error with Retry) and keep per-command approval in the CLI stating plainly that unsandboxed commands *can* change files outside Faro's safety net.

### Gap 2: There is no undo, so the safety net is only half built (critical)

**What the idea says:** the original is always kept, nothing is truly deleted, one undo per job, and a per-bot change log you can undo from.

**What `main` does:**
- Backups happen **only on overwrite**, and nothing ever restores them. There's no undo tool, button or command.
- There's no idea of a **job**. Each `deliver_file` call stands alone.
- The change log is a JSONL file ([`deliver.go:705`](../internal/tools/deliver.go)). The UI can't query it and nothing links its entries to backups.
- There are **no move, rename or delete tools.** "Change files anywhere when told" can't happen through the gate today, so bots will reach for unsandboxed scripts instead (Gap 1).
- `memory.md` can be overwritten by `bot_write` with no history ([`bot_tools.go:399`](../internal/tools/bot_tools.go)). The idea says a bot's durable parts keep their history.

**Fix (smallest version that delivers the promise):**
1. **Record changes in the database, not in log files.** Add a changes table: who, which job (the conversation turn is a fine first job id), what operation, path, backup reference, and time. It's the single source for the change log UI and for undo.
2. **Add one undo action for a whole job.** It restores the backups for that turn in reverse order. If the file has changed since Faro's edit, it keeps both versions and says so, as in the *Tensions* section of the idea.
3. **Add `move`, `rename` and `delete` through the same gate.** Delete moves the file into Faro's holding area in app data instead of removing it. These are hard-boundary primitives, so they belong in code.
4. **Save a copy of `memory.md` (and later skills and scripts) before each overwrite**, using the same backup path, so a bad rewrite can be undone.
5. **Write a retention policy that fits in one sentence** (e.g. "originals kept for 90 days"), cleaned up by the existing blob-GC style sweep.

### Gap 3: The audit trail sits where the bot can write (high)

**What the idea says:** a bot must never be able to change its own permissions, and the record of what it did must be trustworthy.

**What `main` does:** `commands.log` and `changes.log` are written to `<bot home>/.faro/` ([`bot_tools.go:871`](../internal/tools/bot_tools.go), [`deliver.go:709`](../internal/tools/deliver.go)). The bot's tools hide dot-paths, but an unsandboxed script can rewrite them. The backup dir falls back to `<bot home>/.faro/backups` when no data dir is known.

**Fix:** keep everything the bot must not touch (the change records from Gap 2, backups, permissions) in **app data or the database, never in the bot's home**. Drop the bot-home fallback for backups. If no private location is available, fail. Once the change table exists, the JSONL logs can go.

### Gap 4: Permission modes don't exist (high)

**What the idea says:** four modes (supervised, whitelisted, auto approvals, full access), set per bot with a per-job override; scheduled runs use the scope that was approved; some actions always ask; whitelist rules are visible and can be revoked; bots can't change their own permissions.

**What `main` does:** `approval.Config` has three booleans that are CLI flags only ([`internal/approval/gate.go:33`](../internal/approval/gate.go)). The desktop app always asks and offers "allow new files in this folder for this chat". Nothing is per bot, nothing is persisted, and there are no rules.

The good news is that the Gate is already the single place where decisions are made, and its hard rules already behave like the "always asks" list: overwrites are never approved for a whole session, and neither are unsandboxed commands. The modes extend this Gate. They don't replace it.

**Fix:**
1. **Persist a mode per bot in the database**, which the bot can't write to once Gap 1 is fixed. Start with **supervised** (today's behaviour) and **full access**. Add **whitelisted** when a real user asks for "always allow this". Add **auto approvals** once undo exists (Gap 2), because that mode is defined by "undoable writes go through".
2. **Write the always-ask floor into the Gate as code:** anything that can't be undone (permanent delete, network, sending), unusually large jobs, and writes outside any added folder. Full access means "don't ask me about normal work", not "do anything".
3. **Make whitelist rules explicit**: folder × operation, shown in the bot's settings, each revocable.
4. **Approve per job, not per file**, once jobs exist (Gap 2).

### Gap 5: A handed-over file can't be read (medium)

**What the idea says:** a file dropped into a chat from somewhere unwatched (`Downloads`) can be read for that chat only. It isn't indexed, and its folder isn't watched.

**What `main` does:** the read tools are confined to workspaces, and the composer has no attach. The only way to read a file in `Downloads` is to add `Downloads` as a workspace, which indexes the whole junk drawer. That's exactly what the idea rules out.

**Fix:** a **per-conversation file grant**. The user attaches a file, and the grant (conversation id + absolute path) is stored in the database. `read_file` accepts it with the same symlink and placeholder checks as workspace files. It isn't indexed and the folder isn't watched. The "want me to file it?" offer afterwards is a prompt habit, not code. Filing it uses the gated move from Gap 2.

### Gap 6: Memory has no provenance format (medium)

**What the idea says:** every memory records where it came from, so a bot can say "as of when you showed it to me" and can be told to forget.

**What `main` does:** the prompt says "noting where each fact came from" ([`internal/agent/prompt.go:32`](../internal/agent/prompt.go)), with no format, so provenance comes out inconsistent or missing.

**Fix (prompt, not code, following `AGENTS.md`):** define a short convention in `BotWorkshopPrompt`. For example, each note ends with a source tag: `(source: Clients/Acme/NDA.pdf, 2026-11-02)`, `(source: shown in chat, 2026-11-02)` or `(source: you told me, 2026-11-02)`. Only move this into code if the convention turns out unreliable in practice.

### Gap 7: Turf isn't kept current in the app (medium)

**What the idea says:** what's on the bot's turf is always current.

**What `main` does:** the desktop app runs a single `worker.IndexFolder` pass when a folder is added or rescanned ([`internal/desktop/controller.go:242`](../internal/desktop/controller.go)). Only the CLI uses the `watcher` package.

**Fix:** start a `watcher` for each user workspace when the app starts and when a folder is added, and stop it on shutdown. The package already exists and is tested. This is also the first non-chat trigger for the Observer stage later.

### Gap 8: The scratchpad mixes durable and disposable files (low, before skills)

**What the idea says:** a bot's folder has durable parts with history (identity, notebook, skills, scripts, schedules) and a disposable scratchpad that gets cleaned up.

**What `main` does:** everything the bot makes, including scripts it might reuse, goes into `scratch/`, and nothing cleans it up.

**Fix (when skills land, not before):** add `skills/` (and the scripts they use) as a durable area that the bot can write to, with history from Gap 2. Treat `scratch/` as disposable: clean it after a job's result is delivered, or after N days. Don't add `skills/` or `schedules/` before a real skill exists.

---

## Not gaps, but watch these

- **Triggers (decision 3).** Don't build a trigger framework now; `AGENTS.md` rules out speculative layers. When schedules arrive, a scheduled run should be "a conversation started by a schedule, under the scope that schedule was approved for". The CLI already shows a bot can run headless, and that's enough for now.
- **Network plus session approval.** Once a sandbox exists, the Gate will let sandboxed commands be approved for the whole chat. If those commands still have network access, one approval would let every later command send data out. Gap 1, step 2 removes this risk. Don't ship the sandbox without it.
- **One chat per bot.** A local branch named `feat/single-chat-per-bot` exists. The idea treats the conversation as the unit with bots as participants (decision 9), so don't drop `conversation_participants` or tie the schema to one bot per chat, even if the UI shows one chat per bot.
- **Bot memory isn't searchable.** Bot homes aren't indexed, which keeps them out of user search (good). When notebooks grow past the 12 KB prompt budget, index bot homes as `WorkspaceKindBot` and search them deliberately, never mixed in with the user's files.

---

## Suggested order

1. **The OS sandbox and moving the network out of scripts (Gap 1).** Everything else relies on scripts not being able to get around the gate.
2. **The change table, holding area, undo per job, and gated move/rename/delete (Gaps 2 and 3).** This delivers the promise in `IDEA.md`.
3. **A persisted per-bot mode with the always-ask floor, starting with supervised and full access (Gap 4).**
4. **The watcher in the desktop app (Gap 7) and handed-over files (Gap 5).** These are what make the product feel alive.
5. **The provenance convention in the prompt (Gap 6).** A cheap change that can ship any time.
6. **Durable `skills/` and scratch cleanup (Gap 8),** together with the first real skill.
