# BackMeUp

Backup tool that zips local directories and uploads them to cloud storage providers (MEGA, 4shared). Manage multiple accounts, track upload jobs, and verify file integrity.

## Prerequisites

- Go 1.21+
- [Air](https://github.com/air-verse/air) for live reload (optional)

> No C compiler (GCC/CGO) required. The project uses a pure-Go SQLite driver.

## First-time Setup

1. Copy `.env.example` to `.env` and configure your accounts:
   ```
   cp .env.example .env
   ```

2. Edit `config.yml` to adjust settings (chunk size, retry policy, rate limits, etc.).

3. Download dependencies:
   ```
   go mod download
   ```

4. Run the server:
   ```
   go run ./cmd/server
   ```
   Or with Air for live reload:
   ```
   air
   ```

5. Open `http://localhost:8080` in your browser.

## Project Structure

```
cmd/server/            - Application entry point (HTTP server + worker pool)
cmd/fourshared-auth/   - One-time OAuth helper to authorize a 4shared account
internal/
  config/              - YAML configuration loader
  accounts/            - .env account parser (passwords + OAuth app/tokens)
  database/            - SQLite setup, schema, and per-entity queries
  scanner/             - Directory scanner (2 levels deep)
  archive/             - Zip creation
  worker/              - Background upload pool: claim, retry, verify, DB backup
  provider/            - Cloud provider interface + Progress/OAuth types
    mega/              - MEGA implementation (chunked, encrypted)
    fourshared/        - 4shared implementation (OAuth 1.0 REST)
    oauth1/            - Reusable OAuth 1.0a request signing
    registry/          - Maps provider name -> implementation
  server/              - HTTP server, routes, handlers
web/
  templates/           - HTML templates (Alpine.js)
  static/              - CSS, JS assets
```

## .env Account Structure

The `.env` file has two distinct account types:

| Variable prefix | Purpose | Shown in UI |
|---|---|---|
| `MAIN_ACCOUNT_*` | Receives a copy of the SQLite DB after every successful job | No |
| `MEGA_ACCOUNT_1_*`, `MEGA_ACCOUNT_2_*`, … | Accounts available for user-selected backups | Yes |
| `FOURSHARED_ACCOUNT_1_*`, … | Same for 4shared | Yes |

Example — one MEGA backup account plus one 4shared:
```env
MAIN_ACCOUNT_PROVIDER=mega
MAIN_ACCOUNT_EMAIL=db-backup@example.com
MAIN_ACCOUNT_PASSWORD=secret

MEGA_ACCOUNT_1_EMAIL=uploads@example.com
MEGA_ACCOUNT_1_PASSWORD=secret
MEGA_ACCOUNT_1_QUOTA_GB=20

FOURSHARED_ACCOUNT_1_EMAIL=uploads@4shared.com
FOURSHARED_ACCOUNT_1_PASSWORD=secret
FOURSHARED_ACCOUNT_1_QUOTA_GB=15
```

The `MAIN_ACCOUNT` is intentionally excluded from the backup modal — it is reserved for database backup only.

## Provider credentials

Each cloud provider authenticates differently. This section documents, per provider, what credentials you need and how to obtain them. When a new provider is added it gets its own subsection here following the same shape: how it authenticates → what to register → which `.env` keys to set.

### MEGA (password)

MEGA authenticates with the account email and password directly. No app registration is required — set `MEGA_ACCOUNT_<n>_EMAIL` / `_PASSWORD` / `_QUOTA_GB` and you are done. The same email/password must work when you log in at <https://mega.nz>; accounts protected with two-factor authentication are not currently supported.

### 4shared (OAuth 1.0)

4shared's API does **not** accept a plain email/password. It uses OAuth: a per-account application **consumer key/secret** plus a per-account **access token** that you authorize once. The email/password in `.env` are kept only for display. Each 4shared account is authorized through its own registered application, so the consumer key/secret, callback domain, and tokens are all configured per account under the `FOURSHARED_ACCOUNT_<n>_*` keys.

Authorization needs a **callback domain**. 4shared rejects `localhost` ("Invalid application domain"), and its out-of-band "PIN" page is broken (clicking *Allow* dead-ends with *"Invalid token"*). The working setup is to register a real domain you control, point it at your own machine, and let the bundled helper catch the callback locally.

**Step 1 — Point a domain at your machine (one time per account).**

Pick a subdomain of a domain you own, e.g. `backmeup.syncsystem.net`, and make it resolve to loopback so the OAuth callback reaches the helper running locally.

- **Option A — public DNS (recommended):** in your DNS provider, add an **A record** with type `A`, host `backmeup` (i.e. `backmeup.syncsystem.net`), and value `127.0.0.1`. Do **not** use a CNAME to your real site — the browser would follow your site's http→https/www redirects and you'd lose the callback.
- **Option B — if your DNS panel refuses a 127.0.0.1 record:** skip public DNS and add a line to your hosts file (`C:\Windows\System32\drivers\etc\hosts`, edited as Administrator): `127.0.0.1   backmeup.syncsystem.net`.

Then set it in `.env`:
```env
FOURSHARED_ACCOUNT_1_CONSUMER_DOMAIN=backmeup.syncsystem.net
```

**Step 2 — Register the 4shared application (one time per account).**

Sign in to the 4shared account you want to authorize, go to <https://www.4shared.com/developer>, click **My apps**, then **Create new application**, and fill the form:

- **Application title**: `BackMeUp`
- **Application description**: `Personal backup uploader`
- **Application domain**: your domain without the scheme, exactly matching Step 1 — `backmeup.syncsystem.net`
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

The helper starts a local server on `127.0.0.1:8723` and opens your browser to the 4shared authorize page. Log in to the account and click **Allow**. 4shared redirects to `http://backmeup.syncsystem.net:8723/callback?...`, which resolves to your machine and hits the helper; the browser shows *"BackMeUp: 4shared authorized"* and the terminal prints the two token lines:
```env
FOURSHARED_ACCOUNT_1_OAUTH_TOKEN=...
FOURSHARED_ACCOUNT_1_OAUTH_TOKEN_SECRET=...
```

Add those two lines to `.env`. Repeat Steps 1–3 with `-account 2`, `-account 3`, … (and matching `FOURSHARED_ACCOUNT_<n>_*` keys) for additional 4shared accounts.

> **Note:** 4shared implements **OAuth 1.0**, not 1.0a — the authorize callback returns only `oauth_token` and **no `oauth_verifier`**, and the helper completes the moment the callback arrives.

**Flags / fallbacks:** `-port <n>` uses a different local port (the callback then uses that port too); `-manual` is a last-resort flow if you cannot use a callback at all. If several accounts share a single application, you can instead set `FOURSHARED_CONSUMER_KEY` / `FOURSHARED_CONSUMER_SECRET` / `FOURSHARED_CONSUMER_DOMAIN` once as a fallback for accounts that omit their own.

Once the consumer key/secret, the domain, and each account's token are in `.env`, restart the server and 4shared uploads will work. If a 4shared upload later fails with an authorization error, re-run the helper for that account — tokens can be revoked from the 4shared account's connected-apps settings.

> **Adding a future OAuth provider:** reuse `internal/provider/oauth1` for signing, add `<PROVIDER>_ACCOUNT_<n>_CONSUMER_KEY/SECRET/DOMAIN` plus `<PROVIDER>_ACCOUNT_<n>_OAUTH_TOKEN/_SECRET` to `.env`, and add a short authorize helper modelled on `cmd/fourshared-auth`.

## How backups upload

Creating a backup writes one `pending` job per selected account. A background worker pool (its goroutine count is `concurrency.max_workers`, with a hard ceiling of `concurrency.max_concurrent_uploads` simultaneous uploads and `concurrency.max_concurrent_per_account` per account) then runs each job:

1. Claims each pending job atomically and marks it `in_progress`.
2. Uploads the zip in chunks (`upload.chunk_size_mb`), persisting progress after each chunk — the Backups table shows a live progress bar, polled every 2s.
3. Retries on failure with exponential backoff (`retry_policy`).
4. On success: verifies the first chunk's checksum, refreshes the account quota, deletes the temp zip (once every sibling job for that backup is done), and uploads a copy of the metadata DB to the main account.
5. On failure (after retries): marks the job `failed`, records the error, and keeps the temp zip for a future retry.

Click **logs** in a provider column to see that job's log history (including failure reasons) in a modal.

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
- **MEGA accounts not showing in modal**: Verify your `.env` has `MEGA_ACCOUNT_1_EMAIL` (a numbered backup account), not just `MAIN_ACCOUNT_EMAIL`. The main account is not displayed in the UI. See the account structure table above.
- **MEGA upload fails with "Object (typically, node or user) not found" at login**: MEGA reports invalid credentials this way. The usual cause is a password containing `$` (or other special characters) that was silently corrupted by `.env` variable expansion — see the next item. Otherwise confirm you can log in with that exact email and password at <https://mega.nz>, that there are no stray spaces in `.env`, and that the account does not require two-factor authentication (2FA is not currently supported).
- **A password/secret with `$`, `#`, backticks or spaces isn't accepted**: Unquoted and double-quoted `.env` values undergo variable expansion, so `PASSWORD=paSs1$2178` becomes `paSs1`. Wrap such values in **single** quotes to keep them literal: `MEGA_ACCOUNT_1_PASSWORD='paSs1$2178'`. (OAuth tokens are hex and don't need quoting.)
- **4shared upload fails with `401 ... "token ... does not exist"`**: The account's OAuth access token is stale. Re-authorizing an app invalidates its previous token, so re-run `go run ./cmd/fourshared-auth -account <n>` and paste the freshly printed `FOURSHARED_ACCOUNT_<n>_OAUTH_TOKEN`/`_SECRET` into `.env`. Run `go run ./cmd/fourshared-test -account <n>` (add `FOURSHARED_DEBUG=1` for verbose signing logs) to verify a token in isolation.
- **"database is locked (SQLITE_BUSY)" in the logs**: Fixed — the database connection pool is pinned to a single connection so concurrent workers serialize instead of contending. If you still see it, make sure no other process (e.g. a second `go run ./cmd/server`) has the same `backmeup.db` open.
