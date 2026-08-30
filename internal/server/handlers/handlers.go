package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/syncsystem-net/back-me-up/internal/accounts"
	"github.com/syncsystem-net/back-me-up/internal/archive"
	"github.com/syncsystem-net/back-me-up/internal/cloud"
	"github.com/syncsystem-net/back-me-up/internal/credentials"
	"github.com/syncsystem-net/back-me-up/internal/database"
	"github.com/syncsystem-net/back-me-up/internal/keyring"
	"github.com/syncsystem-net/back-me-up/internal/limits"
	"github.com/syncsystem-net/back-me-up/internal/provider"
	"github.com/syncsystem-net/back-me-up/internal/quota"
	"github.com/syncsystem-net/back-me-up/internal/scanner"
)

// UI carries the presentation settings the page needs at render time. It is a
// struct rather than more positional arguments to New so that adding a knob
// later does not ripple through every call site.
type UI struct {
	// PollSeconds is the idle refresh cadence; ActivePollSeconds is used while a
	// job is running. Both come from the ui block in config.yml.
	PollSeconds       int
	ActivePollSeconds int
}

type Handlers struct {
	db *sql.DB
	// accounts is the running credential set. It is the same pointer for the life
	// of the process and is refreshed in place by the credentials manager, so a
	// re-authorized token takes effect here without a restart.
	accounts  *accounts.AccountStore
	tmpl      *template.Template
	chunkSize int64
	// scanMaxDepth caps the recursive directory walk that records a zip's tree
	// (config scan.max_depth, default 3).
	scanMaxDepth int
	// splitMethod is how an oversized backup is divided (config
	// archive.split_method). The zero value normalizes to auto, so a Handlers
	// built without it behaves as configured rather than refusing to split.
	splitMethod archive.SplitMethod
	ui          UI

	// connect resolves a logged-in provider for one account. It wraps
	// cloud.Connect in production and is a field so tests can drive the delete
	// paths — whose whole point is how they behave when a backend fails — against
	// a stub backend, with no network and no real credentials.
	connect func(ctx context.Context, providerName, email string) (provider.Provider, error)
}

func New(db *sql.DB, creds *credentials.Manager, chunkSize int64, scanMaxDepth int, splitMethod string, ui UI) *Handlers {
	tmplPath := filepath.Join("web", "templates", "*.html")
	tmpl, err := template.ParseGlob(tmplPath)
	if err != nil {
		slog.Error("failed to parse templates", "error", err)
		tmpl = template.New("")
	}

	h := &Handlers{
		db:           db,
		accounts:     creds.Store(),
		tmpl:         tmpl,
		chunkSize:    chunkSize,
		scanMaxDepth: scanMaxDepth,
		splitMethod:  archive.NormalizeMethod(splitMethod),
		ui:           ui,
	}
	h.connect = func(ctx context.Context, providerName, email string) (provider.Provider, error) {
		return cloud.Connect(ctx, h.accounts, providerName, email, h.chunkSize)
	}
	return h
}

func (h *Handlers) Home(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	// Poll cadences reach the page as template data rather than through an extra
	// endpoint: they are needed before the first fetch, and a request for them
	// would itself be a poll.
	data := map[string]any{
		"Title":        "BackMeUp",
		"PollMS":       h.ui.PollSeconds * 1000,
		"ActivePollMS": h.ui.ActivePollSeconds * 1000,
	}
	if err := h.tmpl.ExecuteTemplate(w, "index.html", data); err != nil {
		slog.Error("template error", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

func (h *Handlers) Health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"ok"}`))
}

func GetAccountsHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accts, err := database.ListDBAccounts(db)
		if err != nil {
			slog.Error("listing accounts", "error", err)
			jsonError(w, "failed to list accounts", http.StatusInternalServerError)
			return
		}
		if accts == nil {
			accts = []*database.DBAccount{}
		}
		enrichAccountLimits(db, accts)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(accts)
	}
}

// enrichAccountLimits fills in what each account's tier currently means: the
// split threshold in force, and how much of its transfer window is spent.
//
// These are derived per request rather than stored on the row, so they cannot
// drift from the settings the way a cached copy would. A usage lookup failure
// leaves the figure at zero and is logged — a missing number must not cost the
// user the account list.
func enrichAccountLimits(db *sql.DB, accts []*database.DBAccount) {
	limitSet := database.GetProviderLimits(db)
	for _, a := range accts {
		lim := limitSet.For(a.Provider, limits.NormalizeTier(a.Tier))
		a.MaxFileBytes = lim.MaxFileBytes
		a.TransferBudgetBytes = lim.TransferBytes
		a.TransferWindowHours = lim.WindowHours
		if !lim.Budgeted() {
			continue
		}
		used, err := database.TransferUsedSince(db, a.ID, lim.WindowHours)
		if err != nil {
			slog.Warn("reading transfer usage", "account_id", a.ID, "error", err)
			continue
		}
		a.TransferUsedBytes = used
	}
}

// mainAccountResponse describes one provider's database-backup account for the
// Accounts view. Main accounts are not rows in the accounts table (that table
// drives the upload modal and the per-user table), so this reads the in-memory
// store instead. Credentials never leave the server: only the provider, the
// email, and whether the configuration is complete.
type mainAccountResponse struct {
	Provider string `json:"provider"`
	Email    string `json:"email"`
	// Usable is false when the account is configured but missing credentials it
	// needs to log in; MissingKeys then names the .env keys to fill in.
	Usable      bool     `json:"usable"`
	MissingKeys []string `json:"missing_keys"`

	// Cached capacity, polled into main_accounts by the quota syncer. Zero until
	// the first successful poll.
	QuotaTotalGB float64 `json:"quota_total_gb"`
	QuotaUsedGB  float64 `json:"quota_used_gb"`
	LastSync     *string `json:"last_quota_sync"`

	// NeedsReauth is set when the provider rejected this account's OAuth token.
	NeedsReauth  bool   `json:"needs_reauth"`
	ReauthReason string `json:"reauth_reason,omitempty"`
}

// GetMainAccounts lists the configured metadata-database backup accounts, one
// per provider at most. Route: GET /api/accounts/main.
//
// Credentials never reach the browser: this returns identity, configuration
// state, cached quota and whether re-authorization is needed — nothing else.
func (h *Handlers) GetMainAccounts(w http.ResponseWriter, r *http.Request) {
	quotas, err := database.ListMainAccountQuotas(h.db)
	if err != nil {
		// A quota lookup failure must not hide the accounts themselves; report the
		// list without capacity rather than failing the request.
		slog.Warn("listing main account quotas", "error", err)
		quotas = map[string]database.MainAccountQuota{}
	}

	out := make([]mainAccountResponse, 0)
	for _, m := range h.accounts.Mains() {
		missing := m.MissingKeys()
		if missing == nil {
			missing = []string{}
		}
		resp := mainAccountResponse{
			Provider:     string(m.Provider),
			Email:        m.Email,
			Usable:       len(missing) == 0,
			MissingKeys:  missing,
			NeedsReauth:  m.NeedsReauth,
			ReauthReason: m.ReauthReason,
		}
		if q, ok := quotas[string(m.Provider)]; ok {
			resp.QuotaTotalGB, resp.QuotaUsedGB, resp.LastSync = q.QuotaTotalGB, q.QuotaUsedGB, q.LastSync
		}
		out = append(out, resp)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// credentialsStatusResponse tells the page whether credentials are usable. The
// UI shows Reason verbatim in a banner, so it must name the .env key and the
// remedy rather than describing the internal failure.
type credentialsStatusResponse struct {
	Locked bool   `json:"locked"`
	Reason string `json:"reason,omitempty"`
	EnvKey string `json:"env_key"`
}

// GetCredentialsStatus reports whether stored credentials could be opened.
// Route: GET /api/credentials.
func (h *Handlers) GetCredentialsStatus(w http.ResponseWriter, r *http.Request) {
	resp := credentialsStatusResponse{EnvKey: keyring.EnvKey}
	if reason := h.accounts.LockReason(); reason != "" {
		resp.Locked, resp.Reason = true, reason
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// userResponse is one table row: a user (email) with their configured accounts
// (across providers), their single backup record (nil if they never uploaded),
// its accumulated zips, and every job. Users with configured accounts but no
// uploads still appear, with a nil backup and empty zips/jobs.
type userResponse struct {
	Email    string                `json:"email"`
	Accounts []*database.DBAccount `json:"accounts"`
	Backup   *database.Backup      `json:"backup"`
	Zips     []*database.Zip       `json:"zips"`
	Jobs     []*database.Job       `json:"jobs"`
}

// GetUsersHandler returns the backups table grouped by user email. Route: GET
// /api/users.
func GetUsersHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accts, err := database.ListDBAccounts(db)
		if err != nil {
			slog.Error("listing accounts for users view", "error", err)
			jsonError(w, "failed to list users", http.StatusInternalServerError)
			return
		}
		// Group accounts by email, preserving first-seen order (ListDBAccounts
		// orders by provider, email).
		order := make([]string, 0)
		byEmail := make(map[string][]*database.DBAccount)
		for _, a := range accts {
			if _, seen := byEmail[a.Email]; !seen {
				order = append(order, a.Email)
			}
			byEmail[a.Email] = append(byEmail[a.Email], a)
		}

		users := make([]userResponse, 0, len(order))
		for _, email := range order {
			u := userResponse{
				Email:    email,
				Accounts: byEmail[email],
				Zips:     []*database.Zip{},
				Jobs:     []*database.Job{},
			}
			backup, err := database.GetBackupByOwner(db, email)
			if err != nil {
				slog.Error("loading backup for user", "email", email, "error", err)
				jsonError(w, "failed to load user backups", http.StatusInternalServerError)
				return
			}
			if backup != nil {
				u.Backup = backup
				zips, err := database.ListZipsByBackup(db, backup.ID)
				if err != nil {
					slog.Error("listing zips", "backup", backup.ID, "error", err)
					jsonError(w, "failed to list zips", http.StatusInternalServerError)
					return
				}
				jobs, err := database.ListJobsByBackup(db, backup.ID)
				if err != nil {
					slog.Error("listing jobs", "backup", backup.ID, "error", err)
					jsonError(w, "failed to list jobs", http.StatusInternalServerError)
					return
				}
				if zips != nil {
					u.Zips = zips
				}
				if jobs != nil {
					u.Jobs = jobs
				}
			}
			users = append(users, u)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(users)
	}
}

// searchMatch is one directory inside one zip's recorded tree that matched the
// query, attributed back to the record, zip, and accounts holding it.
type searchMatch struct {
	BackupID   int64    `json:"backup_id"`
	Title      string   `json:"title"`
	OwnerEmail string   `json:"owner_email"`
	ZipID      int64    `json:"zip_id"`
	ZipName    string   `json:"zip_name"`
	Accounts   []string `json:"accounts"`
	// Path is the matching directory's location within the tree, joined with
	// "/" from the root, so the user can see where in the structure it sits.
	Path string `json:"path"`
	Name string `json:"name"`
}

// SearchTreesHandler searches every zip's recorded directory tree for q,
// case- and accent-insensitively, and returns each matching directory with the
// backup, zip, and accounts that hold it. An empty q returns an empty list.
// Route: GET /api/search?q=.
func SearchTreesHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := strings.TrimSpace(r.URL.Query().Get("q"))
		w.Header().Set("Content-Type", "application/json")
		if q == "" {
			json.NewEncoder(w).Encode([]searchMatch{})
			return
		}

		zips, err := database.ListSearchableZips(db)
		if err != nil {
			slog.Error("loading zips for search", "q", q, "error", err)
			jsonError(w, "failed to search", http.StatusInternalServerError)
			return
		}

		matches := make([]searchMatch, 0)
		for _, z := range zips {
			var root scanner.Node
			if err := json.Unmarshal([]byte(z.TreeJSON), &root); err != nil {
				// A zip with an unparseable or empty tree simply contributes no
				// matches; one bad row must not fail the whole search.
				continue
			}
			for _, hit := range searchNode(&root, nil, q) {
				matches = append(matches, searchMatch{
					BackupID:   z.BackupID,
					Title:      z.Title,
					OwnerEmail: z.OwnerEmail,
					ZipID:      z.ZipID,
					ZipName:    z.ZipName,
					Accounts:   z.Accounts,
					Path:       hit.path,
					Name:       hit.name,
				})
			}
		}
		json.NewEncoder(w).Encode(matches)
	}
}

type treeHit struct {
	name string
	path string
}

// searchNode walks a recorded tree and collects every node whose name matches q.
// ancestors carries the names from the root down to (but excluding) n, so a hit
// can report its full path.
func searchNode(n *scanner.Node, ancestors []string, q string) []treeHit {
	if n == nil {
		return nil
	}
	here := append(append([]string{}, ancestors...), n.Name)

	var hits []treeHit
	if scanner.Matches(n.Name, q) {
		hits = append(hits, treeHit{name: n.Name, path: strings.Join(here, "/")})
	}
	for _, c := range n.Children {
		hits = append(hits, searchNode(c, here, q)...)
	}
	return hits
}

// GetSettingsHandler returns the user-editable settings. Route: GET
// /api/settings.
//
// The response is a fixed set of keys, not a dump of the settings table. That
// table also holds the credential salt, verifier and KDF parameters, and this
// endpoint must never be a way to read them.
func GetSettingsHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		terms, err := database.GetExcludeTerms(db)
		if err != nil {
			slog.Error("loading settings", "error", err)
			jsonError(w, "failed to load settings", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"exclude_terms":   terms,
			"provider_limits": database.GetProviderLimits(db),
			// The shipped caps, so the Settings modal can offer "reset to defaults"
			// without hardcoding numbers the server owns.
			"provider_limit_defaults": limits.Defaults(),
		})
	}
}

// PutSettingsHandler replaces the exclude terms and the provider limits. Both
// take effect on the next backup; archives already uploaded are left alone, the
// same rule exclude terms have always followed. Route: PUT /api/settings.
//
// Each field is optional and applied only when present, so the Settings modal
// can save one section without having to send the other back untouched.
func PutSettingsHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ExcludeTerms   []string `json:"exclude_terms"`
			ProviderLimits *struct {
				Raw json.RawMessage `json:"-"`
			} `json:"-"`
		}
		// Decode twice: once into the typed shape, once as a raw map so an absent
		// key is distinguishable from an empty one. Sending no exclude_terms must
		// not silently clear the user's terms.
		raw := map[string]json.RawMessage{}
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			jsonError(w, "invalid request body", http.StatusBadRequest)
			return
		}

		if v, ok := raw["exclude_terms"]; ok {
			if err := json.Unmarshal(v, &body.ExcludeTerms); err != nil {
				jsonError(w, "invalid exclude_terms", http.StatusBadRequest)
				return
			}
			if err := database.SetExcludeTerms(db, body.ExcludeTerms); err != nil {
				slog.Error("saving exclude terms", "error", err)
				jsonError(w, "failed to save settings", http.StatusInternalServerError)
				return
			}
		}

		if v, ok := raw["provider_limits"]; ok {
			// Parse rather than store verbatim: it merges over the defaults, drops
			// providers and tiers the code does not know, and clamps nonsense. A
			// value written straight through could otherwise disable a cap by
			// omitting a field.
			set := limits.Parse(string(v))
			if err := database.SetProviderLimits(db, set); err != nil {
				slog.Error("saving provider limits", "error", err)
				jsonError(w, "failed to save settings", http.StatusInternalServerError)
				return
			}
		}

		terms, err := database.GetExcludeTerms(db)
		if err != nil {
			slog.Error("reloading settings", "error", err)
			jsonError(w, "failed to reload settings", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"exclude_terms":           terms,
			"provider_limits":         database.GetProviderLimits(db),
			"provider_limit_defaults": limits.Defaults(),
		})
	}
}

// PutAccountTierHandler changes one account's plan with its provider, which
// selects the size cap and transfer budget applied to it. Route: PUT
// /api/accounts/{id}/tier.
//
// The change is marked as app-set so the .env reconciliation on the next boot
// leaves it alone. Without that, this endpoint would appear to work and revert
// at the next restart.
func PutAccountTierHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			jsonError(w, "invalid account id", http.StatusBadRequest)
			return
		}
		var body struct {
			Tier string `json:"tier"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonError(w, "invalid request body", http.StatusBadRequest)
			return
		}
		tier := limits.NormalizeTier(body.Tier)
		if string(tier) != body.Tier {
			jsonError(w, "tier must be \"free\" or \"paid\"", http.StatusBadRequest)
			return
		}
		if _, err := database.GetDBAccountByID(db, id); err != nil {
			jsonError(w, "account not found", http.StatusNotFound)
			return
		}
		if err := database.UpdateAccountTier(db, id, string(tier)); err != nil {
			slog.Error("updating account tier", "account_id", id, "error", err)
			jsonError(w, "failed to update the account tier", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": id, "tier": string(tier)})
	}
}

// PatchBackupHandler renames a record without re-uploading, backing a save in
// the Upload/Edit modal where the user changed only the title. Route: PATCH
// /api/backups/{id}.
func PatchBackupHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			jsonError(w, "invalid backup id", http.StatusBadRequest)
			return
		}
		var body struct {
			Title string `json:"title"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonError(w, "invalid request body", http.StatusBadRequest)
			return
		}
		title := strings.TrimSpace(body.Title)
		if title == "" {
			jsonError(w, "title is required", http.StatusBadRequest)
			return
		}
		if err := database.UpdateBackupTitle(db, id, title); err == sql.ErrNoRows {
			jsonError(w, "backup not found", http.StatusNotFound)
			return
		} else if err != nil {
			slog.Error("renaming backup", "backup", id, "error", err)
			jsonError(w, "failed to rename backup", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// QuotaSyncHandler triggers an immediate quota refresh for every numbered
// account and returns the updated account list. Route: POST
// /api/accounts/quota-sync.
func (h *Handlers) QuotaSync(syncer *quota.Syncer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Polling a quota needs a credential like everything else, and the syncer
		// would otherwise skip every account server-side and hand back an unchanged
		// list with a 200 — a button that reports success and does nothing.
		if h.refuseWhenLocked(w) {
			return
		}
		if syncer != nil {
			syncer.SyncAll(r.Context())
		}
		accts, err := database.ListDBAccounts(h.db)
		if err != nil {
			slog.Error("listing accounts after quota sync", "error", err)
			jsonError(w, "failed to list accounts", http.StatusInternalServerError)
			return
		}
		if accts == nil {
			accts = []*database.DBAccount{}
		}
		enrichAccountLimits(h.db, accts)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(accts)
	}
}

// conflictInfo describes a same-name file already present on a selected account.
type conflictInfo struct {
	AccountID int64  `json:"account_id"`
	Provider  string `json:"provider"`
	Email     string `json:"email"`
	Name      string `json:"name"`
}

func (h *Handlers) PostBackups(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OwnerEmail string  `json:"owner_email"`
		Title      string  `json:"title"`
		SourcePath string  `json:"source_path"`
		AccountIDs []int64 `json:"account_ids"`
		// ConflictResolutions maps an account id (as a string key, since JSON
		// object keys are strings) to "overwrite" or "skip". Sent on resubmit
		// after the user resolves the conflicts reported by a prior 409.
		ConflictResolutions map[string]string `json:"conflict_resolutions"`
	}

	// Checked before anything else: with credentials locked the store is empty,
	// so every account id would come back "unknown" and send the user looking for
	// a configuration problem they do not have.
	if h.refuseWhenLocked(w) {
		return
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.Title == "" {
		jsonError(w, "title is required", http.StatusBadRequest)
		return
	}

	if req.OwnerEmail == "" {
		jsonError(w, "owner_email is required", http.StatusBadRequest)
		return
	}

	info, err := os.Stat(req.SourcePath)
	if err != nil || !info.IsDir() {
		jsonError(w, "source_path must be an existing directory", http.StatusBadRequest)
		return
	}

	if len(req.AccountIDs) == 0 {
		jsonError(w, "at least one account_id is required", http.StatusBadRequest)
		return
	}

	// Drop accounts the user chose to skip; the rest are the effective upload
	// targets for quota, conflict detection, and job creation.
	effective := make([]int64, 0, len(req.AccountIDs))
	for _, id := range req.AccountIDs {
		if req.ConflictResolutions[strconv.FormatInt(id, 10)] == "skip" {
			continue
		}
		effective = append(effective, id)
	}
	if len(effective) == 0 {
		jsonError(w, "all selected accounts were skipped; nothing to upload", http.StatusBadRequest)
		return
	}

	// Decide the archives before building any of them. A source that cannot fit
	// its targets is refused here, on directory metadata alone, rather than after
	// compressing several gigabytes to no purpose.
	plan, err := h.buildUploadPlan(req.SourcePath, effective)
	if err != nil {
		slog.Error("planning upload", "path", req.SourcePath, "error", err)
		jsonError(w, "failed to plan the backup: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !plan.OK() {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]any{
			"error":    "this backup cannot be uploaded as configured",
			"blockers": plan.Blockers,
			"warnings": plan.Warnings,
		})
		return
	}

	// Exclude terms shape only the recorded tree — the zips below still archive
	// every directory, so the uploaded copies stay complete. A settings read
	// failure degrades to "exclude nothing" rather than blocking the backup.
	excludeTerms, err := database.GetExcludeTerms(h.db)
	if err != nil {
		slog.Warn("could not load exclude terms; recording full tree", "error", err)
		excludeTerms = nil
	}

	built, err := h.buildArchives(req.SourcePath, plan, excludeTerms)
	if err != nil {
		built.discard()
		slog.Error("creating archives", "path", req.SourcePath, "error", err)
		jsonError(w, "failed to create the zip archives: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Conflict detection runs only once the archives exist. "Overwrite" deletes
	// the copy already on the provider, so doing it earlier would mean a failure
	// while zipping (a full disk, an unreadable file, a volume that came out over
	// the threshold) leaves the user with neither the old archive nor a new one.
	// The refusal that has to happen before any compression is the plan check
	// above, which needs no network at all.
	//
	// Unresolved conflicts are reported back (409) so the user can choose
	// overwrite/skip. Detection failures (login/network) are non-fatal: we log and
	// proceed, leaving the upload worker as the source of truth.
	if conflicts := h.resolveConflicts(r.Context(), plan, req.ConflictResolutions); len(conflicts) > 0 {
		built.discard()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]any{
			"error":     "some accounts already have a file with this name",
			"conflicts": conflicts,
		})
		return
	}

	backupID, err := h.recordBackup(req.OwnerEmail, req.Title, req.SourcePath, built)
	if err != nil {
		built.discard()
		slog.Error("recording backup", "error", err)
		jsonError(w, "failed to record the backup", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]any{"id": backupID, "warnings": plan.Warnings})
}

// builtArchive is one zip written to disk, with everything needed to record it.
type builtArchive struct {
	zipPath    string
	remoteName string
	sizeBytes  int64
	treeJSON   string
	accountIDs []int64
}

// builtSet is every archive produced for one backup.
type builtSet []*builtArchive

// discard removes the temp zips. Called on any failure after zipping: a
// half-recorded backup would leave multi-gigabyte files beside the user's source
// directory with nothing in the database pointing at them.
func (b builtSet) discard() {
	for _, a := range b {
		if a.zipPath != "" {
			os.Remove(a.zipPath)
		}
	}
}

// buildArchives compresses the planned volumes. It returns whatever it managed
// to write alongside the error, so the caller can clean up a partial set.
//
// Each group is zipped independently: a group whose threshold requires splitting
// gets one archive per volume, and a group that fits gets one whole-directory
// archive named exactly as an unsplit backup always has been.
func (h *Handlers) buildArchives(sourcePath string, plan *uploadPlan, excludeTerms []string) (builtSet, error) {
	var built builtSet

	for _, g := range plan.Groups {
		names := g.plan.Names(sourcePath)
		accountIDs := make([]int64, 0, len(g.Accounts))
		for _, a := range g.Accounts {
			accountIDs = append(accountIDs, a.ID)
		}

		if g.plan.ByteParts() {
			parts, err := h.buildByteParts(sourcePath, g, accountIDs, excludeTerms)
			built = append(built, parts...)
			if err != nil {
				return built, err
			}
			continue
		}

		for i, vol := range g.plan.Volumes {
			includes := g.plan.Includes(i)

			// Each volume's tree covers only what that volume holds. A volume
			// claiming the whole source would make the record's merged tree and the
			// global search describe archives that do not contain what they say.
			root, err := scanner.TreeFor(sourcePath, includes, scanner.Options{
				MaxDepth:     h.scanMaxDepth,
				ExcludeTerms: excludeTerms,
			})
			if err != nil {
				return built, fmt.Errorf("scanning source directory: %w", err)
			}

			var zipPath string
			if len(g.plan.Volumes) == 1 {
				zipPath, err = archive.Zip(sourcePath)
			} else {
				zipPath, err = archive.ZipItems(sourcePath, vol.Items)
			}
			if err != nil {
				return built, err
			}

			info, err := os.Stat(zipPath)
			if err != nil {
				os.Remove(zipPath)
				return built, fmt.Errorf("stat zip file: %w", err)
			}

			// Packing decided on raw sizes; this is the check against what was
			// actually written. Uploading a volume over the threshold would be
			// uploading something the provider is going to reject, so it fails here
			// with an explanation rather than several minutes later in a job log.
			if g.ThresholdBytes > 0 && info.Size() > g.ThresholdBytes {
				os.Remove(zipPath)
				return built, fmt.Errorf(
					"archive %s came out at %s, over the %s limit for these accounts; lower this provider's split threshold in Settings",
					names[i], archive.HumanBytes(info.Size()), archive.HumanBytes(g.ThresholdBytes))
			}

			built = append(built, &builtArchive{
				zipPath:    zipPath,
				remoteName: names[i],
				sizeBytes:  info.Size(),
				treeJSON:   scanner.TreeJSON(root),
				accountIDs: accountIDs,
			})
		}
	}
	return built, nil
}

// recordBackup writes the record, its zips and their jobs in one transaction, so
// a backup is never half-registered.
func (h *Handlers) recordBackup(ownerEmail, title, sourcePath string, built builtSet) (int64, error) {
	tx, err := h.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("beginning transaction: %w", err)
	}

	// One record per user, accumulating zips. Upsert the record (renaming it if a
	// title was supplied), then attach each archive as a zip with its own tree.
	backupID, err := database.UpsertBackupForUser(tx, ownerEmail, title, sourcePath)
	if err != nil {
		tx.Rollback()
		return 0, fmt.Errorf("upserting backup record: %w", err)
	}

	for _, a := range built {
		zipID, err := database.InsertZip(tx, backupID, a.remoteName, sourcePath, a.sizeBytes, a.treeJSON)
		if err != nil {
			tx.Rollback()
			return 0, fmt.Errorf("inserting zip %s: %w", a.remoteName, err)
		}
		for _, accountID := range a.accountIDs {
			if _, err := database.InsertJob(tx, backupID, zipID, accountID, a.zipPath, a.remoteName, a.sizeBytes); err != nil {
				tx.Rollback()
				return 0, fmt.Errorf("inserting job for account %d: %w", accountID, err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		tx.Rollback()
		return 0, fmt.Errorf("committing transaction: %w", err)
	}
	return backupID, nil
}

// PostBackupsPreflight answers what an upload would do without doing any of it:
// how many archives each provider would receive, how large they would be, and
// anything that blocks or delays them. Route: POST /api/backups/preflight.
//
// It exists so the user sees the split before committing to it. The same
// buildUploadPlan runs here and in PostBackups, so the preview cannot disagree
// with what actually happens.
func (h *Handlers) PostBackupsPreflight(w http.ResponseWriter, r *http.Request) {
	if h.refuseWhenLocked(w) {
		return
	}

	var req struct {
		SourcePath string  `json:"source_path"`
		AccountIDs []int64 `json:"account_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	info, err := os.Stat(req.SourcePath)
	if err != nil || !info.IsDir() {
		jsonError(w, "source_path must be an existing directory", http.StatusBadRequest)
		return
	}
	if len(req.AccountIDs) == 0 {
		jsonError(w, "at least one account_id is required", http.StatusBadRequest)
		return
	}

	plan, err := h.buildUploadPlan(req.SourcePath, req.AccountIDs)
	if err != nil {
		slog.Error("preflight planning failed", "path", req.SourcePath, "error", err)
		jsonError(w, "failed to plan the backup: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"ok":                 plan.OK(),
		"groups":             plan.Groups,
		"blockers":           plan.Blockers,
		"warnings":           plan.Warnings,
		"total_source_bytes": plan.TotalSourceBytes,
	})
}

// resolveConflicts checks each target account's cloud root for existing files
// with any of the names this backup will produce. For an account the user marked
// "overwrite" it deletes the existing files so the upload replaces them; an
// account with an existing file and no resolution is returned as a conflict for
// the UI to prompt on. Connection or lookup failures are logged and treated as
// "no conflict".
//
// A split backup gives each account several names, so a conflict is reported per
// (account, name). Prompting once per account would be ambiguous about which
// archive is being replaced, and checking only the first name would let the
// remaining volumes fail at upload time with 4shared's 403.0201.
func (h *Handlers) resolveConflicts(ctx context.Context, plan *uploadPlan, resolutions map[string]string) []conflictInfo {
	var conflicts []conflictInfo
	for _, g := range plan.Groups {
		names := make([]string, 0, len(g.Volumes))
		for _, v := range g.Volumes {
			names = append(names, v.Name)
		}

		for _, a := range g.Accounts {
			p, err := h.connect(ctx, a.Provider, a.Email)
			if err != nil {
				slog.Warn("conflict check: could not connect", "provider", a.Provider, "email", a.Email, "error", err)
				continue
			}
			resolution := resolutions[strconv.FormatInt(a.ID, 10)]

			for _, name := range names {
				ref, found, err := p.FindByName(ctx, name)
				if err != nil {
					slog.Warn("conflict check: lookup failed", "provider", a.Provider, "email", a.Email, "name", name, "error", err)
					continue
				}
				if !found {
					continue
				}
				if resolution == "overwrite" {
					if err := p.Delete(ctx, ref); err != nil {
						slog.Warn("conflict overwrite: delete failed", "provider", a.Provider, "email", a.Email, "name", name, "error", err)
					}
					continue
				}
				conflicts = append(conflicts, conflictInfo{
					AccountID: a.ID,
					Provider:  a.Provider,
					Email:     a.Email,
					Name:      name,
				})
			}
		}
	}
	return conflicts
}

// GetJobLogsHandler returns the log lines for a single job, powering the
// per-provider "logs" modal. Route: GET /api/jobs/{id}/logs.
func GetJobLogsHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			jsonError(w, "invalid job id", http.StatusBadRequest)
			return
		}
		logs, err := database.ListJobLogs(db, id)
		if err != nil {
			slog.Error("listing job logs", "job", id, "error", err)
			jsonError(w, "failed to list job logs", http.StatusInternalServerError)
			return
		}
		if logs == nil {
			logs = []*database.JobLog{}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(logs)
	}
}

// DownloadJob streams the remote zip for a completed job back to the browser.
// Route: GET /api/jobs/{id}/download.
func (h *Handlers) DownloadJob(w http.ResponseWriter, r *http.Request) {
	if h.refuseWhenLocked(w) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		jsonError(w, "invalid job id", http.StatusBadRequest)
		return
	}
	job, err := database.GetJob(h.db, id)
	if err != nil {
		jsonError(w, "job not found", http.StatusNotFound)
		return
	}
	if job.Status != "complete" || job.RemotePath == "" {
		jsonError(w, "job has no uploaded file to download", http.StatusConflict)
		return
	}

	p, err := h.connect(r.Context(), job.Provider, job.Email)
	if err != nil {
		slog.Error("download: connect failed", "job", id, "error", err)
		jsonError(w, connectFailureMessage(job.Provider, job.Email, err), http.StatusBadGateway)
		return
	}

	filename := job.RemoteName
	if filename == "" {
		filename = filepath.Base(job.ZipPath)
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, filename))

	if err := p.Download(r.Context(), job.RemotePath, w); err != nil {
		// Headers (and likely some bytes) are already sent, so we can't switch to
		// a JSON error here; log it and let the client see a truncated download.
		slog.Error("download stream failed", "job", id, "error", err)
	}
}

func BrowseHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path, err := openFolderDialog()
		if err != nil {
			slog.Warn("folder picker error", "error", err)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"path": path})
	}
}

// refuseWhenLocked answers 503 with the lock reason and reports true when the
// caller should stop. Every operation that needs a credential consults it, so
// the API gives one consistent answer instead of each path inventing its own
// version of "that did not work".
func (h *Handlers) refuseWhenLocked(w http.ResponseWriter) bool {
	reason := h.accounts.LockReason()
	if reason == "" {
		return false
	}
	jsonError(w, "credentials are locked: "+reason, http.StatusServiceUnavailable)
	return true
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// buildByteParts produces one archive of the whole directory and cuts it into
// parts small enough for the group's threshold.
//
// This is the fallback for the one shape whole-file packing cannot express: a
// single file larger than the provider's limit. The parts are a raw byte cut, so
// none of them opens on its own — the tree is therefore recorded against the
// first part, which is both the entry point a rejoining tool is pointed at and
// the only place a tree would not be a lie about what that file contains.
func (h *Handlers) buildByteParts(sourcePath string, g *planGroup, accountIDs []int64, excludeTerms []string) (builtSet, error) {
	var built builtSet

	// The tree describes the source as a whole, because the parts jointly are the
	// source. Passing no includes is exactly that statement.
	root, err := scanner.TreeFor(sourcePath, nil, scanner.Options{
		MaxDepth:     h.scanMaxDepth,
		ExcludeTerms: excludeTerms,
	})
	if err != nil {
		return built, fmt.Errorf("scanning source directory: %w", err)
	}

	zipPath, err := archive.Zip(sourcePath)
	if err != nil {
		return built, err
	}

	partPaths, err := archive.SplitFile(zipPath, g.plan.PartBytes)
	if err != nil {
		os.Remove(zipPath)
		return built, err
	}

	for i, p := range partPaths {
		info, err := os.Stat(p)
		if err != nil {
			// Hand back what exists so the caller can clean all of it up.
			for _, rest := range partPaths[i:] {
				os.Remove(rest)
			}
			return built, fmt.Errorf("stat archive part: %w", err)
		}
		// The cut is exact, so a part over the threshold means the split itself is
		// wrong rather than compression being unlucky — worth failing loudly.
		if g.ThresholdBytes > 0 && info.Size() > g.ThresholdBytes {
			for _, rest := range partPaths[i:] {
				os.Remove(rest)
			}
			return built, fmt.Errorf("archive part %s came out at %s, over the %s limit for these accounts",
				filepath.Base(p), archive.HumanBytes(info.Size()), archive.HumanBytes(g.ThresholdBytes))
		}

		tree := ""
		if i == 0 {
			tree = scanner.TreeJSON(root)
		}
		// Compression can bring the whole archive under the threshold after the
		// plan estimated several parts from the uncompressed total. One part is a
		// complete, openable archive, so it takes the plain name — a .001 suffix
		// would tell the user to go looking for parts that do not exist.
		remoteName := archive.PartName(sourcePath, i+1)
		if len(partPaths) == 1 {
			remoteName = archive.RemoteName(sourcePath)
		}
		built = append(built, &builtArchive{
			zipPath:    p,
			remoteName: remoteName,
			sizeBytes:  info.Size(),
			treeJSON:   tree,
			accountIDs: accountIDs,
		})
	}
	return built, nil
}
