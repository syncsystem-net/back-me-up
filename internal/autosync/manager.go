// Package autosync reconciles the archives actually sitting in a user's cloud
// accounts against what the local database knows about. It exists for the case
// where an account already holds backups this install never uploaded — from an
// earlier tool, or from a machine whose database was lost — which would
// otherwise show as an empty row in a full account.
//
// Because it writes to the user's database from remote state, it always runs in
// two steps: a dry run that only reads (crawl every account, work out what would
// change) and an apply the user explicitly confirms. Nothing is ever deleted; a
// local row whose remote copy has vanished is reported and left alone, since a
// transient API failure must not be able to destroy records.
//
// Crawling is network-bound and can take minutes, so a run happens in the
// background: the HTTP handlers start it and poll a snapshot, which keeps the
// 2-second UI refresh responsive and makes the run cancellable.
package autosync

import (
	"archive/zip"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/syncsystem-net/back-me-up/internal/accounts"
	"github.com/syncsystem-net/back-me-up/internal/cloud"
	"github.com/syncsystem-net/back-me-up/internal/database"
	"github.com/syncsystem-net/back-me-up/internal/provider"
	"github.com/syncsystem-net/back-me-up/internal/scanner"
)

// Run phases, as seen by the UI.
const (
	PhaseIdle         = "idle"
	PhasePreviewing   = "previewing"
	PhasePreviewReady = "preview_ready"
	PhaseApplying     = "applying"
	PhaseApplied      = "applied"
	PhaseCancelled    = "cancelled"
	PhaseFailed       = "failed"
)

// maxFullDownloadIndexBytes caps the whole-archive fallback used when a provider
// cannot serve ranged reads. Pulling a few hundred megabytes to read a directory
// listing is wasteful but tolerable; pulling twenty gigabytes is not, and the
// user would have no idea why the sync had stalled. Past the cap the archive is
// still adopted — just without a tree, with a note saying so.
const maxFullDownloadIndexBytes = 2 << 30 // 2 GiB

// AccountPlan is one account's slice of a run: what the crawl found, and (after
// apply) what was actually written. Error is a listing/connection failure, which
// is reported rather than swallowed — an account whose token expired must never
// look like an account that simply had nothing on it.
type AccountPlan struct {
	AccountID int64  `json:"account_id"`
	Provider  string `json:"provider"`
	Email     string `json:"email"`
	Error     string `json:"error"`

	Proposed []ProposedZip `json:"proposed"`
	Matched  []MatchedZip  `json:"matched"`
	Missing  []MissingZip  `json:"missing"`

	Applied    int    `json:"applied"`
	ApplyError string `json:"apply_error"`
}

// Run is the state of a single Auto-Sync operation.
type Run struct {
	Phase     string         `json:"phase"`
	StartedAt time.Time      `json:"started_at"`
	Accounts  []*AccountPlan `json:"accounts"`
	// Progress is a human-readable note about what the crawl is doing now, so a
	// multi-minute run isn't a blank spinner.
	Progress string `json:"progress"`
	Error    string `json:"error"`
}

// Manager owns the single in-flight run. Only one runs at a time: the whole
// operation is global (every configured account), and concurrent crawls would
// race each other's writes for no benefit.
type Manager struct {
	db           *sql.DB
	store        *accounts.AccountStore
	chunkSize    int64
	scanMaxDepth int
	// maxIndexBytes caps the whole-archive fallback download. A field rather
	// than a bare constant so tests can exercise the ceiling without moving
	// gigabytes; production always gets maxFullDownloadIndexBytes.
	maxIndexBytes int64

	// connect is cloud.Connect in production. It is a field so tests can drive
	// the whole crawl against a stub backend without a network or real
	// credentials — the reconciliation rules are the part worth testing end to
	// end, and they are unreachable otherwise.
	connect func(ctx context.Context, store *accounts.AccountStore, providerName, email string, chunkSize int64) (provider.Provider, error)

	mu     sync.Mutex
	run    *Run
	cancel context.CancelFunc
	busy   bool
}

func New(db *sql.DB, store *accounts.AccountStore, chunkSize int64, scanMaxDepth int) *Manager {
	return &Manager{
		db:            db,
		store:         store,
		chunkSize:     chunkSize,
		scanMaxDepth:  scanMaxDepth,
		maxIndexBytes: maxFullDownloadIndexBytes,
		connect:       cloud.Connect,
		run:           &Run{Phase: PhaseIdle, Accounts: []*AccountPlan{}},
	}
}

// Snapshot returns a copy of the current run, safe to serialize while a crawl is
// still mutating the original.
func (m *Manager) Snapshot() Run {
	m.mu.Lock()
	defer m.mu.Unlock()
	return copyRun(m.run)
}

// StartPreview kicks off a read-only crawl of every configured account and
// returns immediately; the caller polls Snapshot. The run gets its own context
// rather than the HTTP request's, which is gone the moment this returns.
func (m *Manager) StartPreview() error {
	m.mu.Lock()
	if m.busy {
		m.mu.Unlock()
		return errors.New("an auto-sync run is already in progress")
	}
	m.busy = true
	m.run = &Run{Phase: PhasePreviewing, StartedAt: time.Now(), Accounts: []*AccountPlan{}, Progress: "Connecting…"}
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.mu.Unlock()

	go m.preview(ctx)
	return nil
}

// StartApply writes the changes the preview proposed. It refuses unless a
// preview is sitting ready, so the user can only ever confirm something they
// have been shown.
func (m *Manager) StartApply() error {
	m.mu.Lock()
	if m.busy {
		m.mu.Unlock()
		return errors.New("an auto-sync run is already in progress")
	}
	if m.run == nil || m.run.Phase != PhasePreviewReady {
		m.mu.Unlock()
		return errors.New("no auto-sync preview to apply; run a scan first")
	}
	m.busy = true
	m.run.Phase = PhaseApplying
	m.run.Progress = "Applying…"
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.mu.Unlock()

	go m.apply(ctx)
	return nil
}

// Cancel stops an in-flight crawl. A cancelled apply leaves whatever it already
// committed in place — every write is its own transaction, so the database is
// consistent, just partially updated; the next preview shows the rest.
func (m *Manager) Cancel() {
	m.mu.Lock()
	cancel := m.cancel
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// preview crawls each account and records what would change. A per-account
// failure is captured on that account's plan and the crawl moves on, so one
// expired token never costs the user the rest of the run.
func (m *Manager) preview(ctx context.Context) {
	defer m.finish()

	if m.store == nil || len(m.store.Accounts) == 0 {
		m.settle(PhasePreviewReady, "", "No accounts are configured.")
		return
	}

	for _, a := range m.store.Accounts {
		if ctx.Err() != nil {
			m.settle(PhaseCancelled, "", "Cancelled.")
			return
		}
		providerName := string(a.Provider)
		m.setProgress(fmt.Sprintf("Scanning %s — %s…", providerName, a.Email))

		// The plan is built locally and published once it is complete, so the
		// snapshot the UI polls never observes a half-filled account.
		plan := m.scanAccount(ctx, providerName, a.Email)
		m.addPlan(plan)
	}

	if ctx.Err() != nil {
		m.settle(PhaseCancelled, "", "Cancelled.")
		return
	}
	m.settle(PhasePreviewReady, "", "Scan complete.")
}

// scanAccount crawls one account read-only and returns what would change. Every
// failure path fills in Error and returns: an account that could not be listed
// reports why, and is never presented as an account with nothing on it.
func (m *Manager) scanAccount(ctx context.Context, providerName, email string) *AccountPlan {
	plan := &AccountPlan{
		Provider: providerName,
		Email:    email,
		Proposed: []ProposedZip{},
		Matched:  []MatchedZip{},
		Missing:  []MissingZip{},
	}

	acctID, err := database.GetDBAccountIDByProviderEmail(m.db, providerName, email)
	if err != nil {
		plan.Error = fmt.Sprintf("account is not in the database: %v", err)
		return plan
	}
	plan.AccountID = acctID

	p, err := m.connect(ctx, m.store, providerName, email, m.chunkSize)
	if err != nil {
		plan.Error = fmt.Sprintf("could not connect: %v", err)
		return plan
	}
	remote, err := p.List(ctx)
	if err != nil {
		// Explicitly an error, never "nothing found" — 4shared's folder listing
		// in particular can fail in ways that would otherwise be indistinguishable
		// from an empty account.
		plan.Error = fmt.Sprintf("could not list files: %v", err)
		return plan
	}

	local, err := m.localZips(email, acctID)
	if err != nil {
		plan.Error = fmt.Sprintf("could not read local records: %v", err)
		return plan
	}

	proposed, matched, missing := Plan(remote, local)
	if proposed != nil {
		plan.Proposed = proposed
	}
	if matched != nil {
		plan.Matched = matched
	}
	if missing != nil {
		plan.Missing = missing
	}
	return plan
}

// apply performs the proposed changes account by account, reconnecting because
// the preview's provider instances are long gone by the time the user confirms.
func (m *Manager) apply(ctx context.Context) {
	defer m.finish()

	plans := m.plans()
	for _, plan := range plans {
		if ctx.Err() != nil {
			m.settle(PhaseCancelled, "", "Cancelled.")
			return
		}
		if plan.Error != "" || len(plan.Proposed) == 0 {
			continue
		}
		m.setProgress(fmt.Sprintf("Applying %s — %s…", plan.Provider, plan.Email))

		p, err := m.connect(ctx, m.store, plan.Provider, plan.Email, m.chunkSize)
		if err != nil {
			m.update(func() { plan.ApplyError = fmt.Sprintf("could not connect: %v", err) })
			continue
		}

		// Iterate over a copy: the item is mutated (size, note) as it is applied,
		// and the published plan is only updated under the lock afterwards, so a
		// concurrent Snapshot never reads a half-written entry.
		for i := range plan.Proposed {
			if ctx.Err() != nil {
				m.settle(PhaseCancelled, "", "Cancelled.")
				return
			}
			item := plan.Proposed[i]
			m.setProgress(fmt.Sprintf("Applying %s — %s: %s", plan.Provider, plan.Email, item.Name))

			err := m.applyOne(ctx, p, plan, &item)
			if err != nil {
				item.Note = err.Error()
				slog.Warn("auto-sync: could not apply entry",
					"provider", plan.Provider, "email", plan.Email, "name", item.Name, "error", err)
			}
			m.update(func() {
				plan.Proposed[i] = item
				if err == nil {
					plan.Applied++
				}
			})
		}
	}

	if ctx.Err() != nil {
		m.settle(PhaseCancelled, "", "Cancelled.")
		return
	}
	m.settle(PhaseApplied, "", "Sync complete.")
}

// applyOne writes a single proposed change. A "link" only needs the job row; an
// "adopt" also reads the archive's directory tree, which is best-effort — an
// archive whose tree cannot be read is still adopted (with an empty tree and a
// note), because a downloadable record with no tree is far more useful than no
// record at all.
func (m *Manager) applyOne(ctx context.Context, p provider.Provider, plan *AccountPlan, item *ProposedZip) error {
	if item.Action == ActionLink {
		return m.linkZip(plan, item)
	}

	// Re-resolve against the database as it is NOW, not as the preview saw it.
	// Every account is planned before any of them is applied, so when the same
	// archive sits on two accounts both plans say "adopt" — applying them
	// verbatim would create two zip rows for one archive, each visible to the
	// user as a separate file. Whichever account gets there first adopts; the
	// rest degrade to a link against the row that now exists. This also absorbs
	// anything else that changed between the preview and the confirmation.
	if existingID, err := m.existingZipID(plan.Email, item.Name); err != nil {
		return err
	} else if existingID != 0 {
		item.Action = ActionLink
		item.ZipID = existingID
		return m.linkZip(plan, item)
	}

	treeJSON, size, note := m.indexArchive(ctx, p, item)
	if note != "" {
		item.Note = note
	}
	if size > 0 {
		item.SizeBytes = size
	}

	tx, err := m.db.Begin()
	if err != nil {
		return fmt.Errorf("starting transaction: %w", err)
	}
	defer tx.Rollback()

	backupID, err := m.backupIDFor(tx, plan.Email, archiveBaseName(item.Name))
	if err != nil {
		return err
	}
	// No source_path: the archive was discovered remotely, so there is no local
	// directory it came from. The UI tolerates the empty value.
	zipID, err := database.InsertZip(tx, backupID, item.Name, "", item.SizeBytes, treeJSON)
	if err != nil {
		return err
	}
	if _, err := database.InsertAdoptedJob(tx, backupID, zipID, plan.AccountID, item.Name, item.RemoteID, item.SizeBytes); err != nil {
		return err
	}
	return tx.Commit()
}

// existingZipID returns the id of the user's recorded archive with this name, or
// 0 when there is none. It reads live state so an apply can tell an archive that
// is genuinely new from one a sibling account adopted moments ago.
func (m *Manager) existingZipID(email, name string) (int64, error) {
	backup, err := database.GetBackupByOwner(m.db, email)
	if err != nil {
		return 0, fmt.Errorf("looking up backup record: %w", err)
	}
	if backup == nil {
		return 0, nil
	}
	zips, err := database.ListZipsByBackup(m.db, backup.ID)
	if err != nil {
		return 0, fmt.Errorf("listing recorded archives: %w", err)
	}
	// Newest first, matching the ordering Plan's preference rule assumes.
	for _, z := range zips {
		if z.Name == name {
			return z.ID, nil
		}
	}
	return 0, nil
}

// linkZip attaches an existing zip row to this account, for an archive that is
// recorded locally but was not known to live here too.
func (m *Manager) linkZip(plan *AccountPlan, item *ProposedZip) error {
	zip, err := database.GetZip(m.db, item.ZipID)
	if err != nil {
		return fmt.Errorf("loading zip row: %w", err)
	}
	tx, err := m.db.Begin()
	if err != nil {
		return fmt.Errorf("starting transaction: %w", err)
	}
	defer tx.Rollback()

	size := item.SizeBytes
	if size == 0 {
		size = zip.SizeBytes
	}
	if _, err := database.InsertAdoptedJob(tx, zip.BackupID, zip.ID, plan.AccountID, item.Name, item.RemoteID, size); err != nil {
		return err
	}
	return tx.Commit()
}

// backupIDFor returns the user's backup record, creating it when the user has
// never had one. An existing record is returned untouched — passing a title to
// UpsertBackupForUser would rename it, and a crawl has no business renaming a
// record the user titled themselves.
func (m *Manager) backupIDFor(tx *sql.Tx, email, fallbackTitle string) (int64, error) {
	var id int64
	err := tx.QueryRow(`SELECT id FROM backups WHERE owner_email = ?`, email).Scan(&id)
	if err == nil {
		return id, nil
	}
	if err != sql.ErrNoRows {
		return 0, fmt.Errorf("looking up backup record: %w", err)
	}
	return database.UpsertBackupForUser(tx, email, fallbackTitle, "")
}

// indexArchive reads a remote archive's central directory and renders it as the
// same tree a local scan would have produced. It returns the serialized tree,
// the archive's true size when that had to be discovered, and a note when the
// tree could not be read.
//
// The fast path streams only the tail of the archive through ranged reads. When
// the provider cannot do that — 4shared may ignore the Range header entirely —
// it falls back to downloading the archive once to a temp file, which is honest
// and correct rather than a guess at where the bytes came from.
//
// That fallback is always bounded by maxFullDownloadIndexBytes, and the bound is
// enforced on the bytes actually received, not on the size the listing claimed.
// A listing's size is best-effort — 4shared in particular may report 0 — and a
// size of 0 must never buy an archive an unmetered transfer. The user asked to
// read a file list; silently pulling tens of gigabytes to do it would be a
// betrayal of that, so an archive over the ceiling is adopted without a tree and
// says so.
func (m *Manager) indexArchive(ctx context.Context, p provider.Provider, item *ProposedZip) (treeJSON string, size int64, note string) {
	opts, err := m.scanOptions()
	if err != nil {
		slog.Warn("auto-sync: could not load exclude terms; recording full tree", "error", err)
	}

	if item.SizeBytes > 0 {
		r := newRemoteReaderAt(ctx, p, item.RemoteID, item.SizeBytes)
		zr, err := zip.NewReader(r, item.SizeBytes)
		if err == nil {
			root := TreeFromEntries(item.Name, EntriesFromReader(zr), opts)
			return scanner.TreeJSON(root), 0, ""
		}
		if !errors.Is(err, provider.ErrRangeUnsupported) {
			return "", 0, fmt.Sprintf("could not read the archive's directory: %v", err)
		}
		if item.SizeBytes > m.maxIndexBytes {
			return "", 0, tooLargeNote(fmt.Sprintf("%.1f GB", float64(item.SizeBytes)/(1<<30)))
		}
	}

	// Either the size was unknown (so a ranged read has nothing to seek
	// relative to) or ranges are unsupported. Download once and index locally;
	// the download also establishes the true size.
	treeJSON, size, err = m.indexByDownload(ctx, p, item, opts)
	if errors.Is(err, errIndexTooLarge) {
		return "", 0, tooLargeNote(fmt.Sprintf("over %s", byteSize(m.maxIndexBytes)))
	}
	if err != nil {
		return "", 0, fmt.Sprintf("could not read the archive's directory: %v", err)
	}
	return treeJSON, size, ""
}

// byteSize renders a limit for a user-facing note, in whichever unit reads best.
func byteSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.0f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}

func tooLargeNote(size string) string {
	return fmt.Sprintf(
		"this provider does not support ranged reads and the archive is %s, too large to download just to index; adopted without a file tree",
		size)
}

// errIndexTooLarge aborts a fallback download that has crossed the indexing
// ceiling. It is a sentinel rather than a plain error so indexArchive can tell
// "too big to bother with" (adopt, no tree, explain why) apart from a genuine
// transfer failure.
var errIndexTooLarge = errors.New("archive exceeds the auto-sync indexing size limit")

// cappedWriter fails the write that would take the total past limit. Counting
// received bytes is the only trustworthy guard here: the provider's advertised
// size may be wrong or absent, and Download streams without announcing a length,
// so a pre-flight size check alone is bypassed entirely by a listing reporting 0.
type cappedWriter struct {
	w         io.Writer
	remaining int64
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > c.remaining {
		// Write the part that still fits so the caller's accounting stays
		// honest, then stop the transfer.
		n, err := c.w.Write(p[:c.remaining])
		c.remaining -= int64(n)
		if err != nil {
			return n, err
		}
		return n, errIndexTooLarge
	}
	n, err := c.w.Write(p)
	c.remaining -= int64(n)
	return n, err
}

// indexByDownload pulls the archive to a temp file and reads it there, aborting
// once maxFullDownloadIndexBytes have arrived.
func (m *Manager) indexByDownload(ctx context.Context, p provider.Provider, item *ProposedZip, opts scanner.Options) (string, int64, error) {
	f, err := os.CreateTemp("", "backmeup-autosync-*.zip")
	if err != nil {
		return "", 0, fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := f.Name()
	defer os.Remove(tmpPath)
	defer f.Close()

	capped := &cappedWriter{w: f, remaining: m.maxIndexBytes}
	if err := p.Download(ctx, item.RemoteID, capped); err != nil {
		if errors.Is(err, errIndexTooLarge) {
			slog.Warn("auto-sync: stopped indexing download at the size ceiling",
				"name", item.Name, "limit_bytes", m.maxIndexBytes)
			return "", 0, errIndexTooLarge
		}
		return "", 0, fmt.Errorf("downloading archive: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		return "", 0, fmt.Errorf("sizing downloaded archive: %w", err)
	}
	zr, err := zip.NewReader(f, info.Size())
	if err != nil {
		return "", 0, fmt.Errorf("reading archive: %w", err)
	}
	root := TreeFromEntries(item.Name, EntriesFromReader(zr), opts)
	return scanner.TreeJSON(root), info.Size(), nil
}

// scanOptions mirrors what a local backup would record: the configured depth cap
// and the user's exclude terms, so a discovered tree and an uploaded one obey the
// same rules. A settings read failure degrades to "exclude nothing" rather than
// aborting the sync.
func (m *Manager) scanOptions() (scanner.Options, error) {
	terms, err := database.GetExcludeTerms(m.db)
	if err != nil {
		return scanner.Options{MaxDepth: m.scanMaxDepth}, err
	}
	return scanner.Options{MaxDepth: m.scanMaxDepth, ExcludeTerms: terms}, nil
}

// localZips loads the user's recorded archives, annotated with this account's
// job for each — the shape Plan reconciles against.
func (m *Manager) localZips(email string, accountID int64) ([]LocalZip, error) {
	backup, err := database.GetBackupByOwner(m.db, email)
	if err != nil {
		return nil, err
	}
	if backup == nil {
		return nil, nil
	}
	zips, err := database.ListZipsByBackup(m.db, backup.ID)
	if err != nil {
		return nil, err
	}
	jobs, err := database.ListJobsByBackup(m.db, backup.ID)
	if err != nil {
		return nil, err
	}
	// One job per (zip, account) is the norm; if a zip somehow has several on the
	// same account, any of them proves the account holds it.
	jobByZip := make(map[int64]*database.Job, len(jobs))
	for _, j := range jobs {
		if j.AccountID != accountID {
			continue
		}
		if existing, ok := jobByZip[j.ZipID]; ok && existing.RemotePath != "" {
			continue
		}
		jobByZip[j.ZipID] = j
	}

	local := make([]LocalZip, 0, len(zips))
	for _, z := range zips {
		l := LocalZip{ZipID: z.ID, Name: z.Name}
		if j, ok := jobByZip[z.ID]; ok {
			l.JobID = j.ID
			l.RemotePath = j.RemotePath
		}
		local = append(local, l)
	}
	return local, nil
}

// ---- run state helpers ----

func (m *Manager) addPlan(p *AccountPlan) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.run.Accounts = append(m.run.Accounts, p)
}

func (m *Manager) plans() []*AccountPlan {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.run.Accounts
}

// update runs fn holding the run lock, for the small mutations the apply loop
// makes to an already-published plan.
func (m *Manager) update(fn func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fn()
}

func (m *Manager) setProgress(msg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.run.Progress = msg
}

func (m *Manager) settle(phase, errMsg, progress string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.run.Phase = phase
	m.run.Error = errMsg
	m.run.Progress = progress
}

func (m *Manager) finish() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.busy = false
	m.cancel = nil
}

// copyRun deep-copies enough of a run that the caller can serialize it while the
// crawl keeps mutating the original. The per-account plans are the parts a
// running crawl writes to, so they are copied by value.
func copyRun(r *Run) Run {
	out := Run{
		Phase:     r.Phase,
		StartedAt: r.StartedAt,
		Progress:  r.Progress,
		Error:     r.Error,
		Accounts:  make([]*AccountPlan, 0, len(r.Accounts)),
	}
	for _, p := range r.Accounts {
		c := *p
		c.Proposed = append([]ProposedZip{}, p.Proposed...)
		c.Matched = append([]MatchedZip{}, p.Matched...)
		c.Missing = append([]MissingZip{}, p.Missing...)
		out.Accounts = append(out.Accounts, &c)
	}
	return out
}
