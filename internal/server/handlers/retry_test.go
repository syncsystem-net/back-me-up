package handlers

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/syncsystem-net/back-me-up/internal/accounts"
	"github.com/syncsystem-net/back-me-up/internal/database"
)

// retryFixture is a record with one archive queued to one account, the way an
// upload leaves it: a real temp zip on disk whose size is the job's total_bytes.
type retryFixture struct {
	db       *sql.DB
	h        *Handlers
	backupID int64
	zipID    int64
	zipPath  string
	jobID    int64
}

const retryZipBytes = 64

func newRetryFixture(t *testing.T) *retryFixture {
	t.Helper()
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "retry.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if _, err := db.Exec(`INSERT INTO accounts (provider, email) VALUES ('fourshared', 'u@example.com')`); err != nil {
		t.Fatalf("seeding account: %v", err)
	}
	zipPath := filepath.Join(dir, "bkup001.zip")
	if err := os.WriteFile(zipPath, make([]byte, retryZipBytes), 0o644); err != nil {
		t.Fatalf("writing temp zip: %v", err)
	}

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	backupID, err := database.UpsertBackupForUser(tx, "u@example.com", "My Backup", dir)
	if err != nil {
		t.Fatalf("seeding backup: %v", err)
	}
	zipID, err := database.InsertZip(tx, backupID, "bkup001.zip", dir, retryZipBytes, `{"name":"bkup001"}`)
	if err != nil {
		t.Fatalf("seeding zip: %v", err)
	}
	jobID, err := database.InsertJob(tx, backupID, zipID, 1, zipPath, "bkup001.zip", retryZipBytes)
	if err != nil {
		t.Fatalf("seeding job: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Leave the row the way a real failure does: part-way through, with an error
	// recorded and both timestamps set.
	if _, err := db.Exec(`UPDATE jobs SET started_at = CURRENT_TIMESTAMP, uploaded_bytes = 32,
		chunks_uploaded = 1, chunks_total = 2, hold_reason = 'stale hold' WHERE id = ?`, jobID); err != nil {
		t.Fatalf("seeding progress: %v", err)
	}
	if err := database.FailJob(db, jobID, "401 token expired"); err != nil {
		t.Fatalf("FailJob: %v", err)
	}

	return &retryFixture{
		db:       db,
		h:        &Handlers{db: db, accounts: accounts.NewStore(nil, nil, accounts.OAuthApp{})},
		backupID: backupID,
		zipID:    zipID,
		zipPath:  zipPath,
		jobID:    jobID,
	}
}

func (f *retryFixture) retry(t *testing.T, id int64) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/jobs/"+strconv.FormatInt(id, 10)+"/retry", nil)
	r.SetPathValue("id", strconv.FormatInt(id, 10))
	w := httptest.NewRecorder()
	f.h.RetryJob(w, r)
	return w
}

// addLaterJob queues another job for the same archive on the same account — a
// re-upload of the directory — and leaves it in the given state.
func (f *retryFixture) addLaterJob(t *testing.T, status string) int64 {
	t.Helper()
	tx, err := f.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	zipID, err := database.InsertZip(tx, f.backupID, "bkup001.zip", "", retryZipBytes, `{"name":"bkup001"}`)
	if err != nil {
		t.Fatalf("seeding second zip: %v", err)
	}
	id, err := database.InsertJob(tx, f.backupID, zipID, 1, f.zipPath+".2", "bkup001.zip", retryZipBytes)
	if err != nil {
		t.Fatalf("seeding later job: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if _, err := f.db.Exec(`UPDATE jobs SET status = ? WHERE id = ?`, status, id); err != nil {
		t.Fatalf("setting later job status: %v", err)
	}
	return id
}

func wantConflict(t *testing.T, w *httptest.ResponseRecorder, fragment string) {
	t.Helper()
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), fragment) {
		t.Errorf("body %s does not mention %q", w.Body.String(), fragment)
	}
}

func TestRetryRequeuesAFailedJob(t *testing.T) {
	f := newRetryFixture(t)

	w := f.retry(t, f.jobID)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var got database.Job
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding %s: %v", w.Body.String(), err)
	}
	if got.Status != "pending" {
		t.Errorf("status = %q, want pending", got.Status)
	}
	if got.UploadedBytes != 0 || got.ChunksUploaded != 0 || got.ChunksTotal != 0 {
		t.Errorf("progress not reset: %+v", got)
	}
	if got.ErrorMessage != "" || got.HoldReason != "" {
		t.Errorf("error/hold not cleared: error=%q hold=%q", got.ErrorMessage, got.HoldReason)
	}
	if got.StartedAt != nil || got.CompletedAt != nil {
		t.Errorf("timestamps not cleared: started=%v completed=%v", got.StartedAt, got.CompletedAt)
	}
	// The retried upload must stay attached to the same zip row and reuse the
	// same temp archive, or the record's tree grows a duplicate node.
	if got.ZipID != f.zipID || got.ZipPath != f.zipPath {
		t.Errorf("zip changed: zip_id=%d path=%q, want %d %q", got.ZipID, got.ZipPath, f.zipID, f.zipPath)
	}
	var zips int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM backup_zips WHERE backup_id = ?`, f.backupID).Scan(&zips); err != nil {
		t.Fatalf("counting zips: %v", err)
	}
	if zips != 1 {
		t.Errorf("zip rows = %d, want 1", zips)
	}

	logs, err := database.ListJobLogs(f.db, f.jobID)
	if err != nil {
		t.Fatalf("ListJobLogs: %v", err)
	}
	found := false
	for _, l := range logs {
		if l.Level == "info" && strings.Contains(l.Message, "retry requested") {
			found = true
		}
	}
	if !found {
		t.Errorf("no retry log row among %d logs", len(logs))
	}
}

// A double click must not queue the job twice: the second request finds it
// pending and is refused.
func TestRetryTwiceIsRefusedTheSecondTime(t *testing.T) {
	f := newRetryFixture(t)
	if w := f.retry(t, f.jobID); w.Code != http.StatusOK {
		t.Fatalf("first retry: %d %s", w.Code, w.Body.String())
	}
	wantConflict(t, f.retry(t, f.jobID), "pending")
}

func TestRetryRefusesAJobThatIsNotFailed(t *testing.T) {
	f := newRetryFixture(t)
	if _, err := f.db.Exec(`UPDATE jobs SET status = 'complete' WHERE id = ?`, f.jobID); err != nil {
		t.Fatalf("marking complete: %v", err)
	}
	wantConflict(t, f.retry(t, f.jobID), "only a failed job")
}

// A failed job with a later job for the same archive on the same account has
// been superseded, and the refusal names the job that superseded it.
func TestRetryRefusesASupersededJob(t *testing.T) {
	cases := map[string]string{
		"complete":    "already uploaded",
		"in_progress": "already being uploaded",
		"pending":     "already being uploaded",
		"failed":      "instead",
	}
	for status, fragment := range cases {
		t.Run(status, func(t *testing.T) {
			f := newRetryFixture(t)
			later := f.addLaterJob(t, status)
			w := f.retry(t, f.jobID)
			wantConflict(t, w, fragment)
			if !strings.Contains(w.Body.String(), "#"+strconv.FormatInt(later, 10)) {
				t.Errorf("body %s does not name job #%d", w.Body.String(), later)
			}
			job, err := database.GetJob(f.db, f.jobID)
			if err != nil {
				t.Fatalf("GetJob: %v", err)
			}
			if job.Status != "failed" {
				t.Errorf("refused retry changed status to %q", job.Status)
			}
		})
	}
}

func TestRetryRefusesWhenTheTempArchiveIsGone(t *testing.T) {
	f := newRetryFixture(t)
	if err := os.Remove(f.zipPath); err != nil {
		t.Fatalf("removing temp zip: %v", err)
	}
	wantConflict(t, f.retry(t, f.jobID), "is gone")
}

// A temp zip rebuilt from a directory that changed since the job was queued is a
// different archive; uploading it under this job would record the wrong thing.
func TestRetryRefusesWhenTheTempArchiveChangedSize(t *testing.T) {
	f := newRetryFixture(t)
	if err := os.WriteFile(f.zipPath, make([]byte, retryZipBytes+1), 0o644); err != nil {
		t.Fatalf("rewriting temp zip: %v", err)
	}
	wantConflict(t, f.retry(t, f.jobID), "has changed")
}

func TestRetryUnknownJobIs404(t *testing.T) {
	f := newRetryFixture(t)
	if w := f.retry(t, f.jobID+100); w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", w.Code, w.Body.String())
	}
}

func TestRetryRefusedWhileLocked(t *testing.T) {
	const reason = "No credential passphrase is set."
	f := newRetryFixture(t)
	store := accounts.NewStore(nil, nil, accounts.OAuthApp{})
	store.Lock(reason)
	f.h.accounts = store

	w := f.retry(t, f.jobID)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), reason) {
		t.Errorf("body does not carry the lock reason: %s", w.Body.String())
	}
	job, err := database.GetJob(f.db, f.jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.Status != "failed" {
		t.Errorf("locked retry changed status to %q", job.Status)
	}
}
