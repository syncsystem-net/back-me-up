package database

import (
	"database/sql"
	"fmt"
	"time"
)

type DBAccount struct {
	ID            int64      `json:"id"`
	Provider      string     `json:"provider"`
	Email         string     `json:"email"`
	QuotaTotalGB  float64    `json:"quota_total_gb"`
	QuotaUsedGB   float64    `json:"quota_used_gb"`
	LastQuotaSync *time.Time `json:"last_quota_sync"`
	CreatedAt     time.Time  `json:"created_at"`

	// NeedsReauth is set when the provider rejected this account's OAuth token as
	// expired. It rides on the account rather than living in memory so an expired
	// token found during a job is still reported after a restart — and so the
	// Accounts view can offer a Re-authorize button instead of the user learning
	// about it from a failed upload.
	NeedsReauth  bool   `json:"needs_reauth"`
	ReauthReason string `json:"reauth_reason,omitempty"`
	// EnvIndex is the account's .env slot, so a message can name real keys
	// (FOURSHARED_ACCOUNT_2_*) rather than describing them.
	EnvIndex int `json:"env_index"`
}

func ListDBAccounts(db *sql.DB) ([]*DBAccount, error) {
	rows, err := db.Query(
		`SELECT id, provider, email, quota_total_gb, quota_used_gb, last_quota_sync, created_at,
		        needs_reauth, COALESCE(reauth_reason, ''), env_index
		 FROM accounts ORDER BY provider, email`,
	)
	if err != nil {
		return nil, fmt.Errorf("querying accounts: %w", err)
	}
	defer rows.Close()

	var accounts []*DBAccount
	for rows.Next() {
		a := &DBAccount{}
		var lastSync sql.NullTime
		if err := rows.Scan(&a.ID, &a.Provider, &a.Email, &a.QuotaTotalGB, &a.QuotaUsedGB, &lastSync, &a.CreatedAt,
			&a.NeedsReauth, &a.ReauthReason, &a.EnvIndex); err != nil {
			return nil, fmt.Errorf("scanning account row: %w", err)
		}
		if lastSync.Valid {
			a.LastQuotaSync = &lastSync.Time
		}
		accounts = append(accounts, a)
	}
	return accounts, rows.Err()
}

// UpdateAccountQuota stores the latest total/used capacity (in bytes, converted
// to GB) for an account and stamps last_quota_sync. Called after a successful
// upload so the table reflects fresh usage without waiting for a poll cycle.
func UpdateAccountQuota(db *sql.DB, id, totalBytes, usedBytes int64) error {
	const bytesPerGB = 1 << 30
	_, err := db.Exec(
		`UPDATE accounts SET quota_total_gb = ?, quota_used_gb = ?, last_quota_sync = CURRENT_TIMESTAMP WHERE id = ?`,
		float64(totalBytes)/bytesPerGB, float64(usedBytes)/bytesPerGB, id,
	)
	if err != nil {
		return fmt.Errorf("updating account quota: %w", err)
	}
	return nil
}

func GetDBAccountByID(db *sql.DB, id int64) (*DBAccount, error) {
	row := db.QueryRow(
		`SELECT id, provider, email, quota_total_gb, quota_used_gb, last_quota_sync, created_at FROM accounts WHERE id = ?`,
		id,
	)
	a := &DBAccount{}
	var lastSync sql.NullTime
	if err := row.Scan(&a.ID, &a.Provider, &a.Email, &a.QuotaTotalGB, &a.QuotaUsedGB, &lastSync, &a.CreatedAt); err != nil {
		return nil, fmt.Errorf("scanning account: %w", err)
	}
	if lastSync.Valid {
		a.LastQuotaSync = &lastSync.Time
	}
	return a, nil
}

// GetDBAccountIDByProviderEmail resolves a numbered account's DB id from its
// (provider, email) pair. The quota poller uses this to map an in-memory account
// to its row so it can call UpdateAccountQuota.
func GetDBAccountIDByProviderEmail(db *sql.DB, provider, email string) (int64, error) {
	var id int64
	if err := db.QueryRow(
		`SELECT id FROM accounts WHERE provider = ? AND email = ?`,
		provider, email,
	).Scan(&id); err != nil {
		return 0, fmt.Errorf("getting account id for %s/%s: %w", provider, email, err)
	}
	return id, nil
}
