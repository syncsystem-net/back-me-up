// Command fourshared-test exercises a single 4shared account's credentials in
// isolation (no MEGA, no worker, no browser): it loads the account from .env,
// signs a GET /user call, and reports the quota or the error. Run with debug to
// see the exact OAuth base string, Authorization header, and raw response:
//
//	FOURSHARED_DEBUG=1 go run ./cmd/fourshared-test -account 1
//
// Use it to diagnose 4shared 401s after authorizing an account.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/syncsystem-net/back-me-up/internal/accounts"
	"github.com/syncsystem-net/back-me-up/internal/provider/fourshared"
)

func main() {
	accountIndex := flag.Int("account", 1, "the FOURSHARED_ACCOUNT_<n> to test")
	flag.Parse()

	// Force debug logging on for this diagnostic tool.
	if os.Getenv("FOURSHARED_DEBUG") == "" {
		os.Setenv("FOURSHARED_DEBUG", "1")
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	store, err := accounts.Load(".env")
	if err != nil {
		fmt.Fprintf(os.Stderr, "loading .env: %v\n", err)
		os.Exit(1)
	}

	var acct *accounts.Account
	for i := range store.Accounts {
		a := &store.Accounts[i]
		if a.Provider == accounts.ProviderFourShared && a.Index == *accountIndex {
			acct = a
			break
		}
	}
	if acct == nil {
		fmt.Fprintf(os.Stderr, "no FOURSHARED_ACCOUNT_%d found in .env\n", *accountIndex)
		os.Exit(1)
	}

	fmt.Printf("Testing 4shared account %d: %s\n", *accountIndex, acct.Email)
	fmt.Printf("  consumer_key present: %v\n", acct.ConsumerKey != "")
	fmt.Printf("  consumer_secret present: %v\n", acct.ConsumerSecret != "")
	fmt.Printf("  oauth_token present: %v\n", acct.OAuthToken != "")
	fmt.Printf("  oauth_token_secret present: %v\n\n", acct.OAuthTokenSecret != "")

	c := fourshared.New(0, acct.ConsumerKey, acct.ConsumerSecret, acct.OAuthToken, acct.OAuthTokenSecret)
	total, used, err := c.GetQuota(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nFAILED: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("\nOK — quota total=%d bytes, used=%d bytes\n", total, used)
}
