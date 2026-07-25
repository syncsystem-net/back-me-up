package mega

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

// fakeDownload is a chunkSource over a fixed byte slice, split at the given
// boundaries. It stands in for go-mega's *Download so the range-to-chunk mapping
// can be exercised without a live MEGA session — the mapping is the part that
// can silently return the wrong bytes, so it is the part that needs pinning down.
type fakeDownload struct {
	data       []byte
	chunkSizes []int
	fetched    []int // ids actually downloaded, in order
}

// newFakeDownload splits data into chunks of the given sizes. MEGA's real chunk
// sizes are irregular (they ramp up), which is exactly why the sizes are
// explicit here rather than uniform.
func newFakeDownload(data []byte, sizes []int) *fakeDownload {
	return &fakeDownload{data: data, chunkSizes: sizes}
}

func (f *fakeDownload) Chunks() int { return len(f.chunkSizes) }

func (f *fakeDownload) ChunkLocation(id int) (int64, int, error) {
	if id < 0 || id >= len(f.chunkSizes) {
		return 0, 0, errors.New("chunk id out of range")
	}
	var pos int64
	for i := 0; i < id; i++ {
		pos += int64(f.chunkSizes[i])
	}
	return pos, f.chunkSizes[id], nil
}

func (f *fakeDownload) DownloadChunk(id int) ([]byte, error) {
	pos, size, err := f.ChunkLocation(id)
	if err != nil {
		return nil, err
	}
	f.fetched = append(f.fetched, id)
	return f.data[pos : pos+int64(size)], nil
}

func noWait(context.Context) error           { return nil }
func noWaitBytes(context.Context, int) error { return nil }

// testData is 1000 bytes with a position-dependent pattern, so a misplaced copy
// shows up as wrong content rather than coincidentally matching.
func testData(n int) []byte {
	d := make([]byte, n)
	for i := range d {
		d[i] = byte((i*7 + 11) % 251)
	}
	return d
}

// Chunk layout totalling 1000 bytes, deliberately uneven: 100, 250, 50, 400, 200.
var testChunks = []int{100, 250, 50, 400, 200}

func TestReadRangeFromChunkMapping(t *testing.T) {
	data := testData(1000)

	cases := []struct {
		name string
		off  int64
		size int
	}{
		{"whole file", 0, 1000},
		{"aligned to a chunk boundary", 100, 250},
		{"starts mid-chunk", 137, 200},
		{"ends mid-chunk", 100, 173},
		{"starts and ends mid-chunk", 137, 111},
		{"entirely within one chunk", 110, 30},
		{"single byte mid-chunk", 523, 1},
		{"spans every chunk", 1, 998},
		{"covers the final partial chunk", 800, 200},
		{"ends exactly at EOF", 950, 50},
		{"starts at the last byte", 999, 1},
		{"first byte only", 0, 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fd := newFakeDownload(data, testChunks)
			buf := make([]byte, tc.size)

			n, err := readRangeFrom(context.Background(), fd, buf, tc.off, noWait, noWaitBytes)

			if err != nil {
				t.Fatalf("readRangeFrom: unexpected error %v", err)
			}
			if n != tc.size {
				t.Fatalf("read %d bytes, want %d", n, tc.size)
			}
			want := data[tc.off : tc.off+int64(tc.size)]
			if !bytes.Equal(buf, want) {
				t.Errorf("wrong bytes for offset %d len %d", tc.off, tc.size)
			}
		})
	}
}

// A range must cost only the chunks it actually overlaps — that economy is the
// entire reason ranged reads exist here.
func TestReadRangeFromFetchesOnlyOverlappingChunks(t *testing.T) {
	data := testData(1000)

	cases := []struct {
		name string
		off  int64
		size int
		want []int
	}{
		// Chunk boundaries: [0,100) [100,350) [350,400) [400,800) [800,1000)
		{"inside chunk 1 only", 150, 50, []int{1}},
		{"straddles chunks 1 and 2", 340, 30, []int{1, 2}},
		{"tail of the file", 900, 100, []int{4}},
		{"head of the file", 0, 10, []int{0}},
		{"chunks 2 through 4", 360, 500, []int{2, 3, 4}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fd := newFakeDownload(data, testChunks)

			if _, err := readRangeFrom(context.Background(), fd, make([]byte, tc.size), tc.off, noWait, noWaitBytes); err != nil {
				t.Fatalf("readRangeFrom: %v", err)
			}

			if len(fd.fetched) != len(tc.want) {
				t.Fatalf("fetched chunks %v, want %v", fd.fetched, tc.want)
			}
			for i, id := range tc.want {
				if fd.fetched[i] != id {
					t.Fatalf("fetched chunks %v, want %v", fd.fetched, tc.want)
				}
			}
		})
	}
}

// io.ReaderAt semantics: a short read must report io.EOF, and the bytes that did
// exist must still be delivered correctly.
func TestReadRangeFromReturnsEOFOnShortRead(t *testing.T) {
	data := testData(1000)
	fd := newFakeDownload(data, testChunks)

	buf := make([]byte, 100)
	n, err := readRangeFrom(context.Background(), fd, buf, 950, noWait, noWaitBytes)

	if !errors.Is(err, io.EOF) {
		t.Fatalf("got err %v, want io.EOF", err)
	}
	if n != 50 {
		t.Fatalf("read %d bytes, want the 50 that exist", n)
	}
	if !bytes.Equal(buf[:50], data[950:]) {
		t.Error("the bytes that did exist were placed incorrectly")
	}
}

func TestReadRangeFromReturnsEOFPastEnd(t *testing.T) {
	fd := newFakeDownload(testData(1000), testChunks)

	n, err := readRangeFrom(context.Background(), fd, make([]byte, 10), 1000, noWait, noWaitBytes)

	if !errors.Is(err, io.EOF) {
		t.Errorf("got err %v, want io.EOF", err)
	}
	if n != 0 {
		t.Errorf("read %d bytes past EOF, want 0", n)
	}
}

// Every offset/length pair over a small file, against a byte-slice reference.
// The mapping arithmetic has several edges (both range ends, both chunk ends)
// and exhaustively comparing to the obvious implementation is the cheapest way
// to be sure none of them is off by one.
func TestReadRangeFromMatchesReferenceExhaustively(t *testing.T) {
	const size = 120
	data := testData(size)
	chunks := []int{7, 1, 33, 20, 4, 55} // 120 total, deliberately lumpy

	for off := int64(0); off < size; off++ {
		for length := 1; length <= size; length++ {
			fd := newFakeDownload(data, chunks)
			buf := make([]byte, length)

			n, err := readRangeFrom(context.Background(), fd, buf, off, noWait, noWaitBytes)

			available := size - int(off)
			if length > available {
				if !errors.Is(err, io.EOF) {
					t.Fatalf("off=%d len=%d: got err %v, want io.EOF", off, length, err)
				}
				if n != available {
					t.Fatalf("off=%d len=%d: read %d, want %d", off, length, n, available)
				}
			} else {
				if err != nil {
					t.Fatalf("off=%d len=%d: unexpected error %v", off, length, err)
				}
				if n != length {
					t.Fatalf("off=%d len=%d: read %d, want %d", off, length, n, length)
				}
			}
			if !bytes.Equal(buf[:n], data[off:off+int64(n)]) {
				t.Fatalf("off=%d len=%d: wrong bytes", off, length)
			}
		}
	}
}

// A cancelled context must stop the transfer rather than pulling every chunk.
func TestReadRangeFromHonoursContextCancellation(t *testing.T) {
	fd := newFakeDownload(testData(1000), testChunks)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := readRangeFrom(ctx, fd, make([]byte, 500), 0, noWait, noWaitBytes)

	if !errors.Is(err, context.Canceled) {
		t.Errorf("got err %v, want context.Canceled", err)
	}
	if len(fd.fetched) != 0 {
		t.Errorf("cancelled read still fetched chunks %v", fd.fetched)
	}
}

// The rate limiter gates every chunk fetch; a limiter error must abort rather
// than be skipped past.
func TestReadRangeFromPropagatesLimiterErrors(t *testing.T) {
	fd := newFakeDownload(testData(1000), testChunks)
	sentinel := errors.New("rate limit context cancelled")

	_, err := readRangeFrom(context.Background(), fd, make([]byte, 100), 0,
		func(context.Context) error { return sentinel }, noWaitBytes)

	if !errors.Is(err, sentinel) {
		t.Errorf("got err %v, want the limiter's error", err)
	}
	if len(fd.fetched) != 0 {
		t.Errorf("a blocked read still fetched chunks %v", fd.fetched)
	}
}
