package reauth

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/syncsystem-net/back-me-up/internal/accounts"
	"github.com/syncsystem-net/back-me-up/internal/credentials"
	"github.com/syncsystem-net/back-me-up/internal/database"
	"github.com/syncsystem-net/back-me-up/internal/keyring"
	"github.com/syncsystem-net/back-me-up/internal/provider/oauth1"
)

// fakeProvider stands in for 4shared's three OAuth endpoints.
type fakeProvider struct {
	srv *httptest.Server
	// callback is what the client asked us to redirect to; the test plays the
	// browser and calls it.
	callback string
}

func newFakeProvider(t *testing.T) *fakeProvider {
	t.Helper()
	f := &fakeProvider{}
	mux := http.NewServeMux()
	mux.HandleFunc("/initiate", func(w http.ResponseWriter, r *http.Request) {
		// 4shared takes the callback in the OAuth Authorization header, which the
		// signer builds; parsing it back out is enough for the test to know where
		// to redirect.
		f.callback = callbackFromAuthHeader(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/x-www-form-urlencoded")
		fmt.Fprint(w, "oauth_token=reqtok&oauth_token_secret=reqsec")
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-www-form-urlencoded")
		fmt.Fprint(w, "oauth_token=fresh-token&oauth_token_secret=fresh-secret")
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeProvider) endpoints() oauth1.Endpoints {
	return oauth1.Endpoints{
		Initiate:  f.srv.URL + "/initiate",
		Authorize: f.srv.URL + "/authorize",
		Token:     f.srv.URL + "/token",
	}
}

func callbackFromAuthHeader(h string) string {
	const key = `oauth_callback="`
	i := len(key)
	start := indexOf(h, key)
	if start < 0 {
		return ""
	}
	rest := h[start+i:]
	end := indexOf(rest, `"`)
	if end < 0 {
		return ""
	}
	decoded, err := url.QueryUnescape(rest[:end])
	if err != nil {
		return rest[:end]
	}
	return decoded
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// freePort asks the OS for a port and releases it, so the manager can bind it.
// Hard-coding 8723 would make these tests fight a running dev server.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("finding a free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

func newCreds(t *testing.T, env *accounts.EnvConfig) *credentials.Manager {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "reauth.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	kr, err := keyring.Open(db, "test passphrase")
	if err != nil {
		t.Fatalf("keyring.Open: %v", err)
	}
	m := credentials.New(db, kr)
	if err := m.Import(env); err != nil {
		t.Fatalf("Import: %v", err)
	}
	return m
}

func fourSharedAccount() *accounts.EnvConfig {
	return &accounts.EnvConfig{Accounts: []accounts.Account{{
		Provider:         accounts.ProviderFourShared,
		Email:            "4s1@example.com",
		Index:            1,
		ConsumerKey:      "ck",
		ConsumerSecret:   "cs",
		ConsumerDomain:   "127.0.0.1",
		OAuthToken:       "stale-token",
		OAuthTokenSecret: "stale-secret",
	}}}
}

func waitForPhase(t *testing.T, m *Manager, want Phase) Status {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s := m.Status(); s.Phase == want {
			return s
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("phase never reached %q (last: %+v)", want, m.Status())
	return Status{}
}

// The end-to-end path: authorize, exchange, store, and take effect without a
// restart. The stored token must be the new one and the flag must be gone.
func TestSuccessfulReauthorizationStoresTheNewToken(t *testing.T) {
	creds := newCreds(t, fourSharedAccount())
	if err := creds.FlagReauth("fourshared", "4s1@example.com", "token expired", false); err != nil {
		t.Fatalf("FlagReauth: %v", err)
	}

	fake := newFakeProvider(t)
	port := freePort(t)
	m := New(creds, Config{
		CallbackPort: port,
		Timeout:      5 * time.Second,
		OpenBrowser:  func(string) {}, // never launch a browser from a test
		Endpoints:    fake.endpoints(),
	})

	status, err := m.Start("fourshared", "4s1@example.com", false)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// The link must be handed back immediately — it is the user's only reliable
	// way in when the browser does not open.
	if status.AuthorizeURL == "" {
		t.Fatal("Start returned no authorize URL")
	}
	waitForPhase(t, m, PhaseAwaiting)

	// Play the browser: the provider redirects to the callback.
	if fake.callback == "" {
		t.Fatal("provider never received a callback URL")
	}
	resp, err := http.Get(fake.callback)
	if err != nil {
		t.Fatalf("hitting the callback: %v", err)
	}
	resp.Body.Close()

	waitForPhase(t, m, PhaseDone)

	got, ok := creds.Store().Find("fourshared", "4s1@example.com")
	if !ok {
		t.Fatal("account vanished")
	}
	if got.OAuthToken != "fresh-token" || got.OAuthTokenSecret != "fresh-secret" {
		t.Fatalf("token = %q/%q, want the re-authorized pair", got.OAuthToken, got.OAuthTokenSecret)
	}
	if got.NeedsReauth {
		t.Error("the needs-re-authorization flag survived a successful run")
	}

	// The port must be free again, or a second re-authorization could never run.
	assertPortFree(t, port)
}

// A finished run releases the listener; so must a cancelled one.
func TestCancelReleasesTheCallbackPort(t *testing.T) {
	creds := newCreds(t, fourSharedAccount())
	fake := newFakeProvider(t)
	port := freePort(t)
	m := New(creds, Config{CallbackPort: port, Timeout: 5 * time.Second,
		OpenBrowser: func(string) {}, Endpoints: fake.endpoints()})

	if _, err := m.Start("fourshared", "4s1@example.com", false); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForPhase(t, m, PhaseAwaiting)

	m.Cancel()
	waitForPhase(t, m, PhaseCancelled)
	assertPortFree(t, port)

	// And the stale token is untouched: a cancelled run stores nothing.
	got, _ := creds.Store().Find("fourshared", "4s1@example.com")
	if got.OAuthToken != "stale-token" {
		t.Errorf("a cancelled run changed the stored token: %q", got.OAuthToken)
	}
}

// Waiting forever would hold the port and leave the UI spinning.
func TestRunTimesOutRatherThanWaitingForever(t *testing.T) {
	creds := newCreds(t, fourSharedAccount())
	fake := newFakeProvider(t)
	port := freePort(t)
	m := New(creds, Config{CallbackPort: port, Timeout: 150 * time.Millisecond,
		OpenBrowser: func(string) {}, Endpoints: fake.endpoints()})

	if _, err := m.Start("fourshared", "4s1@example.com", false); err != nil {
		t.Fatalf("Start: %v", err)
	}
	s := waitForPhase(t, m, PhaseError)
	if s.Error == "" {
		t.Error("a timed-out run should explain itself")
	}
	assertPortFree(t, port)
}

// The callback port is fixed by the provider's registered application, so two
// runs cannot both have it. Refuse the second rather than fail obscurely later.
func TestSecondRunIsRefusedWhileOneIsInFlight(t *testing.T) {
	creds := newCreds(t, fourSharedAccount())
	fake := newFakeProvider(t)
	m := New(creds, Config{CallbackPort: freePort(t), Timeout: 5 * time.Second,
		OpenBrowser: func(string) {}, Endpoints: fake.endpoints()})

	if _, err := m.Start("fourshared", "4s1@example.com", false); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForPhase(t, m, PhaseAwaiting)

	if _, err := m.Start("fourshared", "4s1@example.com", false); err == nil {
		t.Fatal("a concurrent re-authorization was allowed")
	}
	m.Cancel()
}

func TestStartRefusesWhatItCannotDo(t *testing.T) {
	fake := newFakeProvider(t)
	cfg := func(creds *credentials.Manager) *Manager {
		return New(creds, Config{CallbackPort: freePort(t), Timeout: time.Second,
			OpenBrowser: func(string) {}, Endpoints: fake.endpoints()})
	}

	t.Run("locked credentials", func(t *testing.T) {
		db, err := database.Open(filepath.Join(t.TempDir(), "locked.db"))
		if err != nil {
			t.Fatalf("database.Open: %v", err)
		}
		t.Cleanup(func() { db.Close() })
		m := cfg(credentials.NewLocked(db, "no passphrase"))
		if _, err := m.Start("fourshared", "a@example.com", false); !errors.Is(err, accounts.ErrLocked) {
			t.Fatalf("err = %v, want ErrLocked", err)
		}
	})

	// MEGA is password login. Starting a flow that cannot end would be worse than
	// saying so.
	t.Run("password provider", func(t *testing.T) {
		m := cfg(newCreds(t, &accounts.EnvConfig{Accounts: []accounts.Account{{
			Provider: accounts.ProviderMega, Email: "m@example.com", Password: "p", Index: 1,
		}}}))
		if _, err := m.Start("mega", "m@example.com", false); err == nil {
			t.Fatal("expected MEGA to be refused")
		}
	})

	t.Run("unknown account", func(t *testing.T) {
		m := cfg(newCreds(t, fourSharedAccount()))
		if _, err := m.Start("fourshared", "nobody@example.com", false); err == nil {
			t.Fatal("expected an unknown account to be refused")
		}
	})

	// Without the registered application's credentials there is nothing to sign
	// with; the message has to name the keys to set.
	t.Run("no consumer credentials", func(t *testing.T) {
		env := fourSharedAccount()
		env.Accounts[0].ConsumerKey = ""
		env.Accounts[0].ConsumerSecret = ""
		m := cfg(newCreds(t, env))
		_, err := m.Start("fourshared", "4s1@example.com", false)
		if err == nil || !contains(err.Error(), "CONSUMER_KEY") {
			t.Fatalf("err = %v, want one naming CONSUMER_KEY", err)
		}
	})

	// 4shared rejects localhost as an application domain, so there is no usable
	// default to fall back to.
	t.Run("no callback domain", func(t *testing.T) {
		env := fourSharedAccount()
		env.Accounts[0].ConsumerDomain = ""
		m := cfg(newCreds(t, env))
		_, err := m.Start("fourshared", "4s1@example.com", false)
		if err == nil || !contains(err.Error(), "CONSUMER_DOMAIN") {
			t.Fatalf("err = %v, want one naming CONSUMER_DOMAIN", err)
		}
	})
}

func contains(s, sub string) bool { return indexOf(s, sub) >= 0 }

func assertPortFree(t *testing.T, port int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			ln.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("port %d was never released", port)
}

// Only OAuth providers can be re-authorized. This is not just about the button:
// MEGA answers a wrong password with the same ErrAuthExpired a rejected 4shared
// token produces, so without this check a mistyped MEGA password would flag the
// account as "needs re-authorization" — a state it could never leave, since
// there is no token to replace.
func TestSupportedOnlyCoversOAuthProviders(t *testing.T) {
	if !Supported("fourshared") {
		t.Error("4shared must be re-authorizable")
	}
	for _, p := range []string{"mega", "", "dropbox"} {
		if Supported(p) {
			t.Errorf("Supported(%q) = true", p)
		}
	}
}
