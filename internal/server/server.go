package server

import (
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/syncsystem-net/back-me-up/internal/accounts"
	"github.com/syncsystem-net/back-me-up/internal/autosync"
	"github.com/syncsystem-net/back-me-up/internal/config"
	"github.com/syncsystem-net/back-me-up/internal/credentials"
	"github.com/syncsystem-net/back-me-up/internal/quota"
	"github.com/syncsystem-net/back-me-up/internal/reauth"
	"github.com/syncsystem-net/back-me-up/internal/server/handlers"
	"github.com/syncsystem-net/back-me-up/internal/server/routes"
)

type Server struct {
	cfg      *config.Config
	db       *sql.DB
	accounts *accounts.AccountStore
	syncer   *quota.Syncer
	mux      *http.ServeMux
}

func New(cfg *config.Config, db *sql.DB, creds *credentials.Manager, syncer *quota.Syncer) *Server {
	accts := creds.Store()
	s := &Server{
		cfg:      cfg,
		db:       db,
		accounts: accts,
		syncer:   syncer,
		mux:      http.NewServeMux(),
	}

	chunkSize := int64(cfg.Upload.ChunkSizeMB) << 20
	h := handlers.New(db, creds, chunkSize, cfg.Scan.MaxDepth, cfg.Archive.SplitMethod, handlers.UI{
		PollSeconds:       cfg.UI.PollSeconds,
		ActivePollSeconds: cfg.UI.ActivePollSeconds,
	})
	// One Manager for the process: Auto-Sync is a single global action over every
	// configured account, and it holds the in-flight run's state between the
	// preview request and the apply the user confirms.
	syncMgr := autosync.New(db, accts, chunkSize, cfg.Scan.MaxDepth)
	// Likewise one re-authorization Manager: the flow binds a fixed callback port
	// that the provider's registered application knows about, so two concurrent
	// runs cannot both have it.
	reauthMgr := reauth.New(creds, reauth.Config{
		CallbackPort: cfg.Reauth.CallbackPort,
		Timeout:      time.Duration(cfg.Reauth.TimeoutMinutes) * time.Minute,
	})
	routes.Register(s.mux, h, db, syncer, syncMgr, reauthMgr)

	return s
}

func (s *Server) Start() error {
	addr := fmt.Sprintf("%s:%d", s.cfg.Server.Host, s.cfg.Server.Port)
	slog.Info("server starting", "address", addr)
	return http.ListenAndServe(addr, s.mux)
}
