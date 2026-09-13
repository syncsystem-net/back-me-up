package archive

import (
	"archive/zip"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// linkDir makes link point at target, using whichever mechanism this OS allows
// without special privilege: a junction on Windows (mklink /J needs no elevation,
// where a symlink does), a symlink everywhere else.
//
// It skips rather than fails when the OS refuses. A machine that cannot create
// links cannot exercise this behaviour, and pretending otherwise with a fake
// FileInfo would test the fake, not the walk.
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

// outsideDir is a directory that is NOT under the source, holding one uniquely
// named file. If that name ever turns up in an archive, a link was followed.
func outsideDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(filepath.Join(dir, "nested"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nested", "secret.bin"), make([]byte, 128), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return dir
}

func zipEntries(t *testing.T, zipPath string) []string {
	t.Helper()
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatalf("opening %s: %v", zipPath, err)
	}
	defer r.Close()

	var names []string
	for _, f := range r.File {
		names = append(names, f.Name)
	}
	return names
}

// The failure this whole change exists for: before it, a junction in the source
// failed the entire backup with Windows' "Incorrect function" when the walk
// tried to io.Copy a directory handle.
func TestZipCompletesWithALinkInTheSource(t *testing.T) {
	src := buildTree(t, map[string]int{"real/a.bin": 100, "loose.txt": 20})
	linkDir(t, filepath.Join(src, "linked"), outsideDir(t))

	zipPath, err := Zip(src)
	if err != nil {
		t.Fatalf("Zip with a link in the source: %v", err)
	}
	defer os.Remove(zipPath)

	base := filepath.Base(src)
	entries := zipEntries(t, zipPath)

	for _, want := range []string{base + "/real/a.bin", base + "/loose.txt"} {
		if !contains(entries, want) {
			t.Errorf("archive is missing %q; skipping the link cost a real file", want)
		}
	}
	for _, got := range entries {
		if strings.Contains(got, "linked") || strings.Contains(got, "secret.bin") {
			t.Errorf("entry %q: the link was followed or archived", got)
		}
	}
}

// A link is skipped, never followed — so nothing outside the source can be
// pulled into the archive, and a link pointing at an ancestor cannot loop.
func TestALinkPointingOutsideTheSourceArchivesNothingExtra(t *testing.T) {
	src := buildTree(t, map[string]int{"real/a.bin": 100})
	linkDir(t, filepath.Join(src, "escape"), outsideDir(t))
	linkDir(t, filepath.Join(src, "loop"), src)

	plan, err := PlanVolumes(src, 0, MethodAuto)
	if err != nil {
		t.Fatalf("PlanVolumes: %v", err)
	}
	if len(plan.SkippedLinks) != 2 {
		t.Fatalf("SkippedLinks = %v, want both links named", plan.SkippedLinks)
	}

	zipPath, err := Zip(src)
	if err != nil {
		t.Fatalf("Zip: %v", err)
	}
	defer os.Remove(zipPath)

	if entries := zipEntries(t, zipPath); len(entries) != 1 {
		t.Fatalf("archive holds %v, want only the one real file", entries)
	}
}

func TestPlanNamesEverySkippedLink(t *testing.T) {
	src := buildTree(t, map[string]int{"real/a.bin": 100, "deep/inner/b.bin": 100})
	linkDir(t, filepath.Join(src, "top-link"), outsideDir(t))
	linkDir(t, filepath.Join(src, "deep", "inner-link"), outsideDir(t))

	plan, err := PlanVolumes(src, 0, MethodAuto)
	if err != nil {
		t.Fatalf("PlanVolumes: %v", err)
	}

	want := []string{"deep/inner-link", "top-link"}
	got := plan.SkippedLinks
	if len(got) != len(want) {
		t.Fatalf("SkippedLinks = %v, want %v", got, want)
	}
	for i := range want {
		// Sorted, so the order is part of the contract: the same source planned
		// twice must produce the same message.
		if filepath.ToSlash(got[i]) != want[i] {
			t.Errorf("SkippedLinks[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// A link must not consume packing capacity either. Sized as a file it would be
// packed as an item; measured as a directory it would add its target's bytes.
func TestALinkContributesNoBytesToThePlan(t *testing.T) {
	src := buildTree(t, map[string]int{"a.bin": 500})
	withoutLink, err := PlanVolumes(src, 0, MethodAuto)
	if err != nil {
		t.Fatalf("PlanVolumes: %v", err)
	}

	linkDir(t, filepath.Join(src, "linked"), outsideDir(t))
	withLink, err := PlanVolumes(src, 0, MethodAuto)
	if err != nil {
		t.Fatalf("PlanVolumes after linking: %v", err)
	}

	if withLink.TotalBytes != withoutLink.TotalBytes {
		t.Errorf("TotalBytes changed from %d to %d when a link was added",
			withoutLink.TotalBytes, withLink.TotalBytes)
	}
}

// The parity that PR #14 established between the plan and the zip has to survive
// links: every item the plan assigns must be archivable, and every archived
// entry must be one the plan accounted for.
func TestThePlanAndTheZipAgreeAboutLinks(t *testing.T) {
	src := buildTree(t, map[string]int{
		"alpha/a.bin": 600, "beta/b.bin": 600, "gamma/c.bin": 600,
	})
	linkDir(t, filepath.Join(src, "alpha", "link"), outsideDir(t))
	linkDir(t, filepath.Join(src, "top-link"), outsideDir(t))

	plan, err := PlanVolumes(src, 1000, MethodAuto)
	if err != nil {
		t.Fatalf("PlanVolumes: %v", err)
	}
	if !plan.Split() {
		t.Fatal("expected a split for this fixture")
	}

	base := filepath.Base(src)
	seen := map[string]bool{}
	for i, vol := range plan.Volumes {
		zipPath, err := ZipItems(src, vol.Items)
		if err != nil {
			t.Fatalf("ZipItems(volume %d): %v", i, err)
		}
		for _, name := range zipEntries(t, zipPath) {
			seen[name] = true
		}
		os.Remove(zipPath)
	}

	for _, want := range []string{base + "/alpha/a.bin", base + "/beta/b.bin", base + "/gamma/c.bin"} {
		if !seen[want] {
			t.Errorf("no volume contains %q — the split lost a file", want)
		}
	}
	if len(seen) != 3 {
		t.Errorf("volumes hold %d entries, want 3: %v", len(seen), seen)
	}
}

// Pointing the backup at a link is not the same as meeting one inside it. Neither
// walk descends into its own root, so this would silently produce an empty
// archive — the answer has to be an explanation instead.
func TestASourceThatIsItselfALinkIsRefused(t *testing.T) {
	real := buildTree(t, map[string]int{"a.bin": 100})
	link := filepath.Join(t.TempDir(), "link-to-source")
	linkDir(t, link, real)

	if _, err := PlanVolumes(link, 0, MethodAuto); err == nil {
		t.Error("PlanVolumes accepted a source that is a link")
	} else {
		if !strings.Contains(err.Error(), "link") {
			t.Errorf("error %q does not tell the user the source is a link", err)
		}
		// The sentinel is what lets the handler tag this as its own actionable
		// case rather than lumping it in with "could not read the source".
		if !errors.Is(err, ErrSourceIsLink) {
			t.Errorf("error %v does not wrap ErrSourceIsLink", err)
		}
	}

	zipPath, err := Zip(link)
	if err == nil {
		os.Remove(zipPath)
		t.Fatal("Zip accepted a source that is a link")
	}
	if !strings.Contains(err.Error(), "link") {
		t.Errorf("error %q does not tell the user the source is a link", err)
	}
}

// The byte-parts fallback walks the source too, so it must report links the same
// way — a user who lands on that path is no less entitled to know.
func TestBytePartsPlanAlsoNamesSkippedLinks(t *testing.T) {
	src := buildTree(t, map[string]int{"a.bin": 500})
	linkDir(t, filepath.Join(src, "linked"), outsideDir(t))

	plan, err := PlanBytes(src, 200)
	if err != nil {
		t.Fatalf("PlanBytes: %v", err)
	}
	if len(plan.SkippedLinks) != 1 {
		t.Fatalf("SkippedLinks = %v, want the one link named", plan.SkippedLinks)
	}
}
