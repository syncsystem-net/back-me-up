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

        async init() {
            await Promise.all([this.loadUsers(), this.loadAccounts()]);
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
        filteredUsers() {
            const q = this.search.trim().toLowerCase();
            if (!q) return this.users;
            return this.users.filter(u => {
                if (u.email.toLowerCase().includes(q)) return true;
                if (u.backup && (u.backup.title || '').toLowerCase().includes(q)) return true;
                return (u.zips || []).some(z =>
                    (z.name || '').toLowerCase().includes(q) || (z.tree_json || '').toLowerCase().includes(q));
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
        async submitUpload(resolutions) {
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

        // ---- Files tree (pretty JSON; interactive tree lands in a later phase) ----
        prettyTree(zip) {
            try { return JSON.stringify(JSON.parse(zip.tree_json || '{}'), null, 2); }
            catch { return zip.tree_json || ''; }
        },
    }));
});
