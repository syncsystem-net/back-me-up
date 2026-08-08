// Command fourshared-test exercises a single 4shared account's credentials in
// isolation (no MEGA, no worker, no browser): it loads the account from .env,
// signs a GET /user call, and reports the quota or the error. Run with debug to
// see the exact OAuth base string, Authorization header, and raw response:
//
//	FOURSHARED_DEBUG=1 go run ./cmd/fourshared-test -account 1
//	FOURSHARED_DEBUG=1 go run ./cmd/fourshared-test -account main
//
// Use it to diagnose 4shared 401s after authorizing an account.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/syncsystem-net/back-me-up/internal/accounts"
	"github.com/syncsystem-net/back-me-up/internal/provider/fourshared"
)

func main() {
	account := flag.String("account", "1", `the 4shared account to test: a number, or "main" for the database-backup account`)
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

	// The main account is not in store.Accounts (it is never an upload target),
	// so it is resolved separately. An expired main-account token is exactly what
	// this tool exists to diagnose, and the metadata DB backup — which runs at the
	// end of a job and only logs — is the sole other place it would show up.
	var email, consumerKey, consumerSecret, token, tokenSecret string
	if strings.EqualFold(strings.TrimSpace(*account), "main") {
		m, ok := store.MainFor(accounts.ProviderFourShared)
		if !ok {
			fmt.Fprintln(os.Stderr, "no FOURSHARED_ACCOUNT_MAIN_EMAIL found in .env")
			os.Exit(1)
		}
		email, consumerKey, consumerSecret = m.Email, m.ConsumerKey, m.ConsumerSecret
		token, tokenSecret = m.OAuthToken, m.OAuthTokenSecret
	} else {
		idx, err := strconv.Atoi(strings.TrimSpace(*account))
		if err != nil || idx < 1 {
			fmt.Fprintf(os.Stderr, "-account must be a positive number or \"main\", got %q\n", *account)
			os.Exit(1)
		}
		var acct *accounts.Account
		for i := range store.Accounts {
			a := &store.Accounts[i]
			if a.Provider == accounts.ProviderFourShared && a.Index == idx {
				acct = a
				break
			}
		}
		if acct == nil {
			fmt.Fprintf(os.Stderr, "no FOURSHARED_ACCOUNT_%d found in .env\n", idx)
			os.Exit(1)
		}
		email, consumerKey, consumerSecret = acct.Email, acct.ConsumerKey, acct.ConsumerSecret
		token, tokenSecret = acct.OAuthToken, acct.OAuthTokenSecret
	}

	fmt.Printf("Testing 4shared account %s: %s\n", *account, email)
	fmt.Printf("  consumer_key present: %v\n", consumerKey != "")
	fmt.Printf("  consumer_secret present: %v\n", consumerSecret != "")
	fmt.Printf("  oauth_token present: %v\n", token != "")
	fmt.Printf("  oauth_token_secret present: %v\n\n", tokenSecret != "")

	c := fourshared.New(0, nil, consumerKey, consumerSecret, token, tokenSecret)
	total, used, err := c.GetQuota(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nFAILED: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("\nOK — quota total=%d bytes, used=%d bytes\n", total, used)
}
