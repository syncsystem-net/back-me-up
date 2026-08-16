# BackMeUp

Backup tool that zips local directories and uploads them to cloud storage providers (MEGA, 4shared). Manage multiple accounts, track upload jobs, and verify file integrity.

## Features

- The backups table is grouped by **user** (account email): the same email configured on both MEGA and 4shared shows as one row, and every configured account appears even before its first upload.
- Point at a directory from a user's row (Upload / Edit), give the backup a title (defaults to the folder name), and it uploads to that user's accounts. A record belongs to one user and **accumulates zips** over time — each upload adds another archive, and every zip's directory tree is recorded and shown under "Files: expand".
- A background worker pool uploads in chunks with live progress, automatic retry with exponential backoff, and a quota pre-check that refuses a backup that won't fit. The table refreshes every `ui.poll_seconds` (default 10) while idle and speeds up to `ui.active_poll_seconds` (default 2) while a job is running.
- On success: the first chunk's checksum is verified, the account's quota is refreshed, the temp zip is cleaned up, and the metadata database is backed up to every configured main account (one per provider).
- Per-provider status, a "verifying" state while finalizing, and a logs modal per job (including failure reasons).
- **Download** on an account card fetches every zip stored on that account (one file at a time — the browser asks once for permission to download multiple files); the `(download file)` link on a zip's own node in the tree fetches just that archive. Plus per-provider Delete-All and record-level "Delete Record (not files)" and "Delete Record And Files" (typed `DELETE`) — with overwrite-or-skip prompts when a same-name file already exists on a selected account.
- **Deletes are resilient.** An archive that is already gone from the provider counts as deleted (that is the state the delete was after), and every copy is attempted rather than the whole operation stopping at the first problem. If some copies genuinely can't be removed — an expired 4shared authorization, say — the record is kept and the modal names each archive, its account, and why, then offers to remove the record anyway once you've seen what will be left in the cloud. Retrying is safe too: the copies already deleted simply report "not found", which counts as success.
- A record's archives are shown as **one tree**: every zip is a top-level node carrying its own size and download link, with that archive's full directory tree (down to `scan.max_depth`, default 3 levels) nested beneath it. Expand the whole record at once or one node at a time.
- **Exclude terms** (Settings) keep noisy directories out of the recorded tree. Matching ignores case *and* accents, so one `conteudo` entry covers "Conteúdo", "conteudo", and "CONTEÚDO". Terms affect the recorded tree only — the uploaded zip still contains every directory.
- Two kinds of search, both accent-insensitive: typing filters the table in real time by user, title, or directory name, while the **▶ button runs a global search** across every recorded tree and reports which backup, zip, and account holds each matching directory.
- **Auto-Sync** discovers archives already sitting in your cloud accounts (from before you used this tool, or lost with a previous database) and reconciles them into the table. It shows a dry-run preview of every change before writing anything and never deletes — see "Auto-Sync" below.
- An "Accounts" view groups each provider's accounts in expandable cards with used/total quota and when it was last synced.
- Quotas refresh automatically on a background interval (`quota.sync_interval_minutes`) and on demand via the "Refresh quotas now" button, in addition to refreshing after each successful upload.
- Per-provider rate limiting (`rate_limits.<provider>`) paces API requests and upload bandwidth so a large backup stays within each provider's limits.
- Periodic re-verification (`verification.periodic_check_days`) re-downloads the first chunk of a random sample of already-uploaded files on a schedule and compares it to the checksum recorded at upload time; mismatches surface in the per-job logs modal.
- **Credentials are encrypted at rest** (AES-256-GCM, key derived from a `.env` passphrase plus a per-install salt) and live in the database rather than being re-read from `.env` on every boot — which matters because the metadata database is uploaded to your main accounts after every job. Lose the passphrase and every stored credential is lost; see "Credential storage and the passphrase".
- **Re-authorize 4shared in one click.** An account whose token the provider rejected is flagged in the Accounts view with a button that runs the OAuth flow server-side and stores the new token — no `.env` edit, no restart.
- Providers are pluggable: MEGA (email/password) and 4shared (OAuth 1.0) today, with a registry so new backends are a focused addition.

## Prerequisites

- Go 1.21+
- [Air](https://github.com/air-verse/air) for live reload (optional)

> No C compiler (GCC/CGO) required. The project uses a pure-Go SQLite driver.

## First-time Setup

1. Copy `.env.example` to `.env` and configure your accounts:
   ```
   cp .env.example .env
   ```

2. Set `BACKMEUP_CREDENTIAL_PASSPHRASE` in `.env` to a long random string, and **store it somewhere you will still have if this machine dies**. It encrypts every stored password and token; see [Credential storage and the passphrase](#credential-storage-and-the-passphrase) — losing it loses every stored credential.

3. Edit `config.yml` to adjust settings (chunk size, retry policy, rate limits, etc.).

4. Download dependencies:
   ```
   go mod download
   ```

5. Run the server:
   ```
   go run ./cmd/server
   ```
   Or with Air for live reload:
   ```
   air
   ```

6. Open `http://localhost:8080` in your browser.

## Project Structure

```
cmd/server/            - Application entry point (HTTP server + worker pool)
cmd/fourshared-auth/   - One-time OAuth helper to authorize a 4shared account
cmd/fourshared-test/   - Diagnostic: tests one 4shared account's credentials in isolation
internal/
  config/              - YAML configuration loader
  accounts/            - .env parsing + the running credential set (concurrency-safe)
  keyring/             - scrypt key derivation and AES-256-GCM sealing of credentials
  credentials/         - Reconciles .env into the encrypted store; decides which copy wins
  reauth/              - Server-side OAuth re-authorization flow (the Re-authorize button)
  database/            - SQLite setup, schema, and per-entity queries
  scanner/             - Recursive directory scanner (depth-capped, accent-insensitive excludes)
  archive/             - Zip creation
  worker/              - Background upload pool: claim, retry, verify, DB backup
  provider/            - Cloud provider interface + Progress/OAuth types
    mega/              - MEGA implementation (chunked, encrypted)
    fourshared/        - 4shared implementation (OAuth 1.0 REST)
    oauth1/            - Reusable OAuth 1.0a request signing + the 3-legged authorization flow
    registry/          - Maps provider name -> implementation
  server/              - HTTP server, routes, handlers
web/
  templates/           - HTML templates (Alpine.js)
  static/              - CSS, JS assets
```

## Configuration (config.yml)

Application behavior is tuned in `config.yml`; every value has a sensible default if omitted.

| Setting | Default | What it controls |
|---|---|---|
| `server.host` / `server.port` | `localhost` / `8080` | HTTP listen address |
| `database.path` | `backmeup.db` | SQLite file location |
| `upload.chunk_size_mb` | `100` | Upload chunk size (MEGA dictates its own; applies to providers that chunk) |
| `upload.temp_dir` | `""` | Where zips are staged; empty = same level as the source directory |
| `retry_policy.*` | `3` attempts, 2s→60s backoff, ×2 | Per-job retry with exponential backoff |
| `concurrency.max_workers` | `5` | Worker pool goroutine count (claimers) |
| `concurrency.max_concurrent_uploads` | `2` | Hard ceiling on simultaneous uploads across all accounts |
| `concurrency.max_concurrent_per_account` | `1` | Simultaneous uploads per single account |
| `rate_limits.<provider>.requests_per_second` | `10` (mega) / `5` (4shared) | Per-provider API request rate ceiling; `0` = unlimited |
| `rate_limits.<provider>.bandwidth_mb_per_second` | `5` (mega) / `3` (4shared) | Per-provider upload bandwidth ceiling in MB/s; `0` = unlimited |
| `verification.enabled` / `verify_on_upload` | `true` / `true` | Download the first chunk and compare checksum after upload |
| `verification.periodic_check_days` | `30` | Re-verify a completed file if it hasn't been re-checked within this many days (an omitted/`0` value falls back to 30). Turn periodic re-verification off with `verification.enabled: false`. |
| `quota.sync_interval_minutes` | `60` | How often the background poller refreshes every account's cached quota (also refreshed after each upload and via "Refresh quotas now") |
| `scan.max_depth` | `3` | Directory levels below the source root recorded in a zip's tree. `3` records the root plus three levels; an omitted, `0`, or negative value falls back to 3 |
| `ui.poll_seconds` | `10` | How often the browser re-fetches the table while nothing is uploading |
| `ui.active_poll_seconds` | `2` | Refresh cadence used **only** while a job is pending or in progress, so progress bars stay smooth without polling hard when idle. Clamped to at most `poll_seconds` |
| `reauth.callback_port` | `8723` | Port the temporary local listener binds during in-app re-authorization; must match the callback URL the provider's registered app accepts |
| `reauth.timeout_minutes` | `5` | How long re-authorization waits for you to approve in the browser before giving up and releasing the port |

Credentials are **not** in `config.yml` — they live in `.env` (see below).

### Settings stored in the app (not config.yml)

**Exclude terms** are managed in the UI (the **Settings** button on the backups view) and stored in the database, so they're editable without a restart. A directory whose name contains one of the terms is left out of the recorded tree along with its whole subtree. Matching ignores case *and* accents — one `conteudo` entry covers "Conteúdo", "conteudo", and "CONTEÚDO".

Two things worth being clear about:

- Exclude terms shape the **recorded tree only**. The uploaded zip still contains every directory, so the archive stays a complete copy of the source.
- Terms and `scan.max_depth` apply to the **next** backup. Trees already recorded are never rewritten — re-upload a record to refresh its tree.

## Auto-Sync

If you already have `.zip` archives in a cloud account — uploaded before you used this tool, or lost when a previous database went away — **Auto-Sync** pulls them back into the table without re-uploading anything. The **Auto-Sync** button (top bar) opens a warning, then runs a read-only crawl of every configured account's cloud root and shows a **preview** of exactly what it would change, grouped per account:

- **new record** — an archive in the account your database has never seen. Adopting it creates a record and reads the archive's directory tree (so it's searchable like an uploaded one).
- **connect account** — an archive your database already knows (uploaded to a sibling account) that this account also holds. A reference is added so Download/Delete work here; the existing record and its tree are left unchanged.
- **in sync** — already recorded; nothing happens. (A second run with no remote changes proposes nothing.)
- **not found** — a record pointing at a file no longer in the account. **Reported only** — nothing is modified or deleted, so a transient API error can never lose your data.

Nothing is written until you confirm. Auto-Sync never deletes anything, cloud or local.

Notes and limits: only each account's **cloud root** is crawled (no nested folders), and only `.zip` files are adopted. A discovered archive's tree is read straight from the ZIP's central directory using ranged reads, so only the tail of the file is transferred — **except on 4shared**, whose ranged-read and file-size support are undocumented: if it can't range-read, the archive is downloaded once to read its directory (capped at ~2 GB — a larger archive is still adopted, just without a tree). Adopted archives aren't periodically re-verified (there's no upload-time checksum for them).

## Credential storage and the passphrase

Every cloud password and OAuth token is stored in the database, encrypted with **AES-256-GCM**. The key is derived with scrypt from `BACKMEUP_CREDENTIAL_PASSPHRASE` in `.env` plus a random per-install salt generated on first run and kept in the database. Each account's credentials are sealed as one document under a fresh nonce, bound to that account so a row copied over another fails to open rather than authenticating as the wrong account.

This exists for one concrete reason: **the metadata database is uploaded to your main accounts after every successful job.** Unencrypted, your cloud passwords would routinely travel to the cloud in the clear.

> ### ⚠️ Losing the passphrase loses every stored credential
>
> The passphrase is never written to the database it protects — if it were, it would travel with the backup and protect nothing. There is no recovery path and no reset.
>
> Losing it does **not** lose your backups: the archives stay in the cloud and your provider accounts are unaffected. But BackMeUp will refuse to use its stored credentials, and the only way forward is to clear them and set every password and re-authorize every 4shared account from scratch.
>
> Keep it somewhere that survives this machine. Changing it later is not supported yet.

### What happens if the passphrase is missing or wrong

The app **still starts**. Your records, trees and searches stay browsable, and a red banner at the top of the page names the key to fix. What it will not do is act: uploads, downloads, provider deletes, Auto-Sync, quota polling and the metadata DB backup all refuse with the same reason, queued jobs stay `pending`, and **nothing is re-encrypted** — so a wrong passphrase can never destroy credentials the right one would still open. Fix `.env` and restart.

The startup log says so plainly:

```
level=ERROR msg="credentials are locked; the app is running read-only" reason="No credential passphrase is set. Add BACKMEUP_CREDENTIAL_PASSPHRASE to .env ..."
```

### `.env` is the way in; the database is the source of truth

On first run with this build, every account in `.env` is imported into the database and encrypted (one `imported account from .env into the encrypted store` log line each). After that:

| In `.env` | Effect on the stored account |
|---|---|
| A new `_EMAIL` entry | Creates the account |
| A changed password / consumer key / secret / domain | Updates it |
| A blank or deleted value | Ignored — absence never erases a stored credential |
| A numbered account removed entirely | **Kept.** Removing an upload target has to be deliberate |
| A `MAIN` account block removed | **Deleted** — `.env` is a main account's only removal path, and quietly continuing to copy your database to a destination you removed would be worse |
| A `FOURSHARED_..._OAUTH_TOKEN` the app wrote itself via **Re-authorize** | **Not overwritten** by the older value still in `.env` |

That last row is what makes the Re-authorize button worth having: without it, every restart would put the expired token back.

### Re-authorizing 4shared from the app

OAuth 1.0 has no refresh token, so a rejected token (`401.0301`) can only be replaced by authorizing again. When a provider rejects an account's token, BackMeUp records it: the account's card in the **Accounts** view gets a `needs re-authorization` badge and a **Re-authorize** button, and the flag survives a restart.

Pressing it runs the OAuth dance server-side using that account's own consumer key/secret and `CONSUMER_DOMAIN`, tries to open your browser, and **always shows the authorize URL as a link** in case it doesn't. Approve access as that 4shared account; the new token is encrypted, written to the database, and used immediately — no `.env` edit, no restart.

Requirements, all the same ones `cmd/fourshared-auth` has: the account needs `CONSUMER_KEY`, `CONSUMER_SECRET` and `CONSUMER_DOMAIN`, and the domain must be registered with the 4shared application and resolve to `127.0.0.1` (4shared rejects `localhost`). The listener binds `reauth.callback_port` (default `8723`, matching the CLI helper) and gives up after `reauth.timeout_minutes`. `cmd/fourshared-auth` still works for bootstrapping an account that has no stored row yet.

## .env Account Structure

The `.env` file has two distinct account types:

| Variable prefix | Purpose | Shown in UI |
|---|---|---|
| `MEGA_ACCOUNT_MAIN_*`, `FOURSHARED_ACCOUNT_MAIN_*` | Receives a copy of the SQLite DB after every successful job | In the Accounts view only, never as an upload target |
| `MEGA_ACCOUNT_1_*`, `MEGA_ACCOUNT_2_*`, … | Accounts available for user-selected backups | Yes |
| `FOURSHARED_ACCOUNT_1_*`, … | Same for 4shared | Yes |

Example — one MEGA backup account plus one 4shared, with a MEGA main account:
```env
MEGA_ACCOUNT_MAIN_EMAIL=db-backup@example.com
MEGA_ACCOUNT_MAIN_PASSWORD='secret'

MEGA_ACCOUNT_1_EMAIL=uploads@example.com
MEGA_ACCOUNT_1_PASSWORD=secret
MEGA_ACCOUNT_1_QUOTA_GB=20

FOURSHARED_ACCOUNT_1_EMAIL=uploads@4shared.com
FOURSHARED_ACCOUNT_1_PASSWORD=secret
FOURSHARED_ACCOUNT_1_QUOTA_GB=15
```

Main accounts are intentionally excluded from the backup modal — they are reserved for database backup only. They appear in the Accounts view under **Main accounts** so you can confirm the app read your configuration the way you meant it.

### Main accounts are per provider

There is at most **one main account per provider**, configured with the same `MAIN` slot a numbered account fills with its index:

- MEGA: `MEGA_ACCOUNT_MAIN_EMAIL` / `_PASSWORD`
- 4shared: `FOURSHARED_ACCOUNT_MAIN_EMAIL` plus the same OAuth set a numbered 4shared account needs (`_CONSUMER_KEY`, `_CONSUMER_SECRET`, `_CONSUMER_DOMAIN`, `_OAUTH_TOKEN`, `_OAUTH_TOKEN_SECRET`). Authorize it with `go run ./cmd/fourshared-auth -account main`.

After every successful job the metadata database is uploaded to **every** configured main account. Each destination is attempted independently, so an expired token on one provider does not cost the other its copy; failures are logged per destination, naming the provider and account.

Configuring **no** main account is supported and warns about nothing — it simply means no copy of your index is kept off this machine, so losing this machine loses the record of what was backed up where (the archives themselves remain in the cloud, and Auto-Sync can rebuild a record of them).

A main account that is configured but **incomplete** (a MEGA one with no password, a 4shared one with no token) is a different matter: it is reported as a warning at startup naming the exact `.env` keys to fill in, and flagged in the Accounts view.

Main accounts show their used/total quota and last-synced time in the Accounts view like any other account: the quota poller makes a second pass over them, writing to the `main_accounts` table (they are deliberately not rows in `accounts`, which drives the upload modal and the per-user table).

> **Upgrading:** `MAIN_ACCOUNT_PROVIDER` / `MAIN_ACCOUNT_EMAIL` / `MAIN_ACCOUNT_PASSWORD` are no longer read. Leaving them in `.env` produces a startup warning naming the replacements; their values are **not** adopted, so rename them or your database backups will stop.

## Provider credentials

Each cloud provider authenticates differently. This section documents, per provider, what credentials you need and how to obtain them. When a new provider is added it gets its own subsection here following the same shape: how it authenticates → what to register → which `.env` keys to set.

### MEGA (password)

MEGA authenticates with the account email and password directly. No app registration is required — set `MEGA_ACCOUNT_<n>_EMAIL` / `_PASSWORD` / `_QUOTA_GB` and you are done. The same email/password must work when you log in at <https://mega.nz>; accounts protected with two-factor authentication are not currently supported.

### 4shared (OAuth 1.0)

4shared's API does **not** accept a plain email/password. It uses OAuth: a per-account application **consumer key/secret** plus a per-account **access token** that you authorize once. The email/password in `.env` are kept only for display. Each 4shared account is authorized through its own registered application, so the consumer key/secret, callback domain, and tokens are all configured per account under the `FOURSHARED_ACCOUNT_<n>_*` keys.

Authorization needs a **callback domain**. 4shared rejects `localhost` ("Invalid application domain"), and its out-of-band "PIN" page is broken (clicking *Allow* dead-ends with *"Invalid token"*). The working setup is to register a real domain you control, point it at your own machine, and let the bundled helper catch the callback locally.

**Step 1 — Point a domain at your machine (one time per account).**

Pick a subdomain of a domain you own, e.g. `backmeup.mydomainexample.com`, and make it resolve to loopback so the OAuth callback reaches the helper running locally.

- **Option A — public DNS (recommended):** in your DNS provider, add an **A record** with type `A`, host `backmeup` (i.e. `backmeup.mydomainexample.com`), and value `127.0.0.1`. Do **not** use a CNAME to your real site — the browser would follow your site's http→https/www redirects and you'd lose the callback.
- **Option B — if your DNS panel refuses a 127.0.0.1 record:** skip public DNS and add a line to your hosts file (`C:\Windows\System32\drivers\etc\hosts`, edited as Administrator): `127.0.0.1   backmeup.mydomainexample.com`.

Then set it in `.env`:
```env
FOURSHARED_ACCOUNT_1_CONSUMER_DOMAIN=backmeup.mydomainexample.com
```

**Step 2 — Register the 4shared application (one time per account).**

Sign in to the 4shared account you want to authorize, go to <https://www.4shared.com/developer>, click **My apps**, then **Create new application**, and fill the form:

- **Application title**: `BackMeUp`
- **Application description**: `Personal backup uploader`
- **Application domain**: your domain without the scheme, exactly matching Step 1 — `backmeup.mydomainexample.com`
- Leave the **Initiate / Authorize / Request token addresses** at their shown defaults (`https://api.4shared.com/v1_2/oauth/initiate`, `/authorize`, `/token`).

Click **Create**. The page now shows a **Consumer Key** and **Consumer Secret** — copy both into `.env`:
```env
FOURSHARED_ACCOUNT_1_CONSUMER_KEY=the_consumer_key_shown
FOURSHARED_ACCOUNT_1_CONSUMER_SECRET=the_consumer_secret_shown
```

**Step 3 — Authorize the account (one time per account).**

Run the bundled helper for that account number — it reads the consumer key, secret, and domain from `.env`:
```
go run ./cmd/fourshared-auth -account 1
```

The helper starts a local server on `127.0.0.1:8723` and opens your browser to the 4shared authorize page. Log in to the account and click **Allow**. 4shared redirects to `http://backmeup.mydomainexample.com:8723/callback?...`, which resolves to your machine and hits the helper; the browser shows *"BackMeUp: 4shared authorized"* and the terminal prints the two token lines:
```env
FOURSHARED_ACCOUNT_1_OAUTH_TOKEN=...
FOURSHARED_ACCOUNT_1_OAUTH_TOKEN_SECRET=...
```

Add those two lines to `.env`. Repeat Steps 1–3 with `-account 2`, `-account 3`, … (and matching `FOURSHARED_ACCOUNT_<n>_*` keys) for additional 4shared accounts.

To authorize the 4shared **main** (database-backup) account, run the same steps with `-account main`; the helper reads `FOURSHARED_ACCOUNT_MAIN_CONSUMER_KEY` / `_SECRET` / `_DOMAIN` and prints `FOURSHARED_ACCOUNT_MAIN_OAUTH_TOKEN` / `_SECRET`.

> **Note:** 4shared implements **OAuth 1.0**, not 1.0a — the authorize callback returns only `oauth_token` and **no `oauth_verifier`**, and the helper completes the moment the callback arrives.

**Flags / fallbacks:** `-port <n>` uses a different local port (the callback then uses that port too); `-manual` is a last-resort flow if you cannot use a callback at all. If several accounts share a single application, you can instead set `FOURSHARED_CONSUMER_KEY` / `FOURSHARED_CONSUMER_SECRET` / `FOURSHARED_CONSUMER_DOMAIN` once as a fallback for accounts that omit their own.

Once the consumer key/secret, the domain, and each account's token are in `.env`, restart the server and 4shared uploads will work. If a 4shared upload later fails with an authorization error, re-run the helper for that account — tokens can be revoked from the 4shared account's connected-apps settings.

> **Adding a future OAuth provider:** reuse `internal/provider/oauth1` for signing, add `<PROVIDER>_ACCOUNT_<n>_CONSUMER_KEY/SECRET/DOMAIN` plus `<PROVIDER>_ACCOUNT_<n>_OAUTH_TOKEN/_SECRET` to `.env`, and add a short authorize helper modelled on `cmd/fourshared-auth`.

## How backups upload

Creating a backup writes one `pending` job per selected account. A background worker pool (its goroutine count is `concurrency.max_workers`, with a hard ceiling of `concurrency.max_concurrent_uploads` simultaneous uploads and `concurrency.max_concurrent_per_account` per account) then runs each job:

1. Claims each pending job atomically and marks it `in_progress`.
2. Uploads the zip in chunks (`upload.chunk_size_mb`), persisting progress after each chunk — the Backups table shows a live progress bar, polled every `ui.active_poll_seconds` (default 2s) while the job runs.
3. Retries on failure with exponential backoff (`retry_policy`).
4. On success: verifies the first chunk's checksum, refreshes the account quota, deletes the temp zip (once every sibling job for that backup is done), and uploads a copy of the metadata DB to every configured main account, attempting each independently.
5. On failure (after retries): marks the job `failed`, records the error, and keeps the temp zip for a future retry.

Click **logs** in a provider column to see that job's log history (including failure reasons) in a modal.

### Rate limiting

Each provider has its own request-rate and bandwidth ceiling under `rate_limits.<provider>` in `config.yml`. The limiter is a token bucket shared by every account of that provider (the limits are per-provider, not per-account). 4shared is paced per HTTP request and per upload chunk; MEGA is paced at chunk granularity only, because the MEGA library owns its own HTTP client and its individual requests aren't interceptable. Setting a rate to `0` disables that dimension.

### Periodic re-verification

When `verification.enabled` is on, a background re-verifier wakes on an interval derived from `periodic_check_days` (about a quarter of the period, clamped to between 1 and 24 hours) and re-checks a small random sample of completed backups that haven't been re-verified within the configured number of days. It re-downloads the first chunk and compares it to the checksum captured at upload time. A pass refreshes the file's last-verified timestamp; a mismatch or download failure is recorded in that job's logs (open the **logs** modal to see it). Re-verification only reports — it never re-uploads or deletes; acting on a failure is left to you.

## Development Workflow

After each update, do the following:

- Terminal: Ctrl + c (if the application is running)
- `go run ./cmd/server`
- Open `http://localhost:8080` in your browser.
- Browser: Ctrl+Shift+R

## Troubleshooting

- **"no .env file loaded"**: Copy `.env.example` to `.env` and fill in your credentials.
- **Port already in use**: Change `server.port` in `config.yml`.
- **`go run` says module not found**: Run `go mod download` first to fetch all dependencies.
- **Accounts not showing in modal / UI looks stale after a server update**: The browser may be serving a cached version of the JavaScript. Press **Ctrl+Shift+R** (Windows/Linux) or **Cmd+Shift+R** (macOS) to force a full reload. This is a one-time step after each update — subsequent reloads are automatic because the server now sends `Cache-Control: no-store` for all static assets.
- **MEGA accounts not showing in modal**: Verify your `.env` has `MEGA_ACCOUNT_1_EMAIL` (a numbered backup account), not just `MEGA_ACCOUNT_MAIN_EMAIL`. A main account is shown in the Accounts view but is never offered as an upload target. See the account structure table above.
- **MEGA upload fails with "Object (typically, node or user) not found" at login**: MEGA reports invalid credentials this way. The usual cause is a password containing `$` (or other special characters) that was silently corrupted by `.env` variable expansion — see the next item. Otherwise confirm you can log in with that exact email and password at <https://mega.nz>, that there are no stray spaces in `.env`, and that the account does not require two-factor authentication (2FA is not currently supported).
- **A password/secret with `$`, `#`, backticks or spaces isn't accepted**: Unquoted and double-quoted `.env` values undergo variable expansion, so `PASSWORD=paSs1$2178` becomes `paSs1`. Wrap such values in **single** quotes to keep them literal: `MEGA_ACCOUNT_1_PASSWORD='paSs1$2178'`. (OAuth tokens are hex and don't need quoting.)
- **4shared upload fails with `401 ... "token ... expired, rejected or does not exist"` (code `401.0301`)**: The account's OAuth access token is no longer valid server-side. **There is no token-expiry setting in this app** — the application sets no lifetime on tokens; an OAuth 1.0 access token's validity is controlled entirely by 4shared's servers, so it cannot be extended or configured from here. A token can become invalid because 4shared expired it, because the app was re-authorized (which invalidates the previous token), or because it was revoked. 4shared does not publish the exact lifetime. The fix is always to re-mint the token, and since PR #13 you can do it without leaving the app: the account's card in the **Accounts** view shows a `needs re-authorization` badge and a **Re-authorize** button that runs the flow and stores the new token immediately (see "Re-authorizing 4shared from the app"). The command-line route still works — `go run ./cmd/fourshared-auth -account <n>`, paste the printed `FOURSHARED_ACCOUNT_<n>_OAUTH_TOKEN`/`_SECRET` into `.env`, restart — and is what you need for an account that has no stored row yet. Run `go run ./cmd/fourshared-test -account <n>` (add `FOURSHARED_DEBUG=1` for verbose signing logs) to verify a token in isolation.
- **"Delete Record And Files" reports files it could not delete**: The record is deliberately kept when some copies survive, so you don't silently orphan archives in the cloud. The modal lists each one with its provider, account, archive name and reason. Fix the cause (usually re-authorizing 4shared, see the item above) and press Delete again — copies that were already removed on the first attempt report "not found", which counts as success, so a retry converges. If you'd rather not fix it now, **Remove record anyway** drops the local record only; the listed files stay on their accounts and you'll have to delete them from the provider's own web UI.
- **A record won't delete because its file no longer exists on the provider**: Fixed. A remote file that is already gone is treated as deleted rather than as an error, so records that Auto-Sync flagged as having a missing remote copy can now be removed normally.
- **A directory I expected is missing from a zip's file tree**: Three possible causes, in order of likelihood. (1) It matches an **exclude term** — check the Settings modal; matching ignores case and accents, so `conteudo` also hides "Conteúdo". (2) It sits deeper than `scan.max_depth` (default 3 levels below the source root). (3) The tree was recorded **before** the term or depth changed — trees are captured at upload time and never rewritten, so re-upload the record to refresh it. Note that in every case the directory is still **inside the uploaded zip**; only the recorded tree is filtered.
- **An exclude term emptied the whole tree**: A blank or whitespace-only term would match every directory name, so blanks are rejected on save and ignored on read. If a tree is unexpectedly empty, check the saved terms with `sqlite3 backmeup.db "select * from settings;"` — a very short term like `a` matches far more than intended, since matching is substring-based.
- **Global search finds nothing for a folder I know exists**: The ▶ button searches **directory names inside recorded trees** — not file names (individual files aren't recorded) and not trees recorded before this feature shipped in their old 2-level form. Typing in the box filters the visible table in real time; the ▶ button is the one that runs the global search.
- **"database is locked (SQLITE_BUSY)" in the logs**: Fixed — the database connection pool is pinned to a single connection so concurrent workers serialize instead of contending. If you still see it, make sure no other process (e.g. a second `go run ./cmd/server`) has the same `backmeup.db` open.
