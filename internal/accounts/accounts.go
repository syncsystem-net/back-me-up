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
}

type MainAccount struct {
	Provider ProviderType
	Email    string
	Password string
}

type AccountStore struct {
	Main     MainAccount
	Accounts []Account
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

	store.Accounts = append(store.Accounts, loadProviderAccounts(ProviderMega, "MEGA_ACCOUNT")...)
	store.Accounts = append(store.Accounts, loadProviderAccounts(ProviderFourShared, "FOURSHARED_ACCOUNT")...)

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
			Provider: provider,
			Email:    email,
			Password: password,
			QuotaGB:  quota,
			Index:    i,
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
