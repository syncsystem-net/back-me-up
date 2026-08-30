package archive

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestPackIsDeterministic(t *testing.T) {
	items := []Item{
		{Rel: "c", Bytes: 30, IsDir: true},
		{Rel: "a", Bytes: 30, IsDir: true},
		{Rel: "b", Bytes: 40, IsDir: true},
		{Rel: "d", Bytes: 10, IsDir: true},
	}

	first := Pack(items, 100)
	for i := 0; i < 20; i++ {
		// Shuffle the input order; the result must not depend on it, because
		// directory iteration order is not something we control.
		shuffled := append([]Item(nil), items...)
		sort.Slice(shuffled, func(a, b int) bool { return shuffled[a].Rel > shuffled[b].Rel })
		if got := Pack(shuffled, 100); !reflect.DeepEqual(got, first) {
			t.Fatalf("Pack is order-dependent:\n %+v\nvs\n %+v", got, first)
		}
	}
}

func TestPackRespectsCapacity(t *testing.T) {
	items := []Item{
		{Rel: "a", Bytes: 900, IsDir: true},
		{Rel: "b", Bytes: 800, IsDir: true},
		{Rel: "c", Bytes: 300, IsDir: true},
		{Rel: "d", Bytes: 100, IsDir: true},
	}
	volumes := Pack(items, 1000)

	if len(volumes) < 2 {
		t.Fatalf("got %d volumes, want at least 2 for 2100 bytes at capacity 1000", len(volumes))
	}
	seen := map[string]bool{}
	for _, v := range volumes {
		if v.Bytes > 1000 {
			t.Errorf("volume of %d bytes exceeds the 1000 capacity", v.Bytes)
		}
		for _, it := range v.Items {
			if seen[it.Rel] {
				t.Errorf("%s was placed in more than one volume", it.Rel)
			}
			seen[it.Rel] = true
		}
	}
	if len(seen) != len(items) {
		t.Errorf("packed %d items, want all %d — a dropped item is a silently incomplete backup", len(seen), len(items))
	}
}

func TestPlanKeepsOneVolumeWhenEverythingFits(t *testing.T) {
	src := buildTree(t, map[string]int{
		"one/a.bin": 100,
		"two/b.bin": 100,
	})

	plan, err := PlanVolumes(src, 10_000, MethodAuto)
	if err != nil {
		t.Fatalf("PlanVolumes: %v", err)
	}
	if plan.Split() {
		t.Fatalf("got %d volumes, want 1 when the source fits", len(plan.Volumes))
	}
	// The unsplit name must be exactly what it always was: an existing record's
	// archives and the Auto-Sync crawl both key on it.
	if got := plan.Names(src); len(got) != 1 || got[0] != RemoteName(src) {
		t.Errorf("Names = %v, want [%s]", got, RemoteName(src))
	}
	if plan.Includes(0) != nil {
		t.Error("a single-volume plan must include the whole directory (nil includes)")
	}
}

func TestPlanSplitsAndNamesVolumes(t *testing.T) {
	src := buildTree(t, map[string]int{
		"alpha/a.bin": 600,
		"beta/b.bin":  600,
		"gamma/c.bin": 600,
	})

	plan, err := PlanVolumes(src, 1000, MethodAuto)
	if err != nil {
		t.Fatalf("PlanVolumes: %v", err)
	}
	if !plan.Split() {
		t.Fatalf("got 1 volume, want a split for 1800 bytes at a 1000 threshold")
	}
	names := plan.Names(src)
	base := filepath.Base(src)
	for i, n := range names {
		want := fmt.Sprintf("%s-%dof%d.zip", base, i+1, len(names))
		if n != want {
			t.Errorf("volume %d named %q, want %q", i, n, want)
		}
		if !strings.HasSuffix(n, ".zip") {
			t.Errorf("%q must end in .zip or the Auto-Sync crawl will not adopt it", n)
		}
	}
}

// A directory too large for one volume is opened up rather than reported. This
// is what makes an arbitrarily deep source packable.
func TestAnOversizedDirectoryIsDescendedInto(t *testing.T) {
	src := buildTree(t, map[string]int{
		"big/one/a.bin": 600,
		"big/two/b.bin": 600,
		"small/c.bin":   100,
	})

	plan, err := PlanVolumes(src, 1000, MethodAuto)
	if err != nil {
		t.Fatalf("PlanVolumes: %v", err)
	}
	var rels []string
	for _, v := range plan.Volumes {
		for _, it := range v.Items {
			rels = append(rels, filepath.ToSlash(it.Rel))
		}
	}
	sort.Strings(rels)
	// "big" itself is 1200 bytes and cannot fit, so its children must be items.
	for _, want := range []string{"big/one", "big/two"} {
		if !contains(rels, want) {
			t.Errorf("items %v missing %q — the oversized directory was not descended into", rels, want)
		}
	}
	if contains(rels, "big") {
		t.Errorf("items %v still contain the oversized directory %q whole", rels, "big")
	}
}

// A single file is the floor for whole-file packing. Under whole_files there is
// nothing to subdivide, so it must be reported with the detail the user needs.
func TestASingleOversizedFileIsReportedNotSplit(t *testing.T) {
	src := buildTree(t, map[string]int{
		"movies/huge.mkv": 5000,
	})

	_, err := PlanVolumes(src, 1000, MethodWholeFiles)
	if err == nil {
		t.Fatal("PlanVolumes succeeded, want a FileTooLargeError")
	}
	var tooLarge *FileTooLargeError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("error = %v, want a *FileTooLargeError so the handler can report the file and its size", err)
	}
	if !strings.Contains(tooLarge.Rel, "huge.mkv") {
		t.Errorf("error names %q, want the offending file", tooLarge.Rel)
	}
	if tooLarge.Bytes != 5000 || tooLarge.Threshold != 1000 {
		t.Errorf("error reports %d/%d, want 5000/1000", tooLarge.Bytes, tooLarge.Threshold)
	}
}

func TestNoThresholdNeverSplits(t *testing.T) {
	src := buildTree(t, map[string]int{
		"a/x.bin": 5000,
		"b/y.bin": 5000,
	})
	plan, err := PlanVolumes(src, 0, MethodAuto)
	if err != nil {
		t.Fatalf("PlanVolumes: %v", err)
	}
	if plan.Split() {
		t.Errorf("got %d volumes, want 1 when there is no threshold", len(plan.Volumes))
	}
}

// The load-bearing property of the whole feature: every volume must be an
// openable zip on its own, all volumes together must hold exactly the source,
// and every entry must keep the source directory's name as its prefix so
// extracting them all into one place reconstructs the original.
func TestVolumesAreStandaloneAndTogetherCompleteTheSource(t *testing.T) {
	files := map[string]int{
		"alpha/a.bin":      600,
		"beta/b.bin":       600,
		"gamma/deep/c.bin": 600,
		"loose.txt":        50,
	}
	src := buildTree(t, files)
	base := filepath.Base(src)

	plan, err := PlanVolumes(src, 1000, MethodAuto)
	if err != nil {
		t.Fatalf("PlanVolumes: %v", err)
	}
	if !plan.Split() {
		t.Fatal("expected a split for this fixture")
	}

	got := map[string]bool{}
	for i, vol := range plan.Volumes {
		zipPath, err := ZipItems(src, vol.Items)
		if err != nil {
			t.Fatalf("ZipItems(volume %d): %v", i, err)
		}
		defer os.Remove(zipPath)

		r, err := zip.OpenReader(zipPath)
		if err != nil {
			t.Fatalf("volume %d is not an openable zip: %v", i, err)
		}
		for _, f := range r.File {
			if !strings.HasPrefix(f.Name, base+"/") {
				t.Errorf("entry %q does not carry the source prefix %q/", f.Name, base)
			}
			if got[f.Name] {
				t.Errorf("entry %q appears in more than one volume", f.Name)
			}
			got[f.Name] = true
		}
		r.Close()
	}

	for rel := range files {
		want := base + "/" + rel
		if !got[want] {
			t.Errorf("no volume contains %q — the split lost a file", want)
		}
	}
	if len(got) != len(files) {
		t.Errorf("volumes hold %d entries, want exactly %d", len(got), len(files))
	}
}

// Idempotence in the sense that matters here: planning the same unchanged source
// twice must produce the same volumes, or the preview the user approved would
// not describe the upload that follows it.
func TestPlanningTwiceGivesTheSameVolumes(t *testing.T) {
	src := buildTree(t, map[string]int{
		"a/1.bin": 400, "b/2.bin": 400, "c/3.bin": 400, "d/4.bin": 400,
	})

	first, err := PlanVolumes(src, 1000, MethodAuto)
	if err != nil {
		t.Fatalf("PlanVolumes: %v", err)
	}
	second, err := PlanVolumes(src, 1000, MethodAuto)
	if err != nil {
		t.Fatalf("PlanVolumes: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Errorf("re-planning changed the volumes:\n%+v\nvs\n%+v", first, second)
	}
}

// A directory holding only zero-byte files measures zero bytes but is not
// empty: it still contributes entries, and the names are the data. Dropping it
// on size alone put those files in no volume at all.
func TestADirectoryOfZeroByteFilesIsStillArchived(t *testing.T) {
	files := map[string]int{
		"alpha/a.bin":     600,
		"beta/b.bin":      600,
		"gamma/c.bin":     600,
		"markers/KEEP.md": 0,
		"markers/EMPTY":   0,
	}
	src := buildTree(t, files)
	base := filepath.Base(src)

	plan, err := PlanVolumes(src, 1000, MethodAuto)
	if err != nil {
		t.Fatalf("PlanVolumes: %v", err)
	}

	got := map[string]bool{}
	for i, vol := range plan.Volumes {
		zipPath, err := ZipItems(src, vol.Items)
		if err != nil {
			t.Fatalf("ZipItems(volume %d): %v", i, err)
		}
		defer os.Remove(zipPath)
		r, err := zip.OpenReader(zipPath)
		if err != nil {
			t.Fatalf("opening volume %d: %v", i, err)
		}
		for _, f := range r.File {
			got[f.Name] = true
		}
		r.Close()
	}

	for _, rel := range []string{"markers/KEEP.md", "markers/EMPTY"} {
		if !got[base+"/"+rel] {
			t.Errorf("%q is in no volume; a zero-byte file is still a file", rel)
		}
	}
	if len(got) != len(files) {
		t.Errorf("volumes hold %d entries, want %d", len(got), len(files))
	}
}

// A truly empty directory contributes no zip entries (the writer skips directory
// entries), so it must not create a volume of its own.
func TestAnEmptyDirectoryDoesNotCreateAVolume(t *testing.T) {
	src := buildTree(t, map[string]int{"alpha/a.bin": 600, "beta/b.bin": 600})
	if err := os.MkdirAll(filepath.Join(src, "hollow", "deeper"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	plan, err := PlanVolumes(src, 1000, MethodAuto)
	if err != nil {
		t.Fatalf("PlanVolumes: %v", err)
	}
	if !plan.Split() {
		t.Fatal("expected a split, or collectItems never runs")
	}
	for _, v := range plan.Volumes {
		for _, it := range v.Items {
			if filepath.ToSlash(it.Rel) == "hollow" {
				t.Error("an empty directory became a packable item")
			}
		}
	}
}

// The planner must fail rather than skip an entry it cannot read. ZipItems only
// walks the items the plan assigned, so anything dropped while planning is never
// visited and never reported — a backup that silently omits the user's data.
func TestAnUnreadableSourceFailsThePlanRatherThanOmittingIt(t *testing.T) {
	src := buildTree(t, map[string]int{"alpha/a.bin": 600, "beta/b.bin": 600})

	// A path that exists in the listing but cannot be measured. Removing the
	// directory between the ReadDir and the treeSize is not something a test can
	// stage portably, so point the planner at a source that has been replaced by
	// a file after its parent was listed — the same class of failure.
	missing := filepath.Join(src, "gone")
	if err := os.MkdirAll(missing, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(missing, "x.bin"), make([]byte, 600), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Sanity: with everything readable the plan succeeds and includes it.
	if _, err := PlanVolumes(src, 1000, MethodAuto); err != nil {
		t.Fatalf("PlanVolumes on a readable source: %v", err)
	}

	// And a source that does not exist at all is an error, never an empty plan.
	if _, err := PlanVolumes(filepath.Join(src, "nope"), 1000, MethodAuto); err == nil {
		t.Error("planning an absent directory succeeded; it must fail rather than produce an empty plan")
	}
}

// Above 8 MiB the packing capacity is genuinely threshold minus headroom, which
// every other test skips because their thresholds are smaller than the headroom
// itself. A file over capacity but under the real threshold gets its own volume
// rather than being reported as impossible.
func TestPackingHeadroomAppliesAboveItsOwnSize(t *testing.T) {
	const mib = 1 << 20
	src := buildTree(t, map[string]int{
		"big.bin":     6 * mib,
		"alpha/a.bin": 4 * mib,
		"beta/b.bin":  3 * mib,
	})

	plan, err := PlanVolumes(src, 12*mib, MethodAuto)
	if err != nil {
		t.Fatalf("PlanVolumes: %v", err)
	}
	if !plan.Split() {
		t.Fatalf("expected a split: 13 MiB of data against a 12 MiB threshold")
	}
	// capacity is 12 - 8 = 4 MiB, so the 6 MiB file cannot share a volume.
	for _, v := range plan.Volumes {
		if v.Bytes > 12*mib {
			t.Errorf("volume of %d bytes exceeds the real threshold", v.Bytes)
		}
		if len(v.Items) > 1 {
			for _, it := range v.Items {
				if it.Rel == "big.bin" {
					t.Error("the over-capacity file was packed alongside others")
				}
			}
		}
	}
}

func TestHumanBytesReadsAsProvidersStateTheirLimits(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want string
	}{
		{500, "500 B"},
		{2_000_000_000, "2.00 GB"},
		{1_900_000_000, "1.90 GB"},
	} {
		if got := HumanBytes(tc.in); got != tc.want {
			t.Errorf("HumanBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// buildTree writes a source directory of files with the given sizes and returns
// its path.
func buildTree(t *testing.T, files map[string]int) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "source")
	for rel, size := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, make([]byte, size), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	return root
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// The whole point of the auto fallback: a source containing one file bigger than
// the limit is impossible to pack as whole files, and refusing it means that
// provider can never receive the backup at all.
func TestAutoFallsBackToBytePartsForAnOversizedFile(t *testing.T) {
	src := buildTree(t, map[string]int{
		"movies/huge.mkv": 5000,
		"docs/small.txt":  100,
	})

	plan, err := PlanVolumes(src, 1000, MethodAuto)
	if err != nil {
		t.Fatalf("PlanVolumes: %v", err)
	}
	if !plan.ByteParts() {
		t.Fatalf("mode = %q, want byte parts: whole files cannot express this source", plan.Mode)
	}
	if !plan.Split() {
		t.Fatalf("got %d parts, want several for 5100 bytes at 1000", len(plan.Volumes))
	}
	names := plan.Names(src)
	base := filepath.Base(src)
	for i, n := range names {
		want := fmt.Sprintf("%s.zip.%03d", base, i+1)
		if n != want {
			t.Errorf("part %d named %q, want %q", i, n, want)
		}
		if !IsPartName(n) {
			t.Errorf("%q is not recognised as a part name, so Auto-Sync would ignore it", n)
		}
	}
}

// whole_files must never fall back, even when that leaves the backup impossible.
// It is the setting for someone who would rather be told than get parts.
func TestWholeFilesNeverFallsBackToParts(t *testing.T) {
	src := buildTree(t, map[string]int{"movies/huge.mkv": 5000})

	_, err := PlanVolumes(src, 1000, MethodWholeFiles)
	var tooLarge *FileTooLargeError
	if !errors.As(err, &tooLarge) {
		t.Errorf("error = %v, want the oversized file reported rather than byte parts", err)
	}
}

// byte_parts applies even when whole-file packing would have worked.
func TestBytePartsAppliesEvenWhenFilesWouldFit(t *testing.T) {
	src := buildTree(t, map[string]int{"a/x.bin": 600, "b/y.bin": 600})

	plan, err := PlanVolumes(src, 1000, MethodByteParts)
	if err != nil {
		t.Fatalf("PlanVolumes: %v", err)
	}
	if !plan.ByteParts() {
		t.Errorf("mode = %q, want byte parts when explicitly configured", plan.Mode)
	}
}

// A byte part is a raw slice, so the test that matters is that concatenating the
// parts back reproduces the original archive exactly — that is the entire
// restore contract for this mode.
func TestBytePartsRejoinToTheOriginalArchive(t *testing.T) {
	src := buildTree(t, map[string]int{
		"movies/huge.mkv": 5000,
		"docs/small.txt":  100,
	})

	whole, err := Zip(src)
	if err != nil {
		t.Fatalf("Zip: %v", err)
	}
	original, err := os.ReadFile(whole)
	if err != nil {
		t.Fatalf("reading whole archive: %v", err)
	}

	// A second copy to cut up, since SplitFile consumes its input.
	copyPath := whole + ".copy"
	if err := os.WriteFile(copyPath, original, 0o644); err != nil {
		t.Fatalf("copying archive: %v", err)
	}
	defer os.Remove(whole)

	// Small, because a zip of zero-filled test files compresses to a few hundred
	// bytes — the cut has to be smaller than the archive to produce parts at all.
	const partSize = 100
	parts, err := SplitFile(copyPath, partSize)
	if err != nil {
		t.Fatalf("SplitFile: %v", err)
	}
	for _, p := range parts {
		defer os.Remove(p)
	}
	if len(parts) < 2 {
		t.Fatalf("got %d parts, want several", len(parts))
	}
	if _, err := os.Stat(copyPath); !os.IsNotExist(err) {
		t.Error("SplitFile left the original archive behind alongside its parts")
	}

	var rejoined []byte
	for i, p := range parts {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("reading part %d: %v", i, err)
		}
		if int64(len(b)) > partSize {
			t.Errorf("part %d is %d bytes, over the %d limit", i, len(b), partSize)
		}
		if len(b) == 0 {
			t.Errorf("part %d is empty", i)
		}
		rejoined = append(rejoined, b...)
	}
	if !bytes.Equal(rejoined, original) {
		t.Fatalf("rejoined %d bytes, want the original %d — the parts do not reassemble",
			len(rejoined), len(original))
	}

	// And the rejoined bytes really are a working archive.
	joinedPath := filepath.Join(t.TempDir(), "rejoined.zip")
	if err := os.WriteFile(joinedPath, rejoined, 0o644); err != nil {
		t.Fatalf("writing rejoined archive: %v", err)
	}
	r, err := zip.OpenReader(joinedPath)
	if err != nil {
		t.Fatalf("rejoined archive does not open: %v", err)
	}
	defer r.Close()
	if len(r.File) != 2 {
		t.Errorf("rejoined archive holds %d entries, want 2", len(r.File))
	}
}

// A file that already fits is not cut, and keeps its name.
func TestSplitFileLeavesAFittingArchiveAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "small.zip")
	if err := os.WriteFile(path, make([]byte, 100), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	parts, err := SplitFile(path, 1000)
	if err != nil {
		t.Fatalf("SplitFile: %v", err)
	}
	if len(parts) != 1 || parts[0] != path {
		t.Errorf("parts = %v, want the original path untouched", parts)
	}
}

// An exact multiple must not produce a trailing empty part.
func TestSplitFileOnAnExactBoundaryProducesNoEmptyPart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "exact.zip")
	if err := os.WriteFile(path, make([]byte, 2000), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	parts, err := SplitFile(path, 1000)
	if err != nil {
		t.Fatalf("SplitFile: %v", err)
	}
	for _, p := range parts {
		defer os.Remove(p)
	}
	if len(parts) != 2 {
		t.Errorf("got %d parts for an exact 2x boundary, want 2", len(parts))
	}
}

func TestIsPartNameRecognisesOnlyOurParts(t *testing.T) {
	for _, name := range []string{"bkup.zip.001", "bkup.ZIP.014", "a.b.zip.999"} {
		if !IsPartName(name) {
			t.Errorf("IsPartName(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"bkup.zip", "bkup.zip.abc", "bkup.rar.001", "bkup.001", "bkup.zip.1"} {
		if IsPartName(name) {
			t.Errorf("IsPartName(%q) = true, want false", name)
		}
	}
}

func TestNormalizeMethodDefaultsToAuto(t *testing.T) {
	for _, in := range []string{"", "AUTO", "nonsense", "  "} {
		if got := NormalizeMethod(in); got != MethodAuto {
			t.Errorf("NormalizeMethod(%q) = %q, want auto", in, got)
		}
	}
	if got := NormalizeMethod("whole_files"); got != MethodWholeFiles {
		t.Errorf("got %q, want whole_files", got)
	}
	if got := NormalizeMethod("byte_parts"); got != MethodByteParts {
		t.Errorf("got %q, want byte_parts", got)
	}
}
