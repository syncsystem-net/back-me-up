// Package quota keeps each numbered account's cached storage quota fresh. A
// Syncer polls every provider on an interval (and on demand), reusing the shared
// cloud.Connect path so quota fetching never imports a concrete backend. Results
// are written to the accounts table via database.UpdateAccountQuota, which owns
// the bytes->GB conversion and stamps last_quota_sync. The main (db-backup)
// account is never polled — only numbered upload-target accounts.
package quota

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"github.com/syncsystem-net/back-me-up/internal/accounts"
	"github.com/syncsystem-net/back-me-up/internal/cloud"
	"github.com/syncsystem-net/back-me-up/internal/database"
)

// initialDelay is how long after startup the first sync runs, so it doesn't
// contend with the rest of the boot sequence.
const initialDelay = 5 * time.Second

// Syncer refreshes account quotas. It is safe to call SyncAll directly (e.g.
// from the on-demand HTTP handler) while Run's ticker loop is also active; both
// just issue per-account quota fetches and serialized DB writes.
type Syncer struct {
	db             *sql.DB
	store          *accounts.AccountStore
	chunkSizeBytes int64
	interval       time.Duration
}

// New builds a Syncer. chunkSizeBytes only satisfies cloud.Connect's signature
// (quota calls don't chunk); interval is the poll period.
func New(db *sql.DB, store *accounts.AccountStore, chunkSizeBytes int64, interval time.Duration) *Syncer {
	return &Syncer{
		db:             db,
		store:          store,
		chunkSizeBytes: chunkSizeBytes,
		interval:       interval,
	}
}

// SyncAll refreshes the quota for every numbered account once. A per-account
// failure (login/network/lookup) is logged and skipped so one bad account never
// aborts the cycle or affects the others. ctx cancellation stops the loop early.
func (s *Syncer) SyncAll(ctx context.Context) {
	if s.store == nil {
		return
	}
	for _, a := range s.store.Accounts {
		if ctx.Err() != nil {
			return
		}
		s.syncOne(ctx, a)
	}
}

func (s *Syncer) syncOne(ctx context.Context, a accounts.Account) {
	provider := string(a.Provider)
	p, err := cloud.Connect(ctx, s.store, provider, a.Email, s.chunkSizeBytes)
	if err != nil {
		slog.Warn("quota sync: could not connect", "provider", provider, "email", a.Email, "error", err)
		return
	}
	total, used, err := p.GetQuota(ctx)
	if err != nil {
		slog.Warn("quota sync: GetQuota failed", "provider", provider, "email", a.Email, "error", err)
		return
	}
	id, err := database.GetDBAccountIDByProviderEmail(s.db, provider, a.Email)
	if err != nil {
		slog.Warn("quota sync: account not in db", "provider", provider, "email", a.Email, "error", err)
		return
	}
	if err := database.UpdateAccountQuota(s.db, id, total, used); err != nil {
		slog.Warn("quota sync: updating quota failed", "provider", provider, "email", a.Email, "error", err)
		return
	}
	slog.Info("quota synced", "provider", provider, "email", a.Email)
}

// Run performs one sync shortly after startup, then re-syncs every interval
// until ctx is cancelled. Intended to be launched in its own goroutine.
func (s *Syncer) Run(ctx context.Context) {
	if !wait(ctx, initialDelay) {
		return
	}
	s.SyncAll(ctx)

	interval := s.interval
	if interval <= 0 {
		interval = time.Hour
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.SyncAll(ctx)
		}
	}
}

// wait sleeps for d or until ctx is cancelled; returns false if cancelled.
func wait(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
