package database

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// newTestDB opens a fresh migrated database in a temp dir.
func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestExcludeTermsRoundTrip(t *testing.T) {
	db := newTestDB(t)

	// Unset settings must read as an empty list, not an error — a fresh install
	// has no settings row and every backup would otherwise fail.
	terms, err := GetExcludeTerms(db)
	if err != nil {
		t.Fatalf("GetExcludeTerms on fresh db: %v", err)
	}
	if len(terms) != 0 {
		t.Fatalf("expected no terms on fresh db, got %v", terms)
	}

	if err := SetExcludeTerms(db, []string{"conteúdo", "Layout"}); err != nil {
		t.Fatalf("SetExcludeTerms: %v", err)
	}
	terms, err = GetExcludeTerms(db)
	if err != nil {
		t.Fatalf("GetExcludeTerms: %v", err)
	}
	if strings.Join(terms, ",") != "conteúdo,Layout" {
		t.Errorf("got %v, want [conteúdo Layout]", terms)
	}

	// A second write replaces rather than appends.
	if err := SetExcludeTerms(db, []string{"only"}); err != nil {
		t.Fatalf("SetExcludeTerms (replace): %v", err)
	}
	terms, _ = GetExcludeTerms(db)
	if strings.Join(terms, ",") != "only" {
		t.Errorf("expected replacement, got %v", terms)
	}
}

func TestExcludeTermsNormalization(t *testing.T) {
	db := newTestDB(t)

	// Blanks would match every directory name; duplicates would be noise in the
	// Settings list. Both are dropped, and entry order is preserved.
	if err := SetExcludeTerms(db, []string{" Layout ", "", "   ", "layout", "conteudo", "LAYOUT"}); err != nil {
		t.Fatalf("SetExcludeTerms: %v", err)
	}
	terms, err := GetExcludeTerms(db)
	if err != nil {
		t.Fatalf("GetExcludeTerms: %v", err)
	}
	if strings.Join(terms, ",") != "Layout,conteudo" {
		t.Errorf("got %v, want [Layout conteudo]", terms)
	}
}

// A corrupt settings value must degrade to "exclude nothing" rather than
// blocking every backup behind an unreadable row.
func TestExcludeTermsIgnoresCorruptValue(t *testing.T) {
	db := newTestDB(t)

	if err := SetSetting(db, SettingExcludeTerms, "not json at all"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	terms, err := GetExcludeTerms(db)
	if err != nil {
		t.Fatalf("GetExcludeTerms on corrupt value: %v", err)
	}
	if len(terms) != 0 {
		t.Errorf("expected empty terms, got %v", terms)
	}
}

func TestUpdateBackupTitle(t *testing.T) {
	db := newTestDB(t)

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	id, err := UpsertBackupForUser(tx, "u@example.com", "Original", `C:\src`)
	if err != nil {
		t.Fatalf("UpsertBackupForUser: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if err := UpdateBackupTitle(db, id, "Renamed"); err != nil {
		t.Fatalf("UpdateBackupTitle: %v", err)
	}
	b, err := GetBackup(db, id)
	if err != nil {
		t.Fatalf("GetBackup: %v", err)
	}
	if b.Title != "Renamed" {
		t.Errorf("title = %q, want %q", b.Title, "Renamed")
	}
	// A rename must not disturb the record's source path.
	if b.SourcePath != `C:\src` {
		t.Errorf("source_path = %q, want unchanged", b.SourcePath)
	}

	if err := UpdateBackupTitle(db, 99999, "Nope"); err != sql.ErrNoRows {
		t.Errorf("renaming a missing record: got %v, want sql.ErrNoRows", err)
	}
}

// TestMigrateFrom7aSchema upgrades a database that is already on the PR#7a
// shape (owner_email and zip_id present, backup_directories still around with
// rows). Unlike the pre-7a path, this one must PRESERVE the user's backups
// while dropping the dead directories table and adding settings. Testing the
// upgrade path rather than only the greenfield path is what caught the
// index-over-ALTER ordering bug in 7a.
func TestMigrateFrom7aSchema(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "7a.db")

	// Build a 7a-shaped database using the real migration, then seed it.
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open (build 7a db): %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS backup_directories (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		backup_id INTEGER NOT NULL,
		path TEXT NOT NULL,
		name TEXT NOT NULL,
		level INTEGER NOT NULL,
		size_bytes INTEGER DEFAULT 0,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		FOREIGN KEY (backup_id) REFERENCES backups(id) ON DELETE CASCADE)`); err != nil {
		t.Fatalf("recreating legacy directories table: %v", err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	backupID, err := UpsertBackupForUser(tx, "keep@example.com", "Keep Me", `C:\src`)
	if err != nil {
		t.Fatalf("seeding backup: %v", err)
	}
	if _, err := InsertZip(tx, backupID, "keep.zip", `C:\src`, 10, `{"name":"src"}`); err != nil {
		t.Fatalf("seeding zip: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO backup_directories (backup_id, path, name, level) VALUES (?, 'a', 'a', 1)`, backupID); err != nil {
		t.Fatalf("seeding directory row: %v", err)
	}
	db.Close()

	// Re-open: this runs the PR#8 migration over the seeded 7a database.
	db, err = Open(dbPath)
	if err != nil {
		t.Fatalf("Open (migrate 7a -> 8): %v", err)
	}
	defer db.Close()

	// The dead table is gone.
	var name string
	err = db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='backup_directories'`).Scan(&name)
	if err != sql.ErrNoRows {
		t.Errorf("expected backup_directories dropped, got err=%v name=%q", err, name)
	}

	// Settings is usable.
	if err := SetExcludeTerms(db, []string{"layout"}); err != nil {
		t.Fatalf("settings unusable after migration: %v", err)
	}

	// The user's data survived — this migration is not destructive.
	b, err := GetBackupByOwner(db, "keep@example.com")
	if err != nil {
		t.Fatalf("GetBackupByOwner: %v", err)
	}
	if b == nil || b.Title != "Keep Me" {
		t.Fatalf("expected the 7a backup preserved, got %+v", b)
	}
	zips, err := ListZipsByBackup(db, b.ID)
	if err != nil {
		t.Fatalf("ListZipsByBackup: %v", err)
	}
	if len(zips) != 1 || zips[0].Name != "keep.zip" {
		t.Errorf("expected the 7a zip preserved, got %d zips", len(zips))
	}
}
