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

## Troubleshooting

- **"no .env file loaded"**: Copy `.env.example` to `.env` and fill in your credentials.
- **Port already in use**: Change `server.port` in `config.yml`.
- **`go run` says module not found**: Run `go mod download` first to fetch all dependencies.
