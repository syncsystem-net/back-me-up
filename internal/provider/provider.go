// Package provider defines the abstraction every cloud-storage backend
// implements. Concrete backends live in subpackages (mega, fourshared) and are
// wired together by the registry subpackage, so adding a new provider is a
// focused change: implement Provider, register it in the registry.
package provider

import (
	"context"
	"io"
)

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

// Config carries provider-agnostic settings the registry passes to every
// backend at construction time.
type Config struct {
	// ChunkSizeBytes is the preferred upload chunk size. Providers that control
	// their own chunk boundaries (e.g. MEGA's protocol) may ignore it.
	ChunkSizeBytes int64
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
