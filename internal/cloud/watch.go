package cloud

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/syncsystem-net/back-me-up/internal/provider"
)

// An expired OAuth token can surface from any provider call, not just Login: a
// token that was valid when a job started can be rejected by the delete request
// that ends it. Rather than teach every call site to notice, every provider this
// package hands out is wrapped, and one hook records the account as needing
// re-authorization.
//
// The hook is process-global, installed once from main.go, matching the existing
// ratelimit.Configure pattern. Making it a parameter instead would thread a
// credentials manager through cloud.Connect into the worker, the quota poller,
// Auto-Sync and every handler — for a side effect none of them care about.

type authHookFunc func(providerName, email string, isMain bool)

var (
	authHookMu       sync.RWMutex
	authExpiredHook  authHookFunc
	authRestoredHook authHookFunc
)

// OnAuthExpired installs the callback invoked when a provider rejects an
// account's credentials as expired. Installing nil disables it.
func OnAuthExpired(fn func(providerName, email string, isMain bool)) {
	authHookMu.Lock()
	defer authHookMu.Unlock()
	authExpiredHook = fn
}

// OnAuthRestored installs the callback invoked when an account authenticates
// successfully. It is the counterpart to OnAuthExpired: without it the
// needs-re-authorization flag is a one-way door, and an account that started
// working again would keep its warning until someone re-authorized it
// unnecessarily.
func OnAuthRestored(fn func(providerName, email string, isMain bool)) {
	authHookMu.Lock()
	defer authHookMu.Unlock()
	authRestoredHook = fn
}

func notify(hook authHookFunc, providerName, email string, isMain bool) {
	if hook != nil {
		hook(providerName, email, isMain)
	}
}

func expiredHook() authHookFunc {
	authHookMu.RLock()
	defer authHookMu.RUnlock()
	return authExpiredHook
}

func restoredHook() authHookFunc {
	authHookMu.RLock()
	defer authHookMu.RUnlock()
	return authRestoredHook
}

// watched wraps a provider so every operation's error is inspected for
// provider.ErrAuthExpired. It changes no behaviour: the error is returned
// unchanged, and the hook is a side effect.
type watched struct {
	provider.Provider
	providerName string
	email        string
	isMain       bool

	// Each hook fires at most once per connection, so a run of failing calls (or a
	// long-lived provider making many successful ones) writes the flag once
	// rather than on every call.
	expiredOnce  sync.Once
	restoredOnce sync.Once
}

// watch returns p wrapped, or p unchanged when it is nil.
func watch(p provider.Provider, providerName, email string, isMain bool) provider.Provider {
	if p == nil {
		return nil
	}
	return &watched{Provider: p, providerName: providerName, email: email, isMain: isMain}
}

func (w *watched) note(err error) error {
	if err != nil && errors.Is(err, provider.ErrAuthExpired) {
		w.expiredOnce.Do(func() { notify(expiredHook(), w.providerName, w.email, w.isMain) })
	}
	return err
}

func (w *watched) Login(ctx context.Context, email, password string) error {
	err := w.note(w.Provider.Login(ctx, email, password))
	if err == nil {
		// Success is the only reliable signal that whatever was wrong is no longer
		// wrong. Reported from Login specifically: it is the one call whose success
		// means the credentials themselves are good.
		w.restoredOnce.Do(func() { notify(restoredHook(), w.providerName, w.email, w.isMain) })
	}
	return err
}

func (w *watched) Upload(ctx context.Context, localPath, remoteName string, onProgress func(provider.Progress)) (string, error) {
	ref, err := w.Provider.Upload(ctx, localPath, remoteName, onProgress)
	return ref, w.note(err)
}

func (w *watched) Download(ctx context.Context, remoteRef string, out io.Writer) error {
	return w.note(w.Provider.Download(ctx, remoteRef, out))
}

func (w *watched) List(ctx context.Context) ([]provider.RemoteFile, error) {
	files, err := w.Provider.List(ctx)
	return files, w.note(err)
}

func (w *watched) ReadRange(ctx context.Context, remoteRef string, p []byte, off int64) (int, error) {
	n, err := w.Provider.ReadRange(ctx, remoteRef, p, off)
	return n, w.note(err)
}

func (w *watched) FindByName(ctx context.Context, name string) (string, bool, error) {
	ref, found, err := w.Provider.FindByName(ctx, name)
	return ref, found, w.note(err)
}

func (w *watched) Delete(ctx context.Context, remoteRef string) error {
	return w.note(w.Provider.Delete(ctx, remoteRef))
}

func (w *watched) GetQuota(ctx context.Context) (int64, int64, error) {
	total, used, err := w.Provider.GetQuota(ctx)
	return total, used, w.note(err)
}
