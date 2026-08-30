package worker

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/syncsystem-net/back-me-up/internal/database"
	"github.com/syncsystem-net/back-me-up/internal/limits"
)

// budgetFixture opens a migrated database with one free 4shared account (id 1),
// sets that provider/tier's limits, and returns a Worker wired to it.
func budgetFixture(t *testing.T, lim limits.Limit) (*Worker, *sql.DB) {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "budget.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if _, err := db.Exec(`INSERT INTO accounts (provider, email, tier) VALUES ('fourshared', 'a@example.com', 'free')`); err != nil {
		t.Fatalf("seeding account: %v", err)
	}
	set := limits.Defaults()
	set["fourshared"][limits.TierFree] = lim
	if err := database.SetProviderLimits(db, set); err != nil {
		t.Fatalf("SetProviderLimits: %v", err)
	}
	return &Worker{db: db}, db
}

func seedJob(t *testing.T, db *sql.DB, bytes int64) int64 {
	t.Helper()
	res, err := db.Exec(`INSERT INTO backups (owner_email, title, source_path) VALUES ('a@example.com', 't', 's')`)
	if err != nil {
		t.Fatalf("seeding backup: %v", err)
	}
	backupID, _ := res.LastInsertId()
	res, err = db.Exec(
		`INSERT INTO jobs (backup_id, account_id, status, total_bytes, zip_path, remote_name)
		 VALUES (?, 1, 'pending', ?, 'z.zip', 'r.zip')`, backupID, bytes)
	if err != nil {
		t.Fatalf("seeding job: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

func jobStatus(t *testing.T, db *sql.DB, id int64) (status, hold string) {
	t.Helper()
	var h sql.NullString
	if err := db.QueryRow(`SELECT status, hold_reason FROM jobs WHERE id = ?`, id).Scan(&status, &h); err != nil {
		t.Fatalf("reading job %d: %v", id, err)
	}
	return status, h.String
}

// An unlimited budget is the escape hatch from a number we guessed wrong, so it
// must impose nothing at all.
func TestNoBudgetMeansAnUnlimitedAllowance(t *testing.T) {
	w, db := budgetFixture(t, limits.Limit{MaxFileBytes: 1_000, TransferBytes: 0, WindowHours: 24})
	id := seedJob(t, db, 5_000)

	allow, err := w.allowances()
	if err != nil {
		t.Fatalf("allowances: %v", err)
	}
	if len(allow) != 1 || !allow[0].Unlimited() {
		t.Fatalf("allowances = %+v, want one unlimited entry", allow)
	}
	if status, hold := jobStatus(t, db, id); status != "pending" || hold != "" {
		t.Errorf("job = %q/%q, want an untouched pending job", status, hold)
	}
}

// The allowance shrinks by what the window has already consumed.
func TestAllowanceReflectsWhatTheWindowHasSpent(t *testing.T) {
	w, db := budgetFixture(t, limits.Limit{TransferBytes: 10_000, WindowHours: 24})
	seedJob(t, db, 1_000)
	if err := database.RecordTransfer(db, 1, 6_000); err != nil {
		t.Fatalf("RecordTransfer: %v", err)
	}

	allow, err := w.allowances()
	if err != nil {
		t.Fatalf("allowances: %v", err)
	}
	if len(allow) != 1 || allow[0].MaxBytes != 4_000 {
		t.Fatalf("allowance = %+v, want 4000 remaining", allow)
	}
}

// A job that does not fit right now is held, not failed, and the hold explains
// itself — otherwise it is indistinguishable from a stuck job.
func TestAJobOverTheRemainingBudgetIsHeldWithAReason(t *testing.T) {
	w, db := budgetFixture(t, limits.Limit{TransferBytes: 10_000, WindowHours: 24})
	id := seedJob(t, db, 5_000)
	if err := database.RecordTransfer(db, 1, 8_000); err != nil {
		t.Fatalf("RecordTransfer: %v", err)
	}

	if _, err := w.allowances(); err != nil {
		t.Fatalf("allowances: %v", err)
	}

	status, hold := jobStatus(t, db, id)
	if status != "pending" {
		t.Errorf("status = %q, want pending: a held job must never be failed", status)
	}
	if hold == "" {
		t.Error("held job carries no reason; it would look stuck")
	}
}

// A job larger than the entire budget can never run. Holding it would be waiting
// for something that cannot happen, so it fails with the way out named.
func TestAJobLargerThanTheWholeBudgetFailsRatherThanWaitingForever(t *testing.T) {
	w, db := budgetFixture(t, limits.Limit{TransferBytes: 1_000, WindowHours: 24})
	id := seedJob(t, db, 5_000)

	if _, err := w.allowances(); err != nil {
		t.Fatalf("allowances: %v", err)
	}

	status, _ := jobStatus(t, db, id)
	if status != "failed" {
		t.Fatalf("status = %q, want failed", status)
	}

	logs, err := database.ListJobLogs(db, id)
	if err != nil {
		t.Fatalf("ListJobLogs: %v", err)
	}
	var explained bool
	for _, l := range logs {
		if l.Level == "error" && len(l.Message) > 0 {
			explained = true
		}
	}
	if !explained {
		t.Error("no error log explaining why the job can never run")
	}
}

// Once the window frees up the hold is withdrawn, so a hold that cannot be
// withdrawn is impossible by construction.
func TestAHoldIsWithdrawnWhenTheBudgetAllowsItAgain(t *testing.T) {
	w, db := budgetFixture(t, limits.Limit{TransferBytes: 10_000, WindowHours: 24})
	id := seedJob(t, db, 5_000)
	if err := database.RecordTransfer(db, 1, 8_000); err != nil {
		t.Fatalf("RecordTransfer: %v", err)
	}
	if _, err := w.allowances(); err != nil {
		t.Fatalf("allowances: %v", err)
	}
	if _, hold := jobStatus(t, db, id); hold == "" {
		t.Fatal("expected the job to be held first")
	}

	// The window rolls over: the consumption ages out.
	if _, err := db.Exec(`UPDATE transfer_usage SET at = datetime('now', '-48 hours')`); err != nil {
		t.Fatalf("ageing the ledger: %v", err)
	}
	if _, err := w.allowances(); err != nil {
		t.Fatalf("allowances: %v", err)
	}

	status, hold := jobStatus(t, db, id)
	if status != "pending" || hold != "" {
		t.Errorf("job = %q/%q, want pending with the hold withdrawn", status, hold)
	}
}

// An upload already running is spending the budget but is not in the ledger
// until it finishes. The allowance handed out counts the ledger only — the
// in-flight subtraction happens inside the claim, where it is atomic — so this
// asserts the two observable consequences instead of the intermediate number:
// the job is not claimed, and the hold says what it is really waiting for.
func TestAnUploadInFlightIsCountedAgainstTheBudget(t *testing.T) {
	w, db := budgetFixture(t, limits.Limit{TransferBytes: 10_000, WindowHours: 24})
	id := seedJob(t, db, 5_000) // pending

	// A second job for the same account, already claimed and uploading.
	if _, err := db.Exec(
		`INSERT INTO jobs (backup_id, account_id, status, total_bytes, zip_path, remote_name)
		 VALUES (1, 1, 'in_progress', 7000, 'z2.zip', 'r2.zip')`); err != nil {
		t.Fatalf("seeding in-flight job: %v", err)
	}

	allow, err := w.allowances()
	if err != nil {
		t.Fatalf("allowances: %v", err)
	}

	// 5000 + 7000 in flight exceeds the 10000 budget, so the claim must refuse.
	job, err := database.ClaimNextPendingJobWithin(db, allow)
	if err != nil {
		t.Fatalf("ClaimNextPendingJobWithin: %v", err)
	}
	if job != nil {
		t.Errorf("claimed job %d: 5000 on top of 7000 in flight exceeds the 10000 budget", job.ID)
	}

	status, hold := jobStatus(t, db, id)
	if status != "pending" {
		t.Errorf("status = %q, want pending", status)
	}
	// Waiting minutes behind a sibling upload and waiting hours for a window to
	// roll over feel nothing alike; the reason must distinguish them.
	if !strings.Contains(hold, "already uploading") {
		t.Errorf("hold reason %q does not mention the in-flight upload it is actually waiting on", hold)
	}
}

// A job failed for a standing reason is spotted by every worker on every tick,
// none of which has claimed it. Only the first may fail and log it.
func TestAnImpossibleJobIsFailedAndLoggedOnlyOnce(t *testing.T) {
	w, db := budgetFixture(t, limits.Limit{TransferBytes: 1_000, WindowHours: 24})
	id := seedJob(t, db, 5_000)

	// Several poll ticks, as several worker goroutines would produce.
	for i := 0; i < 4; i++ {
		if _, err := w.allowances(); err != nil {
			t.Fatalf("allowances: %v", err)
		}
	}

	if status, _ := jobStatus(t, db, id); status != "failed" {
		t.Fatalf("status = %q, want failed", status)
	}
	logs, err := database.ListJobLogs(db, id)
	if err != nil {
		t.Fatalf("ListJobLogs: %v", err)
	}
	var errors int
	for _, l := range logs {
		if l.Level == "error" {
			errors++
		}
	}
	if errors != 1 {
		t.Errorf("wrote %d error log lines, want 1: the same explanation must not repeat per worker per tick", errors)
	}
}

// With nothing pending there is nothing to constrain, and a nil allowance list
// means "no restriction" — not "claim nothing".
func TestNoPendingWorkYieldsNoRestriction(t *testing.T) {
	w, _ := budgetFixture(t, limits.Limit{TransferBytes: 10_000, WindowHours: 24})

	allow, err := w.allowances()
	if err != nil {
		t.Fatalf("allowances: %v", err)
	}
	if allow != nil {
		t.Errorf("allowances = %+v, want nil when nothing is pending", allow)
	}
}
