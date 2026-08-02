package fourshared

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/syncsystem-net/back-me-up/internal/provider"
)

// The two statuses callers branch on must be recognisable with errors.Is, since
// message sniffing is exactly what the sentinels exist to replace. Everything
// else stays an opaque error — notably 403, which carries the "already exists"
// upload rejection and must not be mistaken for either.
func TestStatusErrorClassification(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		sentinel error
	}{
		{"404 is not found", http.StatusNotFound, `{"code":"404.0400","message":"Resource not found"}`, provider.ErrNotFound},
		{"401 is expired auth", http.StatusUnauthorized, `{"code":"401.0301","message":"token expired, rejected or does not exist"}`, provider.ErrAuthExpired},
		{"403 is neither", http.StatusForbidden, `{"code":"403.0201","message":"already exists"}`, nil},
		{"500 is neither", http.StatusInternalServerError, `oops`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := statusError("delete", tt.status, []byte(tt.body))
			if err == nil {
				t.Fatal("statusError returned nil")
			}
			for _, s := range []error{provider.ErrNotFound, provider.ErrAuthExpired} {
				want := tt.sentinel != nil && errors.Is(tt.sentinel, s)
				if got := errors.Is(err, s); got != want {
					t.Fatalf("errors.Is(err, %v) = %v, want %v (err: %v)", s, got, want, err)
				}
			}
			// The raw response is still carried for the log line.
			if !strings.Contains(err.Error(), tt.body) {
				t.Fatalf("error %q dropped the response body", err)
			}
		})
	}
}
