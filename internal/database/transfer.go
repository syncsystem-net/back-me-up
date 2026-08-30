package database

import (
	"database/sql"
	"fmt"
	"time"
)

// transferRetentionDays is how long individual transfer records are kept. The
// longest window any provider declares is measured in hours, so a month of
// history is already far more than enforcement needs; it is retained only so a
// user can see what was uploaded recently. Pruning is what stops the ledger
// growing without bound over years of backups.
const transferRetentionDays = 30

// RecordTransfer appends bytes uploaded to an account. It is written once per
// completed upload rather than accumulated into a counter, because the budget is
// a rolling window: "5 GB in the last 6 hours" can only be answered from
// individual transfers with their own timestamps.
func RecordTransfer(db *sql.DB, accountID, bytes int64) error {
	if bytes <= 0 {
		return nil
	}
	_, err := db.Exec(
		`INSERT INTO transfer_usage (account_id, bytes, at) VALUES (?, ?, CURRENT_TIMESTAMP)`,
		accountID, bytes,
	)
	if err != nil {
		return fmt.Errorf("recording transfer for account %d: %w", accountID, err)
	}
	return nil
}

// TransferUsedSince totals the bytes uploaded to one account inside the last
// windowHours. A window of zero or less returns 0: there is no window to measure
// and therefore nothing consumed.
func TransferUsedSince(db *sql.DB, accountID int64, windowHours int) (int64, error) {
	if windowHours <= 0 {
		return 0, nil
	}
	var used sql.NullInt64
	err := db.QueryRow(
		`SELECT SUM(bytes) FROM transfer_usage
		 WHERE account_id = ? AND at > datetime('now', ?)`,
		accountID, fmt.Sprintf("-%d hours", windowHours),
	).Scan(&used)
	if err != nil {
		return 0, fmt.Errorf("summing transfer for account %d: %w", accountID, err)
	}
	if !used.Valid {
		return 0, nil
	}
	return used.Int64, nil
}

// TransferInFlightBytes totals the size of an account's uploads that are running
// right now.
//
// These bytes are not in the ledger yet — a transfer is recorded when its job
// completes — but they are being spent. Without counting them, every worker
// goroutine independently sees the same remaining allowance and claims against
// it, so with max_concurrent_uploads of 2 and a 3 GB budget, two 1.9 GB volumes
// both pass the check and 3.8 GB goes out against a 3 GB limit.
func TransferInFlightBytes(db *sql.DB, accountID int64) (int64, error) {
	var total sql.NullInt64
	err := db.QueryRow(
		`SELECT SUM(total_bytes) FROM jobs WHERE account_id = ? AND status = 'in_progress'`,
		accountID,
	).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("summing in-flight uploads for account %d: %w", accountID, err)
	}
	if !total.Valid {
		return 0, nil
	}
	return total.Int64, nil
}

// TransferWindowResetsAt returns when enough of an account's consumption will
// have aged out of the window for needBytes to fit, or the zero time if it
// already fits (or never will).
//
// It answers the question a held job actually raises — "when does this resume?"
// — which a bare "you are over budget" cannot. The oldest transfers fall out of
// the window first, so it walks them oldest-first, accumulating what each will
// free until the shortfall is covered.
func TransferWindowResetsAt(db *sql.DB, accountID int64, windowHours int, needBytes, budgetBytes int64) (time.Time, error) {
	if windowHours <= 0 || budgetBytes <= 0 {
		return time.Time{}, nil
	}
	used, err := TransferUsedSince(db, accountID, windowHours)
	if err != nil {
		return time.Time{}, err
	}
	shortfall := needBytes - (budgetBytes - used)
	if shortfall <= 0 {
		return time.Time{}, nil
	}

	rows, err := db.Query(
		`SELECT bytes, at FROM transfer_usage
		 WHERE account_id = ? AND at > datetime('now', ?)
		 ORDER BY at ASC`,
		accountID, fmt.Sprintf("-%d hours", windowHours),
	)
	if err != nil {
		return time.Time{}, fmt.Errorf("reading transfer history for account %d: %w", accountID, err)
	}
	defer rows.Close()

	var freed int64
	for rows.Next() {
		var bytes int64
		var at time.Time
		if err := rows.Scan(&bytes, &at); err != nil {
			return time.Time{}, fmt.Errorf("scanning transfer row: %w", err)
		}
		freed += bytes
		if freed >= shortfall {
			// This transfer leaving the window is the moment enough room exists.
			return at.Add(time.Duration(windowHours) * time.Hour), nil
		}
	}
	if err := rows.Err(); err != nil {
		return time.Time{}, err
	}
	// Even emptying the window does not make room: the job is larger than the
	// whole budget. The caller detects that case explicitly and fails the job
	// rather than holding it, so returning "never" here is correct.
	return time.Time{}, nil
}

// PruneTransfers deletes ledger rows older than the retention period and returns
// how many went. Called once at startup, which is often enough for a table that
// gains a handful of rows per backup.
func PruneTransfers(db *sql.DB) (int64, error) {
	res, err := db.Exec(
		`DELETE FROM transfer_usage WHERE at < datetime('now', ?)`,
		fmt.Sprintf("-%d days", transferRetentionDays),
	)
	if err != nil {
		return 0, fmt.Errorf("pruning transfer usage: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return n, nil
}
