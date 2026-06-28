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
	"github.com/syncsystem-net/back-me-up/internal/quota"
	"github.com/syncsystem-net/back-me-up/internal/ratelimit"
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

	// Install per-provider rate limiters (request rate + bandwidth) so every
	// provider built via the registry is paced automatically.
	ratelimit.Configure(rateLimitSet(cfg))

	// Start the background upload worker pool. It runs for the life of the
	// process, claiming pending jobs and uploading them to their providers.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := worker.New(db, accts, workerConfig(cfg), cfg.Database.Path)
	w.Start(ctx)

	// Start the background quota poller. It refreshes every numbered account's
	// cached quota shortly after startup and then on the configured interval, so
	// the UI shows current numbers even without recent uploads.
	chunkSizeBytes := int64(cfg.Upload.ChunkSizeMB) << 20
	syncer := quota.New(db, accts, chunkSizeBytes, time.Duration(cfg.Quota.SyncIntervalMinutes)*time.Minute)
	go syncer.Run(ctx)

	srv := server.New(cfg, db, accts, syncer)
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
		PeriodicCheckDays:       periodicCheckDays(cfg),
		ReverifyInterval:        reverifyInterval(periodicCheckDays(cfg)),
	}
}

// reverifyInterval derives how often the re-verifier scans from the configured
// check period: roughly a quarter of the period so files are re-checked
// reasonably soon after becoming due, clamped to [1h, 24h] so it neither busy-
// loops on a short period nor sleeps for days on a long one.
func reverifyInterval(days int) time.Duration {
	if days <= 0 {
		return 24 * time.Hour // unused (the loop won't start), but a sane value
	}
	interval := time.Duration(days) * 24 * time.Hour / 4
	if interval < time.Hour {
		return time.Hour
	}
	if interval > 24*time.Hour {
		return 24 * time.Hour
	}
	return interval
}

// periodicCheckDays returns the re-verify cadence in days, or 0 (disabled) when
// verification is off. Re-verification relies on checksums captured during
// verify-on-upload, so it is meaningful only while verification is enabled.
func periodicCheckDays(cfg *config.Config) int {
	if !cfg.Verification.Enabled {
		return 0
	}
	return cfg.Verification.PeriodicCheckDays
}

// rateLimitSet builds the per-provider limiter set from config. Bandwidth is
// given in MB/s and converted to bytes/s; a zero rate leaves that dimension
// unlimited.
func rateLimitSet(cfg *config.Config) *ratelimit.Set {
	const mb = 1 << 20
	mkLimiter := func(rl config.ProviderRateLimit) *ratelimit.Limiter {
		return ratelimit.New(float64(rl.RequestsPerSecond), float64(rl.BandwidthMBPerSecond)*mb)
	}
	return ratelimit.NewSet(map[string]*ratelimit.Limiter{
		"mega":       mkLimiter(cfg.RateLimits.Mega),
		"fourshared": mkLimiter(cfg.RateLimits.FourShared),
	})
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
