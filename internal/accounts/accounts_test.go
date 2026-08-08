package accounts

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// resetAccountEnv removes every account-related variable from the process
// environment before a test writes its own .env, and again afterwards. godotenv
// never overwrites a variable that is already set, so a value left behind by an
// earlier test (Load exports everything it reads) would silently win over the
// one under test and make these cases pass or fail for the wrong reason.
func resetAccountEnv(t *testing.T) {
	t.Helper()
	prefixes := []string{"MEGA_ACCOUNT", "FOURSHARED_ACCOUNT", "FOURSHARED_CONSUMER", "MAIN_ACCOUNT"}
	clear := func() {
		for _, kv := range os.Environ() {
			k, _, _ := strings.Cut(kv, "=")
			for _, p := range prefixes {
				if strings.HasPrefix(k, p) {
					os.Unsetenv(k)
				}
			}
		}
	}
	clear()
	t.Cleanup(clear)
}

// loadEnv writes body to a temp .env and loads it.
func loadEnv(t *testing.T, body string) *AccountStore {
	t.Helper()
	store, _ := loadEnvWithLog(t, body)
	return store
}

// loadEnvWithLog is loadEnv plus everything Load logged, so the tests can hold
// Load to the warnings it promises: several acceptance criteria are *about* the
// message (it must name the missing keys, it must name the replacement keys),
// which reading the store cannot check.
func loadEnvWithLog(t *testing.T, body string) (*AccountStore, string) {
	t.Helper()
	resetAccountEnv(t)
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing .env: %v", err)
	}

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	store, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return store, buf.String()
}

func TestLoadMainAccountPerProvider(t *testing.T) {
	store := loadEnv(t, `
MEGA_ACCOUNT_MAIN_EMAIL=mega-main@example.com
MEGA_ACCOUNT_MAIN_PASSWORD=megapass

FOURSHARED_ACCOUNT_MAIN_EMAIL=4s-main@example.com
FOURSHARED_ACCOUNT_MAIN_CONSUMER_KEY=ckey
FOURSHARED_ACCOUNT_MAIN_CONSUMER_SECRET=csecret
FOURSHARED_ACCOUNT_MAIN_OAUTH_TOKEN=otoken
FOURSHARED_ACCOUNT_MAIN_OAUTH_TOKEN_SECRET=osecret
`)

	if len(store.Mains) != 2 {
		t.Fatalf("expected 2 main accounts, got %d: %+v", len(store.Mains), store.Mains)
	}

	mega, ok := store.MainFor(ProviderMega)
	if !ok {
		t.Fatal("no MEGA main account")
	}
	if mega.Email != "mega-main@example.com" || mega.Password != "megapass" {
		t.Errorf("MEGA main = %+v", mega)
	}
	if !mega.Usable() {
		t.Errorf("MEGA main should be usable, missing %v", mega.MissingKeys())
	}

	four, ok := store.MainFor(ProviderFourShared)
	if !ok {
		t.Fatal("no 4shared main account")
	}
	if four.ConsumerKey != "ckey" || four.ConsumerSecret != "csecret" ||
		four.OAuthToken != "otoken" || four.OAuthTokenSecret != "osecret" {
		t.Errorf("4shared main OAuth creds = %+v", four)
	}
	if !four.Usable() {
		t.Errorf("4shared main should be usable, missing %v", four.MissingKeys())
	}
}

// A provider with no main account is skipped, not defaulted to another
// provider's and not an error.
func TestLoadOneMainAccountOnly(t *testing.T) {
	store := loadEnv(t, `
MEGA_ACCOUNT_MAIN_EMAIL=mega-main@example.com
MEGA_ACCOUNT_MAIN_PASSWORD=megapass
`)

	if len(store.Mains) != 1 {
		t.Fatalf("expected 1 main account, got %+v", store.Mains)
	}
	if _, ok := store.MainFor(ProviderFourShared); ok {
		t.Error("4shared reported a main account with none configured")
	}
}

// The regression this phase exists to prevent: a missing main account used to
// fail Load outright, and main.go treats a Load failure as "no accounts at all",
// so one absent optional setting silently dropped every upload target.
func TestLoadWithoutAnyMainAccountKeepsNumberedAccounts(t *testing.T) {
	store := loadEnv(t, `
MEGA_ACCOUNT_1_EMAIL=mega1@example.com
MEGA_ACCOUNT_1_PASSWORD=pass1
MEGA_ACCOUNT_1_QUOTA_GB=20
`)

	if len(store.Mains) != 0 {
		t.Errorf("expected no main accounts, got %+v", store.Mains)
	}
	if len(store.Accounts) != 1 || store.Accounts[0].Email != "mega1@example.com" {
		t.Fatalf("numbered accounts = %+v", store.Accounts)
	}
	if store.Accounts[0].QuotaGB != 20 {
		t.Errorf("quota = %v, want 20", store.Accounts[0].QuotaGB)
	}
}

func TestLoadIncompleteMainAccountsReportMissingKeys(t *testing.T) {
	store := loadEnv(t, `
MEGA_ACCOUNT_MAIN_EMAIL=mega-main@example.com

FOURSHARED_ACCOUNT_MAIN_EMAIL=4s-main@example.com
FOURSHARED_ACCOUNT_MAIN_CONSUMER_KEY=ckey
FOURSHARED_ACCOUNT_MAIN_CONSUMER_SECRET=csecret
`)

	mega, _ := store.MainFor(ProviderMega)
	if mega.Usable() {
		t.Error("MEGA main with no password should not be usable")
	}
	if got := mega.MissingKeys(); len(got) != 1 || got[0] != "MEGA_ACCOUNT_MAIN_PASSWORD" {
		t.Errorf("MEGA missing keys = %v", got)
	}

	four, _ := store.MainFor(ProviderFourShared)
	if four.Usable() {
		t.Error("4shared main with no token should not be usable")
	}
	got := strings.Join(four.MissingKeys(), ",")
	want := "FOURSHARED_ACCOUNT_MAIN_OAUTH_TOKEN,FOURSHARED_ACCOUNT_MAIN_OAUTH_TOKEN_SECRET"
	if got != want {
		t.Errorf("4shared missing keys = %q, want %q", got, want)
	}
}

// A 4shared main account has no password of its own to require: it authenticates
// with OAuth, and the .env password is display-only.
func TestFourSharedMainDoesNotRequireAPassword(t *testing.T) {
	store := loadEnv(t, `
FOURSHARED_ACCOUNT_MAIN_EMAIL=4s-main@example.com
FOURSHARED_ACCOUNT_MAIN_CONSUMER_KEY=ckey
FOURSHARED_ACCOUNT_MAIN_CONSUMER_SECRET=csecret
FOURSHARED_ACCOUNT_MAIN_OAUTH_TOKEN=otoken
FOURSHARED_ACCOUNT_MAIN_OAUTH_TOKEN_SECRET=osecret
`)
	four, ok := store.MainFor(ProviderFourShared)
	if !ok || !four.Usable() {
		t.Fatalf("4shared main should be usable without a password: %+v missing %v", four, four.MissingKeys())
	}
}

func TestLegacyMainAccountKeysAreIgnored(t *testing.T) {
	store := loadEnv(t, `
MAIN_ACCOUNT_PROVIDER=mega
MAIN_ACCOUNT_EMAIL=legacy@example.com
MAIN_ACCOUNT_PASSWORD=legacypass

MEGA_ACCOUNT_1_EMAIL=mega1@example.com
MEGA_ACCOUNT_1_PASSWORD=pass1
`)

	if len(store.Mains) != 0 {
		t.Fatalf("legacy MAIN_ACCOUNT_* keys must not be adopted, got %+v", store.Mains)
	}
	if len(store.Accounts) != 1 {
		t.Errorf("numbered accounts should still load, got %+v", store.Accounts)
	}
}

// The shared consumer app covers the main account exactly as it covers numbered
// ones, so a single registered application can serve every 4shared account.
func TestSharedConsumerFallbackAppliesToMainAccount(t *testing.T) {
	store := loadEnv(t, `
FOURSHARED_CONSUMER_KEY=shared-key
FOURSHARED_CONSUMER_SECRET=shared-secret

FOURSHARED_ACCOUNT_MAIN_EMAIL=4s-main@example.com
FOURSHARED_ACCOUNT_MAIN_OAUTH_TOKEN=otoken
FOURSHARED_ACCOUNT_MAIN_OAUTH_TOKEN_SECRET=osecret

FOURSHARED_ACCOUNT_1_EMAIL=4s1@example.com
FOURSHARED_ACCOUNT_1_OAUTH_TOKEN=otoken1
FOURSHARED_ACCOUNT_1_OAUTH_TOKEN_SECRET=osecret1
`)

	four, ok := store.MainFor(ProviderFourShared)
	if !ok {
		t.Fatal("no 4shared main account")
	}
	if four.ConsumerKey != "shared-key" || four.ConsumerSecret != "shared-secret" {
		t.Errorf("main did not inherit the shared consumer app: %+v", four)
	}
	if !four.Usable() {
		t.Errorf("main should be usable via the shared app, missing %v", four.MissingKeys())
	}
	if store.Accounts[0].ConsumerKey != "shared-key" {
		t.Errorf("numbered account did not inherit the shared consumer app: %+v", store.Accounts[0])
	}
}

// A per-account consumer key beats the shared fallback, for the main account
// just as for a numbered one.
func TestMainAccountConsumerKeyOverridesSharedFallback(t *testing.T) {
	store := loadEnv(t, `
FOURSHARED_CONSUMER_KEY=shared-key
FOURSHARED_CONSUMER_SECRET=shared-secret

FOURSHARED_ACCOUNT_MAIN_EMAIL=4s-main@example.com
FOURSHARED_ACCOUNT_MAIN_CONSUMER_KEY=own-key
FOURSHARED_ACCOUNT_MAIN_CONSUMER_SECRET=own-secret
FOURSHARED_ACCOUNT_MAIN_OAUTH_TOKEN=otoken
FOURSHARED_ACCOUNT_MAIN_OAUTH_TOKEN_SECRET=osecret
`)

	four, _ := store.MainFor(ProviderFourShared)
	if four.ConsumerKey != "own-key" || four.ConsumerSecret != "own-secret" {
		t.Errorf("shared fallback overrode the account's own app: %+v", four)
	}
}

// The warning must name the keys to fill in — an operator should not have to
// consult the README to act on it.
func TestIncompleteMainAccountWarningNamesTheMissingKeys(t *testing.T) {
	_, logged := loadEnvWithLog(t, `
MEGA_ACCOUNT_MAIN_EMAIL=mega-main@example.com
`)
	if !strings.Contains(logged, "MEGA_ACCOUNT_MAIN_PASSWORD") {
		t.Errorf("warning did not name the missing key:\n%s", logged)
	}
	if !strings.Contains(logged, "level=WARN") {
		t.Errorf("an incomplete main account should warn, got:\n%s", logged)
	}
}

// The opposite case: a provider with no main account is a supported choice and
// must produce no warning at all, or the log stops meaning anything.
func TestAbsentMainAccountIsSilent(t *testing.T) {
	_, logged := loadEnvWithLog(t, `
MEGA_ACCOUNT_1_EMAIL=mega1@example.com
MEGA_ACCOUNT_1_PASSWORD=pass1
`)
	if strings.Contains(logged, "level=WARN") {
		t.Errorf("configuring no main account must not warn, got:\n%s", logged)
	}
}

// A user upgrading from the old single-main-account .env must be told exactly
// what to rename, or their database backups stop with no visible cause.
func TestLegacyKeyWarningNamesTheReplacements(t *testing.T) {
	_, logged := loadEnvWithLog(t, `
MAIN_ACCOUNT_PROVIDER=mega
MAIN_ACCOUNT_EMAIL=legacy@example.com
MAIN_ACCOUNT_PASSWORD=legacypass
`)
	for _, want := range []string{"level=WARN", "MAIN_ACCOUNT_PROVIDER", "MEGA_ACCOUNT_MAIN_EMAIL", "FOURSHARED_ACCOUNT_MAIN_EMAIL"} {
		if !strings.Contains(logged, want) {
			t.Errorf("legacy-key warning is missing %q:\n%s", want, logged)
		}
	}
}

// A main account is never an upload target, so it must not appear among the
// numbered accounts that feed the modal and the accounts table.
func TestMainAccountIsNotAnUploadTarget(t *testing.T) {
	store := loadEnv(t, `
MEGA_ACCOUNT_MAIN_EMAIL=mega-main@example.com
MEGA_ACCOUNT_MAIN_PASSWORD=megapass
`)

	if len(store.Accounts) != 0 {
		t.Fatalf("main account leaked into the numbered accounts: %+v", store.Accounts)
	}
	if len(store.GetByProvider(ProviderMega)) != 0 {
		t.Error("GetByProvider returned the main account")
	}
}
