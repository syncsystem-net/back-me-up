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
    title TEXT NOT NULL,
    source_path TEXT NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS backup_directories (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    backup_id INTEGER NOT NULL,
    path TEXT NOT NULL,
    name TEXT NOT NULL,
    level INTEGER NOT NULL,
    size_bytes INTEGER DEFAULT 0,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (backup_id) REFERENCES backups(id) ON DELETE CASCADE
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

CREATE INDEX IF NOT EXISTS idx_backup_directories_backup_id ON backup_directories(backup_id);
CREATE INDEX IF NOT EXISTS idx_jobs_backup_id ON jobs(backup_id);
CREATE INDEX IF NOT EXISTS idx_jobs_account_id ON jobs(account_id);
CREATE INDEX IF NOT EXISTS idx_jobs_status ON jobs(status);
CREATE INDEX IF NOT EXISTS idx_job_logs_job_id ON job_logs(job_id);
CREATE INDEX IF NOT EXISTS idx_backup_directories_name ON backup_directories(name);
`
