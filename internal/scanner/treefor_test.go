package scanner

import (
	"os"
	"path/filepath"
	"testing"
)

func treeSource(t *testing.T, files map[string]int) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "src")
	for rel, size := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, make([]byte, size), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	return root
}

func childNames(n *Node) []string {
	out := make([]string, 0, len(n.Children))
	for _, c := range n.Children {
		out = append(out, c.Name)
	}
	return out
}

func has(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// No includes means the whole directory — the unsplit case, and what every
// existing caller passes.
func TestTreeForWithNoIncludesMatchesTree(t *testing.T) {
	src := treeSource(t, map[string]int{
		"alpha/a.bin": 10,
		"beta/b.bin":  10,
	})

	full, err := Tree(src, Options{MaxDepth: 3})
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	subset, err := TreeFor(src, nil, Options{MaxDepth: 3})
	if err != nil {
		t.Fatalf("TreeFor: %v", err)
	}
	if TreeJSON(full) != TreeJSON(subset) {
		t.Errorf("TreeFor(nil) differs from Tree:\n%s\nvs\n%s", TreeJSON(subset), TreeJSON(full))
	}
}

func TestTreeForRecordsOnlyTheIncludedDirectories(t *testing.T) {
	src := treeSource(t, map[string]int{
		"alpha/a.bin": 10,
		"beta/b.bin":  10,
		"gamma/c.bin": 10,
	})

	got, err := TreeFor(src, []string{"alpha", "gamma"}, Options{MaxDepth: 3})
	if err != nil {
		t.Fatalf("TreeFor: %v", err)
	}
	names := childNames(got)
	if len(names) != 2 || !has(names, "alpha") || !has(names, "gamma") {
		t.Errorf("children = %v, want exactly alpha and gamma", names)
	}
	// The root keeps the source directory's name in every volume, because that is
	// what the archive's internal paths are prefixed with.
	if got.Name != filepath.Base(src) {
		t.Errorf("root = %q, want %q", got.Name, filepath.Base(src))
	}
}

// Including a directory includes everything beneath it, without having to
// enumerate the subtree.
func TestIncludingADirectoryIncludesItsSubtree(t *testing.T) {
	src := treeSource(t, map[string]int{
		"alpha/one/x.bin": 10,
		"alpha/two/y.bin": 10,
		"beta/b.bin":      10,
	})

	got, err := TreeFor(src, []string{"alpha"}, Options{MaxDepth: 3})
	if err != nil {
		t.Fatalf("TreeFor: %v", err)
	}
	if len(got.Children) != 1 || got.Children[0].Name != "alpha" {
		t.Fatalf("children = %v, want just alpha", childNames(got))
	}
	inner := childNames(got.Children[0])
	if len(inner) != 2 || !has(inner, "one") || !has(inner, "two") {
		t.Errorf("alpha's children = %v, want one and two", inner)
	}
}

// A deep include must keep its ancestors as pass-through nodes, or the selected
// directory would be unreachable in the tree.
func TestADeepIncludeKeepsItsAncestorsButNotTheirSiblings(t *testing.T) {
	src := treeSource(t, map[string]int{
		"big/one/x.bin": 10,
		"big/two/y.bin": 10,
		"other/z.bin":   10,
	})

	got, err := TreeFor(src, []string{filepath.Join("big", "one")}, Options{MaxDepth: 3})
	if err != nil {
		t.Fatalf("TreeFor: %v", err)
	}
	names := childNames(got)
	if len(names) != 1 || names[0] != "big" {
		t.Fatalf("children = %v, want just big as a pass-through", names)
	}
	inner := childNames(got.Children[0])
	if len(inner) != 1 || inner[0] != "one" {
		t.Errorf("big's children = %v, want only the included 'one'", inner)
	}
}

// A loose file is only ever included by being named exactly — it has no subtree
// to be pulled in by — and its bytes must land on the volume that holds it.
func TestLooseRootFilesCountOnlyInTheVolumeThatHoldsThem(t *testing.T) {
	src := treeSource(t, map[string]int{
		"loose.txt":   42,
		"other.txt":   7,
		"alpha/a.bin": 10,
	})

	withFile, err := TreeFor(src, []string{"loose.txt", "alpha"}, Options{MaxDepth: 3})
	if err != nil {
		t.Fatalf("TreeFor: %v", err)
	}
	if withFile.SizeBytes != 42 {
		t.Errorf("root size = %d, want only the included loose.txt (42)", withFile.SizeBytes)
	}

	withoutFile, err := TreeFor(src, []string{"alpha"}, Options{MaxDepth: 3})
	if err != nil {
		t.Fatalf("TreeFor: %v", err)
	}
	if withoutFile.SizeBytes != 0 {
		t.Errorf("root size = %d, want 0 when no loose file is included", withoutFile.SizeBytes)
	}
}

// Exclude terms and the depth cap must behave identically for a volume's tree
// and a whole backup's, or a split would quietly change what gets recorded.
func TestTreeForStillHonoursExcludesAndDepth(t *testing.T) {
	src := treeSource(t, map[string]int{
		"alpha/node_modules/x.bin": 10,
		"alpha/keep/y.bin":         10,
		"alpha/keep/deep/z.bin":    10,
	})

	got, err := TreeFor(src, []string{"alpha"}, Options{MaxDepth: 1, ExcludeTerms: []string{"node_modules"}})
	if err != nil {
		t.Fatalf("TreeFor: %v", err)
	}
	if len(got.Children) != 1 || got.Children[0].Name != "alpha" {
		t.Fatalf("children = %v", childNames(got))
	}
	// MaxDepth 1 records the root plus one level, so alpha's own children stop here.
	if len(got.Children[0].Children) != 0 {
		t.Errorf("alpha has children at MaxDepth 1: %v", childNames(got.Children[0]))
	}

	deeper, err := TreeFor(src, []string{"alpha"}, Options{MaxDepth: 3, ExcludeTerms: []string{"node_modules"}})
	if err != nil {
		t.Fatalf("TreeFor: %v", err)
	}
	inner := childNames(deeper.Children[0])
	if has(inner, "node_modules") {
		t.Errorf("alpha's children = %v, want node_modules excluded", inner)
	}
	if !has(inner, "keep") {
		t.Errorf("alpha's children = %v, want keep", inner)
	}
}

// An explicit "." include is "the whole directory", and must not be treated as a
// literal path that matches nothing.
func TestAWholeDirectoryIncludeDropsTheRestriction(t *testing.T) {
	src := treeSource(t, map[string]int{"alpha/a.bin": 10, "beta/b.bin": 10})

	got, err := TreeFor(src, []string{"."}, Options{MaxDepth: 3})
	if err != nil {
		t.Fatalf("TreeFor: %v", err)
	}
	if len(got.Children) != 2 {
		t.Errorf("children = %v, want the whole directory", childNames(got))
	}
}
