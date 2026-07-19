package scanner

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// buildFixture creates a directory tree under t.TempDir from a list of
// slash-separated relative directory paths, plus a file in each so sizes are
// non-zero.
func buildFixture(t *testing.T, dirs []string) string {
	t.Helper()
	root := t.TempDir()
	for _, d := range dirs {
		full := filepath.Join(root, filepath.FromSlash(d))
		if err := os.MkdirAll(full, 0o755); err != nil {
			t.Fatalf("creating %s: %v", d, err)
		}
		if err := os.WriteFile(filepath.Join(full, "f.txt"), []byte("xy"), 0o644); err != nil {
			t.Fatalf("writing file in %s: %v", d, err)
		}
	}
	return root
}

// paths flattens a tree into slash-joined paths (excluding the root's own name)
// so tests can assert on structure with a simple sorted string comparison.
func paths(n *Node, prefix string) []string {
	var out []string
	for _, c := range n.Children {
		p := c.Name
		if prefix != "" {
			p = prefix + "/" + c.Name
		}
		out = append(out, p)
		out = append(out, paths(c, p)...)
	}
	return out
}

func TestTreeDepthCap(t *testing.T) {
	root := buildFixture(t, []string{
		"a/b/c/d/e",
		"x/y",
	})

	tests := []struct {
		maxDepth int
		want     []string
	}{
		// Depth counts levels below the root: 1 records only the root's own
		// children.
		{1, []string{"a", "x"}},
		{2, []string{"a", "a/b", "x", "x/y"}},
		{3, []string{"a", "a/b", "a/b/c", "x", "x/y"}},
		{5, []string{"a", "a/b", "a/b/c", "a/b/c/d", "a/b/c/d/e", "x", "x/y"}},
		// A non-positive cap must fall back to the default rather than walking
		// unbounded or returning nothing.
		{0, []string{"a", "a/b", "a/b/c", "x", "x/y"}},
	}

	for _, tt := range tests {
		got, err := Tree(root, Options{MaxDepth: tt.maxDepth})
		if err != nil {
			t.Fatalf("maxDepth %d: %v", tt.maxDepth, err)
		}
		gotPaths := paths(got, "")
		sort.Strings(gotPaths)
		sort.Strings(tt.want)
		if strings.Join(gotPaths, ",") != strings.Join(tt.want, ",") {
			t.Errorf("maxDepth %d: got %v, want %v", tt.maxDepth, gotPaths, tt.want)
		}
	}
}

func TestTreeExcludesMatchingDirectories(t *testing.T) {
	root := buildFixture(t, []string{
		"Conteúdo Antigo/inner",
		"docs/Layout",
		"docs/keep",
		"conteudo",
	})

	got, err := Tree(root, Options{MaxDepth: 5, ExcludeTerms: []string{"conteudo", "layout"}})
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}

	gotPaths := paths(got, "")
	sort.Strings(gotPaths)
	want := []string{"docs", "docs/keep"}
	if strings.Join(gotPaths, ",") != strings.Join(want, ",") {
		t.Errorf("got %v, want %v", gotPaths, want)
	}
}

// An excluded directory takes its whole subtree with it, so a kept-looking
// child under an excluded parent must not survive.
func TestTreeExcludeRemovesSubtree(t *testing.T) {
	root := buildFixture(t, []string{"Layout/keep/deeper"})

	got, err := Tree(root, Options{MaxDepth: 5, ExcludeTerms: []string{"Layout"}})
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	if len(got.Children) != 0 {
		t.Errorf("expected no children, got %v", paths(got, ""))
	}
}

// The root is the directory the user explicitly chose, so it is recorded even
// when its own name matches an exclude term.
func TestTreeRootIsNeverExcluded(t *testing.T) {
	base := buildFixture(t, []string{"Layout/child"})
	root := filepath.Join(base, "Layout")

	got, err := Tree(root, Options{MaxDepth: 3, ExcludeTerms: []string{"layout"}})
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	if got.Name != "Layout" {
		t.Errorf("root name = %q, want %q", got.Name, "Layout")
	}
	if len(got.Children) != 1 || got.Children[0].Name != "child" {
		t.Errorf("expected child to be recorded, got %v", paths(got, ""))
	}
}

func TestTreeSizeCountsOnlyImmediateFiles(t *testing.T) {
	// Both levels get their own 2-byte file, so a roll-up would show 4 for "a".
	root := buildFixture(t, []string{"a", "a/b"})

	got, err := Tree(root, Options{MaxDepth: 3})
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	if len(got.Children) != 1 {
		t.Fatalf("expected 1 child, got %d", len(got.Children))
	}
	a := got.Children[0]
	if a.SizeBytes != 2 {
		t.Errorf("a.SizeBytes = %d, want 2 (its own file only, not its subtree)", a.SizeBytes)
	}
	if len(a.Children) != 1 || a.Children[0].SizeBytes != 2 {
		t.Errorf("expected b recorded with size 2, got %d children", len(a.Children))
	}
}

// A directory that disappears or cannot be read mid-walk must not fail the
// whole backup — the zip still archives it, so the tree just records it without
// children. Simulated by removing read+execute permission; skipped where the OS
// does not enforce that for the owner (Windows, or running as root).
func TestTreeToleratesUnreadableDirectory(t *testing.T) {
	root := buildFixture(t, []string{"readable", "locked/inner"})
	locked := filepath.Join(root, "locked")

	if err := os.Chmod(locked, 0o000); err != nil {
		t.Skipf("cannot chmod on this platform: %v", err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })
	if _, err := os.ReadDir(locked); err == nil {
		t.Skip("permissions not enforced for the owner on this platform")
	}

	got, err := Tree(root, Options{MaxDepth: 5})
	if err != nil {
		t.Fatalf("an unreadable subdirectory must not fail the walk: %v", err)
	}
	// Both directories are still recorded; the unreadable one simply has no
	// children.
	names := map[string]int{}
	for _, c := range got.Children {
		names[c.Name] = len(c.Children)
	}
	if _, ok := names["readable"]; !ok {
		t.Errorf("expected the readable sibling to survive, got %v", names)
	}
	if n, ok := names["locked"]; !ok || n != 0 {
		t.Errorf("expected locked recorded with no children, got %v", names)
	}
}

// An unreadable ROOT is different: the user pointed at it explicitly, so a
// silent empty tree would be misleading.
func TestTreeFailsOnUnreadableRoot(t *testing.T) {
	if _, err := Tree(filepath.Join(t.TempDir(), "does-not-exist"), Options{MaxDepth: 3}); err == nil {
		t.Error("expected an error for a missing root")
	}
}

func TestFold(t *testing.T) {
	tests := []struct{ in, want string }{
		{"Conteúdo", "conteudo"},
		{"CONTEÚDO", "conteudo"},
		{"conteudo", "conteudo"},
		{"São Paulo", "sao paulo"},
		{"Ação", "acao"},
		{"plain", "plain"},
		// Decomposed (NFD) input: base letter + combining acute. macOS hands
		// filenames back in this form, so folding only the precomposed rune
		// would silently fail to match there. The escape is deliberate — a
		// literal "ú" in Go source is stored precomposed and would not exercise
		// this path.
		{"Conteu\u0301do", "conteudo"},
		{"Ac\u0327a\u0303o", "acao"},
	}
	for _, tt := range tests {
		if got := Fold(tt.in); got != tt.want {
			t.Errorf("Fold(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// The precomposed and decomposed spellings of the same name must fold to the
// same string, so a term typed on one platform matches a directory created on
// the other.
func TestFoldUnifiesComposedAndDecomposed(t *testing.T) {
	if composed, decomposed := Fold("Conteúdo"), Fold("Conteu\u0301do"); composed != decomposed {
		t.Errorf("NFC %q != NFD %q", composed, decomposed)
	}
	if !Matches("Conteu\u0301do Antigo", "conteudo") {
		t.Error("an NFD directory name must match an unaccented term")
	}
	if !Matches("Conteudo Antigo", "conteu\u0301do") {
		t.Error("an NFD search term must match a plain directory name")
	}
}

func TestMatches(t *testing.T) {
	tests := []struct {
		name, term string
		want       bool
	}{
		{"Conteúdo Antigo", "conteudo", true},
		{"conteudo", "CONTEÚDO", true},
		{"My Layout Files", "layout", true},
		{"Layout", "  layout  ", true},
		{"documents", "conteudo", false},
		// A blank term must never match, or one stray entry in the exclude list
		// would silently drop every directory.
		{"anything", "", false},
		{"anything", "   ", false},
	}
	for _, tt := range tests {
		if got := Matches(tt.name, tt.term); got != tt.want {
			t.Errorf("Matches(%q, %q) = %v, want %v", tt.name, tt.term, got, tt.want)
		}
	}
}
