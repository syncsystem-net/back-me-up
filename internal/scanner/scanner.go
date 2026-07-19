package scanner

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
)

// Node is one directory in a recorded tree. SizeBytes counts only the files
// directly inside the directory, not its subdirectories, so a parent's size does
// not double-count its children.
type Node struct {
	Name      string  `json:"name"`
	SizeBytes int64   `json:"size_bytes,omitempty"`
	Children  []*Node `json:"children,omitempty"`
}

// Options bounds the walk. MaxDepth counts directory levels below the root, so
// MaxDepth 3 records the root plus three levels beneath it; a value <= 0 falls
// back to the config default of 3 rather than walking unbounded. ExcludeTerms
// are matched case- and accent-insensitively against each directory name (see
// Matches); a matching directory and its whole subtree are left out of the tree.
//
// Exclusion affects the recorded tree only — the uploaded zip still contains
// every directory, so the archive stays a complete copy of the source.
type Options struct {
	MaxDepth     int
	ExcludeTerms []string
}

const defaultMaxDepth = 3

// Tree walks rootPath recursively and returns it as a Node tree, honouring the
// depth cap and exclude terms in opts. The root node is named after the source
// directory and is never excluded, even if its own name matches a term — the
// user explicitly chose it.
func Tree(rootPath string, opts Options) (*Node, error) {
	maxDepth := opts.MaxDepth
	if maxDepth <= 0 {
		maxDepth = defaultMaxDepth
	}

	// A failure to read the root itself is fatal — the user pointed at a
	// directory we cannot open, and they should hear about it rather than get an
	// empty tree. Failures *inside* the tree are tolerated; see walk.
	if _, err := os.ReadDir(rootPath); err != nil {
		return nil, err
	}

	root := &Node{
		Name:      filepath.Base(filepath.Clean(rootPath)),
		SizeBytes: dirImmediateSize(rootPath),
		Children:  walk(rootPath, 1, maxDepth, opts.ExcludeTerms),
	}
	return root, nil
}

// walk returns the directory children of dirPath. depth is the level the
// children sit at (root's children are depth 1); recursion stops once depth
// exceeds maxDepth, so deeper directories are simply not recorded.
//
// A directory that cannot be read is logged and recorded with no children
// rather than failing the walk. Sources routinely contain something unreadable
// — a permission-denied folder, a Windows junction or reparse point, a cloud
// placeholder — and recursing the full tree meets far more of them than the old
// two-level scan did. Aborting would block the whole backup over one entry that
// the zip itself still archives.
func walk(dirPath string, depth, maxDepth int, excludes []string) []*Node {
	if depth > maxDepth {
		return nil
	}

	entries, err := os.ReadDir(dirPath)
	if err != nil {
		slog.Warn("skipping unreadable directory while recording tree", "path", dirPath, "error", err)
		return nil
	}

	var nodes []*Node
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if Excluded(e.Name(), excludes) {
			continue
		}

		childPath := filepath.Join(dirPath, e.Name())
		nodes = append(nodes, &Node{
			Name:      e.Name(),
			SizeBytes: dirImmediateSize(childPath),
			Children:  walk(childPath, depth+1, maxDepth, excludes),
		})
	}
	return nodes
}

// TreeJSON is the serialized form stored in backup_zips.tree_json. A marshal
// failure degrades to an empty object so a backup is never blocked by an
// unserializable tree.
func TreeJSON(root *Node) string {
	b, err := json.Marshal(root)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// dirImmediateSize totals the files directly inside dirPath, ignoring
// subdirectories. Like walk, it degrades to 0 rather than failing: a size is
// display detail, and an unreadable entry must not cost the user their backup.
func dirImmediateSize(dirPath string) int64 {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return 0
	}

	var total int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		total += info.Size()
	}
	return total
}
