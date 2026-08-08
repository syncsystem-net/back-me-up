# SyncSystem BackMeUp
Agent instructions for building a backup software.

## Development Setup
### Prerequisites
#### Required:
- Go 1.21 or higher (installation guide)
- Windows 10 (cross compatible with macOS)
- Air for live reload during development
Note: Consult me for anything else needed.

## Stack:
- Go (Golang)
- SQLite
- Application Configuration Management: yml file
- Accounts Configuration Management: Environment variables + encrypted database storage
- Frontend Framework: plain HTML + Alpine.js
- Logging Strategy: slog
- Job Queue / Async Processing: Web deployment: Add a jobs table to SQLite for persistence
- Authentication: No need for this now. Let's leave this for a future enhancement.
- File Chunking Strategy: Upload in 100MB chunks (however, I'd like a setting in the Application Configuration Management to change, if I need)
- Encryption Before Upload: No need - nothing secret in the backups
- Retry Logic & Error Handling
    retry_policy:
        max_attempts: 3
        initial_backoff_seconds: 2
        max_backoff_seconds: 60
        backoff_multiplier: 2  # Exponential backoff
- Rate Limiting
    rate_limits:
        mega:
            requests_per_second: 10
            bandwidth_mb_per_second: 5
        4shared:
            requests_per_second: 5
            bandwidth_mb_per_second: 3
- Concurrency Controls
    concurrency:
        max_concurrent_uploads: 2      # Total uploads across all accounts
        max_concurrent_per_account: 1  # Avoid overwhelming single account
        max_workers: 5                  # Goroutine pool size

- Storage Quota Tracking:
    quota_sync_interval_minutes: 60
    sync_method: api_polling  # Poll cloud APIs for current usage
    cache_quota_in_db: true   # Store in SQLite for offline access
    quota_sync_interval_minutes: 60  # Check quotas hourly

- Metadata Database Backup: I'll want an automatic backup to a "main" account (online)
- Verification/Integrity Checks: 
    verification:
        enabled: true
        verify_on_upload: true       # Download first chunk, compare checksum
        periodic_check_days: 30      # Re-verify random files monthly

## Code Style
- Use LF line endings.


## Specs
- The goal is to upload zipped files to the accounts with the help of the application we're building.
- We'll have an .env file that we'll define the the main account (in which the db will be uploaded at every end of a successful job) and all the other accounts.
- We'll build integration for MEGA and 4shared. However, I'd like for it to be built in a way that we could add other cloud storage as we need, with focused refactor.
- The user will define the target directory to backup. The application will scan its subdirectories (only first level and second level deep) to later record in the database related to that storage, along with the meta data for the date created.
- The user will have the option to upload to only one storage or both storages (or as many as it's integrated to the application in future refactor).
  - Manually selects which accounts are configured for MEGA and 4shared - two separate columns with checkboxes for each account. In the side of each account checkbox, there should be the quota.
- The user will insert the "title" of the backup (example: xxx), and the application will create a main record for the xxx and record all subdirectories related to that main backup record.
- As the user selects the directory, the application zips it and uploads to the selected storages selected by the user, in the cloud root.
  - The name of the zip file will be reflected by the name of the directory the user pointed to zip.
    - Temporary space to zip the file will be at the same level the directory pointed to exists.
  - App checks total size vs available quota across accounts.
    - If it the account doesn't have the capacity for the intended upload, it warns with a modal and doesn't allow to proceed.
  - App creates job record in database (status: pending).
  - UI redirects to "Jobs" view showing progress.
  - Background worker picks up job, starts chunked upload.
  - Progress updates in real-time (WebSocket or polling every 2 sec).
    - Ability to resume unsuccessful uploads from where it stopped.
  - On completion: verify first chunk checksum.
  - On success: delete temp zip, mark job complete, backup database.
    - Update the quota for that specific account.
  - On failure: mark job failed, keep temp zip for retry.
### Table
- In the table layout, the user would see something like this:
  - title
  - MEGA (true / false)
  - MEGA Status (pending / in progress / complete or available / fail)
    - On fail, log the type of error in the DB.
    - Provide a button to open a modal to check all the logs from that provider / account.
  - MEGA Download (the zipped file from the storage)
  - MEGA Delete (deletes the record and the zip file in the storage, with a confirmation that I have to type "DELETE")
    - Delete only from the provider.
  - MEGA Upload date
  - 4shared (true / false)
  - 4shared Status (pending / in progress / complete or available / fail)
    - On fail, log the type of error in the DB.
    - Provide a button to open a modal to check all the logs from that provider / account.
  - 4shared Download (the zipped file from the storage)
  - 4shared Delete (deletes the record and the zip file in the storage, with a confirmation that I have to type "DELETE")
    - Delete only from the provider.
  - 4shared Upload date
  - The row would be an accordion that would expand to show the 2 levels of the directories present in the main record.
  - In the bottom of everything, there should be counters for:
    - Total GB
    - Total backup main records
### Other Features
- I'd like a search bar that would search for the subdirectories names in the database, and return me the titles (and date created) that contains that search term.
- I'd like an option "See Accounts Available", where it lists all the accounts (two columns) with their quota.


### .env File Format:

Note: single-quote any value containing `$`, `#`, backticks, or spaces (godotenv
expands unquoted/double-quoted values — see Technical Notes). The full 4shared
credential walkthrough lives in README.md → "Provider credentials".

# Main accounts for database backup — one per provider, both optional.
# Never upload targets; a provider with no _MAIN_EMAIL is skipped silently.
MEGA_ACCOUNT_MAIN_EMAIL=mega-main@example.com
MEGA_ACCOUNT_MAIN_PASSWORD='secret'

# The 4shared main account needs the same OAuth set as a numbered one:
#   go run ./cmd/fourshared-auth -account main
FOURSHARED_ACCOUNT_MAIN_EMAIL=4s-main@example.com
FOURSHARED_ACCOUNT_MAIN_CONSUMER_KEY=...
FOURSHARED_ACCOUNT_MAIN_CONSUMER_SECRET=...
FOURSHARED_ACCOUNT_MAIN_CONSUMER_DOMAIN=backmeup.example.com
FOURSHARED_ACCOUNT_MAIN_OAUTH_TOKEN=...
FOURSHARED_ACCOUNT_MAIN_OAUTH_TOKEN_SECRET=...

# MEGA accounts (numbered) — password login, no app registration
MEGA_ACCOUNT_1_EMAIL=mega1@example.com
MEGA_ACCOUNT_1_PASSWORD='pass1'
MEGA_ACCOUNT_1_QUOTA_GB=20

# 4shared accounts (numbered) — OAuth 1.0, per-account registered app.
# CONSUMER_* come from registering the app; OAUTH_TOKEN/SECRET are produced by
#   go run ./cmd/fourshared-auth -account 1
# CONSUMER_DOMAIN must be a real domain pointed at 127.0.0.1 (4shared rejects localhost).
FOURSHARED_ACCOUNT_1_EMAIL=4s1@example.com
FOURSHARED_ACCOUNT_1_PASSWORD='pass3'
FOURSHARED_ACCOUNT_1_QUOTA_GB=15
FOURSHARED_ACCOUNT_1_CONSUMER_KEY=...
FOURSHARED_ACCOUNT_1_CONSUMER_SECRET=...
FOURSHARED_ACCOUNT_1_CONSUMER_DOMAIN=backmeup.example.com
FOURSHARED_ACCOUNT_1_OAUTH_TOKEN=...
FOURSHARED_ACCOUNT_1_OAUTH_TOKEN_SECRET=...

# Add more as needed...


## Documentation
- Provide a README.md concise with the features, how to use it and troubleshoot.
- Update CLAUDE.md and files in project's .claude directory as needed. Especially after troubleshooting stuff that's worth noting.

## Workflow
- Divide each chunk of the work into PRs.
- For every chunk, create a new branch.
- **PRs are NOT stacked — each PR targets `main`.** I merge each PR into `main` and pull local `main` before the next phase, so every new branch is cut from `main` and its PR uses `--base main`. Do **not** base a new PR on the previous `pr/N` branch. Before opening a PR, sanity-check with `git diff --stat main..<branch>` (should show only the new work) and `git merge-base --is-ancestor pr/<prev> main` (prior PR already merged).
- Build commits however it makes better sense.
- Push the commits however it makes sense.
- Let me manually validate each chunk of work / PR before moving to the next one.
- **At the end of each PR, before committing/pushing to GitHub, provide a step-by-step manual validation checklist** so I can verify the work runs correctly, not just that it compiles. Write it to `dev-tools/prompts/output/validation/<pr-number>-slug.md` and also surface it in the chat. Cover: setup/config needed, the happy path for each new feature, edge cases and error paths (e.g. conflicts, retries, disabled-feature behavior), and how to confirm via UI/logs/DB. Wait for me to validate (and fix anything I find) before committing and opening the PR. This directory is not pushed to the repo.
- Provide PR description in dev-tools/prompts/output/pr-descriptions/number-of-pr.md
  - BTW: This directory shouldn't be pushed to the repo.
- This is the github repo: https://github.com/syncsystem-net/back-me-up.git (totally blank)
- Use 2 sub-agents:
  - One to carry out the work.
  - The other to verify if the work is being done according to plan.

### Deferred Items
Known rough edges we consciously decided not to fix live in `dev-tools/prompts/output/deferred-items.md` (local, gitignored). Read it before proposing a "fix" — the entry probably explains why it is that way. Add to it whenever a review or validation pass surfaces something real that we wave through, and delete the entry when it ships.

### Ticket Stories
Before starting each PR, write a ticket story in `dev-tools/prompts/output/tickets/pr-number-slug.md`.
Write it as a product or engineering management ask — not a retrospective. The format:

```
# Story: [Title]

## Background
[Why this work is needed. Business or product context.]

## User Stories
- As a [user], I want to [action] so that [benefit].

## Acceptance Criteria
- [ ] [Specific, testable condition]

## Out of Scope
- [What this PR explicitly does NOT include]

## Technical Notes
- [Constraints, dependencies, or implementation hints relevant to engineering]
```

This directory is not pushed to the repo (it's in .gitignore via dev-tools/).

---

## Roadmap: Ticket #7 (frontend refactor) — shipped in phases

Ticket #7 was too large for one PR. Detailed plan (local, gitignored): `dev-tools/prompts/output/plans/7-frontend-refactor-phases.md`. Design exports live in `dev-tools/prompts/layout/`.

**Decisions locked with the user (apply to all phases):**
- Grouping key is the **account email** — the same email on MEGA and 4shared is one "user". No account schema change.
- A backup record belongs to **one user and accumulates zips**; re-uploading appends another zip to the same record. Title is editable.
- **Every configured user appears as a row**, even with zero uploads.
- **No top-level "+ New Backup"** — uploads go through the per-row Upload / Edit.
- Full recursive tree is **capped by config, default 3 levels**.

**Phase 7a — redesign + per-user model: DONE.** `backups.owner_email`, `backup_zips` (with `tree_json`), `jobs.zip_id`, `GET /api/users`, record-level delete, full CSS/HTML rewrite to the Figma design.

**Phase 7b — DONE** (PR #8, branch `pr/8-full-tree-settings-search`). Shipped: recursive `scanner.Tree` with `scan.max_depth` (default 3, root + N levels below it); accent-insensitive exclude terms (`scanner.Fold`/`Matches`, hand-rolled fold table — `golang.org/x/text` is **not** in the module cache); DB-backed `settings` key/value table + `GET`/`PUT /api/settings` behind the now-enabled Settings button; interactive tree UI; `PATCH /api/backups/{id}` for title-only saves; `GET /api/search` retargeted at `backup_zips.tree_json`, reporting backup/zip/account per hit. Removed: `backup_directories` (dropped in migration), `directories.go`, `SearchBackupsByDirectory`, `GetBackupsHandler`, `assembleBackups`, `buildTreeJSON`, `prettyTree`, `GET /api/backups`.

**Exclude terms filter the recorded tree only — never the zip.** Decided with the user: the uploaded archive stays a complete copy of the source, so a term is a display/search filter, not a backup policy. Changing terms or `max_depth` does not rewrite trees already recorded.

**Phase 7c — DONE** (PR #9, branch `pr/9-auto-sync-remote-crawl`). The **Auto-Sync** button crawls every configured account's cloud root, reads the archives actually there, and reconciles them into the DB as *warning → dry-run preview → apply on confirm*, deleting nothing. Shipped: `provider.List` + `provider.ReadRange` (with `ErrRangeUnsupported`) on both backends; `internal/autosync` (pure `Plan`, `ziptree`, block-cached `remotereader`, background `Manager`); `database.InsertAdoptedJob`/`GetZip`; four `/api/autosync` routes (preview/apply/cancel/status). **No schema change** — an adopted archive is a synthetic `complete` job (`remote_path` set, empty `zip_path`, NULL `verify_checksum`) that the existing Download/Delete handlers already key on and the re-verifier correctly skips.

Decisions locked with the user, all implemented: crawled `tree_json` **is** derived from the remote ZIP's central directory (not empty); match by remote name against `backup_zips.name`, adopt unmatched `.zip`s, create the record if absent; a local row whose remote copy is gone is **report-only, never modified**; one global action, preview then confirm. See the 7c technical notes below for the ranged-read, idempotency, and cross-account-dedup lessons.

**Exclude terms filter the recorded tree only — never the zip.** Decided with the user: the uploaded archive stays a complete copy of the source, so a term is a display/search filter, not a backup policy. Changing terms or `max_depth` does not rewrite trees already recorded. (Ticket #7 is now fully shipped across 7a/7b/7c.)

---

## Roadmap: Pre-Launch Refinement — shipping in phases

Detailed plan (local, gitignored): `dev-tools/prompts/output/plans/pre-launch-refinement-phases.md`. Each phase is its own branch cut from `main`, with its own ticket, validation checklist and PR description.

**Decisions locked with the user (do not re-litigate):**
- A record renders **one** file tree; each zip is a top-level node with its own size and download link, its directories nested beneath. The zip node replaces the tree's root — they denote the same directory.
- Large archives are handled by **splitting into standalone zips** (bin-packed first-level subdirectories), never raw byte-split parts, with the threshold configured **per provider and per account tier** in the Settings modal and defaulted from each provider's real caps.
- Credentials are encrypted with AES-256-GCM, key derived from a `.env` passphrase plus a random per-install salt stored in the DB. **Losing the passphrase loses every stored credential** — say so loudly in docs and UI.
- The main account becomes **per-provider** (`MEGA_ACCOUNT_MAIN_*`); `MAIN_ACCOUNT_PROVIDER` goes away, and a provider with no main account configured is skipped silently.
- **No `+ New Backup` button**, even though the Figma file shows one (carries over from 7a).

**Provider caps that drive phase 5** — 4shared free: **2 GB max file size**, 15 GB storage, ~3 GB/day (documented as *download*) traffic. MEGA free: no file-size limit, 20 GB storage, ~5 GB per rolling 6h by IP. A ~4.3 GB archive therefore **cannot reach a free 4shared account at all**; this is a per-file cap, not a traffic problem, and it is the real launch blocker.

**There is no 4shared token refresh to automate** — OAuth 1.0 has no refresh-token concept. The remedy is re-authorization, bundled into phase 4 so the new token can be written to the DB from an in-app button.

**Phase 1 — Figma layout + rem + merged tree + poll interval: DONE** (PR #22, branch `pr/10-figma-layout-rem-merged-tree`). Rebuilt against Figma nodes `2-4`/`30-295`/`31-674`; SVG assets in `web/static/img/`; stylesheet fully in `rem`; merged record-level tree; `ui.poll_seconds`/`ui.active_poll_seconds`.

**Phase 2 — Resilient deletes: DONE** (PR #23, branch `pr/11-resilient-deletes`). Ticket `dev-tools/prompts/output/tickets/11-resilient-deletes.md`. Shipped: `provider.ErrNotFound` + `provider.ErrAuthExpired` sentinels (MEGA nil `HashLookup` and rejected login; 4shared 404 and 401), delete treats not-found as success, `DeleteBackup` attempts every job and reports per-archive failures as **409 + structured body**, an explicit "remove the record anyway" force step, and a `connect` seam on `Handlers` so the delete paths are testable against a stub backend. See the "Resilient deletes" technical note below.

**Phase 3 — Per-provider main account: DONE** (PR #24, branch `pr/12-per-provider-main-account`). Ticket `dev-tools/prompts/output/tickets/12-per-provider-main-account.md`. Shipped: `MEGA_ACCOUNT_MAIN_*` / `FOURSHARED_ACCOUNT_MAIN_*` (the 4shared main carrying its own OAuth quintet), `MAIN_ACCOUNT_*` removed with a startup warning naming the replacements, `AccountStore.Mains` + `MainFor`, `MainAccount.MissingKeys`/`Usable`, the metadata DB fanned out to every main account with each destination attempted independently, `GET /api/accounts/main` and a **Main accounts** section in the Accounts view, and `cmd/fourshared-auth -account main`. See the "Per-provider main account" technical note below.

**Phase 4 — NEXT.** Branch `pr/13-encrypted-credentials`, no ticket yet. Accounts move from re-read-from-`.env`-on-every-boot into the `accounts` table with passwords and OAuth tokens encrypted (AES-256-GCM; key derived via scrypt from a `.env` passphrase plus a random per-install salt stored in the DB; fresh nonce per row). `golang.org/x/crypto` **is** in the module cache, so scrypt needs no network fetch. Also here: detect 4shared `401.0301`, flag the account as needing re-authorization in the UI, and add a **Re-authorize** button running `cmd/fourshared-auth`'s local-callback flow server-side, writing the new token straight to the DB — OAuth 1.0 has no refresh token, so re-authorization is the only remedy. **Danger to design around:** the metadata DB is uploaded to every main account after each job, so encrypted credentials travel with it; the passphrase must never be in the DB, and losing it loses every stored credential — say so loudly in docs and UI.

**Phase 5** — per-provider/tier size caps and archive splitting. **This is the actual launch blocker**: a ~4.3 GB archive cannot reach a free 4shared account at all. Per provider × tier, a max-file-size/split threshold and a daily transfer budget, defaulted from the real caps and editable in the Settings modal; each account carries a tier. Splitting bin-packs first-level subdirectories into standalone zips, never raw byte-split parts, so each volume stays an openable archive for the tree reader, Auto-Sync adoption and per-file download. A single file larger than the threshold is reported, not silently split. The multi-zip record model needs no schema change. Open question for the user when starting: whether the daily budget should throttle uploads too, or only downloads (4shared documents its 3 GB as *download* traffic; upload behaviour is unverified).

---

## Technical Notes

Lessons learned and recurring patterns from development. Reference before implementing related features.

### SQLite

**Always set `db.SetMaxOpenConns(1)`**
SQLite allows only one concurrent writer. Without this, concurrent goroutines cause "database is locked" errors. Add immediately after `sql.Open()`.

**FK constraints block `DROP TABLE` when rows exist**
With `PRAGMA foreign_keys=ON`, SQLite refuses to drop a table that is referenced by rows in another table (e.g., dropping `accounts` fails if `jobs` has rows with `account_id` references). Fix: delete the referencing rows first, then drop the parent table.
```go
db.Exec(`DELETE FROM jobs`)       // removes FK ref to accounts
db.Exec(`DROP TABLE IF EXISTS accounts`)  // now succeeds
```
Alternative: rename the old table first (`ALTER TABLE accounts RENAME TO accounts_old`), recreate with new schema, then drop the renamed copy — no FK issues since nothing references `accounts_old`.

**Index over an ALTER-added column must be created after the ALTER**
Never put `CREATE INDEX ... ON t(col)` in the `schema` const when `col` is added later via `addColumnIfMissing`. On an existing database `CREATE TABLE IF NOT EXISTS t` is a **no-op**, so the column does not exist when the schema block runs and the index fails with `no such column`. Create such indexes after the additive column migrations:
```go
addColumnIfMissing(db, "jobs", "zip_id", "INTEGER")
db.Exec(`CREATE INDEX IF NOT EXISTS idx_jobs_zip_id ON jobs(zip_id)`)
```
This broke the first real PR #7 migration run even though fresh-database tests passed. **Always test the upgrade path, not just the greenfield path** — `TestMigrateFromLegacySchema` in `internal/database` builds a real pre-migration DB and now guards it.

**Schema migrations for tables with FK references**
Never use plain `DROP TABLE` in a migration when another table has live rows pointing to it. Always either clear the child rows first or use the rename pattern above.

**UNIQUE constraint scope for accounts**
The `accounts` table must use `UNIQUE(provider, email)` — not `UNIQUE` on `email` alone. The same email address can exist on different providers (e.g., same login on both MEGA and 4shared). A single-column UNIQUE on email silently overwrites the first account when the second is upserted.

**WAL mode and PRAGMA ordering**
`PRAGMA journal_mode=WAL` and `PRAGMA foreign_keys=ON` must be set after `sql.Open()` and before any queries. Both are connection-scoped; with `SetMaxOpenConns(1)` they persist for the lifetime of the app.

---

### Directory scanning and exclude terms

**Accent folding is hand-rolled on purpose.** `internal/scanner/normalize.go` maps accented Latin runes to their base letters instead of using `golang.org/x/text/unicode/norm`. That module is **not in the module cache** (only `x/crypto`, `x/mod`, `x/sync`, `x/sys`, `x/tools` are), so depending on it means a network fetch — the same reason `internal/ratelimit` is a hand-written token bucket. Check the cache before reaching for an `x/` package.

**Fold both Unicode forms, not just precomposed.** An accented name arrives as NFC (one rune, `U+00E9`) on Windows but NFD (base letter + combining mark) on macOS/APFS. Mapping only precomposed runes means "Conteúdo" silently fails to match `conteudo` on macOS — the exact promise the feature makes. `Fold` therefore strips combining marks (`U+0300`–`U+036F`) *and* maps precomposed runes. The JS side (`fold()` in `app.js`) mirrors this with `normalize('NFD')` + the same range strip. Tests must use explicit `́` escapes: a literal `ú` in a Go or JS source file is stored precomposed and will not exercise the NFD path.

**`Fold`/`Matches` are shared by exclude terms and global search** so the two features can never disagree about what "matches" means. A change to folding affects both — and the client-side table filter mirrors it, so all three stay consistent.

**Don't substring-match the raw `tree_json`.** The serialized tree contains the literal keys `children` and `size_bytes`, so matching the raw string means typing either one matches every record that has a tree. Match the extracted directory *names* (`treeNames` in `app.js`, `searchNode` in Go).

**A blank exclude term must never match.** Substring matching means an empty term matches every directory name and would silently empty every recorded tree. Blanks are rejected on write (`normalizeTerms`) *and* ignored on read (`Matches`) — belt and braces, because a corrupt settings row could otherwise bypass the write-side check.

**Depth semantics: `max_depth` counts levels *below* the root.** `3` records the root plus three levels. The root is never excluded even if its name matches a term — the user explicitly chose that directory.

---

### Alpine.js

**Use methods, not getters, in `Alpine.data()`**
Getters (`get filteredBackups() { ... }`) are not reliably invoked through Alpine 3's reactive proxy. Always define computed values as plain methods and call them with `()` in templates:
```js
// ✗ getter — Alpine does not reliably trigger these
get megaAccounts() { return this.accounts.filter(...) }

// ✓ method — works correctly
megaAccounts() { return this.accounts.filter(...) }
```
In templates: `x-for="a in megaAccounts()"`, not `x-for="a in megaAccounts"`.

**Alpine's `x-for` cannot recurse — flatten instead**
There is no clean way to render a recursive tree with `x-for` alone. The file tree walks the parsed `tree_json` into a **flat array of currently-visible rows** (`treeRows` in `app.js`), each carrying a `depth`, and renders it with a single `x-for`, expressing nesting as `padding-left: depth * 16px`. Collapsed subtrees are simply not emitted.

**Parsed JSON must be memoised outside the reactive proxy**
`treeCache` lives at module scope, not on the Alpine component. The page replaces `this.users` on every poll tick and templates re-render constantly, so `JSON.parse` on every render is real cost — and letting Alpine proxy a large read-only tree adds more. Cache by zip id, keyed on the raw string so an edited tree re-parses.

**Any UI state must be keyed to survive the poll**
`loadUsers()` replaces the whole user list on every tick, so state stored positionally (indexes, object identity) is destroyed each time. Key expansion state by stable identifiers: `expandedAccounts`/`expandedFiles` by email, tree nodes by `` `${zipId}:${nodePath}` ``. Tree nodes use **two** lists (`expandedNodes` + `collapsedNodes`) because the default is "root open, rest closed" — one list cannot distinguish "explicitly collapsed root" from "never touched".

**A self-rescheduling poll must reschedule in `finally`**
The refresh loop is a `setTimeout` that re-arms itself (two cadences: `ui.poll_seconds` idle, `ui.active_poll_seconds` while a job or an Auto-Sync crawl is running — a fixed `setInterval` cannot switch between them). That makes one rejected `fetch` fatal in a way `setInterval` never was: if the re-arm sits after an `await` that throws, polling stops **permanently and silently** for the rest of the session. Schedule the next tick in a `finally`, and keep the timer handle so an action that creates work (Upload, Auto-Sync) can re-arm immediately instead of waiting out an already-armed idle timer.

**Deriving "is anything happening" from job rows alone misses Auto-Sync**
An adopted archive is inserted as a `complete` job, so a running crawl creates nothing that `hasActiveJobs()` can see. Anything gating on "work in progress" must check the crawl phase too, or a multi-minute crawl reports its progress at the idle cadence.

**`x-cloak` to prevent blank flash on load**
Add `x-cloak` to any element that should be hidden until Alpine initialises, and add `[x-cloak] { display: none !important; }` at the top of the CSS file.

---

### Go → JS JSON serialization

**All DB/API structs need explicit `json:` tags**
Without tags, Go's `encoding/json` serializes field names in PascalCase (`Provider`, `Email`). JavaScript reads them as `a.provider`, `a.email` (camelCase) — they don't match and the values are `undefined`. Always add snake_case tags:
```go
type DBAccount struct {
    ID       int64  `json:"id"`
    Provider string `json:"provider"`
    Email    string `json:"email"`
}
```

---

### Browser static file caching

**`http.FileServer` caches aggressively via `Last-Modified`**
After updating JS or CSS, the browser may serve a stale cached version. Fix: wrap the static handler to force `Cache-Control: no-store`:
```go
staticFS := http.StripPrefix("/static/", http.FileServer(http.Dir("web/static")))
mux.Handle("/static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
    w.Header().Set("Cache-Control", "no-store")
    staticFS.ServeHTTP(w, r)
}))
```
Users still need one manual **Ctrl+Shift+R** the first time after this change is deployed — after that, subsequent reloads always fetch fresh assets.

---

### Native OS folder picker (Browse button)

**`file.path` is blocked by browsers even on localhost**
`<input webkitdirectory>` only gives `file.webkitRelativePath` (the relative folder name, e.g. `"bkup003"`), not the full OS path. The full path (`file.path`) is blocked by browser security even for `localhost` pages.

**Fix: server-side native dialog via `GET /api/browse`**
The server spawns a native OS dialog and returns the selected path. The HTTP request stays open while the user interacts with the dialog (typically < 30 seconds — browsers don't time out this fast for localhost).

Windows (`browse_windows.go`):
```go
script := `Add-Type -AssemblyName System.Windows.Forms; ` +
    `[System.Windows.Forms.Application]::EnableVisualStyles(); ` +
    `$f = New-Object System.Windows.Forms.FolderBrowserDialog; ` +
    `$owner = New-Object System.Windows.Forms.Form; ` +
    `$owner.TopMost = $true; ` +
    `$owner.Size = New-Object System.Drawing.Size(1,1); ` +
    `$owner.StartPosition = 'CenterScreen'; ` +
    `$owner.ShowInTaskbar = $false; ` +
    `$owner.Show(); $owner.BringToFront(); ` +
    `[System.Windows.Forms.Application]::DoEvents(); ` +
    `$result = $f.ShowDialog($owner); $owner.Dispose(); ` +
    `if ($result -eq [System.Windows.Forms.DialogResult]::OK) { Write-Output $f.SelectedPath }`
cmd := exec.Command("powershell", "-NoProfile", "-STA", "-ExecutionPolicy", "Bypass", "-Command", script)
```

**The `TopMost` + `Show()` + `BringToFront()` + `DoEvents()` sequence is required** — without showing the owner form first, the dialog spawns behind the browser window and the user can't see it. Use `-STA` (Single-Threaded Apartment) so Windows Forms works correctly from a non-UI thread.

macOS: use `osascript -e 'POSIX path of (choose folder ...)'` — it returns the path directly and exits when the user cancels (exit code 1 = no selection, not an error).

---

### Account structure: MAIN vs numbered

`<PROVIDER>_ACCOUNT_MAIN_*` is that provider's database-backup account — **at most one per provider**, **not** synced to the `accounts` DB table and **not** offered in the upload modal. It is reserved for uploading the SQLite DB after each successful job, and shows in the Accounts view under its own "Main accounts" heading.

Only numbered accounts (`MEGA_ACCOUNT_1_*`, `FOURSHARED_ACCOUNT_1_*`, etc.) appear in the New Backup modal as selectable upload targets. If a user configures only `MEGA_ACCOUNT_MAIN_EMAIL` and expects it to appear in the modal, it won't — they need a separate `MEGA_ACCOUNT_1_EMAIL` entry.

Log lines to verify on startup:
```
msg="main account (db backup only, not an upload target)" provider=mega email=... usable=true
msg="syncing account" provider=mega email=...
msg="syncing account" provider=fourshared email=...
msg="accounts synced" count=2
```

---

### Cloud provider integration (architecture)

Providers live behind a small abstraction so adding one is a focused change:

- `internal/provider/provider.go` — the `Provider` interface plus `Progress`, `Config`, `OAuthCreds` types. No concrete imports.
- `internal/provider/<name>/` — one subpackage per backend (`mega`, `fourshared`), each implementing `Provider`.
- `internal/provider/registry/` — maps a provider name to its implementation (`New(name, oauth, cfg)`). This is the only place that imports the concrete packages, so the worker depends on the interface, never on a backend.
- `internal/provider/oauth1/` — reusable OAuth 1.0a HMAC-SHA1 request signing for any OAuth provider.

Adding a provider = implement `Provider` in a new subpackage + add one `case` in the registry. Credentials are never stored in the DB; the worker matches a job's `account_id` (provider+email) to the in-memory `AccountStore` to get the password/token.

---

### MEGA provider

- Library: `github.com/t3rm1n4l/go-mega` (handles MEGA's encryption). Password login.
- **`"Object (typically, node or user) not found"` at login means invalid credentials** — most often the `.env` `$`-expansion gotcha below, occasionally an unregistered email. **2FA is not supported** (go-mega has a separate `MultiFactorLogin` we don't call).
- Chunk-level upload: `NewUpload` → `Chunks()`/`ChunkLocation(id)`/`UploadChunk(id, ...)`/`Finish()`. MEGA dictates its own chunk boundaries (the `upload.chunk_size_mb` config does not apply to it). The node hash (`node.GetHash()`) is the stored remote ref; `fs.HashLookup(hash)` resolves it for download/delete.

---

### 4shared provider (the hard one)

4shared's API is sparsely documented and full of surprises. Hard-won facts:

- **It is OAuth 1.0, not 1.0a.** The authorize callback returns only `oauth_token` and **no `oauth_verifier`**; the access-token exchange is signed with the request token and must **not** send `oauth_verifier`. (`cmd/fourshared-auth` treats the callback's arrival as the completion signal.)
- **Per-account application.** Each 4shared account is authorized through its own registered app: `FOURSHARED_ACCOUNT_<n>_CONSUMER_KEY` / `_CONSUMER_SECRET` / `_CONSUMER_DOMAIN` / `_OAUTH_TOKEN` / `_OAUTH_TOKEN_SECRET`. A shared `FOURSHARED_CONSUMER_KEY/SECRET` is an optional fallback.
- **Callback domain.** 4shared **rejects `localhost`** as an Application domain ("Invalid application domain"), and its out-of-band PIN page is broken (Allow → "Invalid token"). Register a real domain pointed at `127.0.0.1` (DNS A record or hosts file); `cmd/fourshared-auth` runs a local server on `127.0.0.1:<port>` to capture the redirect automatically.
- **POST/PUT params go in the body, not the query string** (else `400.0504 "The parameters must be in the body..."`). Send them form-encoded (`application/x-www-form-urlencoded`); form-body params are part of the OAuth signature base string, so set `req.Form` before signing.
- **Chunked upload flow:** `POST upload.4shared.com/v1_2/upload` (form: `name`, `folderId`, `size`) returns a FileResponse whose **`id` is the permanent file id** — reused for every chunk and for later download/delete. Chunks go to `POST /upload/{id}` with a `Content-Range` header; **308 = "resume incomplete" (more chunks), 201 = complete**, and neither response carries an id. **Do not let Go's HTTP client auto-follow the 308 as a redirect** — set `CheckRedirect` to `http.ErrUseLastResponse` for `/upload/` requests.
- `GET api.4shared.com/v1_2/user` returns quota (`totalSpace`/`freeSpace`) and `rootFolderId`. Delete: `DELETE /files/{id}`.
- **Folder-listing path is ambiguous (singular vs plural).** The published reference and various clients disagree on whether it's `/folder/{id}/files` or `/folders/{id}/files`; the singular form was observed returning **404 "Resource not found" (`404.0400`)** against a real account even though the same `rootFolderId` is accepted as a `folderId` by `POST /upload`. Because the right spelling isn't reliably documented, `listFolderFiles` **probes the candidates** (`folderFilesPaths = {"/folders/%s/files", "/folder/%s/files"}`) and uses the first that doesn't 404, logging `4shared folder listing path resolved` with the winner. A broken listing endpoint silently disables conflict detection (`resolveConflicts` logs the lookup failure as a warn and treats it as "no conflict"), so a duplicate name then surfaces only as a `403.0201` at upload time — see the next bullet.
- **Diagnostics:** `go run ./cmd/fourshared-test -account <n>` checks one account's creds in isolation; `FOURSHARED_DEBUG=1` logs the OAuth signature base string, Authorization header, and raw responses. Reach for these before guessing.
- **`403 ... "already exists" (`403.0201`)` on upload-init.** Unlike MEGA (which allows duplicate names), 4shared **refuses** to create a second file with an existing name at `POST /upload` time instead of overwriting. The UI conflict pre-check (`handlers.resolveConflicts`) deletes a duplicate before queuing when the user picks "overwrite", but it can miss one — most often a **prior upload that failed partway leaves the name reserved without appearing in the folder listing** (so `FindByName` returns not-found and never prompts). Fix: `fourshared.startUpload` takes an `allowReplace` guard — on `403.0201` it `FindByName`s the existing file, `Delete`s it, and retries the init once. This is safe because a queued job means the user already opted to upload to that account ("skip" creates no job). If the name is reserved but *not* listable (a true ghost/incomplete upload), the replace can't find it; the error then tells the user to delete it from the 4shared web UI. `isAlreadyExists()` matches both the `403.0201` code and the "already exists" message.
- **`401 ... "token ... expired, rejected or does not exist"` (`401.0301`)** means the OAuth access token is no longer valid server-side. **We set no token lifetime anywhere** — OAuth 1.0 access tokens have no client-configurable expiry, so there is nothing to tune in config; validity is entirely 4shared's call. Causes: 4shared expired it, the app was re-authorized (invalidates the previous token), or it was revoked. 4shared does not publish the TTL. Fix is always the same: re-run `cmd/fourshared-auth -account <n>` and replace the token in `.env`.

---

### Auto-Sync remote crawl (phase 7c)

`internal/autosync` crawls each account's cloud root and reconciles what's there into the DB. Hard-won facts:

**Read the ZIP central directory with ranged reads, not full downloads.** `provider.ReadRange` has `io.ReaderAt` semantics; `remotereader.go` wraps it (with a small block cache — the stdlib issues several small reads near the directory) and hands it to `zip.NewReader(r, size)`, which seeks to and parses the central directory itself, transferring only the archive's tail. Build the tree from entry **path components**, not `Mode().IsDir()` — a directory can be implied by a nested entry with no explicit directory entry of its own. Reuse `scanner.Excluded`/`Fold` and `scan.max_depth` so a crawled tree and an uploaded tree can never disagree about matching or depth.

**MEGA supports true random access; 4shared may not.** MEGA maps a byte range onto go-mega's download chunks (`Download.ChunkLocation`/`DownloadChunk`), clipping straddling chunks. 4shared's `Range` support is **undocumented**: `ReadRange` treats `206` as honored, `416` as EOF, and **`200` as "range ignored"** — it returns `ErrRangeUnsupported` rather than mis-slicing a full-body response (silently wrong offsets would corrupt every parsed tree). On that sentinel the indexer falls back to one full download to a temp file.

**Cap the fallback download on bytes received, never on advertised size.** `Download` streams without announcing a length, and 4shared's listing may report `size 0`, so a pre-flight size check is bypassable — a size-0 archive would download unbounded. `cappedWriter` fails the write that crosses the ceiling. An over-cap or otherwise unreadable archive is still **adopted, with a note** explaining it has no tree.

**Idempotency is a design property, so keep `Plan` pure.** `autosync.Plan(remote, local)` returns adopt/link/matched/missing without touching either side; feeding it the state a previous apply left must yield zero proposals. Two traps that break it: (1) tree serialization must be **deterministic** — sort children before emitting, or Go's random map order re-serializes an unchanged archive differently every run; (2) same-named zip rows (a record legitimately accumulates zips) must be disambiguated by a **rank** preferring the row with a usable remote handle for this account, else a link attaches to the wrong row.

**Cross-account adoption dedups at apply, not plan.** Every account is planned before any is applied, so an archive on two accounts is proposed `adopt` by both — applying verbatim makes two rows for one archive. `applyOne` re-resolves against **live DB state** before inserting: first account adopts, the rest degrade to a link against the row that now exists.

**A missing remote is report-only.** No code path in the package issues `UPDATE`/`DELETE` for a `missing` row — a transient listing failure must never delete a user's records. A per-account listing failure surfaces as a **visible error** in the preview, never as "nothing found" (same degradation as the 4shared conflict-detection path).

**Start/poll, not one long HTTP request.** A crawl runs minutes; the four routes return immediately and the UI polls `GET /api/autosync`, so the 2s table refresh never blocks and the run is cancellable.

---

### Resilient deletes (pre-launch phase 2)

**"Already gone" is success, not failure.** A delete exists to reach the state "the object is not on the provider". If the provider says it is not there, that state holds. Treating it as an error made records unremovable, and it is *guaranteed* to happen: Auto-Sync deliberately reports a vanished remote copy without modifying the record, so the DB is expected to hold rows pointing at absent archives. `provider.ErrNotFound` is the sentinel; only the delete path may swallow it — `Download`/`ReadRange` return the same sentinel but must still fail.

**Two sentinels, matched with `errors.Is`, never by message text.** `ErrNotFound` and `ErrAuthExpired` live in `internal/provider`. MEGA: nil `HashLookup` → `ErrNotFound` (all three sites), and a rejected login → `ErrAuthExpired` — MEGA answers a wrong password with its generic `ENOENT` ("Object … not found"), so without translation the UI would tell the user their archive is gone when their password is wrong. 4shared: `statusError` maps 404 → `ErrNotFound` and 401 → `ErrAuthExpired`, leaving **403 alone** so the `403.0201` "already exists" upload rejection is not swept up.

**Never abort a multi-target operation on the first failure.** One expired credential used to block deleting even the copies that were reachable, leaving "delete the record but not the files" (which orphans everything) as the only escape. `deleteRemoteFiles` attempts every job and collects failures.

**Partial failure is `409` with a structured body, not `502`.** The client must distinguish "nothing happened" from "some things happened, here is what did not" in order to offer the force step. Body: `{error, deleted, failures:[{job_id, provider, email, archive, reason, message}]}`, `reason` being `credentials` or `provider`. **On partial failure the local record is kept** — nothing in the DB changes.

**A retry after a partial failure converges.** The copies deleted on the first pass now answer `ErrNotFound`, which is success. That is why the record can safely be kept rather than half-deleted.

**The force step deletes the local record only and contacts no provider.** Re-attempting there would be pointless (the previous request just tried) and risks claiming a remote delete that did not happen. It still requires the typed `DELETE`, so a stray `force` flag cannot remove a record on its own.

**`Handlers.connect` is a seam, like `autosync.Manager.connect`.** The delete paths are *entirely about* how they behave when a backend fails, which is unreachable without injecting one. It wraps `cloud.Connect` in production; tests supply a stub provider.

---

### Per-provider main account (pre-launch phase 3)

**An optional setting must never be able to take the required ones down with it.** `accounts.Load` used to *error* when `MAIN_ACCOUNT_PROVIDER` was unset, and `main.go` treats any `Load` error as "running without accounts" — so forgetting the db-backup account silently dropped **every** upload account. Absence of an optional thing returns absence, not an error.

**Absence is silent; half-configured is loud.** A provider with no `*_ACCOUNT_MAIN_EMAIL` produces no log line at all — it is a supported choice, and warning about it trains the user to ignore warnings. A main account that *is* declared but lacks credentials it needs (`MainAccount.MissingKeys` → full `.env` key names, not suffixes, so the message is pasteable) is warned about once at load and flagged in the Accounts view.

**Removed keys warn rather than being quietly adopted.** Leftover `MAIN_ACCOUNT_*` values are ignored *and* reported with their replacements. Silently promoting a value from a key we claim to have removed is worse than a clear break; silently ignoring it stops the user's db backups with no visible cause.

**The main account carries its own credentials.** `MainAccount` holds the OAuth quintet, so the 4shared main account works standalone. It replaced `worker.mainOAuth`'s hack of matching the main email against the *numbered* 4shared accounts to borrow their token — which meant a main account that wasn't also an upload target silently had no token at all.

**The fan-out never stops at the first failure** — the same rule as resilient deletes. One snapshot of the DB is copied once and uploaded to every main account; a rejected login on one provider must not cost the other its copy of the index. `usableMains` filters unsupported/incomplete entries *before* any network call, so the per-job logs only ever contain real failures.

**"No main account configured" is logged once per process** (`Worker.noMainOnce`), not once per successful job. It is a standing state, not an event.

**Main accounts stay out of the `accounts` table, deliberately.** That table feeds the upload modal *and* `GetUsersHandler`, which groups the backups table by account email — a row there would both offer the db-backup account as an upload target and invent a user row for it. `GET /api/accounts/main` therefore reads the in-memory `AccountStore` and returns provider, email and configuration state only; the credentials never reach the browser (`json:"-"` on every secret, plus a test asserting the response body contains none of them). The cost is that main accounts have no quota display and no `last_quota_sync` — nothing polls them. Revisit in phase 4.

**`cloud.Connect` was deliberately *not* widened to reach main accounts.** It resolves credentials from `store.Accounts`, and every handler that takes a provider/email pair goes through it; teaching it about main accounts would make the db-backup account addressable as an upload target. `Worker.registryMainProvider` builds those through the registry directly instead.

---

### .env credentials and special characters

**`godotenv` expands `$` in unquoted AND double-quoted values.** A password like `paSs1$2178` silently becomes `paSs1` (everything from `$` is treated as a variable reference). This is a top cause of "wrong credentials" failures (e.g. MEGA's "Object not found").

Wrap any value with `$`, `#`, backticks, or spaces in **single** quotes (double quotes still expand):
```env
MEGA_ACCOUNT_1_PASSWORD='paSs1$2178'
```
OAuth tokens/consumer keys are hex and don't need quoting.

---

### Background upload worker

- `internal/worker` owns the pool. Sizing: pool goroutines = `concurrency.max_workers`; a global semaphore caps simultaneous uploads at `concurrency.max_concurrent_uploads`; a per-account semaphore enforces `concurrency.max_concurrent_per_account`.
- Jobs are claimed atomically with `UPDATE jobs SET status='in_progress' ... WHERE id=(SELECT ... WHERE status='pending' ... LIMIT 1) RETURNING id` so no two workers take the same job.
- **Resume is whole-file, not chunk-level.** Provider upload sessions can't be reconstructed across a process restart, so `RequeueStaleJobs` resets `in_progress`→`pending` at startup and the job re-uploads from 0 (progress is reset per attempt). Chunk-level resume only happens within a single in-process attempt.
- On success: verify the first chunk's checksum, refresh that account's quota, mark complete, then delete the temp zip **only when every sibling job sharing that zip is complete** (a failed sibling keeps it for retry), and back up the metadata DB to **every configured main account**, each attempted independently.
- The UI shows a **"verifying"** label when bytes are 100% uploaded but the job is still `in_progress` (post-upload checksum/finalize), so the bar doesn't look stuck.

---

### Rate limiting

- `internal/ratelimit` is a self-contained token bucket (no `golang.org/x/time/rate` dependency — it isn't in the module cache, and a custom bucket avoids a network fetch). Each `*Limiter` has independent request-per-second and bytes-per-second buckets; either is skipped when its rate is `0`, and a **nil `*Limiter` is a valid no-op** so providers never need to guard configuration presence (they still nil-check the interface value).
- Limits are **per-provider, shared across all that provider's accounts** (matching the `rate_limits.mega` / `rate_limits.fourshared` config shape), not per-account. `WaitBytes` splits a request larger than the bandwidth burst into burst-sized pieces so a 100MB chunk against a 5MB/s limit smooths out instead of erroring.
- Wiring avoids signature ripple: the limiters live in a **process-global `ratelimit.Set`** installed once in `main.go` via `ratelimit.Configure`. `registry.New` reads `ratelimit.For(name)` into `provider.Config.RateLimiter` (an interface declared in the `provider` package, so backends stay import-clean). `cloud.Connect` and the worker's DB-backup path populate it; handlers/quota/worker call sites are unchanged.
- **4shared** is paced per HTTP request (a `do()` helper calls `WaitRequest` before every `http.Do`) and per upload chunk (`WaitBytes` before sending the body). **MEGA** is paced at **chunk granularity only** — go-mega owns its HTTP client, so individual MEGA requests aren't interceptable; this is best-effort throttling, documented as such.

---

### Periodic re-verification

- Gated on `verification.enabled` (config coerces `periodic_check_days` `0`→`30`, so the days knob can't disable it — turn it off with `verification.enabled: false`). The worker launches `reverifyLoop` (in `internal/worker/reverify.go`) from `Start`; it runs once ~30s after startup then on an interval **derived from `periodic_check_days`** (`reverifyInterval` in `main.go`: ~period/4, clamped to [1h, 24h]).
- It needs a reference checksum that outlives the deleted temp zip: verify-on-upload now **persists the first-chunk SHA-256** to `jobs.verify_checksum` (+ `last_verified_at`) via `database.SetJobVerified`. These are additive column migrations (`addColumnIfMissing`), also added to the `CREATE TABLE jobs` schema for fresh DBs.
- Each scan re-checks a **small random sample** (`reverifyBatchSize = 5`, `ORDER BY RANDOM()`) of completed jobs whose `last_verified_at` is older than `periodic_check_days` (or null). It re-downloads only the first chunk (reusing the `cappedHasher`/`errEnoughBytes` head-download path) and compares to the stored checksum. Pass → `TouchJobVerified`; mismatch or download failure → an `error` row in `job_logs` (visible in the existing logs modal). **Re-verification reports only — it never re-uploads, deletes, or changes job status.**
- Both verify-on-upload and re-verify hash the leading `upload.chunk_size_mb` bytes. **Changing `chunk_size_mb` between an upload and a later re-verify can cause a false mismatch** (different span hashed); documented as a known caveat.
