package accounts

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

type ProviderType string

const (
	ProviderMega       ProviderType = "mega"
	ProviderFourShared ProviderType = "fourshared"
)

// providerEnv pairs a provider with its .env key prefix. It is the single list
// every loader walks — numbered accounts and main accounts alike — so adding a
// provider means adding one entry here rather than editing several loops.
var providerEnv = []struct {
	Provider ProviderType
	Prefix   string
}{
	{ProviderMega, "MEGA_ACCOUNT"},
	{ProviderFourShared, "FOURSHARED_ACCOUNT"},
}

// legacyMainKeys are the pre-phase-3 single-main-account keys. They are no
// longer read; their presence only earns a warning pointing at the replacements,
// because silently ignoring them would stop a user's metadata DB backups with no
// visible cause.
var legacyMainKeys = []string{"MAIN_ACCOUNT_PROVIDER", "MAIN_ACCOUNT_EMAIL", "MAIN_ACCOUNT_PASSWORD"}

type Account struct {
	Provider ProviderType
	Email    string
	Password string
	QuotaGB  float64
	Index    int

	// OAuth credentials. Only used by providers that authenticate with OAuth
	// (e.g. 4shared); empty for password providers like MEGA. ConsumerKey/Secret
	// are the app-level credentials for THIS account's registered application
	// (each 4shared account is authorized through its own app), and
	// OAuthToken/Secret are the per-account access token from the authorize step.
	ConsumerKey      string
	ConsumerSecret   string
	OAuthToken       string
	OAuthTokenSecret string
}

// MainAccount is a provider's database-backup destination: it receives a copy of
// the metadata SQLite file after every successful job. There is at most one per
// provider (MEGA_ACCOUNT_MAIN_*, FOURSHARED_ACCOUNT_MAIN_*), and configuring
// none is a supported choice.
//
// It carries the same credential set as a numbered Account because an OAuth
// provider's main account needs its own consumer key/secret and access token to
// stand on its own — before this it had to borrow them from a numbered account
// that happened to share its email address.
//
// Main accounts are deliberately NOT synced to the accounts database table:
// that table feeds the upload-target modal and the per-user backups table, so a
// row there would offer the db-backup account as an upload target and invent a
// user row for it.
type MainAccount struct {
	Provider ProviderType `json:"provider"`
	Email    string       `json:"email"`
	Password string       `json:"-"`

	ConsumerKey      string `json:"-"`
	ConsumerSecret   string `json:"-"`
	OAuthToken       string `json:"-"`
	OAuthTokenSecret string `json:"-"`
}

// MissingKeys returns the full .env key names this main account needs but does
// not have, so a warning or a UI message can be pasted straight into .env. Empty
// means it is usable. A main account that is not configured at all never reaches
// here — absence is silent, only a half-configured one is worth reporting.
func (m MainAccount) MissingKeys() []string {
	var suffixes []string
	switch m.Provider {
	case ProviderFourShared:
		// 4shared authenticates with OAuth; the password is display-only, so its
		// absence is not a fault.
		if m.ConsumerKey == "" {
			suffixes = append(suffixes, "_CONSUMER_KEY")
		}
		if m.ConsumerSecret == "" {
			suffixes = append(suffixes, "_CONSUMER_SECRET")
		}
		if m.OAuthToken == "" {
			suffixes = append(suffixes, "_OAUTH_TOKEN")
		}
		if m.OAuthTokenSecret == "" {
			suffixes = append(suffixes, "_OAUTH_TOKEN_SECRET")
		}
	default:
		if m.Password == "" {
			suffixes = append(suffixes, "_PASSWORD")
		}
	}

	missing := make([]string, 0, len(suffixes))
	for _, s := range suffixes {
		missing = append(missing, mainKeyPrefix(m.Provider)+s)
	}
	return missing
}

// mainKeyPrefix is the .env prefix a provider's main account keys share, e.g.
// "MEGA_ACCOUNT_MAIN". A provider not in providerEnv cannot be configured from
// .env at all, but falls back to the same shape so a reported key name is never
// a bare suffix.
func mainKeyPrefix(provider ProviderType) string {
	for _, pe := range providerEnv {
		if pe.Provider == provider {
			return pe.Prefix + "_" + mainSuffix
		}
	}
	return strings.ToUpper(string(provider)) + "_ACCOUNT_" + mainSuffix
}

// Usable reports whether this main account has everything it needs to log in.
func (m MainAccount) Usable() bool { return len(m.MissingKeys()) == 0 }

// OAuthApp holds app-level OAuth consumer credentials used as a fallback when an
// account does not declare its own. Obtained by registering an application with
// the provider; see the README "Provider credentials" section.
type OAuthApp struct {
	ConsumerKey    string
	ConsumerSecret string
}

type AccountStore struct {
	// Mains holds the configured database-backup accounts, at most one per
	// provider, in providerEnv order. Providers with no main account configured
	// are simply absent.
	Mains    []MainAccount
	Accounts []Account

	// FourShared is an optional fallback 4shared consumer key/secret applied to
	// any 4shared account that doesn't set its own FOURSHARED_ACCOUNT_<n>_CONSUMER_*.
	// Lets a single shared app cover all accounts when per-account apps aren't used.
	FourShared OAuthApp
}

// MainFor returns the main account configured for a provider, if any.
func (s *AccountStore) MainFor(provider ProviderType) (MainAccount, bool) {
	if s == nil {
		return MainAccount{}, false
	}
	for _, m := range s.Mains {
		if m.Provider == provider {
			return m, true
		}
	}
	return MainAccount{}, false
}

func Load(envPath string) (*AccountStore, error) {
	if err := godotenv.Load(envPath); err != nil {
		return nil, fmt.Errorf("loading .env file: %w", err)
	}

	store := &AccountStore{}

	warnLegacyMainKeys()

	// Optional shared fallback for 4shared accounts that don't set their own.
	store.FourShared = OAuthApp{
		ConsumerKey:    os.Getenv("FOURSHARED_CONSUMER_KEY"),
		ConsumerSecret: os.Getenv("FOURSHARED_CONSUMER_SECRET"),
	}

	for _, pe := range providerEnv {
		if main, ok := loadMainAccount(pe.Provider, pe.Prefix); ok {
			store.Mains = append(store.Mains, main)
		}
		store.Accounts = append(store.Accounts, loadProviderAccounts(pe.Provider, pe.Prefix)...)
	}

	// Apply the shared 4shared consumer fallback where an account omitted its own.
	// Main accounts get the same treatment as numbered ones: a single registered
	// app can cover every 4shared account, main included.
	for i := range store.Accounts {
		a := &store.Accounts[i]
		if a.Provider == ProviderFourShared {
			a.ConsumerKey, a.ConsumerSecret = withFallback(a.ConsumerKey, a.ConsumerSecret, store.FourShared)
		}
	}
	for i := range store.Mains {
		m := &store.Mains[i]
		if m.Provider == ProviderFourShared {
			m.ConsumerKey, m.ConsumerSecret = withFallback(m.ConsumerKey, m.ConsumerSecret, store.FourShared)
		}
	}

	// A main account that is configured but incomplete is a misconfiguration the
	// user wants to hear about; one that is absent is a supported choice and stays
	// silent. Checked after the consumer fallback so a shared app doesn't get
	// reported as missing.
	for _, m := range store.Mains {
		if missing := m.MissingKeys(); len(missing) > 0 {
			slog.Warn("main account is incompletely configured; it cannot receive the metadata database backup",
				"provider", m.Provider, "email", m.Email,
				"missing", strings.Join(missing, ", "))
		}
	}

	slog.Info("accounts loaded", "total", len(store.Accounts), "main_accounts", len(store.Mains))
	return store, nil
}

// withFallback fills empty per-account consumer credentials from the shared app.
func withFallback(key, secret string, app OAuthApp) (string, string) {
	if key == "" {
		key = app.ConsumerKey
	}
	if secret == "" {
		secret = app.ConsumerSecret
	}
	return key, secret
}

// warnLegacyMainKeys reports any leftover MAIN_ACCOUNT_* keys and names their
// replacements. They are not adopted: a value silently promoted from a key we
// claim to have removed is worse than a clear break.
func warnLegacyMainKeys() {
	var found []string
	for _, k := range legacyMainKeys {
		if os.Getenv(k) != "" {
			found = append(found, k)
		}
	}
	if len(found) == 0 {
		return
	}
	slog.Warn("MAIN_ACCOUNT_* keys are no longer used and were ignored; the metadata database backup account is now configured per provider",
		"found", strings.Join(found, ", "),
		"use", "MEGA_ACCOUNT_MAIN_EMAIL/_PASSWORD and/or FOURSHARED_ACCOUNT_MAIN_EMAIL/_CONSUMER_KEY/_CONSUMER_SECRET/_OAUTH_TOKEN/_OAUTH_TOKEN_SECRET")
}

// mainSuffix is the slot a main account occupies where a numbered account has
// its index: MEGA_ACCOUNT_MAIN_EMAIL alongside MEGA_ACCOUNT_1_EMAIL.
const mainSuffix = "MAIN"

// loadMainAccount reads <prefix>_MAIN_* . An unset email means this provider has
// no main account, which is a supported configuration, not an error.
func loadMainAccount(provider ProviderType, prefix string) (MainAccount, bool) {
	key := func(suffix string) string { return fmt.Sprintf("%s_%s%s", prefix, mainSuffix, suffix) }

	email := os.Getenv(key("_EMAIL"))
	if email == "" {
		return MainAccount{}, false
	}
	return MainAccount{
		Provider:         provider,
		Email:            email,
		Password:         os.Getenv(key("_PASSWORD")),
		ConsumerKey:      os.Getenv(key("_CONSUMER_KEY")),
		ConsumerSecret:   os.Getenv(key("_CONSUMER_SECRET")),
		OAuthToken:       os.Getenv(key("_OAUTH_TOKEN")),
		OAuthTokenSecret: os.Getenv(key("_OAUTH_TOKEN_SECRET")),
	}, true
}

func loadProviderAccounts(provider ProviderType, prefix string) []Account {
	var accounts []Account
	for i := 1; ; i++ {
		email := os.Getenv(fmt.Sprintf("%s_%d_EMAIL", prefix, i))
		if email == "" {
			break
		}
		password := os.Getenv(fmt.Sprintf("%s_%d_PASSWORD", prefix, i))
		quotaStr := os.Getenv(fmt.Sprintf("%s_%d_QUOTA_GB", prefix, i))

		quota := 0.0
		if quotaStr != "" {
			q, err := strconv.ParseFloat(quotaStr, 64)
			if err != nil {
				slog.Warn("invalid quota value", "account", email, "value", quotaStr)
			} else {
				quota = q
			}
		}

		accounts = append(accounts, Account{
			Provider:         provider,
			Email:            email,
			Password:         password,
			QuotaGB:          quota,
			Index:            i,
			ConsumerKey:      os.Getenv(fmt.Sprintf("%s_%d_CONSUMER_KEY", prefix, i)),
			ConsumerSecret:   os.Getenv(fmt.Sprintf("%s_%d_CONSUMER_SECRET", prefix, i)),
			OAuthToken:       os.Getenv(fmt.Sprintf("%s_%d_OAUTH_TOKEN", prefix, i)),
			OAuthTokenSecret: os.Getenv(fmt.Sprintf("%s_%d_OAUTH_TOKEN_SECRET", prefix, i)),
		})
	}
	return accounts
}

func (s *AccountStore) GetByProvider(provider ProviderType) []Account {
	var result []Account
	for _, a := range s.Accounts {
		if a.Provider == provider {
			result = append(result, a)
		}
	}
	return result
}
