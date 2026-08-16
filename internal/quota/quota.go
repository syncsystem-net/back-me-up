// Package quota keeps each numbered account's cached storage quota fresh. A
// Syncer polls every provider on an interval (and on demand), reusing the shared
// cloud.Connect path so quota fetching never imports a concrete backend. Results
// are written to the accounts table via database.UpdateAccountQuota, which owns
// the bytes->GB conversion and stamps last_quota_sync. Database-backup (main)
// accounts are polled in a second pass and written to main_accounts, since they
// are deliberately not rows in the accounts table.
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

// SyncAll refreshes the quota for every account once — numbered upload targets
// first, then the database-backup accounts. A per-account failure
// (login/network/lookup) is logged and skipped so one bad account never aborts
// the cycle or affects the others. ctx cancellation stops the loop early.
func (s *Syncer) SyncAll(ctx context.Context) {
	if s.store == nil {
		return
	}
	// Locked credentials cannot be opened, so every connect would fail with the
	// same reason. Say it once instead of once per account per cycle.
	if reason := s.store.LockReason(); reason != "" {
		slog.Warn("quota sync skipped: credentials are locked", "reason", reason)
		return
	}
	for _, a := range s.store.All() {
		if ctx.Err() != nil {
			return
		}
		s.syncOne(ctx, a)
	}
	for _, m := range s.store.Mains() {
		if ctx.Err() != nil {
			return
		}
		s.syncMain(ctx, m)
	}
}

// syncMain polls a database-backup account's quota. Main accounts are not rows
// in the accounts table (see the schema comment), so this writes to
// main_accounts instead — which is what lets the Accounts view show used/free
// for them rather than a card that is conspicuously less informative than the
// upload targets beside it.
func (s *Syncer) syncMain(ctx context.Context, m accounts.MainAccount) {
	providerName := string(m.Provider)
	if !m.Usable() {
		return // already reported at load; polling it would only fail
	}
	p, err := cloud.NewMain(m, s.chunkSizeBytes)
	if err != nil {
		slog.Warn("quota sync: could not build main account provider", "provider", providerName, "email", m.Email, "error", err)
		return
	}
	if err := p.Login(ctx, m.Email, m.Password); err != nil {
		slog.Warn("quota sync: main account login failed", "provider", providerName, "email", m.Email, "error", err)
		return
	}
	total, used, err := p.GetQuota(ctx)
	if err != nil {
		slog.Warn("quota sync: main account GetQuota failed", "provider", providerName, "email", m.Email, "error", err)
		return
	}
	if err := database.UpdateMainAccountQuota(s.db, providerName, total, used); err != nil {
		slog.Warn("quota sync: updating main account quota failed", "provider", providerName, "email", m.Email, "error", err)
		return
	}
	slog.Info("quota synced", "provider", providerName, "email", m.Email, "main", true)
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
