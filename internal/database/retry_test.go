package database

import (
	"database/sql"
	"testing"
)

func seedRetryJob(t *testing.T) (db *sql.DB, backupID, accountID, jobID int64) {
	t.Helper()
	db = newTestDB(t)
	accountID, err := UpsertAccountRow(db, AccountRow{Provider: "mega", Email: "user@example.com", QuotaGB: 20})
	if err != nil {
		t.Fatalf("UpsertAccountRow: %v", err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	backupID, err = UpsertBackupForUser(tx, "user@example.com", "t", "")
	if err != nil {
		t.Fatalf("UpsertBackupForUser: %v", err)
	}
	zipID, err := InsertZip(tx, backupID, "bkup001.zip", "", 10, `{"name":"bkup001"}`)
	if err != nil {
		t.Fatalf("InsertZip: %v", err)
	}
	jobID, err = InsertJob(tx, backupID, zipID, accountID, `C:\bkup001.zip`, "bkup001.zip", 10)
	if err != nil {
		t.Fatalf("InsertJob: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return db, backupID, accountID, jobID
}

// The guard is what makes a double click harmless: only a failed job moves, and
// exactly one of two concurrent retries reports that it moved it.
func TestRetryFailedJobOnlyMovesAFailedJob(t *testing.T) {
	db, _, _, jobID := seedRetryJob(t)

	ok, err := RetryFailedJob(db, jobID)
	if err != nil {
		t.Fatalf("RetryFailedJob(pending): %v", err)
	}
	if ok {
		t.Fatal("a pending job was reported as retried")
	}

	if err := FailJob(db, jobID, "boom"); err != nil {
		t.Fatalf("FailJob: %v", err)
	}
	failed, err := GetJob(db, jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if failed.CompletedAt == nil {
		t.Error("a failed job carries no completed_at")
	}

	if ok, err := RetryFailedJob(db, jobID); err != nil || !ok {
		t.Fatalf("RetryFailedJob(failed) = %v, %v; want true", ok, err)
	}
	if ok, err := RetryFailedJob(db, jobID); err != nil || ok {
		t.Fatalf("second RetryFailedJob = %v, %v; want false", ok, err)
	}

	job, err := GetJob(db, jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.Status != "pending" || job.ErrorMessage != "" || job.CompletedAt != nil || job.StartedAt != nil {
		t.Errorf("job after retry = %+v", job)
	}
	var errNull bool
	if err := db.QueryRow(`SELECT error_message IS NULL FROM jobs WHERE id = ?`, jobID).Scan(&errNull); err != nil {
		t.Fatalf("reading error_message: %v", err)
	}
	if !errNull {
		t.Error("error_message should be NULL after a retry, the convention for 'no error'")
	}
}

func TestLaterJobForSameArchive(t *testing.T) {
	db, backupID, accountID, jobID := seedRetryJob(t)
	job, err := GetJob(db, jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if later, err := LaterJobForSameArchive(db, job); err != nil || later != nil {
		t.Fatalf("with no later job: %v, %v", later, err)
	}

	tx, _ := db.Begin()
	zipID, _ := InsertZip(tx, backupID, "bkup002.zip", "", 10, "")
	// A later job for a different archive does not supersede this one.
	if _, err := InsertJob(tx, backupID, zipID, accountID, `C:\bkup002.zip`, "bkup002.zip", 10); err != nil {
		t.Fatalf("InsertJob: %v", err)
	}
	sameID, err := InsertJob(tx, backupID, zipID, accountID, `C:\bkup001-2.zip`, "bkup001.zip", 10)
	if err != nil {
		t.Fatalf("InsertJob: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	later, err := LaterJobForSameArchive(db, job)
	if err != nil {
		t.Fatalf("LaterJobForSameArchive: %v", err)
	}
	if later == nil || later.ID != sameID {
		t.Fatalf("later = %+v, want job #%d", later, sameID)
	}
}
