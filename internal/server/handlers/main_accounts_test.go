package handlers

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/syncsystem-net/back-me-up/internal/accounts"
	"github.com/syncsystem-net/back-me-up/internal/database"
)

func mainAccountsBody(t *testing.T, store *accounts.AccountStore) (string, []mainAccountResponse) {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "main.db"))
	if err != nil {
		t.Fatalf("opening database: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	h := &Handlers{db: db, accounts: store}
	w := do(t, h.GetMainAccounts, "GET", "/api/accounts/main", "")
	if w.Code != 200 {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	var got []mainAccountResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response %s: %v", w.Body.String(), err)
	}
	return w.Body.String(), got
}

func TestGetMainAccountsListsEachProvider(t *testing.T) {
	store := accounts.NewStore([]accounts.MainAccount{
		{Provider: accounts.ProviderMega, Email: "mega-main@example.com", Password: "megapass"},
		{Provider: accounts.ProviderFourShared, Email: "4s-main@example.com",
			ConsumerKey: "ck", ConsumerSecret: "cs", OAuthToken: "tok", OAuthTokenSecret: "toksec"},
	}, nil, accounts.OAuthApp{})

	raw, got := mainAccountsBody(t, store)
	if len(got) != 2 {
		t.Fatalf("got %d main accounts, want 2: %s", len(got), raw)
	}
	if got[0].Provider != "mega" || got[0].Email != "mega-main@example.com" || !got[0].Usable {
		t.Errorf("mega entry = %+v", got[0])
	}
	if !got[1].Usable || len(got[1].MissingKeys) != 0 {
		t.Errorf("4shared entry = %+v", got[1])
	}

	// The metadata DB is uploaded to these accounts, so their credentials are the
	// last thing that should ever reach a browser.
	for _, secret := range []string{"megapass", "ck", "cs", "tok", "toksec"} {
		if strings.Contains(raw, secret) {
			t.Errorf("response leaked credential %q: %s", secret, raw)
		}
	}
}

func TestGetMainAccountsReportsIncompleteConfiguration(t *testing.T) {
	store := accounts.NewStore([]accounts.MainAccount{
		{Provider: accounts.ProviderMega, Email: "mega-main@example.com"}, // no password
	}, nil, accounts.OAuthApp{})

	_, got := mainAccountsBody(t, store)
	if len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
	if got[0].Usable {
		t.Error("an account with no password should not report as usable")
	}
	if len(got[0].MissingKeys) != 1 || got[0].MissingKeys[0] != "MEGA_ACCOUNT_MAIN_PASSWORD" {
		t.Errorf("missing_keys = %v, want the full .env key name", got[0].MissingKeys)
	}
}

// With nothing configured the endpoint answers with an empty array, not null:
// the page renders it directly and `null.length` would break the empty state.
func TestGetMainAccountsWithNoneConfigured(t *testing.T) {
	raw, got := mainAccountsBody(t, accounts.NewStore(nil, nil, accounts.OAuthApp{}))
	if len(got) != 0 {
		t.Fatalf("got %+v", got)
	}
	if strings.TrimSpace(raw) != "[]" {
		t.Errorf("body = %s, want []", raw)
	}
}

// A store that never loaded (no .env) must answer the same way rather than
// panicking on a nil pointer.
func TestGetMainAccountsWithNilStore(t *testing.T) {
	raw, _ := mainAccountsBody(t, nil)
	if strings.TrimSpace(raw) != "[]" {
		t.Errorf("body = %s, want []", raw)
	}
}
