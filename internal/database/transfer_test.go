package database

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// The upgrade path, not the greenfield one. A database that predates PR #14 has
// an accounts table without tier, a jobs table without hold_reason, and no
// transfer_usage table at all — and CREATE TABLE IF NOT EXISTS is a no-op on the
// two that already exist, so only the ALTERs can add those columns.
func TestMigrateAddsTierHoldAndTransferLedgerToAnExistingDatabase(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "upgrade14.db")

	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	// The schema as it stood after PR #13: accounts with credential columns but
	// no tier, jobs with no hold_reason.
	if _, err := raw.Exec(`
		CREATE TABLE accounts (
		    id INTEGER PRIMARY KEY AUTOINCREMENT,
		    provider TEXT NOT NULL,
		    email TEXT NOT NULL,
		    quota_total_gb REAL DEFAULT 0,
		    quota_used_gb REAL DEFAULT 0,
		    last_quota_sync DATETIME,
		    secrets_enc BLOB,
		    needs_reauth INTEGER NOT NULL DEFAULT 0,
		    reauth_reason TEXT,
		    token_source TEXT,
		    env_index INTEGER NOT NULL DEFAULT 0,
		    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		    UNIQUE(provider, email)
		);
		CREATE TABLE backups (
		    id INTEGER PRIMARY KEY AUTOINCREMENT,
		    owner_email TEXT,
		    title TEXT NOT NULL,
		    source_path TEXT NOT NULL,
		    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE jobs (
		    id INTEGER PRIMARY KEY AUTOINCREMENT,
		    backup_id INTEGER NOT NULL,
		    zip_id INTEGER,
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
		    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);`); err != nil {
		t.Fatalf("creating pre-14 schema: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO accounts (provider, email, quota_total_gb) VALUES ('mega', 'kept@example.com', 20)`); err != nil {
		t.Fatalf("seeding account: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO backups (owner_email, title, source_path) VALUES ('kept@example.com', 'old', 'C:\tmp')`); err != nil {
		t.Fatalf("seeding backup: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO jobs (backup_id, account_id, status, total_bytes) VALUES (1, 1, 'pending', 500)`); err != nil {
		t.Fatalf("seeding job: %v", err)
	}
	raw.Close()

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open (migrate): %v", err)
	}
	defer db.Close()

	// An upgraded account defaults to free, which is the conservative tier.
	accts, err := ListDBAccounts(db)
	if err != nil {
		t.Fatalf("ListDBAccounts after migration: %v", err)
	}
	if len(accts) != 1 || accts[0].Email != "kept@example.com" {
		t.Fatalf("the pre-existing account did not survive: %+v", accts)
	}
	if accts[0].Tier != "free" {
		t.Errorf("upgraded account tier = %q, want free", accts[0].Tier)
	}

	// Every read the app performs must work against the upgraded tables.
	if _, err := ListAccountRows(db); err != nil {
		t.Fatalf("ListAccountRows after migration: %v", err)
	}
	if _, err := ListAccountsWithPendingJobs(db); err != nil {
		t.Fatalf("ListAccountsWithPendingJobs after migration: %v", err)
	}
	if _, err := ListJobsByBackup(db, 1); err != nil {
		t.Fatalf("ListJobsByBackup after migration: %v", err)
	}
	// And the ledger must exist, since nothing created it before this migration.
	if err := RecordTransfer(db, 1, 100); err != nil {
		t.Fatalf("RecordTransfer after migration: %v", err)
	}
}

func TestTransferLedgerCountsOnlyTheCurrentWindow(t *testing.T) {
	db := newTestDB(t)
	seedAccount(t, db, "mega", "a@example.com")

	if err := RecordTransfer(db, 1, 300); err != nil {
		t.Fatalf("RecordTransfer: %v", err)
	}
	// A transfer well outside any window we ask about.
	if _, err := db.Exec(
		`INSERT INTO transfer_usage (account_id, bytes, at) VALUES (1, 900, datetime('now', '-48 hours'))`,
	); err != nil {
		t.Fatalf("seeding old transfer: %v", err)
	}

	used, err := TransferUsedSince(db, 1, 24)
	if err != nil {
		t.Fatalf("TransferUsedSince: %v", err)
	}
	if used != 300 {
		t.Errorf("used in a 24h window = %d, want 300 — the 48h-old transfer must have aged out", used)
	}

	used, err = TransferUsedSince(db, 1, 72)
	if err != nil {
		t.Fatalf("TransferUsedSince: %v", err)
	}
	if used != 1200 {
		t.Errorf("used in a 72h window = %d, want 1200", used)
	}
}

func TestTransferUsedIsZeroWithoutAWindow(t *testing.T) {
	db := newTestDB(t)
	seedAccount(t, db, "mega", "a@example.com")
	if err := RecordTransfer(db, 1, 500); err != nil {
		t.Fatalf("RecordTransfer: %v", err)
	}
	used, err := TransferUsedSince(db, 1, 0)
	if err != nil {
		t.Fatalf("TransferUsedSince: %v", err)
	}
	if used != 0 {
		t.Errorf("used with no window = %d, want 0", used)
	}
}

// An over-budget account must not stop other accounts' jobs from being claimed.
// This is the difference between "this account waits" and "the queue waits".
func TestAnOverBudgetAccountDoesNotBlockOtherAccounts(t *testing.T) {
	db := newTestDB(t)
	seedAccount(t, db, "fourshared", "held@example.com") // id 1
	seedAccount(t, db, "mega", "free@example.com")       // id 2
	backupID := seedBackup(t, db, "held@example.com")

	// The held account's job is queued first, so an unconstrained claim would
	// take it and nothing else would ever run.
	seedPendingJob(t, db, backupID, 1, 5_000)
	seedPendingJob(t, db, backupID, 2, 5_000)

	job, err := ClaimNextPendingJobWithin(db, []Allowance{
		{AccountID: 1, MaxBytes: 100}, // over budget: 5000 does not fit
		{AccountID: 2, MaxBytes: -1},  // unlimited
	})
	if err != nil {
		t.Fatalf("ClaimNextPendingJobWithin: %v", err)
	}
	if job == nil {
		t.Fatal("claimed nothing; the unconstrained account's job should have been taken")
	}
	if job.AccountID != 2 {
		t.Errorf("claimed account %d, want 2 — the over-budget account's job was taken instead", job.AccountID)
	}
}

func TestClaimTakesNothingWhenEveryAccountIsOverBudget(t *testing.T) {
	db := newTestDB(t)
	seedAccount(t, db, "fourshared", "held@example.com")
	backupID := seedBackup(t, db, "held@example.com")
	seedPendingJob(t, db, backupID, 1, 5_000)

	job, err := ClaimNextPendingJobWithin(db, []Allowance{{AccountID: 1, MaxBytes: 100}})
	if err != nil {
		t.Fatalf("ClaimNextPendingJobWithin: %v", err)
	}
	if job != nil {
		t.Errorf("claimed job %d despite the budget; a held job must stay pending", job.ID)
	}

	// It must still be pending — held, never failed.
	var status string
	if err := db.QueryRow(`SELECT status FROM jobs WHERE id = 1`).Scan(&status); err != nil {
		t.Fatalf("reading status: %v", err)
	}
	if status != "pending" {
		t.Errorf("status = %q, want pending", status)
	}
}

// An empty (non-nil) allowance list means every account with pending work is
// over budget, which must claim nothing rather than falling back to unrestricted.
func TestEmptyAllowancesClaimNothingButNilClaimsFreely(t *testing.T) {
	db := newTestDB(t)
	seedAccount(t, db, "mega", "a@example.com")
	backupID := seedBackup(t, db, "a@example.com")
	seedPendingJob(t, db, backupID, 1, 500)

	if job, err := ClaimNextPendingJobWithin(db, []Allowance{}); err != nil {
		t.Fatalf("ClaimNextPendingJobWithin: %v", err)
	} else if job != nil {
		t.Error("an empty allowance list must claim nothing")
	}

	job, err := ClaimNextPendingJobWithin(db, nil)
	if err != nil {
		t.Fatalf("ClaimNextPendingJobWithin(nil): %v", err)
	}
	if job == nil {
		t.Fatal("a nil allowance list means no restriction and must claim")
	}
}

// The invariant a precomputed allowance cannot hold. Two workers both compute
// "3000 left" before either claims; if the claim trusted that figure, both would
// take a 1900-byte job and 3800 would go out against a 3000 budget. Counting
// in-flight work inside the claim statement is what makes the ceiling real.
func TestASecondClaimCannotExceedTheBudgetUsingAStaleAllowance(t *testing.T) {
	db := newTestDB(t)
	seedAccount(t, db, "fourshared", "a@example.com")
	backupID := seedBackup(t, db, "a@example.com")
	seedPendingJob(t, db, backupID, 1, 1_900)
	seedPendingJob(t, db, backupID, 1, 1_900)

	// Both workers read the same allowance before either has claimed.
	allow := []Allowance{{AccountID: 1, MaxBytes: 3_000}}

	first, err := ClaimNextPendingJobWithin(db, allow)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if first == nil {
		t.Fatal("first claim took nothing; 1900 fits in 3000")
	}

	// The second worker acts on its now-stale copy of the same allowance.
	second, err := ClaimNextPendingJobWithin(db, allow)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if second != nil {
		t.Errorf("second claim took job %d: 1900 + 1900 exceeds the 3000 budget, and the first is still in flight", second.ID)
	}

	// Once the first finishes and leaves in_progress, the second fits again —
	// against a ledger that now records what was actually sent.
	if err := CompleteJob(db, first.ID, "ref"); err != nil {
		t.Fatalf("CompleteJob: %v", err)
	}
	if job, err := ClaimNextPendingJobWithin(db, []Allowance{{AccountID: 1, MaxBytes: 1_100}}); err != nil {
		t.Fatalf("third claim: %v", err)
	} else if job != nil {
		t.Error("claimed against an 1100-byte allowance with a 1900-byte job")
	}
	if job, err := ClaimNextPendingJobWithin(db, []Allowance{{AccountID: 1, MaxBytes: 3_000}}); err != nil {
		t.Fatalf("fourth claim: %v", err)
	} else if job == nil {
		t.Error("nothing claimed once the in-flight upload finished and the budget allowed it")
	}
}

// An unlimited allowance is the escape hatch and must not be narrowed by
// in-flight accounting.
func TestInFlightWorkDoesNotConstrainAnUnlimitedAllowance(t *testing.T) {
	db := newTestDB(t)
	seedAccount(t, db, "mega", "a@example.com")
	backupID := seedBackup(t, db, "a@example.com")
	seedPendingJob(t, db, backupID, 1, 9_000)
	if _, err := db.Exec(
		`INSERT INTO jobs (backup_id, account_id, status, total_bytes, zip_path, remote_name)
		 VALUES (?, 1, 'in_progress', 500000, 'z.zip', 'r.zip')`, backupID); err != nil {
		t.Fatalf("seeding in-flight job: %v", err)
	}

	job, err := ClaimNextPendingJobWithin(db, []Allowance{{AccountID: 1, MaxBytes: -1}})
	if err != nil {
		t.Fatalf("ClaimNextPendingJobWithin: %v", err)
	}
	if job == nil {
		t.Error("an unlimited allowance must claim regardless of what is in flight")
	}
}

func TestHoldAndReleaseOnlyReportRealTransitions(t *testing.T) {
	db := newTestDB(t)
	seedAccount(t, db, "fourshared", "a@example.com")
	backupID := seedBackup(t, db, "a@example.com")
	seedPendingJob(t, db, backupID, 1, 5_000)
	seedPendingJob(t, db, backupID, 1, 100)

	// Only the 5000-byte job exceeds the 1000-byte allowance.
	n, err := HoldPendingJobs(db, 1, 1_000, "waiting for the budget")
	if err != nil {
		t.Fatalf("HoldPendingJobs: %v", err)
	}
	if n != 1 {
		t.Errorf("held %d jobs, want 1", n)
	}

	// Holding again with the same reason must report no change, or a standing
	// hold would log once per poll tick for hours.
	n, err = HoldPendingJobs(db, 1, 1_000, "waiting for the budget")
	if err != nil {
		t.Fatalf("HoldPendingJobs (repeat): %v", err)
	}
	if n != 0 {
		t.Errorf("re-holding reported %d changes, want 0", n)
	}

	// Once the window frees up, the hold is withdrawn.
	n, err = ReleasePendingJobs(db, 1, 10_000)
	if err != nil {
		t.Fatalf("ReleasePendingJobs: %v", err)
	}
	if n != 1 {
		t.Errorf("released %d jobs, want 1", n)
	}

	jobs, err := ListJobsByBackup(db, backupID)
	if err != nil {
		t.Fatalf("ListJobsByBackup: %v", err)
	}
	for _, j := range jobs {
		if j.HoldReason != "" {
			t.Errorf("job %d still holds %q after release", j.ID, j.HoldReason)
		}
	}
}

func TestClaimingClearsTheHoldReason(t *testing.T) {
	db := newTestDB(t)
	seedAccount(t, db, "mega", "a@example.com")
	backupID := seedBackup(t, db, "a@example.com")
	seedPendingJob(t, db, backupID, 1, 500)

	if _, err := HoldPendingJobs(db, 1, 100, "held"); err != nil {
		t.Fatalf("HoldPendingJobs: %v", err)
	}
	job, err := ClaimNextPendingJobWithin(db, []Allowance{{AccountID: 1, MaxBytes: -1}})
	if err != nil {
		t.Fatalf("ClaimNextPendingJobWithin: %v", err)
	}
	if job == nil {
		t.Fatal("expected to claim the job once the allowance was unlimited")
	}
	if job.HoldReason != "" {
		t.Errorf("claimed job still carries hold reason %q", job.HoldReason)
	}
}

func TestListAccountsWithPendingJobsReportsTheLargest(t *testing.T) {
	db := newTestDB(t)
	seedAccount(t, db, "fourshared", "a@example.com")
	backupID := seedBackup(t, db, "a@example.com")
	seedPendingJob(t, db, backupID, 1, 100)
	seedPendingJob(t, db, backupID, 1, 9_000)

	pending, err := ListAccountsWithPendingJobs(db)
	if err != nil {
		t.Fatalf("ListAccountsWithPendingJobs: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("got %d accounts, want 1", len(pending))
	}
	if pending[0].LargestPendingBytes != 9_000 {
		t.Errorf("largest = %d, want 9000 — this is what detects a job bigger than the whole budget",
			pending[0].LargestPendingBytes)
	}
	if pending[0].Tier != "free" {
		t.Errorf("tier = %q, want free", pending[0].Tier)
	}
}

func TestPruneTransfersDropsOnlyOldRows(t *testing.T) {
	db := newTestDB(t)
	seedAccount(t, db, "mega", "a@example.com")
	if err := RecordTransfer(db, 1, 100); err != nil {
		t.Fatalf("RecordTransfer: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO transfer_usage (account_id, bytes, at) VALUES (1, 900, datetime('now', '-60 days'))`,
	); err != nil {
		t.Fatalf("seeding ancient transfer: %v", err)
	}

	n, err := PruneTransfers(db)
	if err != nil {
		t.Fatalf("PruneTransfers: %v", err)
	}
	if n != 1 {
		t.Errorf("pruned %d rows, want 1", n)
	}
	used, err := TransferUsedSince(db, 1, 24)
	if err != nil {
		t.Fatalf("TransferUsedSince: %v", err)
	}
	if used != 100 {
		t.Errorf("recent usage = %d after pruning, want 100", used)
	}
}

func seedAccount(t *testing.T, db *sql.DB, provider, email string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO accounts (provider, email) VALUES (?, ?)`, provider, email); err != nil {
		t.Fatalf("seeding account %s/%s: %v", provider, email, err)
	}
}

func seedBackup(t *testing.T, db *sql.DB, owner string) int64 {
	t.Helper()
	res, err := db.Exec(`INSERT INTO backups (owner_email, title, source_path) VALUES (?, 'title', 'src')`, owner)
	if err != nil {
		t.Fatalf("seeding backup: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

func seedPendingJob(t *testing.T, db *sql.DB, backupID, accountID, bytes int64) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO jobs (backup_id, account_id, status, total_bytes, zip_path, remote_name)
		 VALUES (?, ?, 'pending', ?, 'z.zip', 'r.zip')`,
		backupID, accountID, bytes,
	); err != nil {
		t.Fatalf("seeding job: %v", err)
	}
}
