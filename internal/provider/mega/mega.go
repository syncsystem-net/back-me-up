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
	"os"

	gomega "github.com/t3rm1n4l/go-mega"

	"github.com/syncsystem-net/back-me-up/internal/provider"
)

// Client is a logged-in (or about-to-be) MEGA session bound to one account.
type Client struct {
	m *gomega.Mega
}

// New returns an unauthenticated MEGA client. The chunkSize argument is part of
// the provider contract but ignored here: MEGA's protocol dictates its own
// chunk boundaries via the upload session.
func New(_ int64) *Client {
	return &Client{m: gomega.New()}
}

func (c *Client) Name() string { return "mega" }

func (c *Client) Login(ctx context.Context, email, password string) error {
	// go-mega's Login is blocking and not context-aware; honour an already
	// cancelled context before spending a network round-trip.
	if err := ctx.Err(); err != nil {
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
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	root := c.m.FS.GetRoot()
	children, err := c.m.FS.GetChildren(root)
	if err != nil {
		return "", false, fmt.Errorf("listing mega root: %w", err)
	}
	for _, n := range children {
		if n.GetName() == name {
			return n.GetHash(), true, nil
		}
	}
	return "", false, nil
}

func (c *Client) Delete(ctx context.Context, remoteRef string) error {
	if err := ctx.Err(); err != nil {
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
	q, err := c.m.GetQuota()
	if err != nil {
		return 0, 0, fmt.Errorf("getting mega quota: %w", err)
	}
	return int64(q.Mstrg), int64(q.Cstrg), nil
}
