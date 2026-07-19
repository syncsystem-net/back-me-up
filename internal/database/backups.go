package database

import (
	"database/sql"
	"fmt"
	"time"
)

type Backup struct {
	ID         int64     `json:"id"`
	OwnerEmail string    `json:"owner_email"`
	Title      string    `json:"title"`
	SourcePath string    `json:"source_path"`
	CreatedAt  time.Time `json:"created_at"`
}

const backupColumns = `id, COALESCE(owner_email, ''), title, source_path, created_at`

func scanBackup(s interface{ Scan(...any) error }) (*Backup, error) {
	b := &Backup{}
	if err := s.Scan(&b.ID, &b.OwnerEmail, &b.Title, &b.SourcePath, &b.CreatedAt); err != nil {
		return nil, err
	}
	return b, nil
}

// UpsertBackupForUser returns the id of ownerEmail's single backup record,
// creating it if absent. A record belongs to one user and accumulates many zips
// over time (see backup_zips). When title is non-empty it is (re)set, so an
// Upload/Edit can rename the record. sourcePath records the most recent source.
func UpsertBackupForUser(tx *sql.Tx, ownerEmail, title, sourcePath string) (int64, error) {
	var id int64
	err := tx.QueryRow(`SELECT id FROM backups WHERE owner_email = ?`, ownerEmail).Scan(&id)
	switch {
	case err == sql.ErrNoRows:
		res, err := tx.Exec(
			`INSERT INTO backups (owner_email, title, source_path) VALUES (?, ?, ?)`,
			ownerEmail, title, sourcePath,
		)
		if err != nil {
			return 0, fmt.Errorf("inserting backup: %w", err)
		}
		return res.LastInsertId()
	case err != nil:
		return 0, fmt.Errorf("looking up backup for user: %w", err)
	}
	if title != "" {
		if _, err := tx.Exec(`UPDATE backups SET title = ?, source_path = ? WHERE id = ?`, title, sourcePath, id); err != nil {
			return 0, fmt.Errorf("updating backup: %w", err)
		}
	}
	return id, nil
}

func GetBackup(db *sql.DB, id int64) (*Backup, error) {
	b, err := scanBackup(db.QueryRow(`SELECT `+backupColumns+` FROM backups WHERE id = ?`, id))
	if err != nil {
		return nil, fmt.Errorf("scanning backup: %w", err)
	}
	return b, nil
}

func ListBackups(db *sql.DB) ([]*Backup, error) {
	rows, err := db.Query(`SELECT ` + backupColumns + ` FROM backups ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("querying backups: %w", err)
	}
	defer rows.Close()

	var backups []*Backup
	for rows.Next() {
		b, err := scanBackup(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning backup row: %w", err)
		}
		backups = append(backups, b)
	}
	return backups, rows.Err()
}

// GetBackupByOwner returns ownerEmail's backup record, or (nil, nil) when the
// user has never created one (they still appear as an empty row in the UI).
func GetBackupByOwner(db *sql.DB, ownerEmail string) (*Backup, error) {
	b, err := scanBackup(db.QueryRow(`SELECT `+backupColumns+` FROM backups WHERE owner_email = ?`, ownerEmail))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scanning backup by owner: %w", err)
	}
	return b, nil
}

// DeleteBackupRecord removes a backup record and (via ON DELETE CASCADE) its
// zips, jobs, directories, and job logs. The caller is responsible for deleting
// any remote files first when the user chose "delete files too".
func DeleteBackupRecord(db *sql.DB, id int64) error {
	if _, err := db.Exec(`DELETE FROM backups WHERE id = ?`, id); err != nil {
		return fmt.Errorf("deleting backup record: %w", err)
	}
	return nil
}

// UpdateBackupTitle renames a record without touching its zips or jobs, backing
// the title-only save in the Upload/Edit modal. Returns sql.ErrNoRows when no
// record has that id.
func UpdateBackupTitle(db *sql.DB, id int64, title string) error {
	res, err := db.Exec(`UPDATE backups SET title = ? WHERE id = ?`, title, id)
	if err != nil {
		return fmt.Errorf("updating backup title: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking updated rows: %w", err)
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
