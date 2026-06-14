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
cmd/server/          - Application entry point
internal/
  config/            - YAML configuration loader
  accounts/          - .env account parser
  database/          - SQLite setup and schema
  provider/          - Cloud provider interface
  server/            - HTTP server, routes, handlers
web/
  templates/         - HTML templates (Alpine.js)
  static/            - CSS, JS assets
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
