package database

import (
	"database/sql"
	"fmt"
	"time"
)

type Directory struct {
	ID        int64
	BackupID  int64
	Path      string
	Name      string
	Level     int
	SizeBytes int64
	CreatedAt time.Time
}

func InsertDirectories(tx *sql.Tx, backupID int64, dirs []Directory) error {
	stmt, err := tx.Prepare(
		`INSERT INTO backup_directories (backup_id, path, name, level, size_bytes) VALUES (?, ?, ?, ?, ?)`,
	)
	if err != nil {
		return fmt.Errorf("preparing insert statement: %w", err)
	}
	defer stmt.Close()

	for _, d := range dirs {
		if _, err := stmt.Exec(backupID, d.Path, d.Name, d.Level, d.SizeBytes); err != nil {
			return fmt.Errorf("inserting directory %s: %w", d.Path, err)
		}
	}
	return nil
}

func ListDirectoriesByBackup(db *sql.DB, backupID int64) ([]*Directory, error) {
	rows, err := db.Query(
		`SELECT id, backup_id, path, name, level, size_bytes, created_at
		 FROM backup_directories
		 WHERE backup_id = ?
		 ORDER BY level, name`,
		backupID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying directories: %w", err)
	}
	defer rows.Close()

	var dirs []*Directory
	for rows.Next() {
		d := &Directory{}
		if err := rows.Scan(&d.ID, &d.BackupID, &d.Path, &d.Name, &d.Level, &d.SizeBytes, &d.CreatedAt); err != nil {
			return nil, fmt.Errorf("scanning directory row: %w", err)
		}
		dirs = append(dirs, d)
	}
	return dirs, rows.Err()
}
