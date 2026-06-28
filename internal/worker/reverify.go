package worker

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/syncsystem-net/back-me-up/internal/database"
)

const (
	// reverifyInitialDelay staggers the first re-verify scan past startup so it
	// doesn't contend with requeued uploads.
	reverifyInitialDelay = 30 * time.Second
	// reverifyBatchSize caps how many files one scan re-checks, so re-verification
	// trickles network/quota use rather than bursting through every due file at
	// once. Remaining due files are picked up on subsequent scans.
	reverifyBatchSize = 5
)

// reverifyLoop runs an initial scan shortly after startup, then re-scans every
// ReverifyInterval until ctx is cancelled. Each scan re-checks a small random
// sample of completed files whose stored checksum hasn't been confirmed within
// PeriodicCheckDays. Launched only when PeriodicCheckDays > 0.
func (w *Worker) reverifyLoop(ctx context.Context) {
	interval := w.cfg.ReverifyInterval
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	slog.Info("periodic re-verifier started", "check_days", w.cfg.PeriodicCheckDays, "scan_interval", interval)

	if !sleep(ctx, reverifyInitialDelay) {
		return
	}
	w.reverifyDue(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.reverifyDue(ctx)
		}
	}
}

// reverifyDue re-checks up to reverifyBatchSize completed files that are due for
// re-verification (last verified more than PeriodicCheckDays ago, or never).
func (w *Worker) reverifyDue(ctx context.Context) {
	cutoff := time.Now().AddDate(0, 0, -w.cfg.PeriodicCheckDays)
	jobs, err := database.ListJobsDueForReverify(w.db, cutoff, reverifyBatchSize)
	if err != nil {
		slog.Warn("re-verify: listing due files failed", "error", err)
		return
	}
	if len(jobs) == 0 {
		return
	}
	slog.Info("re-verify: checking due files", "count", len(jobs))
	for _, job := range jobs {
		if ctx.Err() != nil {
			return
		}
		w.reverifyOne(ctx, job)
	}
}

// reverifyOne re-downloads the first chunk of one completed file and compares it
// to the checksum captured at upload time. A pass refreshes last_verified_at; a
// mismatch or download failure is logged to the job (visible in the logs modal)
// and left for the operator to act on. The job's completed status is not
// changed — re-verification reports, it does not auto-remediate.
func (w *Worker) reverifyOne(ctx context.Context, job *database.Job) {
	want, err := database.VerifyChecksum(w.db, job.ID)
	if err != nil {
		slog.Warn("re-verify: reading stored checksum failed", "job", job.ID, "error", err)
		return
	}
	if want == "" {
		return // nothing to compare against (shouldn't happen given the query filter)
	}

	p, err := w.connect(ctx, job)
	if err != nil {
		w.log(job.ID, "warn", fmt.Sprintf("re-verify skipped: connect failed: %v", err))
		return
	}

	got, err := w.downloadHeadSum(ctx, p, job.RemotePath, w.verifyLimit())
	if err != nil {
		w.log(job.ID, "error", fmt.Sprintf("re-verify failed: %v", err))
		return
	}
	if got != want {
		w.log(job.ID, "error", fmt.Sprintf("re-verify checksum mismatch (stored %s != remote %s)", want[:12], got[:12]))
		return
	}

	if err := database.TouchJobVerified(w.db, job.ID); err != nil {
		slog.Warn("re-verify: stamping last_verified_at failed", "job", job.ID, "error", err)
	}
	w.log(job.ID, "info", "re-verify passed")
}
