package archive

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// IsLink reports whether info describes a link rather than a real file or
// directory: a POSIX symlink, a Windows directory symlink, or a Windows
// junction (an NTFS mount point).
//
// Every walk in this package consults it, and they must all agree — the plan and
// the zip describing different contents is the one failure the splitting work
// was built to prevent. Following a link is never an option here: it would
// duplicate whatever it points at, or loop.
//
// It deliberately asks Go's mode bits and NOT the raw
// FILE_ATTRIBUTE_REPARSE_POINT attribute, even though that attribute looks like
// the authoritative Windows answer. It is too broad for a backup tool: OneDrive,
// Dropbox and Drive "files on demand" placeholders are reparse points too, and
// they are real files holding the user's data. Excluding them would quietly
// produce an archive missing every cloud-only file in the source — the exact
// silent data loss this work exists to prevent, arrived at from the other
// direction.
//
// Go already draws that line correctly: os.Lstat reports only *name surrogate*
// reparse points (mount points and symlinks) as ModeSymlink or ModeIrregular,
// and reports a cloud placeholder as an ordinary file or directory. Both mode
// bits are tested because which one a junction gets has changed across Go
// releases.
func IsLink(info fs.FileInfo) bool {
	if info == nil {
		return false
	}
	return info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0
}

// isLinkPath answers the same question for a path, using Lstat so the link
// itself is inspected rather than whatever it points at. A path that cannot be
// stat'd is not reported as a link — it is left for the caller's own error
// handling, which is deliberately strict while this is deliberately narrow.
func isLinkPath(path string) bool {
	info, err := os.Lstat(path)
	if err != nil {
		return false
	}
	return IsLink(info)
}

// linkSet accumulates the links a walk skipped. It is keyed by path relative to
// the source root so that the same link, met by both the planning walk and the
// size walk, is reported once rather than twice.
//
// A nil *linkSet is a valid no-op, so a caller that does not care about
// reporting need not construct one.
type linkSet struct {
	seen map[string]bool
}

func (s *linkSet) add(rel string) {
	if s == nil || rel == "" {
		return
	}
	if s.seen == nil {
		s.seen = map[string]bool{}
	}
	s.seen[relKey(rel)] = true
}

// joinRel joins a relative base with a path beneath it, treating "." as "the
// root itself" in either position so a link directly under the source is named
// "link" rather than ".\link".
func joinRel(base, rel string) string {
	if base == "" || base == "." {
		return rel
	}
	if rel == "" || rel == "." {
		return base
	}
	return filepath.Join(base, rel)
}

// relTo is filepath.Rel with a usable answer instead of an error: a name the
// user can recognise is enough here, and a reporting path must not be able to
// fail a backup.
func relTo(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return filepath.Base(path)
	}
	return rel
}

// sorted returns the skipped links in a stable order, so a plan built twice from
// an unchanged source reads identically — the same determinism the tree
// serialization needs for Auto-Sync to be idempotent.
func (s *linkSet) sorted() []string {
	if s == nil || len(s.seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(s.seen))
	for rel := range s.seen {
		out = append(out, rel)
	}
	sort.Strings(out)
	return out
}
