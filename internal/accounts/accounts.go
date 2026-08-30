// Package accounts models the cloud accounts this application uploads to, and
// holds the credential set the rest of the app resolves against.
//
// Since PR #13 the .env file is no longer the running source of truth: LoadEnv
// parses it, the credentials package reconciles it into the database, and the
// AccountStore is populated from there. .env remains how an account is added or
// a password changed, but a credential the application itself obtains (a
// re-authorized 4shared token) lives only in the database and must survive a
// restart — which it cannot do if .env is replayed over it on every boot.
package accounts

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/joho/godotenv"
)

// ErrLocked means the stored credentials could not be opened — the passphrase is
// missing or wrong. Every path that needs a credential wraps it, so the API can
// answer with one consistent status and the UI can explain the real cause rather
// than reporting each account as unconfigured.
var ErrLocked = errors.New("credentials are locked")

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

// Providers returns the providers configurable from .env, in a stable order.
func Providers() []ProviderType {
	out := make([]ProviderType, 0, len(providerEnv))
	for _, pe := range providerEnv {
		out = append(out, pe.Provider)
	}
	return out
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

	// ConsumerDomain is the callback domain registered with the provider's
	// application. Stored because the in-app re-authorization flow needs it to
	// build a callback URL the provider will accept; 4shared rejects localhost.
	ConsumerDomain string

	// NeedsReauth is set when the provider rejected this account's token as
	// expired. It is stored in the database, so it survives a restart and can be
	// shown in the Accounts view rather than only in a failed job's logs.
	NeedsReauth  bool
	ReauthReason string

	// Tier is the account's plan with its provider ("free" or "paid"), selecting
	// which per-file size cap and transfer budget apply. .env seeds it via
	// <PREFIX>_<n>_TIER; the Accounts view can change it without a restart.
	//
	// TierSource records who last set it. "app" means the user chose it in the
	// UI, and the .env reconciliation must then leave it alone — the same rule
	// that protects an app-written OAuth token, and for the same reason: a value
	// the app owns cannot survive a boot that replays .env over it.
	Tier       string
	TierSource string
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
// Main accounts are deliberately NOT rows in the accounts database table: that
// table feeds the upload-target modal and the per-user backups table, so a row
// there would offer the db-backup account as an upload target and invent a user
// row for it. They have their own table instead.
type MainAccount struct {
	Provider ProviderType `json:"provider"`
	Email    string       `json:"email"`
	Password string       `json:"-"`

	ConsumerKey      string `json:"-"`
	ConsumerSecret   string `json:"-"`
	OAuthToken       string `json:"-"`
	OAuthTokenSecret string `json:"-"`
	ConsumerDomain   string `json:"-"`

	NeedsReauth  bool   `json:"needs_reauth"`
	ReauthReason string `json:"reauth_reason,omitempty"`
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
	Domain         string
}

// EnvConfig is what .env declares. It is an input to reconciliation, not the
// running credential set: the database is authoritative once the first import
// has happened.
type EnvConfig struct {
	Mains      []MainAccount
	Accounts   []Account
	FourShared OAuthApp
}

// MainFor returns the main account .env declares for a provider, if any.
func (c *EnvConfig) MainFor(provider ProviderType) (MainAccount, bool) {
	if c == nil {
		return MainAccount{}, false
	}
	for _, m := range c.Mains {
		if m.Provider == provider {
			return m, true
		}
	}
	return MainAccount{}, false
}

// AccountStore is the running credential set, shared by the worker, the HTTP
// handlers, the quota poller and Auto-Sync.
//
// It is read from several goroutines and rewritten at runtime — the
// re-authorization flow replaces an account's token without a restart — so its
// contents are guarded rather than exposed as fields. Readers get copies; there
// is no way to hold a reference into the live slice and observe a torn update.
type AccountStore struct {
	mu         sync.RWMutex
	mains      []MainAccount
	accounts   []Account
	fourShared OAuthApp

	// lockReason is non-empty when credentials could not be unsealed (no
	// passphrase, or the wrong one). The store is then empty and every credential
	// path refuses with this reason rather than reporting "account not
	// configured", which would send the user looking in the wrong place.
	lockReason string
}

// NewStore returns a store holding the given credential set.
func NewStore(mains []MainAccount, accts []Account, app OAuthApp) *AccountStore {
	s := &AccountStore{}
	s.Replace(mains, accts, app)
	return s
}

// Replace swaps in a new credential set atomically.
func (s *AccountStore) Replace(mains []MainAccount, accts []Account, app OAuthApp) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mains = append([]MainAccount(nil), mains...)
	s.accounts = append([]Account(nil), accts...)
	s.fourShared = app
	s.lockReason = ""
}

// Lock marks the store unusable and empties it. Called when the credential
// passphrase is missing or wrong: the credentials on disk are intact but cannot
// be opened, and nothing may act on them until that is fixed.
func (s *AccountStore) Lock(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mains = nil
	s.accounts = nil
	s.lockReason = reason
}

// LockReason returns why credentials are unavailable, or "" when the store is
// usable.
func (s *AccountStore) LockReason() string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lockReason
}

// IsLocked reports whether credentials are unavailable.
func (s *AccountStore) IsLocked() bool { return s.LockReason() != "" }

// All returns every numbered (upload-target) account.
func (s *AccountStore) All() []Account {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]Account(nil), s.accounts...)
}

// Mains returns the configured database-backup accounts, at most one per
// provider, in providerEnv order.
func (s *AccountStore) Mains() []MainAccount {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]MainAccount(nil), s.mains...)
}

// MainFor returns the main account configured for a provider, if any.
func (s *AccountStore) MainFor(provider ProviderType) (MainAccount, bool) {
	if s == nil {
		return MainAccount{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, m := range s.mains {
		if m.Provider == provider {
			return m, true
		}
	}
	return MainAccount{}, false
}

// Find returns the numbered account for a (provider, email) pair.
func (s *AccountStore) Find(provider, email string) (Account, bool) {
	if s == nil {
		return Account{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, a := range s.accounts {
		if string(a.Provider) == provider && a.Email == email {
			return a, true
		}
	}
	return Account{}, false
}

// GetByProvider returns every numbered account belonging to one provider.
func (s *AccountStore) GetByProvider(provider ProviderType) []Account {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []Account
	for _, a := range s.accounts {
		if a.Provider == provider {
			result = append(result, a)
		}
	}
	return result
}

// FourSharedApp returns the optional shared 4shared consumer credentials.
func (s *AccountStore) FourSharedApp() OAuthApp {
	if s == nil {
		return OAuthApp{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.fourShared
}

// LoadEnv reads .env and returns what it declares. It does not decide anything:
// the credentials package reconciles this against the database, which is where
// the running credential set comes from.
//
// Loading also populates the process environment (godotenv), so callers can read
// other .env entries — notably the credential passphrase — afterwards.
func LoadEnv(envPath string) (*EnvConfig, error) {
	if err := godotenv.Load(envPath); err != nil {
		return nil, fmt.Errorf("loading .env file: %w", err)
	}

	cfg := &EnvConfig{}

	warnLegacyMainKeys()

	// Optional shared fallback for 4shared accounts that don't set their own.
	cfg.FourShared = OAuthApp{
		ConsumerKey:    os.Getenv("FOURSHARED_CONSUMER_KEY"),
		ConsumerSecret: os.Getenv("FOURSHARED_CONSUMER_SECRET"),
		Domain:         os.Getenv("FOURSHARED_CONSUMER_DOMAIN"),
	}

	for _, pe := range providerEnv {
		if main, ok := loadMainAccount(pe.Provider, pe.Prefix); ok {
			cfg.Mains = append(cfg.Mains, main)
		}
		cfg.Accounts = append(cfg.Accounts, loadProviderAccounts(pe.Provider, pe.Prefix)...)
	}

	// Apply the shared 4shared consumer fallback where an account omitted its own.
	// Main accounts get the same treatment as numbered ones: a single registered
	// app can cover every 4shared account, main included.
	for i := range cfg.Accounts {
		a := &cfg.Accounts[i]
		if a.Provider == ProviderFourShared {
			a.ConsumerKey, a.ConsumerSecret, a.ConsumerDomain =
				withFallback(a.ConsumerKey, a.ConsumerSecret, a.ConsumerDomain, cfg.FourShared)
		}
	}
	for i := range cfg.Mains {
		m := &cfg.Mains[i]
		if m.Provider == ProviderFourShared {
			m.ConsumerKey, m.ConsumerSecret, m.ConsumerDomain =
				withFallback(m.ConsumerKey, m.ConsumerSecret, m.ConsumerDomain, cfg.FourShared)
		}
	}

	slog.Info("read accounts from .env", "total", len(cfg.Accounts), "main_accounts", len(cfg.Mains))
	return cfg, nil
}

// WarnIncompleteMains reports main accounts that are configured but missing
// credentials they need. A main account that is absent entirely is a supported
// choice and stays silent; only a half-configured one is worth a warning.
func WarnIncompleteMains(mains []MainAccount) {
	for _, m := range mains {
		if missing := m.MissingKeys(); len(missing) > 0 {
			slog.Warn("main account is incompletely configured; it cannot receive the metadata database backup",
				"provider", m.Provider, "email", m.Email,
				"missing", strings.Join(missing, ", "))
		}
	}
}

// withFallback fills empty per-account consumer credentials from the shared app.
func withFallback(key, secret, domain string, app OAuthApp) (string, string, string) {
	if key == "" {
		key = app.ConsumerKey
	}
	if secret == "" {
		secret = app.ConsumerSecret
	}
	if domain == "" {
		domain = app.Domain
	}
	return key, secret, domain
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

// TierFree and TierPaid are the account plans .env may declare. They mirror the
// limits package's tiers; the string lives here too so accounts does not depend
// on the limits table just to parse one .env value.
const (
	TierFree = "free"
	TierPaid = "paid"
)

// normalizeTier maps a .env value onto a known tier. An unset or unrecognised
// value becomes free, which is the conservative answer: a free tier carries the
// strict caps, so mislabelling a paid account only splits archives it did not
// have to, while guessing "paid" would let an oversized file through to a
// provider that rejects it.
func normalizeTier(raw string) string {
	if strings.EqualFold(strings.TrimSpace(raw), TierPaid) {
		return TierPaid
	}
	return TierFree
}

// mainSuffix is the slot a main account occupies where a numbered account has
// its index: MEGA_ACCOUNT_MAIN_EMAIL alongside MEGA_ACCOUNT_1_EMAIL.
const mainSuffix = "MAIN"

// EnvSlot is the .env key fragment identifying an account: its index, or MAIN
// for a database-backup account. Used when telling the user which keys to edit.
func EnvSlot(index int) string {
	if index <= 0 {
		return mainSuffix
	}
	return strconv.Itoa(index)
}

// KeyPrefix returns the .env prefix for one account slot, e.g.
// "FOURSHARED_ACCOUNT_2". Messages quote real keys so they can be pasted.
func KeyPrefix(provider ProviderType, index int) string {
	for _, pe := range providerEnv {
		if pe.Provider == provider {
			return pe.Prefix + "_" + EnvSlot(index)
		}
	}
	return strings.ToUpper(string(provider)) + "_ACCOUNT_" + EnvSlot(index)
}

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
		ConsumerDomain:   os.Getenv(key("_CONSUMER_DOMAIN")),
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
			Tier:             normalizeTier(os.Getenv(fmt.Sprintf("%s_%d_TIER", prefix, i))),
			ConsumerKey:      os.Getenv(fmt.Sprintf("%s_%d_CONSUMER_KEY", prefix, i)),
			ConsumerSecret:   os.Getenv(fmt.Sprintf("%s_%d_CONSUMER_SECRET", prefix, i)),
			ConsumerDomain:   os.Getenv(fmt.Sprintf("%s_%d_CONSUMER_DOMAIN", prefix, i)),
			OAuthToken:       os.Getenv(fmt.Sprintf("%s_%d_OAUTH_TOKEN", prefix, i)),
			OAuthTokenSecret: os.Getenv(fmt.Sprintf("%s_%d_OAUTH_TOKEN_SECRET", prefix, i)),
		})
	}
	return accounts
}
