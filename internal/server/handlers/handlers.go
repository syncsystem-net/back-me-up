package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/syncsystem-net/back-me-up/internal/accounts"
	"github.com/syncsystem-net/back-me-up/internal/archive"
	"github.com/syncsystem-net/back-me-up/internal/cloud"
	"github.com/syncsystem-net/back-me-up/internal/database"
	"github.com/syncsystem-net/back-me-up/internal/quota"
	"github.com/syncsystem-net/back-me-up/internal/scanner"
)

type Handlers struct {
	db        *sql.DB
	accounts  *accounts.AccountStore
	tmpl      *template.Template
	chunkSize int64
}

func New(db *sql.DB, accts *accounts.AccountStore, chunkSize int64) *Handlers {
	tmplPath := filepath.Join("web", "templates", "*.html")
	tmpl, err := template.ParseGlob(tmplPath)
	if err != nil {
		slog.Error("failed to parse templates", "error", err)
		tmpl = template.New("")
	}

	return &Handlers{
		db:        db,
		accounts:  accts,
		tmpl:      tmpl,
		chunkSize: chunkSize,
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

// backupResponse is the JSON shape the frontend row template renders: a backup
// with its directories and provider jobs attached. Shared by /api/backups and
// /api/search so search results render with the same template.
type backupResponse struct {
	ID          int64                 `json:"id"`
	Title       string                `json:"title"`
	SourcePath  string                `json:"source_path"`
	CreatedAt   time.Time             `json:"created_at"`
	Directories []*database.Directory `json:"directories"`
	Jobs        []*database.Job       `json:"jobs"`
}

// assembleBackups attaches each backup's directories and jobs, producing the
// response shape the UI expects. A query/scan failure is returned so the caller
// can emit an HTTP error.
func assembleBackups(db *sql.DB, backups []*database.Backup) ([]backupResponse, error) {
	result := make([]backupResponse, 0, len(backups))
	for _, b := range backups {
		dirs, err := database.ListDirectoriesByBackup(db, b.ID)
		if err != nil {
			return nil, fmt.Errorf("listing directories for backup %d: %w", b.ID, err)
		}
		jobs, err := database.ListJobsByBackup(db, b.ID)
		if err != nil {
			return nil, fmt.Errorf("listing jobs for backup %d: %w", b.ID, err)
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
	return result, nil
}

func GetBackupsHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		backups, err := database.ListBackups(db)
		if err != nil {
			slog.Error("listing backups", "error", err)
			jsonError(w, "failed to list backups", http.StatusInternalServerError)
			return
		}

		result, err := assembleBackups(db, backups)
		if err != nil {
			slog.Error("assembling backups", "error", err)
			jsonError(w, "failed to assemble backups", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
	}
}

// SearchBackupsHandler returns backups that have a subdirectory name matching
// q, in the same JSON shape as /api/backups so the UI reuses the row template.
// An empty q returns an empty list (the client falls back to the in-memory
// list). Route: GET /api/search?q=.
func SearchBackupsHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := strings.TrimSpace(r.URL.Query().Get("q"))
		w.Header().Set("Content-Type", "application/json")
		if q == "" {
			json.NewEncoder(w).Encode([]backupResponse{})
			return
		}
		backups, err := database.SearchBackupsByDirectory(db, q)
		if err != nil {
			slog.Error("searching backups", "q", q, "error", err)
			jsonError(w, "failed to search backups", http.StatusInternalServerError)
			return
		}
		result, err := assembleBackups(db, backups)
		if err != nil {
			slog.Error("assembling search results", "error", err)
			jsonError(w, "failed to assemble search results", http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(result)
	}
}

// QuotaSyncHandler triggers an immediate quota refresh for every numbered
// account and returns the updated account list. Route: POST
// /api/accounts/quota-sync.
func QuotaSyncHandler(db *sql.DB, syncer *quota.Syncer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if syncer != nil {
			syncer.SyncAll(r.Context())
		}
		accts, err := database.ListDBAccounts(db)
		if err != nil {
			slog.Error("listing accounts after quota sync", "error", err)
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

// conflictInfo describes a same-name file already present on a selected account.
type conflictInfo struct {
	AccountID int64  `json:"account_id"`
	Provider  string `json:"provider"`
	Email     string `json:"email"`
	Name      string `json:"name"`
}

func (h *Handlers) PostBackups(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title      string  `json:"title"`
		SourcePath string  `json:"source_path"`
		AccountIDs []int64 `json:"account_ids"`
		// ConflictResolutions maps an account id (as a string key, since JSON
		// object keys are strings) to "overwrite" or "skip". Sent on resubmit
		// after the user resolves the conflicts reported by a prior 409.
		ConflictResolutions map[string]string `json:"conflict_resolutions"`
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

	// Drop accounts the user chose to skip; the rest are the effective upload
	// targets for quota, conflict detection, and job creation.
	effective := make([]int64, 0, len(req.AccountIDs))
	for _, id := range req.AccountIDs {
		if req.ConflictResolutions[strconv.FormatInt(id, 10)] == "skip" {
			continue
		}
		effective = append(effective, id)
	}
	if len(effective) == 0 {
		jsonError(w, "all selected accounts were skipped; nothing to upload", http.StatusBadRequest)
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
	remoteName := archive.RemoteName(req.SourcePath)

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
	if msg, ok := checkQuota(h.db, effective, totalBytes); !ok {
		os.Remove(zipPath)
		jsonError(w, msg, http.StatusConflict)
		return
	}

	// Detect same-name files already on each target account. Unresolved
	// conflicts are reported back (409) so the user can choose overwrite/skip;
	// "overwrite" deletes the existing remote here so the new upload replaces it.
	// Detection failures (login/network) are non-fatal: we log and proceed,
	// leaving the upload worker as the source of truth.
	if conflicts := h.resolveConflicts(r.Context(), effective, remoteName, req.ConflictResolutions); len(conflicts) > 0 {
		os.Remove(zipPath)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]any{
			"error":     "some accounts already have a file with this name",
			"conflicts": conflicts,
		})
		return
	}

	tx, err := h.db.Begin()
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

	for _, accountID := range effective {
		if _, err := database.InsertJob(tx, backupID, accountID, zipPath, remoteName, totalBytes); err != nil {
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

// resolveConflicts checks each target account's cloud root for an existing file
// named remoteName. For an account the user marked "overwrite" it deletes the
// existing file so the upload replaces it; an account with an existing file and
// no resolution is returned as a conflict for the UI to prompt on. Connection or
// lookup failures are logged and treated as "no conflict".
func (h *Handlers) resolveConflicts(ctx context.Context, accountIDs []int64, remoteName string, resolutions map[string]string) []conflictInfo {
	var conflicts []conflictInfo
	for _, id := range accountIDs {
		acct, err := database.GetDBAccountByID(h.db, id)
		if err != nil {
			slog.Warn("conflict check: account not found", "account_id", id, "error", err)
			continue
		}
		p, err := cloud.Connect(ctx, h.accounts, acct.Provider, acct.Email, h.chunkSize)
		if err != nil {
			slog.Warn("conflict check: could not connect", "provider", acct.Provider, "email", acct.Email, "error", err)
			continue
		}
		ref, found, err := p.FindByName(ctx, remoteName)
		if err != nil {
			slog.Warn("conflict check: lookup failed", "provider", acct.Provider, "email", acct.Email, "error", err)
			continue
		}
		if !found {
			continue
		}
		switch resolutions[strconv.FormatInt(id, 10)] {
		case "overwrite":
			if err := p.Delete(ctx, ref); err != nil {
				slog.Warn("conflict overwrite: delete failed", "provider", acct.Provider, "email", acct.Email, "error", err)
			}
		default:
			conflicts = append(conflicts, conflictInfo{
				AccountID: id,
				Provider:  acct.Provider,
				Email:     acct.Email,
				Name:      remoteName,
			})
		}
	}
	return conflicts
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

// DownloadJob streams the remote zip for a completed job back to the browser.
// Route: GET /api/jobs/{id}/download.
func (h *Handlers) DownloadJob(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		jsonError(w, "invalid job id", http.StatusBadRequest)
		return
	}
	job, err := database.GetJob(h.db, id)
	if err != nil {
		jsonError(w, "job not found", http.StatusNotFound)
		return
	}
	if job.Status != "complete" || job.RemotePath == "" {
		jsonError(w, "job has no uploaded file to download", http.StatusConflict)
		return
	}

	p, err := cloud.Connect(r.Context(), h.accounts, job.Provider, job.Email, h.chunkSize)
	if err != nil {
		slog.Error("download: connect failed", "job", id, "error", err)
		jsonError(w, "could not connect to provider", http.StatusBadGateway)
		return
	}

	filename := job.RemoteName
	if filename == "" {
		filename = filepath.Base(job.ZipPath)
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, filename))

	if err := p.Download(r.Context(), job.RemotePath, w); err != nil {
		// Headers (and likely some bytes) are already sent, so we can't switch to
		// a JSON error here; log it and let the client see a truncated download.
		slog.Error("download stream failed", "job", id, "error", err)
	}
}

// DeleteJob removes a job's file from its provider and deletes the job record.
// It requires a JSON body {"confirm":"DELETE"} (also enforced in the UI). Only
// that provider's copy is affected — the backup record, its directories, and any
// sibling provider's job remain. Route: DELETE /api/jobs/{id}.
func (h *Handlers) DeleteJob(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		jsonError(w, "invalid job id", http.StatusBadRequest)
		return
	}
	var body struct {
		Confirm string `json:"confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if body.Confirm != "DELETE" {
		jsonError(w, `confirmation must be exactly "DELETE"`, http.StatusBadRequest)
		return
	}

	job, err := database.GetJob(h.db, id)
	if err != nil {
		jsonError(w, "job not found", http.StatusNotFound)
		return
	}

	// Remove the remote file first. If it fails, keep the record so the user can
	// retry rather than orphaning a file in the cloud. A job that never uploaded
	// (no remote_path) skips straight to record deletion.
	if job.RemotePath != "" {
		p, err := cloud.Connect(r.Context(), h.accounts, job.Provider, job.Email, h.chunkSize)
		if err != nil {
			slog.Error("delete: connect failed", "job", id, "error", err)
			jsonError(w, "could not connect to provider", http.StatusBadGateway)
			return
		}
		if err := p.Delete(r.Context(), job.RemotePath); err != nil {
			slog.Error("delete: remote delete failed", "job", id, "error", err)
			jsonError(w, "failed to delete file from provider", http.StatusBadGateway)
			return
		}
	}

	if err := database.DeleteJob(h.db, id); err != nil {
		slog.Error("delete: removing job record failed", "job", id, "error", err)
		jsonError(w, "deleted from provider but failed to remove record", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
