package database

import (
	"database/sql"
	"fmt"
	"time"
)

type Job struct {
	ID         int64     `json:"id"`
	BackupID   int64     `json:"backup_id"`
	AccountID  int64     `json:"account_id"`
	Provider   string    `json:"provider"`
	Email      string    `json:"email"`
	Status     string    `json:"status"`
	ZipPath    string    `json:"zip_path"`
	TotalBytes int64     `json:"total_bytes"`
	CreatedAt  time.Time `json:"created_at"`
}

func InsertJob(tx *sql.Tx, backupID, accountID int64, zipPath string, totalBytes int64) (int64, error) {
	res, err := tx.Exec(
		`INSERT INTO jobs (backup_id, account_id, zip_path, total_bytes) VALUES (?, ?, ?, ?)`,
		backupID, accountID, zipPath, totalBytes,
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

func ListJobsByBackup(db *sql.DB, backupID int64) ([]*Job, error) {
	rows, err := db.Query(
		`SELECT j.id, j.backup_id, j.account_id, a.provider, a.email, j.status, COALESCE(j.zip_path, ''), j.total_bytes, j.created_at
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
		j := &Job{}
		if err := rows.Scan(&j.ID, &j.BackupID, &j.AccountID, &j.Provider, &j.Email, &j.Status, &j.ZipPath, &j.TotalBytes, &j.CreatedAt); err != nil {
			return nil, fmt.Errorf("scanning job row: %w", err)
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

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
