package autosync

import (
	"context"
	"errors"
	"io"

	"github.com/syncsystem-net/back-me-up/internal/provider"
)

const (
	// readBlockSize is the granularity every remote fetch is rounded up to.
	// archive/zip probes the tail of the file with a series of small reads while
	// it hunts for the end-of-central-directory record and then walks the
	// directory entry by entry; serving those from block-aligned fetches turns
	// dozens of tiny requests into a handful.
	readBlockSize = 256 << 10

	// maxCachedBlocks bounds the reader's memory at readBlockSize * this. The
	// access pattern is a backwards scan of the tail followed by a forward walk,
	// so a small window covers nearly every read.
	maxCachedBlocks = 16
)

// remoteReaderAt adapts a provider's ranged read to io.ReaderAt so archive/zip
// can locate and parse a remote archive's central directory itself. Only the
// blocks the stdlib actually touches are transferred, which is the whole point:
// the directory sits at the tail, so indexing a multi-gigabyte archive costs a
// few hundred kilobytes.
//
// It is not safe for concurrent use; each indexing pass gets its own.
type remoteReaderAt struct {
	ctx    context.Context
	p      provider.Provider
	ref    string
	size   int64
	blocks map[int64][]byte
	order  []int64 // insertion order, for eviction
}

func newRemoteReaderAt(ctx context.Context, p provider.Provider, ref string, size int64) *remoteReaderAt {
	return &remoteReaderAt{ctx: ctx, p: p, ref: ref, size: size, blocks: map[int64][]byte{}}
}

// ReadAt implements io.ReaderAt over block-aligned remote fetches.
func (r *remoteReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if off < 0 {
		return 0, errors.New("autosync: negative read offset")
	}
	if off >= r.size {
		return 0, io.EOF
	}

	var n int
	for n < len(p) {
		pos := off + int64(n)
		if pos >= r.size {
			return n, io.EOF
		}
		blockStart := pos - pos%readBlockSize
		block, err := r.block(blockStart)
		if err != nil {
			return n, err
		}
		inBlock := pos - blockStart
		if inBlock >= int64(len(block)) {
			return n, io.EOF
		}
		n += copy(p[n:], block[inBlock:])
	}
	return n, nil
}

// block returns the cached block starting at start, fetching it if needed. A
// short read at the end of the file is kept as a short block rather than an
// error, so the final partial block behaves like any other.
func (r *remoteReaderAt) block(start int64) ([]byte, error) {
	if b, ok := r.blocks[start]; ok {
		return b, nil
	}
	size := int64(readBlockSize)
	if remaining := r.size - start; remaining < size {
		size = remaining
	}
	if size <= 0 {
		return nil, io.EOF
	}

	buf := make([]byte, size)
	n, err := r.p.ReadRange(r.ctx, r.ref, buf, start)
	if err != nil && !errors.Is(err, io.EOF) {
		// ErrRangeUnsupported travels up unchanged: the caller has an explicit
		// fallback for it and must not mistake it for a transport failure.
		return nil, err
	}
	if n == 0 {
		return nil, io.EOF
	}
	buf = buf[:n]

	r.blocks[start] = buf
	r.order = append(r.order, start)
	if len(r.order) > maxCachedBlocks {
		delete(r.blocks, r.order[0])
		r.order = r.order[1:]
	}
	return buf, nil
}
