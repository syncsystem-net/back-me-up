package database

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// legacySchema is the pre-PR#7 shape: backups without owner_email, jobs without
// zip_id, and no backup_zips table.
const legacySchema = `
CREATE TABLE backups (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    title TEXT NOT NULL,
    source_path TEXT NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE accounts (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    provider TEXT NOT NULL,
    email TEXT NOT NULL,
    quota_total_gb REAL DEFAULT 0,
    quota_used_gb REAL DEFAULT 0,
    last_quota_sync DATETIME,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(provider, email)
);
CREATE TABLE backup_directories (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    backup_id INTEGER NOT NULL,
    path TEXT NOT NULL,
    name TEXT NOT NULL,
    level INTEGER NOT NULL,
    size_bytes INTEGER DEFAULT 0,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (backup_id) REFERENCES backups(id) ON DELETE CASCADE
);
CREATE TABLE jobs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    backup_id INTEGER NOT NULL,
    account_id INTEGER NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending',
    zip_path TEXT,
    remote_path TEXT,
    remote_name TEXT,
    total_bytes INTEGER DEFAULT 0,
    uploaded_bytes INTEGER DEFAULT 0,
    chunks_total INTEGER DEFAULT 0,
    chunks_uploaded INTEGER DEFAULT 0,
    error_message TEXT,
    verify_checksum TEXT,
    last_verified_at DATETIME,
    started_at DATETIME,
    completed_at DATETIME,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (backup_id) REFERENCES backups(id) ON DELETE CASCADE,
    FOREIGN KEY (account_id) REFERENCES accounts(id)
);
CREATE TABLE job_logs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    job_id INTEGER NOT NULL,
    level TEXT NOT NULL,
    message TEXT NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (job_id) REFERENCES jobs(id) ON DELETE CASCADE
);
`

// TestMigrateFromLegacySchema upgrades a pre-PR#7 database in place. It guards
// the ordering hazard that indexes over ALTER-added columns (jobs.zip_id) must
// be created after the column exists — a fresh-database test cannot catch it,
// because there CREATE TABLE already includes the column.
func TestMigrateFromLegacySchema(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.db")

	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := raw.Exec(legacySchema); err != nil {
		t.Fatalf("creating legacy schema: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO accounts (provider, email) VALUES ('mega', 'old@example.com')`); err != nil {
		t.Fatalf("seeding account: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO backups (title, source_path) VALUES ('legacy', 'C:\old')`); err != nil {
		t.Fatalf("seeding backup: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO jobs (backup_id, account_id, status) VALUES (1, 1, 'complete')`); err != nil {
		t.Fatalf("seeding job: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO job_logs (job_id, level, message) VALUES (1, 'info', 'hi')`); err != nil {
		t.Fatalf("seeding job log: %v", err)
	}
	raw.Close()

	// The real migration path.
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open (migrate legacy): %v", err)
	}
	defer db.Close()

	for _, c := range []struct{ table, column string }{
		{"backups", "owner_email"},
		{"jobs", "zip_id"},
	} {
		ok, err := columnExists(db, c.table, c.column)
		if err != nil {
			t.Fatalf("columnExists(%s.%s): %v", c.table, c.column, err)
		}
		if !ok {
			t.Fatalf("expected %s.%s to exist after migration", c.table, c.column)
		}
	}

	// backup_zips must have been created, and legacy rows cleared.
	if _, err := db.Exec(`SELECT 1 FROM backup_zips LIMIT 1`); err != nil {
		t.Fatalf("backup_zips missing after migration: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM backups`).Scan(&n); err != nil {
		t.Fatalf("counting backups: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected legacy backups cleared, got %d", n)
	}

	// Accounts survive the migration (they are the user list).
	if err := db.QueryRow(`SELECT COUNT(*) FROM accounts`).Scan(&n); err != nil {
		t.Fatalf("counting accounts: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected accounts preserved, got %d", n)
	}

	// Migration must be idempotent — a second Open is a no-op.
	db.Close()
	db2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open (second run): %v", err)
	}
	db2.Close()
}

// TestPerUserBackupFlow exercises the PR #7 schema end to end on a fresh temp
// database: migrate, upsert a per-user record (idempotently), attach a zip, and
// create a job referencing that zip. It guards the reshaped relationships
// (backups.owner_email, backup_zips, jobs.zip_id).
func TestPerUserBackupFlow(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	acctID, err := UpsertAccount(db, "mega", "user@example.com", 20)
	if err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}

	// The user has no record yet.
	if b, err := GetBackupByOwner(db, "user@example.com"); err != nil {
		t.Fatalf("GetBackupByOwner (empty): %v", err)
	} else if b != nil {
		t.Fatalf("expected no backup, got %+v", b)
	}

	// First upload creates the record + a zip + a job.
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	backupID, err := UpsertBackupForUser(tx, "user@example.com", "My Backup", `C:\src\docs`)
	if err != nil {
		t.Fatalf("UpsertBackupForUser: %v", err)
	}
	zipID, err := InsertZip(tx, backupID, "docs.zip", `C:\src\docs`, 1024, `{"name":"docs"}`)
	if err != nil {
		t.Fatalf("InsertZip: %v", err)
	}
	if _, err := InsertJob(tx, backupID, zipID, acctID, `C:\tmp\docs-x.zip`, "docs.zip", 1024); err != nil {
		t.Fatalf("InsertJob: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Re-uploading for the same user reuses the record (one record per user) and
	// may rename it, while accumulating a second zip.
	tx2, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin 2: %v", err)
	}
	backupID2, err := UpsertBackupForUser(tx2, "user@example.com", "Renamed", `C:\src\photos`)
	if err != nil {
		t.Fatalf("UpsertBackupForUser 2: %v", err)
	}
	if backupID2 != backupID {
		t.Fatalf("expected same backup id, got %d != %d", backupID2, backupID)
	}
	if _, err := InsertZip(tx2, backupID2, "photos.zip", `C:\src\photos`, 2048, `{"name":"photos"}`); err != nil {
		t.Fatalf("InsertZip 2: %v", err)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatalf("Commit 2: %v", err)
	}

	b, err := GetBackupByOwner(db, "user@example.com")
	if err != nil || b == nil {
		t.Fatalf("GetBackupByOwner: %v (b=%v)", err, b)
	}
	if b.OwnerEmail != "user@example.com" || b.Title != "Renamed" {
		t.Fatalf("unexpected backup: %+v", b)
	}

	zips, err := ListZipsByBackup(db, b.ID)
	if err != nil {
		t.Fatalf("ListZipsByBackup: %v", err)
	}
	if len(zips) != 2 {
		t.Fatalf("expected 2 zips, got %d", len(zips))
	}

	jobs, err := ListJobsByBackup(db, b.ID)
	if err != nil {
		t.Fatalf("ListJobsByBackup: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	if jobs[0].ZipID != zipID {
		t.Fatalf("job zip_id = %d, want %d", jobs[0].ZipID, zipID)
	}

	// Deleting the record cascades zips and jobs away.
	if err := DeleteBackupRecord(db, b.ID); err != nil {
		t.Fatalf("DeleteBackupRecord: %v", err)
	}
	if zips, err := ListZipsByBackup(db, b.ID); err != nil {
		t.Fatalf("ListZipsByBackup after delete: %v", err)
	} else if len(zips) != 0 {
		t.Fatalf("expected 0 zips after delete, got %d", len(zips))
	}
}
