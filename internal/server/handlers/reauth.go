package handlers

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/syncsystem-net/back-me-up/internal/accounts"
	"github.com/syncsystem-net/back-me-up/internal/reauth"
)

// Re-authorization is start/poll rather than one long request: the flow waits on
// a browser round trip that can take a minute, and the table keeps refreshing
// underneath it. Same shape as the Auto-Sync routes.

type reauthStartRequest struct {
	Provider string `json:"provider"`
	Email    string `json:"email"`
	IsMain   bool   `json:"is_main"`
}

// ReauthStartHandler begins re-authorizing one account. Route:
// POST /api/reauth/start.
func ReauthStartHandler(m *reauth.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req reauthStartRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "invalid request body", http.StatusBadRequest)
			return
		}
		if req.Provider == "" || (req.Email == "" && !req.IsMain) {
			jsonError(w, "provider and email are required", http.StatusBadRequest)
			return
		}

		status, err := m.Start(req.Provider, req.Email, req.IsMain)
		if err != nil {
			// Locked credentials are a different kind of "no" from a bad request:
			// nothing about the request would make it work.
			if errors.Is(err, accounts.ErrLocked) {
				jsonError(w, err.Error(), http.StatusServiceUnavailable)
				return
			}
			jsonError(w, err.Error(), http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(status)
	}
}

// ReauthStatusHandler reports the in-flight run. Route: GET /api/reauth.
func ReauthStatusHandler(m *reauth.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(m.Status())
	}
}

// ReauthCancelHandler stops an in-flight run and releases the callback port.
// Route: POST /api/reauth/cancel.
func ReauthCancelHandler(m *reauth.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m.Cancel()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(m.Status())
	}
}
