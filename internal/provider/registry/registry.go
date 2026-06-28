// Package registry constructs provider.Provider implementations by name. It is
// the single place that knows about every concrete backend, so adding a new
// cloud storage is a focused change: implement provider.Provider in a new
// subpackage and add one case here.
package registry

import (
	"fmt"

	"github.com/syncsystem-net/back-me-up/internal/provider"
	"github.com/syncsystem-net/back-me-up/internal/provider/fourshared"
	"github.com/syncsystem-net/back-me-up/internal/provider/mega"
)

// New returns an unauthenticated provider for the given name. oauth carries the
// credentials OAuth-based providers need at construction; password providers
// ignore it and authenticate later via Provider.Login.
func New(name string, oauth provider.OAuthCreds, cfg provider.Config) (provider.Provider, error) {
	switch name {
	case "mega":
		return mega.New(cfg.ChunkSizeBytes, cfg.RateLimiter), nil
	case "fourshared":
		return fourshared.New(cfg.ChunkSizeBytes, cfg.RateLimiter, oauth.ConsumerKey, oauth.ConsumerSecret, oauth.Token, oauth.TokenSecret), nil
	default:
		return nil, fmt.Errorf("unknown provider %q", name)
	}
}

// Supported reports whether a provider name has an implementation.
func Supported(name string) bool {
	switch name {
	case "mega", "fourshared":
		return true
	default:
		return false
	}
}
