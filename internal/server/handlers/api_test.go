package handlers

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/syncsystem-net/back-me-up/internal/database"
)

// newAPITestDB opens a migrated database seeded with one user, one account, one
// backup record, and one zip whose recorded tree contains an accented directory.
func newAPITestDB(t *testing.T) (*sql.DB, int64) {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if _, err := db.Exec(`INSERT INTO accounts (provider, email) VALUES ('mega', 'u@example.com')`); err != nil {
		t.Fatalf("seeding account: %v", err)
	}

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	backupID, err := database.UpsertBackupForUser(tx, "u@example.com", "My Backup", `C:\src`)
	if err != nil {
		t.Fatalf("seeding backup: %v", err)
	}
	tree := `{"name":"src","children":[{"name":"docs","children":[{"name":"Conteúdo Antigo"}]}]}`
	zipID, err := database.InsertZip(tx, backupID, "src.zip", `C:\src`, 100, tree)
	if err != nil {
		t.Fatalf("seeding zip: %v", err)
	}
	if _, err := database.InsertJob(tx, backupID, zipID, 1, `C:\tmp\src.zip`, "src.zip", 100); err != nil {
		t.Fatalf("seeding job: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return db, backupID
}

func do(t *testing.T, h http.HandlerFunc, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	w := httptest.NewRecorder()
	h(w, r)
	return w
}

func TestSettingsEndpointRoundTrip(t *testing.T) {
	db, _ := newAPITestDB(t)

	w := do(t, GetSettingsHandler(db), "GET", "/api/settings", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET status = %d", w.Code)
	}
	var got struct {
		ExcludeTerms []string `json:"exclude_terms"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding GET: %v (body %s)", err, w.Body.String())
	}
	// A fresh install must return an empty ARRAY, not null — the frontend does
	// `s.exclude_terms || []` but an explicit [] keeps the contract honest.
	if got.ExcludeTerms == nil || len(got.ExcludeTerms) != 0 {
		t.Errorf("fresh settings = %#v, want empty slice", got.ExcludeTerms)
	}

	w = do(t, PutSettingsHandler(db), "PUT", "/api/settings", `{"exclude_terms":[" Layout ","","layout","conteudo"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT status = %d (%s)", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding PUT: %v", err)
	}
	// The PUT response is what the UI re-renders from, so it must already be
	// trimmed and de-duplicated.
	if strings.Join(got.ExcludeTerms, ",") != "Layout,conteudo" {
		t.Errorf("PUT response = %v, want [Layout conteudo]", got.ExcludeTerms)
	}

	w = do(t, GetSettingsHandler(db), "GET", "/api/settings", "")
	json.Unmarshal(w.Body.Bytes(), &got)
	if strings.Join(got.ExcludeTerms, ",") != "Layout,conteudo" {
		t.Errorf("terms did not persist, got %v", got.ExcludeTerms)
	}
}

func TestSearchEndpointAttributesMatches(t *testing.T) {
	db, _ := newAPITestDB(t)

	// Accent-insensitive: the stored name is "Conteúdo Antigo".
	w := do(t, SearchTreesHandler(db), "GET", "/api/search?q=conteudo", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var matches []searchMatch
	if err := json.Unmarshal(w.Body.Bytes(), &matches); err != nil {
		t.Fatalf("decoding: %v (body %s)", err, w.Body.String())
	}
	if len(matches) != 1 {
		t.Fatalf("got %d matches, want 1: %s", len(matches), w.Body.String())
	}
	m := matches[0]
	if m.Name != "Conteúdo Antigo" {
		t.Errorf("Name = %q", m.Name)
	}
	if m.Path != "src/docs/Conteúdo Antigo" {
		t.Errorf("Path = %q", m.Path)
	}
	// The whole point of the feature: attribute the hit to a record, a zip, and
	// the account holding it.
	if m.Title != "My Backup" || m.OwnerEmail != "u@example.com" || m.ZipName != "src.zip" {
		t.Errorf("attribution wrong: %+v", m)
	}
	if len(m.Accounts) != 1 || !strings.Contains(m.Accounts[0], "mega") {
		t.Errorf("Accounts = %v, want the mega account", m.Accounts)
	}
}

func TestSearchEndpointEmptyAndNoMatch(t *testing.T) {
	db, _ := newAPITestDB(t)

	for _, q := range []string{"", "zzz-nothing"} {
		w := do(t, SearchTreesHandler(db), "GET", "/api/search?q="+q, "")
		if w.Code != http.StatusOK {
			t.Fatalf("q=%q status = %d", q, w.Code)
		}
		// Must be `[]`, never `null` — the frontend iterates the result directly.
		if body := strings.TrimSpace(w.Body.String()); body != "[]" {
			t.Errorf("q=%q body = %s, want []", q, body)
		}
	}
}

func TestPatchBackupRenames(t *testing.T) {
	db, backupID := newAPITestDB(t)

	h := PatchBackupHandler(db)

	r := httptest.NewRequest("PATCH", "/api/backups/"+strconv.FormatInt(backupID, 10), strings.NewReader(`{"title":"Renamed"}`))
	r.SetPathValue("id", strconv.FormatInt(backupID, 10))
	w := httptest.NewRecorder()
	h(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d (%s)", w.Code, w.Body.String())
	}

	b, err := database.GetBackup(db, backupID)
	if err != nil {
		t.Fatalf("GetBackup: %v", err)
	}
	if b.Title != "Renamed" {
		t.Errorf("title = %q, want Renamed", b.Title)
	}

	// A rename must not disturb the record's zips — the tree survives the edit.
	zips, err := database.ListZipsByBackup(db, backupID)
	if err != nil {
		t.Fatalf("ListZipsByBackup: %v", err)
	}
	if len(zips) != 1 || !strings.Contains(zips[0].TreeJSON, "Conteúdo") {
		t.Errorf("expected the zip and its tree to survive a rename, got %d zips", len(zips))
	}
}

func TestPatchBackupRejectsBlankTitle(t *testing.T) {
	db, backupID := newAPITestDB(t)

	r := httptest.NewRequest("PATCH", "/api/backups/1", strings.NewReader(`{"title":"   "}`))
	r.SetPathValue("id", strconv.FormatInt(backupID, 10))
	w := httptest.NewRecorder()
	PatchBackupHandler(db)(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestPatchBackupMissingRecord(t *testing.T) {
	db, _ := newAPITestDB(t)

	r := httptest.NewRequest("PATCH", "/api/backups/9999", strings.NewReader(`{"title":"x"}`))
	r.SetPathValue("id", "9999")
	w := httptest.NewRecorder()
	PatchBackupHandler(db)(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}
