package autosync

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/syncsystem-net/back-me-up/internal/provider"
	"github.com/syncsystem-net/back-me-up/internal/scanner"
)

// fakeProvider serves a fixed byte slice through the provider interface. Only
// the methods the indexer touches are meaningful; the rest satisfy the contract.
type fakeProvider struct {
	data []byte
	// rangeSupported false emulates a backend (4shared, possibly) that ignores
	// the Range header and answers 200 with the whole object.
	rangeSupported bool
	reads          int
	readBytes      int
	downloads      int
}

func (f *fakeProvider) Name() string                                   { return "fake" }
func (f *fakeProvider) Login(context.Context, string, string) error    { return nil }
func (f *fakeProvider) Delete(context.Context, string) error           { return nil }
func (f *fakeProvider) GetQuota(context.Context) (int64, int64, error) { return 0, 0, nil }
func (f *fakeProvider) List(context.Context) ([]provider.RemoteFile, error) {
	return []provider.RemoteFile{{ID: "ref", Name: "fake.zip", Size: int64(len(f.data))}}, nil
}
func (f *fakeProvider) FindByName(context.Context, string) (string, bool, error) {
	return "ref", true, nil
}
func (f *fakeProvider) Upload(context.Context, string, string, func(provider.Progress)) (string, error) {
	return "ref", nil
}

func (f *fakeProvider) Download(_ context.Context, _ string, w io.Writer) error {
	f.downloads++
	_, err := w.Write(f.data)
	return err
}

func (f *fakeProvider) ReadRange(_ context.Context, _ string, p []byte, off int64) (int, error) {
	f.reads++
	if !f.rangeSupported {
		return 0, provider.ErrRangeUnsupported
	}
	if off >= int64(len(f.data)) {
		return 0, io.EOF
	}
	n := copy(p, f.data[off:])
	f.readBytes += n
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func TestRemoteReaderAtServesRequestedRanges(t *testing.T) {
	data := make([]byte, readBlockSize*3+17)
	for i := range data {
		data[i] = byte(i % 251)
	}
	fp := &fakeProvider{data: data, rangeSupported: true}
	r := newRemoteReaderAt(context.Background(), fp, "ref", int64(len(data)))

	// A read that spans three blocks, starting mid-block.
	off := int64(readBlockSize) - 5
	buf := make([]byte, readBlockSize*2+10)
	n, err := r.ReadAt(buf, off)
	if err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if n != len(buf) {
		t.Fatalf("read %d bytes, want %d", n, len(buf))
	}
	if !bytes.Equal(buf, data[off:off+int64(len(buf))]) {
		t.Error("ranged read returned the wrong bytes")
	}
}

func TestRemoteReaderAtReturnsEOFPastEnd(t *testing.T) {
	fp := &fakeProvider{data: []byte("0123456789"), rangeSupported: true}
	r := newRemoteReaderAt(context.Background(), fp, "ref", 10)

	if _, err := r.ReadAt(make([]byte, 4), 10); !errors.Is(err, io.EOF) {
		t.Errorf("reading at EOF: got %v, want io.EOF", err)
	}
	// A read straddling the end returns what exists, plus EOF.
	buf := make([]byte, 6)
	n, err := r.ReadAt(buf, 7)
	if n != 3 || !errors.Is(err, io.EOF) {
		t.Errorf("straddling read: got n=%d err=%v, want n=3 io.EOF", n, err)
	}
	if string(buf[:3]) != "789" {
		t.Errorf("straddling read returned %q, want %q", buf[:3], "789")
	}
}

// The stdlib issues many small reads while parsing a central directory; the
// block cache must collapse those into a handful of fetches.
func TestRemoteReaderAtCachesBlocks(t *testing.T) {
	data := make([]byte, readBlockSize*2)
	fp := &fakeProvider{data: data, rangeSupported: true}
	r := newRemoteReaderAt(context.Background(), fp, "ref", int64(len(data)))

	for i := 0; i < 50; i++ {
		if _, err := r.ReadAt(make([]byte, 8), int64(i*16)); err != nil {
			t.Fatalf("ReadAt %d: %v", i, err)
		}
	}
	if fp.reads != 1 {
		t.Errorf("50 reads inside one block caused %d remote fetches, want 1", fp.reads)
	}
}

// The critical failure mode: a provider that ignores the Range header returns
// the whole object with a 200. The reader must surface that as
// ErrRangeUnsupported and never treat those leading bytes as if they began at
// the requested offset, which would silently produce a corrupt tree.
func TestRemoteReaderAtPropagatesRangeUnsupported(t *testing.T) {
	fp := &fakeProvider{data: make([]byte, 1024), rangeSupported: false}
	r := newRemoteReaderAt(context.Background(), fp, "ref", 1024)

	n, err := r.ReadAt(make([]byte, 16), 512)

	if !errors.Is(err, provider.ErrRangeUnsupported) {
		t.Fatalf("got err=%v, want ErrRangeUnsupported", err)
	}
	if n != 0 {
		t.Errorf("a rejected ranged read must yield no bytes, got %d", n)
	}
}

// entryBodySize is each test entry's uncompressed size.
const entryBodySize = 300 << 10

// buildZip returns a real archive whose entries are large enough that reading
// only its central directory is visibly cheaper than reading the whole thing.
func buildZip(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range []string{
		"payload/root.txt",
		"payload/alpha/one.bin",
		"payload/alpha/two.bin",
		"payload/beta/nested/deep.dat",
	} {
		// Stored (uncompressed) bodies, so the archive is comfortably larger
		// than several read blocks and a tail-only read is actually measurable.
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
		if err != nil {
			t.Fatalf("creating %s: %v", name, err)
		}
		if _, err := w.Write(bytes.Repeat([]byte("x"), entryBodySize)); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("closing zip: %v", err)
	}
	return buf.Bytes()
}

// The whole indexing path: archive/zip locating and parsing a central directory
// through the provider-backed ReaderAt, and the tree built from what it found.
func TestZipReaderOverRemoteReaderAt(t *testing.T) {
	data := buildZip(t)
	fp := &fakeProvider{data: data, rangeSupported: true}
	r := newRemoteReaderAt(context.Background(), fp, "ref", int64(len(data)))

	zr, err := zip.NewReader(r, int64(len(data)))
	if err != nil {
		t.Fatalf("zip.NewReader over the remote reader: %v", err)
	}

	root := TreeFromEntries("payload.zip", EntriesFromReader(zr), scanner.Options{MaxDepth: 3})

	if root.Name != "payload" {
		t.Fatalf("root = %q, want %q", root.Name, "payload")
	}
	if root.SizeBytes != entryBodySize {
		t.Errorf("root size = %d, want %d", root.SizeBytes, entryBodySize)
	}
	if got := child(t, root, "alpha").SizeBytes; got != 2*entryBodySize {
		t.Errorf("alpha size = %d, want %d", got, 2*entryBodySize)
	}
	beta := child(t, root, "beta")
	if got := child(t, beta, "nested").SizeBytes; got != entryBodySize {
		t.Errorf("nested size = %d, want %d", got, entryBodySize)
	}

	// The point of ranged reads: only a fraction of the archive moved.
	if fp.readBytes >= len(data)/2 {
		t.Errorf("read %d bytes of a %d byte archive; the tail-only read did not happen", fp.readBytes, len(data))
	}
}

// When ranges are unavailable, zip.NewReader fails with ErrRangeUnsupported so
// the caller can fall back to a full download rather than guessing.
func TestZipReaderSurfacesRangeUnsupported(t *testing.T) {
	data := buildZip(t)
	fp := &fakeProvider{data: data, rangeSupported: false}
	r := newRemoteReaderAt(context.Background(), fp, "ref", int64(len(data)))

	_, err := zip.NewReader(r, int64(len(data)))

	if !errors.Is(err, provider.ErrRangeUnsupported) {
		t.Fatalf("got %v, want an error wrapping ErrRangeUnsupported", err)
	}
	if fp.downloads != 0 {
		t.Errorf("the reader must not download anything on its own, got %d downloads", fp.downloads)
	}
}
