package credentials

import (
	"bytes"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/syncsystem-net/back-me-up/internal/accounts"
	"github.com/syncsystem-net/back-me-up/internal/database"
	"github.com/syncsystem-net/back-me-up/internal/keyring"
)

const passphrase = "correct horse battery staple"

func newDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "creds.db")
	db, err := database.Open(path)
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, path
}

func newManager(t *testing.T, db *sql.DB, pass string) *Manager {
	t.Helper()
	kr, err := keyring.Open(db, pass)
	if err != nil {
		t.Fatalf("keyring.Open: %v", err)
	}
	return New(db, kr)
}

// megaEnv is a minimal .env-equivalent with one MEGA account.
func megaEnv(password string) *accounts.EnvConfig {
	return &accounts.EnvConfig{
		Accounts: []accounts.Account{{
			Provider: accounts.ProviderMega,
			Email:    "mega1@example.com",
			Password: password,
			QuotaGB:  20,
			Index:    1,
		}},
	}
}

// fourSharedEnv is one 4shared account with a full OAuth set.
func fourSharedEnv(token, secret string) *accounts.EnvConfig {
	return &accounts.EnvConfig{
		Accounts: []accounts.Account{{
			Provider:         accounts.ProviderFourShared,
			Email:            "4s1@example.com",
			Index:            1,
			ConsumerKey:      "ck",
			ConsumerSecret:   "cs",
			ConsumerDomain:   "backmeup.example.com",
			OAuthToken:       token,
			OAuthTokenSecret: secret,
		}},
	}
}

func mustImport(t *testing.T, m *Manager, env *accounts.EnvConfig) {
	t.Helper()
	if err := m.Import(env); err != nil {
		t.Fatalf("Import: %v", err)
	}
}

// rawSecrets returns the stored ciphertext for one account, for the tests that
// assert on what is (and is not) on disk.
func rawSecrets(t *testing.T, db *sql.DB, provider, email string) []byte {
	t.Helper()
	var blob []byte
	err := db.QueryRow(`SELECT secrets_enc FROM accounts WHERE provider = ? AND email = ?`, provider, email).Scan(&blob)
	if err != nil {
		t.Fatalf("reading stored secrets: %v", err)
	}
	return blob
}

func TestImportSealsCredentialsAndLoadsThem(t *testing.T) {
	db, _ := newDB(t)
	m := newManager(t, db, passphrase)
	mustImport(t, m, megaEnv("paSs1$2178"))

	got, ok := m.Store().Find("mega", "mega1@example.com")
	if !ok {
		t.Fatal("account did not reach the running store")
	}
	if got.Password != "paSs1$2178" {
		t.Errorf("password = %q, want the imported one", got.Password)
	}
	if got.QuotaGB != 20 {
		t.Errorf("quota = %v, want 20", got.QuotaGB)
	}

	// The point of the exercise: the metadata database is uploaded to the cloud
	// after every job, so the password must not be in it in the clear.
	if bytes.Contains(rawSecrets(t, db, "mega", "mega1@example.com"), []byte("paSs1")) {
		t.Error("stored credentials contain the plaintext password")
	}
}

// The whole file on disk must not carry the plaintext either — a column that
// happens to be sealed is no good if the same value is written somewhere else.
func TestPlaintextCredentialsNeverReachTheDatabaseFile(t *testing.T) {
	db, path := newDB(t)
	m := newManager(t, db, passphrase)
	mustImport(t, m, fourSharedEnv("tok-secret-value", "tok-secret-secret"))
	if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}

	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading database file: %v", err)
	}
	// Long, distinctive values: a short one could appear in a binary file by
	// coincidence and make this pass for the wrong reason.
	for _, secret := range []string{"tok-secret-value", "tok-secret-secret"} {
		if bytes.Contains(blob, []byte(secret)) {
			t.Errorf("database file contains credential %q in the clear", secret)
		}
	}
}

// The rule the Re-authorize button depends on: a token the app obtained itself
// is never overwritten by the stale value still sitting in .env.
func TestAppWrittenTokenSurvivesAStaleEnvValue(t *testing.T) {
	db, _ := newDB(t)
	m := newManager(t, db, passphrase)
	mustImport(t, m, fourSharedEnv("old-token", "old-secret"))

	if err := m.SaveOAuthToken("fourshared", "4s1@example.com", "new-token", "new-secret", false); err != nil {
		t.Fatalf("SaveOAuthToken: %v", err)
	}

	// A restart: .env still holds the token the user re-authorized to replace.
	mustImport(t, m, fourSharedEnv("old-token", "old-secret"))

	got, _ := m.Store().Find("fourshared", "4s1@example.com")
	if got.OAuthToken != "new-token" || got.OAuthTokenSecret != "new-secret" {
		t.Fatalf("re-authorized token was overwritten by .env: %q/%q", got.OAuthToken, got.OAuthTokenSecret)
	}
	// The rest of the credential document must survive the token write.
	if got.ConsumerKey != "ck" || got.ConsumerDomain != "backmeup.example.com" {
		t.Errorf("token write clobbered other credentials: %+v", got)
	}
}

// The flip side: a token that was only ever adopted from .env is still updated
// when .env changes, or pasting a fresh token in by hand would stop working.
func TestEnvTokenIsAdoptedWhenTheAppNeverWroteOne(t *testing.T) {
	db, _ := newDB(t)
	m := newManager(t, db, passphrase)
	mustImport(t, m, fourSharedEnv("token-a", "secret-a"))
	mustImport(t, m, fourSharedEnv("token-b", "secret-b"))

	got, _ := m.Store().Find("fourshared", "4s1@example.com")
	if got.OAuthToken != "token-b" || got.OAuthTokenSecret != "secret-b" {
		t.Fatalf("env token not adopted: %q/%q", got.OAuthToken, got.OAuthTokenSecret)
	}
}

// Half a token pair cannot sign anything, so it is not adopted at all.
func TestIncompleteEnvTokenPairIsIgnored(t *testing.T) {
	db, _ := newDB(t)
	m := newManager(t, db, passphrase)
	mustImport(t, m, fourSharedEnv("token-a", "secret-a"))
	mustImport(t, m, fourSharedEnv("token-b", "")) // secret missing

	got, _ := m.Store().Find("fourshared", "4s1@example.com")
	if got.OAuthToken != "token-a" {
		t.Fatalf("adopted half a token pair: %q/%q", got.OAuthToken, got.OAuthTokenSecret)
	}
}

func TestChangedPasswordInEnvUpdatesTheStoredCredential(t *testing.T) {
	db, _ := newDB(t)
	m := newManager(t, db, passphrase)
	mustImport(t, m, megaEnv("old-password"))
	mustImport(t, m, megaEnv("new-password"))

	got, _ := m.Store().Find("mega", "mega1@example.com")
	if got.Password != "new-password" {
		t.Fatalf("password = %q, want the updated one", got.Password)
	}
}

// Absence is not deletion: a blank value in .env must never erase a stored
// credential, or commenting a line out would silently break an account.
func TestBlankEnvValueDoesNotEraseAStoredCredential(t *testing.T) {
	db, _ := newDB(t)
	m := newManager(t, db, passphrase)
	mustImport(t, m, megaEnv("keep-me"))
	mustImport(t, m, megaEnv(""))

	got, _ := m.Store().Find("mega", "mega1@example.com")
	if got.Password != "keep-me" {
		t.Fatalf("password = %q, want it kept", got.Password)
	}
}

// Removing an upload target has to be deliberate, not a side effect of editing
// a file — an account gone from .env keeps its row and its credentials.
func TestAccountMissingFromEnvIsKept(t *testing.T) {
	db, _ := newDB(t)
	m := newManager(t, db, passphrase)
	mustImport(t, m, megaEnv("pass"))
	mustImport(t, m, &accounts.EnvConfig{})

	got, ok := m.Store().Find("mega", "mega1@example.com")
	if !ok {
		t.Fatal("account disappeared when .env stopped mentioning it")
	}
	if got.Password != "pass" {
		t.Errorf("credentials lost: %+v", got)
	}
}

// Main accounts are the deliberate exception: .env is their only removal path,
// and continuing to copy the metadata database to a destination the user
// deleted would be worse than the asymmetry.
func TestMainAccountRemovedFromEnvIsDeleted(t *testing.T) {
	db, _ := newDB(t)
	m := newManager(t, db, passphrase)

	withMain := &accounts.EnvConfig{Mains: []accounts.MainAccount{{
		Provider: accounts.ProviderMega, Email: "main@example.com", Password: "mainpass",
	}}}
	mustImport(t, m, withMain)
	if _, ok := m.Store().MainFor(accounts.ProviderMega); !ok {
		t.Fatal("main account was not imported")
	}

	mustImport(t, m, &accounts.EnvConfig{})
	if _, ok := m.Store().MainFor(accounts.ProviderMega); ok {
		t.Fatal("main account survived removal from .env")
	}
	rows, err := database.ListMainAccountRows(db)
	if err != nil {
		t.Fatalf("ListMainAccountRows: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("main account row survived: %+v", rows)
	}
}

// A main account keyed to a provider whose email changed must not inherit the
// previous account's credentials — they belong to a different login.
func TestChangedMainAccountEmailDoesNotInheritCredentials(t *testing.T) {
	db, _ := newDB(t)
	m := newManager(t, db, passphrase)

	mustImport(t, m, &accounts.EnvConfig{Mains: []accounts.MainAccount{{
		Provider: accounts.ProviderMega, Email: "first@example.com", Password: "first-pass",
	}}})
	mustImport(t, m, &accounts.EnvConfig{Mains: []accounts.MainAccount{{
		Provider: accounts.ProviderMega, Email: "second@example.com",
	}}})

	got, ok := m.Store().MainFor(accounts.ProviderMega)
	if !ok {
		t.Fatal("main account missing")
	}
	if got.Email != "second@example.com" {
		t.Fatalf("email = %q", got.Email)
	}
	if got.Password == "first-pass" {
		t.Error("new main account inherited the previous account's password")
	}
}

// The most dangerous failure in the phase: a wrong passphrase must not be able
// to destroy credentials that the right one would still open.
func TestWrongPassphraseChangesNothingOnDisk(t *testing.T) {
	db, _ := newDB(t)
	m := newManager(t, db, passphrase)
	mustImport(t, m, megaEnv("original-password"))
	before := rawSecrets(t, db, "mega", "mega1@example.com")

	// A boot with the wrong passphrase: the keyring refuses, and main.go locks.
	if _, err := keyring.Open(db, "wrong passphrase"); !errors.Is(err, keyring.ErrWrongPassphrase) {
		t.Fatalf("expected ErrWrongPassphrase, got %v", err)
	}
	locked := NewLocked(db, "wrong passphrase")

	// Every write path must refuse rather than re-seal.
	if err := locked.Import(megaEnv("attacker-supplied")); !errors.Is(err, accounts.ErrLocked) {
		t.Fatalf("Import while locked = %v, want ErrLocked", err)
	}
	if err := locked.Reload(); !errors.Is(err, accounts.ErrLocked) {
		t.Fatalf("Reload while locked = %v, want ErrLocked", err)
	}
	if err := locked.SaveOAuthToken("fourshared", "4s1@example.com", "t", "s", false); !errors.Is(err, accounts.ErrLocked) {
		t.Fatalf("SaveOAuthToken while locked = %v, want ErrLocked", err)
	}

	if after := rawSecrets(t, db, "mega", "mega1@example.com"); !bytes.Equal(before, after) {
		t.Fatal("stored credentials were rewritten while locked")
	}

	// And the credentials are still recoverable with the right passphrase.
	recovered := newManager(t, db, passphrase)
	if err := recovered.Import(nil); err != nil {
		t.Fatalf("reload after locked run: %v", err)
	}
	got, ok := recovered.Store().Find("mega", "mega1@example.com")
	if !ok || got.Password != "original-password" {
		t.Fatalf("credentials not recoverable after a locked run: %+v", got)
	}
}

// A locked store must be empty and say why, so no path can mistake "cannot open
// the credentials" for "this account is not configured".
func TestLockedStoreIsEmptyAndExplainsItself(t *testing.T) {
	db, _ := newDB(t)
	m := newManager(t, db, passphrase)
	mustImport(t, m, megaEnv("pass"))

	locked := NewLocked(db, "passphrase is missing")
	if !locked.Locked() {
		t.Fatal("Locked() = false")
	}
	if len(locked.Store().All()) != 0 {
		t.Error("a locked store must expose no accounts")
	}
	if locked.Store().LockReason() != "passphrase is missing" {
		t.Errorf("LockReason = %q", locked.Store().LockReason())
	}
	if _, ok := locked.Store().Find("mega", "mega1@example.com"); ok {
		t.Error("a locked store must not resolve an account")
	}
}

// Re-importing an unchanged .env must be a no-op on disk, not a re-seal with a
// fresh nonce every boot: pointless writes, and they make it impossible to tell
// from the file whether anything actually changed.
func TestReimportingUnchangedEnvRewritesNothing(t *testing.T) {
	db, _ := newDB(t)
	m := newManager(t, db, passphrase)
	mustImport(t, m, megaEnv("pass"))
	before := rawSecrets(t, db, "mega", "mega1@example.com")

	mustImport(t, m, megaEnv("pass"))
	if after := rawSecrets(t, db, "mega", "mega1@example.com"); !bytes.Equal(before, after) {
		t.Error("an unchanged .env re-sealed the stored credentials")
	}
}

// The flag has to survive a restart, which is the reason it is stored at all
// rather than kept in memory.
func TestReauthFlagPersistsAndClearsOnANewToken(t *testing.T) {
	db, _ := newDB(t)
	m := newManager(t, db, passphrase)
	mustImport(t, m, fourSharedEnv("token-a", "secret-a"))

	if err := m.FlagReauth("fourshared", "4s1@example.com", "token expired", false); err != nil {
		t.Fatalf("FlagReauth: %v", err)
	}
	got, _ := m.Store().Find("fourshared", "4s1@example.com")
	if !got.NeedsReauth || got.ReauthReason != "token expired" {
		t.Fatalf("flag not reflected in the store: %+v", got)
	}

	// A fresh process reading the same database still sees it.
	restarted := newManager(t, db, passphrase)
	if err := restarted.Import(nil); err != nil {
		t.Fatalf("Import: %v", err)
	}
	if got, _ := restarted.Store().Find("fourshared", "4s1@example.com"); !got.NeedsReauth {
		t.Error("flag did not survive a restart")
	}

	// Re-authorizing clears it.
	if err := restarted.SaveOAuthToken("fourshared", "4s1@example.com", "token-b", "secret-b", false); err != nil {
		t.Fatalf("SaveOAuthToken: %v", err)
	}
	got, _ = restarted.Store().Find("fourshared", "4s1@example.com")
	if got.NeedsReauth {
		t.Error("re-authorization did not clear the flag")
	}
	if got.OAuthToken != "token-b" {
		t.Errorf("token = %q, want the new one", got.OAuthToken)
	}
}

// A token write with a missing half is refused rather than stored: it would
// leave an account that cannot sign anything and looks configured.
func TestSaveOAuthTokenRefusesAnIncompletePair(t *testing.T) {
	db, _ := newDB(t)
	m := newManager(t, db, passphrase)
	mustImport(t, m, fourSharedEnv("token-a", "secret-a"))

	if err := m.SaveOAuthToken("fourshared", "4s1@example.com", "token-b", "", false); err == nil {
		t.Fatal("expected an error for an incomplete token pair")
	}
	got, _ := m.Store().Find("fourshared", "4s1@example.com")
	if got.OAuthToken != "token-a" {
		t.Errorf("refused write still changed the stored token: %q", got.OAuthToken)
	}
}

// Main accounts are never upload targets. This is enforced by them living in
// their own table, and it is the invariant that table exists to protect.
func TestMainAccountsNeverAppearAsUploadTargets(t *testing.T) {
	db, _ := newDB(t)
	m := newManager(t, db, passphrase)

	mustImport(t, m, &accounts.EnvConfig{
		Mains: []accounts.MainAccount{{
			Provider: accounts.ProviderMega, Email: "shared@example.com", Password: "mainpass",
		}},
		Accounts: []accounts.Account{{
			Provider: accounts.ProviderMega, Email: "shared@example.com", Password: "uploadpass", Index: 1,
		}},
	})

	// Same email on both roles — the case a single flagged row could not
	// represent — must produce exactly one upload target and one main account,
	// each with its own credentials.
	all := m.Store().All()
	if len(all) != 1 {
		t.Fatalf("expected one upload target, got %+v", all)
	}
	if all[0].Password != "uploadpass" {
		t.Errorf("upload target has the main account's credentials: %+v", all[0])
	}
	main, ok := m.Store().MainFor(accounts.ProviderMega)
	if !ok || main.Password != "mainpass" {
		t.Errorf("main account = %+v", main)
	}

	dbAccts, err := database.ListDBAccounts(db)
	if err != nil {
		t.Fatalf("ListDBAccounts: %v", err)
	}
	if len(dbAccts) != 1 {
		t.Fatalf("the accounts table (which feeds the upload modal and the users view) has %d rows, want 1", len(dbAccts))
	}
}

// Regression: a main account is keyed by provider, so a re-authorization request
// may carry a different email than the stored row — or, per the API, none at
// all. Sealing under the caller's version bound the blob to an identity nothing
// ever opens it with, destroying that account's entire credential document.
func TestMainAccountTokenWriteSealsUnderTheStoredEmail(t *testing.T) {
	db, _ := newDB(t)
	m := newManager(t, db, passphrase)
	mustImport(t, m, &accounts.EnvConfig{Mains: []accounts.MainAccount{{
		Provider:         accounts.ProviderFourShared,
		Email:            "4s-main@example.com",
		ConsumerKey:      "ck",
		ConsumerSecret:   "cs",
		ConsumerDomain:   "backmeup.example.com",
		OAuthToken:       "old-token",
		OAuthTokenSecret: "old-secret",
	}}})

	// The email the caller passes is deliberately not the stored one.
	if err := m.SaveOAuthToken("fourshared", "", "fresh-token", "fresh-secret", true); err != nil {
		t.Fatalf("SaveOAuthToken: %v", err)
	}

	got, ok := m.Store().MainFor(accounts.ProviderFourShared)
	if !ok {
		t.Fatal("main account disappeared")
	}
	if got.OAuthToken != "fresh-token" || got.OAuthTokenSecret != "fresh-secret" {
		t.Fatalf("token = %q/%q, want the re-authorized pair", got.OAuthToken, got.OAuthTokenSecret)
	}
	// The rest of the document must still be readable — an AAD mismatch would
	// blank all of it.
	if got.ConsumerKey != "ck" || got.ConsumerSecret != "cs" || got.ConsumerDomain != "backmeup.example.com" {
		t.Fatalf("credential document was lost: %+v", got)
	}

	// And it must survive a restart, which is where an unopenable blob shows up.
	restarted := newManager(t, db, passphrase)
	if err := restarted.Import(nil); err != nil {
		t.Fatalf("Import: %v", err)
	}
	got, _ = restarted.Store().MainFor(accounts.ProviderFourShared)
	if got.OAuthToken != "fresh-token" || got.ConsumerKey != "ck" {
		t.Fatalf("credentials unreadable after restart: %+v", got)
	}
}

// Regression: Import(nil) means "nothing to reconcile" and must not be read as
// "no main accounts are declared", which would delete them. main.go passes nil
// when .env cannot be read — a typo in that file must not cost the user their
// database-backup destinations.
func TestImportWithNoEnvLeavesMainAccountsAlone(t *testing.T) {
	db, _ := newDB(t)
	m := newManager(t, db, passphrase)
	mustImport(t, m, &accounts.EnvConfig{Mains: []accounts.MainAccount{{
		Provider: accounts.ProviderMega, Email: "main@example.com", Password: "mainpass",
	}}})

	if err := m.Import(nil); err != nil {
		t.Fatalf("Import(nil): %v", err)
	}
	got, ok := m.Store().MainFor(accounts.ProviderMega)
	if !ok {
		t.Fatal("Import(nil) deleted the main account")
	}
	if got.Password != "mainpass" {
		t.Errorf("credentials lost: %+v", got)
	}
}

// Regression: the flag has to be withdrawable. A transient rejection used to
// leave a permanent badge that only an unnecessary re-authorization could clear.
func TestClearReauthWithdrawsTheFlag(t *testing.T) {
	db, _ := newDB(t)
	m := newManager(t, db, passphrase)
	mustImport(t, m, fourSharedEnv("token-a", "secret-a"))

	if err := m.FlagReauth("fourshared", "4s1@example.com", "token expired", false); err != nil {
		t.Fatalf("FlagReauth: %v", err)
	}
	if err := m.ClearReauth("fourshared", "4s1@example.com", false); err != nil {
		t.Fatalf("ClearReauth: %v", err)
	}
	got, _ := m.Store().Find("fourshared", "4s1@example.com")
	if got.NeedsReauth || got.ReauthReason != "" {
		t.Fatalf("flag survived: %+v", got)
	}

	// Clearing an unflagged account is a no-op, not an error: it runs after every
	// successful login.
	if err := m.ClearReauth("fourshared", "4s1@example.com", false); err != nil {
		t.Fatalf("ClearReauth on an unflagged account: %v", err)
	}
	if err := m.ClearReauth("mega", "nobody@example.com", false); err != nil {
		t.Fatalf("ClearReauth on an unknown account should be a no-op: %v", err)
	}
}

func TestClearReauthWithdrawsAMainAccountFlag(t *testing.T) {
	db, _ := newDB(t)
	m := newManager(t, db, passphrase)
	mustImport(t, m, &accounts.EnvConfig{Mains: []accounts.MainAccount{{
		Provider: accounts.ProviderMega, Email: "main@example.com", Password: "p",
	}}})

	if err := m.FlagReauth("mega", "main@example.com", "rejected", true); err != nil {
		t.Fatalf("FlagReauth: %v", err)
	}
	if got, _ := m.Store().MainFor(accounts.ProviderMega); !got.NeedsReauth {
		t.Fatal("flag not set")
	}
	if err := m.ClearReauth("mega", "main@example.com", true); err != nil {
		t.Fatalf("ClearReauth: %v", err)
	}
	if got, _ := m.Store().MainFor(accounts.ProviderMega); got.NeedsReauth {
		t.Fatal("main account flag survived")
	}
}
