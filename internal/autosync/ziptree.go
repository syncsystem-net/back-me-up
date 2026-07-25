package autosync

import (
	"archive/zip"
	"path"
	"sort"
	"strings"

	"github.com/syncsystem-net/back-me-up/internal/scanner"
)

// ZipEntry is one member of an archive, reduced to what the tree needs. Dir
// marks an explicit directory entry (a name ending in "/"), whose size never
// counts toward anything.
type ZipEntry struct {
	Name string
	Size int64
	Dir  bool
}

// EntriesFromReader converts a parsed archive into ZipEntry values.
func EntriesFromReader(zr *zip.Reader) []ZipEntry {
	entries := make([]ZipEntry, 0, len(zr.File))
	for _, f := range zr.File {
		entries = append(entries, ZipEntry{
			Name: f.Name,
			Size: int64(f.UncompressedSize64),
			Dir:  strings.HasSuffix(f.Name, "/") || f.FileInfo().IsDir(),
		})
	}
	return entries
}

// TreeFromEntries builds the same shape scanner.Tree produces, but from an
// archive's central directory instead of a local walk, so a discovered archive
// renders and searches exactly like an uploaded one.
//
// The tree is derived from the entries' path components, not from directory
// flags: a ZIP may reference "a/b/c.txt" without ever carrying explicit entries
// for "a" or "a/b", so any directory that only exists implicitly would otherwise
// be lost.
//
// opts is honoured exactly as it is for a local scan: MaxDepth counts levels
// below the root, a directory whose name matches an exclude term is dropped
// along with its whole subtree, and the root is never excluded. Sizes follow the
// scanner's rule too — a node's size counts only the files sitting directly in
// it, so a parent never double-counts its children.
func TreeFromEntries(zipName string, entries []ZipEntry, opts scanner.Options) *scanner.Node {
	rootName := archiveBaseName(zipName)

	forest := &dirNode{children: map[string]*dirNode{}}
	for _, e := range entries {
		comps := splitEntryPath(e.Name)
		if len(comps) == 0 {
			continue
		}
		if e.Dir {
			forest.ensure(comps)
			continue
		}
		// The last component is the file itself; its bytes belong to the
		// directory holding it (the synthetic root when it sits at top level).
		forest.ensure(comps[:len(comps)-1]).size += e.Size
	}

	root := forest
	// An archive produced by this tool wraps everything in a single directory
	// named after the source folder, which is also the zip's own name. Using it
	// as the root avoids a pointless "bkup003 > bkup003" nesting; anything else
	// (several top-level entries, or a differently named wrapper) hangs under a
	// synthetic root named after the archive.
	if len(forest.children) == 1 && forest.size == 0 {
		for name, only := range forest.children {
			if name == rootName {
				root = only
			}
		}
	}

	maxDepth := opts.MaxDepth
	if maxDepth <= 0 {
		maxDepth = defaultMaxDepth
	}
	return &scanner.Node{
		Name:      rootName,
		SizeBytes: root.size,
		Children:  convertChildren(root, 1, maxDepth, opts.ExcludeTerms),
	}
}

// defaultMaxDepth mirrors the scanner's fallback so a missing config value
// records the same depth either way.
const defaultMaxDepth = 3

// dirNode is the mutable directory under construction. size accumulates the
// files directly inside it.
type dirNode struct {
	size     int64
	children map[string]*dirNode
}

// ensure walks (creating as needed) the directory chain named by comps and
// returns the deepest node. An empty comps returns the receiver, which is how
// top-level files are attributed to the root.
func (d *dirNode) ensure(comps []string) *dirNode {
	cur := d
	for _, c := range comps {
		next, ok := cur.children[c]
		if !ok {
			next = &dirNode{children: map[string]*dirNode{}}
			cur.children[c] = next
		}
		cur = next
	}
	return cur
}

// convertChildren renders a directory's children as scanner nodes. depth is the
// level the children sit at (the root's children are depth 1), so recursion
// stops once depth exceeds maxDepth — a directory at exactly maxDepth is still
// recorded, with its size but no children, matching the local scanner.
func convertChildren(d *dirNode, depth, maxDepth int, excludes []string) []*scanner.Node {
	if depth > maxDepth || len(d.children) == 0 {
		return nil
	}
	names := make([]string, 0, len(d.children))
	for name := range d.children {
		if scanner.Excluded(name, excludes) {
			continue
		}
		names = append(names, name)
	}
	// Map iteration is random; sorting keeps a re-crawl of an unchanged archive
	// producing byte-identical tree_json.
	sort.Strings(names)

	nodes := make([]*scanner.Node, 0, len(names))
	for _, name := range names {
		child := d.children[name]
		nodes = append(nodes, &scanner.Node{
			Name:      name,
			SizeBytes: child.size,
			Children:  convertChildren(child, depth+1, maxDepth, excludes),
		})
	}
	if len(nodes) == 0 {
		return nil
	}
	return nodes
}

// splitEntryPath normalises a ZIP entry name into its path components. Entry
// names are always "/"-separated regardless of the writing platform, but they
// are attacker-adjacent data: absolute paths and "..", which could otherwise
// escape the root, are dropped.
func splitEntryPath(name string) []string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = strings.Trim(name, "/")
	if name == "" {
		return nil
	}
	cleaned := path.Clean(name)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return nil
	}
	var comps []string
	for _, c := range strings.Split(cleaned, "/") {
		if c == "" || c == "." {
			continue
		}
		comps = append(comps, c)
	}
	return comps
}

// archiveBaseName is the archive's name without its ".zip" suffix, which is what
// the wrapping directory inside a BackMeUp archive is called.
func archiveBaseName(zipName string) string {
	if len(zipName) >= 4 && strings.EqualFold(zipName[len(zipName)-4:], ".zip") {
		return zipName[:len(zipName)-4]
	}
	return zipName
}
