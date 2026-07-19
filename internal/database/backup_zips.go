package database

import (
	"database/sql"
	"fmt"
	"time"
)

// Zip is one uploaded archive within a user's backup record. A record
// accumulates zips over time (one per Upload/Edit); TreeJSON maps the archive to
// the directory tree it contains. Jobs (one per account) reference a zip.
type Zip struct {
	ID         int64     `json:"id"`
	BackupID   int64     `json:"backup_id"`
	Name       string    `json:"name"`
	SourcePath string    `json:"source_path"`
	SizeBytes  int64     `json:"size_bytes"`
	TreeJSON   string    `json:"tree_json"`
	CreatedAt  time.Time `json:"created_at"`
}

// InsertZip records a new archive under a backup, returning its id (used as the
// jobs' zip_id).
func InsertZip(tx *sql.Tx, backupID int64, name, sourcePath string, sizeBytes int64, treeJSON string) (int64, error) {
	res, err := tx.Exec(
		`INSERT INTO backup_zips (backup_id, name, source_path, size_bytes, tree_json) VALUES (?, ?, ?, ?, ?)`,
		backupID, name, sourcePath, sizeBytes, treeJSON,
	)
	if err != nil {
		return 0, fmt.Errorf("inserting zip: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("getting last insert id: %w", err)
	}
	return id, nil
}

const zipColumns = `id, backup_id, name, source_path, size_bytes, COALESCE(tree_json, ''), created_at`

func scanZip(s interface{ Scan(...any) error }) (*Zip, error) {
	z := &Zip{}
	if err := s.Scan(&z.ID, &z.BackupID, &z.Name, &z.SourcePath, &z.SizeBytes, &z.TreeJSON, &z.CreatedAt); err != nil {
		return nil, err
	}
	return z, nil
}

// SearchableZip is one zip paired with the record it belongs to and the accounts
// that hold it, so a tree-JSON hit can be attributed back to a backup, a zip, and
// an account. Accounts come from the zip's jobs — a zip that never uploaded
// anywhere still appears, with an empty Accounts list.
type SearchableZip struct {
	ZipID      int64
	ZipName    string
	TreeJSON   string
	BackupID   int64
	Title      string
	OwnerEmail string
	Accounts   []string
}

// ListSearchableZips returns every recorded zip with its owning record and the
// provider/email pairs holding it. The whole set is loaded because matching is
// done in Go: exclude terms and search share accent-insensitive folding (see
// scanner.Fold), which SQL LIKE cannot do. One row per zip keeps this small.
func ListSearchableZips(db *sql.DB) ([]*SearchableZip, error) {
	rows, err := db.Query(
		`SELECT z.id, z.name, COALESCE(z.tree_json, ''), b.id, b.title, COALESCE(b.owner_email, '')
		 FROM backup_zips z
		 JOIN backups b ON b.id = z.backup_id
		 ORDER BY z.created_at DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("querying searchable zips: %w", err)
	}
	defer rows.Close()

	var zips []*SearchableZip
	byID := make(map[int64]*SearchableZip)
	for rows.Next() {
		z := &SearchableZip{Accounts: []string{}}
		if err := rows.Scan(&z.ZipID, &z.ZipName, &z.TreeJSON, &z.BackupID, &z.Title, &z.OwnerEmail); err != nil {
			return nil, fmt.Errorf("scanning searchable zip: %w", err)
		}
		zips = append(zips, z)
		byID[z.ZipID] = z
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	acctRows, err := db.Query(
		`SELECT DISTINCT j.zip_id, a.provider, a.email
		 FROM jobs j JOIN accounts a ON a.id = j.account_id
		 WHERE j.zip_id IS NOT NULL`,
	)
	if err != nil {
		return nil, fmt.Errorf("querying zip accounts: %w", err)
	}
	defer acctRows.Close()

	for acctRows.Next() {
		var zipID int64
		var provider, email string
		if err := acctRows.Scan(&zipID, &provider, &email); err != nil {
			return nil, fmt.Errorf("scanning zip account: %w", err)
		}
		if z, ok := byID[zipID]; ok {
			z.Accounts = append(z.Accounts, provider+" — "+email)
		}
	}
	return zips, acctRows.Err()
}

// ListZipsByBackup returns a record's archives, newest first.
func ListZipsByBackup(db *sql.DB, backupID int64) ([]*Zip, error) {
	rows, err := db.Query(`SELECT `+zipColumns+` FROM backup_zips WHERE backup_id = ? ORDER BY created_at DESC`, backupID)
	if err != nil {
		return nil, fmt.Errorf("querying zips: %w", err)
	}
	defer rows.Close()

	var zips []*Zip
	for rows.Next() {
		z, err := scanZip(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning zip row: %w", err)
		}
		zips = append(zips, z)
	}
	return zips, rows.Err()
}
