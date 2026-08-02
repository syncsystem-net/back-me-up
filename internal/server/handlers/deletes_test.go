package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/syncsystem-net/back-me-up/internal/database"
	"github.com/syncsystem-net/back-me-up/internal/provider"
)

// stubProvider is a backend whose Delete outcome the test dictates. Only the
// methods the delete paths touch do anything; the rest satisfy the interface.
type stubProvider struct {
	name      string
	deleteErr error
	deletes   []string
}

func (s *stubProvider) Name() string                                { return s.name }
func (s *stubProvider) Login(context.Context, string, string) error { return nil }
func (s *stubProvider) Download(context.Context, string, io.Writer) error {
	return fmt.Errorf("not used")
}
func (s *stubProvider) List(context.Context) ([]provider.RemoteFile, error) { return nil, nil }
func (s *stubProvider) ReadRange(context.Context, string, []byte, int64) (int, error) {
	return 0, provider.ErrRangeUnsupported
}
func (s *stubProvider) FindByName(context.Context, string) (string, bool, error) {
	return "", false, nil
}
func (s *stubProvider) GetQuota(context.Context) (int64, int64, error) { return 0, 0, nil }
func (s *stubProvider) Upload(context.Context, string, string, func(provider.Progress)) (string, error) {
	return "", fmt.Errorf("not used")
}
func (s *stubProvider) Delete(_ context.Context, remoteRef string) error {
	s.deletes = append(s.deletes, remoteRef)
	return s.deleteErr
}

// deleteFixture is a record with two uploaded archives, one per provider, so a
// test can make exactly one of them fail and check the other was still tried.
type deleteFixture struct {
	db       *sql.DB
	h        *Handlers
	backupID int64
	megaJob  int64
	fourJob  int64
	// stubs is keyed by provider name; connectErr short-circuits Connect for a
	// provider, standing in for an unusable credential.
	stubs      map[string]*stubProvider
	connectErr map[string]error
}

func newDeleteFixture(t *testing.T) *deleteFixture {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "del.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	for _, p := range []string{"mega", "fourshared"} {
		if _, err := db.Exec(`INSERT INTO accounts (provider, email) VALUES (?, 'u@example.com')`, p); err != nil {
			t.Fatalf("seeding account: %v", err)
		}
	}

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	backupID, err := database.UpsertBackupForUser(tx, "u@example.com", "My Backup", `C:\src`)
	if err != nil {
		t.Fatalf("seeding backup: %v", err)
	}
	zipID, err := database.InsertZip(tx, backupID, "src.zip", `C:\src`, 100, `{"name":"src"}`)
	if err != nil {
		t.Fatalf("seeding zip: %v", err)
	}
	megaJob, err := database.InsertAdoptedJob(tx, backupID, zipID, 1, "src.zip", "mega-ref", 100)
	if err != nil {
		t.Fatalf("seeding mega job: %v", err)
	}
	fourJob, err := database.InsertAdoptedJob(tx, backupID, zipID, 2, "src.zip", "four-ref", 100)
	if err != nil {
		t.Fatalf("seeding 4shared job: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	f := &deleteFixture{
		db:         db,
		backupID:   backupID,
		megaJob:    megaJob,
		fourJob:    fourJob,
		stubs:      map[string]*stubProvider{"mega": {name: "mega"}, "fourshared": {name: "fourshared"}},
		connectErr: map[string]error{},
	}
	f.h = &Handlers{db: db}
	f.h.connect = func(_ context.Context, providerName, _ string) (provider.Provider, error) {
		if err := f.connectErr[providerName]; err != nil {
			return nil, err
		}
		return f.stubs[providerName], nil
	}
	return f
}

func (f *deleteFixture) deleteBackup(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodDelete, "/api/backups/"+strconv.FormatInt(f.backupID, 10), strings.NewReader(body))
	r.SetPathValue("id", strconv.FormatInt(f.backupID, 10))
	w := httptest.NewRecorder()
	f.h.DeleteBackup(w, r)
	return w
}

func (f *deleteFixture) deleteJob(t *testing.T, id int64, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodDelete, "/api/jobs/"+strconv.FormatInt(id, 10), strings.NewReader(body))
	r.SetPathValue("id", strconv.FormatInt(id, 10))
	w := httptest.NewRecorder()
	f.h.DeleteJob(w, r)
	return w
}

func (f *deleteFixture) recordExists(t *testing.T) bool {
	t.Helper()
	_, err := database.GetBackup(f.db, f.backupID)
	return err == nil
}

type failureBody struct {
	Error    string          `json:"error"`
	Deleted  int             `json:"deleted"`
	Failures []deleteFailure `json:"failures"`
}

func decodeFailures(t *testing.T, w *httptest.ResponseRecorder) failureBody {
	t.Helper()
	var b failureBody
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatalf("decoding response %q: %v", w.Body.String(), err)
	}
	return b
}

// An archive that is already gone is the end state a delete wants, so the record
// must still be removed. This is the mega node "not found" case from the field.
func TestDeleteBackupTreatsMissingRemoteAsDeleted(t *testing.T) {
	f := newDeleteFixture(t)
	f.stubs["mega"].deleteErr = fmt.Errorf("mega node %q: %w", "mega-ref", provider.ErrNotFound)
	f.stubs["fourshared"].deleteErr = fmt.Errorf("delete: %w", provider.ErrNotFound)

	w := f.deleteBackup(t, `{"confirm":"DELETE","delete_files":true}`)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body %s", w.Code, w.Body.String())
	}
	if f.recordExists(t) {
		t.Fatal("record still present after delete")
	}
}

// One unreachable account must not block the copies that are reachable, and the
// record survives so the user can decide what to do about what is left.
func TestDeleteBackupAttemptsEveryJobAndReportsFailures(t *testing.T) {
	f := newDeleteFixture(t)
	f.connectErr["fourshared"] = fmt.Errorf("4shared auth check failed: %w", provider.ErrAuthExpired)

	w := f.deleteBackup(t, `{"confirm":"DELETE","delete_files":true}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", w.Code, w.Body.String())
	}
	if got := f.stubs["mega"].deletes; len(got) != 1 || got[0] != "mega-ref" {
		t.Fatalf("mega deletes = %v, want the reachable copy to still be deleted", got)
	}
	body := decodeFailures(t, w)
	if body.Deleted != 1 {
		t.Fatalf("deleted = %d, want 1", body.Deleted)
	}
	if len(body.Failures) != 1 {
		t.Fatalf("failures = %d, want 1: %s", len(body.Failures), w.Body.String())
	}
	fail := body.Failures[0]
	if fail.Provider != "fourshared" || fail.Email != "u@example.com" || fail.Archive != "src.zip" {
		t.Fatalf("failure does not name provider/account/archive: %+v", fail)
	}
	if fail.Reason != reasonCredentials {
		t.Fatalf("reason = %q, want %q", fail.Reason, reasonCredentials)
	}
	// The user must be told to re-authorize, not shown a raw 401.0301.
	if !strings.Contains(fail.Message, "Re-authorize") || strings.Contains(fail.Message, "401.0301") {
		t.Fatalf("message is not actionable: %q", fail.Message)
	}
	if !f.recordExists(t) {
		t.Fatal("record was removed despite a failed remote delete")
	}
}

// A provider that refuses the delete for its own reasons is a different class
// from an unusable credential, because the remedy differs.
func TestDeleteBackupClassifiesProviderRefusal(t *testing.T) {
	f := newDeleteFixture(t)
	f.stubs["fourshared"].deleteErr = fmt.Errorf("delete: 4shared returned 403: forbidden")

	w := f.deleteBackup(t, `{"confirm":"DELETE","delete_files":true}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}
	body := decodeFailures(t, w)
	if len(body.Failures) != 1 || body.Failures[0].Reason != reasonProvider {
		t.Fatalf("want one %q failure, got %+v", reasonProvider, body.Failures)
	}
}

// The force step removes the local record only. It must not touch a provider —
// claiming a remote delete it did not perform would be a lie about what is left
// in the cloud.
func TestDeleteBackupForceRemovesRecordWithoutTouchingRemotes(t *testing.T) {
	f := newDeleteFixture(t)
	w := f.deleteBackup(t, `{"confirm":"DELETE","delete_files":true,"force":true}`)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body %s", w.Code, w.Body.String())
	}
	if f.recordExists(t) {
		t.Fatal("record still present after forced delete")
	}
	for name, s := range f.stubs {
		if len(s.deletes) != 0 {
			t.Fatalf("force path called Delete on %s: %v", name, s.deletes)
		}
	}
}

// Record-only delete keeps working: nothing remote is attempted.
func TestDeleteBackupWithoutFilesLeavesRemotesAlone(t *testing.T) {
	f := newDeleteFixture(t)
	w := f.deleteBackup(t, `{"delete_files":false}`)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body %s", w.Code, w.Body.String())
	}
	for name, s := range f.stubs {
		if len(s.deletes) != 0 {
			t.Fatalf("record-only delete called Delete on %s: %v", name, s.deletes)
		}
	}
}

func TestDeleteBackupRequiresConfirmationForFiles(t *testing.T) {
	f := newDeleteFixture(t)
	w := f.deleteBackup(t, `{"confirm":"delete","delete_files":true}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if !f.recordExists(t) {
		t.Fatal("record removed without confirmation")
	}
}

// Forcing still requires the typed confirmation, so a stray force flag cannot
// remove a record on its own.
func TestDeleteBackupForceStillRequiresConfirmation(t *testing.T) {
	f := newDeleteFixture(t)
	w := f.deleteBackup(t, `{"delete_files":true,"force":true}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if !f.recordExists(t) {
		t.Fatal("record removed without confirmation")
	}
}

func TestDeleteJobTreatsMissingRemoteAsDeleted(t *testing.T) {
	f := newDeleteFixture(t)
	f.stubs["mega"].deleteErr = fmt.Errorf("mega node %q: %w", "mega-ref", provider.ErrNotFound)

	w := f.deleteJob(t, f.megaJob, `{"confirm":"DELETE"}`)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body %s", w.Code, w.Body.String())
	}
	if _, err := database.GetJob(f.db, f.megaJob); err == nil {
		t.Fatal("job record still present")
	}
	// The sibling job is untouched — only this provider's copy is affected.
	if _, err := database.GetJob(f.db, f.fourJob); err != nil {
		t.Fatalf("sibling job was removed: %v", err)
	}
}

func TestDeleteJobReportsFailureWithoutRemovingRecord(t *testing.T) {
	f := newDeleteFixture(t)
	f.connectErr["fourshared"] = fmt.Errorf("4shared auth check failed: %w", provider.ErrAuthExpired)

	w := f.deleteJob(t, f.fourJob, `{"confirm":"DELETE"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", w.Code, w.Body.String())
	}
	body := decodeFailures(t, w)
	if len(body.Failures) != 1 || body.Failures[0].Archive != "src.zip" {
		t.Fatalf("failure does not name the archive: %+v", body.Failures)
	}
	if !strings.Contains(body.Failures[0].Message, "Re-authorize") {
		t.Fatalf("message is not actionable: %q", body.Failures[0].Message)
	}
	if _, err := database.GetJob(f.db, f.fourJob); err != nil {
		t.Fatalf("job record was removed despite the failure: %v", err)
	}
}

// A job that never uploaded has no remote file, so no provider is contacted and
// the record goes away regardless of whether the account is usable.
func TestDeleteJobWithoutRemoteFileSkipsProvider(t *testing.T) {
	f := newDeleteFixture(t)
	f.connectErr["mega"] = fmt.Errorf("boom")

	tx, err := f.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	zipID, err := database.InsertZip(tx, f.backupID, "pending.zip", `C:\src`, 10, `{"name":"src"}`)
	if err != nil {
		t.Fatalf("InsertZip: %v", err)
	}
	pendingJob, err := database.InsertJob(tx, f.backupID, zipID, 1, `C:\tmp\pending.zip`, "pending.zip", 10)
	if err != nil {
		t.Fatalf("InsertJob: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	w := f.deleteJob(t, pendingJob, `{"confirm":"DELETE"}`)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body %s", w.Code, w.Body.String())
	}
	if _, err := database.GetJob(f.db, pendingJob); err == nil {
		t.Fatal("job record still present")
	}
}

// A record whose jobs never uploaded must still delete with delete_files set —
// there is nothing remote to remove, so nothing can fail.
func TestDeleteBackupWithNoUploadedFiles(t *testing.T) {
	f := newDeleteFixture(t)
	if _, err := f.db.Exec(`UPDATE jobs SET remote_path = ''`); err != nil {
		t.Fatalf("clearing remote paths: %v", err)
	}
	f.connectErr["mega"] = fmt.Errorf("must not be called")
	f.connectErr["fourshared"] = fmt.Errorf("must not be called")

	w := f.deleteBackup(t, `{"confirm":"DELETE","delete_files":true}`)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body %s", w.Code, w.Body.String())
	}
	if f.recordExists(t) {
		t.Fatal("record still present")
	}
}
