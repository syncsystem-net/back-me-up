package routes

import (
	"database/sql"
	"net/http"

	"github.com/syncsystem-net/back-me-up/internal/server/handlers"
)

func Register(mux *http.ServeMux, h *handlers.Handlers, db *sql.DB) {
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("web/static"))))

	mux.HandleFunc("/", h.Home)

	mux.HandleFunc("/api/health", h.Health)
	mux.HandleFunc("/api/accounts", handlers.GetAccountsHandler(db))
	mux.HandleFunc("/api/browse", handlers.BrowseHandler())
	mux.HandleFunc("/api/backups", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			handlers.GetBackupsHandler(db)(w, r)
		case http.MethodPost:
			handlers.PostBackupsHandler(db)(w, r)
		default:
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		}
	})
}
