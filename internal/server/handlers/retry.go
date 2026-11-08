package handlers

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"

	"github.com/syncsystem-net/back-me-up/internal/database"
)

// RetryJob puts a failed upload back in the queue, reusing the temporary archive
// the failure kept on disk. Route: POST /api/jobs/{id}/retry.
//
// The point of retrying rather than uploading the directory again is that
// nothing new is created: the job keeps its zip_id, so the record's tree stays
// one node per archive, and no second zip is built beside the source. Every
// refusal below is a case where that promise could not be kept, and each one
// says why, because "409" alone tells the user nothing about what to do next.
func (h *Handlers) RetryJob(w http.ResponseWriter, r *http.Request) {
	// A retried job is claimed by a worker that needs the account's credential.
	// Queuing it while locked would look like success and then sit there.
	if h.refuseWhenLocked(w) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		jsonError(w, "invalid job id", http.StatusBadRequest)
		return
	}

	job, err := database.GetJob(h.db, id)
	if errors.Is(err, sql.ErrNoRows) {
		jsonError(w, "job not found", http.StatusNotFound)
		return
	}
	if err != nil {
		slog.Error("retry: loading job failed", "job", id, "error", err)
		jsonError(w, "failed to load job", http.StatusInternalServerError)
		return
	}
	if job.Status != "failed" {
		jsonError(w, fmt.Sprintf("only a failed job can be retried; job #%d is %s", job.ID, job.Status), http.StatusConflict)
		return
	}

	// A later job for the same archive on the same account has already taken
	// this one's place. Retrying would upload a duplicate or race that upload.
	later, err := database.LaterJobForSameArchive(h.db, job)
	if err != nil {
		slog.Error("retry: looking up later job failed", "job", id, "error", err)
		jsonError(w, "failed to check for a later upload of this archive", http.StatusInternalServerError)
		return
	}
	if later != nil {
		jsonError(w, supersededMessage(job, later), http.StatusConflict)
		return
	}

	// The kept archive must still be the one this job was queued with. Its size
	// is the check available without re-hashing gigabytes: a temp zip rebuilt
	// from a directory that has since changed will almost always differ, and
	// uploading it under this job would record a copy of something the user
	// never queued.
	if job.ZipPath == "" {
		jsonError(w, "this job has no temporary archive to retry from; upload the directory again", http.StatusConflict)
		return
	}
	info, err := os.Stat(job.ZipPath)
	if err != nil {
		jsonError(w, fmt.Sprintf("the temporary archive %s is gone; upload the directory again", job.ZipPath), http.StatusConflict)
		return
	}
	if info.Size() != job.TotalBytes {
		jsonError(w, fmt.Sprintf(
			"the temporary archive %s has changed since this job was queued (%d bytes, expected %d); upload the directory again",
			job.ZipPath, info.Size(), job.TotalBytes), http.StatusConflict)
		return
	}

	queued, err := database.RetryFailedJob(h.db, id)
	if err != nil {
		slog.Error("retry: re-queuing job failed", "job", id, "error", err)
		jsonError(w, "failed to re-queue job", http.StatusInternalServerError)
		return
	}
	if !queued {
		// Someone else retried it between the status check above and now — the
		// guarded UPDATE is what keeps that from queuing it twice.
		jsonError(w, "this job is no longer failed; it may already have been retried", http.StatusConflict)
		return
	}
	if err := database.InsertJobLog(h.db, id, "info", "retry requested from the Jobs view"); err != nil {
		// The job is queued; a missing note must not make that look like a failure.
		slog.Warn("retry: writing job log failed", "job", id, "error", err)
	}

	job, err = database.GetJob(h.db, id)
	if err != nil {
		slog.Error("retry: reloading job failed", "job", id, "error", err)
		jsonError(w, "job re-queued but could not be reloaded", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(job)
}

// supersededMessage names the job that replaced a failed one and what state it
// is in, because the right next step differs: nothing (it is uploaded), wait (it
// is running), or retry that one instead (it failed too).
func supersededMessage(job, later *database.Job) string {
	switch later.Status {
	case "complete":
		return fmt.Sprintf("%s is already uploaded to this account by job #%d", archiveName(job), later.ID)
	case "pending", "in_progress":
		return fmt.Sprintf("%s is already being uploaded to this account by job #%d", archiveName(job), later.ID)
	default:
		return fmt.Sprintf("job #%d replaced this one for %s and also failed; retry job #%d instead", later.ID, archiveName(job), later.ID)
	}
}
