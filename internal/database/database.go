package database

import (
	"database/sql"
	"fmt"
	"log/slog"
	"strings"

	_ "modernc.org/sqlite"
)

func Open(dbPath string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}

	// SQLite allows only one writer at a time. With a connection pool, several
	// upload workers and HTTP handlers race for the write lock and lose with
	// SQLITE_BUSY ("database is locked"), and a per-connection busy_timeout set
	// below would only cover whichever pooled connection happened to run it.
	// Pinning the pool to a single connection funnels all access through one
	// serialized path, so SQLite never sees concurrent writers. Throughput is a
	// non-issue for a local single-user tool, and uploads hold no DB lock while
	// transferring (only short progress writes touch the database).
	db.SetMaxOpenConns(1)

	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		return nil, fmt.Errorf("setting journal mode: %w", err)
	}
	if _, err := db.Exec("PRAGMA foreign_keys=ON"); err != nil {
		return nil, fmt.Errorf("enabling foreign keys: %w", err)
	}
	// Several upload workers and HTTP handlers write concurrently; without a
	// busy timeout a contended writer fails immediately with SQLITE_BUSY and a
	// progress update would be silently dropped. Wait briefly for the lock.
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		return nil, fmt.Errorf("setting busy timeout: %w", err)
	}

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("pinging database: %w", err)
	}

	if err := migrate(db); err != nil {
		return nil, fmt.Errorf("running migrations: %w", err)
	}

	slog.Info("database ready", "path", dbPath)
	return db, nil
}

func migrate(db *sql.DB) error {
	if err := migrateAccountsUniqueConstraint(db); err != nil {
		return fmt.Errorf("accounts migration: %w", err)
	}
	// PR #7 reshapes backups to be per-user (owner_email) with an intermediate
	// backup_zips table that jobs reference. The old model tied jobs directly to a
	// single-upload backup, so pre-existing backup/job rows can't be mapped onto
	// the new relationships. Clear them (accounts are always re-synced from .env,
	// so only disposable backup test data is lost) before adding the new columns.
	if err := clearBackupsIfLegacy(db); err != nil {
		return fmt.Errorf("per-user backups migration: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		return err
	}
	// Additive column migration for the clean cloud upload name. CREATE TABLE
	// above adds it for fresh databases; existing databases need an ALTER.
	if err := addColumnIfMissing(db, "jobs", "remote_name", "TEXT"); err != nil {
		return fmt.Errorf("jobs.remote_name migration: %w", err)
	}
	// Additive column for the cached quota poll timestamp. CREATE TABLE adds it
	// for fresh databases; existing databases that already have the
	// UNIQUE(provider, email) constraint skip the recreate above and need an
	// ALTER so the accounts/quota queries don't hit "no such column".
	if err := addColumnIfMissing(db, "accounts", "last_quota_sync", "DATETIME"); err != nil {
		return fmt.Errorf("accounts.last_quota_sync migration: %w", err)
	}
	// Additive columns for periodic re-verification: the first-chunk checksum
	// captured at verify-on-upload time, and when the file was last re-verified
	// against it. CREATE TABLE adds them for fresh databases; existing databases
	// need an ALTER.
	if err := addColumnIfMissing(db, "jobs", "verify_checksum", "TEXT"); err != nil {
		return fmt.Errorf("jobs.verify_checksum migration: %w", err)
	}
	if err := addColumnIfMissing(db, "jobs", "last_verified_at", "DATETIME"); err != nil {
		return fmt.Errorf("jobs.last_verified_at migration: %w", err)
	}
	// PR #7: the user (email) that owns the backup record, and the zip a job
	// uploads. CREATE TABLE above adds them for fresh databases; existing
	// databases need an ALTER.
	if err := addColumnIfMissing(db, "backups", "owner_email", "TEXT"); err != nil {
		return fmt.Errorf("backups.owner_email migration: %w", err)
	}
	if err := addColumnIfMissing(db, "jobs", "zip_id", "INTEGER"); err != nil {
		return fmt.Errorf("jobs.zip_id migration: %w", err)
	}
	// Indexes over ALTER-added columns must be created here, not in the schema
	// block above: on an existing database `CREATE TABLE IF NOT EXISTS jobs` is a
	// no-op, so the column does not exist yet when that block runs.
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_jobs_zip_id ON jobs(zip_id)`); err != nil {
		return fmt.Errorf("jobs.zip_id index: %w", err)
	}
	// PR #8: backup_zips.tree_json fully replaced backup_directories, and nothing
	// has written to that table since 7a. Drop it now that the last reader
	// (the legacy /api/backups + /api/search path) is gone.
	if err := dropLegacyDirectories(db); err != nil {
		return fmt.Errorf("dropping backup_directories: %w", err)
	}
	return nil
}

// dropLegacyDirectories removes the dead backup_directories table. Its rows are
// deleted first: nothing references them, but the table references backups, and
// clearing before the drop keeps the pattern consistent with the FK-safe
// migrations above. Absent on a fresh database, where the drop is a no-op.
func dropLegacyDirectories(db *sql.DB) error {
	var name string
	err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='backup_directories'`).Scan(&name)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return fmt.Errorf("checking backup_directories table: %w", err)
	}
	slog.Info("dropping legacy backup_directories table (superseded by backup_zips.tree_json)")
	if _, err := db.Exec(`DELETE FROM backup_directories`); err != nil {
		return fmt.Errorf("clearing backup_directories: %w", err)
	}
	if _, err := db.Exec(`DROP TABLE IF EXISTS backup_directories`); err != nil {
		return fmt.Errorf("dropping table: %w", err)
	}
	return nil
}

// columnExists reports whether table has a column named column, using
// PRAGMA table_info. Returns false (no error) when the table does not exist.
func columnExists(db *sql.DB, table, column string) (bool, error) {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false, fmt.Errorf("reading %s columns: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, fmt.Errorf("scanning %s columns: %w", table, err)
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

// clearBackupsIfLegacy wipes backup/job rows when the backups table predates the
// per-user reshape (no owner_email column). Rows are deleted child-first so FK
// constraints don't block. A fresh database (no backups table) is a no-op — the
// schema will create the new shape directly.
func clearBackupsIfLegacy(db *sql.DB) error {
	var exists int
	if err := db.QueryRow(`SELECT 1 FROM sqlite_master WHERE type='table' AND name='backups'`).Scan(&exists); err == sql.ErrNoRows {
		return nil // fresh database
	} else if err != nil {
		return fmt.Errorf("checking backups table: %w", err)
	}
	hasOwner, err := columnExists(db, "backups", "owner_email")
	if err != nil {
		return err
	}
	if hasOwner {
		return nil // already migrated
	}
	slog.Info("migrating to per-user backups: clearing legacy backup/job rows")
	for _, stmt := range []string{
		`DELETE FROM job_logs`,
		`DELETE FROM jobs`,
		// backup_directories rows cascade away with their backups, and the table
		// itself is dropped later in migrate().
		`DELETE FROM backups`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("clearing legacy rows (%s): %w", stmt, err)
		}
	}
	return nil
}

// addColumnIfMissing adds a column to a table only if it is not already present,
// using PRAGMA table_info to check. SQLite has no "ADD COLUMN IF NOT EXISTS".
func addColumnIfMissing(db *sql.DB, table, column, typ string) error {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return fmt.Errorf("reading %s columns: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return fmt.Errorf("scanning %s columns: %w", table, err)
		}
		if name == column {
			return nil // already present
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, typ))
	return err
}

// migrateAccountsUniqueConstraint recreates the accounts table with the
// correct UNIQUE(provider, email) constraint. Jobs rows are deleted first
// because they hold FK references to accounts. Accounts are always re-synced
// from .env on startup so data loss here is safe.
func migrateAccountsUniqueConstraint(db *sql.DB) error {
	var tableSQL string
	err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='accounts'`).Scan(&tableSQL)
	if err == sql.ErrNoRows {
		return nil // table does not exist yet; schema will create it
	}
	if err != nil {
		return fmt.Errorf("checking accounts schema: %w", err)
	}
	if strings.Contains(tableSQL, "UNIQUE(provider, email)") {
		return nil // already on new constraint
	}
	slog.Info("migrating accounts table: clearing jobs and recreating with (provider, email) unique constraint")
	// jobs.account_id references accounts — delete them first so the FK
	// constraint does not block the DROP TABLE below.
	if _, err = db.Exec(`DELETE FROM jobs`); err != nil {
		return fmt.Errorf("clearing jobs for migration: %w", err)
	}
	if _, err = db.Exec(`DROP TABLE IF EXISTS accounts`); err != nil {
		return fmt.Errorf("dropping accounts table: %w", err)
	}
	return nil
}

const schema = `
CREATE TABLE IF NOT EXISTS backups (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    owner_email TEXT,
    title TEXT NOT NULL,
    source_path TEXT NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS backup_zips (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    backup_id INTEGER NOT NULL,
    name TEXT NOT NULL,
    source_path TEXT NOT NULL,
    size_bytes INTEGER DEFAULT 0,
    tree_json TEXT,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (backup_id) REFERENCES backups(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS settings (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS accounts (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    provider TEXT NOT NULL,
    email TEXT NOT NULL,
    quota_total_gb REAL DEFAULT 0,
    quota_used_gb REAL DEFAULT 0,
    last_quota_sync DATETIME,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(provider, email)
);

CREATE TABLE IF NOT EXISTS jobs (
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
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (backup_id) REFERENCES backups(id) ON DELETE CASCADE,
    FOREIGN KEY (account_id) REFERENCES accounts(id)
);

CREATE TABLE IF NOT EXISTS job_logs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    job_id INTEGER NOT NULL,
    level TEXT NOT NULL,
    message TEXT NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (job_id) REFERENCES jobs(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_backup_zips_backup_id ON backup_zips(backup_id);
CREATE INDEX IF NOT EXISTS idx_jobs_backup_id ON jobs(backup_id);
CREATE INDEX IF NOT EXISTS idx_jobs_account_id ON jobs(account_id);
CREATE INDEX IF NOT EXISTS idx_jobs_status ON jobs(status);
CREATE INDEX IF NOT EXISTS idx_job_logs_job_id ON job_logs(job_id);
`
