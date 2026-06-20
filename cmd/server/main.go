package main

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"time"

	"github.com/syncsystem-net/back-me-up/internal/accounts"
	"github.com/syncsystem-net/back-me-up/internal/config"
	"github.com/syncsystem-net/back-me-up/internal/database"
	"github.com/syncsystem-net/back-me-up/internal/server"
	"github.com/syncsystem-net/back-me-up/internal/worker"
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

	slog.Info("main account (db backup only, not shown in UI)", "provider", accts.Main.Provider, "email", accts.Main.Email)

	if err := syncAccountsToDB(db, accts); err != nil {
		slog.Warn("failed to sync accounts to database", "error", err)
	}

	// Start the background upload worker pool. It runs for the life of the
	// process, claiming pending jobs and uploading them to their providers.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := worker.New(db, accts, workerConfig(cfg), cfg.Database.Path)
	w.Start(ctx)

	srv := server.New(cfg, db, accts)
	if err := srv.Start(); err != nil {
		slog.Error("server error", "error", err)
		os.Exit(1)
	}
}

// workerConfig translates the application config into the worker's units
// (bytes, durations).
func workerConfig(cfg *config.Config) worker.Config {
	return worker.Config{
		ChunkSizeBytes:          int64(cfg.Upload.ChunkSizeMB) << 20,
		MaxWorkers:              cfg.Concurrency.MaxWorkers,
		MaxConcurrentUploads:    cfg.Concurrency.MaxConcurrentUploads,
		MaxConcurrentPerAccount: cfg.Concurrency.MaxConcurrentPerAccount,
		MaxAttempts:             cfg.RetryPolicy.MaxAttempts,
		InitialBackoff:          time.Duration(cfg.RetryPolicy.InitialBackoffSeconds) * time.Second,
		MaxBackoff:              time.Duration(cfg.RetryPolicy.MaxBackoffSeconds) * time.Second,
		BackoffMultiplier:       cfg.RetryPolicy.BackoffMultiplier,
		VerifyOnUpload:          cfg.Verification.Enabled && cfg.Verification.VerifyOnUpload,
		PollInterval:            2 * time.Second,
	}
}

func syncAccountsToDB(db *sql.DB, accts *accounts.AccountStore) error {
	for _, a := range accts.Accounts {
		slog.Info("syncing account", "provider", a.Provider, "email", a.Email, "quota_gb", a.QuotaGB)
		if _, err := database.UpsertAccount(db, string(a.Provider), a.Email, a.QuotaGB); err != nil {
			return err
		}
	}
	slog.Info("accounts synced", "count", len(accts.Accounts))
	return nil
}
