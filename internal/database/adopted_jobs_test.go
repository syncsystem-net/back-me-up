package database

import (
	"testing"
	"time"
)

// An adopted archive must be actionable exactly like an uploaded one: the
// Download and Delete handlers only require status 'complete' and a remote_path,
// so those are what the synthetic row has to carry.
func TestInsertAdoptedJobIsDownloadable(t *testing.T) {
	db := newTestDB(t)

	acctID, err := UpsertAccountRow(db, AccountRow{Provider: "mega", Email: "user@example.com", QuotaGB: 20})
	if err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	backupID, err := UpsertBackupForUser(tx, "user@example.com", "discovered", "")
	if err != nil {
		t.Fatalf("UpsertBackupForUser: %v", err)
	}
	zipID, err := InsertZip(tx, backupID, "bkup001.zip", "", 4096, `{"name":"bkup001"}`)
	if err != nil {
		t.Fatalf("InsertZip: %v", err)
	}
	jobID, err := InsertAdoptedJob(tx, backupID, zipID, acctID, "bkup001.zip", "remote-handle-1", 4096)
	if err != nil {
		t.Fatalf("InsertAdoptedJob: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	job, err := GetJob(db, jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.Status != "complete" {
		t.Errorf("status = %q, want %q", job.Status, "complete")
	}
	if job.RemotePath != "remote-handle-1" {
		t.Errorf("remote_path = %q, want the provider handle", job.RemotePath)
	}
	if job.RemoteName != "bkup001.zip" {
		t.Errorf("remote_name = %q, want %q", job.RemoteName, "bkup001.zip")
	}
	if job.ZipID != zipID {
		t.Errorf("zip_id = %d, want %d", job.ZipID, zipID)
	}
	// No local temp zip ever existed for a discovered archive.
	if job.ZipPath != "" {
		t.Errorf("zip_path = %q, want empty", job.ZipPath)
	}
	// The UI shows a completed transfer rather than a 0% bar.
	if job.UploadedBytes != job.TotalBytes || job.TotalBytes != 4096 {
		t.Errorf("bytes = %d/%d, want 4096/4096", job.UploadedBytes, job.TotalBytes)
	}

	// It must also surface through the per-record listing the table is built from.
	jobs, err := ListJobsByBackup(db, backupID)
	if err != nil {
		t.Fatalf("ListJobsByBackup: %v", err)
	}
	if len(jobs) != 1 || jobs[0].ID != jobID {
		t.Fatalf("expected the adopted job in the record's jobs, got %+v", jobs)
	}
}

// An adopted archive has no local original that was ever hashed, so it carries
// no verify_checksum — and the periodic re-verifier must skip it rather than
// report a mismatch it has no reference for.
func TestAdoptedJobIsSkippedByReverification(t *testing.T) {
	db := newTestDB(t)

	acctID, err := UpsertAccountRow(db, AccountRow{Provider: "mega", Email: "user@example.com", QuotaGB: 20})
	if err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	backupID, err := UpsertBackupForUser(tx, "user@example.com", "discovered", "")
	if err != nil {
		t.Fatalf("UpsertBackupForUser: %v", err)
	}
	zipID, err := InsertZip(tx, backupID, "bkup001.zip", "", 10, "")
	if err != nil {
		t.Fatalf("InsertZip: %v", err)
	}
	if _, err := InsertAdoptedJob(tx, backupID, zipID, acctID, "bkup001.zip", "handle", 10); err != nil {
		t.Fatalf("InsertAdoptedJob: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	due, err := ListJobsDueForReverify(db, time.Now().Add(24*time.Hour), 10)
	if err != nil {
		t.Fatalf("ListJobsDueForReverify: %v", err)
	}
	if len(due) != 0 {
		t.Errorf("adopted jobs must not be selected for re-verification, got %+v", due)
	}
}

func TestGetZipReturnsTheRow(t *testing.T) {
	db := newTestDB(t)

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	backupID, err := UpsertBackupForUser(tx, "user@example.com", "t", "")
	if err != nil {
		t.Fatalf("UpsertBackupForUser: %v", err)
	}
	zipID, err := InsertZip(tx, backupID, "a.zip", "", 7, `{"name":"a"}`)
	if err != nil {
		t.Fatalf("InsertZip: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	z, err := GetZip(db, zipID)
	if err != nil {
		t.Fatalf("GetZip: %v", err)
	}
	if z.Name != "a.zip" || z.BackupID != backupID || z.SizeBytes != 7 {
		t.Errorf("unexpected zip: %+v", z)
	}
}
