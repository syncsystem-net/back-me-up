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

	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		return nil, fmt.Errorf("setting journal mode: %w", err)
	}
	if _, err := db.Exec("PRAGMA foreign_keys=ON"); err != nil {
		return nil, fmt.Errorf("enabling foreign keys: %w", err)
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
	_, err := db.Exec(schema)
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
    total_bytes INTEGER DEFAULT 0,
    uploaded_bytes INTEGER DEFAULT 0,
    chunks_total INTEGER DEFAULT 0,
    chunks_uploaded INTEGER DEFAULT 0,
    error_message TEXT,
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
