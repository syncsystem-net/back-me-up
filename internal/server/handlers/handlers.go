package handlers

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/syncsystem-net/back-me-up/internal/accounts"
	"github.com/syncsystem-net/back-me-up/internal/archive"
	"github.com/syncsystem-net/back-me-up/internal/database"
	"github.com/syncsystem-net/back-me-up/internal/scanner"
)

type Handlers struct {
	db       *sql.DB
	accounts *accounts.AccountStore
	tmpl     *template.Template
}

func New(db *sql.DB, accts *accounts.AccountStore) *Handlers {
	tmplPath := filepath.Join("web", "templates", "*.html")
	tmpl, err := template.ParseGlob(tmplPath)
	if err != nil {
		slog.Error("failed to parse templates", "error", err)
		tmpl = template.New("")
	}

	return &Handlers{
		db:       db,
		accounts: accts,
		tmpl:     tmpl,
	}
}

func (h *Handlers) Home(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	data := map[string]any{
		"Title": "BackMeUp",
	}
	if err := h.tmpl.ExecuteTemplate(w, "index.html", data); err != nil {
		slog.Error("template error", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

func (h *Handlers) Health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"ok"}`))
}

func GetAccountsHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accts, err := database.ListDBAccounts(db)
		if err != nil {
			slog.Error("listing accounts", "error", err)
			jsonError(w, "failed to list accounts", http.StatusInternalServerError)
			return
		}
		if accts == nil {
			accts = []*database.DBAccount{}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(accts)
	}
}

func GetBackupsHandler(db *sql.DB) http.HandlerFunc {
	type backupResponse struct {
		ID          int64                 `json:"id"`
		Title       string                `json:"title"`
		SourcePath  string                `json:"source_path"`
		CreatedAt   time.Time             `json:"created_at"`
		Directories []*database.Directory `json:"directories"`
		Jobs        []*database.Job       `json:"jobs"`
	}

	return func(w http.ResponseWriter, r *http.Request) {
		backups, err := database.ListBackups(db)
		if err != nil {
			slog.Error("listing backups", "error", err)
			jsonError(w, "failed to list backups", http.StatusInternalServerError)
			return
		}

		result := make([]backupResponse, 0, len(backups))
		for _, b := range backups {
			dirs, err := database.ListDirectoriesByBackup(db, b.ID)
			if err != nil {
				slog.Error("listing directories", "backup_id", b.ID, "error", err)
				jsonError(w, "failed to list directories", http.StatusInternalServerError)
				return
			}
			jobs, err := database.ListJobsByBackup(db, b.ID)
			if err != nil {
				slog.Error("listing jobs", "backup_id", b.ID, "error", err)
				jsonError(w, "failed to list jobs", http.StatusInternalServerError)
				return
			}
			if dirs == nil {
				dirs = []*database.Directory{}
			}
			if jobs == nil {
				jobs = []*database.Job{}
			}
			result = append(result, backupResponse{
				ID:          b.ID,
				Title:       b.Title,
				SourcePath:  b.SourcePath,
				CreatedAt:   b.CreatedAt,
				Directories: dirs,
				Jobs:        jobs,
			})
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
	}
}

func PostBackupsHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Title      string  `json:"title"`
			SourcePath string  `json:"source_path"`
			AccountIDs []int64 `json:"account_ids"`
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "invalid request body", http.StatusBadRequest)
			return
		}

		if req.Title == "" {
			jsonError(w, "title is required", http.StatusBadRequest)
			return
		}

		info, err := os.Stat(req.SourcePath)
		if err != nil || !info.IsDir() {
			jsonError(w, "source_path must be an existing directory", http.StatusBadRequest)
			return
		}

		if len(req.AccountIDs) == 0 {
			jsonError(w, "at least one account_id is required", http.StatusBadRequest)
			return
		}

		entries, err := scanner.Scan(req.SourcePath)
		if err != nil {
			slog.Error("scanning directory", "path", req.SourcePath, "error", err)
			jsonError(w, "failed to scan source directory", http.StatusInternalServerError)
			return
		}

		zipPath, err := archive.Zip(req.SourcePath)
		if err != nil {
			slog.Error("creating zip", "path", req.SourcePath, "error", err)
			jsonError(w, "failed to create zip archive", http.StatusInternalServerError)
			return
		}

		zipInfo, err := os.Stat(zipPath)
		if err != nil {
			os.Remove(zipPath)
			jsonError(w, "failed to stat zip file", http.StatusInternalServerError)
			return
		}
		totalBytes := zipInfo.Size()

		// Quota pre-check: refuse the backup (and discard the zip) if any selected
		// account lacks the free space to hold it, so we never start an upload
		// that is doomed to fail partway. Accounts with unknown quota (0) are
		// skipped rather than blocking the user.
		if msg, ok := checkQuota(db, req.AccountIDs, totalBytes); !ok {
			os.Remove(zipPath)
			jsonError(w, msg, http.StatusConflict)
			return
		}

		tx, err := db.Begin()
		if err != nil {
			os.Remove(zipPath)
			jsonError(w, "failed to begin transaction", http.StatusInternalServerError)
			return
		}

		backupID, err := database.CreateBackup(tx, req.Title, req.SourcePath)
		if err != nil {
			tx.Rollback()
			os.Remove(zipPath)
			slog.Error("creating backup record", "error", err)
			jsonError(w, "failed to create backup", http.StatusInternalServerError)
			return
		}

		dirs := make([]database.Directory, 0, len(entries))
		for _, e := range entries {
			dirs = append(dirs, database.Directory{
				Path:      e.Path,
				Name:      e.Name,
				Level:     e.Level,
				SizeBytes: e.SizeBytes,
			})
		}

		if err := database.InsertDirectories(tx, backupID, dirs); err != nil {
			tx.Rollback()
			os.Remove(zipPath)
			slog.Error("inserting directories", "error", err)
			jsonError(w, "failed to insert directories", http.StatusInternalServerError)
			return
		}

		for _, accountID := range req.AccountIDs {
			if _, err := database.InsertJob(tx, backupID, accountID, zipPath, totalBytes); err != nil {
				tx.Rollback()
				os.Remove(zipPath)
				slog.Error("inserting job", "account_id", accountID, "error", err)
				jsonError(w, "failed to insert job", http.StatusInternalServerError)
				return
			}
		}

		if err := tx.Commit(); err != nil {
			tx.Rollback()
			os.Remove(zipPath)
			slog.Error("committing transaction", "error", err)
			jsonError(w, "failed to commit transaction", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]int64{"id": backupID})
	}
}

// checkQuota verifies each selected account has room for sizeBytes. It returns
// ("", true) when every account fits (or has unknown quota), or a human-readable
// message and false on the first account that does not fit.
func checkQuota(db *sql.DB, accountIDs []int64, sizeBytes int64) (string, bool) {
	const bytesPerGB = 1 << 30
	for _, id := range accountIDs {
		acct, err := database.GetDBAccountByID(db, id)
		if err != nil {
			return "selected account not found", false
		}
		if acct.QuotaTotalGB <= 0 {
			continue // quota unknown; don't block
		}
		availableBytes := int64((acct.QuotaTotalGB - acct.QuotaUsedGB) * bytesPerGB)
		if sizeBytes > availableBytes {
			return fmt.Sprintf(
				"%s account %s does not have enough space: backup is %.2f GB but only %.2f GB is free",
				acct.Provider, acct.Email,
				float64(sizeBytes)/bytesPerGB, float64(availableBytes)/bytesPerGB,
			), false
		}
	}
	return "", true
}

// GetJobLogsHandler returns the log lines for a single job, powering the
// per-provider "logs" modal. Route: GET /api/jobs/{id}/logs.
func GetJobLogsHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			jsonError(w, "invalid job id", http.StatusBadRequest)
			return
		}
		logs, err := database.ListJobLogs(db, id)
		if err != nil {
			slog.Error("listing job logs", "job", id, "error", err)
			jsonError(w, "failed to list job logs", http.StatusInternalServerError)
			return
		}
		if logs == nil {
			logs = []*database.JobLog{}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(logs)
	}
}

func BrowseHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path, err := openFolderDialog()
		if err != nil {
			slog.Warn("folder picker error", "error", err)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"path": path})
	}
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
