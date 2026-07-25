package autosync

import (
	"context"
	"fmt"
	"io"
	"testing"

	"github.com/syncsystem-net/back-me-up/internal/accounts"
	"github.com/syncsystem-net/back-me-up/internal/database"
	"github.com/syncsystem-net/back-me-up/internal/provider"
)

// countingBackend streams a body of a chosen length and counts how much the
// consumer actually accepted, so the indexing cap can be asserted on bytes
// transferred rather than on whatever size the provider claimed.
type countingBackend struct {
	*stubBackend
	streamBytes int64
	written     int64
}

func (c *countingBackend) Download(_ context.Context, _ string, w io.Writer) error {
	const block = 1 << 20
	buf := make([]byte, block)
	for c.written < c.streamBytes {
		n, err := w.Write(buf)
		c.written += int64(n)
		if err != nil {
			// The cap aborts the transfer mid-stream; a real backend surfaces
			// that like any other write failure, wrapped once.
			return fmt.Errorf("streaming download: %w", err)
		}
	}
	return nil
}

// A listing reporting size 0 — the likely 4shared outcome, since its size field
// is decoded defensively — must not buy an archive an unbounded download. The
// user asked to read a file list; a 60 GB transfer is not that. The archive is
// still adopted, with a note explaining why it has no tree.
func TestApplyDoesNotDownloadUnboundedlyForUnknownSize(t *testing.T) {
	// Larger than the ceiling, from a provider that reports no size and refuses
	// ranged reads — the worst realistic combination.
	backend := &stubBackend{
		fakeProvider: &fakeProvider{rangeSupported: false},
		files:        []provider.RemoteFile{{ID: "ref-1", Name: "huge.zip", Size: 0}},
	}
	const testCap = 4 << 20
	oversized := &countingBackend{stubBackend: backend, streamBytes: testCap * 8}

	accts := []accounts.Account{{Provider: accounts.ProviderFourShared, Email: "u@example.com"}}
	m := newManagerForTest(t, map[string]*stubBackend{"fourshared|u@example.com": backend}, accts)
	// Same ceiling logic, small enough to assert without moving gigabytes.
	m.maxIndexBytes = testCap
	// Route connect at the counting wrapper so its Download is the one exercised.
	m.connect = func(context.Context, *accounts.AccountStore, string, string, int64) (provider.Provider, error) {
		return oversized, nil
	}

	if err := m.StartPreview(); err != nil {
		t.Fatalf("StartPreview: %v", err)
	}
	waitFor(t, m, PhasePreviewReady)
	if err := m.StartApply(); err != nil {
		t.Fatalf("StartApply: %v", err)
	}
	run := waitFor(t, m, PhaseApplied)

	// The guard has to hold on bytes RECEIVED: the listing said 0, so nothing
	// about the declared size could have stopped this.
	if oversized.written > testCap {
		t.Errorf("downloaded %d bytes, past the %d ceiling — an unknown size bypassed the cap",
			oversized.written, int64(testCap))
	}
	if oversized.written >= oversized.streamBytes {
		t.Errorf("the whole %d byte stream was consumed; the transfer was never capped", oversized.streamBytes)
	}

	// It is still adopted, and it explains itself.
	plan := run.Accounts[0]
	if plan.Applied != 1 {
		t.Fatalf("an unindexable archive must still be adopted, got applied=%d", plan.Applied)
	}
	if plan.Proposed[0].Note == "" {
		t.Error("expected a note explaining why the archive has no file tree")
	}

	backup, err := database.GetBackupByOwner(m.db, "u@example.com")
	if err != nil || backup == nil {
		t.Fatalf("expected the record to exist: %v", err)
	}
	zips, err := database.ListZipsByBackup(m.db, backup.ID)
	if err != nil {
		t.Fatalf("ListZipsByBackup: %v", err)
	}
	if len(zips) != 1 {
		t.Fatalf("expected the archive to be recorded, got %+v", zips)
	}
	if zips[0].TreeJSON != "" {
		t.Errorf("expected no tree for an unindexable archive, got %q", zips[0].TreeJSON)
	}
}

// The link action end to end: an archive already recorded (uploaded to a sibling
// account) that also turns out to live here needs its own job row, while the
// existing zip row and its tree are left exactly as they were.
func TestApplyLinksExistingZipToAnotherAccount(t *testing.T) {
	data := archiveBytes(t, "shared", []string{"a.txt"})
	backends := map[string]*stubBackend{
		"mega|u@example.com": {
			fakeProvider: &fakeProvider{data: data, rangeSupported: true},
			files:        []provider.RemoteFile{{ID: "mega-ref", Name: "shared.zip", Size: int64(len(data))}},
		},
		"fourshared|u@example.com": {
			fakeProvider: &fakeProvider{data: data, rangeSupported: true},
			files:        []provider.RemoteFile{{ID: "4s-ref", Name: "shared.zip", Size: int64(len(data))}},
		},
	}
	accts := []accounts.Account{
		{Provider: accounts.ProviderMega, Email: "u@example.com"},
		{Provider: accounts.ProviderFourShared, Email: "u@example.com"},
	}
	m := newManagerForTest(t, backends, accts)

	// Both accounts are crawled in one run, so one adopts and the other links.
	if err := m.StartPreview(); err != nil {
		t.Fatalf("StartPreview: %v", err)
	}
	waitFor(t, m, PhasePreviewReady)
	if err := m.StartApply(); err != nil {
		t.Fatalf("StartApply: %v", err)
	}
	waitFor(t, m, PhaseApplied)

	backup, err := database.GetBackupByOwner(m.db, "u@example.com")
	if err != nil || backup == nil {
		t.Fatalf("expected a record: %v", err)
	}
	zips, err := database.ListZipsByBackup(m.db, backup.ID)
	if err != nil {
		t.Fatalf("ListZipsByBackup: %v", err)
	}
	// The point of link: ONE zip row shared by both accounts, not a duplicate.
	if len(zips) != 1 {
		t.Fatalf("expected a single zip row shared by both accounts, got %d: %+v", len(zips), zips)
	}
	if zips[0].TreeJSON == "" {
		t.Fatal("the adopted zip should have kept its tree")
	}

	jobs, err := database.ListJobsByBackup(m.db, backup.ID)
	if err != nil {
		t.Fatalf("ListJobsByBackup: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("expected one job per account, got %d: %+v", len(jobs), jobs)
	}
	byRef := map[string]bool{}
	for _, j := range jobs {
		if j.ZipID != zips[0].ID {
			t.Errorf("job %d points at zip %d, want %d", j.ID, j.ZipID, zips[0].ID)
		}
		if j.Status != "complete" || j.RemotePath == "" {
			t.Errorf("job %d is not actionable: status=%q remote_path=%q", j.ID, j.Status, j.RemotePath)
		}
		byRef[j.RemotePath] = true
	}
	// Each account's job must carry ITS OWN handle — sharing one would send
	// download or delete to the wrong account.
	if !byRef["mega-ref"] || !byRef["4s-ref"] {
		t.Errorf("expected each account's own remote handle, got %v", byRef)
	}

	// And the whole thing settles: a re-run proposes nothing.
	if err := m.StartPreview(); err != nil {
		t.Fatalf("second StartPreview: %v", err)
	}
	run := waitFor(t, m, PhasePreviewReady)
	for _, plan := range run.Accounts {
		if len(plan.Proposed) != 0 {
			t.Errorf("%s proposed %+v on the second run, want nothing", plan.Provider, plan.Proposed)
		}
	}
}

// The acceptance criterion is about the DATABASE: a record whose remote copy has
// vanished is reported and otherwise left completely alone. A transient API
// failure must never be able to destroy a user's records.
func TestApplyLeavesMissingRemoteRowsUntouchedInTheDatabase(t *testing.T) {
	// The account is reachable and lists successfully — it simply no longer has
	// the file. That is the dangerous case: it looks like a legitimate deletion.
	backend := &stubBackend{
		fakeProvider: &fakeProvider{},
		files:        []provider.RemoteFile{},
	}
	accts := []accounts.Account{{Provider: accounts.ProviderMega, Email: "u@example.com"}}
	m := newManagerForTest(t, map[string]*stubBackend{"mega|u@example.com": backend}, accts)

	acctID, err := database.GetDBAccountIDByProviderEmail(m.db, "mega", "u@example.com")
	if err != nil {
		t.Fatalf("account lookup: %v", err)
	}
	tx, err := m.db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	backupID, err := database.UpsertBackupForUser(tx, "u@example.com", "kept", "/src")
	if err != nil {
		t.Fatalf("UpsertBackupForUser: %v", err)
	}
	zipID, err := database.InsertZip(tx, backupID, "gone.zip", "/src", 4242, `{"name":"gone"}`)
	if err != nil {
		t.Fatalf("InsertZip: %v", err)
	}
	jobID, err := database.InsertAdoptedJob(tx, backupID, zipID, acctID, "gone.zip", "old-ref", 4242)
	if err != nil {
		t.Fatalf("InsertAdoptedJob: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	if err := m.StartPreview(); err != nil {
		t.Fatalf("StartPreview: %v", err)
	}
	run := waitFor(t, m, PhasePreviewReady)

	plan := run.Accounts[0]
	if len(plan.Missing) != 1 || plan.Missing[0].ZipID != zipID {
		t.Fatalf("expected the vanished archive reported as missing, got %+v", plan.Missing)
	}
	if len(plan.Proposed) != 0 {
		t.Fatalf("a missing remote must never produce a change, got %+v", plan.Proposed)
	}

	// Apply anyway — even an explicit confirmation must not touch it.
	if err := m.StartApply(); err != nil {
		t.Fatalf("StartApply: %v", err)
	}
	waitFor(t, m, PhaseApplied)

	zips, err := database.ListZipsByBackup(m.db, backupID)
	if err != nil {
		t.Fatalf("ListZipsByBackup: %v", err)
	}
	if len(zips) != 1 {
		t.Fatalf("the zip row must survive, got %+v", zips)
	}
	if zips[0].Name != "gone.zip" || zips[0].SizeBytes != 4242 || zips[0].TreeJSON != `{"name":"gone"}` {
		t.Errorf("the zip row was modified: %+v", zips[0])
	}
	jobs, err := database.ListJobsByBackup(m.db, backupID)
	if err != nil {
		t.Fatalf("ListJobsByBackup: %v", err)
	}
	if len(jobs) != 1 || jobs[0].ID != jobID {
		t.Fatalf("the job row must survive, got %+v", jobs)
	}
	if jobs[0].RemotePath != "old-ref" || jobs[0].Status != "complete" {
		t.Errorf("the job row was modified: %+v", jobs[0])
	}
	if b, err := database.GetBackupByOwner(m.db, "u@example.com"); err != nil || b == nil || b.Title != "kept" {
		t.Errorf("the backup record was modified or removed: %+v (%v)", b, err)
	}
}
