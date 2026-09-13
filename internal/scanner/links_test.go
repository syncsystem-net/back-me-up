package scanner

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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

// The recorded tree describes what the archive holds. The archive skips links,
// so a tree listing one would promise contents that are not there — and the
// record's tree and the global search are how a user decides whether a backup
// contains what they need.
func TestTreeLeavesLinksOut(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "real", "inner"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "real", "a.bin"), make([]byte, 64), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	linkDir(t, filepath.Join(root, "linked"), target)

	tree, err := Tree(root, Options{MaxDepth: 3})
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}

	var names []string
	var walkNode func(*Node)
	walkNode = func(n *Node) {
		names = append(names, n.Name)
		for _, c := range n.Children {
			walkNode(c)
		}
	}
	walkNode(tree)

	for _, n := range names {
		if n == "linked" {
			t.Fatalf("tree lists the link: %v", names)
		}
	}
	var sawReal bool
	for _, n := range names {
		if n == "real" {
			sawReal = true
		}
	}
	if !sawReal {
		t.Errorf("tree lost a real directory: %v", names)
	}
}
