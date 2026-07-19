// Parsed tree_json cache, deliberately OUTSIDE Alpine.data so the parsed objects
// never get wrapped in a reactive proxy — the trees are read-only render input and
// proxying them would cost on every one of the many re-renders the 2s poll triggers.
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

        // Expansion state is keyed by email so it survives the 2s polling refresh.
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

        // Global tree search (server-side, accent-insensitive). Separate from the
        // real-time `search` filtering, which stays purely client-side.
        searchResults: [],
        searchRan: false,
        searching: false,
        searchError: '',

        async init() {
            await Promise.all([this.loadUsers(), this.loadAccounts(), this.loadSettings()]);
            setInterval(() => this.refresh(), 2000);
        },
        async refresh() {
            if (document.hidden) return;
            await this.loadUsers();
            if (this.hasActiveJobs()) await this.loadAccounts();
        },
        async loadUsers() {
            const r = await fetch('/api/users');
            this.users = await r.json() || [];
        },
        async loadAccounts() {
            const r = await fetch('/api/accounts');
            this.accounts = await r.json() || [];
        },
        hasActiveJobs() {
            return this.users.some(u => (u.jobs || []).some(j => j.status === 'pending' || j.status === 'in_progress'));
        },

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
        openDeleteRecord(u, withFiles) {
            if (!u.backup) return;
            this.error = '';
            this.deleteConfirm = '';
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
            this.error = '';
            this.deleteConfirm = '';
            this.deleteTarget = { kind: 'provider', jobIds: ids, label: `${provider} — ${u.email}`, requireType: true };
            this.showDeleteModal = true;
        },
        deleteMessage() {
            const t = this.deleteTarget;
            if (!t) return '';
            if (t.kind === 'record-files') return `This deletes the record “${t.label}” and every uploaded file from its provider(s). Type DELETE to confirm.`;
            if (t.kind === 'provider') return `This deletes all files for ${t.label} from that provider. Type DELETE to confirm.`;
            return `This removes the record “${t.label}” locally. Uploaded files are left on their providers.`;
        },
        canConfirmDelete() {
            if (!this.deleteTarget || this.deleting) return false;
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
                    for (const id of t.jobIds) {
                        const r = await fetch(`/api/jobs/${id}`, {
                            method: 'DELETE',
                            headers: { 'Content-Type': 'application/json' },
                            body: JSON.stringify({ confirm: 'DELETE' }),
                        });
                        if (!r.ok) { const e = await r.json().catch(() => ({})); this.error = e.error || 'Failed to delete'; return; }
                    }
                } else {
                    const r = await fetch(`/api/backups/${t.backupId}`, {
                        method: 'DELETE',
                        headers: { 'Content-Type': 'application/json' },
                        body: JSON.stringify({ confirm: this.deleteConfirm, delete_files: t.kind === 'record-files' }),
                    });
                    if (!r.ok) { const e = await r.json().catch(() => ({})); this.error = e.error || 'Failed to delete'; return; }
                }
                this.showDeleteModal = false;
                this.deleteTarget = null;
                await this.loadUsers();
            } finally {
                this.deleting = false;
            }
        },

        // ---- Files tree ----
        // Alpine cannot recurse with x-for, so the nested tree is flattened into the
        // list of rows that are currently VISIBLE (collapsed subtrees are skipped) and
        // rendered by one x-for, with depth expressed as left padding.
        treeRows(zip) {
            const root = parseTreeJSON(zip);
            if (!root) return [];
            const rows = [];
            const walk = (node, parentPath, depth) => {
                const name = node.name || '(unnamed)';
                const path = parentPath ? `${parentPath}/${name}` : name;
                const children = node.children || [];
                const expanded = this.isNodeOpen(zip.id, path, depth);
                rows.push({
                    path, name, depth,
                    size: node.size_bytes || 0,
                    hasChildren: children.length > 0,
                    expanded,
                });
                if (children.length && expanded) for (const c of children) walk(c, path, depth + 1);
            };
            walk(root, '', 0);
            return rows;
        },
        hasTree(zip) { return parseTreeJSON(zip) !== null; },
        nodeKey(zipId, path) { return `${zipId}:${path}`; },
        // Default state: root open, everything below closed. An explicit entry in
        // either list overrides that default.
        isNodeOpen(zipId, path, depth) {
            const k = this.nodeKey(zipId, path);
            if (this.expandedNodes.includes(k)) return true;
            if (this.collapsedNodes.includes(k)) return false;
            return depth === 0;
        },
        toggleNode(zipId, row) {
            const k = this.nodeKey(zipId, row.path);
            const open = row.expanded;
            this.expandedNodes = this.expandedNodes.filter(x => x !== k);
            this.collapsedNodes = this.collapsedNodes.filter(x => x !== k);
            if (open) this.collapsedNodes.push(k);
            else this.expandedNodes.push(k);
        },
        // Every path in the zip that has children — the only nodes expand/collapse-all
        // needs to record. Walks the full tree, not just the visible rows.
        allNodePaths(zip) {
            const root = parseTreeJSON(zip);
            if (!root) return [];
            const paths = [];
            const walk = (node, parentPath) => {
                const name = node.name || '(unnamed)';
                const path = parentPath ? `${parentPath}/${name}` : name;
                const children = node.children || [];
                if (children.length) {
                    paths.push(path);
                    for (const c of children) walk(c, path);
                }
            };
            walk(root, '');
            return paths;
        },
        setZipTreeOpen(zip, open) {
            const keys = this.allNodePaths(zip).map(p => this.nodeKey(zip.id, p));
            // Drop this zip's existing entries from both lists first so the two lists
            // never disagree about the same node.
            this.expandedNodes = this.expandedNodes.filter(k => !keys.includes(k));
            this.collapsedNodes = this.collapsedNodes.filter(k => !keys.includes(k));
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
