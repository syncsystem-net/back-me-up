package handlers

import (
	"archive/zip"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/syncsystem-net/back-me-up/internal/archive"
	"github.com/syncsystem-net/back-me-up/internal/database"
	"github.com/syncsystem-net/back-me-up/internal/limits"
)

// splitTestDB opens a migrated database with a free 4shared account (id 1) and a
// free MEGA account (id 2), and tightens 4shared's threshold so a small fixture
// exercises the split.
func splitTestDB(t *testing.T, fourSharedThreshold int64) *sql.DB {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "split.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	for _, a := range []struct{ provider, email, tier string }{
		{"fourshared", "4s@example.com", "free"},
		{"mega", "m@example.com", "free"},
	} {
		if _, err := db.Exec(`INSERT INTO accounts (provider, email, tier) VALUES (?, ?, ?)`,
			a.provider, a.email, a.tier); err != nil {
			t.Fatalf("seeding account: %v", err)
		}
	}

	set := limits.Defaults()
	set["fourshared"][limits.TierFree] = limits.Limit{MaxFileBytes: fourSharedThreshold, TransferBytes: 0, WindowHours: 24}
	set["mega"][limits.TierFree] = limits.Limit{MaxFileBytes: 0, TransferBytes: 0, WindowHours: 6}
	if err := database.SetProviderLimits(db, set); err != nil {
		t.Fatalf("SetProviderLimits: %v", err)
	}
	return db
}

func splitTestSource(t *testing.T, files map[string]int) string {
	return splitTestSourceWith(t, files, false)
}

// splitTestSourceWith writes a source tree. incompressible fills the files with
// random bytes, which matters whenever a test needs the *finished archive* to be
// over a threshold: zero-filled files deflate to almost nothing, so an archive
// that should have been cut into parts comes out as one small file instead.
func splitTestSourceWith(t *testing.T, files map[string]int, incompressible bool) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "bkup")
	for rel, size := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		buf := make([]byte, size)
		if incompressible {
			if _, err := rand.Read(buf); err != nil {
				t.Fatalf("rand: %v", err)
			}
		}
		if err := os.WriteFile(full, buf, 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	return root
}

// The decision the user made: MEGA keeps one whole archive while a free 4shared
// account gets volumes, so restoring from MEGA stays a single download.
func TestPlanGivesEachProviderItsOwnArchiveSet(t *testing.T) {
	db := splitTestDB(t, 1_000)
	h := &Handlers{db: db, scanMaxDepth: 3}
	src := splitTestSource(t, map[string]int{
		"alpha/a.bin": 600,
		"beta/b.bin":  600,
		"gamma/c.bin": 600,
	})

	plan, err := h.buildUploadPlan(src, []int64{1, 2})
	if err != nil {
		t.Fatalf("buildUploadPlan: %v", err)
	}
	if !plan.OK() {
		t.Fatalf("plan blocked: %+v", plan.Blockers)
	}
	if len(plan.Groups) != 2 {
		t.Fatalf("got %d groups, want one per threshold: %+v", len(plan.Groups), plan.Groups)
	}

	byProvider := map[string]*planGroup{}
	for _, g := range plan.Groups {
		byProvider[g.Accounts[0].Provider] = g
	}
	if n := len(byProvider["mega"].Volumes); n != 1 {
		t.Errorf("MEGA got %d volumes, want 1 whole archive", n)
	}
	if n := len(byProvider["fourshared"].Volumes); n < 2 {
		t.Errorf("4shared got %d volumes, want a split at the 1000-byte threshold", n)
	}
	if got := byProvider["mega"].Volumes[0].Name; got != filepath.Base(src)+".zip" {
		t.Errorf("MEGA archive named %q, want the plain unsplit name", got)
	}
}

// Two accounts needing identical volumes must share one archive set, or a 4.3 GB
// source would be compressed once per account for no benefit.
func TestAccountsWithTheSameThresholdShareOneArchiveSet(t *testing.T) {
	db := splitTestDB(t, 1_000)
	if _, err := db.Exec(`INSERT INTO accounts (provider, email, tier) VALUES ('fourshared', 'second@example.com', 'free')`); err != nil {
		t.Fatalf("seeding second account: %v", err)
	}
	h := &Handlers{db: db, scanMaxDepth: 3}
	src := splitTestSource(t, map[string]int{"alpha/a.bin": 600, "beta/b.bin": 600})

	plan, err := h.buildUploadPlan(src, []int64{1, 3})
	if err != nil {
		t.Fatalf("buildUploadPlan: %v", err)
	}
	if len(plan.Groups) != 1 {
		t.Fatalf("got %d groups, want 1 shared set: %+v", len(plan.Groups), plan.Groups)
	}
	if len(plan.Groups[0].Accounts) != 2 {
		t.Errorf("group holds %d accounts, want both", len(plan.Groups[0].Accounts))
	}
}

// A backup no bigger than the threshold must behave exactly as it did before
// splitting existed: one archive, one zip row, one compression pass — even
// though MEGA and free 4shared have different thresholds and start out as
// different groups. This is the ordinary case, not an edge case.
func TestAnUnsplitBackupProducesExactlyOneArchiveForEveryone(t *testing.T) {
	db := splitTestDB(t, 1_000_000)
	h := &Handlers{db: db, scanMaxDepth: 3}
	src := splitTestSource(t, map[string]int{"docs/a.txt": 10})

	plan, err := h.buildUploadPlan(src, []int64{1, 2})
	if err != nil {
		t.Fatalf("buildUploadPlan: %v", err)
	}
	if len(plan.Groups) != 1 {
		t.Fatalf("got %d groups, want 1: accounts that all take the whole archive must share it", len(plan.Groups))
	}
	g := plan.Groups[0]
	if len(g.Accounts) != 2 {
		t.Errorf("group holds %d accounts, want both", len(g.Accounts))
	}
	if len(g.Volumes) != 1 || g.Volumes[0].Name != filepath.Base(src)+".zip" {
		t.Errorf("volumes = %+v, want the single plain unsplit name", g.Volumes)
	}
	// The merged group keeps the strictest threshold that applies, so the
	// post-write size check still measures against the tightest limit.
	if g.ThresholdBytes != 1_000_000 {
		t.Errorf("merged threshold = %d, want the strictest (1000000)", g.ThresholdBytes)
	}

	built, err := h.buildArchives(src, plan, nil)
	defer built.discard()
	if err != nil {
		t.Fatalf("buildArchives: %v", err)
	}
	if len(built) != 1 {
		t.Fatalf("wrote %d archives, want 1 — the source must not be compressed twice", len(built))
	}

	backupID, err := h.recordBackup("u@example.com", "Unsplit", src, built)
	if err != nil {
		t.Fatalf("recordBackup: %v", err)
	}
	zips, err := database.ListZipsByBackup(db, backupID)
	if err != nil {
		t.Fatalf("ListZipsByBackup: %v", err)
	}
	if len(zips) != 1 {
		t.Errorf("recorded %d zip rows, want 1: a duplicate row shows the archive twice in the tree and in search", len(zips))
	}
	jobs, err := database.ListJobsByBackup(db, backupID)
	if err != nil {
		t.Fatalf("ListJobsByBackup: %v", err)
	}
	if len(jobs) != 2 {
		t.Errorf("recorded %d jobs, want one per account", len(jobs))
	}
}

// Merging only applies to groups that need no split. A group whose volumes are
// threshold-specific must keep its own archives.
func TestAGroupThatSplitsIsNeverMergedWithOneThatDoesNot(t *testing.T) {
	db := splitTestDB(t, 1_000)
	h := &Handlers{db: db, scanMaxDepth: 3}
	src := splitTestSource(t, map[string]int{"alpha/a.bin": 600, "beta/b.bin": 600})

	plan, err := h.buildUploadPlan(src, []int64{1, 2})
	if err != nil {
		t.Fatalf("buildUploadPlan: %v", err)
	}
	if len(plan.Groups) != 2 {
		t.Fatalf("got %d groups, want 2: MEGA takes the whole archive, 4shared takes volumes", len(plan.Groups))
	}
}

// Each volume must record only what it holds. A volume claiming the whole source
// would make the record's merged tree and the global search describe archives
// that do not contain what they say.
func TestEachVolumeRecordsOnlyItsOwnTree(t *testing.T) {
	db := splitTestDB(t, 1_000)
	h := &Handlers{db: db, scanMaxDepth: 3}
	src := splitTestSource(t, map[string]int{
		"alpha/a.bin": 600,
		"beta/b.bin":  600,
		"gamma/c.bin": 600,
	})

	plan, err := h.buildUploadPlan(src, []int64{1})
	if err != nil {
		t.Fatalf("buildUploadPlan: %v", err)
	}
	built, err := h.buildArchives(src, plan, nil)
	defer built.discard()
	if err != nil {
		t.Fatalf("buildArchives: %v", err)
	}
	if len(built) < 2 {
		t.Fatalf("built %d archives, want a split", len(built))
	}

	seen := map[string]int{}
	for _, a := range built {
		var root struct {
			Name     string `json:"name"`
			Children []struct {
				Name string `json:"name"`
			} `json:"children"`
		}
		if err := json.Unmarshal([]byte(a.treeJSON), &root); err != nil {
			t.Fatalf("parsing tree for %s: %v", a.remoteName, err)
		}
		if root.Name != filepath.Base(src) {
			t.Errorf("volume %s roots its tree at %q, want the source directory name", a.remoteName, root.Name)
		}
		for _, c := range root.Children {
			seen[c.Name]++
		}
	}

	for _, dir := range []string{"alpha", "beta", "gamma"} {
		switch seen[dir] {
		case 0:
			t.Errorf("%q appears in no volume's tree", dir)
		case 1: // exactly right
		default:
			t.Errorf("%q appears in %d volumes' trees; each directory belongs to one volume", dir, seen[dir])
		}
	}
}

// Every produced archive is written to disk and recorded as its own zip row with
// jobs for the accounts that receive it.
func TestRecordBackupWritesOneZipRowPerArchive(t *testing.T) {
	db := splitTestDB(t, 1_000)
	h := &Handlers{db: db, scanMaxDepth: 3}
	src := splitTestSource(t, map[string]int{
		"alpha/a.bin": 600,
		"beta/b.bin":  600,
	})

	plan, err := h.buildUploadPlan(src, []int64{1, 2})
	if err != nil {
		t.Fatalf("buildUploadPlan: %v", err)
	}
	built, err := h.buildArchives(src, plan, nil)
	defer built.discard()
	if err != nil {
		t.Fatalf("buildArchives: %v", err)
	}

	backupID, err := h.recordBackup("u@example.com", "Split test", src, built)
	if err != nil {
		t.Fatalf("recordBackup: %v", err)
	}

	zips, err := database.ListZipsByBackup(db, backupID)
	if err != nil {
		t.Fatalf("ListZipsByBackup: %v", err)
	}
	if len(zips) != len(built) {
		t.Errorf("recorded %d zip rows, want %d (one per archive)", len(zips), len(built))
	}

	jobs, err := database.ListJobsByBackup(db, backupID)
	if err != nil {
		t.Fatalf("ListJobsByBackup: %v", err)
	}
	var want int
	for _, a := range built {
		want += len(a.accountIDs)
	}
	if len(jobs) != want {
		t.Errorf("recorded %d jobs, want %d", len(jobs), want)
	}
	// Every archive really exists on disk, and each job points at one.
	for _, a := range built {
		if _, err := os.Stat(a.zipPath); err != nil {
			t.Errorf("archive %s was recorded but not written: %v", a.remoteName, err)
		}
	}
}

// A budget smaller than one archive can never be satisfied, so the upload is
// refused rather than queued to be held forever.
func TestABudgetSmallerThanOneArchiveBlocksThePlan(t *testing.T) {
	db := splitTestDB(t, 1_000)
	set := database.GetProviderLimits(db)
	set["fourshared"][limits.TierFree] = limits.Limit{MaxFileBytes: 1_000, TransferBytes: 100, WindowHours: 24}
	if err := database.SetProviderLimits(db, set); err != nil {
		t.Fatalf("SetProviderLimits: %v", err)
	}
	h := &Handlers{db: db, scanMaxDepth: 3}
	src := splitTestSource(t, map[string]int{"alpha/a.bin": 600, "beta/b.bin": 600})

	plan, err := h.buildUploadPlan(src, []int64{1})
	if err != nil {
		t.Fatalf("buildUploadPlan: %v", err)
	}
	if plan.OK() {
		t.Fatal("plan reported OK; an archive larger than the whole budget can never upload")
	}
	if plan.Blockers[0].Reason != "budget_impossible" {
		t.Errorf("reason = %q, want budget_impossible", plan.Blockers[0].Reason)
	}
	if !strings.Contains(plan.Blockers[0].Message, "Settings") {
		t.Errorf("message %q should name the way out", plan.Blockers[0].Message)
	}
}

// A budget merely spent for now is a warning, not a blocker: the jobs are
// created and the worker holds them until the window rolls over, which is what
// enforcement means.
func TestASpentBudgetWarnsButDoesNotBlock(t *testing.T) {
	db := splitTestDB(t, 0)
	set := database.GetProviderLimits(db)
	set["fourshared"][limits.TierFree] = limits.Limit{MaxFileBytes: 0, TransferBytes: 10_000, WindowHours: 24}
	if err := database.SetProviderLimits(db, set); err != nil {
		t.Fatalf("SetProviderLimits: %v", err)
	}
	if err := database.RecordTransfer(db, 1, 9_900); err != nil {
		t.Fatalf("RecordTransfer: %v", err)
	}
	h := &Handlers{db: db, scanMaxDepth: 3}
	src := splitTestSource(t, map[string]int{"alpha/a.bin": 5_000})

	plan, err := h.buildUploadPlan(src, []int64{1})
	if err != nil {
		t.Fatalf("buildUploadPlan: %v", err)
	}
	if !plan.OK() {
		t.Fatalf("plan blocked, want a warning only: %+v", plan.Blockers)
	}
	if len(plan.Warnings) == 0 || plan.Warnings[0].Reason != "budget_hold" {
		t.Errorf("warnings = %+v, want a budget_hold", plan.Warnings)
	}
}

// The tier is what selects a threshold, so changing it must change the plan.
func TestChangingTheTierChangesTheThreshold(t *testing.T) {
	db := splitTestDB(t, 1_000)
	set := database.GetProviderLimits(db)
	set["fourshared"][limits.TierPaid] = limits.Limit{MaxFileBytes: 0, TransferBytes: 0, WindowHours: 24}
	if err := database.SetProviderLimits(db, set); err != nil {
		t.Fatalf("SetProviderLimits: %v", err)
	}
	h := &Handlers{db: db, scanMaxDepth: 3}
	src := splitTestSource(t, map[string]int{"alpha/a.bin": 600, "beta/b.bin": 600})

	plan, err := h.buildUploadPlan(src, []int64{1})
	if err != nil {
		t.Fatalf("buildUploadPlan: %v", err)
	}
	if len(plan.Groups[0].Volumes) < 2 {
		t.Fatalf("free tier should split; got %d volumes", len(plan.Groups[0].Volumes))
	}

	w := putTier(t, db, "1", `{"tier":"paid"}`)
	if w.Code != 200 {
		t.Fatalf("tier PUT status = %d: %s", w.Code, w.Body.String())
	}

	plan, err = h.buildUploadPlan(src, []int64{1})
	if err != nil {
		t.Fatalf("buildUploadPlan after tier change: %v", err)
	}
	if len(plan.Groups[0].Volumes) != 1 {
		t.Errorf("paid tier still split into %d volumes", len(plan.Groups[0].Volumes))
	}
}

func TestTierEndpointRejectsAnUnknownTier(t *testing.T) {
	db := splitTestDB(t, 0)
	w := putTier(t, db, "1", `{"tier":"platinum"}`)
	if w.Code != 400 {
		t.Errorf("status = %d, want 400 for an unknown tier: %s", w.Code, w.Body.String())
	}
	// The account's tier must be untouched by the rejected request.
	acct, err := database.GetDBAccountByID(db, 1)
	if err != nil {
		t.Fatalf("GetDBAccountByID: %v", err)
	}
	if acct.Tier != "free" {
		t.Errorf("tier = %q after a rejected write, want free", acct.Tier)
	}
}

// putTier drives the tier endpoint with the path value the router would supply.
func putTier(t *testing.T, db *sql.DB, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("PUT", "/api/accounts/"+id+"/tier", strings.NewReader(body))
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	PutAccountTierHandler(db)(w, r)
	return w
}

// The Accounts view needs to show what each account's tier currently means.
func TestAccountsEndpointReportsLimitsAndUsage(t *testing.T) {
	db := splitTestDB(t, 1_000)
	set := database.GetProviderLimits(db)
	set["fourshared"][limits.TierFree] = limits.Limit{MaxFileBytes: 1_000, TransferBytes: 8_000, WindowHours: 24}
	if err := database.SetProviderLimits(db, set); err != nil {
		t.Fatalf("SetProviderLimits: %v", err)
	}
	if err := database.RecordTransfer(db, 1, 2_000); err != nil {
		t.Fatalf("RecordTransfer: %v", err)
	}

	w := do(t, GetAccountsHandler(db), "GET", "/api/accounts", "")
	if w.Code != 200 {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var got []database.DBAccount
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	var fs *database.DBAccount
	for i := range got {
		if got[i].Provider == "fourshared" {
			fs = &got[i]
		}
	}
	if fs == nil {
		t.Fatal("no 4shared account in the response")
	}
	if fs.Tier != "free" || fs.MaxFileBytes != 1_000 || fs.TransferBudgetBytes != 8_000 || fs.TransferUsedBytes != 2_000 {
		t.Errorf("account = %+v, want tier free with the configured limits and 2000 used", fs)
	}
}

// The preview and the upload must agree, so they run the same planner. This
// checks the endpoint reports the same shape the planner produced.
func TestPreflightReportsThePlanWithoutZipping(t *testing.T) {
	db := splitTestDB(t, 1_000)
	h := &Handlers{db: db, scanMaxDepth: 3}
	src := splitTestSource(t, map[string]int{"alpha/a.bin": 600, "beta/b.bin": 600})

	body := fmt.Sprintf(`{"source_path":%q,"account_ids":[1,2]}`, filepath.ToSlash(src))
	w := do(t, h.PostBackupsPreflight, "POST", "/api/backups/preflight", body)
	if w.Code != 200 {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}

	var got struct {
		OK     bool `json:"ok"`
		Groups []struct {
			ThresholdBytes int64 `json:"threshold_bytes"`
			Volumes        []struct {
				Name           string `json:"name"`
				EstimatedBytes int64  `json:"estimated_bytes"`
			} `json:"volumes"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if !got.OK || len(got.Groups) != 2 {
		t.Fatalf("preflight = %+v, want ok with two groups", got)
	}

	// Nothing may have been written beside the source directory.
	entries, err := os.ReadDir(filepath.Dir(src))
	if err != nil {
		t.Fatalf("reading temp dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".zip") {
			t.Errorf("preflight created %s; it must not compress anything", e.Name())
		}
	}
}

// The fallback, end to end: a source with one oversized file produces numbered
// parts that rejoin into a working archive, and the plan warns about it.
func TestAnOversizedFileFallsBackToBytePartsUnderAuto(t *testing.T) {
	db := splitTestDB(t, 1_000)
	h := &Handlers{db: db, scanMaxDepth: 3, splitMethod: archive.MethodAuto}
	src := splitTestSourceWith(t, map[string]int{
		"movies/huge.mkv": 5_000,
		"docs/small.txt":  100,
	}, true)

	plan, err := h.buildUploadPlan(src, []int64{1})
	if err != nil {
		t.Fatalf("buildUploadPlan: %v", err)
	}
	if !plan.OK() {
		t.Fatalf("plan blocked, want the byte-part fallback: %+v", plan.Blockers)
	}
	if !plan.Groups[0].ByteParts {
		t.Fatal("group is not marked as byte parts, so the UI would call them archives")
	}
	// A warning, not a blocker — but it must be there, because the user cannot
	// open one of these files on its own and needs to know before committing.
	var warned bool
	for _, wn := range plan.Warnings {
		if wn.Reason == "byte_parts" && strings.Contains(wn.Message, "rejoined") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("warnings = %+v, want one explaining the parts must be rejoined", plan.Warnings)
	}

	built, err := h.buildArchives(src, plan, nil)
	defer built.discard()
	if err != nil {
		t.Fatalf("buildArchives: %v", err)
	}
	if len(built) < 2 {
		t.Fatalf("wrote %d archives, want several parts", len(built))
	}

	// Only the first part carries the tree: the others are slices of the same
	// archive, and claiming each holds the whole source would be a lie.
	if built[0].treeJSON == "" {
		t.Error("the first part has no recorded tree")
	}
	for i, a := range built[1:] {
		if a.treeJSON != "" {
			t.Errorf("part %d carries a tree; only the first part represents the set", i+2)
		}
		if !archive.IsPartName(a.remoteName) {
			t.Errorf("part named %q is not recognisable as a part", a.remoteName)
		}
	}

	// Rejoined, the parts must be a working archive holding both source files.
	var joined []byte
	for i, a := range built {
		b, err := os.ReadFile(a.zipPath)
		if err != nil {
			t.Fatalf("reading part %d: %v", i, err)
		}
		joined = append(joined, b...)
	}
	joinedPath := filepath.Join(t.TempDir(), "rejoined.zip")
	if err := os.WriteFile(joinedPath, joined, 0o644); err != nil {
		t.Fatalf("writing rejoined archive: %v", err)
	}
	r, err := zip.OpenReader(joinedPath)
	if err != nil {
		t.Fatalf("the parts do not rejoin into a readable archive: %v", err)
	}
	defer r.Close()
	if len(r.File) != 2 {
		t.Errorf("rejoined archive holds %d entries, want 2", len(r.File))
	}
}

// whole_files keeps today's refusal, for someone who would rather be told than
// receive files they cannot open individually.
func TestWholeFilesMethodStillRefusesAnOversizedFile(t *testing.T) {
	db := splitTestDB(t, 1_000)
	h := &Handlers{db: db, scanMaxDepth: 3, splitMethod: archive.MethodWholeFiles}
	src := splitTestSource(t, map[string]int{"movies/huge.mkv": 5_000})

	plan, err := h.buildUploadPlan(src, []int64{1})
	if err != nil {
		t.Fatalf("buildUploadPlan: %v", err)
	}
	if plan.OK() {
		t.Fatal("plan reported OK; whole_files must report the oversized file")
	}
	if plan.Blockers[0].Reason != "file_too_large" {
		t.Errorf("reason = %q, want file_too_large", plan.Blockers[0].Reason)
	}
}
