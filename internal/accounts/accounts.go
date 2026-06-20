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

type MainAccount struct {
	Provider ProviderType
	Email    string
	Password string
}

// OAuthApp holds app-level OAuth consumer credentials used as a fallback when an
// account does not declare its own. Obtained by registering an application with
// the provider; see the README "Provider credentials" section.
type OAuthApp struct {
	ConsumerKey    string
	ConsumerSecret string
}

type AccountStore struct {
	Main     MainAccount
	Accounts []Account

	// FourShared is an optional fallback 4shared consumer key/secret applied to
	// any 4shared account that doesn't set its own FOURSHARED_ACCOUNT_<n>_CONSUMER_*.
	// Lets a single shared app cover all accounts when per-account apps aren't used.
	FourShared OAuthApp
}

func Load(envPath string) (*AccountStore, error) {
	if err := godotenv.Load(envPath); err != nil {
		return nil, fmt.Errorf("loading .env file: %w", err)
	}

	store := &AccountStore{}

	mainProvider := os.Getenv("MAIN_ACCOUNT_PROVIDER")
	if mainProvider == "" {
		return nil, fmt.Errorf("MAIN_ACCOUNT_PROVIDER is required")
	}
	store.Main = MainAccount{
		Provider: ProviderType(strings.ToLower(mainProvider)),
		Email:    os.Getenv("MAIN_ACCOUNT_EMAIL"),
		Password: os.Getenv("MAIN_ACCOUNT_PASSWORD"),
	}

	// Optional shared fallback for 4shared accounts that don't set their own.
	store.FourShared = OAuthApp{
		ConsumerKey:    os.Getenv("FOURSHARED_CONSUMER_KEY"),
		ConsumerSecret: os.Getenv("FOURSHARED_CONSUMER_SECRET"),
	}

	store.Accounts = append(store.Accounts, loadProviderAccounts(ProviderMega, "MEGA_ACCOUNT")...)
	store.Accounts = append(store.Accounts, loadProviderAccounts(ProviderFourShared, "FOURSHARED_ACCOUNT")...)

	// Apply the shared 4shared consumer fallback where an account omitted its own.
	for i := range store.Accounts {
		a := &store.Accounts[i]
		if a.Provider == ProviderFourShared {
			if a.ConsumerKey == "" {
				a.ConsumerKey = store.FourShared.ConsumerKey
			}
			if a.ConsumerSecret == "" {
				a.ConsumerSecret = store.FourShared.ConsumerSecret
			}
		}
	}

	slog.Info("accounts loaded", "total", len(store.Accounts), "main_provider", store.Main.Provider)
	return store, nil
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
