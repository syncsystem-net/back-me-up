package database

import (
	"database/sql"
	"fmt"
	"strings"
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

// SearchBackupsByDirectory returns the distinct backups whose title, or one of
// whose subdirectory names (level 1 or 2), contains term, ordered newest first.
// The LEFT JOIN keeps title-only matches (backups with no recorded directories).
// The term's LIKE wildcards (% and _) and the escape char are escaped so it
// matches literally.
func SearchBackupsByDirectory(db *sql.DB, term string) ([]*Backup, error) {
	pattern := "%" + escapeLike(term) + "%"
	rows, err := db.Query(
		`SELECT DISTINCT b.id, b.title, b.source_path, b.created_at
		 FROM backups b
		 LEFT JOIN backup_directories d ON d.backup_id = b.id
		 WHERE b.title LIKE ? ESCAPE '\' OR d.name LIKE ? ESCAPE '\'
		 ORDER BY b.created_at DESC`,
		pattern, pattern,
	)
	if err != nil {
		return nil, fmt.Errorf("searching backups by directory: %w", err)
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

// escapeLike escapes the LIKE special characters (\, %, _) so a user's search
// term is matched literally rather than interpreted as wildcards. Used with
// `ESCAPE '\'`.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}
