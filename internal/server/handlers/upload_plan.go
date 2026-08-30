package handlers

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/syncsystem-net/back-me-up/internal/archive"
	"github.com/syncsystem-net/back-me-up/internal/database"
	"github.com/syncsystem-net/back-me-up/internal/limits"
)

// planAccount is one upload target as the planner sees it.
type planAccount struct {
	ID       int64  `json:"id"`
	Provider string `json:"provider"`
	Email    string `json:"email"`
	Tier     string `json:"tier"`
}

// planVolume is one archive that will be produced and uploaded.
type planVolume struct {
	Name string `json:"name"`
	// EstimatedBytes is the raw size of the files assigned to this volume.
	// Compression only shrinks it, so it is an upper bound on what is uploaded
	// and the number the quota and budget checks are made against.
	EstimatedBytes int64 `json:"estimated_bytes"`
}

// planGroup is a set of accounts that share an effective size threshold, and so
// share one set of archives.
//
// Grouping is by threshold rather than by provider name because that is what
// actually decides the archives: two free 4shared accounts need identical
// volumes and must not cause the source to be compressed twice, while MEGA and
// an uncapped paid account both take the whole thing. Provider is what the user
// thinks in, threshold is what the zip writer needs, and these coincide in every
// case the user will see.
type planGroup struct {
	ThresholdBytes int64         `json:"threshold_bytes"`
	Accounts       []planAccount `json:"accounts"`
	Volumes        []planVolume  `json:"volumes"`
	// ByteParts is true when these archives are a raw cut of one zip rather than
	// standalone volumes. The UI has to say so: the user cannot open one of these
	// on its own, and a download of a single part is not a usable file.
	ByteParts bool `json:"byte_parts"`

	// plan is the archive-level detail (which paths land in which volume). It is
	// not serialized: the browser has no use for it and a large source would make
	// the response enormous.
	plan *archive.Plan
}

// uploadPlan is the whole decision for one backup: what will be produced, for
// whom, and anything that stands in the way.
type uploadPlan struct {
	Groups []*planGroup `json:"groups"`
	// TotalSourceBytes is the raw size of the source directory.
	TotalSourceBytes int64 `json:"total_source_bytes"`
	// Blockers stop the upload entirely; Warnings do not.
	Blockers []planIssue `json:"blockers"`
	Warnings []planIssue `json:"warnings"`
}

// planIssue is one reason an upload cannot proceed, or one thing the user should
// know before it does. Reason is a stable machine-readable tag; Message is what
// a person reads.
type planIssue struct {
	Reason    string `json:"reason"`
	Message   string `json:"message"`
	AccountID int64  `json:"account_id,omitempty"`
	Provider  string `json:"provider,omitempty"`
	Email     string `json:"email,omitempty"`
}

// OK reports whether the upload may proceed.
func (p *uploadPlan) OK() bool { return len(p.Blockers) == 0 }

// buildUploadPlan works out how sourcePath must be archived for each selected
// account, and whether it can be uploaded at all.
//
// Everything it does is metadata-only: directory sizes are read, nothing is
// compressed and no file is opened. That is the point — an upload that cannot
// succeed is refused in seconds rather than after several gigabytes of zipping,
// which is the whole reason this runs before the archive is built rather than
// after.
func (h *Handlers) buildUploadPlan(sourcePath string, accountIDs []int64) (*uploadPlan, error) {
	limitSet := database.GetProviderLimits(h.db)

	// Preserve the caller's account order inside each group, but order the groups
	// themselves by threshold so the plan is stable between the preview and the
	// upload that follows it.
	byThreshold := map[int64]*planGroup{}
	var thresholds []int64

	for _, id := range accountIDs {
		acct, err := database.GetDBAccountByID(h.db, id)
		if err != nil {
			return nil, fmt.Errorf("selected account not found: %w", err)
		}
		threshold := limitSet.For(acct.Provider, limits.NormalizeTier(acct.Tier)).MaxFileBytes

		g, ok := byThreshold[threshold]
		if !ok {
			g = &planGroup{ThresholdBytes: threshold}
			byThreshold[threshold] = g
			thresholds = append(thresholds, threshold)
		}
		g.Accounts = append(g.Accounts, planAccount{
			ID: acct.ID, Provider: acct.Provider, Email: acct.Email, Tier: acct.Tier,
		})
	}

	sort.Slice(thresholds, func(i, j int) bool { return thresholds[i] < thresholds[j] })

	out := &uploadPlan{}
	for _, t := range thresholds {
		g := byThreshold[t]
		plan, err := archive.PlanVolumes(sourcePath, t, h.splitMethod)
		if err != nil {
			var tooLarge *archive.FileTooLargeError
			if errors.As(err, &tooLarge) {
				// The one failure with a specific, actionable cause. Naming the file
				// and both sizes is the entire value here: there is nothing the app
				// can do, and everything the user can.
				out.Blockers = append(out.Blockers, planIssue{
					Reason:  "file_too_large",
					Message: tooLarge.Error(),
				})
				continue
			}
			// Anything else is a source the planner could not read. The planner
			// refuses rather than skipping (skipping would omit the directory from
			// the archives without saying so), and the path is already named in the
			// message — so this belongs in the preview's blocker list beside the
			// oversized-file case, not as a bare 500 the user cannot act on.
			out.Blockers = append(out.Blockers, planIssue{
				Reason:  "unreadable",
				Message: err.Error(),
			})
			continue
		}

		g.plan = plan
		g.ByteParts = plan.ByteParts()
		names := plan.Names(sourcePath)
		for i, v := range plan.Volumes {
			g.Volumes = append(g.Volumes, planVolume{Name: names[i], EstimatedBytes: v.Bytes})
		}
		if plan.TotalBytes > out.TotalSourceBytes {
			out.TotalSourceBytes = plan.TotalBytes
		}
		out.Groups = append(out.Groups, g)
	}

	out.Groups = mergeWholeGroups(out.Groups)

	for _, g := range out.Groups {
		if !g.ByteParts {
			continue
		}
		// Not a blocker: this is the arrangement that makes the backup possible at
		// all. But the user must know before committing that these files are not
		// individually openable, because that changes how they would restore.
		var who []string
		for _, a := range g.Accounts {
			who = append(who, a.Provider+" "+a.Email)
		}
		out.Warnings = append(out.Warnings, planIssue{
			Reason: "byte_parts",
			Message: fmt.Sprintf("%s: this backup contains a file larger than the limit, so it is split into %d numbered parts instead of standalone archives. The parts must all be downloaded and rejoined before anything can be extracted (open the .001 part in 7-Zip, or join them with copy /b on Windows).",
				strings.Join(who, ", "), len(g.Volumes)),
		})
	}

	if !out.OK() {
		// A blocked plan's remaining checks would only add noise about an upload
		// that is not going to happen.
		return out, nil
	}

	h.checkPlanQuota(out)
	h.checkPlanBudget(out, limitSet)
	return out, nil
}

// mergeWholeGroups collapses every group that needs no split into a single
// group, because they all produce the identical whole-directory archive.
//
// Grouping starts from the threshold, and different thresholds are different
// groups even when the source fits comfortably under all of them — which is the
// ordinary case: MEGA (no limit) and a free 4shared account (1.9 GB) receiving a
// 200 MB backup. Left alone, that compresses the source twice and records the
// same archive name as two zip rows, so the record's tree lists it twice, search
// returns it twice, and Auto-Sync inherits two same-named rows to disambiguate.
// A split group is never merged: its volumes really are threshold-specific.
//
// The merged group keeps the strictest threshold among its members, so the
// post-write size check still measures against the tightest limit that applies.
func mergeWholeGroups(groups []*planGroup) []*planGroup {
	var merged *planGroup
	out := make([]*planGroup, 0, len(groups))

	for _, g := range groups {
		// A byte-parts plan is never merged even when it produces a single part:
		// its archives are named .001 and must be rejoined, so they are not the
		// same deliverable as a whole-file group's plain .zip.
		if g.plan == nil || g.plan.Split() || g.plan.ByteParts() {
			out = append(out, g)
			continue
		}
		if merged == nil {
			merged = g
			out = append(out, g)
			continue
		}
		merged.Accounts = append(merged.Accounts, g.Accounts...)
		// 0 means "no limit", so it never wins the strictest-threshold contest.
		if g.ThresholdBytes > 0 && (merged.ThresholdBytes == 0 || g.ThresholdBytes < merged.ThresholdBytes) {
			merged.ThresholdBytes = g.ThresholdBytes
		}
	}
	return out
}

// checkPlanQuota blocks the upload if an account lacks the free space for the
// archives it would receive. Accounts whose quota has never been polled (0) are
// skipped rather than blocking the user on a figure we do not have.
func (h *Handlers) checkPlanQuota(p *uploadPlan) {
	const bytesPerGB = 1 << 30
	for _, g := range p.Groups {
		var groupBytes int64
		for _, v := range g.Volumes {
			groupBytes += v.EstimatedBytes
		}
		for _, a := range g.Accounts {
			acct, err := database.GetDBAccountByID(h.db, a.ID)
			if err != nil || acct.QuotaTotalGB <= 0 {
				continue
			}
			available := int64((acct.QuotaTotalGB - acct.QuotaUsedGB) * bytesPerGB)
			if groupBytes > available {
				p.Blockers = append(p.Blockers, planIssue{
					Reason:    "quota",
					AccountID: a.ID,
					Provider:  a.Provider,
					Email:     a.Email,
					Message: fmt.Sprintf("%s account %s does not have enough space: this backup needs %s but only %s is free",
						a.Provider, a.Email, archive.HumanBytes(groupBytes), archive.HumanBytes(available)),
				})
			}
		}
	}
}

// checkPlanBudget reports what the transfer budget will do to this upload.
//
// A budget shortage is a warning, not a blocker: the jobs are created and the
// worker holds them until the window rolls over, which is what the user asked
// for. The one case that *is* a blocker is a volume larger than an account's
// entire budget — no amount of waiting makes room for it, so queueing it would
// be queueing a job that can only ever fail.
func (h *Handlers) checkPlanBudget(p *uploadPlan, limitSet limits.Set) {
	for _, g := range p.Groups {
		for _, a := range g.Accounts {
			lim := limitSet.For(a.Provider, limits.NormalizeTier(a.Tier))
			if !lim.Budgeted() {
				continue
			}

			var largest, total int64
			for _, v := range g.Volumes {
				total += v.EstimatedBytes
				if v.EstimatedBytes > largest {
					largest = v.EstimatedBytes
				}
			}

			if largest > lim.TransferBytes {
				p.Blockers = append(p.Blockers, planIssue{
					Reason:    "budget_impossible",
					AccountID: a.ID,
					Provider:  a.Provider,
					Email:     a.Email,
					Message: fmt.Sprintf("%s account %s has a %s per %dh transfer budget, but one archive is %s. Lower this provider's split threshold or set its transfer budget to unlimited in Settings.",
						a.Provider, a.Email, archive.HumanBytes(lim.TransferBytes), lim.WindowHours, archive.HumanBytes(largest)),
				})
				continue
			}

			used, err := database.TransferUsedSince(h.db, a.ID, lim.WindowHours)
			if err != nil {
				continue
			}
			if remaining := lim.TransferBytes - used; total > remaining {
				p.Warnings = append(p.Warnings, planIssue{
					Reason:    "budget_hold",
					AccountID: a.ID,
					Provider:  a.Provider,
					Email:     a.Email,
					Message: fmt.Sprintf("%s account %s has %s left of its %s per %dh transfer budget; this backup is %s, so some archives will wait for the window to roll over before uploading",
						a.Provider, a.Email, archive.HumanBytes(maxInt64(remaining, 0)),
						archive.HumanBytes(lim.TransferBytes), lim.WindowHours, archive.HumanBytes(total)),
				})
			}
		}
	}
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
