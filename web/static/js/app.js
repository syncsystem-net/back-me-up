// Parsed tree_json cache, deliberately OUTSIDE Alpine.data so the parsed objects
// never get wrapped in a reactive proxy — the trees are read-only render input and
// proxying them would cost on every one of the many re-renders the poll triggers.
// Keyed by zip id; the raw string is stored alongside so an edited tree re-parses.
const treeCache = new Map();

function parseTreeJSON(zip) {
    if (!zip) return null;
    const raw = zip.tree_json || '';
    const hit = treeCache.get(zip.id);
    if (hit && hit.raw === raw) return hit.tree;
    let tree = null;
    try { tree = raw ? JSON.parse(raw) : null; } catch { tree = null; }
    // A tree with no root name is the server's marshal-failure placeholder ("{}"),
    // not a real tree — show the "none recorded" fallback rather than a bare
    // "(unnamed)" row.
    if (tree && !tree.name) tree = null;
    // Sorted once here rather than on every render: the poll re-renders the
    // table constantly and the parsed tree is reused until its raw string
    // changes. The raw string is what the cache compares, so sorting the parsed
    // copy cannot make an unchanged tree look edited.
    if (tree) tree = sortTree(tree);
    treeCache.set(zip.id, { raw, tree });
    return tree;
}

// Natural, case-insensitive order: bkup2 before bkup10, "Docs" beside "docs".
// The server records children in walk order and lists zips newest first, so
// left alone a record reads bkup040 … bkup001.
function naturalCompare(a, b) {
    return String(a || '').localeCompare(String(b || ''), undefined, { numeric: true, sensitivity: 'base' });
}

// A sorted copy of a parsed tree, children ordered by name at every level.
function sortTree(node) {
    const kids = (node.children || []).map(sortTree);
    kids.sort((a, b) => naturalCompare(a.name, b.name));
    return { ...node, children: kids };
}

// Alpine hands templates reactive proxies. Caches keyed by object identity need
// the underlying object, or each proxy wrapper would look like a new key.
function rawOf(v) {
    return (window.Alpine && typeof Alpine.raw === 'function') ? Alpine.raw(v) : v;
}

// ---- Effective jobs ----
// A record keeps every upload attempt: a failed 4shared job stays in the table
// after the directory is re-uploaded to 4shared and succeeds. The *effective*
// job for an archive on an account is the newest one — the highest id among
// jobs sharing (record, account, remote name). Everything that asks "is this
// still a problem?" uses this one rule — the provider chip, the record label,
// the Jobs view's Replaced marker and its Retry button — and the server's retry
// refusal applies the same key, so none of them can disagree.
//
// Memoised per jobs array: /api/users replaces the arrays on every poll tick,
// so a stale entry is simply never asked for again and the WeakMap drops it.
const effectiveCache = new WeakMap();

function jobArchiveKey(j) {
    return `${j.backup_id}|${j.account_id}|${j.remote_name || ''}`;
}

function effectiveIndex(jobs) {
    const raw = rawOf(jobs || []);
    const hit = effectiveCache.get(raw);
    if (hit) return hit;
    const byKey = new Map();
    for (const j of raw) {
        const k = jobArchiveKey(j);
        const cur = byKey.get(k);
        if (!cur || j.id > cur.id) byKey.set(k, j);
    }
    const ids = new Set();
    for (const j of byKey.values()) ids.add(j.id);
    const entry = { byKey, ids };
    effectiveCache.set(raw, entry);
    return entry;
}

function isActiveJob(j) { return j.status === 'pending' || j.status === 'in_progress'; }

// Display names for providers, as the user knows them.
function providerName(p) {
    return p === 'mega' ? 'MEGA' : p === 'fourshared' ? '4shared' : (p || '');
}

// Case- and accent-insensitive folding, mirroring scanner.Fold on the server so
// the live table filter and the ▶ global search agree on what "matches" means.
// normalize('NFD') splits an accented rune into base + combining mark, and the
// range strip removes the marks — the same two steps the Go side performs.
function fold(s) {
    return (s || '').normalize('NFD').replace(/[\u0300-\u036f]/g, '').toLowerCase().trim();
}

// Folded directory names of a zip's recorded tree, cached alongside the parsed
// tree. Matching names rather than the raw tree_json string matters: the raw
// JSON contains the keys "children" and "size_bytes", so typing either of those
// used to match every row that had a tree.
function treeNames(zip) {
    const raw = zip && zip.tree_json || '';
    const hit = treeCache.get(zip && zip.id);
    if (hit && hit.raw === raw && hit.names !== undefined) return hit.names;
    const tree = parseTreeJSON(zip);
    const parts = [];
    const walk = (n) => {
        if (!n) return;
        parts.push(fold(n.name || ''));
        for (const c of (n.children || [])) walk(c);
    };
    walk(tree);
    const names = parts.join('\n');
    const entry = treeCache.get(zip.id);
    if (entry) entry.names = names;
    return names;
}

document.addEventListener('alpine:init', () => {
    Alpine.data('app', () => ({
        page: 'backups',
        // Table rows grouped by user (email). Each: {email, accounts, backup, zips, jobs}.
        users: [],
        accounts: [],
        // Database-backup accounts, at most one per provider. Read from .env at
        // startup, so this is fetched on load and on entering the Accounts tab
        // rather than on every poll tick. mainAccountsLoaded separates "none are
        // configured" from "we never got an answer" — claiming the former when a
        // fetch failed would tell the user their index is not being backed up
        // when it is.
        mainAccounts: [],
        mainAccountsLoaded: false,
        // Whether the server could open the stored credentials. Locked means the
        // passphrase is missing or wrong: the app runs, records stay browsable,
        // and every cloud action refuses. It can only change across a restart, so
        // it is fetched on load rather than polled.
        credentials: { locked: false, reason: '', env_key: '' },
        // In-flight re-authorization, mirrored from the server. The flow spans a
        // browser round trip, so the modal polls rather than awaiting one request.
        reauthRun: { phase: 'idle' },
        showReauthModal: false,
        reauthError: '',
        search: '',
        refreshingQuotas: false,
        error: '',

        // Expansion state is keyed by email so it survives the polling refresh.
        expandedAccounts: [],
        expandedFiles: [],
        allExpanded: false,
        // Tree node expansion, keyed by `${backupId}/${archiveName}:${nodePath}` for
        // the same reason. Not by zip id: same-named zip rows render as one node,
        // and a re-upload adding a row to that group must neither reset the node
        // nor give it a second state. Two lists because "open" has a default (root open, rest closed) that a single
        // list could not distinguish from "never touched".
        expandedNodes: [],
        collapsedNodes: [],
        // Accounts page: which provider accordions are open.
        expandedProviders: ['mega', 'fourshared'],

        // Upload / Edit modal.
        showUploadModal: false,
        uploadForm: { owner_email: '', backup_id: null, title: '', source_path: '', account_ids: [], accounts: [] },
        uploading: false,
        browseLoading: false,
        // What the server says this upload would produce: one archive set per
        // provider threshold, plus anything blocking or delaying it. Null until
        // checked, and cleared whenever the inputs change so a stale preview can
        // never describe a different upload than the one about to run.
        preflight: null,
        preflightLoading: false,
        preflightError: '',

        // Logs modal.
        showLogsModal: false,
        logsTitle: '',
        logs: [],

        // Delete modal — handles record-only, record+files, and per-provider deletes.
        showDeleteModal: false,
        deleteTarget: null, // { kind, backupId?, jobIds?, label, requireType }
        deleteConfirm: '',
        deleting: false,
        // Failure step: archives the provider(s) would not give up, as returned by a
        // 409 from the delete endpoints. Non-empty means the modal is showing the
        // "what is left behind / remove anyway" state instead of the confirm state.
        deleteFailures: [],
        deletedCount: 0,

        // Overwrite-on-conflict modal.
        showConflictModal: false,
        conflicts: [],
        conflictChoices: {},

        // Settings modal (exclude terms and provider limits).
        showSettingsModal: false,
        excludeTerms: [],
        newTerm: '',
        settingsSaving: false,
        settingsError: '',
        // Per-provider, per-tier size cap and transfer budget. Held as an editable
        // copy in the units the user thinks in (GB), converted on save — a byte
        // count is not something anyone wants to type.
        providerLimits: {},
        providerLimitDefaults: {},
        limitsForm: [],
        // Whether the modal's contents actually reflect what is stored. False
        // means the load failed, and saving would write memory over the database.
        settingsLoaded: false,

        // Auto-Sync (remote crawl). The run lives on the server; this is a view of
        // it, refreshed by the same poll tick while the modal is open. openedFromWarning
        // keeps the modal on the warning step until the user actually starts a scan,
        // since a finished run from earlier in the session is still on the server.
        showAutoSyncModal: false,
        autoSyncRun: { phase: 'idle', accounts: [] },
        autoSyncStarted: false,
        // Which account sections are expanded, keyed by provider|email so the
        // refresh (which replaces autoSyncRun wholesale) cannot lose the state.
        expandedSyncAccounts: [],

        // Global tree search (server-side, accent-insensitive). Separate from the
        // real-time `search` filtering, which stays purely client-side.
        searchResults: [],
        searchRan: false,
        searching: false,
        searchError: '',

        // Jobs view. Built entirely from `users`, which the poll already
        // refreshes, so it needs no endpoint of its own and updates on the same
        // cadence. The filter and per-row retry state are plain component state
        // keyed by job id, so they survive the poll replacing every job object.
        jobsFilter: 'all',
        retryingJobs: [],
        jobErrors: {},

        async init() {
            // Same rule as the refresh loop: the poll must be armed even if a
            // first fetch rejects (server still starting under Air), or the page
            // never recovers for the rest of the session.
            try {
                await Promise.all([this.loadUsers(), this.loadAccounts(), this.loadMainAccounts(),
                    this.loadSettings(), this.loadCredentials()]);
            } finally {
                this.scheduleRefresh();
            }
        },
        // Two cadences, both from config.yml: a slow idle tick, and a faster one
        // while something is actually happening so progress bars stay smooth.
        // Rescheduled after each refresh rather than run on a fixed setInterval,
        // so the cadence can change the moment work starts or finishes.
        //
        // The handle is kept so an action that creates work can re-arm the timer
        // immediately: without that, clicking Upload would leave the already-armed
        // idle timer in flight and the first progress update could be a full
        // poll_seconds away.
        _refreshTimer: null,
        scheduleRefresh() {
            const cfg = window.APP_CONFIG || {};
            const idle = cfg.pollMS > 0 ? cfg.pollMS : 10000;
            const active = cfg.activePollMS > 0 ? cfg.activePollMS : 2000;
            const delay = this.needsFastPoll() ? active : idle;
            if (this._refreshTimer) clearTimeout(this._refreshTimer);
            this._refreshTimer = setTimeout(async () => {
                // The next tick is scheduled in `finally`: a rejected refresh (a
                // server restart under Air, a network blip, a non-JSON error body)
                // must not silently kill polling for the rest of the session.
                try {
                    await this.refresh();
                } finally {
                    this.scheduleRefresh();
                }
            }, delay);
        },
        async refresh() {
            if (document.hidden) return;
            await this.loadUsers();
            if (this.hasActiveJobs()) await this.loadAccounts();
            // Only poll the crawl while its modal is open — it is a deliberate,
            // user-triggered operation, not background state the table needs.
            if (this.showAutoSyncModal && this.autoSyncStarted) await this.loadAutoSync();
            // Same rule as the crawl: only poll the re-authorization while its
            // modal is open, since it is a deliberate one-off action.
            if (this.showReauthModal) await this.loadReauth();
        },
        async loadUsers() {
            const r = await fetch('/api/users');
            if (!r.ok) return;
            this.users = await r.json() || [];
        },
        async loadAccounts() {
            const r = await fetch('/api/accounts');
            if (!r.ok) return;
            this.accounts = await r.json() || [];
        },
        async loadMainAccounts() {
            const r = await fetch('/api/accounts/main');
            if (!r.ok) return;
            this.mainAccounts = await r.json() || [];
            this.mainAccountsLoaded = true;
        },
        // Main accounts come from .env, so they only change across a restart —
        // but "edit .env, restart, check the Accounts view" is exactly the loop
        // this section exists to serve, and the page may have been open the whole
        // time. Re-read on entering the tab rather than on every poll tick.
        openAccountsPage() {
            this.page = 'accounts';
            this.loadMainAccounts();
            this.loadAccounts();
            this.loadCredentials();
        },
        async loadCredentials() {
            const r = await fetch('/api/credentials');
            if (!r.ok) return;
            this.credentials = await r.json() || { locked: false };
        },

        // ---- Re-authorization (4shared OAuth) ----
        // OAuth 1.0 has no refresh token, so an expired one can only be replaced
        // by authorizing again. The server runs the flow and writes the new token
        // straight to the encrypted store; this drives and watches it.
        needsReauth(a) { return !!(a && a.needs_reauth); },
        canReauth(a) { return !!(a && a.provider === 'fourshared'); },

        // ---- Metadata database backup status ----
        // The database is the index: it says which archive holds what. If copies
        // of it stop reaching a main account, the only place that has ever said
        // so is a log line, and the user finds out during a restore. These read
        // the record the worker now keeps.
        //
        // Never attempted, succeeded and failed are three states, not two. A
        // missing record must not render as a failure (a fresh install has simply
        // not run a job yet) and must not render as a blank that looks like
        // success.
        dbBackup(m) { return (m && m.metadata_backup) || null; },
        dbBackupFailed(m) {
            const b = this.dbBackup(m);
            return !!(b && b.last_attempt_at && !b.ok);
        },
        dbBackupText(m) {
            const b = this.dbBackup(m);
            if (!b || !b.last_attempt_at) return 'Database copy: not attempted yet.';
            if (b.ok) {
                return `Database copy: last sent ${this.formatStamp(b.last_success_at)}`
                    + (b.last_success_name ? ` as ${b.last_success_name}.` : '.');
            }
            const stage = b.stage ? ` while ${b.stage}` : '';
            const why = b.error ? `: ${b.error}` : '.';
            let text = `Database copy FAILED ${this.formatStamp(b.last_attempt_at)}${stage}${why}`;
            if (b.last_success_at) {
                // The copy that did land is still out there and still restorable,
                // and its name is what a recovery needs in order to find it.
                text += ` Last copy that landed: ${this.formatStamp(b.last_success_at)}`
                    + (b.last_success_name ? ` (${b.last_success_name}).` : '.');
            } else {
                text += ' No copy has ever reached this account.';
            }
            return text;
        },
        // Stamps are stored as RFC3339 UTC. Rendering them in the browser's own
        // locale is the point — an hour read off a server-side UTC string is the
        // kind of thing that makes someone think a backup is a day stale.
        formatStamp(s) {
            if (!s) return 'never';
            const d = new Date(s);
            return isNaN(d.getTime()) ? s : d.toLocaleString();
        },
        async startReauth(provider, email, isMain) {
            this.reauthError = '';
            this.reauthRun = { phase: 'starting', provider, email, is_main: !!isMain };
            this.showReauthModal = true;
            try {
                const r = await fetch('/api/reauth/start', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ provider, email, is_main: !!isMain }),
                });
                const body = await r.json().catch(() => ({}));
                if (!r.ok) {
                    this.reauthError = body.error || 'Could not start re-authorization.';
                    this.reauthRun = { phase: 'error', provider, email };
                    return;
                }
                this.reauthRun = body;
            } catch (e) {
                this.reauthError = String(e);
                this.reauthRun = { phase: 'error', provider, email };
            } finally {
                // The run creates work the poll should watch, so re-arm now rather
                // than waiting out an already-armed idle timer.
                this.scheduleRefresh();
            }
        },
        async loadReauth() {
            const r = await fetch('/api/reauth');
            if (!r.ok) return;
            const before = this.reauthRun && this.reauthRun.phase;
            this.reauthRun = await r.json() || { phase: 'idle' };
            // A finished run changes stored credentials, so the account cards must
            // be re-read to drop the "needs re-authorization" flag.
            if (before !== 'done' && this.reauthRun.phase === 'done') {
                await Promise.all([this.loadAccounts(), this.loadMainAccounts()]);
            }
        },
        async cancelReauth() {
            await fetch('/api/reauth/cancel', { method: 'POST' }).catch(() => {});
            await this.loadReauth();
        },
        closeReauthModal() {
            // Closing while the flow is still waiting would leave the callback
            // listener bound with nothing watching it, so closing cancels.
            if (this.reauthInFlight()) this.cancelReauth();
            this.showReauthModal = false;
        },
        reauthInFlight() {
            const p = this.reauthRun && this.reauthRun.phase;
            return p === 'starting' || p === 'awaiting' || p === 'exchanging';
        },
        hasActiveJobs() {
            return this.users.some(u => (u.jobs || []).some(j => j.status === 'pending' || j.status === 'in_progress'));
        },
        // An Auto-Sync crawl runs for minutes and reports progress through the same
        // tick, but creates no upload job — so it has to opt into the fast cadence
        // explicitly or its modal would update at the idle rate.
        autoSyncRunning() {
            const p = this.autoSyncRun && this.autoSyncRun.phase;
            return this.showAutoSyncModal && (p === 'previewing' || p === 'applying');
        },
        // A waiting re-authorization produces no job either, and the user is
        // staring at the modal for the moment it completes — so it opts into the
        // fast cadence the same way the crawl does.
        needsFastPoll() { return this.hasActiveJobs() || this.autoSyncRunning() || this.reauthInFlight(); },

        // ---- Header totals ----
        activeAccountsCount() { return this.accounts.length; },
        totalUsedGB() {
            return this.accounts.reduce((s, a) => s + (a.quota_used_gb || 0), 0).toFixed(2);
        },
        totalFreeGB() {
            return this.accounts.reduce((s, a) => s + Math.max(0, (a.quota_total_gb || 0) - (a.quota_used_gb || 0)), 0).toFixed(2);
        },

        // ---- Filtering (real-time, client-side) ----
        // Folded to match the server's accent-insensitive rules, so typing
        // "conteudo" filters the table the same way the ▶ global search finds it.
        filteredUsers() {
            const q = fold(this.search);
            if (!q) return this.users;
            return this.users.filter(u => {
                if (fold(u.email).includes(q)) return true;
                if (u.backup && fold(u.backup.title || '').includes(q)) return true;
                return (u.zips || []).some(z =>
                    fold(z.name || '').includes(q) || treeNames(z).includes(q));
            });
        },

        // ---- Per-user / per-provider helpers ----
        userAccount(u, provider) { return (u.accounts || []).find(a => a.provider === provider) || null; },
        // Every job ever run against this provider, superseded ones included. The
        // logs and the bulk Download/Delete actions want the full history; status
        // questions go through effectiveJobs instead.
        providerJobs(u, provider) { return (u.jobs || []).filter(j => j.provider === provider); },
        completedJobs(u, provider) { return this.providerJobs(u, provider).filter(j => j.status === 'complete'); },
        // The newest job per archive on this provider's account — the only ones
        // whose outcome still describes what the cloud holds.
        effectiveJobs(u, provider) {
            const { ids } = effectiveIndex(u.jobs);
            return this.providerJobs(u, provider).filter(j => ids.has(j.id));
        },
        // Activity first: a running upload is what the user needs to see, and an
        // old failure must not hide it. Only then does an unresolved failure win
        // over success — and "unresolved" means the effective job failed, so a
        // failure that a later upload of the same archive fixed no longer counts.
        providerStatus(u, provider) {
            const jobs = this.effectiveJobs(u, provider);
            if (jobs.length === 0) return null;
            if (jobs.some(j => j.status === 'in_progress')) return 'in_progress';
            if (jobs.some(j => j.status === 'pending')) return 'pending';
            if (jobs.some(j => j.status === 'failed')) return 'failed';
            return 'complete';
        },
        providerDisplayStatus(u, provider) {
            const s = this.providerStatus(u, provider);
            if (s === 'in_progress' && this.providerVerifying(u, provider)) return 'verifying';
            // A job the worker is deliberately not claiming reads as "pending" and
            // would otherwise be indistinguishable from a stuck one.
            if (s === 'pending' && this.providerHold(u, provider)) return 'waiting';
            return s;
        },
        // The hold on this provider's pending jobs, if any. The reason is stored on
        // the job so it survives a restart and explains itself without the user
        // having to open a log.
        providerHold(u, provider) {
            const held = this.providerJobs(u, provider).find(j => this.jobHoldReason(j));
            return held ? held.hold_reason : '';
        },
        // Progress of the work still to do, not of everything this account ever
        // received. Summing history made a new upload start near 100%.
        providerProgress(u, provider) {
            const jobs = this.providerJobs(u, provider).filter(isActiveJob);
            if (jobs.length === 0) return null;
            let up = 0, tot = 0;
            for (const j of jobs) { up += j.uploaded_bytes || 0; tot += j.total_bytes || 0; }
            if (tot === 0) return 0;
            return Math.min(100, Math.round((up / tot) * 100));
        },
        // Every running upload on this provider has sent all its bytes and is in
        // the post-upload checksum. Judged on the running jobs alone: a queued
        // sibling volume sits at 0 bytes, so a sum over every active job would
        // keep the label on In Progress through each verification of a split
        // backup — the label would almost never appear.
        providerVerifying(u, provider) {
            const running = this.providerJobs(u, provider).filter(j => j.status === 'in_progress');
            return running.length > 0 && running.every(j => this.jobVerifying(j));
        },
        // "3 of 40 archives failed" for the card, or '' when nothing needs
        // attention. Counted over effective jobs, so it is the number of archives
        // this account is actually missing.
        providerFailureNote(u, provider) {
            const jobs = this.effectiveJobs(u, provider);
            const failed = jobs.filter(j => j.status === 'failed').length;
            if (!failed) return '';
            return `${failed} of ${jobs.length} archive${jobs.length === 1 ? '' : 's'} failed`;
        },
        // The vocabulary every status chip uses. The raw values are database
        // states; nobody should have to read "in_progress" in a product.
        statusLabel(s) {
            const labels = {
                pending: 'Pending',
                waiting: 'Waiting',
                in_progress: 'In Progress',
                verifying: 'Verifying',
                complete: 'Complete',
                failed: 'Failed',
                replaced: 'Replaced',
                none: 'No Uploads',
            };
            return labels[s || 'none'] || s;
        },
        providerName(p) { return providerName(p); },

        // ---- Record-level status / totals ----
        // Same rules as the provider chip: Syncing while anything is running,
        // otherwise Unsynced only for a failure no later upload has fixed.
        recordStatus(u) {
            const jobs = u.jobs || [];
            if (jobs.length === 0) return null;
            if (jobs.some(isActiveJob)) return 'syncing';
            const { ids } = effectiveIndex(u.jobs);
            if (jobs.some(j => ids.has(j.id) && j.status === 'failed')) return 'unsynced';
            return 'synced';
        },
        recordStatusLabel(u) {
            const s = this.recordStatus(u);
            return s === 'synced' ? 'Synced' : s === 'unsynced' ? 'Unsynced' : s === 'syncing' ? 'Syncing…' : '';
        },
        // Same-named zip rows are one archive (see archiveGroups), so each is
        // counted once, at the size the tree shows for it.
        storedBytes(u) {
            let b = 0;
            for (const g of this.archiveGroups(u)) b += g.rep.size_bytes || 0;
            return b;
        },
        storedGB(u) { return (this.storedBytes(u) / 1073741824).toFixed(2); },
        accountUsedGB(a) { return (a ? a.quota_used_gb || 0 : 0); },
        accountTotalGB(a) { return (a ? a.quota_total_gb || 0 : 0); },
        accountPct(a) {
            if (!a || !a.quota_total_gb) return 0;
            return Math.min(100, Math.round((a.quota_used_gb / a.quota_total_gb) * 100));
        },
        recordCreated(u) { return u.backup ? new Date(u.backup.created_at).toLocaleDateString() : ''; },

        // ---- Table footer totals ----
        totalRecords() { return this.users.filter(u => u.backup).length; },
        grandTotalGB() {
            let b = 0;
            for (const u of this.users) b += this.storedBytes(u);
            return (b / 1073741824).toFixed(2);
        },

        // ---- Expansion ----
        _toggle(arr, key) { const i = arr.indexOf(key); if (i === -1) arr.push(key); else arr.splice(i, 1); },
        isAccountsOpen(u) { return this.expandedAccounts.includes(u.email); },
        isFilesOpen(u) { return this.expandedFiles.includes(u.email); },
        toggleAccounts(u) { this._toggle(this.expandedAccounts, u.email); },
        toggleFiles(u) { this._toggle(this.expandedFiles, u.email); },
        toggleExpandAll() {
            this.allExpanded = !this.allExpanded;
            if (this.allExpanded) {
                this.expandedAccounts = this.users.map(u => u.email);
                this.expandedFiles = this.users.map(u => u.email);
            } else {
                this.expandedAccounts = [];
                this.expandedFiles = [];
            }
        },
        isProviderOpen(p) { return this.expandedProviders.includes(p); },
        toggleProvider(p) { this._toggle(this.expandedProviders, p); },

        // ---- Accounts page ----
        megaAccounts() { return this.accounts.filter(a => a.provider === 'mega'); },
        foursharedAccounts() { return this.accounts.filter(a => a.provider === 'fourshared'); },
        lastSynced(a) {
            if (!a || !a.last_quota_sync) return 'never';
            return new Date(a.last_quota_sync).toLocaleString();
        },

        // ---- Account tier and transfer budget ----
        // Changing a tier changes which size cap and transfer budget apply, so the
        // account list is reloaded to show the new figures immediately.
        async setAccountTier(a, tier) {
            if (!a || a.tier === tier) return;
            const previous = a.tier;
            a.tier = tier; // optimistic, so the select does not snap back while saving
            try {
                const r = await fetch(`/api/accounts/${a.id}/tier`, {
                    method: 'PUT',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ tier }),
                });
                if (!r.ok) {
                    a.tier = previous;
                    const e = await r.json().catch(() => ({}));
                    this.error = e.error || 'Could not change the account tier';
                    return;
                }
                await this.loadAccounts();
            } catch (e) {
                a.tier = previous;
                this.error = 'Could not change the account tier';
            }
        },
        // What this account's tier means, in one line for the account card.
        accountLimitLabel(a) {
            if (!a) return '';
            const cap = a.max_file_bytes > 0
                ? `archives split at ${this.formatLimit(a.max_file_bytes)}`
                : 'no file size limit';
            if (!a.transfer_budget_bytes) return `${cap} · unlimited transfer`;
            const left = Math.max(0, a.transfer_budget_bytes - (a.transfer_used_bytes || 0));
            return `${cap} · ${this.formatLimit(left)} of ${this.formatLimit(a.transfer_budget_bytes)} left this ${a.transfer_window_hours}h`;
        },
        // Fraction of the transfer window consumed, for a meter.
        transferPercent(a) {
            if (!a || !a.transfer_budget_bytes) return 0;
            return Math.min(100, Math.round(((a.transfer_used_bytes || 0) / a.transfer_budget_bytes) * 100));
        },
        // A pending job the worker is deliberately not claiming. Distinguishing
        // this from a stuck job is the whole reason the reason is stored.
        jobHoldReason(j) {
            return j && j.status === 'pending' && j.hold_reason ? j.hold_reason : '';
        },
        heldJobs(u) {
            return (u.jobs || []).filter(j => this.jobHoldReason(j));
        },
        async refreshQuotas() {
            this.refreshingQuotas = true;
            try {
                const r = await fetch('/api/accounts/quota-sync', { method: 'POST' });
                if (r.ok) this.accounts = await r.json() || [];
                else await this.loadAccounts();
            } finally {
                this.refreshingQuotas = false;
            }
        },

        // ---- Upload / Edit ----
        openUpload(u) {
            this.error = '';
            // A preview describes one directory for one set of accounts. Carrying
            // it into a different user's modal would show them someone else's plan.
            this.preflight = null;
            this.preflightError = '';
            this.uploadForm = {
                owner_email: u.email,
                backup_id: u.backup ? u.backup.id : null,
                title: u.backup ? (u.backup.title || '') : '',
                source_path: '',
                account_ids: (u.accounts || []).map(a => a.id),
                accounts: u.accounts || [],
            };
            this.showUploadModal = true;
        },
        async browsePath() {
            this.browseLoading = true;
            try {
                const r = await fetch('/api/browse');
                if (!r.ok) return;
                const { path } = await r.json();
                if (path) {
                    // Setting x-model in code fires no DOM input event, so the
                    // input's own @input handler will not clear the preview here.
                    this.preflight = null;
                    this.preflightError = '';
                    this.uploadForm.source_path = path;
                    // Default the title to the selected folder name (the zip name) when
                    // the user hasn't already given one.
                    if (!this.uploadForm.title) {
                        this.uploadForm.title = path.replace(/[\\/]+$/, '').split(/[\\/]/).pop();
                    }
                }
            } finally {
                this.browseLoading = false;
            }
        },
        toggleUploadAccount(id) {
            this._toggle(this.uploadForm.account_ids, id);
            // The plan depends on which accounts are selected, so a stale preview
            // would describe a different upload than the one about to run.
            this.preflight = null;
            this.preflightError = '';
        },

        // ---- Pre-flight ----
        // Asks the server what the upload would produce before anything is
        // compressed. This is where an impossible upload is refused: the same
        // planner runs here and in POST /api/backups, so the preview cannot
        // disagree with what actually happens.
        async runPreflight() {
            const path = (this.uploadForm.source_path || '').trim();
            if (!path || !this.uploadForm.account_ids.length) {
                this.preflight = null;
                return;
            }
            this.preflightLoading = true;
            this.preflightError = '';
            try {
                const r = await fetch('/api/backups/preflight', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({
                        source_path: path,
                        account_ids: this.uploadForm.account_ids,
                    }),
                });
                if (!r.ok) {
                    const e = await r.json().catch(() => ({}));
                    this.preflightError = e.error || 'Could not check this backup';
                    this.preflight = null;
                    return;
                }
                this.preflight = await r.json();
            } catch (e) {
                // A failed check must not stop the user uploading: the server runs
                // the same checks again and refuses there if it has to.
                this.preflightError = 'Could not check this backup';
                this.preflight = null;
            } finally {
                this.preflightLoading = false;
            }
        },
        // A one-line summary of what a group of accounts receives.
        planGroupSummary(g) {
            const n = (g.volumes || []).length;
            const who = (g.accounts || []).map(a => `${a.provider} — ${a.email}`).join(', ');
            // Byte parts are named and counted like volumes but are not archives,
            // so the summary must not call them one.
            if (g.byte_parts) return `${who}: split into ${n} numbered parts`;
            if (n <= 1) return `${who}: one archive`;
            return `${who}: split into ${n} archives`;
        },
        // With no directory chosen there is nothing to zip, so an existing record can
        // only be renamed. Drives both the submit label and the branch in submitUpload.
        isTitleOnly() {
            return !(this.uploadForm.source_path || '').trim() && !!this.uploadForm.backup_id;
        },
        async saveTitle() {
            this.uploading = true;
            this.error = '';
            try {
                const r = await fetch(`/api/backups/${this.uploadForm.backup_id}`, {
                    method: 'PATCH',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ title: this.uploadForm.title.trim() }),
                });
                if (!r.ok) {
                    const e = await r.json().catch(() => ({}));
                    this.error = e.error || 'Could not save the title';
                    return;
                }
                this.showUploadModal = false;
                await this.loadUsers();
            } finally {
                this.uploading = false;
            }
        },
        async submitUpload(resolutions) {
            if (!(this.uploadForm.source_path || '').trim()) {
                if (!this.uploadForm.backup_id) {
                    this.error = 'Choose a directory to compress and back up.';
                    return;
                }
                if (!this.uploadForm.title.trim()) {
                    this.error = 'Enter a title, or choose a directory to upload.';
                    return;
                }
                await this.saveTitle();
                return;
            }
            this.uploading = true;
            this.error = '';
            try {
                const body = {
                    owner_email: this.uploadForm.owner_email,
                    title: this.uploadForm.title,
                    source_path: this.uploadForm.source_path,
                    account_ids: this.uploadForm.account_ids,
                };
                if (resolutions) body.conflict_resolutions = resolutions;
                const r = await fetch('/api/backups', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify(body),
                });
                if (r.status === 409) {
                    const e = await r.json();
                    if (e.conflicts && e.conflicts.length) {
                        this.conflicts = e.conflicts;
                        this.conflictChoices = {};
                        for (const c of e.conflicts) this.conflictChoices[c.account_id] = 'overwrite';
                        this.showConflictModal = true;
                        return;
                    }
                    // A blocked plan: a file too large to split, no quota, or an
                    // archive bigger than the whole transfer budget. Each blocker
                    // names its own cause, so show them rather than a generic line.
                    if (e.blockers && e.blockers.length) {
                        this.error = e.blockers.map(b => b.message).join(' ');
                        return;
                    }
                    this.error = e.error || 'Not enough space';
                    return;
                }
                if (!r.ok) {
                    const e = await r.json().catch(() => ({}));
                    this.error = e.error || 'Upload failed';
                    return;
                }
                this.showUploadModal = false;
                this.showConflictModal = false;
                await this.loadUsers();
                // Jobs now exist, so switch to the fast cadence straight away rather
                // than waiting out the idle timer that is already armed.
                this.scheduleRefresh();
            } finally {
                this.uploading = false;
            }
        },
        async submitConflicts() {
            this.showConflictModal = false;
            await this.submitUpload(this.conflictChoices);
        },

        // ---- Downloads ----
        // One link per provider holding a completed copy of the archive, each
        // pointing at that provider's newest completed job. The group can span
        // several zip rows (a re-upload to one provider appends a row), so the
        // lookup takes every zip id in the group, not just the representative's.
        archiveDownloads(u, zipIds) {
            const out = [];
            for (const provider of ['mega', 'fourshared']) {
                let best = null;
                for (const j of (u.jobs || [])) {
                    if (j.provider !== provider || j.status !== 'complete' || !zipIds.has(j.zip_id)) continue;
                    if (!best || j.id > best.id) best = j;
                }
                if (best) out.push({ provider, label: providerName(provider), job: best });
            }
            return out;
        },
        downloadAllLabel(u, provider) {
            const n = this.completedJobs(u, provider).length;
            return n > 1 ? `Download All (${n})` : 'Download';
        },
        // Downloads every zip stored on this account. There is no server-side
        // bundle endpoint, so this triggers one download per file — browsers ask
        // once for permission to download multiple files, then handle the rest.
        // Spaced slightly apart because a tight loop of navigations gets some of
        // them dropped.
        downloadAll(u, provider) {
            const jobs = this.completedJobs(u, provider);
            jobs.forEach((j, i) => {
                setTimeout(() => {
                    const a = document.createElement('a');
                    a.href = `/api/jobs/${j.id}/download`;
                    a.download = j.remote_name || '';
                    document.body.appendChild(a);
                    a.click();
                    a.remove();
                }, i * 800);
            });
        },

        // ---- Logs ----
        async openLogs(u, provider) {
            const jobs = this.providerJobs(u, provider);
            this.logsTitle = `${provider} logs — ${u.email}`;
            this.logs = [];
            this.showLogsModal = true;
            for (const j of jobs) {
                const r = await fetch(`/api/jobs/${j.id}/logs`);
                if (!r.ok) continue;
                const lines = await r.json() || [];
                for (const l of lines) this.logs.push({ ...l, email: j.email });
            }
            this.logs.sort((a, b) => new Date(a.created_at) - new Date(b.created_at));
        },

        // One job's log lines, for the Jobs view. Same modal as the per-provider
        // logs, so the two read alike.
        async openJobLogs(j) {
            this.logsTitle = `${providerName(j.provider)} logs — ${j.remote_name || 'job #' + j.id} (job #${j.id})`;
            this.logs = [];
            this.showLogsModal = true;
            const r = await fetch(`/api/jobs/${j.id}/logs`).catch(() => null);
            if (!r || !r.ok) return;
            const lines = (await r.json().catch(() => [])) || [];
            this.logs = lines.map(l => ({ ...l, email: j.email }));
        },

        // ---- Deletes ----
        // The modal has two states. It opens on the confirm step; if the server
        // answers 409 it switches to the failure step, listing every archive that
        // could not be removed and (for a record) offering to drop the local record
        // anyway. Nothing local has been deleted at that point — the record is only
        // removed once the user takes the second step.
        openDeleteRecord(u, withFiles) {
            if (!u.backup) return;
            this.resetDeleteState();
            this.deleteTarget = {
                kind: withFiles ? 'record-files' : 'record',
                backupId: u.backup.id,
                label: u.backup.title || u.email,
                requireType: withFiles,
            };
            this.showDeleteModal = true;
        },
        openDeleteProvider(u, provider) {
            const ids = this.completedJobs(u, provider).map(j => j.id);
            if (ids.length === 0) return;
            this.resetDeleteState();
            this.deleteTarget = { kind: 'provider', jobIds: ids, label: `${provider} — ${u.email}`, requireType: true };
            this.showDeleteModal = true;
        },
        resetDeleteState() {
            this.error = '';
            this.deleteConfirm = '';
            this.deleteFailures = [];
            this.deletedCount = 0;
        },
        // Closing after a partial failure still needs a refresh: files may well have
        // been deleted from their providers even though the record stayed.
        closeDeleteModal() {
            const touched = this.deleteFailures.length > 0 || this.deletedCount > 0;
            this.showDeleteModal = false;
            this.deleteTarget = null;
            this.deleteFailures = [];
            this.deletedCount = 0;
            // `error` is shared with the upload and conflict modals; leaving a stale
            // delete error behind would surface it in whichever opens next.
            this.error = '';
            if (touched) this.loadUsers();
        },
        deleteMessage() {
            const t = this.deleteTarget;
            if (!t) return '';
            if (t.kind === 'record-files') return `This deletes the record “${t.label}” and every uploaded file from its provider(s). Type DELETE to confirm.`;
            if (t.kind === 'provider') return `This deletes all files for ${t.label} from that provider. Type DELETE to confirm.`;
            return `This removes the record “${t.label}” locally. Uploaded files are left on their providers.`;
        },
        // Headline for the failure step, and the sentence spelling out what forcing
        // would leave behind — the user has to be told before taking that step.
        deleteFailureSummary() {
            const n = this.deleteFailures.length;
            const files = n === 1 ? 'file' : 'files';
            const done = this.deletedCount > 0 ? ` ${this.deletedCount} other ${this.deletedCount === 1 ? 'file was' : 'files were'} deleted.` : '';
            // Delete-All on one provider removes jobs, not the record, so it must not
            // talk about a record that was never going to be removed.
            const kept = this.deleteTarget && this.deleteTarget.kind === 'provider'
                ? ` ${n === 1 ? 'It is' : 'They are'} still on the account.`
                : ' The record has not been removed.';
            return `${n} ${files} could not be deleted from ${n === 1 ? 'its provider' : 'their providers'}.${done}${kept}`;
        },
        forceDeleteMessage() {
            const n = this.deleteFailures.length;
            const accounts = [...new Set(this.deleteFailures.map(f => `${f.provider} — ${f.email}`))];
            return `Remove the record anyway? ${n} ${n === 1 ? 'file stays' : 'files stay'} on ${accounts.join(', ')}. ` +
                `Nothing is deleted from the provider — you would have to remove ${n === 1 ? 'it' : 'them'} there yourself.`;
        },
        // Whether the force step applies at all. Deliberately NOT gated on `deleting`:
        // it drives x-show, and hiding the button (and its explanation) the moment it
        // is clicked would leave the modal blank mid-request. The button is disabled
        // instead.
        canForceDelete() {
            return !!this.deleteTarget && this.deleteTarget.kind === 'record-files' && this.deleteFailures.length > 0;
        },
        canConfirmDelete() {
            if (!this.deleteTarget || this.deleting) return false;
            if (this.deleteFailures.length > 0) return false;
            if (this.deleteTarget.requireType && this.deleteConfirm !== 'DELETE') return false;
            return true;
        },
        async confirmDelete() {
            if (!this.canConfirmDelete()) return;
            this.deleting = true;
            this.error = '';
            try {
                const t = this.deleteTarget;
                if (t.kind === 'provider') {
                    // Same rule as the server's record delete: attempt every job rather
                    // than stopping at the first one that fails, so one unreachable
                    // account cannot block the copies that are reachable.
                    const failures = [];
                    let deleted = 0;
                    for (const id of t.jobIds) {
                        const r = await fetch(`/api/jobs/${id}`, {
                            method: 'DELETE',
                            headers: { 'Content-Type': 'application/json' },
                            body: JSON.stringify({ confirm: 'DELETE' }),
                        });
                        if (r.ok) { deleted++; continue; }
                        const e = await r.json().catch(() => ({}));
                        if (e.failures && e.failures.length) failures.push(...e.failures);
                        else failures.push({ provider: t.label, email: '', archive: `job #${id}`, message: e.error || 'Failed to delete' });
                    }
                    if (failures.length) { this.deletedCount = deleted; this.deleteFailures = failures; return; }
                } else {
                    const r = await fetch(`/api/backups/${t.backupId}`, {
                        method: 'DELETE',
                        headers: { 'Content-Type': 'application/json' },
                        body: JSON.stringify({ confirm: this.deleteConfirm, delete_files: t.kind === 'record-files' }),
                    });
                    if (!r.ok) {
                        const e = await r.json().catch(() => ({}));
                        if (r.status === 409 && e.failures && e.failures.length) {
                            this.deletedCount = e.deleted || 0;
                            this.deleteFailures = e.failures;
                            return;
                        }
                        this.error = e.error || 'Failed to delete';
                        return;
                    }
                }
                this.showDeleteModal = false;
                this.deleteTarget = null;
                this.deleteFailures = [];
                this.deletedCount = 0;
                await this.loadUsers();
            } finally {
                this.deleting = false;
            }
        },
        // Second step: drop the local record while the listed files stay in the
        // cloud. The server deletes nothing remote on this call, so the UI must not
        // claim otherwise.
        async forceDeleteRecord() {
            if (!this.canForceDelete() || this.deleting) return;
            this.deleting = true;
            this.error = '';
            try {
                const r = await fetch(`/api/backups/${this.deleteTarget.backupId}`, {
                    method: 'DELETE',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ confirm: 'DELETE', delete_files: true, force: true }),
                });
                if (!r.ok) { const e = await r.json().catch(() => ({})); this.error = e.error || 'Failed to remove the record'; return; }
                this.showDeleteModal = false;
                this.deleteTarget = null;
                this.deleteFailures = [];
                this.deletedCount = 0;
                await this.loadUsers();
            } finally {
                this.deleting = false;
            }
        },

        // ---- Files tree ----
        // ONE tree per record, with one top-level node per *archive*. A record can
        // hold several zip rows with the same name — the directory reached MEGA,
        // failed on 4shared, and was re-uploaded to 4shared alone, which appends a
        // row — and those rows are one archive in the cloud, so they render as
        // one node carrying a download link per provider that holds a copy.
        //
        // The node's tree and size come from the group's representative: the
        // newest row with a recorded tree, falling back to the newest row. Newest
        // because a re-upload of a changed directory records the current state.
        archiveGroups(u) {
            const byName = new Map();
            for (const z of (u.zips || [])) {
                const name = z.name || '(unnamed)';
                if (!byName.has(name)) byName.set(name, []);
                byName.get(name).push(z);
            }
            const recordKey = u.backup ? u.backup.id : u.email;
            const groups = [];
            for (const [name, zips] of byName) {
                const newestFirst = [...zips].sort((a, b) => b.id - a.id);
                const rep = newestFirst.find(z => parseTreeJSON(z)) || newestFirst[0];
                groups.push({
                    key: `${recordKey}/${name}`,
                    name,
                    rep,
                    root: parseTreeJSON(rep),
                    zipIds: new Set(zips.map(z => z.id)),
                });
            }
            groups.sort((a, b) => naturalCompare(a.name, b.name));
            return groups;
        },
        // The zip node replaces the parsed tree's root — they denote the same
        // directory — so the root's *children* are emitted at depth 1 rather than
        // repeating the same name twice.
        //
        // Alpine cannot recurse with x-for, so the result is flattened to the rows
        // that are currently VISIBLE (collapsed subtrees are skipped) and rendered by
        // a single x-for, with depth expressed as left padding.
        recordTreeRows(u) {
            const rows = [];
            for (const g of this.archiveGroups(u)) {
                const children = g.root ? (g.root.children || []) : [];
                // The archive node's path is empty: it stands in for the tree root,
                // so it inherits the root's "open by default" behaviour via depth 0.
                const zipOpen = this.isNodeOpen(g.key, '', 0);
                rows.push({
                    key: this.nodeKey(g.key, ''),
                    isZip: true,
                    groupKey: g.key,
                    path: '',
                    name: g.name,
                    depth: 0,
                    size: g.rep.size_bytes || 0,
                    hasChildren: children.length > 0,
                    // Distinct from hasChildren: an archive whose tree was recorded
                    // but is empty (an empty source directory) has a tree, and must
                    // not be labelled as having none.
                    noTree: g.root === null,
                    expanded: zipOpen,
                    downloads: this.archiveDownloads(u, g.zipIds),
                });
                if (!zipOpen) continue;
                const walk = (node, parentPath, depth) => {
                    const name = node.name || '(unnamed)';
                    const path = parentPath ? `${parentPath}/${name}` : name;
                    const kids = node.children || [];
                    const open = this.isNodeOpen(g.key, path, depth);
                    rows.push({
                        key: this.nodeKey(g.key, path),
                        isZip: false,
                        groupKey: g.key,
                        path, name, depth,
                        size: node.size_bytes || 0,
                        hasChildren: kids.length > 0,
                        expanded: open,
                        downloads: [],
                    });
                    if (kids.length && open) for (const c of kids) walk(c, path, depth + 1);
                };
                for (const c of children) walk(c, '', 1);
            }
            return rows;
        },
        // groupKey is `${backupId}/${archiveName}`, so a key reads
        // `${backupId}/${archiveName}:${path}`.
        nodeKey(groupKey, path) { return `${groupKey}:${path}`; },
        // Default state: root open, everything below closed. An explicit entry in
        // either list overrides that default.
        isNodeOpen(groupKey, path, depth) {
            const k = this.nodeKey(groupKey, path);
            if (this.expandedNodes.includes(k)) return true;
            if (this.collapsedNodes.includes(k)) return false;
            return depth === 0;
        },
        toggleTreeNode(row) {
            const k = this.nodeKey(row.groupKey, row.path);
            const open = row.expanded;
            this.expandedNodes = this.expandedNodes.filter(x => x !== k);
            this.collapsedNodes = this.collapsedNodes.filter(x => x !== k);
            if (open) this.collapsedNodes.push(k);
            else this.expandedNodes.push(k);
        },
        // Every node key in the record that has children — the only nodes
        // expand/collapse-all needs to record. Walks the full tree of every
        // archive, not just the visible rows, and includes each archive's own node.
        allRecordNodeKeys(u) {
            const keys = [];
            for (const g of this.archiveGroups(u)) {
                const children = g.root ? (g.root.children || []) : [];
                if (children.length) keys.push(this.nodeKey(g.key, ''));
                const walk = (node, parentPath) => {
                    const name = node.name || '(unnamed)';
                    const path = parentPath ? `${parentPath}/${name}` : name;
                    const kids = node.children || [];
                    if (kids.length) {
                        keys.push(this.nodeKey(g.key, path));
                        for (const c of kids) walk(c, path);
                    }
                };
                for (const c of children) walk(c, '');
            }
            return keys;
        },
        // Whether this record has anything to expand — a record whose archives carry
        // no recorded tree would otherwise show Expand All / Collapse All buttons
        // that silently do nothing. Asked of the representatives, because those are
        // the trees actually rendered.
        hasExpandableTree(u) {
            return this.archiveGroups(u).some(g => !!(g.root && (g.root.children || []).length));
        },
        setRecordTreeOpen(u, open) {
            // A Set, not includes(): this filters both lists once per key, which is
            // quadratic over a record with several large trees.
            const keys = new Set(this.allRecordNodeKeys(u));
            // Drop this record's existing entries from both lists first so the two
            // lists never disagree about the same node.
            this.expandedNodes = this.expandedNodes.filter(k => !keys.has(k));
            this.collapsedNodes = this.collapsedNodes.filter(k => !keys.has(k));
            if (open) this.expandedNodes.push(...keys);
            else this.collapsedNodes.push(...keys);
        },

        // ---- Jobs view ----
        // Every job across every record, derived from the polled `users` payload.
        // Each row carries what the template needs to label it, including whether
        // a later job for the same archive on the same account has replaced it.
        jobRows() {
            const rows = [];
            for (const u of this.users) {
                for (const j of (u.jobs || [])) {
                    const replacedBy = this.supersedingJob(u, j);
                    rows.push({
                        job: j,
                        title: u.backup ? (u.backup.title || '(untitled)') : '(untitled)',
                        owner: u.email,
                        effective: !replacedBy,
                        replacedBy,
                    });
                }
            }
            // Active first — that is what someone opening this view wants to see —
            // then newest first. Ids rise with creation, and unlike created_at they
            // cannot tie.
            rows.sort((a, b) => {
                const aa = isActiveJob(a.job) ? 0 : 1;
                const ba = isActiveJob(b.job) ? 0 : 1;
                return aa !== ba ? aa - ba : b.job.id - a.job.id;
            });
            return rows;
        },
        // The job that replaced j, or null when j is itself the effective one.
        supersedingJob(u, j) {
            const eff = effectiveIndex(u.jobs).byKey.get(jobArchiveKey(j));
            return eff && eff.id !== j.id ? eff : null;
        },
        // A failure no later upload has fixed — the only kind that needs anything
        // from the user, and the only kind Retry is offered for.
        isUnresolvedFailure(row) { return row.effective && row.job.status === 'failed'; },
        jobsCounts() {
            const c = { in_progress: 0, pending: 0, failed: 0, complete: 0 };
            for (const row of this.jobRows()) {
                const s = row.job.status;
                if (s === 'in_progress') c.in_progress++;
                else if (s === 'pending') c.pending++;
                else if (s === 'complete') c.complete++;
                else if (this.isUnresolvedFailure(row)) c.failed++;
            }
            return c;
        },
        // "Failed" means unresolved: a failure a later upload replaced is history,
        // not something to act on, so it shows under All but not here.
        jobsFilters() {
            return [
                { key: 'all', label: 'All' },
                { key: 'active', label: 'Active' },
                { key: 'failed', label: 'Failed' },
                { key: 'complete', label: 'Complete' },
            ];
        },
        filteredJobRows() {
            const rows = this.jobRows();
            switch (this.jobsFilter) {
                case 'active': return rows.filter(r => isActiveJob(r.job));
                case 'failed': return rows.filter(r => this.isUnresolvedFailure(r));
                case 'complete': return rows.filter(r => r.job.status === 'complete');
                default: return rows;
            }
        },
        // The note on a provider card links here, already narrowed to what the
        // note is about.
        openJobsPage(filter) {
            this.jobsFilter = filter || 'all';
            this.page = 'jobs';
        },
        jobVerifying(j) {
            return j.status === 'in_progress' && j.total_bytes > 0 && (j.uploaded_bytes || 0) >= j.total_bytes;
        },
        jobDisplayStatus(row) {
            const j = row.job;
            if (j.status === 'failed' && !row.effective) return 'replaced';
            if (this.jobVerifying(j)) return 'verifying';
            if (this.jobHoldReason(j)) return 'waiting';
            return j.status;
        },
        jobPercent(j) {
            if (!j.total_bytes) return 0;
            return Math.min(100, Math.round(((j.uploaded_bytes || 0) / j.total_bytes) * 100));
        },
        jobBytes(j) {
            return `${this.formatSize(j.uploaded_bytes) || '0 B'} of ${this.formatSize(j.total_bytes) || '0 B'}`;
        },
        jobDetail(j) { return j.error_message || this.jobHoldReason(j) || ''; },
        replacedNote(row) {
            const r = row.replacedBy;
            if (!r) return '';
            return `Replaced by job #${r.id} (${this.statusLabel(r.status).toLowerCase()}).`;
        },
        isRetrying(j) { return this.retryingJobs.includes(j.id); },
        async retryJob(j) {
            if (this.isRetrying(j)) return;
            this.retryingJobs.push(j.id);
            this.jobErrors = { ...this.jobErrors, [j.id]: '' };
            try {
                const r = await fetch(`/api/jobs/${j.id}/retry`, { method: 'POST' });
                if (!r.ok) {
                    const e = await r.json().catch(() => ({}));
                    this.jobErrors = { ...this.jobErrors, [j.id]: e.error || 'Could not retry this job' };
                    return;
                }
                await this.loadUsers();
                // A queued job is work: re-arm now so the fast cadence starts
                // immediately rather than after the idle timer already in flight.
                this.scheduleRefresh();
            } catch (e) {
                this.jobErrors = { ...this.jobErrors, [j.id]: 'Could not retry this job' };
            } finally {
                this.retryingJobs = this.retryingJobs.filter(id => id !== j.id);
            }
        },
        jobError(j) { return this.jobErrors[j.id] || ''; },
        formatSize(b) {
            if (!b) return '';
            const units = ['B', 'KB', 'MB', 'GB', 'TB'];
            let i = 0, n = b;
            while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
            return `${i === 0 ? n : n.toFixed(n < 10 ? 2 : 1)} ${units[i]}`;
        },
        // Decimal units, for anything to do with provider limits. Both providers
        // state their caps in decimal GB, the Settings fields are authored in
        // decimal GB, and the server's own messages render decimal — showing a
        // 1.9 GB threshold back as "1.77 GB" reads as if the setting did not save.
        // The binary formatSize above stays where it always was, on file sizes.
        formatLimit(b) {
            if (!b) return '';
            const units = ['B', 'kB', 'MB', 'GB', 'TB'];
            let i = 0, n = b;
            while (n >= 1000 && i < units.length - 1) { n /= 1000; i++; }
            return `${i === 0 ? n : n.toFixed(2)} ${units[i]}`;
        },

        // ---- Settings (exclude terms and provider limits) ----
        async loadSettings() {
            try {
                const r = await fetch('/api/settings');
                if (!r.ok) return false;
                const s = await r.json().catch(() => ({}));
                this.excludeTerms = s.exclude_terms || [];
                this.providerLimits = s.provider_limits || {};
                this.providerLimitDefaults = s.provider_limit_defaults || {};
                return true;
            } catch (e) {
                return false;
            }
        },
        async openSettings() {
            this.settingsError = '';
            this.newTerm = '';
            // Reload so the modal always opens on the persisted list — this also makes
            // Cancel a genuine discard of anything edited in a previous session.
            this.settingsLoaded = await this.loadSettings();
            this.limitsForm = this.settingsLoaded ? this.limitsToForm(this.providerLimits) : [];
            if (!this.settingsLoaded) {
                // Nothing in the modal reflects what is stored, so saving would
                // overwrite both sections with whatever happens to be in memory —
                // an empty limits table (which the server reads as "use the
                // defaults") and a stale term list. saveSettings refuses instead.
                this.settingsError = 'Could not load the current settings, so nothing can be saved from here. Close this and try again.';
            }
            this.showSettingsModal = true;
        },

        // Flattens the provider/tier table into rows the template can iterate, in
        // the units a person types. Bytes are the storage format; GB is the
        // editing format, and 0 means unlimited in both.
        limitsToForm(set) {
            const rows = [];
            for (const provider of Object.keys(set).sort()) {
                for (const tier of ['free', 'paid']) {
                    const l = (set[provider] || {})[tier];
                    if (!l) continue;
                    rows.push({
                        provider,
                        tier,
                        max_file_gb: this.bytesToGB(l.max_file_bytes),
                        transfer_gb: this.bytesToGB(l.transfer_bytes),
                        window_hours: l.window_hours || 24,
                    });
                }
            }
            return rows;
        },
        // Decimal GB, matching how both providers state their limits (and how the
        // server's own messages render them).
        bytesToGB(b) {
            if (!b) return 0;
            // toPrecision, not a fixed number of decimal places. Rounding to two
            // decimals turned 0.001 GB into 0 on the way back into the form — and 0
            // means unlimited, so reopening Settings and saving silently disabled
            // the very threshold the user had just set. The precision here only
            // trims float noise (1.9000000000000001 back to 1.9).
            return Number((b / 1e9).toPrecision(12));
        },
        gbToBytes(gb) {
            const n = Number(gb);
            if (!isFinite(n) || n <= 0) return 0;
            // A positive value must never round down to zero, because zero is how
            // this field says "no limit". A 1-byte threshold refuses every file,
            // which is wrong but loudly wrong — silently lifting the cap is not.
            return Math.max(1, Math.round(n * 1e9));
        },
        limitsFromForm() {
            const out = {};
            for (const row of this.limitsForm) {
                out[row.provider] = out[row.provider] || {};
                out[row.provider][row.tier] = {
                    max_file_bytes: this.gbToBytes(row.max_file_gb),
                    transfer_bytes: this.gbToBytes(row.transfer_gb),
                    window_hours: Math.max(1, Number(row.window_hours) || 24),
                };
            }
            return out;
        },
        resetLimitsToDefaults() {
            this.limitsForm = this.limitsToForm(this.providerLimitDefaults);
        },
        limitLabel(row) {
            const cap = row.max_file_gb > 0 ? `split at ${row.max_file_gb} GB` : 'no size limit';
            const budget = row.transfer_gb > 0
                ? `${row.transfer_gb} GB per ${row.window_hours}h`
                : 'unlimited transfer';
            return `${cap}, ${budget}`;
        },
        addTerm() {
            const t = this.newTerm.trim();
            if (!t) return;
            if (this.excludeTerms.some(x => x.toLowerCase() === t.toLowerCase())) { this.newTerm = ''; return; }
            this.excludeTerms.push(t);
            this.newTerm = '';
        },
        removeTerm(i) { this.excludeTerms.splice(i, 1); },
        async saveSettings() {
            // Refuse outright rather than writing half a modal. If the load failed,
            // the exclude terms on screen are as unrepresentative of the stored
            // state as the empty limits table is — guarding only the limits would
            // promise more safety than it delivers.
            if (!this.settingsLoaded) {
                this.settingsError = 'Could not load the current settings, so nothing can be saved from here. Close this and try again.';
                return;
            }
            this.settingsSaving = true;
            this.settingsError = '';
            try {
                // The server applies whichever keys are present, so a section with
                // nothing to say is simply omitted and its stored value is left
                // alone.
                const body = { exclude_terms: this.excludeTerms };
                if (this.limitsForm.length) body.provider_limits = this.limitsFromForm();

                const r = await fetch('/api/settings', {
                    method: 'PUT',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify(body),
                });
                if (!r.ok) {
                    const e = await r.json().catch(() => ({}));
                    this.settingsError = e.error || 'Could not save settings';
                    return;
                }
                // Re-render from the response: the server trims blanks and folds
                // duplicates, and clamps limits, so what was saved may differ from
                // what was sent.
                const s = await r.json().catch(() => ({}));
                this.excludeTerms = s.exclude_terms || [];
                this.providerLimits = s.provider_limits || {};
                this.limitsForm = this.limitsToForm(this.providerLimits);
                this.showSettingsModal = false;
                // Thresholds changed, so any account card showing one is now stale.
                await this.loadAccounts();
            } finally {
                this.settingsSaving = false;
            }
        },
        async cancelSettings() {
            this.showSettingsModal = false;
            await this.loadSettings();
        },

        // ---- Auto-Sync (remote crawl) ----
        // Always opens on the warning: this writes to the database from remote
        // state, so the user confirms the intent, then confirms the concrete plan.
        async openAutoSync() {
            this.autoSyncStarted = false;
            this.autoSyncRun = { phase: 'idle', accounts: [] };
            this.showAutoSyncModal = true;
            // A run started earlier is still going server-side even if the modal
            // was closed. Rejoin it rather than showing a warning whose "Scan"
            // button would only be rejected as already-in-progress.
            await this.loadAutoSync();
            const p = this.autoSyncRun.phase;
            if (p === 'previewing' || p === 'applying') this.autoSyncStarted = true;
        },
        closeAutoSync() {
            // A run that is still crawling keeps going server-side; reopening the
            // modal and scanning again picks up wherever it got to.
            this.showAutoSyncModal = false;
        },
        // 'warning' is a client-side step with no server equivalent — it is what we
        // show before the user has started anything in this sitting.
        autoSyncPhase() {
            if (!this.autoSyncStarted) return 'warning';
            return (this.autoSyncRun && this.autoSyncRun.phase) || 'idle';
        },
        async loadAutoSync() {
            const r = await fetch('/api/autosync');
            if (!r.ok) return;
            this.autoSyncRun = await r.json() || { phase: 'idle', accounts: [] };
        },
        async startAutoSyncPreview() {
            const r = await fetch('/api/autosync/preview', { method: 'POST' });
            const body = await r.json().catch(() => ({}));
            if (!r.ok) {
                this.autoSyncRun = { phase: 'failed', accounts: [], error: body.error || 'Could not start the scan' };
                this.autoSyncStarted = true;
                return;
            }
            this.autoSyncRun = body;
            this.autoSyncStarted = true;
            // A crawl is now running: re-arm so its progress reports at the fast
            // cadence instead of the idle one already in flight.
            this.scheduleRefresh();
        },
        async applyAutoSync() {
            const r = await fetch('/api/autosync/apply', { method: 'POST' });
            const body = await r.json().catch(() => ({}));
            if (!r.ok) {
                this.autoSyncRun = { ...this.autoSyncRun, error: body.error || 'Could not apply the changes' };
                return;
            }
            this.autoSyncRun = body;
            this.scheduleRefresh();
        },
        async cancelAutoSync() {
            const r = await fetch('/api/autosync/cancel', { method: 'POST' });
            if (r.ok) this.autoSyncRun = await r.json() || this.autoSyncRun;
        },
        autoSyncProposedCount() {
            return (this.autoSyncRun.accounts || []).reduce((n, a) => n + (a.proposed || []).length, 0);
        },
        autoSyncSummary() {
            const accts = this.autoSyncRun.accounts || [];
            const failed = accts.filter(a => a.error).length;
            const phase = this.autoSyncPhase();
            if (phase === 'applied') {
                const applied = accts.reduce((n, a) => n + (a.applied || 0), 0);
                return `Added ${applied} record${applied === 1 ? '' : 's'} across ${accts.length} account${accts.length === 1 ? '' : 's'}.`;
            }
            if (phase === 'cancelled') return 'Run cancelled. Anything already written is kept; scan again to see what is left.';
            const n = this.autoSyncProposedCount();
            const base = n === 0
                ? 'Nothing to add — every archive found is already recorded.'
                : `${n} archive${n === 1 ? '' : 's'} would be added.`;
            // A failed account is called out here too: "nothing to add" would be a
            // lie if an account could not be read at all.
            return failed ? `${base} ${failed} account${failed === 1 ? '' : 's'} could not be read (see below).` : base;
        },
        syncAccountCounts(a) {
            if (a.error) return 'could not be read';
            const parts = [];
            if ((a.proposed || []).length) parts.push(`${a.proposed.length} to add`);
            if ((a.missing || []).length) parts.push(`${a.missing.length} missing remotely`);
            if ((a.matched || []).length) parts.push(`${a.matched.length} in sync`);
            return parts.length ? parts.join(' · ') : 'nothing found';
        },
        // Outcome vocabulary. The server's action names ('adopt', 'link') are
        // internal jargon — and "link" in a web UI reads as a hyperlink — so the
        // preview never shows them raw. Each outcome gets a plain-language chip
        // and a sentence saying what it will do to the database.
        syncActionLabel(action) {
            return action === 'adopt' ? 'new record' : 'connect account';
        },
        syncActionChipClass(action) {
            return action === 'adopt' ? 'chip-complete' : 'chip-in_progress';
        },
        // The legend, in the order a user meets these: things that change the
        // database first, then the two that never do.
        syncLegend() {
            return [
                {
                    key: 'adopt',
                    label: 'new record',
                    cls: 'chip-complete',
                    text: 'This archive is in your account but not in your database. A new record will be created for it, including its file tree.',
                },
                {
                    key: 'link',
                    label: 'connect account',
                    cls: 'chip-in_progress',
                    text: 'Your database already knows this archive, but not that this account holds a copy. A reference will be added so Download and Delete work here. The existing record and its file tree are left unchanged.',
                },
                {
                    key: 'matched',
                    label: 'in sync',
                    cls: 'chip-none',
                    text: 'Already recorded for this account. Nothing will happen.',
                },
                {
                    key: 'missing',
                    label: 'not found',
                    cls: 'chip-failed',
                    text: 'Your database references a file that is no longer in this account. Nothing will be changed — this is reported for your information only.',
                },
            ];
        },
        showSyncLegend: true,
        toggleSyncLegend() { this.showSyncLegend = !this.showSyncLegend; },

        syncAccountKey(a) { return `${a.provider}|${a.email}`; },
        isSyncAccountOpen(a) { return this.expandedSyncAccounts.includes(this.syncAccountKey(a)); },
        toggleSyncAccount(a) { this._toggle(this.expandedSyncAccounts, this.syncAccountKey(a)); },

        // ---- Global search across every stored tree ----
        async runSearch() {
            const q = this.search.trim();
            if (!q) return;
            this.searching = true;
            this.searchError = '';
            try {
                const r = await fetch(`/api/search?q=${encodeURIComponent(q)}`);
                if (!r.ok) {
                    const e = await r.json().catch(() => ({}));
                    this.searchError = e.error || 'Search failed';
                    this.searchResults = [];
                } else {
                    this.searchResults = await r.json() || [];
                }
                this.searchRan = true;
            } finally {
                this.searching = false;
            }
        },
        clearSearchResults() {
            this.searchResults = [];
            this.searchRan = false;
            this.searchError = '';
        },
    }));
});
