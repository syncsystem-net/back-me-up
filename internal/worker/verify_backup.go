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
func (w *Worker) verify(ctx context.Context, p provider.Provider, job *database.Job, remoteRef string) error {
	limit := w.cfg.ChunkSizeBytes
	if limit <= 0 {
		limit = 100 << 20
	}

	localSum, err := hashFileHead(job.ZipPath, limit)
	if err != nil {
		return fmt.Errorf("hashing local head: %w", err)
	}

	cw := &cappedHasher{h: sha256.New(), limit: limit}
	if err := p.Download(ctx, remoteRef, cw); err != nil && !errors.Is(err, errEnoughBytes) {
		return fmt.Errorf("downloading head: %w", err)
	}
	remoteSum := fmt.Sprintf("%x", cw.h.Sum(nil))

	if localSum != remoteSum {
		return fmt.Errorf("checksum mismatch (local %s != remote %s)", localSum[:12], remoteSum[:12])
	}
	return nil
}

// backupDatabase checkpoints the WAL, copies the SQLite file, and uploads the
// copy to the main account so the metadata survives loss of this machine. Any
// failure here is logged but never fails the originating job.
func (w *Worker) backupDatabase(ctx context.Context) {
	main := w.accounts.Main
	if main.Provider == "" || main.Email == "" {
		slog.Warn("no main account configured; skipping metadata DB backup")
		return
	}
	if !registry.Supported(string(main.Provider)) {
		slog.Warn("main account provider unsupported; skipping DB backup", "provider", main.Provider)
		return
	}

	// Flush WAL into the main db file so the copy is consistent.
	if _, err := w.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		slog.Warn("wal checkpoint before db backup failed", "error", err)
	}

	tmp, err := copyToTemp(w.dbPath)
	if err != nil {
		slog.Warn("copying db for backup failed", "error", err)
		return
	}
	defer os.Remove(tmp)

	p, err := registry.New(string(main.Provider), w.mainOAuth(), provider.Config{ChunkSizeBytes: w.cfg.ChunkSizeBytes})
	if err != nil {
		slog.Warn("building main provider failed", "error", err)
		return
	}
	if err := p.Login(ctx, main.Email, main.Password); err != nil {
		slog.Warn("main account login failed; skipping db backup", "error", err)
		return
	}
	name := fmt.Sprintf("backmeup-metadata-%s.db", time.Now().Format("20060102-150405"))
	if _, err := p.Upload(ctx, tmp, name, nil); err != nil {
		slog.Warn("uploading metadata db backup failed", "error", err)
		return
	}
	slog.Info("metadata db backed up to main account", "provider", main.Provider, "name", name)
}

// mainOAuth supplies OAuth creds when the main account is an OAuth provider.
func (w *Worker) mainOAuth() provider.OAuthCreds {
	if w.accounts.Main.Provider != "fourshared" {
		return provider.OAuthCreds{}
	}
	// Match the main account against the configured 4shared accounts to find its
	// per-account consumer creds and tokens (the main account may also appear in
	// the account list).
	for _, a := range w.accounts.Accounts {
		if a.Email == w.accounts.Main.Email && string(a.Provider) == "fourshared" {
			return provider.OAuthCreds{
				ConsumerKey:    a.ConsumerKey,
				ConsumerSecret: a.ConsumerSecret,
				Token:          a.OAuthToken,
				TokenSecret:    a.OAuthTokenSecret,
			}
		}
	}
	return provider.OAuthCreds{
		ConsumerKey:    w.accounts.FourShared.ConsumerKey,
		ConsumerSecret: w.accounts.FourShared.ConsumerSecret,
	}
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
