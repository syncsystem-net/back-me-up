// Package provider defines the abstraction every cloud-storage backend
// implements. Concrete backends live in subpackages (mega, fourshared) and are
// wired together by the registry subpackage, so adding a new provider is a
// focused change: implement Provider, register it in the registry.
package provider

import (
	"context"
	"errors"
	"io"
)

// ErrRangeUnsupported is returned by ReadRange when the backend cannot serve a
// partial read — either it has no range support at all, or the server ignored
// the request and answered with the whole object. Callers must treat it as
// "ranged access is not available here" and fall back explicitly (e.g. download
// the whole file) rather than assuming the bytes they got start at the offset
// they asked for. Mis-slicing a full response as if it were a range would
// silently produce garbage.
var ErrRangeUnsupported = errors.New("provider does not support ranged reads")

// RemoteFile is one object in an account's cloud root as reported by List. Size
// is best-effort: a backend whose listing omits it reports 0.
type RemoteFile struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// Progress reports the cumulative state of an in-flight upload. It is delivered
// to the worker after each chunk so progress can be persisted to the database.
type Progress struct {
	UploadedBytes  int64
	TotalBytes     int64
	ChunksUploaded int
	ChunksTotal    int
}

// Provider is a cloud-storage backend (MEGA, 4shared, ...).
//
// A Provider instance is stateful: Login must be called before Upload,
// Download, Delete, or GetQuota, and an instance is bound to a single account
// for its lifetime. Implementations are not required to be safe for concurrent
// use; the worker gives each in-flight job its own logged-in instance.
type Provider interface {
	// Name returns the provider identifier ("mega", "fourshared").
	Name() string

	// Login authenticates the instance against a single account.
	Login(ctx context.Context, email, password string) error

	// Upload sends the file at localPath to the account's cloud root using the
	// name remoteName. The upload is performed in chunks; onProgress (may be
	// nil) is invoked after each chunk with cumulative counts. It returns a
	// remoteRef — an opaque handle the same provider can later resolve in
	// Download and Delete. Resume across process restarts is best-effort: a
	// provider that cannot resume a server-side session restarts the transfer.
	Upload(ctx context.Context, localPath, remoteName string, onProgress func(Progress)) (remoteRef string, err error)

	// Download streams the object identified by remoteRef into w.
	Download(ctx context.Context, remoteRef string, w io.Writer) error

	// List returns every file in the account's cloud root (the location Upload
	// writes to). Directories are not reported — only files. It backs the
	// auto-sync crawl, which reconciles what is actually stored on an account
	// against the local database.
	List(ctx context.Context) ([]RemoteFile, error)

	// ReadRange reads up to len(p) bytes of remoteRef starting at byte offset
	// off, following io.ReaderAt semantics: it returns a short read only
	// together with an error, and io.EOF once off is at or past the end of the
	// object. It exists so a remote ZIP's central directory (which lives at the
	// tail of the archive) can be read without transferring the whole file.
	//
	// A backend that cannot serve partial reads must return ErrRangeUnsupported
	// rather than the whole object's leading bytes.
	ReadRange(ctx context.Context, remoteRef string, p []byte, off int64) (int, error)

	// FindByName looks for a file with the given name in the account's cloud
	// root (the same location Upload writes to). It returns the matching
	// remoteRef and found=true when one exists, or found=false when none does.
	// It is used to detect name conflicts before queuing a backup so the user
	// can choose to overwrite or skip.
	FindByName(ctx context.Context, name string) (remoteRef string, found bool, err error)

	// Delete removes the object identified by remoteRef from the provider.
	Delete(ctx context.Context, remoteRef string) error

	// GetQuota returns the account's total and used capacity in bytes.
	GetQuota(ctx context.Context) (totalBytes, usedBytes int64, err error)
}

// RateLimiter paces a provider's API requests and upload bandwidth. It is
// declared here (rather than imported) so backends depend only on this package;
// internal/ratelimit provides the concrete implementation. A nil RateLimiter
// means no limiting, so providers must tolerate it.
type RateLimiter interface {
	// WaitRequest blocks until one request may proceed, or ctx is cancelled.
	WaitRequest(ctx context.Context) error
	// WaitBytes blocks until n bytes of bandwidth budget are available, or ctx is
	// cancelled.
	WaitBytes(ctx context.Context, n int) error
}

// Config carries provider-agnostic settings the registry passes to every
// backend at construction time.
type Config struct {
	// ChunkSizeBytes is the preferred upload chunk size. Providers that control
	// their own chunk boundaries (e.g. MEGA's protocol) may ignore it.
	ChunkSizeBytes int64

	// RateLimiter paces this provider's requests and bandwidth. May be nil (no
	// limiting); providers must guard against that.
	RateLimiter RateLimiter
}

// OAuthCreds carries the credentials an OAuth-based provider needs at
// construction time. Password providers (MEGA) leave it zero and authenticate
// through Login instead; OAuth providers (4shared) receive their app-level
// consumer key/secret and per-account access token here, because there is no
// password to pass to Login.
type OAuthCreds struct {
	ConsumerKey    string
	ConsumerSecret string
	Token          string
	TokenSecret    string
}
