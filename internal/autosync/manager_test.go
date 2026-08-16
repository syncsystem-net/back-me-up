package autosync

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/syncsystem-net/back-me-up/internal/accounts"
	"github.com/syncsystem-net/back-me-up/internal/database"
	"github.com/syncsystem-net/back-me-up/internal/provider"
	"github.com/syncsystem-net/back-me-up/internal/scanner"
)

// stubBackend is a fakeProvider plus the listing and failure behaviour a crawl
// exercises.
type stubBackend struct {
	*fakeProvider
	files   []provider.RemoteFile
	listErr error
}

func (s *stubBackend) List(context.Context) ([]provider.RemoteFile, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.files, nil
}

// archiveBytes builds a small real archive laid out the way this tool writes
// them (everything under a directory named after the source folder).
func archiveBytes(t *testing.T, base string, paths []string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, p := range paths {
		w, err := zw.Create(base + "/" + p)
		if err != nil {
			t.Fatalf("creating %s: %v", p, err)
		}
		if _, err := w.Write([]byte("data")); err != nil {
			t.Fatalf("writing %s: %v", p, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("closing zip: %v", err)
	}
	return buf.Bytes()
}

// newManagerForTest wires a Manager to a real (migrated, temp) database and a
// set of stub backends keyed by "provider|email".
func newManagerForTest(t *testing.T, backends map[string]*stubBackend, accts []accounts.Account) *Manager {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("opening database: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	for _, a := range accts {
		if _, err := database.UpsertAccountRow(db, database.AccountRow{Provider: string(a.Provider), Email: a.Email, QuotaGB: 20}); err != nil {
			t.Fatalf("UpsertAccount: %v", err)
		}
	}

	m := New(db, accounts.NewStore(nil, accts, accounts.OAuthApp{}), 1<<20, 3)
	m.connect = func(_ context.Context, _ *accounts.AccountStore, providerName, email string, _ int64) (provider.Provider, error) {
		b, ok := backends[providerName+"|"+email]
		if !ok {
			return nil, errors.New("no stub backend for this account")
		}
		return b, nil
	}
	return m
}

// waitFor blocks until the run reaches one of the given phases, so tests do not
// race the background goroutine.
func waitFor(t *testing.T, m *Manager, phases ...string) Run {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		run := m.Snapshot()
		for _, p := range phases {
			if run.Phase == p {
				return run
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for phase %v (last: %s)", phases, m.Snapshot().Phase)
	return Run{}
}

// The headline flow: a user with archives already sitting in MEGA gets them
// discovered, previewed, adopted with a real directory tree, and a second run
// then proposes nothing.
func TestPreviewApplyAndIdempotentRerun(t *testing.T) {
	data := archiveBytes(t, "bkup007", []string{"top.txt", "docs/report.pdf", "docs/img/a.jpg"})
	backend := &stubBackend{
		fakeProvider: &fakeProvider{data: data, rangeSupported: true},
		files: []provider.RemoteFile{
			{ID: "ref-1", Name: "bkup007.zip", Size: int64(len(data))},
			{ID: "ref-2", Name: "not-an-archive.txt", Size: 10},
		},
	}
	accts := []accounts.Account{{Provider: accounts.ProviderMega, Email: "user@example.com"}}
	m := newManagerForTest(t, map[string]*stubBackend{"mega|user@example.com": backend}, accts)

	// --- Dry run: proposes, writes nothing. ---
	if err := m.StartPreview(); err != nil {
		t.Fatalf("StartPreview: %v", err)
	}
	run := waitFor(t, m, PhasePreviewReady)

	if len(run.Accounts) != 1 {
		t.Fatalf("expected one account plan, got %d", len(run.Accounts))
	}
	plan := run.Accounts[0]
	if plan.Error != "" {
		t.Fatalf("unexpected account error: %s", plan.Error)
	}
	if len(plan.Proposed) != 1 || plan.Proposed[0].Action != ActionAdopt {
		t.Fatalf("expected one adopt proposal (the .txt must be ignored), got %+v", plan.Proposed)
	}

	backup, err := database.GetBackupByOwner(m.db, "user@example.com")
	if err != nil {
		t.Fatalf("GetBackupByOwner: %v", err)
	}
	if backup != nil {
		t.Fatal("the dry run must not have written a backup record")
	}

	// --- Apply. ---
	if err := m.StartApply(); err != nil {
		t.Fatalf("StartApply: %v", err)
	}
	run = waitFor(t, m, PhaseApplied)
	if run.Accounts[0].Applied != 1 {
		t.Fatalf("expected 1 applied change, got %d (note: %q)",
			run.Accounts[0].Applied, run.Accounts[0].Proposed[0].Note)
	}

	backup, err = database.GetBackupByOwner(m.db, "user@example.com")
	if err != nil || backup == nil {
		t.Fatalf("expected a backup record to have been created: %v", err)
	}
	zips, err := database.ListZipsByBackup(m.db, backup.ID)
	if err != nil {
		t.Fatalf("ListZipsByBackup: %v", err)
	}
	if len(zips) != 1 || zips[0].Name != "bkup007.zip" {
		t.Fatalf("unexpected zips: %+v", zips)
	}
	// Adopted rows carry no local directory.
	if zips[0].SourcePath != "" {
		t.Errorf("source_path = %q, want empty for a discovered archive", zips[0].SourcePath)
	}

	// The tree must have been read out of the archive's central directory, in the
	// shape the existing UI and global search already understand.
	var root scanner.Node
	if err := json.Unmarshal([]byte(zips[0].TreeJSON), &root); err != nil {
		t.Fatalf("tree_json is not a scanner tree: %v (%q)", err, zips[0].TreeJSON)
	}
	if root.Name != "bkup007" {
		t.Fatalf("tree root = %q, want %q", root.Name, "bkup007")
	}
	docs := child(t, &root, "docs")
	child(t, docs, "img")

	// The synthetic job makes it downloadable.
	jobs, err := database.ListJobsByBackup(m.db, backup.ID)
	if err != nil {
		t.Fatalf("ListJobsByBackup: %v", err)
	}
	if len(jobs) != 1 || jobs[0].Status != "complete" || jobs[0].RemotePath != "ref-1" {
		t.Fatalf("expected one complete job holding the remote ref, got %+v", jobs)
	}

	// --- Re-run: nothing left to do. ---
	if err := m.StartPreview(); err != nil {
		t.Fatalf("second StartPreview: %v", err)
	}
	run = waitFor(t, m, PhasePreviewReady)
	if n := len(run.Accounts[0].Proposed); n != 0 {
		t.Fatalf("second run must propose nothing, got %d: %+v", n, run.Accounts[0].Proposed)
	}
	if len(run.Accounts[0].Matched) != 1 {
		t.Errorf("expected the archive reported as in sync, got %+v", run.Accounts[0].Matched)
	}
}

// One account failing to list must not cost the user the others, and it must
// read as an error rather than as an empty account.
func TestPreviewDegradesPerAccount(t *testing.T) {
	data := archiveBytes(t, "good", []string{"a.txt"})
	backends := map[string]*stubBackend{
		"fourshared|user@example.com": {
			fakeProvider: &fakeProvider{},
			listErr:      errors.New("401.0301 token expired, rejected or does not exist"),
		},
		"mega|user@example.com": {
			fakeProvider: &fakeProvider{data: data, rangeSupported: true},
			files:        []provider.RemoteFile{{ID: "ref-1", Name: "good.zip", Size: int64(len(data))}},
		},
	}
	accts := []accounts.Account{
		{Provider: accounts.ProviderFourShared, Email: "user@example.com"},
		{Provider: accounts.ProviderMega, Email: "user@example.com"},
	}
	m := newManagerForTest(t, backends, accts)

	if err := m.StartPreview(); err != nil {
		t.Fatalf("StartPreview: %v", err)
	}
	run := waitFor(t, m, PhasePreviewReady)

	if len(run.Accounts) != 2 {
		t.Fatalf("both accounts must appear in the preview, got %d", len(run.Accounts))
	}
	var failed, ok *AccountPlan
	for _, p := range run.Accounts {
		if p.Provider == "fourshared" {
			failed = p
		} else {
			ok = p
		}
	}
	if failed.Error == "" {
		t.Error("the failing account must report an error, never look empty")
	}
	if len(failed.Proposed) != 0 {
		t.Errorf("a failed listing must propose nothing, got %+v", failed.Proposed)
	}
	if len(ok.Proposed) != 1 {
		t.Errorf("the healthy account must still be scanned, got %+v", ok.Proposed)
	}
}

// A provider that cannot serve ranges still yields a correct tree, via the
// whole-archive download fallback rather than a guess at the offsets.
func TestApplyFallsBackToFullDownloadWhenRangesUnsupported(t *testing.T) {
	data := archiveBytes(t, "nr", []string{"x.txt", "sub/y.txt"})
	backend := &stubBackend{
		fakeProvider: &fakeProvider{data: data, rangeSupported: false},
		files:        []provider.RemoteFile{{ID: "ref-1", Name: "nr.zip", Size: int64(len(data))}},
	}
	accts := []accounts.Account{{Provider: accounts.ProviderFourShared, Email: "u@example.com"}}
	m := newManagerForTest(t, map[string]*stubBackend{"fourshared|u@example.com": backend}, accts)

	if err := m.StartPreview(); err != nil {
		t.Fatalf("StartPreview: %v", err)
	}
	waitFor(t, m, PhasePreviewReady)
	if err := m.StartApply(); err != nil {
		t.Fatalf("StartApply: %v", err)
	}
	run := waitFor(t, m, PhaseApplied)

	if run.Accounts[0].Applied != 1 {
		t.Fatalf("expected the archive to be adopted, got %d (note %q)",
			run.Accounts[0].Applied, run.Accounts[0].Proposed[0].Note)
	}
	if backend.downloads != 1 {
		t.Errorf("expected exactly one full download as the fallback, got %d", backend.downloads)
	}

	backup, _ := database.GetBackupByOwner(m.db, "u@example.com")
	zips, _ := database.ListZipsByBackup(m.db, backup.ID)
	var root scanner.Node
	if err := json.Unmarshal([]byte(zips[0].TreeJSON), &root); err != nil {
		t.Fatalf("tree_json: %v", err)
	}
	// The point: the fallback produced the RIGHT tree, not plausible garbage.
	if root.Name != "nr" {
		t.Fatalf("root = %q, want %q", root.Name, "nr")
	}
	child(t, &root, "sub")
}

// Exclude terms and the depth cap apply to a discovered tree exactly as they do
// to an uploaded one.
func TestApplyHonoursExcludeTermsAndDepth(t *testing.T) {
	data := archiveBytes(t, "cfg", []string{
		"keep/a.txt",
		"Layouts/b.txt",
		"deep/one/two/three/c.txt",
	})
	backend := &stubBackend{
		fakeProvider: &fakeProvider{data: data, rangeSupported: true},
		files:        []provider.RemoteFile{{ID: "ref-1", Name: "cfg.zip", Size: int64(len(data))}},
	}
	accts := []accounts.Account{{Provider: accounts.ProviderMega, Email: "u@example.com"}}
	m := newManagerForTest(t, map[string]*stubBackend{"mega|u@example.com": backend}, accts)

	if err := database.SetExcludeTerms(m.db, []string{"layout"}); err != nil {
		t.Fatalf("SetExcludeTerms: %v", err)
	}
	m.scanMaxDepth = 2

	if err := m.StartPreview(); err != nil {
		t.Fatalf("StartPreview: %v", err)
	}
	waitFor(t, m, PhasePreviewReady)
	if err := m.StartApply(); err != nil {
		t.Fatalf("StartApply: %v", err)
	}
	waitFor(t, m, PhaseApplied)

	backup, _ := database.GetBackupByOwner(m.db, "u@example.com")
	zips, _ := database.ListZipsByBackup(m.db, backup.ID)
	var root scanner.Node
	if err := json.Unmarshal([]byte(zips[0].TreeJSON), &root); err != nil {
		t.Fatalf("tree_json: %v", err)
	}

	if hasChild(&root, "Layouts") {
		t.Errorf("excluded directory was recorded: %v", childNames(&root))
	}
	child(t, &root, "keep")
	// max_depth 2 records the root plus two levels, so "deep/one" exists but
	// "deep/one/two" does not.
	one := child(t, child(t, &root, "deep"), "one")
	if len(one.Children) != 0 {
		t.Errorf("depth cap not applied, got %v", childNames(one))
	}
}

// Apply is only reachable from a ready preview, so the user can never confirm
// something they were not shown.
func TestApplyRequiresAPreview(t *testing.T) {
	m := newManagerForTest(t, map[string]*stubBackend{}, nil)

	if err := m.StartApply(); err == nil {
		t.Fatal("applying without a preview must fail")
	}
}
