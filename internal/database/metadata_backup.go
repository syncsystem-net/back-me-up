package database

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// SettingMetadataBackup is the settings key holding the outcome of the last
// metadata-database copy to each main account. The value is a JSON object keyed
// by provider.
//
// It lives in settings rather than in main_accounts because it describes a run,
// not an account: a provider that is removed from .env and later restored should
// not resurrect a stale claim that its copy is current, and a settings row is
// simply absent until something writes it.
const SettingMetadataBackup = "metadata_backup_status"

// MetadataBackupStatus is what is known about one provider's copy of the
// metadata database.
//
// The last attempt and the last success are kept separately and on purpose. A
// run of failures must not erase the record of the copy that did land — that
// copy is still out there and still restorable, and its name is the thing a
// recovery actually needs in order to find it.
type MetadataBackupStatus struct {
	// LastAttemptAt is when the most recent copy was tried, in RFC3339. Empty
	// means never — which the UI must render as "not attempted", never as a
	// failure and never as a blank that reads like success.
	LastAttemptAt string `json:"last_attempt_at,omitempty"`
	// OK reports whether that attempt succeeded.
	OK bool `json:"ok"`
	// Stage and Error describe a failed attempt: which step gave way (building
	// the provider, logging in, uploading) and what it said. The text is the same
	// one the log carries, so the UI and the log cannot tell different stories.
	Stage string `json:"stage,omitempty"`
	Error string `json:"error,omitempty"`
	// LastSuccessAt and LastSuccessName record the most recent copy that landed,
	// whatever has happened since.
	LastSuccessAt   string `json:"last_success_at,omitempty"`
	LastSuccessName string `json:"last_success_name,omitempty"`
}

// GetMetadataBackupStatuses returns the recorded outcome per provider.
//
// An unset or unreadable row yields an empty map — "nothing recorded" — which
// renders as "not attempted". That is the safe direction here: the dangerous
// failure would be a corrupt row that reads as a successful copy, because the
// entire point of this record is to stop the user believing in a backup that is
// not happening.
func GetMetadataBackupStatuses(db *sql.DB) map[string]MetadataBackupStatus {
	raw, err := GetSetting(db, SettingMetadataBackup)
	if err != nil {
		slog.Warn("could not read metadata backup status", "error", err)
		return map[string]MetadataBackupStatus{}
	}
	return parseMetadataBackupStatuses(raw)
}

func parseMetadataBackupStatuses(raw string) map[string]MetadataBackupStatus {
	out := map[string]MetadataBackupStatus{}
	if strings.TrimSpace(raw) == "" {
		return out
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		slog.Warn("metadata backup status is unreadable; reporting nothing recorded", "error", err)
		return map[string]MetadataBackupStatus{}
	}
	return out
}

// RecordMetadataBackupSuccess notes that name reached provider.
func RecordMetadataBackupSuccess(db *sql.DB, provider, name string, at time.Time) error {
	stamp := at.UTC().Format(time.RFC3339)
	return updateMetadataBackupStatus(db, provider, func(s MetadataBackupStatus) MetadataBackupStatus {
		return MetadataBackupStatus{
			LastAttemptAt:   stamp,
			OK:              true,
			LastSuccessAt:   stamp,
			LastSuccessName: name,
		}
	})
}

// RecordMetadataBackupFailure notes that a copy to provider did not land, at
// which stage and why, while preserving whatever last succeeded.
func RecordMetadataBackupFailure(db *sql.DB, provider, stage, reason string, at time.Time) error {
	stamp := at.UTC().Format(time.RFC3339)
	return updateMetadataBackupStatus(db, provider, func(s MetadataBackupStatus) MetadataBackupStatus {
		s.LastAttemptAt = stamp
		s.OK = false
		s.Stage = stage
		s.Error = reason
		// LastSuccessAt / LastSuccessName are carried over untouched: that copy
		// still exists, and hiding it because a later run failed would remove the
		// one piece of information a restore needs.
		return s
	})
}

// updateMetadataBackupStatus applies fn to one provider's entry and stores the
// result.
//
// It runs in a transaction because it is a read-modify-write of a shared row and
// several worker goroutines can finish jobs at the same moment. With
// SetMaxOpenConns(1) the transaction holds the only connection, so the pair
// cannot interleave with another goroutine's — without it, two providers
// finishing together could each write a map missing the other's entry.
func updateMetadataBackupStatus(db *sql.DB, provider string, fn func(MetadataBackupStatus) MetadataBackupStatus) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("recording metadata backup status: %w", err)
	}
	defer tx.Rollback()

	var raw string
	err = tx.QueryRow(`SELECT value FROM settings WHERE key = ?`, SettingMetadataBackup).Scan(&raw)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("reading metadata backup status: %w", err)
	}

	// A row that will not parse cannot be preserved — whatever it held is already
	// unreadable, and the UI is already reporting "nothing recorded" for every
	// provider in it. Starting a fresh object restores the reporting; saying so
	// loudly is the part that matters, because it is the only moment anyone could
	// notice that a previous record was lost rather than never written.
	statuses := parseMetadataBackupStatuses(raw)
	if strings.TrimSpace(raw) != "" && len(statuses) == 0 {
		slog.Warn("replacing an unreadable metadata backup status row",
			"provider", provider, "discarded", raw)
	}
	statuses[provider] = fn(statuses[provider])

	encoded, err := json.Marshal(statuses)
	if err != nil {
		return fmt.Errorf("encoding metadata backup status: %w", err)
	}

	if _, err := tx.Exec(
		`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = CURRENT_TIMESTAMP`,
		SettingMetadataBackup, string(encoded),
	); err != nil {
		return fmt.Errorf("writing metadata backup status: %w", err)
	}
	return tx.Commit()
}
