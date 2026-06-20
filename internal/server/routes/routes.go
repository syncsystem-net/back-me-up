package routes

import (
	"database/sql"
	"net/http"

	"github.com/syncsystem-net/back-me-up/internal/server/handlers"
)

func Register(mux *http.ServeMux, h *handlers.Handlers, db *sql.DB) {
	staticFS := http.StripPrefix("/static/", http.FileServer(http.Dir("web/static")))
	mux.Handle("/static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		staticFS.ServeHTTP(w, r)
	}))

	mux.HandleFunc("/", h.Home)

	mux.HandleFunc("/api/health", h.Health)
	mux.HandleFunc("/api/accounts", handlers.GetAccountsHandler(db))
	mux.HandleFunc("/api/browse", handlers.BrowseHandler())
	mux.HandleFunc("/api/backups", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			handlers.GetBackupsHandler(db)(w, r)
		case http.MethodPost:
			h.PostBackups(w, r)
		default:
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("GET /api/jobs/{id}/logs", handlers.GetJobLogsHandler(db))
	mux.HandleFunc("GET /api/jobs/{id}/download", h.DownloadJob)
	mux.HandleFunc("DELETE /api/jobs/{id}", h.DeleteJob)
}
