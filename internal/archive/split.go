package archive

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// packHeadroom is held back from a provider's threshold when packing. Packing
// decides on raw file sizes, but a zip is not purely the sum of its contents: a
// local header and a central-directory record ride along with every entry, and a
// source of many small files carries thousands of them. Deflate more than repays
// that on real data, so the margin is insurance rather than arithmetic — the
// written volume is measured against the true threshold afterwards regardless.
const packHeadroom = 8 << 20 // 8 MiB

// Item is one unit of packing: a path relative to the source root that is
// assigned to exactly one volume, together with the bytes it contributes.
//
// A directory is kept whole where it fits, so a volume stays something a person
// can reason about ("this one has Photos and Documents") rather than an
// arbitrary slice. Only a directory too large for one volume is broken open, and
// then its own children become items.
type Item struct {
	// Rel is the path relative to the source root, in OS separator form.
	Rel string
	// Bytes is the raw, uncompressed size: the file's own size, or the total of
	// every file beneath a directory.
	Bytes int64
	// IsDir distinguishes a whole subtree from a single file. A file is the one
	// thing that cannot be subdivided, which is why an oversized one is reported
	// rather than split.
	IsDir bool
}

// Volume is one standalone zip to be produced: a set of items and their raw
// total. Volumes are what the record accumulates as separate archives.
type Volume struct {
	Items []Item
	Bytes int64
}

// Plan is the full set of volumes a source directory will be zipped into for one
// threshold. A plan with a single volume means no split is needed, and the
// backup is produced exactly as it was before splitting existed.
type Plan struct {
	Volumes []Volume
	// TotalBytes is the raw size of everything to be archived, across volumes.
	TotalBytes int64
	// Mode is how the division was actually achieved. ModeByteParts means the
	// archives are NOT independently openable and must be rejoined; everything
	// downstream that treats a volume as a readable zip has to check this.
	Mode SplitMode
	// PartBytes is the size each byte part is cut to, when Mode is ModeByteParts.
	PartBytes int64
}

// ByteParts reports whether this plan produces raw parts rather than standalone
// archives.
func (p *Plan) ByteParts() bool { return p != nil && p.Mode == ModeByteParts }

// Split reports whether this plan produces more than one archive.
func (p *Plan) Split() bool { return p != nil && len(p.Volumes) > 1 }

// FileTooLargeError reports a single file that no threshold-respecting archive
// can contain. It is deliberately not a generic failure: the only useful thing
// we can tell the user is which file and how big, because the remedy is theirs
// (move it out, or raise the threshold if the provider really does accept it).
type FileTooLargeError struct {
	Rel       string
	Bytes     int64
	Threshold int64
}

func (e *FileTooLargeError) Error() string {
	return fmt.Sprintf("%s is %s, larger than the %s limit for this account, and a single file cannot be split",
		filepath.ToSlash(e.Rel), HumanBytes(e.Bytes), HumanBytes(e.Threshold))
}

// PlanVolumes decides how srcDir is divided for a provider whose per-file
// threshold is thresholdBytes, using the configured split method. A threshold of
// 0 (or one the whole source already fits under) yields a single volume.
//
// The walk it performs reads directory metadata only — no file is opened and
// nothing is compressed — so an impossible upload is refused in seconds instead
// of after zipping several gigabytes.
//
// Under MethodAuto a source containing a single file larger than the threshold
// falls back to byte parts. That case is the only one whole-file packing cannot
// express at all, and refusing it outright would mean a provider simply could
// never receive that backup.
func PlanVolumes(srcDir string, thresholdBytes int64, method SplitMethod) (*Plan, error) {
	srcDir = filepath.Clean(srcDir)

	if NormalizeMethod(string(method)) == MethodByteParts && thresholdBytes > 0 {
		return PlanBytes(srcDir, thresholdBytes)
	}

	total, _, err := treeSize(srcDir)
	if err != nil {
		return nil, err
	}

	// Nothing to decide: no threshold, or the whole thing already fits. The
	// caller zips the directory whole, exactly as it always has.
	if thresholdBytes <= 0 || total <= thresholdBytes {
		return &Plan{
			Volumes:    []Volume{{Items: []Item{{Rel: ".", Bytes: total, IsDir: true}}, Bytes: total}},
			TotalBytes: total,
			Mode:       ModeWholeFiles,
		}, nil
	}

	capacity := thresholdBytes - packHeadroom
	if capacity <= 0 {
		capacity = thresholdBytes
	}

	items, err := collectItems(srcDir, ".", capacity, thresholdBytes)
	if err != nil {
		var tooLarge *FileTooLargeError
		if errors.As(err, &tooLarge) && NormalizeMethod(string(method)) == MethodAuto {
			// Whole files cannot express this source. Cutting the finished archive
			// into parts can, at the cost of parts that must be rejoined — which is
			// strictly better than the backup being impossible.
			return PlanBytes(srcDir, thresholdBytes)
		}
		return nil, err
	}

	return &Plan{Volumes: Pack(items, capacity), TotalBytes: total, Mode: ModeWholeFiles}, nil
}

// collectItems turns the contents of dir into packable items, descending into
// any subdirectory too large to fit a volume whole. relBase is dir's path
// relative to the source root ("." at the top).
//
// Descending is what makes an arbitrarily deep source packable: a first-level
// subdirectory bigger than a whole volume is replaced by its own children, and
// the rule applies again to each of them. Files directly inside such a directory
// become items in their own right, so nothing is lost when a directory is opened
// up.
//
// Every failure here is fatal, deliberately, and this is the one place the
// planner must NOT copy scanner.walk's tolerance of unreadable entries. A
// recorded tree that omits a directory is a cosmetic loss; a *plan* that omits
// one produces volumes that are never asked to archive it, because ZipItems
// walks only the items it was given. Skipping quietly here would upload a backup
// that silently does not contain the user's data — where the unsplit path, which
// walks the whole directory, would have failed loudly instead.
func collectItems(dir, relBase string, capacity, threshold int64) ([]Item, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("cannot read %s while planning the archives: %w", dir, err)
	}

	var items []Item
	for _, e := range entries {
		rel := e.Name()
		if relBase != "." {
			rel = filepath.Join(relBase, e.Name())
		}
		full := filepath.Join(dir, e.Name())

		if !e.IsDir() {
			info, err := e.Info()
			if err != nil {
				return nil, fmt.Errorf("cannot read %s while planning the archives: %w", full, err)
			}
			size := info.Size()
			// A file is the floor: there is nothing smaller to fall back to. Only
			// the real threshold counts here, not the packing capacity, so a file
			// that genuinely fits is never reported as impossible.
			if size > threshold {
				return nil, &FileTooLargeError{Rel: rel, Bytes: size, Threshold: threshold}
			}
			// A zero-byte file still contributes an entry and a name, so it is an
			// item like any other; it simply weighs nothing when packing.
			items = append(items, Item{Rel: rel, Bytes: size})
			continue
		}

		size, files, err := treeSize(full)
		if err != nil {
			return nil, fmt.Errorf("cannot measure %s while planning the archives: %w", full, err)
		}
		if size <= capacity {
			// Emptiness is "holds no files", not "sums to zero bytes". A directory
			// of zero-byte files (.gitkeep, markers, empty logs) measures zero yet
			// still contributes entries, so dropping it on size alone would leave
			// those files in no volume at all — something the unsplit
			// whole-directory zip never does.
			if files == 0 {
				continue
			}
			items = append(items, Item{Rel: rel, Bytes: size, IsDir: true})
			continue
		}

		// Too big to keep whole: open it up and apply the same rule inside.
		children, err := collectItems(full, rel, capacity, threshold)
		if err != nil {
			return nil, err
		}
		items = append(items, children...)
	}
	return items, nil
}

// Pack distributes items into volumes of at most capacity bytes, first-fit
// decreasing. It is pure: given the same items and capacity it always returns
// the same volumes, which is what makes the plan testable without a filesystem
// and reproducible between the pre-flight preview and the upload that follows.
//
// An item larger than capacity gets a volume to itself. That only happens for a
// single file between the packing capacity and the real threshold — a directory
// that large was already broken open by collectItems.
func Pack(items []Item, capacity int64) []Volume {
	if len(items) == 0 {
		return nil
	}

	// Sort by size descending, then by path, so the result does not depend on
	// directory iteration order.
	ordered := append([]Item(nil), items...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Bytes != ordered[j].Bytes {
			return ordered[i].Bytes > ordered[j].Bytes
		}
		return ordered[i].Rel < ordered[j].Rel
	})

	var volumes []Volume
	for _, it := range ordered {
		placed := false
		for i := range volumes {
			if volumes[i].Bytes+it.Bytes <= capacity {
				volumes[i].Items = append(volumes[i].Items, it)
				volumes[i].Bytes += it.Bytes
				placed = true
				break
			}
		}
		if !placed {
			volumes = append(volumes, Volume{Items: []Item{it}, Bytes: it.Bytes})
		}
	}

	// Within a volume, order by path: the zip's entry order then matches what a
	// whole-directory walk would have produced.
	for i := range volumes {
		sort.Slice(volumes[i].Items, func(a, b int) bool {
			return volumes[i].Items[a].Rel < volumes[i].Items[b].Rel
		})
	}
	return volumes
}

// VolumeName is the cloud filename for one volume of a split backup, e.g.
// "bkup003-2of3.zip". The ".zip" stays last so the Auto-Sync crawl, which
// adopts remote files by that extension, still recognises a volume as an
// archive.
func VolumeName(srcDir string, index, count int) string {
	base := filepath.Base(filepath.Clean(srcDir))
	return fmt.Sprintf("%s-%dof%d.zip", base, index, count)
}

// Names returns the cloud filenames this plan will produce, in volume order. A
// single-volume plan uses the plain RemoteName, so a backup that does not need
// splitting is named exactly as it was before splitting existed.
func (p *Plan) Names(srcDir string) []string {
	if p == nil || len(p.Volumes) == 0 {
		return nil
	}
	if p.ByteParts() {
		// Parts are numbered from 1 even when there is only one of them: the
		// suffix is what tells the user (and 7-Zip) that rejoining is required.
		names := make([]string, 0, len(p.Volumes))
		for i := range p.Volumes {
			names = append(names, PartName(srcDir, i+1))
		}
		return names
	}
	if len(p.Volumes) == 1 {
		return []string{RemoteName(srcDir)}
	}
	names := make([]string, 0, len(p.Volumes))
	for i := range p.Volumes {
		names = append(names, VolumeName(srcDir, i+1, len(p.Volumes)))
	}
	return names
}

// Includes returns the relative paths one volume archives, for the tree recorded
// against it. A single-volume plan returns nil, meaning "the whole directory".
func (p *Plan) Includes(volume int) []string {
	if p == nil || volume < 0 || volume >= len(p.Volumes) {
		return nil
	}
	// Byte parts are slices of one archive of the whole directory, so the tree
	// that describes them is the whole tree. It is recorded against the first
	// part only; see buildArchives.
	if p.ByteParts() || len(p.Volumes) == 1 {
		return nil
	}
	out := make([]string, 0, len(p.Volumes[volume].Items))
	for _, it := range p.Volumes[volume].Items {
		out = append(out, it.Rel)
	}
	return out
}

// treeSize totals every file beneath root and counts them, ignoring directories
// themselves — matching what Zip actually writes, since the zip writer skips
// directory entries.
//
// The file count is returned alongside the byte total because the two answer
// different questions: bytes decide which volume a directory fits in, and the
// count decides whether it holds anything at all. A tree of zero-byte files has
// a size of 0 but is emphatically not empty.
//
// Like collectItems, it fails rather than skipping an unreadable entry: a size
// that quietly under-counts would put a directory in a volume it does not fit,
// and a subtree silently missed here is a subtree missing from the backup.
func treeSize(root string) (int64, int, error) {
	info, err := os.Stat(root)
	if err != nil {
		return 0, 0, err
	}
	if !info.IsDir() {
		return info.Size(), 1, nil
	}

	var total int64
	var files int
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("cannot read %s: %w", path, err)
		}
		if d.IsDir() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return fmt.Errorf("cannot read %s: %w", path, err)
		}
		total += fi.Size()
		files++
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	return total, files, nil
}

// HumanBytes renders a byte count for a message a user reads. It uses decimal
// units because that is how both providers state their limits.
func HumanBytes(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(n)/float64(div), "kMGTP"[exp])
}

// relKey normalises a relative path for comparison: slashes, no leading "./".
func relKey(rel string) string {
	return strings.TrimPrefix(filepath.ToSlash(filepath.Clean(rel)), "./")
}

// SplitMethod is how a backup too large for a provider is divided.
type SplitMethod string

const (
	// MethodAuto keeps files whole, and falls back to byte parts only for the one
	// case whole files cannot express: a single file larger than the limit. It is
	// the default because every other backup keeps the properties that depend on
	// a volume being a real archive.
	MethodAuto SplitMethod = "auto"
	// MethodWholeFiles never byte-splits. A single oversized file is reported.
	MethodWholeFiles SplitMethod = "whole_files"
	// MethodByteParts always cuts the finished archive into fixed-size parts.
	MethodByteParts SplitMethod = "byte_parts"
)

// NormalizeMethod maps configuration input onto a known method, defaulting to
// auto. An unrecognised value must not silently become "never split" or "always
// byte-split" — both are surprising in opposite directions.
func NormalizeMethod(s string) SplitMethod {
	switch SplitMethod(strings.ToLower(strings.TrimSpace(s))) {
	case MethodWholeFiles:
		return MethodWholeFiles
	case MethodByteParts:
		return MethodByteParts
	default:
		return MethodAuto
	}
}

// SplitMode is what a finished plan actually decided to do.
type SplitMode string

const (
	// ModeWholeFiles: each volume is a standalone zip holding whole files.
	ModeWholeFiles SplitMode = "whole_files"
	// ModeByteParts: one archive, cut into fixed-size parts. A part is NOT an
	// archive on its own — every part must be rejoined before extracting.
	ModeByteParts SplitMode = "byte_parts"
)

// PartSuffix is the numbered extension a byte part carries, e.g. ".001". The
// convention is 7-Zip's: opening the .001 part reassembles the rest.
func PartSuffix(index int) string { return fmt.Sprintf(".%03d", index) }

// PartName is the cloud filename of one byte part of a backup.
func PartName(srcDir string, index int) string {
	return RemoteName(srcDir) + PartSuffix(index)
}

// IsPartName reports whether a remote filename looks like one of our byte parts.
func IsPartName(name string) bool {
	i := strings.LastIndex(name, ".")
	if i < 0 || len(name)-i != 4 {
		return false
	}
	for _, r := range name[i+1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return strings.EqualFold(filepath.Ext(name[:i]), ".zip")
}

// PlanBytes describes a backup that will be zipped whole and then cut into parts
// of at most partBytes. The part count is an upper bound: it is derived from the
// uncompressed total, and compression only reduces it, so the real count is
// settled after the archive is written.
func PlanBytes(srcDir string, partBytes int64) (*Plan, error) {
	total, _, err := treeSize(srcDir)
	if err != nil {
		return nil, err
	}
	parts := 1
	if partBytes > 0 && total > partBytes {
		parts = int((total + partBytes - 1) / partBytes)
	}

	volumes := make([]Volume, 0, parts)
	remaining := total
	for i := 0; i < parts; i++ {
		size := remaining
		if partBytes > 0 && size > partBytes {
			size = partBytes
		}
		volumes = append(volumes, Volume{Bytes: size})
		remaining -= size
	}
	return &Plan{Volumes: volumes, TotalBytes: total, Mode: ModeByteParts, PartBytes: partBytes}, nil
}

// SplitFile cuts path into consecutive parts of at most partBytes, named with
// PartSuffix, and removes the original. It returns the part paths in order.
//
// The parts are a raw byte cut, so none of them is an openable archive: they are
// rejoined (7-Zip opens the .001, or `copy /b` and `cat` concatenate them) and
// then extracted. That is the cost of the one case whole-file packing cannot
// express, and it is why this is a fallback rather than the default.
func SplitFile(path string, partBytes int64) ([]string, error) {
	if partBytes <= 0 {
		return []string{path}, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat archive: %w", err)
	}
	if info.Size() <= partBytes {
		return []string{path}, nil
	}

	src, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open archive: %w", err)
	}
	defer src.Close()

	var parts []string
	// A failure part-way through must not leave a half-cut set beside the
	// original, so everything written so far is removed before returning.
	abandon := func(err error) ([]string, error) {
		for _, p := range parts {
			os.Remove(p)
		}
		return nil, err
	}

	for i := 1; ; i++ {
		partPath := path + PartSuffix(i)
		dst, err := os.Create(partPath)
		if err != nil {
			return abandon(fmt.Errorf("creating part %s: %w", partPath, err))
		}
		written, err := io.CopyN(dst, src, partBytes)
		closeErr := dst.Close()
		if err != nil && err != io.EOF {
			os.Remove(partPath)
			return abandon(fmt.Errorf("writing part %s: %w", partPath, err))
		}
		if closeErr != nil {
			os.Remove(partPath)
			return abandon(fmt.Errorf("closing part %s: %w", partPath, closeErr))
		}
		if written == 0 {
			// The previous part ended exactly on the boundary; this one is empty.
			os.Remove(partPath)
			break
		}
		parts = append(parts, partPath)
		if err == io.EOF || written < partBytes {
			break
		}
	}

	src.Close()
	if err := os.Remove(path); err != nil {
		// The parts are the deliverable; a leftover original is wasted disk, not a
		// broken backup, so report it without discarding the work.
		return parts, nil
	}
	return parts, nil
}
