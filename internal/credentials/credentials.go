// Package credentials owns where cloud credentials live and which copy wins.
//
// Before PR #13 the answer was simple and limiting: .env was re-read on every
// boot and held in memory, so nothing the application learned about an account
// could be persisted — including a 4shared token it had just re-authorized.
// Now the database is authoritative. .env is still read at every start and
// reconciled into it, because .env remains the only way to add an account or
// change a password, but the reconciliation has one rule that makes the
// Re-authorize button worth having: an OAuth token this application obtained
// itself is never overwritten by a stale value in .env.
//
// Credentials are sealed with the keyring before they are written, so the
// metadata database — which is uploaded to every main account after every
// successful job — does not carry passwords and tokens in the clear.
package credentials

import (
	"database/sql"
	"fmt"
	"log/slog"
	"sync"

	"github.com/syncsystem-net/back-me-up/internal/accounts"
	"github.com/syncsystem-net/back-me-up/internal/database"
	"github.com/syncsystem-net/back-me-up/internal/keyring"
)

// Secrets is the sealed credential document stored per account. It is one blob
// rather than a column per field so a provider's credential set can grow without
// a schema migration, and so each account has exactly one nonce and one
// authentication scope.
type Secrets struct {
	Password         string `json:"password,omitempty"`
	ConsumerKey      string `json:"consumer_key,omitempty"`
	ConsumerSecret   string `json:"consumer_secret,omitempty"`
	ConsumerDomain   string `json:"consumer_domain,omitempty"`
	OAuthToken       string `json:"oauth_token,omitempty"`
	OAuthTokenSecret string `json:"oauth_token_secret,omitempty"`
}

// Kinds of account, used as the first component of the sealing AAD so a blob
// cannot be moved between the two tables and still open.
const (
	kindAccount = "account"
	kindMain    = "main"
)

// ErrLocked is returned by every write path when credentials cannot be opened.
// A wrong passphrase must never cause a re-seal: the credentials on disk are
// intact and a correct passphrase would still open them, so writing anything
// under the wrong key would destroy what is recoverable.
var ErrLocked = accounts.ErrLocked

// Manager reconciles .env into the database and serves the running credential
// set. A nil keyring means locked.
type Manager struct {
	db         *sql.DB
	kr         *keyring.Keyring
	store      *accounts.AccountStore
	lockReason string

	// mu serialises credential writes. Reads go through the store, which does its
	// own locking.
	mu sync.Mutex
}

// New returns a Manager that can open and seal credentials.
func New(db *sql.DB, kr *keyring.Keyring) *Manager {
	return &Manager{db: db, kr: kr, store: accounts.NewStore(nil, nil, accounts.OAuthApp{})}
}

// NewLocked returns a Manager that refuses every credential operation, with a
// reason the UI shows verbatim. The server still runs: existing backup records
// stay browsable, and a process that simply exits would tell the user nothing
// beyond "connection refused".
func NewLocked(db *sql.DB, reason string) *Manager {
	store := accounts.NewStore(nil, nil, accounts.OAuthApp{})
	store.Lock(reason)
	return &Manager{db: db, store: store, lockReason: reason}
}

// Store returns the shared credential set. It is the same pointer for the life
// of the Manager, so consumers that captured it at startup see re-authorized
// tokens without a restart.
func (m *Manager) Store() *accounts.AccountStore { return m.store }

// Locked reports whether credentials are unavailable.
func (m *Manager) Locked() bool { return m.kr == nil }

// LockReason explains why, or "" when unlocked.
func (m *Manager) LockReason() string { return m.lockReason }

// Import reconciles what .env declares into the database and then loads the
// running credential set from it.
//
// The rules, in one place because they are the whole contract:
//
//   - An account in .env that has no row is created, credentials sealed.
//   - An account with a row has its password and consumer credentials updated
//     from .env when .env gives a non-empty value.
//   - An OAuth token written by this application (token_source = app) is kept,
//     even if .env still holds the old one. This is the rule the Re-authorize
//     button depends on.
//   - A blank value in .env never erases a stored one. Absence is not deletion.
//   - A numbered account with a row but no .env entry is kept. Removing an
//     upload target must be deliberate, not a side effect of editing a file.
//   - A main account is the exception: it is keyed by provider and .env is its
//     only removal path, so deleting its .env block does delete the row.
//     Continuing to copy the metadata database to a destination the user removed
//     would be worse than the asymmetry.
func (m *Manager) Import(env *accounts.EnvConfig) error {
	if m.Locked() {
		return ErrLocked
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	if env != nil {
		if err := m.importAccounts(env.Accounts); err != nil {
			return err
		}
		if err := m.importMains(env.Mains); err != nil {
			return err
		}
	}
	return m.load(fourSharedApp(env))
}

// Reload rebuilds the running credential set from the database without touching
// .env. Used after a re-authorization so the worker and handlers pick up the new
// token immediately.
func (m *Manager) Reload() error {
	if m.Locked() {
		return ErrLocked
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.load(m.store.FourSharedApp())
}

func fourSharedApp(env *accounts.EnvConfig) accounts.OAuthApp {
	if env == nil {
		return accounts.OAuthApp{}
	}
	return env.FourShared
}

func (m *Manager) importAccounts(declared []accounts.Account) error {
	stored, err := database.ListAccountRows(m.db)
	if err != nil {
		return err
	}
	byKey := make(map[string]database.AccountRow, len(stored))
	for _, r := range stored {
		byKey[r.Provider+"/"+r.Email] = r
	}

	for _, a := range declared {
		provider, email := string(a.Provider), a.Email
		row, exists := byKey[provider+"/"+email]

		current := Secrets{}
		if exists && len(row.SecretsEnc) > 0 {
			current = m.openOrWarn(kindAccount, provider, email, row.SecretsEnc)
		}
		merged, tokenSource := merge(current, envSecrets(a), row.TokenSource)
		warnIgnoredEnvToken(provider, email, current, envSecrets(a), row.TokenSource)

		if exists && merged == current && row.EnvIndex == a.Index && row.TokenSource == tokenSource && !quotaChanged(row, a) {
			continue // nothing .env can tell us that we do not already have
		}
		sealed, err := m.kr.SealJSON(keyring.AccountAAD(kindAccount, provider, email), merged)
		if err != nil {
			return err
		}
		if _, err := database.UpsertAccountRow(m.db, database.AccountRow{
			Provider:    provider,
			Email:       email,
			QuotaGB:     a.QuotaGB,
			EnvIndex:    a.Index,
			SecretsEnc:  sealed,
			TokenSource: tokenSource,
		}); err != nil {
			return err
		}
		if exists {
			slog.Info("updated stored credentials from .env", "provider", provider, "email", email)
		} else {
			slog.Info("imported account from .env into the encrypted store", "provider", provider, "email", email)
		}
		// A token that actually changed clears any standing re-authorization
		// flag: whatever was wrong with the old one no longer applies.
		if current.OAuthToken != merged.OAuthToken {
			if err := database.SetAccountReauth(m.db, provider, email, false, ""); err != nil {
				return err
			}
		}
	}
	return nil
}

// quotaChanged reports whether .env declares a quota different from the stored
// one. A declared zero means "not declared" and never counts as a change — the
// quota poller writes the provider's real figure into the same column.
func quotaChanged(row database.AccountRow, a accounts.Account) bool {
	return a.QuotaGB > 0 && a.QuotaGB != row.QuotaGB
}

func (m *Manager) importMains(declared []accounts.MainAccount) error {
	stored, err := database.ListMainAccountRows(m.db)
	if err != nil {
		return err
	}
	byProvider := make(map[string]database.MainAccountRow, len(stored))
	for _, r := range stored {
		byProvider[r.Provider] = r
	}
	declaredProviders := map[string]bool{}

	for _, mn := range declared {
		provider := string(mn.Provider)
		declaredProviders[provider] = true
		row, exists := byProvider[provider]

		current := Secrets{}
		// A main account's identity is its provider, so a changed email means the
		// stored credentials belong to a different account and must not be merged
		// into the new one.
		if exists && len(row.SecretsEnc) > 0 && row.Email == mn.Email {
			current = m.openOrWarn(kindMain, provider, row.Email, row.SecretsEnc)
		}
		merged, tokenSource := merge(current, mainSecrets(mn), row.TokenSource)

		if exists && row.Email == mn.Email && merged == current && row.TokenSource == tokenSource {
			continue
		}
		sealed, err := m.kr.SealJSON(keyring.AccountAAD(kindMain, provider, mn.Email), merged)
		if err != nil {
			return err
		}
		if _, err := database.UpsertMainAccountRow(m.db, database.MainAccountRow{
			Provider:    provider,
			Email:       mn.Email,
			SecretsEnc:  sealed,
			TokenSource: tokenSource,
		}); err != nil {
			return err
		}
		slog.Info("stored main account credentials", "provider", provider, "email", mn.Email)
		if current.OAuthToken != merged.OAuthToken {
			if err := database.SetMainAccountReauth(m.db, provider, false, ""); err != nil {
				return err
			}
		}
	}

	// A main account removed from .env is removed here too — see Import's doc
	// comment for why this is the one place absence means deletion.
	for _, r := range stored {
		if declaredProviders[r.Provider] {
			continue
		}
		slog.Warn("main account is no longer configured in .env; removing it as a database-backup destination",
			"provider", r.Provider, "email", r.Email)
		if err := database.DeleteMainAccount(m.db, r.Provider); err != nil {
			return err
		}
	}
	return nil
}

// load rebuilds the in-memory credential set from the database.
func (m *Manager) load(app accounts.OAuthApp) error {
	rows, err := database.ListAccountRows(m.db)
	if err != nil {
		return err
	}
	accts := make([]accounts.Account, 0, len(rows))
	for _, r := range rows {
		s := Secrets{}
		if len(r.SecretsEnc) > 0 {
			s = m.openOrWarn(kindAccount, r.Provider, r.Email, r.SecretsEnc)
		}
		accts = append(accts, accounts.Account{
			Provider:         accounts.ProviderType(r.Provider),
			Email:            r.Email,
			Password:         s.Password,
			QuotaGB:          r.QuotaGB,
			Index:            r.EnvIndex,
			ConsumerKey:      s.ConsumerKey,
			ConsumerSecret:   s.ConsumerSecret,
			ConsumerDomain:   s.ConsumerDomain,
			OAuthToken:       s.OAuthToken,
			OAuthTokenSecret: s.OAuthTokenSecret,
			NeedsReauth:      r.NeedsReauth,
			ReauthReason:     r.ReauthReason,
		})
	}

	mainRows, err := database.ListMainAccountRows(m.db)
	if err != nil {
		return err
	}
	mains := make([]accounts.MainAccount, 0, len(mainRows))
	for _, r := range mainRows {
		s := Secrets{}
		if len(r.SecretsEnc) > 0 {
			s = m.openOrWarn(kindMain, r.Provider, r.Email, r.SecretsEnc)
		}
		mains = append(mains, accounts.MainAccount{
			Provider:         accounts.ProviderType(r.Provider),
			Email:            r.Email,
			Password:         s.Password,
			ConsumerKey:      s.ConsumerKey,
			ConsumerSecret:   s.ConsumerSecret,
			ConsumerDomain:   s.ConsumerDomain,
			OAuthToken:       s.OAuthToken,
			OAuthTokenSecret: s.OAuthTokenSecret,
			NeedsReauth:      r.NeedsReauth,
			ReauthReason:     r.ReauthReason,
		})
	}
	// Order mains by the provider list so the Accounts view is stable rather than
	// alphabetical-by-accident.
	mains = orderMains(mains)

	m.store.Replace(mains, accts, app)
	slog.Info("credentials loaded from the encrypted store", "accounts", len(accts), "main_accounts", len(mains))
	return nil
}

func orderMains(in []accounts.MainAccount) []accounts.MainAccount {
	out := make([]accounts.MainAccount, 0, len(in))
	for _, p := range accounts.Providers() {
		for _, m := range in {
			if m.Provider == p {
				out = append(out, m)
			}
		}
	}
	// Anything naming an unknown provider still belongs in the list; it is
	// reported as unsupported elsewhere rather than silently dropped here.
	for _, m := range in {
		known := false
		for _, p := range accounts.Providers() {
			if m.Provider == p {
				known = true
			}
		}
		if !known {
			out = append(out, m)
		}
	}
	return out
}

// openOrWarn unseals a stored blob, degrading to empty credentials with a
// warning rather than failing the whole load. One unopenable row (a database
// copied from another install, say) must not cost every other account its
// credentials; the affected account fails at login with a message that names it.
func (m *Manager) openOrWarn(kind, provider, email string, blob []byte) Secrets {
	var s Secrets
	if err := m.kr.OpenJSON(keyring.AccountAAD(kind, provider, email), blob, &s); err != nil {
		slog.Error("stored credentials could not be decrypted; this account has no usable credentials until .env supplies them again",
			"kind", kind, "provider", provider, "email", email, "error", err)
		return Secrets{}
	}
	return s
}

// merge applies .env on top of what is stored. Non-empty .env values win for
// everything except an OAuth token this application wrote itself.
func merge(stored, declared Secrets, tokenSource string) (Secrets, string) {
	out := stored
	if declared.Password != "" {
		out.Password = declared.Password
	}
	if declared.ConsumerKey != "" {
		out.ConsumerKey = declared.ConsumerKey
	}
	if declared.ConsumerSecret != "" {
		out.ConsumerSecret = declared.ConsumerSecret
	}
	if declared.ConsumerDomain != "" {
		out.ConsumerDomain = declared.ConsumerDomain
	}

	// The token and its secret are one credential and only move as a pair: half a
	// pair cannot sign anything.
	appWritten := tokenSource == database.TokenSourceApp && stored.OAuthToken != ""
	switch {
	case appWritten:
		// Keep what re-authorization produced. .env's copy is the stale one.
	case declared.OAuthToken != "" && declared.OAuthTokenSecret != "":
		out.OAuthToken = declared.OAuthToken
		out.OAuthTokenSecret = declared.OAuthTokenSecret
		tokenSource = database.TokenSourceEnv
	}
	if out.OAuthToken == "" {
		tokenSource = ""
	}
	return out, tokenSource
}

// warnIgnoredEnvToken reports a .env token that was discarded because the
// application wrote its own.
//
// Keeping the app-written token is the rule that makes re-authorization work,
// but discarding a *different* value in silence is its own trap: a user whose
// app-written token later expires will reach for cmd/fourshared-auth, paste the
// result into .env, restart, and watch it keep failing with nothing in the log
// to explain why. Naming the remedy costs one line.
func warnIgnoredEnvToken(provider, email string, stored, declared Secrets, tokenSource string) {
	if tokenSource != database.TokenSourceApp || stored.OAuthToken == "" {
		return
	}
	if declared.OAuthToken == "" || declared.OAuthToken == stored.OAuthToken {
		return
	}
	slog.Warn("ignoring the OAuth token in .env: this account was re-authorized in the app, and that token is the current one",
		"provider", provider, "email", email,
		"remedy", "to use the .env value instead, clear this account's stored token by re-authorizing it in the Accounts view")
}

func envSecrets(a accounts.Account) Secrets {
	return Secrets{
		Password:         a.Password,
		ConsumerKey:      a.ConsumerKey,
		ConsumerSecret:   a.ConsumerSecret,
		ConsumerDomain:   a.ConsumerDomain,
		OAuthToken:       a.OAuthToken,
		OAuthTokenSecret: a.OAuthTokenSecret,
	}
}

func mainSecrets(m accounts.MainAccount) Secrets {
	return Secrets{
		Password:         m.Password,
		ConsumerKey:      m.ConsumerKey,
		ConsumerSecret:   m.ConsumerSecret,
		ConsumerDomain:   m.ConsumerDomain,
		OAuthToken:       m.OAuthToken,
		OAuthTokenSecret: m.OAuthTokenSecret,
	}
}

// SaveOAuthToken stores a token this application obtained through the in-app
// re-authorization flow, marks it as app-written so .env cannot overwrite it,
// clears the account's re-authorization flag, and refreshes the running
// credential set so the change takes effect without a restart.
func (m *Manager) SaveOAuthToken(provider, email, token, secret string, isMain bool) error {
	if m.Locked() {
		return ErrLocked
	}
	if token == "" || secret == "" {
		return fmt.Errorf("refusing to store an incomplete OAuth token for %s/%s", provider, email)
	}

	m.mu.Lock()
	if err := m.saveToken(provider, email, token, secret, isMain); err != nil {
		m.mu.Unlock()
		return err
	}
	err := m.load(m.store.FourSharedApp())
	m.mu.Unlock()
	return err
}

func (m *Manager) saveToken(provider, email, token, secret string, isMain bool) error {
	kind := kindAccount
	if isMain {
		kind = kindMain
	}

	// The identity to seal under comes back from the lookup, never from the
	// caller. A main account is keyed by provider, so the request may name a
	// different email (or none at all) than the stored row — sealing under the
	// caller's version would bind the blob to an identity nothing ever opens it
	// with, silently destroying that account's whole credential document.
	current, storedEmail, err := m.currentSecrets(kind, provider, email)
	if err != nil {
		return err
	}
	current.OAuthToken = token
	current.OAuthTokenSecret = secret

	sealed, err := m.kr.SealJSON(keyring.AccountAAD(kind, provider, storedEmail), current)
	if err != nil {
		return err
	}
	if isMain {
		return database.SetMainAccountSecrets(m.db, provider, sealed, database.TokenSourceApp)
	}
	return database.SetAccountSecrets(m.db, provider, storedEmail, sealed, database.TokenSourceApp)
}

// currentSecrets reads an account's stored credentials so a token write can keep
// the rest of the document intact. It also returns the email the row is stored
// under, which is the only identity a blob may be sealed with.
func (m *Manager) currentSecrets(kind, provider, email string) (Secrets, string, error) {
	if kind == kindMain {
		rows, err := database.ListMainAccountRows(m.db)
		if err != nil {
			return Secrets{}, "", err
		}
		for _, r := range rows {
			if r.Provider == provider {
				if len(r.SecretsEnc) == 0 {
					return Secrets{}, r.Email, nil
				}
				return m.openOrWarn(kind, provider, r.Email, r.SecretsEnc), r.Email, nil
			}
		}
		return Secrets{}, "", fmt.Errorf("no stored main account for %s", provider)
	}

	rows, err := database.ListAccountRows(m.db)
	if err != nil {
		return Secrets{}, "", err
	}
	for _, r := range rows {
		if r.Provider == provider && r.Email == email {
			if len(r.SecretsEnc) == 0 {
				return Secrets{}, r.Email, nil
			}
			return m.openOrWarn(kind, provider, r.Email, r.SecretsEnc), r.Email, nil
		}
	}
	return Secrets{}, "", fmt.Errorf("no stored account for %s/%s", provider, email)
}

// FlagReauth records that a provider rejected this account's token, so the
// Accounts view can offer a Re-authorize button instead of the user discovering
// it from a failed job. Flagging needs no key, so it works while locked.
func (m *Manager) FlagReauth(provider, email, reason string, isMain bool) error {
	return m.setReauth(provider, email, reason, isMain, true)
}

// ClearReauth withdraws the flag after the account authenticates successfully.
// Without it the flag is a one-way door: a transient rejection would leave a
// standing "needs re-authorization" badge that only a full re-authorization
// could remove, even though the credentials are demonstrably working.
//
// It is a no-op when nothing is flagged, so the success path of every login does
// not write to the database.
func (m *Manager) ClearReauth(provider, email string, isMain bool) error {
	if !m.isFlagged(provider, email, isMain) {
		return nil
	}
	return m.setReauth(provider, email, "", isMain, false)
}

// isFlagged consults the in-memory set rather than the database, so the common
// case (nothing is flagged) costs no query.
func (m *Manager) isFlagged(provider, email string, isMain bool) bool {
	if isMain {
		mn, ok := m.store.MainFor(accounts.ProviderType(provider))
		return ok && mn.NeedsReauth
	}
	a, ok := m.store.Find(provider, email)
	return ok && a.NeedsReauth
}

func (m *Manager) setReauth(provider, email, reason string, isMain, needs bool) error {
	if isMain {
		if err := database.SetMainAccountReauth(m.db, provider, needs, reason); err != nil {
			return err
		}
	} else if err := database.SetAccountReauth(m.db, provider, email, needs, reason); err != nil {
		return err
	}
	if m.Locked() {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.load(m.store.FourSharedApp())
}
