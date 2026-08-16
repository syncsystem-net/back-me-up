package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/syncsystem-net/back-me-up/internal/accounts"
	"github.com/syncsystem-net/back-me-up/internal/cloud"
	"github.com/syncsystem-net/back-me-up/internal/config"
	"github.com/syncsystem-net/back-me-up/internal/credentials"
	"github.com/syncsystem-net/back-me-up/internal/database"
	"github.com/syncsystem-net/back-me-up/internal/keyring"
	"github.com/syncsystem-net/back-me-up/internal/provider/registry"
	"github.com/syncsystem-net/back-me-up/internal/quota"
	"github.com/syncsystem-net/back-me-up/internal/ratelimit"
	"github.com/syncsystem-net/back-me-up/internal/reauth"
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

	// .env is parsed first because it also supplies the credential passphrase.
	// It is no longer the running source of truth — the database is — but it
	// remains how an account is added or a password changed.
	// A .env that cannot be read yields nil, not an empty config. An empty config
	// declares "no main accounts", and a main account is the one thing .env can
	// delete — so a typo'd or renamed .env would silently drop every db-backup
	// destination, including a token the app re-authorized. Nil means "nothing to
	// reconcile", which is the truth.
	env, err := accounts.LoadEnv(".env")
	if err != nil {
		slog.Warn("could not read .env; the stored accounts are used unchanged and nothing is reconciled", "error", err)
		env = nil
	}

	db, err := database.Open(cfg.Database.Path)
	if err != nil {
		slog.Error("failed to open database", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	creds := openCredentials(db, env)
	accts := creds.Store()
	logMainAccounts(accts)

	// Install per-provider rate limiters (request rate + bandwidth) so every
	// provider built via the registry is paced automatically.
	ratelimit.Configure(rateLimitSet(cfg))

	// Record any account whose credentials a provider rejects as expired, so the
	// Accounts view can offer Re-authorize instead of the user finding out from a
	// failed job. Installed once, like the rate limiters: an expired token can
	// surface from any provider call, and threading a credentials manager through
	// every one of them for a side effect none of them care about would be worse.
	cloud.OnAuthExpired(func(providerName, email string, isMain bool) {
		// Only providers that can actually be re-authorized are flagged. MEGA
		// answers a wrong password with the same sentinel, and flagging it would
		// paint a red badge the user has no way to clear — there is no OAuth token
		// to replace. A bad MEGA password already surfaces in the job's logs.
		if !reauth.Supported(providerName) {
			slog.Warn("provider rejected these credentials", "provider", providerName, "email", email, "main", isMain)
			return
		}
		reason := "The provider rejected this account's access token as expired or revoked."
		if err := creds.FlagReauth(providerName, email, reason, isMain); err != nil {
			slog.Warn("could not record that an account needs re-authorization",
				"provider", providerName, "email", email, "error", err)
			return
		}
		slog.Warn("account needs re-authorization", "provider", providerName, "email", email, "main", isMain)
	})

	// The counterpart: an account that authenticates again is no longer in need of
	// re-authorization, so the badge withdraws itself. Without this a single
	// transient rejection would leave a standing warning that only an unnecessary
	// re-authorization could clear.
	cloud.OnAuthRestored(func(providerName, email string, isMain bool) {
		if err := creds.ClearReauth(providerName, email, isMain); err != nil {
			slog.Warn("could not clear an account's re-authorization flag",
				"provider", providerName, "email", email, "error", err)
		}
	})

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

	srv := server.New(cfg, db, creds, syncer)
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

// openCredentials derives the credential key from the .env passphrase and
// reconciles .env into the encrypted store.
//
// A missing or wrong passphrase does not stop the server. The credentials on
// disk are intact and a correct passphrase would still open them, so the app
// starts in a locked state: the UI loads, existing records stay browsable, a
// banner names the exact key to fix, and every credential operation refuses.
// Exiting instead would leave the user with "connection refused" and no
// explanation — and, worse, any attempt to re-import under the wrong key would
// overwrite credentials that are currently recoverable.
func openCredentials(db *sql.DB, env *accounts.EnvConfig) *credentials.Manager {
	passphrase := os.Getenv(keyring.EnvKey)
	kr, err := keyring.Open(db, passphrase)
	if err != nil {
		reason := lockReason(err)
		slog.Error("credentials are locked; the app is running read-only", "reason", reason)
		return credentials.NewLocked(db, reason)
	}

	mgr := credentials.New(db, kr)
	if err := mgr.Import(env); err != nil {
		// Reconciliation failed with a valid key: a database problem, not a
		// credential one. Lock rather than run with a half-imported set.
		reason := "could not read the stored credentials: " + err.Error()
		slog.Error("credentials are locked", "reason", reason)
		return credentials.NewLocked(db, reason)
	}
	accounts.WarnIncompleteMains(mgr.Store().Mains())
	return mgr
}

func lockReason(err error) string {
	switch {
	case errors.Is(err, keyring.ErrNoPassphrase):
		return "No credential passphrase is set. Add " + keyring.EnvKey +
			" to .env (single-quote it if it contains $, # or spaces) and restart."
	case errors.Is(err, keyring.ErrWrongPassphrase):
		return keyring.EnvKey + " does not match the passphrase this database was encrypted with. " +
			"Restore the original passphrase and restart — stored credentials cannot be recovered without it."
	default:
		return "The credential key could not be derived: " + err.Error()
	}
}

// logMainAccounts reports the configured database-backup destinations, one line
// per provider, so a misread .env is visible at startup rather than at the end
// of the first job. Configuring none is a supported choice and says so plainly;
// WarnIncompleteMains has already warned about half-configured ones.
func logMainAccounts(accts *accounts.AccountStore) {
	if accts.IsLocked() {
		return // the lock is already reported, loudly, and the list is empty
	}
	mains := accts.Mains()
	if len(mains) == 0 {
		slog.Info("no main account configured; the metadata database will not be copied to any provider")
		return
	}
	for _, m := range mains {
		if !registry.Supported(string(m.Provider)) {
			slog.Warn("main account names an unsupported provider; it will receive no db backup",
				"provider", m.Provider, "email", m.Email)
			continue
		}
		slog.Info("main account (db backup only, not an upload target)",
			"provider", m.Provider, "email", m.Email, "usable", m.Usable())
	}
}
