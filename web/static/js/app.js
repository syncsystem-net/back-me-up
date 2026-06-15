document.addEventListener('alpine:init', () => {
    Alpine.data('app', () => ({
        page: 'backups',
        backups: [],
        accounts: [],
        search: '',
        showNewModal: false,
        newBackup: { title: '', source_path: '', account_ids: [] },
        creating: false,
        browseLoading: false,
        error: '',

        // Expanded accordion rows survive the 2s polling refresh.
        expandedIds: [],

        // Logs modal state.
        showLogsModal: false,
        logsTitle: '',
        logs: [],

        megaAccounts() {
            return this.accounts.filter(a => a.provider === 'mega');
        },
        foursharedAccounts() {
            return this.accounts.filter(a => a.provider === 'fourshared');
        },
        filteredBackups() {
            if (!this.search) return this.backups;
            const q = this.search.toLowerCase();
            return this.backups.filter(b => b.title.toLowerCase().includes(q));
        },
        totalGB() {
            let bytes = 0;
            for (const b of this.backups) {
                for (const j of (b.jobs || [])) bytes += j.total_bytes || 0;
            }
            return (bytes / 1073741824).toFixed(2);
        },

        async init() {
            await Promise.all([this.loadBackups(), this.loadAccounts()]);
            // Poll for live progress every 2s.
            setInterval(() => this.refresh(), 2000);
        },
        async refresh() {
            if (document.hidden) return;
            await this.loadBackups();
            // Refresh quota numbers only while uploads are active (cheap, avoids churn).
            if (this.hasActiveJobs()) await this.loadAccounts();
        },
        async loadBackups() {
            const r = await fetch('/api/backups');
            const data = await r.json();
            this.backups = (data || []).map(b => ({ ...b, expanded: this.expandedIds.includes(b.id) }));
        },
        async loadAccounts() {
            const r = await fetch('/api/accounts');
            this.accounts = await r.json() || [];
        },

        hasActiveJobs() {
            return this.backups.some(b => (b.jobs || []).some(j => j.status === 'pending' || j.status === 'in_progress'));
        },

        toggleExpand(b) {
            b.expanded = !b.expanded;
            const idx = this.expandedIds.indexOf(b.id);
            if (b.expanded && idx === -1) this.expandedIds.push(b.id);
            if (!b.expanded && idx !== -1) this.expandedIds.splice(idx, 1);
        },

        async browsePath() {
            this.browseLoading = true;
            try {
                const r = await fetch('/api/browse');
                if (!r.ok) return;
                const { path } = await r.json();
                if (path) this.newBackup.source_path = path;
            } finally {
                this.browseLoading = false;
            }
        },

        toggleAccount(id) {
            const idx = this.newBackup.account_ids.indexOf(id);
            if (idx === -1) this.newBackup.account_ids.push(id);
            else this.newBackup.account_ids.splice(idx, 1);
        },

        async createBackup() {
            this.creating = true;
            this.error = '';
            try {
                const r = await fetch('/api/backups', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify(this.newBackup),
                });
                if (!r.ok) {
                    const e = await r.json();
                    this.error = e.error || 'Failed to create backup';
                    return;
                }
                this.showNewModal = false;
                this.newBackup = { title: '', source_path: '', account_ids: [] };
                await this.loadBackups();
            } finally {
                this.creating = false;
            }
        },

        providerJobs(backup, provider) {
            return (backup.jobs || []).filter(j => j.provider === provider);
        },
        jobStatus(backup, provider) {
            const jobs = this.providerJobs(backup, provider);
            if (jobs.length === 0) return null;
            if (jobs.some(j => j.status === 'failed')) return 'failed';
            if (jobs.some(j => j.status === 'in_progress')) return 'in_progress';
            if (jobs.every(j => j.status === 'complete')) return 'complete';
            return 'pending';
        },
        // Display label: once bytes are fully uploaded but the job is still
        // in_progress, it's verifying the checksum / finalizing — surface that so
        // the bar doesn't appear stuck at 100%.
        displayStatus(backup, provider) {
            const s = this.jobStatus(backup, provider);
            if (s === 'in_progress' && this.jobProgress(backup, provider) >= 100) return 'verifying';
            return s;
        },
        // Aggregate upload percentage across a provider's jobs (0-100), or null.
        jobProgress(backup, provider) {
            const jobs = this.providerJobs(backup, provider);
            if (jobs.length === 0) return null;
            let uploaded = 0, total = 0;
            for (const j of jobs) { uploaded += j.uploaded_bytes || 0; total += j.total_bytes || 0; }
            if (total === 0) return 0;
            return Math.min(100, Math.round((uploaded / total) * 100));
        },

        async openLogs(backup, provider) {
            const jobs = this.providerJobs(backup, provider);
            this.logsTitle = `${provider} logs — ${backup.title}`;
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

        dirLevel1(backup) {
            return (backup.directories || []).filter(d => d.level === 1);
        },
        dirLevel2(backup, parentPath) {
            return (backup.directories || []).filter(d => d.level === 2 && d.path.startsWith(parentPath + '/'));
        },
    }));
});
