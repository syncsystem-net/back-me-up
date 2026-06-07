package database

import (
	"database/sql"
	"fmt"
	"time"
)

type Backup struct {
	ID         int64     `json:"id"`
	Title      string    `json:"title"`
	SourcePath string    `json:"source_path"`
	CreatedAt  time.Time `json:"created_at"`
}

func CreateBackup(tx *sql.Tx, title, sourcePath string) (int64, error) {
	res, err := tx.Exec(
		`INSERT INTO backups (title, source_path) VALUES (?, ?)`,
		title, sourcePath,
	)
	if err != nil {
		return 0, fmt.Errorf("inserting backup: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("getting last insert id: %w", err)
	}
	return id, nil
}

func GetBackup(db *sql.DB, id int64) (*Backup, error) {
	row := db.QueryRow(
		`SELECT id, title, source_path, created_at FROM backups WHERE id = ?`,
		id,
	)
	b := &Backup{}
	if err := row.Scan(&b.ID, &b.Title, &b.SourcePath, &b.CreatedAt); err != nil {
		return nil, fmt.Errorf("scanning backup: %w", err)
	}
	return b, nil
}

func ListBackups(db *sql.DB) ([]*Backup, error) {
	rows, err := db.Query(
		`SELECT id, title, source_path, created_at FROM backups ORDER BY created_at DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("querying backups: %w", err)
	}
	defer rows.Close()

	var backups []*Backup
	for rows.Next() {
		b := &Backup{}
		if err := rows.Scan(&b.ID, &b.Title, &b.SourcePath, &b.CreatedAt); err != nil {
			return nil, fmt.Errorf("scanning backup row: %w", err)
		}
		backups = append(backups, b)
	}
	return backups, rows.Err()
}
