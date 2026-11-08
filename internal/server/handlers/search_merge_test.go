package handlers

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/syncsystem-net/back-me-up/internal/database"
)

// The production shape that motivated the merge: bkup001.zip uploaded to MEGA
// (complete) and 4shared (failed), then re-uploaded to 4shared alone, which
// appended a second zip row of the same name. Search must report the archive
// once, credited to the accounts that actually hold it — both, via different
// rows — and never to the failed attempt on its own.
func TestSearchMergesSameNamedZipsAndCreditsOnlyCompletedCopies(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "search.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	for _, p := range []string{"mega", "fourshared"} {
		if _, err := db.Exec(`INSERT INTO accounts (provider, email) VALUES (?, 'u@example.com')`, p); err != nil {
			t.Fatalf("seeding account: %v", err)
		}
	}
	const megaAcct, fourAcct = 1, 2
	const tree = `{"name":"bkup001","children":[{"name":"Faturas"}]}`

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	backupID, err := database.UpsertBackupForUser(tx, "u@example.com", "bkup001 - bkup030", `C:\src`)
	if err != nil {
		t.Fatalf("seeding backup: %v", err)
	}
	first, err := database.InsertZip(tx, backupID, "bkup001.zip", `C:\src`, 100, tree)
	if err != nil {
		t.Fatalf("seeding first zip: %v", err)
	}
	if _, err := database.InsertAdoptedJob(tx, backupID, first, megaAcct, "bkup001.zip", "mega-ref", 100); err != nil {
		t.Fatalf("seeding mega job: %v", err)
	}
	failedJob, err := database.InsertJob(tx, backupID, first, fourAcct, `C:\bkup001.zip`, "bkup001.zip", 100)
	if err != nil {
		t.Fatalf("seeding failed 4shared job: %v", err)
	}
	second, err := database.InsertZip(tx, backupID, "bkup001.zip", `C:\src`, 100, tree)
	if err != nil {
		t.Fatalf("seeding second zip: %v", err)
	}
	if _, err := database.InsertAdoptedJob(tx, backupID, second, fourAcct, "bkup001.zip", "four-ref", 100); err != nil {
		t.Fatalf("seeding 4shared re-upload: %v", err)
	}
	// A third archive whose only upload failed: it is still searchable (the tree
	// is recorded), but no account holds it.
	orphan, err := database.InsertZip(tx, backupID, "bkup002.zip", `C:\src2`, 100,
		`{"name":"bkup002","children":[{"name":"faturas 2"}]}`)
	if err != nil {
		t.Fatalf("seeding orphan zip: %v", err)
	}
	orphanJob, err := database.InsertJob(tx, backupID, orphan, megaAcct, `C:\bkup002.zip`, "bkup002.zip", 100)
	if err != nil {
		t.Fatalf("seeding orphan job: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	for _, id := range []int64{failedJob, orphanJob} {
		if err := database.FailJob(db, id, "boom"); err != nil {
			t.Fatalf("FailJob: %v", err)
		}
	}

	w := do(t, SearchTreesHandler(db), "GET", "/api/search?q=faturas", "")
	if w.Code != 200 {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var got []searchMatch
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding %s: %v", w.Body.String(), err)
	}

	byZip := map[string][]searchMatch{}
	for _, m := range got {
		byZip[m.ZipName] = append(byZip[m.ZipName], m)
	}
	if n := len(byZip["bkup001.zip"]); n != 1 {
		t.Fatalf("bkup001.zip matched %d times, want once: %+v", n, got)
	}
	m := byZip["bkup001.zip"][0]
	if accts := strings.Join(m.Accounts, ","); accts != "fourshared — u@example.com,mega — u@example.com" {
		t.Errorf("accounts = %q, want both completed copies in sorted order", accts)
	}

	if n := len(byZip["bkup002.zip"]); n != 1 {
		t.Fatalf("bkup002.zip matched %d times, want once: %+v", n, got)
	}
	if accts := byZip["bkup002.zip"][0].Accounts; accts == nil || len(accts) != 0 {
		t.Errorf("an archive with only a failed upload is credited to %v, want []", accts)
	}
}

// The failed attempt on the first row must not be what credits 4shared: with
// the re-upload gone, only MEGA holds the archive.
func TestSearchIgnoresAFailedUploadOnTheSameRow(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "search.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for _, p := range []string{"mega", "fourshared"} {
		if _, err := db.Exec(`INSERT INTO accounts (provider, email) VALUES (?, 'u@example.com')`, p); err != nil {
			t.Fatalf("seeding account: %v", err)
		}
	}
	tx, _ := db.Begin()
	backupID, _ := database.UpsertBackupForUser(tx, "u@example.com", "t", `C:\src`)
	zipID, _ := database.InsertZip(tx, backupID, "bkup001.zip", `C:\src`, 100, `{"name":"bkup001"}`)
	if _, err := database.InsertAdoptedJob(tx, backupID, zipID, 1, "bkup001.zip", "mega-ref", 100); err != nil {
		t.Fatalf("seeding mega job: %v", err)
	}
	failed, err := database.InsertJob(tx, backupID, zipID, 2, `C:\bkup001.zip`, "bkup001.zip", 100)
	if err != nil {
		t.Fatalf("seeding 4shared job: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := database.FailJob(db, failed, "401"); err != nil {
		t.Fatalf("FailJob: %v", err)
	}

	w := do(t, SearchTreesHandler(db), "GET", "/api/search?q=bkup001", "")
	var got []searchMatch
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding %s: %v", w.Body.String(), err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d matches, want 1: %+v", len(got), got)
	}
	if accts := strings.Join(got[0].Accounts, ","); accts != "mega — u@example.com" {
		t.Errorf("accounts = %q, want only the completed MEGA copy", accts)
	}
}
