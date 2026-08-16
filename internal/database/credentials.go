package database

import (
	"database/sql"
	"fmt"
)

// This file is the storage layer for account credentials. It deliberately knows
// nothing about what a sealed blob contains or how it is sealed — it moves
// opaque bytes. The keyring owns the crypto and the credentials package owns the
// reconciliation rules; keeping those apart means the schema never has to change
// when a provider's credential set grows.

// TokenSourceApp marks an OAuth token this application obtained itself, through
// the in-app re-authorization flow. Reconciliation refuses to overwrite such a
// token with a value from .env: the .env copy is by definition the stale one the
// user re-authorized to replace, and clobbering it at the next restart would
// make the Re-authorize button pointless.
const TokenSourceApp = "app"

// TokenSourceEnv marks an OAuth token adopted from .env.
const TokenSourceEnv = "env"

// AccountRow is a numbered (upload-target) account as stored. SecretsEnc is a
// sealed credential document; it is never logged and never leaves the server.
type AccountRow struct {
	ID           int64
	Provider     string
	Email        string
	QuotaGB      float64
	EnvIndex     int
	SecretsEnc   []byte
	NeedsReauth  bool
	ReauthReason string
	TokenSource  string
}

// MainAccountRow is a provider's database-backup destination as stored. It lives
// in its own table; see the schema comment for why it is not a flagged row in
// accounts.
type MainAccountRow struct {
	ID           int64
	Provider     string
	Email        string
	SecretsEnc   []byte
	NeedsReauth  bool
	ReauthReason string
	TokenSource  string
}

// ListAccountRows returns every stored numbered account, credentials included.
func ListAccountRows(db *sql.DB) ([]AccountRow, error) {
	rows, err := db.Query(`SELECT id, provider, email, quota_total_gb, env_index,
		secrets_enc, needs_reauth, COALESCE(reauth_reason, ''), COALESCE(token_source, '')
		FROM accounts ORDER BY provider, env_index, email`)
	if err != nil {
		return nil, fmt.Errorf("querying account credentials: %w", err)
	}
	defer rows.Close()

	var out []AccountRow
	for rows.Next() {
		var a AccountRow
		if err := rows.Scan(&a.ID, &a.Provider, &a.Email, &a.QuotaGB, &a.EnvIndex,
			&a.SecretsEnc, &a.NeedsReauth, &a.ReauthReason, &a.TokenSource); err != nil {
			return nil, fmt.Errorf("scanning account credential row: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// UpsertAccountRow inserts or updates an account's identity, declared quota and
// sealed credentials, returning its id.
//
// A declared quota of zero does not overwrite a stored one: quota_total_gb is
// also written by the quota poller with the provider's real figure, and .env
// omitting the optional _QUOTA_GB key must not reset that to zero on every boot.
func UpsertAccountRow(db *sql.DB, a AccountRow) (int64, error) {
	_, err := db.Exec(
		`INSERT INTO accounts (provider, email, quota_total_gb, env_index, secrets_enc, token_source)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(provider, email) DO UPDATE SET
		   quota_total_gb = CASE WHEN excluded.quota_total_gb > 0 THEN excluded.quota_total_gb ELSE accounts.quota_total_gb END,
		   env_index      = excluded.env_index,
		   secrets_enc    = excluded.secrets_enc,
		   token_source   = excluded.token_source`,
		a.Provider, a.Email, a.QuotaGB, a.EnvIndex, a.SecretsEnc, a.TokenSource,
	)
	if err != nil {
		return 0, fmt.Errorf("upserting account %s/%s: %w", a.Provider, a.Email, err)
	}
	var id int64
	if err := db.QueryRow(`SELECT id FROM accounts WHERE provider = ? AND email = ?`, a.Provider, a.Email).Scan(&id); err != nil {
		return 0, fmt.Errorf("getting account id: %w", err)
	}
	return id, nil
}

// SetAccountSecrets replaces an account's sealed credentials and records where
// its token came from. Used by the re-authorization flow, which also clears the
// needs-re-authorization flag in the same statement so the two can never
// disagree.
func SetAccountSecrets(db *sql.DB, provider, email string, sealed []byte, tokenSource string) error {
	res, err := db.Exec(
		`UPDATE accounts SET secrets_enc = ?, token_source = ?, needs_reauth = 0, reauth_reason = NULL
		 WHERE provider = ? AND email = ?`,
		sealed, tokenSource, provider, email,
	)
	if err != nil {
		return fmt.Errorf("updating credentials for %s/%s: %w", provider, email, err)
	}
	return requireRow(res, provider, email)
}

// SetMainAccountSecrets is SetAccountSecrets for a database-backup account.
func SetMainAccountSecrets(db *sql.DB, provider string, sealed []byte, tokenSource string) error {
	res, err := db.Exec(
		`UPDATE main_accounts SET secrets_enc = ?, token_source = ?, needs_reauth = 0, reauth_reason = NULL
		 WHERE provider = ?`,
		sealed, tokenSource, provider,
	)
	if err != nil {
		return fmt.Errorf("updating main account credentials for %s: %w", provider, err)
	}
	return requireRow(res, provider, "main")
}

// SetAccountReauth flags (or clears) an account as needing re-authorization. The
// flag is stored rather than kept in memory so an expired token discovered
// during a job is still reported after a restart.
func SetAccountReauth(db *sql.DB, provider, email string, needs bool, reason string) error {
	if !needs {
		reason = ""
	}
	_, err := db.Exec(
		`UPDATE accounts SET needs_reauth = ?, reauth_reason = ? WHERE provider = ? AND email = ?`,
		needs, nullIfEmpty(reason), provider, email,
	)
	if err != nil {
		return fmt.Errorf("flagging %s/%s for re-authorization: %w", provider, email, err)
	}
	return nil
}

// SetMainAccountReauth is SetAccountReauth for a database-backup account.
func SetMainAccountReauth(db *sql.DB, provider string, needs bool, reason string) error {
	if !needs {
		reason = ""
	}
	_, err := db.Exec(
		`UPDATE main_accounts SET needs_reauth = ?, reauth_reason = ? WHERE provider = ?`,
		needs, nullIfEmpty(reason), provider,
	)
	if err != nil {
		return fmt.Errorf("flagging main account %s for re-authorization: %w", provider, err)
	}
	return nil
}

// ListMainAccountRows returns every stored database-backup account.
func ListMainAccountRows(db *sql.DB) ([]MainAccountRow, error) {
	rows, err := db.Query(`SELECT id, provider, email, secrets_enc, needs_reauth,
		COALESCE(reauth_reason, ''), COALESCE(token_source, '')
		FROM main_accounts ORDER BY provider`)
	if err != nil {
		return nil, fmt.Errorf("querying main account credentials: %w", err)
	}
	defer rows.Close()

	var out []MainAccountRow
	for rows.Next() {
		var m MainAccountRow
		if err := rows.Scan(&m.ID, &m.Provider, &m.Email, &m.SecretsEnc, &m.NeedsReauth,
			&m.ReauthReason, &m.TokenSource); err != nil {
			return nil, fmt.Errorf("scanning main account row: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// UpsertMainAccountRow inserts or updates a provider's database-backup account.
// The provider is the key: there is at most one main account per provider.
func UpsertMainAccountRow(db *sql.DB, m MainAccountRow) (int64, error) {
	_, err := db.Exec(
		`INSERT INTO main_accounts (provider, email, secrets_enc, token_source)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(provider) DO UPDATE SET
		   email        = excluded.email,
		   secrets_enc  = excluded.secrets_enc,
		   token_source = excluded.token_source`,
		m.Provider, m.Email, m.SecretsEnc, m.TokenSource,
	)
	if err != nil {
		return 0, fmt.Errorf("upserting main account %s: %w", m.Provider, err)
	}
	var id int64
	if err := db.QueryRow(`SELECT id FROM main_accounts WHERE provider = ?`, m.Provider).Scan(&id); err != nil {
		return 0, fmt.Errorf("getting main account id: %w", err)
	}
	return id, nil
}

// DeleteMainAccount removes a provider's database-backup account. Unlike a
// numbered account (which is kept when it disappears from .env, because removing
// an upload target must be deliberate), a main account IS keyed by provider and
// removing its .env block is the only way to say "stop copying the database
// here" — so that removal has to take effect.
func DeleteMainAccount(db *sql.DB, provider string) error {
	if _, err := db.Exec(`DELETE FROM main_accounts WHERE provider = ?`, provider); err != nil {
		return fmt.Errorf("deleting main account %s: %w", provider, err)
	}
	return nil
}

// MainAccountQuota is the cached capacity of a database-backup account. Kept
// separate from DBAccount so a main account can never be mistaken for an upload
// target by code that takes a *DBAccount.
type MainAccountQuota struct {
	Provider     string  `json:"provider"`
	QuotaTotalGB float64 `json:"quota_total_gb"`
	QuotaUsedGB  float64 `json:"quota_used_gb"`
	LastSync     *string `json:"last_quota_sync"`
}

// UpdateMainAccountQuota caches a database-backup account's capacity, so the
// Accounts view can show used/free for it like it does for every other account.
func UpdateMainAccountQuota(db *sql.DB, provider string, totalBytes, usedBytes int64) error {
	const bytesPerGB = 1 << 30
	_, err := db.Exec(
		`UPDATE main_accounts SET quota_total_gb = ?, quota_used_gb = ?, last_quota_sync = CURRENT_TIMESTAMP
		 WHERE provider = ?`,
		float64(totalBytes)/bytesPerGB, float64(usedBytes)/bytesPerGB, provider,
	)
	if err != nil {
		return fmt.Errorf("updating main account quota for %s: %w", provider, err)
	}
	return nil
}

// ListMainAccountQuotas returns the cached capacity of every stored main
// account, keyed by provider.
func ListMainAccountQuotas(db *sql.DB) (map[string]MainAccountQuota, error) {
	rows, err := db.Query(`SELECT provider, quota_total_gb, quota_used_gb, last_quota_sync FROM main_accounts`)
	if err != nil {
		return nil, fmt.Errorf("querying main account quotas: %w", err)
	}
	defer rows.Close()

	out := map[string]MainAccountQuota{}
	for rows.Next() {
		var q MainAccountQuota
		var last sql.NullString
		if err := rows.Scan(&q.Provider, &q.QuotaTotalGB, &q.QuotaUsedGB, &last); err != nil {
			return nil, fmt.Errorf("scanning main account quota: %w", err)
		}
		if last.Valid {
			v := last.String
			q.LastSync = &v
		}
		out[q.Provider] = q
	}
	return out, rows.Err()
}

// AccountReauthState is the per-account re-authorization flag, keyed by
// "provider/email", for the accounts API to merge into its response.
func AccountReauthState(db *sql.DB) (map[string]string, map[string]bool, error) {
	rows, err := db.Query(`SELECT provider, email, needs_reauth, COALESCE(reauth_reason, '') FROM accounts`)
	if err != nil {
		return nil, nil, fmt.Errorf("querying re-authorization state: %w", err)
	}
	defer rows.Close()

	reasons := map[string]string{}
	needs := map[string]bool{}
	for rows.Next() {
		var provider, email, reason string
		var flag bool
		if err := rows.Scan(&provider, &email, &flag, &reason); err != nil {
			return nil, nil, fmt.Errorf("scanning re-authorization state: %w", err)
		}
		key := provider + "/" + email
		needs[key] = flag
		reasons[key] = reason
	}
	return reasons, needs, rows.Err()
}

// HasSealedCredentials reports whether any account row holds encrypted
// credentials. The keyring uses it to refuse to generate a fresh salt over a
// database that already has sealed rows — that would derive a different key and
// make every one of them unopenable.
func HasSealedCredentials(db *sql.DB) (bool, error) {
	var n int
	err := db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM accounts WHERE secrets_enc IS NOT NULL AND LENGTH(secrets_enc) > 0) +
		(SELECT COUNT(*) FROM main_accounts WHERE secrets_enc IS NOT NULL AND LENGTH(secrets_enc) > 0)`).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("checking for stored credentials: %w", err)
	}
	return n > 0, nil
}

func requireRow(res sql.Result, provider, email string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("no stored account for %s/%s", provider, email)
	}
	return nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
