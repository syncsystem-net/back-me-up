package database

import (
	"database/sql"
	"fmt"
	"time"
)

type Job struct {
	ID             int64     `json:"id"`
	BackupID       int64     `json:"backup_id"`
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
}

func InsertJob(tx *sql.Tx, backupID, accountID int64, zipPath, remoteName string, totalBytes int64) (int64, error) {
	res, err := tx.Exec(
		`INSERT INTO jobs (backup_id, account_id, zip_path, remote_name, total_bytes) VALUES (?, ?, ?, ?, ?)`,
		backupID, accountID, zipPath, remoteName, totalBytes,
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

const jobColumns = `j.id, j.backup_id, j.account_id, a.provider, a.email, j.status,
	COALESCE(j.zip_path, ''), COALESCE(j.remote_path, ''), COALESCE(j.remote_name, ''), j.total_bytes,
	j.uploaded_bytes, j.chunks_total, j.chunks_uploaded, COALESCE(j.error_message, ''), j.created_at`

func scanJob(s interface {
	Scan(...any) error
}) (*Job, error) {
	j := &Job{}
	if err := s.Scan(&j.ID, &j.BackupID, &j.AccountID, &j.Provider, &j.Email, &j.Status,
		&j.ZipPath, &j.RemotePath, &j.RemoteName, &j.TotalBytes, &j.UploadedBytes, &j.ChunksTotal,
		&j.ChunksUploaded, &j.ErrorMessage, &j.CreatedAt); err != nil {
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

// ClaimNextPendingJob atomically marks the oldest pending job as in_progress and
// returns it, so no two workers pick up the same job. It returns (nil, nil) when
// there is no pending work. The UPDATE ... RETURNING is a single statement and
// therefore atomic under SQLite's write lock.
func ClaimNextPendingJob(db *sql.DB) (*Job, error) {
	var id int64
	err := db.QueryRow(
		`UPDATE jobs SET status = 'in_progress', started_at = CURRENT_TIMESTAMP, error_message = NULL
		 WHERE id = (
		     SELECT id FROM jobs WHERE status = 'pending' ORDER BY created_at LIMIT 1
		 )
		 RETURNING id`,
	).Scan(&id)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claiming job: %w", err)
	}
	return GetJob(db, id)
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
