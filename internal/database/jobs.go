package database

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

type Job struct {
	ID             int64     `json:"id"`
	BackupID       int64     `json:"backup_id"`
	ZipID          int64     `json:"zip_id"`
	AccountID      int64     `json:"account_id"`
	Provider       string    `json:"provider"`
	Email          string    `json:"email"`
	Status         string    `json:"status"`
	ZipPath        string    `json:"zip_path"`
	RemotePath     string    `json:"remote_path"`
	RemoteName     string    `json:"remote_name"`
	TotalBytes     int64     `json:"total_bytes"`
	UploadedBytes  int64     `json:"uploaded_bytes"`
	ChunksTotal    int       `json:"chunks_total"`
	ChunksUploaded int       `json:"chunks_uploaded"`
	ErrorMessage   string    `json:"error_message"`
	CreatedAt      time.Time `json:"created_at"`

	// HoldReason explains why a pending job is not being claimed — currently only
	// the provider's transfer budget. It is a column rather than something derived
	// in the UI because a job waiting hours for a window to roll over is
	// indistinguishable from a stuck one, and the explanation has to survive a
	// restart to be worth anything.
	HoldReason string `json:"hold_reason,omitempty"`
}

func InsertJob(tx *sql.Tx, backupID, zipID, accountID int64, zipPath, remoteName string, totalBytes int64) (int64, error) {
	res, err := tx.Exec(
		`INSERT INTO jobs (backup_id, zip_id, account_id, zip_path, remote_name, total_bytes) VALUES (?, ?, ?, ?, ?, ?)`,
		backupID, zipID, accountID, zipPath, remoteName, totalBytes,
	)
	if err != nil {
		return 0, fmt.Errorf("inserting job: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("getting last insert id: %w", err)
	}
	return id, nil
}

// InsertAdoptedJob records a job for an archive discovered on a provider by the
// auto-sync crawl rather than uploaded by this install. It is written already
// 'complete' with the provider's handle in remote_path, which is all the
// existing Download and Delete actions need, so an adopted archive is as
// actionable in the UI as an uploaded one.
//
// zip_path is empty on purpose: no local temp zip ever existed. verify_checksum
// is left NULL too, so the periodic re-verifier skips these rows — there is no
// local original to have hashed, and it must never report a mismatch it cannot
// substantiate.
func InsertAdoptedJob(tx *sql.Tx, backupID, zipID, accountID int64, remoteName, remotePath string, totalBytes int64) (int64, error) {
	res, err := tx.Exec(
		`INSERT INTO jobs (backup_id, zip_id, account_id, status, zip_path, remote_path, remote_name,
		                   total_bytes, uploaded_bytes, completed_at)
		 VALUES (?, ?, ?, 'complete', '', ?, ?, ?, ?, CURRENT_TIMESTAMP)`,
		backupID, zipID, accountID, remotePath, remoteName, totalBytes, totalBytes,
	)
	if err != nil {
		return 0, fmt.Errorf("inserting adopted job: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("getting last insert id: %w", err)
	}
	return id, nil
}

const jobColumns = `j.id, j.backup_id, COALESCE(j.zip_id, 0), j.account_id, a.provider, a.email, j.status,
	COALESCE(j.zip_path, ''), COALESCE(j.remote_path, ''), COALESCE(j.remote_name, ''), j.total_bytes,
	j.uploaded_bytes, j.chunks_total, j.chunks_uploaded, COALESCE(j.error_message, ''), j.created_at,
	COALESCE(j.hold_reason, '')`

func scanJob(s interface {
	Scan(...any) error
}) (*Job, error) {
	j := &Job{}
	if err := s.Scan(&j.ID, &j.BackupID, &j.ZipID, &j.AccountID, &j.Provider, &j.Email, &j.Status,
		&j.ZipPath, &j.RemotePath, &j.RemoteName, &j.TotalBytes, &j.UploadedBytes, &j.ChunksTotal,
		&j.ChunksUploaded, &j.ErrorMessage, &j.CreatedAt, &j.HoldReason); err != nil {
		return nil, err
	}
	return j, nil
}

func ListJobsByBackup(db *sql.DB, backupID int64) ([]*Job, error) {
	rows, err := db.Query(
		`SELECT `+jobColumns+`
		 FROM jobs j
		 JOIN accounts a ON j.account_id = a.id
		 WHERE j.backup_id = ?
		 ORDER BY j.created_at`,
		backupID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying jobs: %w", err)
	}
	defer rows.Close()

	var jobs []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning job row: %w", err)
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

// GetJob loads a single job (with its account's provider/email) by id.
func GetJob(db *sql.DB, id int64) (*Job, error) {
	row := db.QueryRow(
		`SELECT `+jobColumns+`
		 FROM jobs j JOIN accounts a ON j.account_id = a.id
		 WHERE j.id = ?`, id)
	j, err := scanJob(row)
	if err != nil {
		return nil, fmt.Errorf("scanning job: %w", err)
	}
	return j, nil
}

// Allowance is how many more bytes may be uploaded to one account, counting only
// what the transfer ledger already records. MaxBytes below zero means unlimited
// — the provider/tier has no transfer budget configured, or the user set it to
// unlimited.
//
// Uploads currently running are deliberately NOT subtracted here. They are
// subtracted inside the claim statement instead, because a figure computed
// before the claim is a figure two workers can both act on: each reads the same
// allowance, each claims, and the budget is exceeded by whatever the second one
// sends. Only the claim itself is atomic.
type Allowance struct {
	AccountID int64
	MaxBytes  int64
}

// Unlimited reports whether this account has no byte ceiling at the moment.
func (a Allowance) Unlimited() bool { return a.MaxBytes < 0 }

// ClaimNextPendingJob atomically marks the oldest pending job as in_progress and
// returns it, so no two workers pick up the same job. It returns (nil, nil) when
// there is no pending work. The UPDATE ... RETURNING is a single statement and
// therefore atomic under SQLite's write lock.
func ClaimNextPendingJob(db *sql.DB) (*Job, error) {
	return ClaimNextPendingJobWithin(db, nil)
}

// inFlightForAccount is a correlated subquery totalling an account's uploads
// that are already running. It is evaluated inside the claim so the figure is
// read and acted on in one atomic statement.
const inFlightForAccount = `COALESCE((SELECT SUM(f.total_bytes) FROM jobs AS f
	WHERE f.account_id = ? AND f.status = 'in_progress'), 0)`

// ClaimNextPendingJobWithin is ClaimNextPendingJob restricted to what each
// account's transfer budget currently permits. A nil allowances slice means no
// restriction at all.
//
// The budget is folded into the claim itself rather than checked after claiming
// and released on failure. That is the difference between "this account waits"
// and "the queue waits": a claim-then-release loop would keep taking the oldest
// pending job — which belongs to the over-budget account — and put it straight
// back, so no other account's jobs would ever be reached.
//
// Bytes already in flight are subtracted here rather than by the caller. An
// allowance computed before the claim is one that two workers can both pass:
// each reads "3 GB left", each claims a 1.9 GB volume, and 3.8 GB goes out. The
// UPDATE ... RETURNING is a single statement under SQLite's write lock, so
// counting in-flight work inside it makes the ceiling actually hold instead of
// merely narrowing the window.
//
// An empty (non-nil) slice means no account may be claimed from, which is the
// honest reading of "every account with pending work is over budget".
func ClaimNextPendingJobWithin(db *sql.DB, allowances []Allowance) (*Job, error) {
	where := "status = 'pending'"
	var args []any

	if allowances != nil {
		if len(allowances) == 0 {
			return nil, nil
		}
		clauses := make([]string, 0, len(allowances))
		for _, a := range allowances {
			if a.Unlimited() {
				clauses = append(clauses, "account_id = ?")
				args = append(args, a.AccountID)
				continue
			}
			clauses = append(clauses, "(account_id = ? AND total_bytes <= ? - "+inFlightForAccount+")")
			args = append(args, a.AccountID, a.MaxBytes, a.AccountID)
		}
		where += " AND (" + strings.Join(clauses, " OR ") + ")"
	}

	var id int64
	err := db.QueryRow(
		`UPDATE jobs SET status = 'in_progress', started_at = CURRENT_TIMESTAMP,
		                 error_message = NULL, hold_reason = NULL
		 WHERE id = (
		     SELECT id FROM jobs WHERE `+where+` ORDER BY created_at LIMIT 1
		 )
		 RETURNING id`,
		args...,
	).Scan(&id)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claiming job: %w", err)
	}
	return GetJob(db, id)
}

// PendingAccount is one account that currently has jobs waiting, with what the
// budget lookup needs to size its allowance.
type PendingAccount struct {
	AccountID int64
	Provider  string
	Email     string
	Tier      string
	// LargestPendingBytes is the biggest job waiting on this account. A job larger
	// than the account's entire budget can never run and must be failed rather
	// than held forever, and this is what lets the worker notice that cheaply.
	LargestPendingBytes int64
}

// ListAccountsWithPendingJobs returns the accounts that have pending work.
func ListAccountsWithPendingJobs(db *sql.DB) ([]PendingAccount, error) {
	rows, err := db.Query(
		`SELECT a.id, a.provider, a.email, COALESCE(a.tier, 'free'), MAX(j.total_bytes)
		 FROM jobs j JOIN accounts a ON j.account_id = a.id
		 WHERE j.status = 'pending'
		 GROUP BY a.id, a.provider, a.email, a.tier
		 ORDER BY a.id`,
	)
	if err != nil {
		return nil, fmt.Errorf("listing accounts with pending jobs: %w", err)
	}
	defer rows.Close()

	var out []PendingAccount
	for rows.Next() {
		var pa PendingAccount
		if err := rows.Scan(&pa.AccountID, &pa.Provider, &pa.Email, &pa.Tier, &pa.LargestPendingBytes); err != nil {
			return nil, fmt.Errorf("scanning pending account row: %w", err)
		}
		out = append(out, pa)
	}
	return out, rows.Err()
}

// ListPendingJobsOverBytes returns an account's pending jobs larger than limit.
// Used to fail jobs that exceed the account's entire transfer budget, which no
// amount of waiting can fix.
func ListPendingJobsOverBytes(db *sql.DB, accountID, limitBytes int64) ([]*Job, error) {
	rows, err := db.Query(
		`SELECT `+jobColumns+`
		 FROM jobs j JOIN accounts a ON j.account_id = a.id
		 WHERE j.status = 'pending' AND j.account_id = ? AND j.total_bytes > ?
		 ORDER BY j.created_at`,
		accountID, limitBytes,
	)
	if err != nil {
		return nil, fmt.Errorf("listing oversized pending jobs: %w", err)
	}
	defer rows.Close()

	var jobs []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning pending job row: %w", err)
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

// HoldPendingJobs marks an account's pending jobs that do not fit maxBytes with
// a reason, and returns how many rows actually changed. Rows already carrying
// the same reason are left alone, so a caller can log only on a real transition
// instead of once per poll tick.
func HoldPendingJobs(db *sql.DB, accountID, maxBytes int64, reason string) (int64, error) {
	res, err := db.Exec(
		`UPDATE jobs SET hold_reason = ?
		 WHERE status = 'pending' AND account_id = ? AND total_bytes > ?
		   AND COALESCE(hold_reason, '') != ?`,
		reason, accountID, maxBytes, reason,
	)
	if err != nil {
		return 0, fmt.Errorf("holding pending jobs: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return n, nil
}

// ReleasePendingJobs clears the hold on an account's pending jobs that now fit
// maxBytes, returning how many were released. A negative maxBytes releases
// everything, which is what an unlimited budget means.
func ReleasePendingJobs(db *sql.DB, accountID, maxBytes int64) (int64, error) {
	query := `UPDATE jobs SET hold_reason = NULL
	          WHERE status = 'pending' AND account_id = ? AND hold_reason IS NOT NULL`
	args := []any{accountID}
	if maxBytes >= 0 {
		query += ` AND total_bytes <= ?`
		args = append(args, maxBytes)
	}
	res, err := db.Exec(query, args...)
	if err != nil {
		return 0, fmt.Errorf("releasing pending jobs: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return n, nil
}

// UpdateJobProgress persists incremental upload progress for a job.
func UpdateJobProgress(db *sql.DB, id, uploadedBytes int64, chunksUploaded, chunksTotal int) error {
	_, err := db.Exec(
		`UPDATE jobs SET uploaded_bytes = ?, chunks_uploaded = ?, chunks_total = ? WHERE id = ?`,
		uploadedBytes, chunksUploaded, chunksTotal, id,
	)
	if err != nil {
		return fmt.Errorf("updating job progress: %w", err)
	}
	return nil
}

// CompleteJob marks a job complete, recording the provider's remote handle.
func CompleteJob(db *sql.DB, id int64, remotePath string) error {
	_, err := db.Exec(
		`UPDATE jobs SET status = 'complete', remote_path = ?, error_message = NULL, completed_at = CURRENT_TIMESTAMP WHERE id = ?`,
		remotePath, id,
	)
	if err != nil {
		return fmt.Errorf("completing job: %w", err)
	}
	return nil
}

// SetJobVerified records the first-chunk checksum captured during
// verify-on-upload and stamps the job as verified now. Called after a successful
// upload verification so periodic re-verification has a reference to compare
// against later (the temp zip is deleted, so the checksum is the only record).
func SetJobVerified(db *sql.DB, id int64, checksum string) error {
	_, err := db.Exec(
		`UPDATE jobs SET verify_checksum = ?, last_verified_at = CURRENT_TIMESTAMP WHERE id = ?`,
		checksum, id,
	)
	if err != nil {
		return fmt.Errorf("recording job verification: %w", err)
	}
	return nil
}

// TouchJobVerified stamps last_verified_at to now after a successful periodic
// re-verification, leaving the stored checksum unchanged.
func TouchJobVerified(db *sql.DB, id int64) error {
	_, err := db.Exec(`UPDATE jobs SET last_verified_at = CURRENT_TIMESTAMP WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("touching job verification: %w", err)
	}
	return nil
}

// ListJobsDueForReverify returns up to limit completed jobs that carry a stored
// verify checksum and have not been re-verified since olderThan (or never).
// Selection is randomized so the periodic checker samples different files across
// runs rather than always re-checking the same ones.
func ListJobsDueForReverify(db *sql.DB, olderThan time.Time, limit int) ([]*Job, error) {
	if limit <= 0 {
		limit = 1
	}
	rows, err := db.Query(
		`SELECT `+jobColumns+`
		 FROM jobs j
		 JOIN accounts a ON j.account_id = a.id
		 WHERE j.status = 'complete'
		   AND j.verify_checksum IS NOT NULL AND j.verify_checksum != ''
		   AND (j.last_verified_at IS NULL OR j.last_verified_at < ?)
		 ORDER BY RANDOM()
		 LIMIT ?`,
		olderThan, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("listing jobs due for reverify: %w", err)
	}
	defer rows.Close()

	var jobs []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning reverify job row: %w", err)
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

// VerifyChecksum returns the stored first-chunk checksum for a job (empty if
// none recorded). Used by the periodic re-verifier to compare against the
// freshly downloaded head.
func VerifyChecksum(db *sql.DB, id int64) (string, error) {
	var sum sql.NullString
	if err := db.QueryRow(`SELECT verify_checksum FROM jobs WHERE id = ?`, id).Scan(&sum); err != nil {
		return "", fmt.Errorf("reading verify checksum: %w", err)
	}
	return sum.String, nil
}

// FailJob marks a job failed and records the error message.
func FailJob(db *sql.DB, id int64, errMsg string) error {
	_, err := db.Exec(
		`UPDATE jobs SET status = 'failed', error_message = ?, completed_at = CURRENT_TIMESTAMP WHERE id = ?`,
		errMsg, id,
	)
	if err != nil {
		return fmt.Errorf("failing job: %w", err)
	}
	return nil
}

// FailPendingJob fails a job only while it is still pending, and reports whether
// it was this call that did so.
//
// Unlike a claimed job, a job failed for a standing reason (an archive larger
// than its account's whole transfer budget) is spotted by every worker goroutine
// on every poll tick, none of which has claimed it. Without the status guard all
// of them would fail the same job and write the same explanation, filling that
// job's log modal with identical lines.
func FailPendingJob(db *sql.DB, id int64, errMsg string) (bool, error) {
	res, err := db.Exec(
		`UPDATE jobs SET status = 'failed', error_message = ?, hold_reason = NULL,
		                 completed_at = CURRENT_TIMESTAMP
		 WHERE id = ? AND status = 'pending'`,
		errMsg, id,
	)
	if err != nil {
		return false, fmt.Errorf("failing pending job: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, nil
	}
	return n > 0, nil
}

// RequeueStaleJobs resets jobs left in_progress by a previous run back to
// pending so they are retried. Called once at startup, before workers begin.
func RequeueStaleJobs(db *sql.DB) (int64, error) {
	res, err := db.Exec(`UPDATE jobs SET status = 'pending' WHERE status = 'in_progress'`)
	if err != nil {
		return 0, fmt.Errorf("requeuing stale jobs: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// CountUnfinishedJobsForZip reports how many jobs still reference the given zip
// path without having reached a terminal state of 'complete'. The worker uses
// it to decide whether deleting the shared temp zip is safe: several jobs (one
// per account) point at the same zip, so it may only be removed once every
// sibling is done. A failed sibling keeps the count above zero on purpose — the
// zip is retained for retry, per spec.
func CountUnfinishedJobsForZip(db *sql.DB, zipPath string, excludeJobID int64) (int, error) {
	var n int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM jobs WHERE zip_path = ? AND id != ? AND status != 'complete'`,
		zipPath, excludeJobID,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("counting unfinished jobs for zip: %w", err)
	}
	return n, nil
}

// DeleteJob removes a single job row. Used by the per-provider Delete action
// after the remote file has been removed from that provider's storage. The
// backup record, its directories, and any sibling provider's job are untouched
// (job_logs for this job cascade away via the FK).
func DeleteJob(db *sql.DB, id int64) error {
	if _, err := db.Exec(`DELETE FROM jobs WHERE id = ?`, id); err != nil {
		return fmt.Errorf("deleting job: %w", err)
	}
	return nil
}

// UpdateJobStatus is a low-level status setter retained for callers that only
// need a status transition (e.g. resetting a failed job to pending for retry).
func UpdateJobStatus(db *sql.DB, id int64, status, errMsg string) error {
	var query string
	switch status {
	case "in_progress":
		query = `UPDATE jobs SET status = ?, error_message = ?, started_at = CURRENT_TIMESTAMP WHERE id = ?`
	case "complete", "failed":
		query = `UPDATE jobs SET status = ?, error_message = ?, completed_at = CURRENT_TIMESTAMP WHERE id = ?`
	default:
		query = `UPDATE jobs SET status = ?, error_message = ? WHERE id = ?`
	}
	_, err := db.Exec(query, status, errMsg, id)
	if err != nil {
		return fmt.Errorf("updating job status: %w", err)
	}
	return nil
}
