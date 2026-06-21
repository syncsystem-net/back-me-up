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
)

// Connect resolves the credentials for (providerName, email) from the account
// store, constructs the matching provider, and authenticates it. The returned
// provider is bound to that single account and ready for Upload/Download/
// Delete/FindByName/GetQuota.
func Connect(ctx context.Context, store *accounts.AccountStore, providerName, email string, chunkSizeBytes int64) (provider.Provider, error) {
	acct, ok := findAccount(store, providerName, email)
	if !ok {
		return nil, fmt.Errorf("no credentials in .env for %s account %s", providerName, email)
	}
	p, err := registry.New(providerName, oauthFor(acct), provider.Config{ChunkSizeBytes: chunkSizeBytes})
	if err != nil {
		return nil, err
	}
	if err := p.Login(ctx, acct.Email, acct.Password); err != nil {
		return nil, err
	}
	return p, nil
}

func findAccount(store *accounts.AccountStore, providerName, email string) (accounts.Account, bool) {
	if store == nil {
		return accounts.Account{}, false
	}
	for _, a := range store.Accounts {
		if string(a.Provider) == providerName && a.Email == email {
			return a, true
		}
	}
	return accounts.Account{}, false
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
