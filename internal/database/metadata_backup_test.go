package database

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func metadataTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestASuccessfulCopyIsRecordedWithItsName(t *testing.T) {
	db := metadataTestDB(t)
	at := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)

	if err := RecordMetadataBackupSuccess(db, "mega", "backmeup-metadata-20260830-120000.db", at); err != nil {
		t.Fatalf("RecordMetadataBackupSuccess: %v", err)
	}

	got := GetMetadataBackupStatuses(db)["mega"]
	if !got.OK {
		t.Error("OK = false after a successful copy")
	}
	if got.LastSuccessName != "backmeup-metadata-20260830-120000.db" {
		t.Errorf("LastSuccessName = %q, want the uploaded name", got.LastSuccessName)
	}
	if got.LastSuccessAt != "2026-08-30T12:00:00Z" || got.LastAttemptAt != "2026-08-30T12:00:00Z" {
		t.Errorf("timestamps = %q / %q, want the time of the copy", got.LastAttemptAt, got.LastSuccessAt)
	}
}

// The reason the two are stored separately: the copy that landed is still out
// there and still restorable, and its name is what a recovery needs in order to
// find it. A later failure must not erase it.
func TestAFailureKeepsTheLastCopyThatLanded(t *testing.T) {
	db := metadataTestDB(t)
	good := time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC)
	bad := time.Date(2026, 8, 30, 9, 0, 0, 0, time.UTC)

	if err := RecordMetadataBackupSuccess(db, "fourshared", "meta-good.db", good); err != nil {
		t.Fatalf("RecordMetadataBackupSuccess: %v", err)
	}
	if err := RecordMetadataBackupFailure(db, "fourshared", "login", "token expired", bad); err != nil {
		t.Fatalf("RecordMetadataBackupFailure: %v", err)
	}

	got := GetMetadataBackupStatuses(db)["fourshared"]
	if got.OK {
		t.Error("OK = true after a failed copy")
	}
	if got.Stage != "login" || got.Error != "token expired" {
		t.Errorf("failure = %q/%q, want the stage and reason", got.Stage, got.Error)
	}
	if got.LastAttemptAt != "2026-08-30T09:00:00Z" {
		t.Errorf("LastAttemptAt = %q, want the failed attempt", got.LastAttemptAt)
	}
	if got.LastSuccessName != "meta-good.db" || got.LastSuccessAt != "2026-08-29T09:00:00Z" {
		t.Errorf("the last good copy was lost: %+v", got)
	}
}

// A success after a failure must clear the failure, or the card would keep
// warning about a problem that is over.
func TestASuccessClearsAnEarlierFailure(t *testing.T) {
	db := metadataTestDB(t)
	if err := RecordMetadataBackupFailure(db, "mega", "upload", "disk full", time.Now()); err != nil {
		t.Fatalf("RecordMetadataBackupFailure: %v", err)
	}
	if err := RecordMetadataBackupSuccess(db, "mega", "meta.db", time.Now()); err != nil {
		t.Fatalf("RecordMetadataBackupSuccess: %v", err)
	}

	got := GetMetadataBackupStatuses(db)["mega"]
	if !got.OK || got.Stage != "" || got.Error != "" {
		t.Errorf("failure survived a later success: %+v", got)
	}
}

// Each provider is its own destination and they fail independently, so one
// entry must never overwrite another.
func TestProvidersAreRecordedIndependently(t *testing.T) {
	db := metadataTestDB(t)
	if err := RecordMetadataBackupSuccess(db, "mega", "meta.db", time.Now()); err != nil {
		t.Fatalf("RecordMetadataBackupSuccess: %v", err)
	}
	if err := RecordMetadataBackupFailure(db, "fourshared", "login", "rejected", time.Now()); err != nil {
		t.Fatalf("RecordMetadataBackupFailure: %v", err)
	}

	all := GetMetadataBackupStatuses(db)
	if len(all) != 2 {
		t.Fatalf("statuses = %+v, want one per provider", all)
	}
	if !all["mega"].OK || all["fourshared"].OK {
		t.Errorf("one provider's outcome overwrote the other's: %+v", all)
	}
}

// Nothing recorded is the normal state of a fresh install and must read as "not
// attempted" — never as a copy that worked.
func TestNothingRecordedIsNotASuccess(t *testing.T) {
	db := metadataTestDB(t)

	all := GetMetadataBackupStatuses(db)
	if len(all) != 0 {
		t.Fatalf("statuses = %+v, want empty on a fresh database", all)
	}
	if got := all["mega"]; got.LastAttemptAt != "" || got.OK {
		t.Errorf("an absent provider reads as %+v, want a zero status", got)
	}
}

// A corrupt row degrades the same way, and in the safe direction: the dangerous
// failure would be one that reads as a successful copy.
func TestACorruptStatusRowDegradesToNothingRecorded(t *testing.T) {
	db := metadataTestDB(t)
	if err := SetSetting(db, SettingMetadataBackup, "{not json"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}

	if all := GetMetadataBackupStatuses(db); len(all) != 0 {
		t.Fatalf("statuses = %+v, want empty for an unreadable row", all)
	}
}
