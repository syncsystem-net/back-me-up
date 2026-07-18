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
