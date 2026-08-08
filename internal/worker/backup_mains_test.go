package worker

import (
	"context"
	"fmt"
	"io"
	"testing"

	"github.com/syncsystem-net/back-me-up/internal/accounts"
	"github.com/syncsystem-net/back-me-up/internal/provider"
)

// stubMainProvider stands in for a backend the metadata-DB backup uploads to.
// Login and Upload can be made to fail independently, because those are the two
// ways a real destination drops out: a rejected credential and a refused upload.
type stubMainProvider struct {
	name      string
	loginErr  error
	uploadErr error
	uploaded  []string
}

func (s *stubMainProvider) Name() string { return s.name }
func (s *stubMainProvider) Login(context.Context, string, string) error {
	return s.loginErr
}
func (s *stubMainProvider) Upload(_ context.Context, _, remoteName string, _ func(provider.Progress)) (string, error) {
	if s.uploadErr != nil {
		return "", s.uploadErr
	}
	s.uploaded = append(s.uploaded, remoteName)
	return "ref-" + remoteName, nil
}
func (s *stubMainProvider) Download(context.Context, string, io.Writer) error {
	return fmt.Errorf("not used")
}
func (s *stubMainProvider) Delete(context.Context, string) error { return fmt.Errorf("not used") }
func (s *stubMainProvider) FindByName(context.Context, string) (string, bool, error) {
	return "", false, nil
}
func (s *stubMainProvider) List(context.Context) ([]provider.RemoteFile, error) { return nil, nil }
func (s *stubMainProvider) ReadRange(context.Context, string, []byte, int64) (int, error) {
	return 0, provider.ErrRangeUnsupported
}
func (s *stubMainProvider) GetQuota(context.Context) (int64, int64, error) { return 0, 0, nil }

// mainsFixture wires a Worker whose main-account providers are stubs, keyed by
// provider name.
func mainsFixture(t *testing.T, mains []accounts.MainAccount) (*Worker, map[string]*stubMainProvider) {
	t.Helper()
	stubs := map[string]*stubMainProvider{}
	for _, m := range mains {
		stubs[string(m.Provider)] = &stubMainProvider{name: string(m.Provider)}
	}
	w := &Worker{accounts: &accounts.AccountStore{Mains: mains}}
	w.newMainProvider = func(m accounts.MainAccount) (provider.Provider, error) {
		s, ok := stubs[string(m.Provider)]
		if !ok {
			return nil, fmt.Errorf("no stub for %s", m.Provider)
		}
		return s, nil
	}
	return w, stubs
}

func megaMain() accounts.MainAccount {
	return accounts.MainAccount{Provider: accounts.ProviderMega, Email: "mega-main@example.com", Password: "pw"}
}

func fourSharedMain() accounts.MainAccount {
	return accounts.MainAccount{
		Provider: accounts.ProviderFourShared, Email: "4s-main@example.com",
		ConsumerKey: "ck", ConsumerSecret: "cs", OAuthToken: "t", OAuthTokenSecret: "ts",
	}
}

func TestUploadDBToMainsSendsToEveryDestination(t *testing.T) {
	mains := []accounts.MainAccount{megaMain(), fourSharedMain()}
	w, stubs := mainsFixture(t, mains)

	if got := w.uploadDBToMains(context.Background(), mains, "db.tmp", "meta.db"); got != 2 {
		t.Fatalf("uploaded to %d destinations, want 2", got)
	}
	for name, s := range stubs {
		if len(s.uploaded) != 1 || s.uploaded[0] != "meta.db" {
			t.Errorf("%s received %v, want [meta.db]", name, s.uploaded)
		}
	}
}

// The rule carried over from resilient deletes: a multi-target operation never
// stops at the first failure. An expired 4shared token must not cost MEGA its
// copy of the index.
func TestUploadDBToMainsContinuesAfterAFailedDestination(t *testing.T) {
	mains := []accounts.MainAccount{fourSharedMain(), megaMain()}
	w, stubs := mainsFixture(t, mains)
	stubs["fourshared"].loginErr = provider.ErrAuthExpired

	if got := w.uploadDBToMains(context.Background(), mains, "db.tmp", "meta.db"); got != 1 {
		t.Fatalf("uploaded to %d destinations, want 1", got)
	}
	if len(stubs["fourshared"].uploaded) != 0 {
		t.Errorf("4shared uploaded despite a failed login: %v", stubs["fourshared"].uploaded)
	}
	if len(stubs["mega"].uploaded) != 1 {
		t.Errorf("MEGA missed its copy because 4shared failed first: %v", stubs["mega"].uploaded)
	}
}

// A failure at upload time (not login) must be contained the same way.
func TestUploadDBToMainsContinuesAfterAFailedUpload(t *testing.T) {
	mains := []accounts.MainAccount{fourSharedMain(), megaMain()}
	w, stubs := mainsFixture(t, mains)
	stubs["fourshared"].uploadErr = fmt.Errorf("403.0201 already exists")

	if got := w.uploadDBToMains(context.Background(), mains, "db.tmp", "meta.db"); got != 1 {
		t.Fatalf("uploaded to %d destinations, want 1", got)
	}
	if len(stubs["mega"].uploaded) != 1 {
		t.Errorf("MEGA missed its copy: %v", stubs["mega"].uploaded)
	}
}

// Every destination failing is reported as zero successes rather than a panic or
// a partial claim.
func TestUploadDBToMainsAllDestinationsFail(t *testing.T) {
	mains := []accounts.MainAccount{fourSharedMain(), megaMain()}
	w, stubs := mainsFixture(t, mains)
	stubs["fourshared"].loginErr = provider.ErrAuthExpired
	stubs["mega"].loginErr = provider.ErrAuthExpired

	if got := w.uploadDBToMains(context.Background(), mains, "db.tmp", "meta.db"); got != 0 {
		t.Fatalf("uploaded to %d destinations, want 0", got)
	}
}

// A main account missing credentials is skipped before any network call, so a
// half-configured entry cannot produce a confusing login failure after each job.
func TestUsableMainsSkipsIncompleteAccounts(t *testing.T) {
	incomplete := accounts.MainAccount{Provider: accounts.ProviderMega, Email: "mega-main@example.com"} // no password
	w := &Worker{accounts: &accounts.AccountStore{Mains: []accounts.MainAccount{incomplete, fourSharedMain()}}}

	usable := w.usableMains()
	if len(usable) != 1 || usable[0].Provider != accounts.ProviderFourShared {
		t.Fatalf("usableMains = %+v, want only the 4shared account", usable)
	}
}

func TestUsableMainsWithNoAccountsConfigured(t *testing.T) {
	w := &Worker{accounts: &accounts.AccountStore{}}
	if got := w.usableMains(); len(got) != 0 {
		t.Fatalf("usableMains = %+v, want none", got)
	}
}

// A main account whose provider we do not implement must not take the others
// down with it.
func TestUsableMainsSkipsUnsupportedProvider(t *testing.T) {
	unknown := accounts.MainAccount{Provider: "dropbox", Email: "x@example.com", Password: "pw"}
	w := &Worker{accounts: &accounts.AccountStore{Mains: []accounts.MainAccount{unknown, megaMain()}}}

	usable := w.usableMains()
	if len(usable) != 1 || usable[0].Provider != accounts.ProviderMega {
		t.Fatalf("usableMains = %+v, want only the MEGA account", usable)
	}
}

// The OAuth credentials handed to a 4shared main account come from the main
// account itself — the whole point of giving it its own credential set, rather
// than borrowing from a numbered account that happens to share an email.
func TestMainOAuthUsesTheMainAccountsOwnCredentials(t *testing.T) {
	creds := mainOAuth(fourSharedMain())
	if creds.ConsumerKey != "ck" || creds.ConsumerSecret != "cs" || creds.Token != "t" || creds.TokenSecret != "ts" {
		t.Errorf("mainOAuth = %+v", creds)
	}
	if got := mainOAuth(megaMain()); got != (provider.OAuthCreds{}) {
		t.Errorf("a password provider should get no OAuth creds, got %+v", got)
	}
}
