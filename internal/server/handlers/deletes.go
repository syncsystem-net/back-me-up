package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/syncsystem-net/back-me-up/internal/database"
	"github.com/syncsystem-net/back-me-up/internal/provider"
)

// Deleting is best-effort by design.
//
// Two things routinely go wrong when removing a record's cloud copies, and
// neither should be able to pin a record to the table forever:
//
//   - The remote file is already gone. That is the state a delete is trying to
//     reach, so it counts as success. It is normal, not exotic: Auto-Sync
//     deliberately reports a vanished remote copy without touching the record,
//     so the database is expected to hold rows pointing at absent archives.
//   - One account is unreachable (an expired 4shared OAuth token, most often).
//     Aborting there used to leave the reachable copies undeleted too, with the
//     user's only escape being "delete the record but not the files", which
//     orphans everything.
//
// So every job is attempted, and what failed is reported per archive. On a
// partial failure the record is KEPT and the response is 409 with the list, and
// the user can then re-issue the delete with {"force":true} to drop the local
// record knowing exactly what is left behind. Retrying instead of forcing is
// also safe: the copies deleted on the first pass now report "not found", which
// is success, so a retry converges.

// deleteFailure describes one archive that could not be removed from its
// provider. It is what the delete modal lists, so every field is something the
// user needs in order to know what is left behind and where.
type deleteFailure struct {
	JobID    int64  `json:"job_id"`
	Provider string `json:"provider"`
	Email    string `json:"email"`
	Archive  string `json:"archive"`
	// Reason is a machine-readable class: "credentials" (could not connect /
	// the account was rejected) or "provider" (the backend refused the delete).
	Reason string `json:"reason"`
	// Message is the human-readable explanation shown in the UI.
	Message string `json:"message"`
}

// deleteFilesResult is the outcome of attempting every job of a record.
type deleteFilesResult struct {
	deleted  int
	failures []deleteFailure
}

const (
	reasonCredentials = "credentials"
	reasonProvider    = "provider"
)

// deleteRemoteForJob removes one job's uploaded file from its provider. A job
// that never uploaded (no remote_path) and a file that is already gone are both
// reported as success — in each case there is nothing left in the cloud, which
// is the whole point of the call.
func (h *Handlers) deleteRemoteForJob(ctx context.Context, job *database.Job) *deleteFailure {
	if job.RemotePath == "" {
		return nil
	}
	p, err := h.connect(ctx, job.Provider, job.Email)
	if err != nil {
		slog.Error("delete: connect failed", "job", job.ID, "provider", job.Provider, "email", job.Email, "error", err)
		return &deleteFailure{
			JobID:    job.ID,
			Provider: job.Provider,
			Email:    job.Email,
			Archive:  archiveName(job),
			Reason:   reasonCredentials,
			Message:  connectFailureMessage(job.Provider, job.Email, err),
		}
	}
	err = p.Delete(ctx, job.RemotePath)
	if err == nil {
		return nil
	}
	if errors.Is(err, provider.ErrNotFound) {
		// Already absent — the desired end state holds, so this is a success.
		slog.Info("delete: remote file already gone, treating as deleted",
			"job", job.ID, "provider", job.Provider, "email", job.Email, "archive", archiveName(job))
		return nil
	}
	slog.Error("delete: remote delete failed", "job", job.ID, "provider", job.Provider, "email", job.Email, "error", err)
	reason, msg := reasonCredentials, ""
	if errors.Is(err, provider.ErrAuthExpired) {
		msg = credentialsMessage(job.Provider, job.Email)
	} else {
		reason = reasonProvider
		msg = fmt.Sprintf("%s refused to delete this file. It is still on the account. (%v)", providerLabel(job.Provider), err)
	}
	return &deleteFailure{
		JobID:    job.ID,
		Provider: job.Provider,
		Email:    job.Email,
		Archive:  archiveName(job),
		Reason:   reason,
		Message:  msg,
	}
}

// deleteRemoteFiles attempts every job, never stopping at the first failure, so
// the archives that can be removed are removed even when a sibling account is
// unreachable.
func (h *Handlers) deleteRemoteFiles(ctx context.Context, jobs []*database.Job) deleteFilesResult {
	res := deleteFilesResult{}
	for _, j := range jobs {
		if j.RemotePath == "" {
			continue
		}
		if f := h.deleteRemoteForJob(ctx, j); f != nil {
			res.failures = append(res.failures, *f)
			continue
		}
		res.deleted++
	}
	return res
}

// DeleteJob removes a job's file from its provider and deletes the job record.
// It requires a JSON body {"confirm":"DELETE"} (also enforced in the UI). Only
// that provider's copy is affected — the backup record, its zips, and any
// sibling provider's job remain. A file that is already gone on the provider
// counts as deleted, so the job record still goes away. Route: DELETE
// /api/jobs/{id}.
func (h *Handlers) DeleteJob(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		jsonError(w, "invalid job id", http.StatusBadRequest)
		return
	}
	var body struct {
		Confirm string `json:"confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if body.Confirm != "DELETE" {
		jsonError(w, `confirmation must be exactly "DELETE"`, http.StatusBadRequest)
		return
	}

	job, err := database.GetJob(h.db, id)
	if err != nil {
		jsonError(w, "job not found", http.StatusNotFound)
		return
	}

	// Remove the remote file first. If it genuinely fails, keep the record so the
	// user can retry rather than silently orphaning a file in the cloud.
	if f := h.deleteRemoteForJob(r.Context(), job); f != nil {
		writeDeleteFailures(w, 0, []deleteFailure{*f})
		return
	}

	if err := database.DeleteJob(h.db, id); err != nil {
		slog.Error("delete: removing job record failed", "job", id, "error", err)
		jsonError(w, "deleted from provider but failed to remove record", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DeleteBackup removes a whole backup record. With {"delete_files":true} it also
// deletes every uploaded zip from its provider(s) first (requiring
// {"confirm":"DELETE"}); the record's zips, jobs, and logs cascade away. With
// delete_files false only the local record is removed and remote files are left
// in place.
//
// Every job is attempted. If some archives could not be deleted the record is
// kept and the response is 409 with the per-archive failure list, so the user
// can see what is left behind; re-issuing the request with {"force":true} then
// removes the local record only, touching no provider. Route: DELETE
// /api/backups/{id}.
func (h *Handlers) DeleteBackup(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		jsonError(w, "invalid backup id", http.StatusBadRequest)
		return
	}
	var body struct {
		Confirm     string `json:"confirm"`
		DeleteFiles bool   `json:"delete_files"`
		// Force removes the local record regardless of what is still on the
		// providers. It is the second step the UI offers after a partial failure,
		// so no remote delete is retried here — the previous request already
		// attempted them all, and the user has been told what remains.
		Force bool `json:"force"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if body.DeleteFiles && body.Confirm != "DELETE" {
		jsonError(w, `confirmation must be exactly "DELETE"`, http.StatusBadRequest)
		return
	}

	if body.DeleteFiles && !body.Force {
		jobs, err := database.ListJobsByBackup(h.db, id)
		if err != nil {
			slog.Error("delete backup: listing jobs", "backup", id, "error", err)
			jsonError(w, "failed to load backup jobs", http.StatusInternalServerError)
			return
		}
		res := h.deleteRemoteFiles(r.Context(), jobs)
		if len(res.failures) > 0 {
			slog.Warn("delete backup: partial failure, keeping record",
				"backup", id, "deleted", res.deleted, "failed", len(res.failures))
			writeDeleteFailures(w, res.deleted, res.failures)
			return
		}
	}
	if body.Force {
		slog.Warn("delete backup: forced, removing record without deleting remote files", "backup", id)
	}

	if err := database.DeleteBackupRecord(h.db, id); err != nil {
		slog.Error("delete backup: removing record failed", "backup", id, "error", err)
		jsonError(w, "failed to remove backup record", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeDeleteFailures answers a delete that partly (or wholly) failed. The
// status is 409 rather than 502 because the operation is not simply "broken":
// some files may well be gone, and the client has to be able to tell "nothing
// happened" from "here is exactly what did not" in order to offer the force
// step. The local record has NOT been removed.
func writeDeleteFailures(w http.ResponseWriter, deleted int, failures []deleteFailure) {
	noun := "file"
	if len(failures) != 1 {
		noun = "files"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	json.NewEncoder(w).Encode(map[string]any{
		"error":    fmt.Sprintf("%d %s could not be deleted from their provider.", len(failures), noun),
		"deleted":  deleted,
		"failures": failures,
	})
}

// archiveName is the zip's name as the user knows it, falling back to the job id
// for a job whose remote name was never recorded.
func archiveName(job *database.Job) string {
	if job.RemoteName != "" {
		return job.RemoteName
	}
	return fmt.Sprintf("job #%d", job.ID)
}

func providerLabel(name string) string {
	switch name {
	case "mega":
		return "MEGA"
	case "fourshared":
		return "4shared"
	}
	return name
}

// connectFailureMessage explains why an account could not be reached, in terms
// of what the user has to do about it. An expired authorization is called out
// by name because the remedy (re-authorize the account) is specific and a raw
// "401.0301" tells the user nothing.
func connectFailureMessage(providerName, email string, err error) string {
	if errors.Is(err, provider.ErrAuthExpired) {
		return credentialsMessage(providerName, email)
	}
	return fmt.Sprintf("Could not connect to %s account %s. (%v)", providerLabel(providerName), email, err)
}

func credentialsMessage(providerName, email string) string {
	if providerName == "fourshared" {
		return fmt.Sprintf("The 4shared authorization for %s has expired or was revoked. "+
			"Re-authorize it with `go run ./cmd/fourshared-auth -account <n>` and update the OAuth token in .env, then try again.", email)
	}
	return fmt.Sprintf("%s rejected the stored credentials for %s. Check the account's password in .env, then try again.",
		providerLabel(providerName), email)
}
