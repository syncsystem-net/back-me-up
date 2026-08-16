package routes

import (
	"database/sql"
	"net/http"

	"github.com/syncsystem-net/back-me-up/internal/autosync"
	"github.com/syncsystem-net/back-me-up/internal/quota"
	"github.com/syncsystem-net/back-me-up/internal/reauth"
	"github.com/syncsystem-net/back-me-up/internal/server/handlers"
)

func Register(mux *http.ServeMux, h *handlers.Handlers, db *sql.DB, syncer *quota.Syncer, sync *autosync.Manager, reauthMgr *reauth.Manager) {
	staticFS := http.StripPrefix("/static/", http.FileServer(http.Dir("web/static")))
	mux.Handle("/static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		staticFS.ServeHTTP(w, r)
	}))

	mux.HandleFunc("/", h.Home)

	mux.HandleFunc("/api/health", h.Health)
	mux.HandleFunc("/api/accounts", handlers.GetAccountsHandler(db))
	mux.HandleFunc("GET /api/accounts/main", h.GetMainAccounts)
	mux.HandleFunc("GET /api/credentials", h.GetCredentialsStatus)
	mux.HandleFunc("GET /api/reauth", handlers.ReauthStatusHandler(reauthMgr))
	mux.HandleFunc("POST /api/reauth/start", handlers.ReauthStartHandler(reauthMgr))
	mux.HandleFunc("POST /api/reauth/cancel", handlers.ReauthCancelHandler(reauthMgr))
	mux.HandleFunc("POST /api/accounts/quota-sync", h.QuotaSync(syncer))
	mux.HandleFunc("GET /api/users", handlers.GetUsersHandler(db))
	mux.HandleFunc("GET /api/search", handlers.SearchTreesHandler(db))
	mux.HandleFunc("GET /api/settings", handlers.GetSettingsHandler(db))
	mux.HandleFunc("PUT /api/settings", handlers.PutSettingsHandler(db))
	mux.HandleFunc("GET /api/autosync", handlers.AutoSyncStatusHandler(sync))
	mux.HandleFunc("POST /api/autosync/preview", handlers.AutoSyncPreviewHandler(sync))
	mux.HandleFunc("POST /api/autosync/apply", handlers.AutoSyncApplyHandler(sync))
	mux.HandleFunc("POST /api/autosync/cancel", handlers.AutoSyncCancelHandler(sync))
	mux.HandleFunc("/api/browse", handlers.BrowseHandler())
	mux.HandleFunc("POST /api/backups", h.PostBackups)
	mux.HandleFunc("PATCH /api/backups/{id}", handlers.PatchBackupHandler(db))
	mux.HandleFunc("DELETE /api/backups/{id}", h.DeleteBackup)
	mux.HandleFunc("GET /api/jobs/{id}/logs", handlers.GetJobLogsHandler(db))
	mux.HandleFunc("GET /api/jobs/{id}/download", h.DownloadJob)
	mux.HandleFunc("DELETE /api/jobs/{id}", h.DeleteJob)
}
