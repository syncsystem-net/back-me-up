package autosync

import (
	"strings"

	"github.com/syncsystem-net/back-me-up/internal/provider"
)

// The two ways a remote archive can become visible locally.
const (
	// ActionAdopt records a remote archive the database has never heard of: a
	// new backup_zips row (with the tree read out of the archive) plus the
	// synthetic job that makes it downloadable.
	ActionAdopt = "adopt"

	// ActionLink is the cheaper half of the same idea. The zip row already
	// exists — the archive was uploaded to a sibling account — but this account
	// has no job pointing at its copy, so Download/Delete are unavailable here.
	// Only the job is created; the recorded tree is left exactly as it is.
	ActionLink = "link"
)

// ProposedZip is one change the apply step would make to the database.
type ProposedZip struct {
	RemoteID  string `json:"remote_id"`
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
	Action    string `json:"action"`
	// ZipID is the existing row a "link" attaches to; 0 for "adopt".
	ZipID int64 `json:"zip_id"`
	// Note carries anything the user should know about how this entry was
	// handled, e.g. that its tree could not be read. Filled in during apply.
	Note string `json:"note"`
}

// MatchedZip is a remote archive already fully represented locally. It is listed
// so the preview shows the whole picture rather than only the deltas, and it is
// what makes a second run visibly a no-op.
type MatchedZip struct {
	Name  string `json:"name"`
	ZipID int64  `json:"zip_id"`
}

// MissingZip is a local row whose remote copy was not in the listing. It is
// reported and never acted on: a transient API hiccup must not be able to delete
// a user's records.
type MissingZip struct {
	Name  string `json:"name"`
	ZipID int64  `json:"zip_id"`
	JobID int64  `json:"job_id"`
}

// LocalZip is one recorded archive as it relates to a single account: the zip
// row, plus the job tying it to this account if there is one.
type LocalZip struct {
	ZipID int64
	Name  string
	// JobID is this account's job for the zip, or 0 when the account has no
	// record of holding it.
	JobID int64
	// RemotePath is that job's provider handle. A job without one never
	// completed an upload, so its absence remotely is not news.
	RemotePath string
}

// Plan reconciles one account's remote listing against what the database knows,
// without touching either. Keeping it pure is what makes the adopt/link/matched/
// missing rules testable and makes idempotency something you can assert rather
// than hope for: feeding it the state left by a previous apply must yield no
// proposals.
//
// Matching is by remote file name against backup_zips.name within the owning
// user's record, and only ".zip" files are considered — the crawl adopts
// archives, not whatever else the user keeps in their cloud root.
func Plan(remote []provider.RemoteFile, local []LocalZip) (proposed []ProposedZip, matched []MatchedZip, missing []MissingZip) {
	byName := make(map[string]LocalZip, len(local))
	for _, l := range local {
		// A record accumulates zips over time, so several rows can legitimately
		// share a name (the same directory backed up twice). Blind last-wins
		// would pick whichever the query happened to return last — with
		// ListZipsByBackup's created_at DESC ordering, the OLDEST row — and could
		// then attach this account's job to a row whose tree describes different
		// contents. Prefer the row that already accounts for this account's copy.
		if existing, ok := byName[l.Name]; ok && rank(existing) >= rank(l) {
			continue
		}
		byName[l.Name] = l
	}
	remoteNames := make(map[string]bool, len(remote))

	for _, r := range remote {
		if !isZipName(r.Name) {
			continue
		}
		remoteNames[r.Name] = true

		l, known := byName[r.Name]
		switch {
		case !known:
			proposed = append(proposed, ProposedZip{
				RemoteID:  r.ID,
				Name:      r.Name,
				SizeBytes: r.Size,
				Action:    ActionAdopt,
			})
		// A job with no remote handle never completed an upload, so it cannot
		// download or delete anything. The file IS on the account, so calling
		// that "in sync" would leave the user with a row they can't act on —
		// precisely the situation this feature exists to repair. It needs a job
		// that actually points at the remote copy, same as having none at all.
		case l.JobID == 0 || l.RemotePath == "":
			proposed = append(proposed, ProposedZip{
				RemoteID:  r.ID,
				Name:      r.Name,
				SizeBytes: r.Size,
				Action:    ActionLink,
				ZipID:     l.ZipID,
			})
		default:
			matched = append(matched, MatchedZip{Name: l.Name, ZipID: l.ZipID})
		}
	}

	for _, l := range local {
		// Only a row this account actually claims to hold can go missing from
		// this account's listing.
		if l.JobID == 0 || l.RemotePath == "" {
			continue
		}
		if !remoteNames[l.Name] {
			missing = append(missing, MissingZip{Name: l.Name, ZipID: l.ZipID, JobID: l.JobID})
		}
	}
	return proposed, matched, missing
}

// rank scores how completely a local row accounts for this account's copy of an
// archive, so the best of several same-named rows wins: a row with a usable
// remote handle beats one whose job never finished uploading, which beats a row
// with no job on this account at all.
func rank(l LocalZip) int {
	switch {
	case l.JobID != 0 && l.RemotePath != "":
		return 2
	case l.JobID != 0:
		return 1
	default:
		return 0
	}
}

// isZipName reports whether name looks like an archive this tool would have
// produced. Case-insensitive because the extension's case is the remote
// server's business, not ours.
func isZipName(name string) bool {
	return len(name) > 4 && strings.EqualFold(name[len(name)-4:], ".zip")
}
