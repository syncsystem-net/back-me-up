// Package mega implements the provider.Provider interface for MEGA, using the
// github.com/t3rm1n4l/go-mega client which handles MEGA's proprietary
// encryption. Uploads go to the account's cloud root and are performed chunk by
// chunk so the worker can report progress; the returned remoteRef is the node
// hash, which Download and Delete resolve via the filesystem.
package mega

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"

	gomega "github.com/t3rm1n4l/go-mega"

	"github.com/syncsystem-net/back-me-up/internal/provider"
)

// Client is a logged-in (or about-to-be) MEGA session bound to one account.
type Client struct {
	m       *gomega.Mega
	limiter provider.RateLimiter
}

// New returns an unauthenticated MEGA client. The chunkSize argument is part of
// the provider contract but ignored here: MEGA's protocol dictates its own
// chunk boundaries via the upload session. limiter (may be nil) paces requests
// and bandwidth; because go-mega owns its own HTTP client, limiting here is
// applied at chunk granularity rather than per underlying HTTP request.
func New(_ int64, limiter provider.RateLimiter) *Client {
	return &Client{m: gomega.New(), limiter: limiter}
}

// wait blocks for one request token (nil limiter is a no-op).
func (c *Client) wait(ctx context.Context) error {
	if c.limiter == nil {
		return nil
	}
	return c.limiter.WaitRequest(ctx)
}

// waitBytes blocks for n bytes of bandwidth budget (nil limiter is a no-op).
func (c *Client) waitBytes(ctx context.Context, n int) error {
	if c.limiter == nil {
		return nil
	}
	return c.limiter.WaitBytes(ctx, n)
}

func (c *Client) Name() string { return "mega" }

func (c *Client) Login(ctx context.Context, email, password string) error {
	// go-mega's Login is blocking and not context-aware; honour an already
	// cancelled context before spending a network round-trip.
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.wait(ctx); err != nil {
		return err
	}
	if err := c.m.Login(email, password); err != nil {
		return fmt.Errorf("mega login (%s): %w", email, err)
	}
	return nil
}

func (c *Client) Upload(ctx context.Context, localPath, remoteName string, onProgress func(provider.Progress)) (string, error) {
	f, err := os.Open(localPath)
	if err != nil {
		return "", fmt.Errorf("opening %s: %w", localPath, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", localPath, err)
	}
	size := info.Size()

	root := c.m.FS.GetRoot()
	u, err := c.m.NewUpload(root, remoteName, size)
	if err != nil {
		return "", fmt.Errorf("starting mega upload: %w", err)
	}

	chunks := u.Chunks()
	var uploaded int64
	for id := 0; id < chunks; id++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		pos, csize, err := u.ChunkLocation(id)
		if err != nil {
			return "", fmt.Errorf("locating chunk %d: %w", id, err)
		}
		buf := make([]byte, csize)
		if _, err := f.ReadAt(buf, pos); err != nil && err != io.EOF {
			return "", fmt.Errorf("reading chunk %d: %w", id, err)
		}
		// Pace per chunk: one request token, then this chunk's bytes of bandwidth.
		// go-mega owns the underlying HTTP, so this is the finest granularity we
		// can throttle MEGA at.
		if err := c.wait(ctx); err != nil {
			return "", err
		}
		if err := c.waitBytes(ctx, csize); err != nil {
			return "", err
		}
		if err := u.UploadChunk(id, buf); err != nil {
			return "", fmt.Errorf("uploading chunk %d/%d: %w", id+1, chunks, err)
		}
		uploaded += int64(csize)
		if onProgress != nil {
			onProgress(provider.Progress{
				UploadedBytes:  uploaded,
				TotalBytes:     size,
				ChunksUploaded: id + 1,
				ChunksTotal:    chunks,
			})
		}
	}

	node, err := u.Finish()
	if err != nil {
		return "", fmt.Errorf("finalising mega upload: %w", err)
	}
	return node.GetHash(), nil
}

func (c *Client) Download(ctx context.Context, remoteRef string, w io.Writer) error {
	node := c.m.FS.HashLookup(remoteRef)
	if node == nil {
		return fmt.Errorf("mega node %q not found", remoteRef)
	}
	d, err := c.m.NewDownload(node)
	if err != nil {
		return fmt.Errorf("starting mega download: %w", err)
	}
	for id := 0; id < d.Chunks(); id++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := c.wait(ctx); err != nil {
			return err
		}
		chunk, err := d.DownloadChunk(id)
		if err != nil {
			return fmt.Errorf("downloading chunk %d: %w", id, err)
		}
		if _, err := w.Write(chunk); err != nil {
			return fmt.Errorf("writing chunk %d: %w", id, err)
		}
	}
	if err := d.Finish(); err != nil {
		return fmt.Errorf("finalising mega download: %w", err)
	}
	return nil
}

func (c *Client) FindByName(ctx context.Context, name string) (string, bool, error) {
	files, err := c.List(ctx)
	if err != nil {
		return "", false, err
	}
	for _, f := range files {
		if f.Name == name {
			return f.ID, true, nil
		}
	}
	return "", false, nil
}

// List returns the files in the account's cloud root. Folder nodes are skipped:
// the crawl only cares about the archives Upload writes at the root.
func (c *Client) List(ctx context.Context) ([]provider.RemoteFile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := c.wait(ctx); err != nil {
		return nil, err
	}
	root := c.m.FS.GetRoot()
	children, err := c.m.FS.GetChildren(root)
	if err != nil {
		return nil, fmt.Errorf("listing mega root: %w", err)
	}
	files := make([]provider.RemoteFile, 0, len(children))
	for _, n := range children {
		if n.GetType() != gomega.FILE {
			continue
		}
		files = append(files, provider.RemoteFile{
			ID:   n.GetHash(),
			Name: n.GetName(),
			Size: n.GetSize(),
		})
	}
	return files, nil
}

// ReadRange serves a byte range out of a MEGA object. MEGA supports genuine
// random access: a download session exposes each chunk's offset and size, and a
// chunk can be fetched (and decrypted) on its own, so a range maps to the chunks
// it overlaps. Only those chunks are transferred, which is what makes reading a
// large archive's central directory cheap.
//
// Finish is still called so the session is closed cleanly.
func (c *Client) ReadRange(ctx context.Context, remoteRef string, p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if off < 0 {
		return 0, fmt.Errorf("mega read range: negative offset %d", off)
	}
	node := c.m.FS.HashLookup(remoteRef)
	if node == nil {
		return 0, fmt.Errorf("mega node %q not found", remoteRef)
	}
	if off >= node.GetSize() {
		return 0, io.EOF
	}

	d, err := c.m.NewDownload(node)
	if err != nil {
		return 0, fmt.Errorf("starting mega download: %w", err)
	}

	n, readErr := readRangeFrom(ctx, d, p, off, c.wait, c.waitBytes)

	// go-mega returns nil from Finish for a partial download — it cannot check
	// the MAC of a transfer it only saw part of — so this is normally a no-op.
	// It is NOT a no-op when the requested range happened to cover the whole
	// file, in which case the MAC is checked for real. The read still stands
	// (the bytes are the caller's to use, and this is a directory listing, not a
	// restore), but a genuine integrity failure must not vanish silently.
	if err := d.Finish(); err != nil {
		slog.Warn("mega: integrity check failed after a ranged read covering the whole file",
			"ref", remoteRef, "error", err)
	}
	return n, readErr
}

// chunkSource is the slice of go-mega's *Download that ranged reads need. It is
// an interface so the chunk-to-range mapping below can be tested against a
// synthetic chunk table instead of a live MEGA session.
type chunkSource interface {
	Chunks() int
	ChunkLocation(id int) (position int64, size int, err error)
	DownloadChunk(id int) ([]byte, error)
}

// readRangeFrom copies the bytes covering [off, off+len(p)) out of src, fetching
// only the chunks that overlap the range. MEGA dictates its own chunk
// boundaries, so a range routinely starts and ends mid-chunk; each fetched chunk
// is clipped to the overlap and placed at its own offset within p.
//
// It follows io.ReaderAt semantics: a short read is always accompanied by an
// error, and a range extending past the end of the file yields io.EOF along with
// whatever did exist.
func readRangeFrom(ctx context.Context, src chunkSource, p []byte, off int64,
	wait func(context.Context) error, waitBytes func(context.Context, int) error) (int, error) {

	end := off + int64(len(p)) // exclusive
	var n int
	for id := 0; id < src.Chunks(); id++ {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		pos, size, err := src.ChunkLocation(id)
		if err != nil {
			return n, fmt.Errorf("locating chunk %d: %w", id, err)
		}
		chunkEnd := pos + int64(size)
		if chunkEnd <= off {
			continue // entirely before the range
		}
		if pos >= end {
			break // chunks are ordered, so everything after this is past the range
		}
		if err := wait(ctx); err != nil {
			return n, err
		}
		if err := waitBytes(ctx, size); err != nil {
			return n, err
		}
		chunk, err := src.DownloadChunk(id)
		if err != nil {
			return n, fmt.Errorf("downloading chunk %d: %w", id, err)
		}
		// Clip to the overlap. The bounds are re-derived from the chunk actually
		// returned rather than trusted from ChunkLocation, so a short chunk
		// cannot slice out of range.
		from := max64(off, pos) - pos
		to := min64(end, chunkEnd) - pos
		if to > int64(len(chunk)) {
			to = int64(len(chunk))
		}
		if from < 0 || from >= to {
			continue
		}
		n += copy(p[max64(off, pos)-off:], chunk[from:to])
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func (c *Client) Delete(ctx context.Context, remoteRef string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.wait(ctx); err != nil {
		return err
	}
	node := c.m.FS.HashLookup(remoteRef)
	if node == nil {
		return fmt.Errorf("mega node %q not found", remoteRef)
	}
	// destroy=true removes permanently rather than moving to trash.
	if err := c.m.Delete(node, true); err != nil {
		return fmt.Errorf("deleting mega node %q: %w", remoteRef, err)
	}
	return nil
}

func (c *Client) GetQuota(ctx context.Context) (int64, int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	if err := c.wait(ctx); err != nil {
		return 0, 0, err
	}
	q, err := c.m.GetQuota()
	if err != nil {
		return 0, 0, fmt.Errorf("getting mega quota: %w", err)
	}
	return int64(q.Mstrg), int64(q.Cstrg), nil
}
