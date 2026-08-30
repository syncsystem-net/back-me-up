package scanner

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
	return TreeFor(rootPath, nil, opts)
}

// TreeFor is Tree restricted to part of the source: only the relative paths in
// include, and whatever lies beneath them, are recorded. A nil or empty include
// list means the whole directory, which is what Tree passes.
//
// This is what gives each volume of a split backup an honest tree. A volume
// holds some of the source's subdirectories, and its recorded tree must show
// exactly those — otherwise every volume of a split would claim to contain the
// entire backup, and the merged record tree and the global search would both be
// describing archives that do not hold what they say.
//
// The root node is still named after the source directory in every volume,
// because that is what the archive's internal paths are prefixed with.
func TreeFor(rootPath string, include []string, opts Options) (*Node, error) {
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

	sel := newSelector(include)
	root := &Node{
		Name:      filepath.Base(filepath.Clean(rootPath)),
		SizeBytes: dirImmediateSize(rootPath, ".", sel),
		Children:  walk(rootPath, ".", 1, maxDepth, opts.ExcludeTerms, sel),
	}
	return root, nil
}

// walk returns the directory children of dirPath. depth is the level the
// children sit at (root's children are depth 1); recursion stops once depth
// exceeds maxDepth, so deeper directories are simply not recorded. rel is
// dirPath's path relative to the tree root, which sel is consulted with.
//
// A directory that cannot be read is logged and recorded with no children
// rather than failing the walk. Sources routinely contain something unreadable
// — a permission-denied folder, a Windows junction or reparse point, a cloud
// placeholder — and recursing the full tree meets far more of them than the old
// two-level scan did. Aborting would block the whole backup over one entry that
// the zip itself still archives.
func walk(dirPath, rel string, depth, maxDepth int, excludes []string, sel *selector) []*Node {
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

		childRel := joinRel(rel, e.Name())
		childSel, keep := sel.descend(childRel)
		if !keep {
			continue
		}

		childPath := filepath.Join(dirPath, e.Name())
		nodes = append(nodes, &Node{
			Name:      e.Name(),
			SizeBytes: dirImmediateSize(childPath, childRel, childSel),
			Children:  walk(childPath, childRel, depth+1, maxDepth, excludes, childSel),
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
// subdirectories, and counting only files sel includes. Like walk, it degrades
// to 0 rather than failing: a size is display detail, and an unreadable entry
// must not cost the user their backup.
func dirImmediateSize(dirPath, rel string, sel *selector) int64 {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return 0
	}

	var total int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !sel.includesFile(joinRel(rel, e.Name())) {
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

// selector answers "is this path part of the archive being described". A nil
// selector means everything, which is the unsplit case and therefore the one
// every existing caller gets.
//
// It carries only the paths a volume was given; membership is decided by exact
// match or by ancestry, so naming a directory pulls in everything beneath it
// without having to enumerate it.
type selector struct {
	inc map[string]bool
}

// newSelector builds a selector over relative paths. An empty list yields nil —
// "no restriction" — rather than a selector that matches nothing, because an
// unsplit backup passes no includes and must record its whole tree.
func newSelector(include []string) *selector {
	if len(include) == 0 {
		return nil
	}
	inc := make(map[string]bool, len(include))
	for _, r := range include {
		k := normalizeRel(r)
		if k == "." || k == "" {
			// An explicit "the whole directory" entry makes every other entry
			// redundant; drop the restriction entirely.
			return nil
		}
		inc[k] = true
	}
	return &selector{inc: inc}
}

// descend reports whether a directory belongs in the tree, and returns the
// selector to apply beneath it. Once a directory is included outright, its whole
// subtree is included, so the returned selector is nil from there down.
func (s *selector) descend(rel string) (*selector, bool) {
	if s == nil {
		return nil, true
	}
	if s.inc[rel] {
		return nil, true
	}
	// Not selected itself, but an ancestor of something that is: keep it as a
	// pass-through so the selected descendant is still reachable in the tree.
	prefix := rel + "/"
	for k := range s.inc {
		if strings.HasPrefix(k, prefix) {
			return s, true
		}
	}
	return nil, false
}

// includesFile reports whether a file sitting directly in a directory is part of
// this archive. A loose file is only ever included by being named exactly: it
// has no subtree to be pulled in by.
func (s *selector) includesFile(rel string) bool {
	if s == nil {
		return true
	}
	return s.inc[rel]
}

// joinRel appends a name to a relative path, treating "." as the root.
func joinRel(rel, name string) string {
	if rel == "." || rel == "" {
		return name
	}
	return rel + "/" + name
}

// normalizeRel puts a caller-supplied relative path into the slash-separated
// form the selector compares with. Callers hand over OS-separator paths from the
// split planner, so this is where Windows backslashes are reconciled.
func normalizeRel(rel string) string {
	r := filepath.ToSlash(filepath.Clean(rel))
	return strings.TrimPrefix(r, "./")
}
