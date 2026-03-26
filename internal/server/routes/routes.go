package routes

import (
	"net/http"

	"github.com/syncsystem-net/back-me-up/internal/server/handlers"
)

func Register(mux *http.ServeMux, h *handlers.Handlers) {
	// Static files
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("web/static"))))

	// Pages
	mux.HandleFunc("/", h.Home)

	// API
	mux.HandleFunc("/api/health", h.Health)
}
