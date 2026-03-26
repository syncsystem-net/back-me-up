package main

import (
	"log/slog"
	"os"

	"github.com/syncsystem-net/back-me-up/internal/accounts"
	"github.com/syncsystem-net/back-me-up/internal/config"
	"github.com/syncsystem-net/back-me-up/internal/database"
	"github.com/syncsystem-net/back-me-up/internal/server"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	cfg, err := config.Load("config.yml")
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	accts, err := accounts.Load(".env")
	if err != nil {
		slog.Warn("no .env file loaded, running without accounts", "error", err)
		accts = &accounts.AccountStore{}
	}

	db, err := database.Open(cfg.Database.Path)
	if err != nil {
		slog.Error("failed to open database", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	srv := server.New(cfg, db, accts)
	if err := srv.Start(); err != nil {
		slog.Error("server error", "error", err)
		os.Exit(1)
	}
}
