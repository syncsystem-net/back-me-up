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
        },
        async loadBackups() {
            const r = await fetch('/api/backups');
            const data = await r.json();
            this.backups = (data || []).map(b => ({ ...b, expanded: false }));
        },
        async loadAccounts() {
            const r = await fetch('/api/accounts');
            this.accounts = await r.json() || [];
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

        jobStatus(backup, provider) {
            const jobs = (backup.jobs || []).filter(j => j.provider === provider);
            if (jobs.length === 0) return null;
            if (jobs.some(j => j.status === 'failed')) return 'failed';
            if (jobs.some(j => j.status === 'in_progress')) return 'in_progress';
            if (jobs.every(j => j.status === 'complete')) return 'complete';
            return 'pending';
        },

        dirLevel1(backup) {
            return (backup.directories || []).filter(d => d.level === 1);
        },
        dirLevel2(backup, parentPath) {
            return (backup.directories || []).filter(d => d.level === 2 && d.path.startsWith(parentPath + '/'));
        },
    }));
});
