package server

import (
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/syncsystem-net/back-me-up/internal/accounts"
	"github.com/syncsystem-net/back-me-up/internal/config"
	"github.com/syncsystem-net/back-me-up/internal/server/handlers"
	"github.com/syncsystem-net/back-me-up/internal/server/routes"
)

type Server struct {
	cfg      *config.Config
	db       *sql.DB
	accounts *accounts.AccountStore
	mux      *http.ServeMux
}

func New(cfg *config.Config, db *sql.DB, accts *accounts.AccountStore) *Server {
	s := &Server{
		cfg:      cfg,
		db:       db,
		accounts: accts,
		mux:      http.NewServeMux(),
	}

	h := handlers.New(db, accts)
	routes.Register(s.mux, h, db)

	return s
}

func (s *Server) Start() error {
	addr := fmt.Sprintf("%s:%d", s.cfg.Server.Host, s.cfg.Server.Port)
	slog.Info("server starting", "address", addr)
	return http.ListenAndServe(addr, s.mux)
}
