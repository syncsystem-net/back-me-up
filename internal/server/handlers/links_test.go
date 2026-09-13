package handlers

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/syncsystem-net/back-me-up/internal/database"
)

// linkDir mirrors the archive package's helper: a junction on Windows (no
// elevation needed), a symlink elsewhere, and a skip when the OS refuses.
func linkDir(t *testing.T, link, target string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
		if err != nil {
			t.Skipf("cannot create a junction here: %v (%s)", err, strings.TrimSpace(string(out)))
		}
		return
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
}

// The pre-flight is the one moment the user can still change their mind, so a
// link that will be left out has to be named there — before anything is
// compressed, not in a log afterwards.
func TestPreflightWarnsAboutSkippedLinks(t *testing.T) {
	db := splitTestDB(t, 0)
	h := &Handlers{db: db, scanMaxDepth: 3}
	src := splitTestSource(t, map[string]int{"alpha/a.bin": 100})
	linkDir(t, filepath.Join(src, "linked"), t.TempDir())

	plan, err := h.buildUploadPlan(src, []int64{1, 2})
	if err != nil {
		t.Fatalf("buildUploadPlan: %v", err)
	}
	if !plan.OK() {
		t.Fatalf("a link should not block the upload: %+v", plan.Blockers)
	}
	if len(plan.SkippedLinks) != 1 || !strings.Contains(plan.SkippedLinks[0], "linked") {
		t.Fatalf("SkippedLinks = %v, want the one link", plan.SkippedLinks)
	}

	var found string
	for _, w := range plan.Warnings {
		if w.Reason == "skipped_links" {
			found = w.Message
		}
	}
	if found == "" {
		t.Fatalf("no skipped_links warning in %+v", plan.Warnings)
	}
	if !strings.Contains(found, "linked") || !strings.Contains(found, "NOT") {
		t.Errorf("warning does not say what will be missing: %q", found)
	}
}

// Two accounts on different thresholds are planned separately and then merged;
// the link must be reported once, not once per group.
func TestSkippedLinksAreReportedOncePerBackup(t *testing.T) {
	db := splitTestDB(t, 1_000_000)
	h := &Handlers{db: db, scanMaxDepth: 3}
	src := splitTestSource(t, map[string]int{"alpha/a.bin": 100})
	linkDir(t, filepath.Join(src, "linked"), t.TempDir())

	plan, err := h.buildUploadPlan(src, []int64{1, 2})
	if err != nil {
		t.Fatalf("buildUploadPlan: %v", err)
	}
	if len(plan.SkippedLinks) != 1 {
		t.Fatalf("SkippedLinks = %v, want exactly one entry", plan.SkippedLinks)
	}
	count := 0
	for _, w := range plan.Warnings {
		if w.Reason == "skipped_links" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("got %d skipped_links warnings, want 1", count)
	}
}

// The pre-flight warning is gone once the modal closes. The job log is where
// someone asks months later whether a backup is complete.
func TestEveryJobRecordsWhatTheArchiveOmits(t *testing.T) {
	db := splitTestDB(t, 0)
	h := &Handlers{db: db, scanMaxDepth: 3}
	src := splitTestSource(t, map[string]int{"alpha/a.bin": 100})
	linkDir(t, filepath.Join(src, "linked"), t.TempDir())

	plan, err := h.buildUploadPlan(src, []int64{1, 2})
	if err != nil {
		t.Fatalf("buildUploadPlan: %v", err)
	}
	built, err := h.buildArchives(src, plan, nil)
	if err != nil {
		built.discard()
		t.Fatalf("buildArchives: %v", err)
	}
	defer built.discard()

	_, jobIDs, err := h.recordBackup("u@example.com", "Linked", src, built)
	if err != nil {
		t.Fatalf("recordBackup: %v", err)
	}
	if len(jobIDs) == 0 {
		t.Fatal("recordBackup returned no job ids")
	}

	h.logSkippedLinks(jobIDs, plan.SkippedLinks)

	for _, id := range jobIDs {
		logs, err := database.ListJobLogs(db, id)
		if err != nil {
			t.Fatalf("ListJobLogs(%d): %v", id, err)
		}
		var found bool
		for _, l := range logs {
			if strings.Contains(l.Message, "linked") {
				found = true
				if l.Level != "warn" {
					t.Errorf("job %d: level = %q, want warn", id, l.Level)
				}
			}
		}
		if !found {
			t.Errorf("job %d has no record of the skipped link", id)
		}
	}
}

// A backup with nothing skipped must not gain a log line saying so — an empty
// warning on every job would make the real ones invisible.
func TestNoLinksMeansNoJobLog(t *testing.T) {
	db := splitTestDB(t, 0)
	h := &Handlers{db: db, scanMaxDepth: 3}
	src := splitTestSource(t, map[string]int{"alpha/a.bin": 100})

	plan, err := h.buildUploadPlan(src, []int64{1})
	if err != nil {
		t.Fatalf("buildUploadPlan: %v", err)
	}
	if len(plan.SkippedLinks) != 0 {
		t.Fatalf("SkippedLinks = %v, want none", plan.SkippedLinks)
	}
	built, err := h.buildArchives(src, plan, nil)
	if err != nil {
		built.discard()
		t.Fatalf("buildArchives: %v", err)
	}
	defer built.discard()

	_, jobIDs, err := h.recordBackup("u@example.com", "Clean", src, built)
	if err != nil {
		t.Fatalf("recordBackup: %v", err)
	}
	h.logSkippedLinks(jobIDs, plan.SkippedLinks)

	for _, id := range jobIDs {
		logs, err := database.ListJobLogs(db, id)
		if err != nil {
			t.Fatalf("ListJobLogs(%d): %v", id, err)
		}
		if len(logs) != 0 {
			t.Errorf("job %d gained %d log lines with nothing to report", id, len(logs))
		}
	}
}

// A long list is capped so one pathological source cannot produce an unreadable
// warning or an unbounded log row — but the count still tells the truth.
func TestALongLinkListIsCappedButCounted(t *testing.T) {
	links := make([]string, 0, 25)
	for i := 0; i < 25; i++ {
		links = append(links, filepath.Join("dir", "link"+string(rune('a'+i))))
	}
	msg := skippedLinksMessage(links)

	if !strings.Contains(msg, "25 links") {
		t.Errorf("message does not report the real count: %q", msg)
	}
	if !strings.Contains(msg, "and 15 more") {
		t.Errorf("message does not say how many were elided: %q", msg)
	}
	// Count the generated names themselves, not the word "link" — the sentence
	// contains "links" and "Symlinks" too, which would make this pass whatever
	// the cap did.
	named := 0
	for _, l := range links {
		if strings.Contains(msg, l) {
			named++
		}
	}
	if named != maxNamedLinks {
		t.Errorf("message names %d links, want exactly %d: %q", named, maxNamedLinks, msg)
	}
}

// The pre-flight speaks about an archive that does not exist yet; the job log is
// read about one that does. Same facts, different tense — a log line promising
// that files "will be" skipped describes a decision already taken.
func TestTheJobLogIsWrittenAboutAnArchiveThatExists(t *testing.T) {
	links := []string{"one", "two"}

	preview := skippedLinksMessage(links)
	logged := skippedLinksLogMessage(links)

	if !strings.Contains(preview, "will be skipped") {
		t.Errorf("pre-flight message is not about what will happen: %q", preview)
	}
	if strings.Contains(logged, "will be") {
		t.Errorf("job log message is written in the future tense: %q", logged)
	}
	for _, l := range links {
		if !strings.Contains(logged, l) {
			t.Errorf("job log message does not name %q: %q", l, logged)
		}
	}
}

// A source that is a link is blocked, and tagged as its own case rather than as
// an unreadable directory — the remedy is completely different.
func TestASourceThatIsALinkIsBlockedWithItsOwnReason(t *testing.T) {
	db := splitTestDB(t, 0)
	h := &Handlers{db: db, scanMaxDepth: 3}
	real := splitTestSource(t, map[string]int{"alpha/a.bin": 100})
	link := filepath.Join(t.TempDir(), "link-to-src")
	linkDir(t, link, real)

	plan, err := h.buildUploadPlan(link, []int64{1})
	if err != nil {
		t.Fatalf("buildUploadPlan: %v", err)
	}
	if plan.OK() {
		t.Fatal("a source that is a link was accepted")
	}
	var found string
	for _, b := range plan.Blockers {
		if b.Reason == "source_is_link" {
			found = b.Message
		}
	}
	if found == "" {
		t.Fatalf("no source_is_link blocker in %+v", plan.Blockers)
	}
	if !strings.Contains(found, "point the backup at the directory it refers to") {
		t.Errorf("blocker does not name the remedy: %q", found)
	}
}
