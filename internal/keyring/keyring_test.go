package keyring

import (
	"bytes"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/syncsystem-net/back-me-up/internal/database"
)

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func mustOpen(t *testing.T, db *sql.DB, pass string) *Keyring {
	t.Helper()
	k, err := Open(db, pass)
	if err != nil {
		t.Fatalf("Open(%q): %v", pass, err)
	}
	return k
}

func TestSealOpenRoundTrip(t *testing.T) {
	k := mustOpen(t, newTestDB(t), "correct horse battery staple")

	aad := AccountAAD("account", "mega", "a@example.com")
	blob, err := k.SealBlob(aad, []byte("paSs1$2178"))
	if err != nil {
		t.Fatalf("SealBlob: %v", err)
	}
	if bytes.Contains(blob, []byte("paSs1")) {
		t.Fatal("sealed blob contains the plaintext")
	}
	got, err := k.OpenBlob(aad, blob)
	if err != nil {
		t.Fatalf("OpenBlob: %v", err)
	}
	if string(got) != "paSs1$2178" {
		t.Fatalf("round trip returned %q", got)
	}
}

// A fresh nonce per write is the property that keeps GCM safe. Sealing the same
// plaintext repeatedly must never produce the same bytes twice.
func TestEveryWriteUsesAFreshNonce(t *testing.T) {
	k := mustOpen(t, newTestDB(t), "pass")

	seen := make(map[string]bool)
	for i := 0; i < 64; i++ {
		blob, err := k.SealBlob("aad", []byte("same plaintext every time"))
		if err != nil {
			t.Fatalf("SealBlob: %v", err)
		}
		if seen[string(blob)] {
			t.Fatalf("iteration %d repeated a sealed value: nonce reuse", i)
		}
		seen[string(blob)] = true
	}
}

func TestWrongPassphraseIsDetectedAtOpen(t *testing.T) {
	db := newTestDB(t)
	mustOpen(t, db, "the right one")

	_, err := Open(db, "the wrong one")
	if !errors.Is(err, ErrWrongPassphrase) {
		t.Fatalf("expected ErrWrongPassphrase, got %v", err)
	}
}

func TestBlankPassphraseReportsAndChangesNothing(t *testing.T) {
	db := newTestDB(t)

	_, err := Open(db, "")
	if !errors.Is(err, ErrNoPassphrase) {
		t.Fatalf("expected ErrNoPassphrase, got %v", err)
	}
	// Nothing may be initialised without a passphrase: a later correct open must
	// still be performing the install, not colliding with a half-written one.
	for _, key := range []string{SettingSalt, SettingParams, SettingVerifier} {
		v, err := database.GetSetting(db, key)
		if err != nil {
			t.Fatalf("GetSetting(%s): %v", key, err)
		}
		if v != "" {
			t.Fatalf("blank passphrase wrote %s = %q", key, v)
		}
	}
}

// The salt is per install, so the same passphrase against two databases must
// derive different keys — a blob from one must not open in the other.
func TestSaltIsPerInstall(t *testing.T) {
	one := mustOpen(t, newTestDB(t), "shared passphrase")
	two := mustOpen(t, newTestDB(t), "shared passphrase")

	blob, err := one.SealBlob("aad", []byte("secret"))
	if err != nil {
		t.Fatalf("SealBlob: %v", err)
	}
	if _, err := two.OpenBlob("aad", blob); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("expected a different install's key to fail, got %v", err)
	}
}

func TestTamperedCiphertextIsRejected(t *testing.T) {
	k := mustOpen(t, newTestDB(t), "pass")

	blob, err := k.SealBlob("aad", []byte("secret value"))
	if err != nil {
		t.Fatalf("SealBlob: %v", err)
	}

	for _, tc := range []struct {
		name    string
		mutate  func([]byte) []byte
		wantErr error
	}{
		{"flipped ciphertext bit", func(b []byte) []byte {
			c := append([]byte(nil), b...)
			c[len(c)-1] ^= 0x01
			return c
		}, ErrCorrupt},
		{"flipped nonce bit", func(b []byte) []byte {
			c := append([]byte(nil), b...)
			c[0] ^= 0x01
			return c
		}, ErrCorrupt},
		{"truncated", func(b []byte) []byte { return b[:len(b)-1] }, ErrCorrupt},
		{"empty", func([]byte) []byte { return nil }, ErrCorrupt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := k.OpenBlob("aad", tc.mutate(blob)); !errors.Is(err, tc.wantErr) {
				t.Fatalf("expected %v, got %v", tc.wantErr, err)
			}
		})
	}
}

// The AAD binds a blob to one account. A row copied over another row must fail
// to open rather than authenticate as the wrong account.
func TestBlobIsBoundToItsAccount(t *testing.T) {
	k := mustOpen(t, newTestDB(t), "pass")

	blob, err := k.SealBlob(AccountAAD("account", "mega", "a@example.com"), []byte("secret"))
	if err != nil {
		t.Fatalf("SealBlob: %v", err)
	}
	for _, aad := range []string{
		AccountAAD("account", "mega", "b@example.com"),
		AccountAAD("account", "fourshared", "a@example.com"),
		AccountAAD("main", "mega", "a@example.com"),
	} {
		if _, err := k.OpenBlob(aad, blob); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("blob opened under foreign aad %q (err=%v)", aad, err)
		}
	}
}

func TestReopeningWithTheSamePassphraseOpensExistingBlobs(t *testing.T) {
	db := newTestDB(t)
	first := mustOpen(t, db, "pass")
	blob, err := first.SealBlob("aad", []byte("secret"))
	if err != nil {
		t.Fatalf("SealBlob: %v", err)
	}

	second := mustOpen(t, db, "pass")
	got, err := second.OpenBlob("aad", blob)
	if err != nil {
		t.Fatalf("OpenBlob after reopen: %v", err)
	}
	if string(got) != "secret" {
		t.Fatalf("got %q", got)
	}
}

func TestSealJSONRoundTrip(t *testing.T) {
	k := mustOpen(t, newTestDB(t), "pass")

	type creds struct {
		Password string `json:"password"`
		Token    string `json:"token"`
	}
	in := creds{Password: "p@ss", Token: "abc123"}
	blob, err := k.SealJSON("aad", in)
	if err != nil {
		t.Fatalf("SealJSON: %v", err)
	}
	var out creds
	if err := k.OpenJSON("aad", blob, &out); err != nil {
		t.Fatalf("OpenJSON: %v", err)
	}
	if out != in {
		t.Fatalf("got %+v, want %+v", out, in)
	}
}

// Regression: a database whose salt row is missing but whose accounts still hold
// sealed credentials is damaged, not new. Installing a fresh salt there would
// derive a different key, quietly make every stored credential unopenable, and
// let the next .env import re-seal over the ciphertext — destroying an
// app-written token permanently. Refuse instead.
func TestMissingSaltBesideStoredCredentialsIsRefused(t *testing.T) {
	db := newTestDB(t)
	k := mustOpen(t, db, "pass")

	blob, err := k.SealBlob(AccountAAD("account", "mega", "a@example.com"), []byte("secret"))
	if err != nil {
		t.Fatalf("SealBlob: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO accounts (provider, email, secrets_enc) VALUES ('mega', 'a@example.com', ?)`, blob); err != nil {
		t.Fatalf("seeding a sealed account: %v", err)
	}

	// The salt goes missing (a restored partial backup, a hand-edited settings
	// table).
	if _, err := db.Exec(`DELETE FROM settings WHERE key = ?`, SettingSalt); err != nil {
		t.Fatalf("removing the salt: %v", err)
	}

	if _, err := Open(db, "pass"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("expected ErrCorrupt, got %v", err)
	}
	// And the ciphertext must be untouched by the refused open.
	var after []byte
	if err := db.QueryRow(`SELECT secrets_enc FROM accounts WHERE email = 'a@example.com'`).Scan(&after); err != nil {
		t.Fatalf("re-reading: %v", err)
	}
	if !bytes.Equal(blob, after) {
		t.Fatal("the refused open rewrote stored credentials")
	}
}

// The opposite case must still work: a genuinely fresh database installs.
func TestFreshDatabaseWithNoCredentialsInstalls(t *testing.T) {
	db := newTestDB(t)
	if _, err := Open(db, "pass"); err != nil {
		t.Fatalf("Open on a fresh database: %v", err)
	}
}
