// Package limits models what each cloud provider will actually accept: the
// largest single file it takes, and how much may be transferred to it inside a
// rolling window. Both vary by account tier, so a free 4shared account and a
// paid one under the same provider carry different numbers.
//
// The values are user-editable (Settings modal) because they are not facts we
// can verify — 4shared publishes its transfer allowance as *download* traffic
// and says nothing about uploads. The defaults here are the researched caps; the
// user is expected to correct them when reality disagrees.
//
// Zero means unlimited, for both the file cap and the transfer budget. That is
// the escape hatch: a budget we guessed wrong must never be able to stall a
// backup with no way out.
package limits

import (
	"encoding/json"
	"sort"
)

// Tier is an account's plan with its provider. Limits are looked up by
// (provider, tier), so upgrading an account is a tier change rather than an
// edit to every threshold.
type Tier string

const (
	TierFree Tier = "free"
	TierPaid Tier = "paid"
)

// Tiers returns every tier, in display order.
func Tiers() []Tier { return []Tier{TierFree, TierPaid} }

// NormalizeTier maps arbitrary input onto a known tier. Anything unrecognised
// becomes free: it is the conservative answer, since a free tier's limits are
// the strict ones and mislabelling a paid account merely splits more than it
// had to. Guessing "paid" would instead let an oversized file through.
func NormalizeTier(s string) Tier {
	if Tier(s) == TierPaid {
		return TierPaid
	}
	return TierFree
}

// Limit is one provider/tier's ceilings.
type Limit struct {
	// MaxFileBytes is the largest single file the provider accepts, and so the
	// threshold an archive is split at. 0 means no limit.
	MaxFileBytes int64 `json:"max_file_bytes"`
	// TransferBytes is how much may be uploaded inside WindowHours. 0 means no
	// budget, and disables holding entirely for this provider/tier.
	TransferBytes int64 `json:"transfer_bytes"`
	// WindowHours is the length of the rolling window TransferBytes applies to.
	WindowHours int `json:"window_hours"`
}

// Splits reports whether archives for this provider/tier are subject to a size
// threshold at all.
func (l Limit) Splits() bool { return l.MaxFileBytes > 0 }

// Budgeted reports whether uploads to this provider/tier are held when the
// transfer budget runs out.
func (l Limit) Budgeted() bool { return l.TransferBytes > 0 && l.WindowHours > 0 }

// Set is the full table: provider name to tier to limit.
type Set map[string]map[Tier]Limit

// Researched provider caps, as of the pre-launch refinement.
//
// 4shared free rejects any single file over 2 GB; the threshold sits at 1.9 GB
// decimal so it is safely under that whether the provider means 2×10⁹ or 2×2³⁰
// bytes — their documentation does not say which, and being wrong by a factor of
// 7% costs one extra volume while being wrong the other way costs the upload.
// The 3 GB/day figure is documented as *download* traffic; it is applied to
// uploads here because the user chose enforcement, and it is editable because
// that application is unverified.
//
// MEGA free has no per-file cap at all and a transfer allowance of roughly 5 GB
// per rolling 6 hours, measured by IP rather than by account.
//
// Paid tiers ship uncapped: we have no researched numbers for them, and
// inventing a limit that silently splits a paid user's archives is worse than
// leaving the field for them to fill in.
const (
	fourSharedFreeMaxFile  = 1_900_000_000
	fourSharedFreeTransfer = 3_000_000_000
	fourSharedWindowHours  = 24

	megaFreeTransfer = 5_000_000_000
	megaWindowHours  = 6
)

// Defaults returns the shipped limits table. It is also the fallback for every
// value a stored settings row does not supply, so a truncated or hand-edited row
// degrades to these rather than to "unlimited".
func Defaults() Set {
	return Set{
		"mega": {
			TierFree: {MaxFileBytes: 0, TransferBytes: megaFreeTransfer, WindowHours: megaWindowHours},
			TierPaid: {MaxFileBytes: 0, TransferBytes: 0, WindowHours: megaWindowHours},
		},
		"fourshared": {
			TierFree: {MaxFileBytes: fourSharedFreeMaxFile, TransferBytes: fourSharedFreeTransfer, WindowHours: fourSharedWindowHours},
			TierPaid: {MaxFileBytes: 0, TransferBytes: 0, WindowHours: fourSharedWindowHours},
		},
	}
}

// For returns the limit for one provider and tier. A provider the table does not
// know is unlimited: an unknown backend cannot have had its caps researched, and
// refusing every upload to it would be worse than not splitting.
func (s Set) For(provider string, tier Tier) Limit {
	byTier, ok := s[provider]
	if !ok {
		return Limit{}
	}
	l, ok := byTier[NormalizeTier(string(tier))]
	if !ok {
		return Limit{}
	}
	return l
}

// Providers returns the providers in the table, sorted, so serialization and UI
// order are stable.
func (s Set) Providers() []string {
	out := make([]string, 0, len(s))
	for p := range s {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// rawLimit decodes with pointers so an absent field is distinguishable from an
// explicit zero. This matters: zero means "unlimited", so decoding a partial
// object into a value type would silently disable the very cap that makes an
// oversized upload possible.
type rawLimit struct {
	MaxFileBytes  *int64 `json:"max_file_bytes"`
	TransferBytes *int64 `json:"transfer_bytes"`
	WindowHours   *int   `json:"window_hours"`
}

// Parse reads a stored settings value over the defaults. Anything missing,
// unparseable or out of range keeps its default, and unknown providers or tiers
// are ignored — the table's shape comes from the code, not from the database, so
// a corrupt row cannot invent a provider or drop one.
func Parse(raw string) Set {
	out := Defaults()
	if raw == "" {
		return out
	}

	var decoded map[string]map[string]rawLimit
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return out
	}

	for provider, byTier := range decoded {
		known, ok := out[provider]
		if !ok {
			continue
		}
		for tierName, rl := range byTier {
			tier := Tier(tierName)
			if tier != TierFree && tier != TierPaid {
				continue
			}
			known[tier] = apply(known[tier], rl)
		}
	}
	return out
}

// apply overlays the supplied fields onto a default limit, clamping nonsense.
func apply(base Limit, rl rawLimit) Limit {
	if rl.MaxFileBytes != nil && *rl.MaxFileBytes >= 0 {
		base.MaxFileBytes = *rl.MaxFileBytes
	}
	if rl.TransferBytes != nil && *rl.TransferBytes >= 0 {
		base.TransferBytes = *rl.TransferBytes
	}
	if rl.WindowHours != nil && *rl.WindowHours > 0 {
		base.WindowHours = *rl.WindowHours
	}
	// A budget with no window cannot be evaluated. Rather than treat it as
	// unlimited (which discards a limit the user asked for) or as instantly
	// exhausted (which stalls every upload), fall back to the default window.
	if base.TransferBytes > 0 && base.WindowHours <= 0 {
		base.WindowHours = Defaults().For("mega", TierFree).WindowHours
	}
	return base
}

// Marshal serializes the full table, every provider and tier explicit, so a
// stored value is self-describing rather than a sparse diff against whatever the
// defaults happened to be when it was written.
func (s Set) Marshal() (string, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
