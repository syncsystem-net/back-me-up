package autosync

import (
	"testing"

	"github.com/syncsystem-net/back-me-up/internal/provider"
)

func names(t *testing.T, proposed []ProposedZip) map[string]ProposedZip {
	t.Helper()
	out := make(map[string]ProposedZip, len(proposed))
	for _, p := range proposed {
		out[p.Name] = p
	}
	return out
}

func TestPlanAdoptsUnknownArchives(t *testing.T) {
	remote := []provider.RemoteFile{
		{ID: "r1", Name: "bkup001.zip", Size: 1234},
		{ID: "r2", Name: "bkup002.zip", Size: 99},
	}

	proposed, matched, missing := Plan(remote, nil)

	if len(proposed) != 2 {
		t.Fatalf("expected 2 proposals, got %d: %+v", len(proposed), proposed)
	}
	if len(matched) != 0 || len(missing) != 0 {
		t.Fatalf("expected no matched/missing, got %d/%d", len(matched), len(missing))
	}
	got := names(t, proposed)
	if p := got["bkup001.zip"]; p.Action != ActionAdopt || p.RemoteID != "r1" || p.SizeBytes != 1234 {
		t.Errorf("unexpected proposal for bkup001.zip: %+v", p)
	}
}

func TestPlanIgnoresNonZipFiles(t *testing.T) {
	remote := []provider.RemoteFile{
		{ID: "r1", Name: "notes.txt"},
		{ID: "r2", Name: "photo.jpg"},
		{ID: "r3", Name: ".zip"}, // extension only, no actual name
		{ID: "r4", Name: "real.ZIP"},
	}

	proposed, _, _ := Plan(remote, nil)

	if len(proposed) != 1 {
		t.Fatalf("expected only the .ZIP to be adopted, got %+v", proposed)
	}
	if proposed[0].Name != "real.ZIP" {
		t.Errorf("expected real.ZIP, got %q", proposed[0].Name)
	}
}

// A zip recorded locally but with no job on this account means a sibling account
// uploaded it. The archive is here too, so the account needs its own job row to
// make Download/Delete work — but the existing tree must not be re-read.
func TestPlanLinksZipKnownButNotOnThisAccount(t *testing.T) {
	remote := []provider.RemoteFile{{ID: "r1", Name: "bkup001.zip", Size: 500}}
	local := []LocalZip{{ZipID: 7, Name: "bkup001.zip"}} // no JobID

	proposed, matched, missing := Plan(remote, local)

	if len(proposed) != 1 || proposed[0].Action != ActionLink {
		t.Fatalf("expected one link proposal, got %+v", proposed)
	}
	if proposed[0].ZipID != 7 {
		t.Errorf("link should carry the existing zip id, got %d", proposed[0].ZipID)
	}
	if len(matched) != 0 || len(missing) != 0 {
		t.Errorf("expected no matched/missing, got %d/%d", len(matched), len(missing))
	}
}

func TestPlanReportsAlreadyMatched(t *testing.T) {
	remote := []provider.RemoteFile{{ID: "r1", Name: "bkup001.zip"}}
	local := []LocalZip{{ZipID: 7, Name: "bkup001.zip", JobID: 42, RemotePath: "r1"}}

	proposed, matched, missing := Plan(remote, local)

	if len(proposed) != 0 {
		t.Fatalf("a fully recorded archive must propose nothing, got %+v", proposed)
	}
	if len(matched) != 1 || matched[0].ZipID != 7 {
		t.Fatalf("expected one matched zip, got %+v", matched)
	}
	if len(missing) != 0 {
		t.Errorf("expected no missing, got %+v", missing)
	}
}

// The remote copy is gone. It must be reported and never turned into a change.
func TestPlanReportsMissingRemoteCopyWithoutProposingAnything(t *testing.T) {
	local := []LocalZip{{ZipID: 7, Name: "bkup001.zip", JobID: 42, RemotePath: "r1"}}

	proposed, matched, missing := Plan(nil, local)

	if len(proposed) != 0 || len(matched) != 0 {
		t.Fatalf("a missing remote copy must not propose or match anything: %+v / %+v", proposed, matched)
	}
	if len(missing) != 1 {
		t.Fatalf("expected one missing entry, got %+v", missing)
	}
	if missing[0].ZipID != 7 || missing[0].JobID != 42 || missing[0].Name != "bkup001.zip" {
		t.Errorf("unexpected missing entry: %+v", missing[0])
	}
}

// A zip row this account never held (no job, or a job that never finished
// uploading) is not "missing from this account" — it was never here.
func TestPlanDoesNotReportZipsThisAccountNeverHeld(t *testing.T) {
	local := []LocalZip{
		{ZipID: 1, Name: "elsewhere.zip"},                                // no job here
		{ZipID: 2, Name: "never-finished.zip", JobID: 9, RemotePath: ""}, // job, but no remote handle
	}

	_, _, missing := Plan(nil, local)

	if len(missing) != 0 {
		t.Fatalf("expected nothing reported missing, got %+v", missing)
	}
}

// A job that never completed its upload has no remote handle, so it cannot
// download or delete anything. When the archive turns out to be on the account
// after all, calling that "in sync" would leave the user staring at a row whose
// buttons do nothing — one of the exact situations Auto-Sync exists to repair.
// It needs a job that actually points at the remote copy.
func TestPlanLinksZipWhoseJobHasNoRemoteHandle(t *testing.T) {
	remote := []provider.RemoteFile{{ID: "r1", Name: "half-uploaded.zip", Size: 64}}
	local := []LocalZip{{ZipID: 3, Name: "half-uploaded.zip", JobID: 9, RemotePath: ""}}

	proposed, matched, missing := Plan(remote, local)

	if len(matched) != 0 {
		t.Errorf("a job with no remote handle is not in sync, got %+v", matched)
	}
	if len(proposed) != 1 || proposed[0].Action != ActionLink {
		t.Fatalf("expected a link proposal, got %+v", proposed)
	}
	if proposed[0].ZipID != 3 {
		t.Errorf("link should target the existing zip row, got %d", proposed[0].ZipID)
	}
	if proposed[0].RemoteID != "r1" {
		t.Errorf("link must carry the remote handle it is repairing, got %q", proposed[0].RemoteID)
	}
	if len(missing) != 0 {
		t.Errorf("the archive is present remotely, nothing is missing: %+v", missing)
	}
}

// A record accumulates zips, so two rows can share a name. The one that already
// accounts for this account's copy must win — picking the other would attach a
// job to a row whose recorded tree describes different contents.
func TestPlanPrefersTheSameNamedRowThatHoldsThisAccountsCopy(t *testing.T) {
	remote := []provider.RemoteFile{{ID: "r1", Name: "bkup001.zip", Size: 100}}
	// ListZipsByBackup orders created_at DESC, so the newest row comes first and
	// naive last-wins would select the older one.
	local := []LocalZip{
		{ZipID: 20, Name: "bkup001.zip", JobID: 55, RemotePath: "r1"}, // newest, holds this account's copy
		{ZipID: 10, Name: "bkup001.zip"},                              // older re-upload of the same name
	}

	proposed, matched, missing := Plan(remote, local)

	if len(proposed) != 0 {
		t.Fatalf("the account's copy is already recorded; expected no proposals, got %+v", proposed)
	}
	if len(matched) != 1 || matched[0].ZipID != 20 {
		t.Fatalf("expected the row holding this account's copy (20) to match, got %+v", matched)
	}
	if len(missing) != 0 {
		t.Errorf("expected nothing missing, got %+v", missing)
	}
}

// Same situation, opposite ordering, to prove the choice is by content and not
// by position in the slice.
func TestPlanPrefersHoldingRowRegardlessOfOrder(t *testing.T) {
	remote := []provider.RemoteFile{{ID: "r1", Name: "bkup001.zip", Size: 100}}
	local := []LocalZip{
		{ZipID: 10, Name: "bkup001.zip"},
		{ZipID: 20, Name: "bkup001.zip", JobID: 55, RemotePath: "r1"},
	}

	proposed, matched, _ := Plan(remote, local)

	if len(proposed) != 0 || len(matched) != 1 || matched[0].ZipID != 20 {
		t.Fatalf("expected row 20 to match with no proposals, got proposed=%+v matched=%+v", proposed, matched)
	}
}

// Between two same-named rows that both have a job here, the one with a usable
// remote handle is the better record of this account's copy.
func TestPlanPrefersRowWithARemoteHandle(t *testing.T) {
	remote := []provider.RemoteFile{{ID: "r1", Name: "bkup001.zip"}}
	local := []LocalZip{
		{ZipID: 10, Name: "bkup001.zip", JobID: 41, RemotePath: ""},
		{ZipID: 20, Name: "bkup001.zip", JobID: 42, RemotePath: "r1"},
	}

	proposed, matched, _ := Plan(remote, local)

	if len(proposed) != 0 {
		t.Fatalf("expected no proposals, got %+v", proposed)
	}
	if len(matched) != 1 || matched[0].ZipID != 20 {
		t.Fatalf("expected the row with a remote handle to match, got %+v", matched)
	}
}

// The acceptance criterion: applying a plan and re-running the crawl over an
// unchanged account must propose nothing at all. This simulates the state the
// apply step leaves behind (a zip row plus a job on this account) and asserts
// the second pass is a no-op.
func TestPlanIsIdempotentAfterApply(t *testing.T) {
	remote := []provider.RemoteFile{
		{ID: "r1", Name: "bkup001.zip", Size: 10},
		{ID: "r2", Name: "bkup002.zip", Size: 20},
	}

	firstPass, _, _ := Plan(remote, nil)
	if len(firstPass) != 2 {
		t.Fatalf("expected both archives adopted on the first pass, got %+v", firstPass)
	}

	// What the database looks like once those adoptions have been written.
	applied := []LocalZip{
		{ZipID: 1, Name: "bkup001.zip", JobID: 101, RemotePath: "r1"},
		{ZipID: 2, Name: "bkup002.zip", JobID: 102, RemotePath: "r2"},
	}

	secondPass, matched, missing := Plan(remote, applied)

	if len(secondPass) != 0 {
		t.Fatalf("second run must propose nothing, got %+v", secondPass)
	}
	if len(matched) != 2 {
		t.Errorf("expected both archives reported as in sync, got %+v", matched)
	}
	if len(missing) != 0 {
		t.Errorf("expected nothing missing, got %+v", missing)
	}
}
