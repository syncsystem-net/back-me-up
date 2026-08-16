// Package cloud builds logged-in provider instances from the in-memory account
// store. It is the single place that turns a (provider, email) pair plus the
// credentials loaded from .env into an authenticated provider.Provider, so both
// the upload worker and the HTTP handlers (download/delete/conflict checks)
// share one connection path. The provider registry remains the only thing that
// knows about concrete backends; this package depends on the interface.
package cloud

import (
	"context"
	"fmt"

	"github.com/syncsystem-net/back-me-up/internal/accounts"
	"github.com/syncsystem-net/back-me-up/internal/provider"
	"github.com/syncsystem-net/back-me-up/internal/provider/registry"
	"github.com/syncsystem-net/back-me-up/internal/ratelimit"
)

// Connect resolves the credentials for (providerName, email) from the account
// store, constructs the matching provider, and authenticates it. The returned
// provider is bound to that single account and ready for Upload/Download/
// Delete/FindByName/GetQuota.
func Connect(ctx context.Context, store *accounts.AccountStore, providerName, email string, chunkSizeBytes int64) (provider.Provider, error) {
	// A locked store is empty, so without this check every credential operation
	// would report "no credentials for this account" and send the user looking in
	// the wrong place. The credentials are on disk and intact; they just cannot
	// be opened.
	if reason := store.LockReason(); reason != "" {
		return nil, fmt.Errorf("%w: %s", accounts.ErrLocked, reason)
	}
	acct, ok := store.Find(providerName, email)
	if !ok {
		return nil, fmt.Errorf("no stored credentials for %s account %s", providerName, email)
	}
	p, err := registry.New(providerName, oauthFor(acct), provider.Config{
		ChunkSizeBytes: chunkSizeBytes,
		RateLimiter:    ratelimit.For(providerName),
	})
	if err != nil {
		return nil, err
	}
	// Wrapped before Login so a rejected token is noticed there too — that is the
	// most common place an expired one shows up.
	p = watch(p, providerName, email, false)
	if err := p.Login(ctx, acct.Email, acct.Password); err != nil {
		return nil, err
	}
	return p, nil
}

// NewMain builds — but does not log in — the backend for a database-backup
// account.
//
// It is deliberately a separate entry point rather than a widening of Connect.
// Connect resolves credentials from the numbered accounts only, and every
// handler that takes a provider/email pair goes through it; teaching it about
// main accounts would make the db-backup account addressable as an upload
// target. Callers here already hold the MainAccount and have to have gone
// looking for it.
func NewMain(m accounts.MainAccount, chunkSizeBytes int64) (provider.Provider, error) {
	p, err := registry.New(string(m.Provider), MainOAuth(m), provider.Config{
		ChunkSizeBytes: chunkSizeBytes,
		RateLimiter:    ratelimit.For(string(m.Provider)),
	})
	if err != nil {
		return nil, err
	}
	return watch(p, string(m.Provider), m.Email, true), nil
}

// MainOAuth supplies OAuth creds when the main account is an OAuth provider. A
// main account carries its own consumer credentials and access token, so nothing
// here has to look them up on a numbered account.
func MainOAuth(m accounts.MainAccount) provider.OAuthCreds {
	if m.Provider != accounts.ProviderFourShared {
		return provider.OAuthCreds{}
	}
	return provider.OAuthCreds{
		ConsumerKey:    m.ConsumerKey,
		ConsumerSecret: m.ConsumerSecret,
		Token:          m.OAuthToken,
		TokenSecret:    m.OAuthTokenSecret,
	}
}

func oauthFor(a accounts.Account) provider.OAuthCreds {
	switch a.Provider {
	case accounts.ProviderFourShared:
		// Account-level consumer creds are resolved (with shared fallback) at
		// load time in the accounts package.
		return provider.OAuthCreds{
			ConsumerKey:    a.ConsumerKey,
			ConsumerSecret: a.ConsumerSecret,
			Token:          a.OAuthToken,
			TokenSecret:    a.OAuthTokenSecret,
		}
	default:
		return provider.OAuthCreds{}
	}
}
