package credentials

import (
	"testing"

	"github.com/syncsystem-net/back-me-up/internal/accounts"
	"github.com/syncsystem-net/back-me-up/internal/database"
)

// .env seeds the tier on first import, the same way it seeds a password.
func TestTierIsAdoptedFromEnv(t *testing.T) {
	db, _ := newDB(t)
	m := newManager(t, db, passphrase)

	env := megaEnv("pw")
	env.Accounts[0].Tier = accounts.TierPaid
	if err := m.Import(env); err != nil {
		t.Fatalf("Import: %v", err)
	}

	rows, err := database.ListAccountRows(db)
	if err != nil {
		t.Fatalf("ListAccountRows: %v", err)
	}
	if len(rows) != 1 || rows[0].Tier != accounts.TierPaid {
		t.Fatalf("stored tier = %+v, want paid from .env", rows)
	}
	if rows[0].TierSource != database.TierSourceEnv {
		t.Errorf("tier_source = %q, want env", rows[0].TierSource)
	}
}

// An account .env says nothing about defaults to free, which is the tier whose
// caps are strict — mislabelling a paid account only splits more than needed,
// while guessing "paid" would let an oversized file through.
func TestUnsetTierDefaultsToFree(t *testing.T) {
	db, _ := newDB(t)
	m := newManager(t, db, passphrase)

	if err := m.Import(megaEnv("pw")); err != nil {
		t.Fatalf("Import: %v", err)
	}
	rows, _ := database.ListAccountRows(db)
	if len(rows) != 1 || rows[0].Tier != accounts.TierFree {
		t.Fatalf("stored tier = %+v, want free", rows)
	}
}

// The rule that makes the Accounts view's tier dropdown worth having. .env is
// replayed on every boot, so without this a tier changed in the UI would appear
// to work and silently revert at the next restart.
func TestAppSetTierSurvivesAStaleEnvValue(t *testing.T) {
	db, _ := newDB(t)
	m := newManager(t, db, passphrase)

	env := megaEnv("pw")
	env.Accounts[0].Tier = accounts.TierFree
	if err := m.Import(env); err != nil {
		t.Fatalf("first Import: %v", err)
	}

	rows, _ := database.ListAccountRows(db)
	if err := database.UpdateAccountTier(db, rows[0].ID, accounts.TierPaid); err != nil {
		t.Fatalf("UpdateAccountTier: %v", err)
	}

	// A restart: .env still says free.
	if err := m.Import(env); err != nil {
		t.Fatalf("second Import: %v", err)
	}

	rows, _ = database.ListAccountRows(db)
	if len(rows) != 1 {
		t.Fatalf("expected one account, got %d", len(rows))
	}
	if rows[0].Tier != accounts.TierPaid {
		t.Errorf("tier = %q after re-import, want paid: the app's own value must win over a stale .env one", rows[0].Tier)
	}
	if rows[0].TierSource != database.TierSourceApp {
		t.Errorf("tier_source = %q, want app", rows[0].TierSource)
	}
}

// Until the user overrides it, .env remains the way to change a tier.
func TestChangedEnvTierUpdatesAnEnvSourcedAccount(t *testing.T) {
	db, _ := newDB(t)
	m := newManager(t, db, passphrase)

	env := megaEnv("pw")
	env.Accounts[0].Tier = accounts.TierFree
	if err := m.Import(env); err != nil {
		t.Fatalf("first Import: %v", err)
	}

	env.Accounts[0].Tier = accounts.TierPaid
	if err := m.Import(env); err != nil {
		t.Fatalf("second Import: %v", err)
	}

	rows, _ := database.ListAccountRows(db)
	if rows[0].Tier != accounts.TierPaid {
		t.Errorf("tier = %q, want paid — .env is still how an env-sourced tier changes", rows[0].Tier)
	}
}

// The tier must reach the running credential set, not just the row.
func TestLoadedAccountCarriesItsTier(t *testing.T) {
	db, _ := newDB(t)
	m := newManager(t, db, passphrase)

	env := megaEnv("pw")
	env.Accounts[0].Tier = accounts.TierPaid
	if err := m.Import(env); err != nil {
		t.Fatalf("Import: %v", err)
	}
	if err := m.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	all := m.Store().All()
	if len(all) != 1 {
		t.Fatalf("store holds %d accounts, want 1", len(all))
	}
	if all[0].Tier != accounts.TierPaid {
		t.Errorf("in-memory tier = %q, want paid", all[0].Tier)
	}
}
