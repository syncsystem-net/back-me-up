// Package worker runs the background upload pipeline. A small pool of goroutines
// claims pending jobs from the database, uploads each job's zip to its provider
// in chunks (reporting progress as it goes), retries with exponential backoff,
// verifies the result, refreshes the account's quota, and backs up the metadata
// database to the main account. The pool is provider-agnostic: it talks to the
// provider registry, never to a concrete backend.
package worker

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/syncsystem-net/back-me-up/internal/accounts"
	"github.com/syncsystem-net/back-me-up/internal/cloud"
	"github.com/syncsystem-net/back-me-up/internal/database"
	"github.com/syncsystem-net/back-me-up/internal/provider"
)

// Config is the worker's slice of application configuration, pre-converted into
// the units the worker uses (bytes, durations).
type Config struct {
	ChunkSizeBytes          int64
	MaxWorkers              int // goroutine pool size (claimers)
	MaxConcurrentUploads    int // ceiling on simultaneous uploads across all accounts
	MaxConcurrentPerAccount int
	MaxAttempts             int
	InitialBackoff          time.Duration
	MaxBackoff              time.Duration
	BackoffMultiplier       int
	VerifyOnUpload          bool
	PollInterval            time.Duration

	// PeriodicCheckDays re-verifies a completed file if it has not been checked
	// within this many days. Zero disables periodic re-verification.
	PeriodicCheckDays int
	// ReverifyInterval is how often the re-verifier wakes to look for due files.
	ReverifyInterval time.Duration
}

// Worker owns the upload pool and its dependencies.
type Worker struct {
	db       *sql.DB
	accounts *accounts.AccountStore
	cfg      Config
	dbPath   string

	uploadSem chan struct{} // global ceiling on concurrent uploads

	mu      sync.Mutex
	acctSem map[int64]chan struct{} // per-account concurrency limiter
}

// New builds a Worker. dbPath is the SQLite file, copied and uploaded to the
// main account after each successful job.
func New(db *sql.DB, accts *accounts.AccountStore, cfg Config, dbPath string) *Worker {
	if cfg.MaxConcurrentUploads < 1 {
		cfg.MaxConcurrentUploads = 1
	}
	if cfg.MaxConcurrentPerAccount < 1 {
		cfg.MaxConcurrentPerAccount = 1
	}
	// The claimer pool must be at least as large as the upload ceiling, or we
	// could never reach max_concurrent_uploads.
	if cfg.MaxWorkers < cfg.MaxConcurrentUploads {
		cfg.MaxWorkers = cfg.MaxConcurrentUploads
	}
	if cfg.MaxAttempts < 1 {
		cfg.MaxAttempts = 1
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 2 * time.Second
	}
	return &Worker{
		db:        db,
		accounts:  accts,
		cfg:       cfg,
		dbPath:    dbPath,
		uploadSem: make(chan struct{}, cfg.MaxConcurrentUploads),
		acctSem:   make(map[int64]chan struct{}),
	}
}

// Start launches the pool and returns immediately. Workers run until ctx is
// cancelled. Jobs left in_progress by a previous run are requeued first.
func (w *Worker) Start(ctx context.Context) {
	if n, err := database.RequeueStaleJobs(w.db); err != nil {
		slog.Error("requeue stale jobs failed", "error", err)
	} else if n > 0 {
		slog.Info("requeued stale jobs from previous run", "count", n)
	}

	for i := 0; i < w.cfg.MaxWorkers; i++ {
		go w.loop(ctx, i)
	}
	slog.Info("upload worker pool started", "workers", w.cfg.MaxWorkers, "max_concurrent_uploads", w.cfg.MaxConcurrentUploads)

	if w.cfg.PeriodicCheckDays > 0 {
		go w.reverifyLoop(ctx)
	}
}

func (w *Worker) loop(ctx context.Context, id int) {
	for {
		if ctx.Err() != nil {
			return
		}
		job, err := database.ClaimNextPendingJob(w.db)
		if err != nil {
			slog.Error("claiming job failed", "worker", id, "error", err)
			if !sleep(ctx, w.cfg.PollInterval) {
				return
			}
			continue
		}
		if job == nil {
			if !sleep(ctx, w.cfg.PollInterval) {
				return
			}
			continue
		}
		w.process(ctx, job)
	}
}

// process runs the full lifecycle for one claimed job.
func (w *Worker) process(ctx context.Context, job *database.Job) {
	// Global ceiling on simultaneous uploads (max_concurrent_uploads), acquired
	// before the per-account limiter so the acquisition order is consistent.
	select {
	case w.uploadSem <- struct{}{}:
		defer func() { <-w.uploadSem }()
	case <-ctx.Done():
		return
	}

	// Serialize uploads per account.
	sem := w.accountSem(job.AccountID)
	select {
	case sem <- struct{}{}:
		defer func() { <-sem }()
	case <-ctx.Done():
		return
	}

	w.log(job.ID, "info", fmt.Sprintf("starting upload to %s (%s)", job.Provider, job.Email))

	p, err := w.connect(ctx, job)
	if err != nil {
		w.fail(job, fmt.Sprintf("connect: %v", err))
		return
	}

	// Upload under the clean cloud name recorded on the job. The local zip may
	// carry a uniqueness suffix to avoid clobbering an in-flight zip on disk, so
	// we cannot derive the remote name from the zip's basename.
	remoteName := job.RemoteName
	if remoteName == "" {
		remoteName = filepath.Base(job.ZipPath)
	}
	remoteRef, err := w.uploadWithRetry(ctx, p, job, remoteName)
	if err != nil {
		w.fail(job, err.Error())
		return
	}

	// Verify the upload by comparing the checksum of the first chunk.
	var verifiedSum string
	if w.cfg.VerifyOnUpload {
		sum, err := w.verify(ctx, p, job, remoteRef)
		if err != nil {
			w.fail(job, fmt.Sprintf("verification failed: %v", err))
			return
		}
		verifiedSum = sum
		w.log(job.ID, "info", "verification passed")
	}

	if err := database.CompleteJob(w.db, job.ID, remoteRef); err != nil {
		slog.Error("marking job complete failed", "job", job.ID, "error", err)
	}
	// Persist the verified checksum so periodic re-verification can re-check this
	// file later, once the local temp zip is gone.
	if verifiedSum != "" {
		if err := database.SetJobVerified(w.db, job.ID, verifiedSum); err != nil {
			slog.Warn("recording job verification failed", "job", job.ID, "error", err)
		}
	}
	w.log(job.ID, "info", "upload complete")

	w.refreshQuota(ctx, p, job)
	w.cleanupZip(job)
	w.backupDatabase(ctx)
}

// uploadWithRetry attempts the upload up to MaxAttempts times with exponential
// backoff, persisting progress after each chunk.
func (w *Worker) uploadWithRetry(ctx context.Context, p provider.Provider, job *database.Job, remoteName string) (string, error) {
	backoff := w.cfg.InitialBackoff
	var lastErr error
	for attempt := 1; attempt <= w.cfg.MaxAttempts; attempt++ {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		onProgress := func(pr provider.Progress) {
			if err := database.UpdateJobProgress(w.db, job.ID, pr.UploadedBytes, pr.ChunksUploaded, pr.ChunksTotal); err != nil {
				slog.Warn("persisting progress failed", "job", job.ID, "error", err)
			}
		}
		remoteRef, err := p.Upload(ctx, job.ZipPath, remoteName, onProgress)
		if err == nil {
			if attempt > 1 {
				w.log(job.ID, "info", fmt.Sprintf("upload succeeded on attempt %d", attempt))
			}
			return remoteRef, nil
		}
		lastErr = err
		w.log(job.ID, "error", fmt.Sprintf("attempt %d/%d failed: %v", attempt, w.cfg.MaxAttempts, err))

		// Reset progress; providers restart the transfer on a fresh attempt.
		_ = database.UpdateJobProgress(w.db, job.ID, 0, 0, 0)

		if attempt < w.cfg.MaxAttempts {
			if !sleep(ctx, backoff) {
				return "", ctx.Err()
			}
			backoff = nextBackoff(backoff, w.cfg.BackoffMultiplier, w.cfg.MaxBackoff)
		}
	}
	return "", fmt.Errorf("upload failed after %d attempts: %w", w.cfg.MaxAttempts, lastErr)
}

// connect builds a provider for the job's account and authenticates it.
func (w *Worker) connect(ctx context.Context, job *database.Job) (provider.Provider, error) {
	return cloud.Connect(ctx, w.accounts, job.Provider, job.Email, w.cfg.ChunkSizeBytes)
}

func (w *Worker) refreshQuota(ctx context.Context, p provider.Provider, job *database.Job) {
	total, used, err := p.GetQuota(ctx)
	if err != nil {
		w.log(job.ID, "warn", fmt.Sprintf("could not refresh quota: %v", err))
		return
	}
	if err := database.UpdateAccountQuota(w.db, job.AccountID, total, used); err != nil {
		slog.Warn("updating account quota failed", "account", job.AccountID, "error", err)
	}
}

// cleanupZip deletes the temp zip once no other job still needs it. A failed
// sibling keeps the zip for retry.
func (w *Worker) cleanupZip(job *database.Job) {
	remaining, err := database.CountUnfinishedJobsForZip(w.db, job.ZipPath, job.ID)
	if err != nil {
		slog.Warn("counting unfinished jobs failed", "job", job.ID, "error", err)
		return
	}
	if remaining > 0 {
		w.log(job.ID, "info", fmt.Sprintf("keeping temp zip; %d sibling job(s) still pending", remaining))
		return
	}
	if err := removeFile(job.ZipPath); err != nil {
		w.log(job.ID, "warn", fmt.Sprintf("could not delete temp zip %s: %v", job.ZipPath, err))
		return
	}
	w.log(job.ID, "info", "temp zip deleted")
}

func (w *Worker) fail(job *database.Job, msg string) {
	w.log(job.ID, "error", msg)
	if err := database.FailJob(w.db, job.ID, msg); err != nil {
		slog.Error("marking job failed errored", "job", job.ID, "error", err)
	}
}

func (w *Worker) log(jobID int64, level, msg string) {
	if err := database.InsertJobLog(w.db, jobID, level, msg); err != nil {
		slog.Warn("writing job log failed", "job", jobID, "error", err)
	}
	slog.Info("job", "id", jobID, "level", level, "msg", msg)
}

func (w *Worker) accountSem(accountID int64) chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	sem, ok := w.acctSem[accountID]
	if !ok {
		sem = make(chan struct{}, w.cfg.MaxConcurrentPerAccount)
		w.acctSem[accountID] = sem
	}
	return sem
}

// nextBackoff grows the delay geometrically, capped at max.
func nextBackoff(current time.Duration, multiplier int, max time.Duration) time.Duration {
	if multiplier < 1 {
		multiplier = 1
	}
	next := current * time.Duration(multiplier)
	if max > 0 && next > max {
		return max
	}
	return next
}

// sleep waits for d or until ctx is cancelled. It returns false if cancelled.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
