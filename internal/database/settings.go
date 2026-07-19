package database

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// SettingExcludeTerms is the settings key holding the directory-name terms that
// are left out of a recorded tree. The value is a JSON array of strings.
//
// Settings are a key/value table rather than one column per setting so the
// Settings modal can grow without a migration each time.
const SettingExcludeTerms = "exclude_terms"

// GetSetting returns the raw stored value for key, or ("", nil) when unset.
func GetSetting(db *sql.DB, key string) (string, error) {
	var value string
	err := db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading setting %q: %w", key, err)
	}
	return value, nil
}

// SetSetting writes key's value, replacing any existing one.
func SetSetting(db *sql.DB, key, value string) error {
	_, err := db.Exec(
		`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = CURRENT_TIMESTAMP`,
		key, value,
	)
	if err != nil {
		return fmt.Errorf("writing setting %q: %w", key, err)
	}
	return nil
}

// GetExcludeTerms returns the configured exclude terms. An unset or unparseable
// value yields an empty list rather than an error: a corrupt settings row should
// degrade to "exclude nothing" instead of blocking every backup.
func GetExcludeTerms(db *sql.DB) ([]string, error) {
	raw, err := GetSetting(db, SettingExcludeTerms)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(raw) == "" {
		return []string{}, nil
	}
	var terms []string
	if err := json.Unmarshal([]byte(raw), &terms); err != nil {
		return []string{}, nil
	}
	return normalizeTerms(terms), nil
}

// SetExcludeTerms replaces the stored exclude terms.
func SetExcludeTerms(db *sql.DB, terms []string) error {
	cleaned := normalizeTerms(terms)
	b, err := json.Marshal(cleaned)
	if err != nil {
		return fmt.Errorf("encoding exclude terms: %w", err)
	}
	return SetSetting(db, SettingExcludeTerms, string(b))
}

// normalizeTerms trims each term and drops blanks and case-insensitive
// duplicates, preserving the order the user entered them. A blank term would
// otherwise match every directory name.
func normalizeTerms(terms []string) []string {
	seen := make(map[string]bool, len(terms))
	out := make([]string, 0, len(terms))
	for _, t := range terms {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		key := strings.ToLower(t)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, t)
	}
	return out
}
