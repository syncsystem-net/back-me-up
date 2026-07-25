package autosync

import (
	"archive/zip"
	"bytes"
	"testing"

	"github.com/syncsystem-net/back-me-up/internal/scanner"
)

// child finds a direct child by name, failing the test when it is absent.
func child(t *testing.T, n *scanner.Node, name string) *scanner.Node {
	t.Helper()
	for _, c := range n.Children {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("node %q has no child %q (children: %v)", n.Name, name, childNames(n))
	return nil
}

func childNames(n *scanner.Node) []string {
	var out []string
	for _, c := range n.Children {
		out = append(out, c.Name)
	}
	return out
}

func hasChild(n *scanner.Node, name string) bool {
	for _, c := range n.Children {
		if c.Name == name {
			return true
		}
	}
	return false
}

// An archive written by this tool wraps everything in a directory named after
// the source folder, which is also the archive's own name. That wrapper is the
// root — the tree must not come out as "bkup003 > bkup003".
func TestTreeFromEntriesUsesTheArchiveWrapperAsRoot(t *testing.T) {
	entries := []ZipEntry{
		{Name: "bkup003/notes.txt", Size: 100},
		{Name: "bkup003/docs/a.pdf", Size: 200},
	}

	root := TreeFromEntries("bkup003.zip", entries, scanner.Options{MaxDepth: 3})

	if root.Name != "bkup003" {
		t.Fatalf("expected root %q, got %q", "bkup003", root.Name)
	}
	if hasChild(root, "bkup003") {
		t.Fatalf("wrapper directory was nested under itself: %v", childNames(root))
	}
	if root.SizeBytes != 100 {
		t.Errorf("root size should count only its own files, got %d", root.SizeBytes)
	}
	if got := child(t, root, "docs").SizeBytes; got != 200 {
		t.Errorf("docs size = %d, want 200", got)
	}
}

// Directories are frequently implied by an entry path with no explicit entry of
// their own; building from the flags alone would lose them entirely.
func TestTreeFromEntriesCreatesImpliedDirectories(t *testing.T) {
	entries := []ZipEntry{
		{Name: "bkup/a/b/c/deep.txt", Size: 42},
	}

	root := TreeFromEntries("bkup.zip", entries, scanner.Options{MaxDepth: 5})

	a := child(t, root, "a")
	b := child(t, a, "b")
	c := child(t, b, "c")
	if c.SizeBytes != 42 {
		t.Errorf("deepest directory size = %d, want 42", c.SizeBytes)
	}
	if a.SizeBytes != 0 || b.SizeBytes != 0 {
		t.Errorf("intermediate directories should hold no files directly, got %d/%d", a.SizeBytes, b.SizeBytes)
	}
}

// An explicit directory entry (name ending in "/") must create the directory
// even when nothing inside it was archived, and must not contribute size.
func TestTreeFromEntriesHonoursExplicitEmptyDirectoryEntries(t *testing.T) {
	entries := []ZipEntry{
		{Name: "bkup/empty/", Size: 0, Dir: true},
		{Name: "bkup/full/x.bin", Size: 7},
	}

	root := TreeFromEntries("bkup.zip", entries, scanner.Options{MaxDepth: 3})

	empty := child(t, root, "empty")
	if empty.SizeBytes != 0 || len(empty.Children) != 0 {
		t.Errorf("empty directory should be present but bare, got %+v", empty)
	}
	if got := child(t, root, "full").SizeBytes; got != 7 {
		t.Errorf("full size = %d, want 7", got)
	}
}

// MaxDepth counts levels BELOW the root: 2 records the root plus two levels, and
// a directory sitting exactly at the cap is still recorded (with its size) but
// its children are not.
func TestTreeFromEntriesRespectsMaxDepth(t *testing.T) {
	entries := []ZipEntry{
		{Name: "bkup/l1/l2/l3/file.txt", Size: 5},
		{Name: "bkup/l1/l2/atlevel2.txt", Size: 9},
	}

	root := TreeFromEntries("bkup.zip", entries, scanner.Options{MaxDepth: 2})

	l1 := child(t, root, "l1")
	l2 := child(t, l1, "l2")
	if l2.SizeBytes != 9 {
		t.Errorf("level-2 directory should still record its own files, got %d", l2.SizeBytes)
	}
	if len(l2.Children) != 0 {
		t.Errorf("level 3 is past the cap and must not be recorded, got %v", childNames(l2))
	}
}

func TestTreeFromEntriesDefaultsMaxDepthWhenUnset(t *testing.T) {
	entries := []ZipEntry{{Name: "bkup/a/b/c/d/too-deep.txt", Size: 1}}

	root := TreeFromEntries("bkup.zip", entries, scanner.Options{}) // MaxDepth 0

	a := child(t, root, "a")
	b := child(t, a, "b")
	c := child(t, b, "c")
	if len(c.Children) != 0 {
		t.Errorf("default depth of 3 should stop below %q, got %v", c.Name, childNames(c))
	}
}

// Exclusion uses the same accent- and case-insensitive matcher the local scanner
// and global search share, and it removes the matching directory's whole subtree.
func TestTreeFromEntriesAppliesExcludeTerms(t *testing.T) {
	entries := []ZipEntry{
		// Written with an explicit U+0301 escape (base letter + combining acute):
		// a literal accented character in this source file would be stored
		// precomposed and would never exercise the decomposed form macOS returns.
		{Name: "bkup/Conteu\u0301do/inner/x.txt", Size: 10},
		{Name: "bkup/Layouts/y.txt", Size: 20},
		{Name: "bkup/keep/z.txt", Size: 30},
	}

	root := TreeFromEntries("bkup.zip", entries, scanner.Options{
		MaxDepth:     4,
		ExcludeTerms: []string{"conteudo", "layout"},
	})

	if len(root.Children) != 1 {
		t.Fatalf("expected only the non-excluded directory, got %v", childNames(root))
	}
	if root.Children[0].Name != "keep" {
		t.Errorf("expected %q to survive, got %q", "keep", root.Children[0].Name)
	}
}

// The root is the archive itself, which the user chose — it is never excluded,
// matching the local scanner's rule.
func TestTreeFromEntriesNeverExcludesTheRoot(t *testing.T) {
	entries := []ZipEntry{{Name: "Layouts/file.txt", Size: 3}}

	root := TreeFromEntries("Layouts.zip", entries, scanner.Options{
		MaxDepth:     3,
		ExcludeTerms: []string{"layout"},
	})

	if root == nil || root.Name != "Layouts" {
		t.Fatalf("root must survive its own name matching a term, got %+v", root)
	}
	if root.SizeBytes != 3 {
		t.Errorf("root size = %d, want 3", root.SizeBytes)
	}
}

// A foreign archive with several top-level entries has no single wrapper, so a
// synthetic root named after the archive holds them.
func TestTreeFromEntriesSynthesisesRootForMultipleTopLevelEntries(t *testing.T) {
	entries := []ZipEntry{
		{Name: "one/a.txt", Size: 1},
		{Name: "two/b.txt", Size: 2},
		{Name: "loose.txt", Size: 4},
	}

	root := TreeFromEntries("mixed.zip", entries, scanner.Options{MaxDepth: 3})

	if root.Name != "mixed" {
		t.Fatalf("expected synthetic root %q, got %q", "mixed", root.Name)
	}
	if root.SizeBytes != 4 {
		t.Errorf("top-level files belong to the root, got size %d", root.SizeBytes)
	}
	if len(root.Children) != 2 {
		t.Errorf("expected both top-level directories, got %v", childNames(root))
	}
}

// A single top-level directory that is NOT the archive's name is real content,
// not a wrapper, so it must stay one level down.
func TestTreeFromEntriesKeepsMismatchedSingleTopLevelDirectory(t *testing.T) {
	entries := []ZipEntry{{Name: "something-else/a.txt", Size: 1}}

	root := TreeFromEntries("archive.zip", entries, scanner.Options{MaxDepth: 3})

	if root.Name != "archive" {
		t.Fatalf("expected synthetic root %q, got %q", "archive", root.Name)
	}
	child(t, root, "something-else")
}

// Entry names are untrusted; a "../" path must not climb out of the tree.
func TestTreeFromEntriesDropsTraversalPaths(t *testing.T) {
	entries := []ZipEntry{
		{Name: "../escape.txt", Size: 1},
		{Name: "/absolute/a.txt", Size: 2},
		{Name: "bkup/ok.txt", Size: 3},
	}

	root := TreeFromEntries("bkup.zip", entries, scanner.Options{MaxDepth: 3})

	if hasChild(root, "..") {
		t.Errorf("traversal component leaked into the tree: %v", childNames(root))
	}
	// The absolute path is kept, rooted at the tree rather than the filesystem.
	if !hasChild(root, "absolute") {
		t.Errorf("expected the absolute path to be re-rooted, got %v", childNames(root))
	}
}

// Two crawls of the same unchanged archive must serialize identically, or an
// idempotent re-run would still look like a change.
func TestTreeFromEntriesIsDeterministic(t *testing.T) {
	entries := []ZipEntry{
		{Name: "bkup/zeta/a.txt", Size: 1},
		{Name: "bkup/alpha/b.txt", Size: 2},
		{Name: "bkup/mid/c.txt", Size: 3},
	}
	opts := scanner.Options{MaxDepth: 3}

	first := scanner.TreeJSON(TreeFromEntries("bkup.zip", entries, opts))
	for i := 0; i < 5; i++ {
		if got := scanner.TreeJSON(TreeFromEntries("bkup.zip", entries, opts)); got != first {
			t.Fatalf("tree serialization is not stable:\n%s\n%s", first, got)
		}
	}
}

// End-to-end over a real archive: build a zip, parse it with archive/zip, and
// confirm the tree matches what a local scan of the same layout would record.
func TestTreeFromRealZipArchive(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	files := map[string]string{
		"bkup009/top.txt":            "aaaa",
		"bkup009/docs/report.pdf":    "bbbbbb",
		"bkup009/docs/img/photo.jpg": "cc",
	}
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("creating zip entry %s: %v", name, err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatalf("writing zip entry %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("closing zip: %v", err)
	}

	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("reading zip: %v", err)
	}

	root := TreeFromEntries("bkup009.zip", EntriesFromReader(zr), scanner.Options{MaxDepth: 3})

	if root.Name != "bkup009" || root.SizeBytes != 4 {
		t.Fatalf("unexpected root: %+v", root)
	}
	docs := child(t, root, "docs")
	if docs.SizeBytes != 6 {
		t.Errorf("docs size = %d, want 6", docs.SizeBytes)
	}
	if got := child(t, docs, "img").SizeBytes; got != 2 {
		t.Errorf("img size = %d, want 2", got)
	}
}
