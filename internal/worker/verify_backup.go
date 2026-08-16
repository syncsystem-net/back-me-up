package worker

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/syncsystem-net/back-me-up/internal/accounts"
	"github.com/syncsystem-net/back-me-up/internal/database"
	"github.com/syncsystem-net/back-me-up/internal/provider"
	"github.com/syncsystem-net/back-me-up/internal/provider/registry"
)

// errEnoughBytes halts a verification download once the first chunk has been
// read, so we don't pull the whole file back just to check its head.
var errEnoughBytes = errors.New("verify: first chunk read")

// verify downloads the first chunk of the uploaded object and compares its
// SHA-256 with the same span of the local zip. This is a cheap integrity check
// that catches a corrupted or truncated upload without re-downloading gigabytes.
// It returns the verified first-chunk checksum so the caller can persist it for
// later periodic re-verification (by then the local zip is gone).
func (w *Worker) verify(ctx context.Context, p provider.Provider, job *database.Job, remoteRef string) (string, error) {
	limit := w.verifyLimit()

	localSum, err := hashFileHead(job.ZipPath, limit)
	if err != nil {
		return "", fmt.Errorf("hashing local head: %w", err)
	}

	remoteSum, err := w.downloadHeadSum(ctx, p, remoteRef, limit)
	if err != nil {
		return "", err
	}

	if localSum != remoteSum {
		return "", fmt.Errorf("checksum mismatch (local %s != remote %s)", localSum[:12], remoteSum[:12])
	}
	return localSum, nil
}

// verifyLimit is the number of leading bytes verification hashes (the configured
// chunk size, with a sane fallback). Both upload verification and periodic
// re-verification hash this same span; changing chunk_size_mb between an
// upload and a later re-verify can therefore cause a false mismatch.
func (w *Worker) verifyLimit() int64 {
	limit := w.cfg.ChunkSizeBytes
	if limit <= 0 {
		limit = 100 << 20
	}
	return limit
}

// downloadHeadSum downloads the first limit bytes of remoteRef and returns their
// hex SHA-256, stopping the transfer once enough bytes have been read.
func (w *Worker) downloadHeadSum(ctx context.Context, p provider.Provider, remoteRef string, limit int64) (string, error) {
	cw := &cappedHasher{h: sha256.New(), limit: limit}
	if err := p.Download(ctx, remoteRef, cw); err != nil && !errors.Is(err, errEnoughBytes) {
		return "", fmt.Errorf("downloading head: %w", err)
	}
	return fmt.Sprintf("%x", cw.h.Sum(nil)), nil
}

// backupDatabase checkpoints the WAL, copies the SQLite file, and uploads that
// one copy to every configured main account, so the metadata survives loss of
// this machine — and loss of any single provider. Any failure here is logged but
// never fails the originating job.
func (w *Worker) backupDatabase(ctx context.Context) {
	mains := w.usableMains()
	if len(mains) == 0 {
		// Said once per process, not once per job: both of these are standing
		// states, not events, and repeating them after every successful upload
		// would train the user to ignore the log. The two cases are distinct —
		// telling someone who configured a main account that they have none
		// would contradict the startup log and send them looking in the wrong
		// place.
		w.noMainOnce.Do(func() {
			configured := len(w.accounts.Mains())
			if configured == 0 {
				slog.Info("no main account configured; the metadata database is not being copied off this machine")
				return
			}
			slog.Warn("no usable main account; the metadata database is not being copied off this machine",
				"configured", configured)
		})
		return
	}

	// Flush WAL into the main db file so the copy is consistent.
	if _, err := w.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		slog.Warn("wal checkpoint before db backup failed", "error", err)
	}

	// One snapshot, uploaded to every destination, so the copies agree with each
	// other and the DB is read once however many main accounts are configured.
	tmp, err := copyToTemp(w.dbPath)
	if err != nil {
		slog.Warn("copying db for backup failed", "error", err)
		return
	}
	defer os.Remove(tmp)

	// The timestamp keeps successive backups from colliding on 4shared, which
	// refuses an upload whose name already exists rather than replacing it.
	name := fmt.Sprintf("backmeup-metadata-%s.db", time.Now().Format("20060102-150405"))
	if uploaded := w.uploadDBToMains(ctx, mains, tmp, name); uploaded < len(mains) {
		// Each failure was logged with its reason; this is the summary that says
		// how much of the index actually made it off the machine.
		slog.Warn("metadata db backup incomplete", "uploaded", uploaded, "destinations", len(mains))
	}
}

// uploadDBToMains uploads path to every main account. Each destination is
// attempted independently: one expired token or unreachable provider must not
// cost the other providers their copy of the index — the same rule the delete
// path follows. Returns the number of destinations that received the file.
func (w *Worker) uploadDBToMains(ctx context.Context, mains []accounts.MainAccount, path, name string) int {
	uploaded := 0
	for _, m := range mains {
		p, err := w.newMainProvider(m)
		if err != nil {
			slog.Warn("metadata db backup failed", "provider", m.Provider, "email", m.Email, "stage", "building provider", "error", err)
			continue
		}
		if err := p.Login(ctx, m.Email, m.Password); err != nil {
			slog.Warn("metadata db backup failed", "provider", m.Provider, "email", m.Email, "stage", "login", "error", err)
			continue
		}
		if _, err := p.Upload(ctx, path, name, nil); err != nil {
			slog.Warn("metadata db backup failed", "provider", m.Provider, "email", m.Email, "stage", "upload", "error", err)
			continue
		}
		uploaded++
		slog.Info("metadata db backed up to main account", "provider", m.Provider, "email", m.Email, "name", name)
	}
	return uploaded
}

// usableMains returns the main accounts that can actually be logged in to: ones
// whose provider we support and whose credentials are complete. Both kinds of
// exclusion are reported once at startup (accounts.Load for an incomplete
// account, logMainAccounts for an unsupported provider), so this filters
// silently — it runs after every completed job, and a standing misconfiguration
// must not produce a line per upload.
func (w *Worker) usableMains() []accounts.MainAccount {
	var usable []accounts.MainAccount
	for _, m := range w.accounts.Mains() {
		if m.Usable() && registry.Supported(string(m.Provider)) {
			usable = append(usable, m)
		}
	}
	return usable
}

// cappedHasher hashes the bytes written to it until limit is reached, then
// returns errEnoughBytes to stop the upstream copy.
type cappedHasher struct {
	h     hash.Hash
	n     int64
	limit int64
}

func (c *cappedHasher) Write(p []byte) (int, error) {
	remaining := c.limit - c.n
	if remaining <= 0 {
		// Already have the first chunk; report all bytes consumed so callers see
		// a clean stop rather than a short-write error, then signal to halt.
		return len(p), errEnoughBytes
	}
	if int64(len(p)) <= remaining {
		c.h.Write(p)
		c.n += int64(len(p))
		return len(p), nil
	}
	c.h.Write(p[:remaining])
	c.n += remaining
	return len(p), errEnoughBytes
}

// hashFileHead returns the hex SHA-256 of the first limit bytes of path.
func hashFileHead(path string, limit int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.CopyN(h, f, limit); err != nil && err != io.EOF {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// copyToTemp copies src to a sibling temp file and returns its path.
func copyToTemp(src string) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()
	out, err := os.CreateTemp(filepath.Dir(src), "backmeup-dbbackup-*.db")
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(out.Name())
		return "", err
	}
	if err := out.Close(); err != nil {
		os.Remove(out.Name())
		return "", err
	}
	return out.Name(), nil
}

func removeFile(path string) error {
	if path == "" {
		return nil
	}
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
