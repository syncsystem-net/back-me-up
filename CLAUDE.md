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

# Main account for database backup
MAIN_ACCOUNT_PROVIDER=mega
MAIN_ACCOUNT_EMAIL=main@example.com
MAIN_ACCOUNT_PASSWORD='secret'

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
- Provide PR description in dev-tools/prompts/output/pr-descriptions/number-of-pr.md
  - BTW: This directory shouldn't be pushed to the repo.
- This is the github repo: https://github.com/syncsystem-net/back-me-up.git (totally blank)
- Use 2 sub-agents:
  - One to carry out the work.
  - The other to verify if the work is being done according to plan.

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

**Schema migrations for tables with FK references**
Never use plain `DROP TABLE` in a migration when another table has live rows pointing to it. Always either clear the child rows first or use the rename pattern above.

**UNIQUE constraint scope for accounts**
The `accounts` table must use `UNIQUE(provider, email)` — not `UNIQUE` on `email` alone. The same email address can exist on different providers (e.g., same login on both MEGA and 4shared). A single-column UNIQUE on email silently overwrites the first account when the second is upserted.

**WAL mode and PRAGMA ordering**
`PRAGMA journal_mode=WAL` and `PRAGMA foreign_keys=ON` must be set after `sql.Open()` and before any queries. Both are connection-scoped; with `SetMaxOpenConns(1)` they persist for the lifetime of the app.

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

`MAIN_ACCOUNT_*` is the database-backup account — it is **not** synced to the `accounts` DB table and **not** shown in the UI modal. It is reserved for uploading the SQLite DB after each successful job.

Only numbered accounts (`MEGA_ACCOUNT_1_*`, `FOURSHARED_ACCOUNT_1_*`, etc.) appear in the New Backup modal as selectable upload targets. If a user configures only `MAIN_ACCOUNT_EMAIL` for MEGA and expects it to appear in the modal, it won't — they need a separate `MEGA_ACCOUNT_1_EMAIL` entry.

Log lines to verify on startup:
```
msg="main account (db backup only, not shown in UI)" provider=mega email=...
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
- `GET api.4shared.com/v1_2/user` returns quota (`totalSpace`/`freeSpace`) and `rootFolderId`. Folder listing: `GET /folder/{id}/files`; delete: `DELETE /files/{id}`.
- **Diagnostics:** `go run ./cmd/fourshared-test -account <n>` checks one account's creds in isolation; `FOURSHARED_DEBUG=1` logs the OAuth signature base string, Authorization header, and raw responses. Reach for these before guessing.
- **`401 ... "token ... does not exist"`** usually means a **stale token** — re-authorizing an app invalidates the previous token. Re-run `cmd/fourshared-auth`.

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
- On success: verify the first chunk's checksum, refresh that account's quota, mark complete, then delete the temp zip **only when every sibling job sharing that zip is complete** (a failed sibling keeps it for retry), and back up the metadata DB to the main account.
- The UI shows a **"verifying"** label when bytes are 100% uploaded but the job is still `in_progress` (post-upload checksum/finalize), so the bar doesn't look stuck.
