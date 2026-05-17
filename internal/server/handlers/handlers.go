package handlers

import (
	"database/sql"
	"html/template"
	"log/slog"
	"net/http"
	"path/filepath"

	"github.com/syncsystem-net/back-me-up/internal/accounts"
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
