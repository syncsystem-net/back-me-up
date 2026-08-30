package handlers

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/syncsystem-net/back-me-up/internal/accounts"
	"github.com/syncsystem-net/back-me-up/internal/database"
	"github.com/syncsystem-net/back-me-up/internal/keyring"
)

// lockedHandlers builds Handlers whose credential store could not be opened.
func lockedHandlers(t *testing.T, reason string) *Handlers {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "locked.db"))
	if err != nil {
		t.Fatalf("opening database: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	store := accounts.NewStore(nil, nil, accounts.OAuthApp{})
	store.Lock(reason)
	return &Handlers{db: db, accounts: store}
}

// Locked credentials are not a bad request and not a server fault: the request
// was fine and the server is healthy, it just cannot act until the passphrase is
// fixed. 503 is the answer, and the body carries the reason the UI shows.
func TestCredentialOperationsRefuseWhileLocked(t *testing.T) {
	const reason = "No credential passphrase is set."
	h := lockedHandlers(t, reason)

	cases := []struct {
		name   string
		method string
		target string
		body   string
		fn     http.HandlerFunc
	}{
		{"upload", "POST", "/api/backups", `{"owner_email":"a@example.com","title":"t","source_path":"."}`, h.PostBackups},
		{"download", "GET", "/api/jobs/1/download", "", h.DownloadJob},
		{"delete archive", "DELETE", "/api/jobs/1", `{"confirm":"DELETE"}`, h.DeleteJob},
		{"delete record", "DELETE", "/api/backups/1", `{"confirm":"DELETE"}`, h.DeleteBackup},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := do(t, tc.fn, tc.method, tc.target, tc.body)
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503: %s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), reason) {
				t.Errorf("body does not carry the lock reason: %s", w.Body.String())
			}
		})
	}
}

// The banner is driven by this endpoint, so it must report the lock and name the
// .env key to fix — the message is the whole feature.
func TestCredentialsStatusReportsTheLock(t *testing.T) {
	const reason = "BACKMEUP_CREDENTIAL_PASSPHRASE does not match this database."
	h := lockedHandlers(t, reason)

	w := do(t, h.GetCredentialsStatus, "GET", "/api/credentials", "")
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	var got credentialsStatusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding %s: %v", w.Body.String(), err)
	}
	if !got.Locked || got.Reason != reason {
		t.Fatalf("response = %+v", got)
	}
	if got.EnvKey != keyring.EnvKey {
		t.Errorf("env_key = %q, want %q", got.EnvKey, keyring.EnvKey)
	}
}

// An unlocked server must not report a lock, or the banner would be permanent.
func TestCredentialsStatusWhenUnlocked(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "ok.db"))
	if err != nil {
		t.Fatalf("opening database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	h := &Handlers{db: db, accounts: accounts.NewStore(nil, nil, accounts.OAuthApp{})}

	w := do(t, h.GetCredentialsStatus, "GET", "/api/credentials", "")
	var got credentialsStatusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding %s: %v", w.Body.String(), err)
	}
	if got.Locked {
		t.Fatalf("reported locked: %+v", got)
	}
}

// The settings API serves a fixed whitelist. The crypto material happens to live
// in the same table, and a PUT that could overwrite the salt would make every
// stored credential permanently unopenable.
func TestSettingsAPINeverExposesOrAcceptsCryptoMaterial(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatalf("opening database: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// Install a keyring so the crypto rows exist to be leaked.
	if _, err := keyring.Open(db, "a passphrase"); err != nil {
		t.Fatalf("keyring.Open: %v", err)
	}
	saltBefore, err := database.GetSetting(db, keyring.SettingSalt)
	if err != nil || saltBefore == "" {
		t.Fatalf("expected a stored salt, got %q (%v)", saltBefore, err)
	}

	w := do(t, GetSettingsHandler(db), "GET", "/api/settings", "")
	for _, key := range []string{keyring.SettingSalt, keyring.SettingParams, keyring.SettingVerifier, saltBefore} {
		if strings.Contains(w.Body.String(), key) {
			t.Errorf("settings response exposed %q: %s", key, w.Body.String())
		}
	}

	// A PUT naming those keys must not touch them — including alongside the
	// provider limits, the second key this endpoint learned to write.
	body := `{"exclude_terms":["node_modules"],` +
		`"provider_limits":{"fourshared":{"free":{"max_file_bytes":123}}},` +
		`"` + keyring.SettingSalt + `":"00","` + keyring.SettingVerifier + `":"00"}`
	if w := do(t, PutSettingsHandler(db), "PUT", "/api/settings", body); w.Code != 200 {
		t.Fatalf("PUT status = %d: %s", w.Code, w.Body.String())
	}
	saltAfter, _ := database.GetSetting(db, keyring.SettingSalt)
	if saltAfter != saltBefore {
		t.Fatalf("the settings API overwrote the credential salt: %q -> %q", saltBefore, saltAfter)
	}
	// And the keyring still opens with the original passphrase.
	if _, err := keyring.Open(db, "a passphrase"); err != nil {
		t.Fatalf("keyring no longer opens after a settings write: %v", err)
	}
	// The legitimate half of that same request must still have applied.
	if got := database.GetProviderLimits(db).For("fourshared", "free").MaxFileBytes; got != 123 {
		t.Errorf("provider limit = %d, want the written 123", got)
	}
}
