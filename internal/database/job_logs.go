package database

import (
	"database/sql"
	"fmt"
	"time"
)

// JobLog is a single timestamped log line attached to a job. The UI surfaces
// these in the per-provider "logs" modal so a user can see why an upload failed.
type JobLog struct {
	ID        int64     `json:"id"`
	JobID     int64     `json:"job_id"`
	Level     string    `json:"level"`
	Message   string    `json:"message"`
	CreatedAt time.Time `json:"created_at"`
}

// InsertJobLog appends a log line for a job. Errors are returned but callers
// typically log-and-continue: a failed log write must not abort an upload.
func InsertJobLog(db *sql.DB, jobID int64, level, message string) error {
	_, err := db.Exec(
		`INSERT INTO job_logs (job_id, level, message) VALUES (?, ?, ?)`,
		jobID, level, message,
	)
	if err != nil {
		return fmt.Errorf("inserting job log: %w", err)
	}
	return nil
}

// ListJobLogs returns a job's log lines oldest-first.
func ListJobLogs(db *sql.DB, jobID int64) ([]*JobLog, error) {
	rows, err := db.Query(
		`SELECT id, job_id, level, message, created_at FROM job_logs WHERE job_id = ? ORDER BY created_at, id`,
		jobID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying job logs: %w", err)
	}
	defer rows.Close()

	var logs []*JobLog
	for rows.Next() {
		l := &JobLog{}
		if err := rows.Scan(&l.ID, &l.JobID, &l.Level, &l.Message, &l.CreatedAt); err != nil {
			return nil, fmt.Errorf("scanning job log: %w", err)
		}
		logs = append(logs, l)
	}
	return logs, rows.Err()
}
