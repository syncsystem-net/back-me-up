package database

import (
	"database/sql"
	"fmt"
	"time"
)

type DBAccount struct {
	ID           int64
	Provider     string
	Email        string
	QuotaTotalGB float64
	QuotaUsedGB  float64
	CreatedAt    time.Time
}

func UpsertAccount(db *sql.DB, provider, email string, quotaTotalGB float64) (int64, error) {
	_, err := db.Exec(
		`INSERT INTO accounts (provider, email, quota_total_gb) VALUES (?, ?, ?)
		 ON CONFLICT(email) DO UPDATE SET provider=excluded.provider, quota_total_gb=excluded.quota_total_gb`,
		provider, email, quotaTotalGB,
	)
	if err != nil {
		return 0, fmt.Errorf("upserting account: %w", err)
	}

	var id int64
	if err := db.QueryRow(`SELECT id FROM accounts WHERE email = ?`, email).Scan(&id); err != nil {
		return 0, fmt.Errorf("getting account id: %w", err)
	}
	return id, nil
}

func ListDBAccounts(db *sql.DB) ([]*DBAccount, error) {
	rows, err := db.Query(
		`SELECT id, provider, email, quota_total_gb, quota_used_gb, created_at FROM accounts ORDER BY provider, email`,
	)
	if err != nil {
		return nil, fmt.Errorf("querying accounts: %w", err)
	}
	defer rows.Close()

	var accounts []*DBAccount
	for rows.Next() {
		a := &DBAccount{}
		if err := rows.Scan(&a.ID, &a.Provider, &a.Email, &a.QuotaTotalGB, &a.QuotaUsedGB, &a.CreatedAt); err != nil {
			return nil, fmt.Errorf("scanning account row: %w", err)
		}
		accounts = append(accounts, a)
	}
	return accounts, rows.Err()
}

func GetDBAccountByID(db *sql.DB, id int64) (*DBAccount, error) {
	row := db.QueryRow(
		`SELECT id, provider, email, quota_total_gb, quota_used_gb, created_at FROM accounts WHERE id = ?`,
		id,
	)
	a := &DBAccount{}
	if err := row.Scan(&a.ID, &a.Provider, &a.Email, &a.QuotaTotalGB, &a.QuotaUsedGB, &a.CreatedAt); err != nil {
		return nil, fmt.Errorf("scanning account: %w", err)
	}
	return a, nil
}
