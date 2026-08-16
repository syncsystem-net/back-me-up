package database

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// A real upgrade starts from a database that already has an accounts table
// without the credential columns, where CREATE TABLE IF NOT EXISTS is a no-op.
// Fresh-database tests cannot catch a missed ALTER, which is exactly how the
// PR #7 migration broke in the field.
func TestMigrateAddsCredentialColumnsToAnExistingAccountsTable(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "upgrade.db")

	// The accounts table as it stood before PR #13, already on the
	// UNIQUE(provider, email) constraint so the older recreate path is skipped.
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := raw.Exec(`
		CREATE TABLE accounts (
		    id INTEGER PRIMARY KEY AUTOINCREMENT,
		    provider TEXT NOT NULL,
		    email TEXT NOT NULL,
		    quota_total_gb REAL DEFAULT 0,
		    quota_used_gb REAL DEFAULT 0,
		    last_quota_sync DATETIME,
		    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		    UNIQUE(provider, email)
		);`); err != nil {
		t.Fatalf("creating pre-13 accounts table: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO accounts (provider, email, quota_total_gb) VALUES ('mega', 'existing@example.com', 20)`); err != nil {
		t.Fatalf("seeding account: %v", err)
	}
	raw.Close()

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open (migrate): %v", err)
	}
	defer db.Close()

	// The existing row must survive with its identity and quota intact — the
	// accounts table is no longer rebuilt from .env on every boot, so losing it
	// now loses the user's credentials.
	rows, err := ListAccountRows(db)
	if err != nil {
		t.Fatalf("ListAccountRows: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected the pre-existing account to survive, got %+v", rows)
	}
	if rows[0].Email != "existing@example.com" || rows[0].QuotaGB != 20 {
		t.Errorf("row = %+v", rows[0])
	}
	// An upgraded row has no credentials yet; the next .env import supplies them.
	if len(rows[0].SecretsEnc) != 0 || rows[0].NeedsReauth || rows[0].TokenSource != "" {
		t.Errorf("upgraded row should start with empty credential state: %+v", rows[0])
	}

	// The reads the app performs must all work against the upgraded table.
	if _, err := ListDBAccounts(db); err != nil {
		t.Fatalf("ListDBAccounts after migration: %v", err)
	}
	if _, err := ListMainAccountRows(db); err != nil {
		t.Fatalf("ListMainAccountRows after migration: %v", err)
	}
}

func TestAccountRowRoundTrip(t *testing.T) {
	db := newTestDB(t)

	id, err := UpsertAccountRow(db, AccountRow{
		Provider: "fourshared", Email: "a@example.com", QuotaGB: 15, EnvIndex: 2,
		SecretsEnc: []byte("sealed"), TokenSource: TokenSourceEnv,
	})
	if err != nil {
		t.Fatalf("UpsertAccountRow: %v", err)
	}
	if id == 0 {
		t.Fatal("no id returned")
	}

	rows, err := ListAccountRows(db)
	if err != nil {
		t.Fatalf("ListAccountRows: %v", err)
	}
	if len(rows) != 1 || string(rows[0].SecretsEnc) != "sealed" || rows[0].EnvIndex != 2 {
		t.Fatalf("row = %+v", rows)
	}

	// A second upsert updates in place rather than adding a row.
	if _, err := UpsertAccountRow(db, AccountRow{
		Provider: "fourshared", Email: "a@example.com", QuotaGB: 15, EnvIndex: 2,
		SecretsEnc: []byte("resealed"), TokenSource: TokenSourceApp,
	}); err != nil {
		t.Fatalf("second UpsertAccountRow: %v", err)
	}
	rows, _ = ListAccountRows(db)
	if len(rows) != 1 || string(rows[0].SecretsEnc) != "resealed" || rows[0].TokenSource != TokenSourceApp {
		t.Fatalf("row after update = %+v", rows)
	}
}

// The quota poller writes the provider's real capacity into the same column
// .env's optional _QUOTA_GB seeds. An omitted (zero) declaration must not reset
// the polled figure on every boot.
func TestUpsertDoesNotResetAPolledQuotaToZero(t *testing.T) {
	db := newTestDB(t)

	id, err := UpsertAccountRow(db, AccountRow{Provider: "mega", Email: "a@example.com", QuotaGB: 20})
	if err != nil {
		t.Fatalf("UpsertAccountRow: %v", err)
	}
	if err := UpdateAccountQuota(db, id, 50<<30, 10<<30); err != nil {
		t.Fatalf("UpdateAccountQuota: %v", err)
	}

	if _, err := UpsertAccountRow(db, AccountRow{Provider: "mega", Email: "a@example.com", QuotaGB: 0}); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	rows, _ := ListAccountRows(db)
	if rows[0].QuotaGB != 50 {
		t.Errorf("quota_total_gb = %v, want the polled 50 kept", rows[0].QuotaGB)
	}
}

func TestReauthFlagRoundTrip(t *testing.T) {
	db := newTestDB(t)
	if _, err := UpsertAccountRow(db, AccountRow{Provider: "fourshared", Email: "a@example.com"}); err != nil {
		t.Fatalf("UpsertAccountRow: %v", err)
	}

	if err := SetAccountReauth(db, "fourshared", "a@example.com", true, "token expired"); err != nil {
		t.Fatalf("SetAccountReauth: %v", err)
	}
	rows, _ := ListAccountRows(db)
	if !rows[0].NeedsReauth || rows[0].ReauthReason != "token expired" {
		t.Fatalf("row = %+v", rows[0])
	}

	// Storing new credentials clears the flag in the same statement, so the two
	// can never disagree.
	if err := SetAccountSecrets(db, "fourshared", "a@example.com", []byte("sealed"), TokenSourceApp); err != nil {
		t.Fatalf("SetAccountSecrets: %v", err)
	}
	rows, _ = ListAccountRows(db)
	if rows[0].NeedsReauth || rows[0].ReauthReason != "" {
		t.Fatalf("flag survived a credential write: %+v", rows[0])
	}
}

// Writing credentials for an account that does not exist must fail loudly
// rather than silently affecting no rows — that would report a successful
// re-authorization that stored nothing.
func TestSetAccountSecretsFailsWhenTheAccountIsAbsent(t *testing.T) {
	db := newTestDB(t)
	if err := SetAccountSecrets(db, "fourshared", "missing@example.com", []byte("sealed"), TokenSourceApp); err == nil {
		t.Fatal("expected an error for an unknown account")
	}
	if err := SetMainAccountSecrets(db, "fourshared", []byte("sealed"), TokenSourceApp); err == nil {
		t.Fatal("expected an error for an unknown main account")
	}
}

func TestMainAccountQuotaRoundTrip(t *testing.T) {
	db := newTestDB(t)
	if _, err := UpsertMainAccountRow(db, MainAccountRow{Provider: "mega", Email: "main@example.com"}); err != nil {
		t.Fatalf("UpsertMainAccountRow: %v", err)
	}

	quotas, err := ListMainAccountQuotas(db)
	if err != nil {
		t.Fatalf("ListMainAccountQuotas: %v", err)
	}
	if q := quotas["mega"]; q.LastSync != nil {
		t.Errorf("an unpolled main account should have no last-synced time, got %+v", q)
	}

	if err := UpdateMainAccountQuota(db, "mega", 20<<30, 5<<30); err != nil {
		t.Fatalf("UpdateMainAccountQuota: %v", err)
	}
	quotas, _ = ListMainAccountQuotas(db)
	q := quotas["mega"]
	if q.QuotaTotalGB != 20 || q.QuotaUsedGB != 5 || q.LastSync == nil {
		t.Fatalf("quota = %+v", q)
	}
}
