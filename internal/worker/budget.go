package worker

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/syncsystem-net/back-me-up/internal/archive"
	"github.com/syncsystem-net/back-me-up/internal/database"
	"github.com/syncsystem-net/back-me-up/internal/limits"
)

// allowances works out how many bytes each account with pending work may still
// upload inside its provider's rolling transfer window, and returns the result
// in the form the claim query takes.
//
// Enforcement lives in the claim rather than after it for a specific reason: a
// claim-then-release loop would keep taking the oldest pending job — which
// belongs to the over-budget account — and put it straight back, so no other
// account's jobs would ever run. Constraining the claim lets a held account wait
// without the queue waiting with it.
//
// It also does two things a pure calculation would not:
//
//   - A job larger than the account's entire budget is failed here rather than
//     held. No amount of waiting makes room for it, and a job that waits forever
//     with no explanation is worse than a job that fails with one.
//   - Jobs that do not fit right now are marked with a hold reason, and jobs that
//     fit again have it cleared, so the UI can say "waiting for the transfer
//     budget" instead of showing a pending job that looks stuck.
func (w *Worker) allowances() ([]database.Allowance, error) {
	pending, err := database.ListAccountsWithPendingJobs(w.db)
	if err != nil {
		return nil, err
	}
	if len(pending) == 0 {
		// No pending work at all. An unrestricted claim is equivalent and avoids
		// asserting "no account may be claimed from", which an empty slice means.
		return nil, nil
	}

	limitSet := database.GetProviderLimits(w.db)
	out := make([]database.Allowance, 0, len(pending))

	for _, pa := range pending {
		lim := limitSet.For(pa.Provider, limits.NormalizeTier(pa.Tier))
		if !lim.Budgeted() {
			out = append(out, database.Allowance{AccountID: pa.AccountID, MaxBytes: -1})
			w.releaseHolds(pa, -1)
			continue
		}

		// An archive bigger than the whole budget can never be uploaded under
		// these settings. Fail it now, with the numbers and the two ways out.
		if pa.LargestPendingBytes > lim.TransferBytes {
			w.failImpossible(pa, lim)
		}

		used, err := database.TransferUsedSince(w.db, pa.AccountID, lim.WindowHours)
		if err != nil {
			// Without a usage figure we cannot enforce anything honestly. Allowing
			// the upload is the right failure direction: the provider is the real
			// authority on its limits, and blocking on our own bookkeeping error
			// would stall backups for a reason that has nothing to do with them.
			slog.Warn("could not read transfer usage; not enforcing the budget this tick",
				"account_id", pa.AccountID, "error", err)
			out = append(out, database.Allowance{AccountID: pa.AccountID, MaxBytes: -1})
			continue
		}

		// The allowance handed to the claim counts the ledger only. Uploads already
		// running are subtracted inside the claim statement itself, because a
		// figure computed out here is one two workers can both pass.
		remaining := lim.TransferBytes - used
		if remaining < 0 {
			remaining = 0
		}
		out = append(out, database.Allowance{AccountID: pa.AccountID, MaxBytes: remaining})

		// The hold shown in the UI does account for in-flight work, because a job
		// waiting behind a sibling upload is genuinely waiting. A read failure here
		// only costs the badge its precision, so it degrades rather than skipping
		// enforcement.
		inFlight, err := database.TransferInFlightBytes(w.db, pa.AccountID)
		if err != nil {
			slog.Warn("could not read in-flight uploads for the hold message",
				"account_id", pa.AccountID, "error", err)
			inFlight = 0
		}
		available := remaining - inFlight
		if available < 0 {
			available = 0
		}

		w.holdOverBudget(pa, lim, available, inFlight)
		w.releaseHolds(pa, available)
	}
	return out, nil
}

// failImpossible fails an account's pending jobs that exceed its entire transfer
// budget, since holding them would be waiting for something that cannot happen.
//
// The job is failed under a status guard and logged only if this call is what
// failed it. Every worker goroutine runs this on every poll tick against jobs
// none of them has claimed, so without that the same explanation would be
// written once per worker.
func (w *Worker) failImpossible(pa database.PendingAccount, lim limits.Limit) {
	jobs, err := database.ListPendingJobsOverBytes(w.db, pa.AccountID, lim.TransferBytes)
	if err != nil {
		slog.Warn("could not list over-budget jobs", "account_id", pa.AccountID, "error", err)
		return
	}
	for _, j := range jobs {
		msg := fmt.Sprintf(
			"this archive is %s but %s account %s has a transfer budget of only %s per %dh, so it can never be uploaded. "+
				"Lower this provider's split threshold or set its transfer budget to unlimited in Settings.",
			archive.HumanBytes(j.TotalBytes), pa.Provider, pa.Email,
			archive.HumanBytes(lim.TransferBytes), lim.WindowHours)

		failed, err := database.FailPendingJob(w.db, j.ID, msg)
		if err != nil {
			slog.Error("failing an over-budget job", "job", j.ID, "error", err)
			continue
		}
		if failed {
			w.log(j.ID, "error", msg)
		}
	}
}

// holdOverBudget marks the account's pending jobs that do not fit the remaining
// allowance, naming when they are expected to resume.
//
// The message is written only when it changes, which is what keeps a standing
// hold from writing a log line every poll tick for hours — the same discipline
// as noMainOnce and lockedOnce.
func (w *Worker) holdOverBudget(pa database.PendingAccount, lim limits.Limit, available, inFlight int64) {
	reason := fmt.Sprintf("waiting for the %s account's transfer budget (%s per %dh); %s left right now",
		pa.Provider, archive.HumanBytes(lim.TransferBytes), lim.WindowHours, archive.HumanBytes(available))

	// Distinguish the two reasons the allowance is short. Waiting minutes behind
	// a sibling upload and waiting hours for a window to roll over feel nothing
	// alike, and "0 B left" with no resume time reads like the second when it is
	// usually the first.
	if inFlight > 0 {
		reason += fmt.Sprintf(", with %s already uploading to this account", archive.HumanBytes(inFlight))
	}

	if resets, err := database.TransferWindowResetsAt(
		w.db, pa.AccountID, lim.WindowHours, pa.LargestPendingBytes, lim.TransferBytes,
	); err == nil && !resets.IsZero() {
		reason += fmt.Sprintf("; expected to resume around %s", resets.Local().Format(time.RFC1123))
	}

	n, err := database.HoldPendingJobs(w.db, pa.AccountID, available, reason)
	if err != nil {
		slog.Warn("could not mark jobs as held", "account_id", pa.AccountID, "error", err)
		return
	}
	if n > 0 {
		slog.Info("holding uploads for the transfer budget",
			"provider", pa.Provider, "email", pa.Email, "jobs", n,
			"available_bytes", available, "in_flight_bytes", inFlight, "window_hours", lim.WindowHours)
	}
}

// releaseHolds clears the hold on jobs that fit again. A negative maxBytes
// releases everything, which is what an unlimited budget means.
func (w *Worker) releaseHolds(pa database.PendingAccount, maxBytes int64) {
	n, err := database.ReleasePendingJobs(w.db, pa.AccountID, maxBytes)
	if err != nil {
		slog.Warn("could not release held jobs", "account_id", pa.AccountID, "error", err)
		return
	}
	if n > 0 {
		slog.Info("transfer budget allows held uploads to resume",
			"provider", pa.Provider, "email", pa.Email, "jobs", n)
	}
}

// recordTransfer adds a completed upload to the account's transfer ledger. It is
// recorded on success only: a failed upload may have moved bytes, but we have no
// reliable figure for how many, and over-counting would hold later jobs for
// traffic that may never have happened.
func (w *Worker) recordTransfer(job *database.Job) {
	if err := database.RecordTransfer(w.db, job.AccountID, job.TotalBytes); err != nil {
		slog.Warn("could not record transfer usage", "job", job.ID, "error", err)
	}
}
