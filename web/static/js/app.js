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
    treeCache.set(zip.id, { raw, tree });
    return tree;
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
        search: '',
        refreshingQuotas: false,
        error: '',

        // Expansion state is keyed by email so it survives the polling refresh.
        expandedAccounts: [],
        expandedFiles: [],
        allExpanded: false,
        // Tree node expansion, keyed by `${zipId}:${nodePath}` for the same reason.
        // Two lists because "open" has a default (root open, rest closed) that a single
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

        // Settings modal (exclude terms).
        showSettingsModal: false,
        excludeTerms: [],
        newTerm: '',
        settingsSaving: false,
        settingsError: '',

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

        async init() {
            await Promise.all([this.loadUsers(), this.loadAccounts(), this.loadSettings()]);
            this.scheduleRefresh();
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
        needsFastPoll() { return this.hasActiveJobs() || this.autoSyncRunning(); },

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
        providerJobs(u, provider) { return (u.jobs || []).filter(j => j.provider === provider); },
        completedJobs(u, provider) { return this.providerJobs(u, provider).filter(j => j.status === 'complete'); },
        providerStatus(u, provider) {
            const jobs = this.providerJobs(u, provider);
            if (jobs.length === 0) return null;
            if (jobs.some(j => j.status === 'failed')) return 'failed';
            if (jobs.some(j => j.status === 'in_progress')) return 'in_progress';
            if (jobs.every(j => j.status === 'complete')) return 'complete';
            return 'pending';
        },
        providerDisplayStatus(u, provider) {
            const s = this.providerStatus(u, provider);
            if (s === 'in_progress' && this.providerProgress(u, provider) >= 100) return 'verifying';
            return s;
        },
        providerProgress(u, provider) {
            const jobs = this.providerJobs(u, provider);
            if (jobs.length === 0) return null;
            let up = 0, tot = 0;
            for (const j of jobs) { up += j.uploaded_bytes || 0; tot += j.total_bytes || 0; }
            if (tot === 0) return 0;
            return Math.min(100, Math.round((up / tot) * 100));
        },

        // ---- Record-level status / totals ----
        // A record is Synced when every job completed, Unsynced if any failed, and
        // Syncing while uploads are still running.
        recordStatus(u) {
            const jobs = u.jobs || [];
            if (jobs.length === 0) return null;
            if (jobs.some(j => j.status === 'failed')) return 'unsynced';
            if (jobs.every(j => j.status === 'complete')) return 'synced';
            return 'syncing';
        },
        recordStatusLabel(u) {
            const s = this.recordStatus(u);
            return s === 'synced' ? 'Synced' : s === 'unsynced' ? 'Unsynced' : s === 'syncing' ? 'Syncing…' : '';
        },
        storedGB(u) {
            let b = 0;
            for (const z of (u.zips || [])) b += z.size_bytes || 0;
            return (b / 1073741824).toFixed(2);
        },
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
            for (const u of this.users) for (const z of (u.zips || [])) b += z.size_bytes || 0;
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
        toggleUploadAccount(id) { this._toggle(this.uploadForm.account_ids, id); },
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
        // The completed job holding this zip, preferring one on an account the row
        // is showing. A zip can be stored on several accounts; any completed copy
        // is the same file, so the first one is fine.
        zipDownloadJob(u, zip) {
            return (u.jobs || []).find(j => j.zip_id === zip.id && j.status === 'complete') || null;
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
        // ONE tree per record. Each zip contributes a depth-0 node — the archive
        // itself, carrying its own download link — and that zip's directories nest
        // beneath it. The parsed tree's root IS the archive (same directory), so its
        // *children* are emitted at depth 1 rather than repeating the same name twice.
        //
        // Alpine cannot recurse with x-for, so the result is flattened to the rows
        // that are currently VISIBLE (collapsed subtrees are skipped) and rendered by
        // a single x-for, with depth expressed as left padding.
        recordTreeRows(u) {
            const rows = [];
            for (const z of (u.zips || [])) {
                const root = parseTreeJSON(z);
                const children = root ? (root.children || []) : [];
                // The zip node's path is empty: it stands in for the tree root, so it
                // inherits the root's "open by default" behaviour via depth 0.
                const zipOpen = this.isNodeOpen(z.id, '', 0);
                rows.push({
                    key: this.nodeKey(z.id, ''),
                    isZip: true,
                    zipId: z.id,
                    path: '',
                    name: z.name || '(unnamed)',
                    depth: 0,
                    size: z.size_bytes || 0,
                    hasChildren: children.length > 0,
                    // Distinct from hasChildren: an archive whose tree was recorded
                    // but is empty (an empty source directory) has a tree, and must
                    // not be labelled as having none.
                    noTree: root === null,
                    expanded: zipOpen,
                    downloadJob: this.zipDownloadJob(u, z),
                });
                if (!zipOpen) continue;
                const walk = (node, parentPath, depth) => {
                    const name = node.name || '(unnamed)';
                    const path = parentPath ? `${parentPath}/${name}` : name;
                    const kids = node.children || [];
                    const open = this.isNodeOpen(z.id, path, depth);
                    rows.push({
                        key: this.nodeKey(z.id, path),
                        isZip: false,
                        zipId: z.id,
                        path, name, depth,
                        size: node.size_bytes || 0,
                        hasChildren: kids.length > 0,
                        expanded: open,
                        downloadJob: null,
                    });
                    if (kids.length && open) for (const c of kids) walk(c, path, depth + 1);
                };
                for (const c of children) walk(c, '', 1);
            }
            return rows;
        },
        nodeKey(zipId, path) { return `${zipId}:${path}`; },
        // Default state: root open, everything below closed. An explicit entry in
        // either list overrides that default.
        isNodeOpen(zipId, path, depth) {
            const k = this.nodeKey(zipId, path);
            if (this.expandedNodes.includes(k)) return true;
            if (this.collapsedNodes.includes(k)) return false;
            return depth === 0;
        },
        toggleTreeNode(row) {
            const k = this.nodeKey(row.zipId, row.path);
            const open = row.expanded;
            this.expandedNodes = this.expandedNodes.filter(x => x !== k);
            this.collapsedNodes = this.collapsedNodes.filter(x => x !== k);
            if (open) this.collapsedNodes.push(k);
            else this.expandedNodes.push(k);
        },
        // Every node key in the record that has children — the only nodes
        // expand/collapse-all needs to record. Walks the full tree of every zip,
        // not just the visible rows, and includes each zip's own node.
        allRecordNodeKeys(u) {
            const keys = [];
            for (const z of (u.zips || [])) {
                const root = parseTreeJSON(z);
                const children = root ? (root.children || []) : [];
                if (children.length) keys.push(this.nodeKey(z.id, ''));
                const walk = (node, parentPath) => {
                    const name = node.name || '(unnamed)';
                    const path = parentPath ? `${parentPath}/${name}` : name;
                    const kids = node.children || [];
                    if (kids.length) {
                        keys.push(this.nodeKey(z.id, path));
                        for (const c of kids) walk(c, path);
                    }
                };
                for (const c of children) walk(c, '');
            }
            return keys;
        },
        // Whether this record has anything to expand — a record whose zips carry no
        // recorded tree would otherwise show Expand All / Collapse All buttons that
        // silently do nothing.
        hasExpandableTree(u) {
            return (u.zips || []).some(z => {
                const root = parseTreeJSON(z);
                return !!(root && (root.children || []).length);
            });
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
        formatSize(b) {
            if (!b) return '';
            const units = ['B', 'KB', 'MB', 'GB', 'TB'];
            let i = 0, n = b;
            while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
            return `${i === 0 ? n : n.toFixed(n < 10 ? 2 : 1)} ${units[i]}`;
        },

        // ---- Settings (exclude terms) ----
        async loadSettings() {
            const r = await fetch('/api/settings');
            if (!r.ok) return;
            const s = await r.json().catch(() => ({}));
            this.excludeTerms = s.exclude_terms || [];
        },
        async openSettings() {
            this.settingsError = '';
            this.newTerm = '';
            // Reload so the modal always opens on the persisted list — this also makes
            // Cancel a genuine discard of anything edited in a previous session.
            await this.loadSettings();
            this.showSettingsModal = true;
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
            this.settingsSaving = true;
            this.settingsError = '';
            try {
                const r = await fetch('/api/settings', {
                    method: 'PUT',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ exclude_terms: this.excludeTerms }),
                });
                if (!r.ok) {
                    const e = await r.json().catch(() => ({}));
                    this.settingsError = e.error || 'Could not save settings';
                    return;
                }
                // Re-render from the response: the server trims blanks and folds
                // duplicates, so the saved list may differ from what was sent.
                const s = await r.json().catch(() => ({}));
                this.excludeTerms = s.exclude_terms || [];
                this.showSettingsModal = false;
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
