package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/syncsystem-net/back-me-up/internal/autosync"
)

// Auto-Sync is exposed as start/poll rather than one long request. A crawl of
// every account is network-bound and can run for minutes, which would hold an
// HTTP connection open past any sane timeout and stall the page's 2-second
// refresh; instead each of these returns immediately and the UI polls the
// snapshot.

// AutoSyncPreviewHandler starts the read-only crawl. Route: POST
// /api/autosync/preview.
func AutoSyncPreviewHandler(m *autosync.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if m == nil {
			jsonError(w, "auto-sync is unavailable", http.StatusServiceUnavailable)
			return
		}
		if err := m.StartPreview(); err != nil {
			jsonError(w, err.Error(), http.StatusConflict)
			return
		}
		writeRun(w, m)
	}
}

// AutoSyncApplyHandler writes the changes the preview proposed. It fails unless
// a preview is ready, so nothing can be applied that the user was not shown.
// Route: POST /api/autosync/apply.
func AutoSyncApplyHandler(m *autosync.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if m == nil {
			jsonError(w, "auto-sync is unavailable", http.StatusServiceUnavailable)
			return
		}
		if err := m.StartApply(); err != nil {
			jsonError(w, err.Error(), http.StatusConflict)
			return
		}
		writeRun(w, m)
	}
}

// AutoSyncStatusHandler returns the current run. Route: GET /api/autosync.
func AutoSyncStatusHandler(m *autosync.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if m == nil {
			jsonError(w, "auto-sync is unavailable", http.StatusServiceUnavailable)
			return
		}
		writeRun(w, m)
	}
}

// AutoSyncCancelHandler stops an in-flight run. Route: POST
// /api/autosync/cancel.
func AutoSyncCancelHandler(m *autosync.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if m == nil {
			jsonError(w, "auto-sync is unavailable", http.StatusServiceUnavailable)
			return
		}
		m.Cancel()
		writeRun(w, m)
	}
}

func writeRun(w http.ResponseWriter, m *autosync.Manager) {
	run := m.Snapshot()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(run)
}
