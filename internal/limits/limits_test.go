package limits

import (
	"encoding/json"
	"testing"
)

func TestDefaultsMatchTheResearchedCaps(t *testing.T) {
	d := Defaults()

	fs := d.For("fourshared", TierFree)
	if !fs.Splits() {
		t.Error("4shared free must have a file-size threshold; without one the launch blocker is not fixed")
	}
	if fs.MaxFileBytes >= 2_000_000_000 {
		t.Errorf("4shared free threshold = %d, must be under the 2 GB hard cap", fs.MaxFileBytes)
	}
	if !fs.Budgeted() || fs.WindowHours != 24 {
		t.Errorf("4shared free budget = %+v, want a daily budget", fs)
	}

	mega := d.For("mega", TierFree)
	if mega.Splits() {
		t.Error("MEGA has no per-file limit, so it must not split by default")
	}
	if !mega.Budgeted() || mega.WindowHours != 6 {
		t.Errorf("MEGA free budget = %+v, want a 6-hour window", mega)
	}
}

// A partial settings row must not silently disable a cap. Zero means unlimited,
// so decoding an object that omits max_file_bytes into a value type would turn
// "I only changed the budget" into "stop splitting entirely".
func TestPartialValueKeepsTheDefaultRatherThanBecomingUnlimited(t *testing.T) {
	got := Parse(`{"fourshared":{"free":{"transfer_bytes":1000}}}`)

	l := got.For("fourshared", TierFree)
	if l.MaxFileBytes != Defaults().For("fourshared", TierFree).MaxFileBytes {
		t.Errorf("max_file_bytes = %d, want the default kept when the field is absent", l.MaxFileBytes)
	}
	if l.TransferBytes != 1000 {
		t.Errorf("transfer_bytes = %d, want the supplied 1000", l.TransferBytes)
	}
	if l.WindowHours != Defaults().For("fourshared", TierFree).WindowHours {
		t.Errorf("window_hours = %d, want the default kept", l.WindowHours)
	}
}

// An explicit zero is the documented way to say "unlimited", and must be
// honoured — it is the escape hatch from a budget we guessed wrong.
func TestExplicitZeroMeansUnlimited(t *testing.T) {
	got := Parse(`{"fourshared":{"free":{"max_file_bytes":0,"transfer_bytes":0}}}`)

	l := got.For("fourshared", TierFree)
	if l.Splits() {
		t.Error("an explicit max_file_bytes of 0 must disable splitting")
	}
	if l.Budgeted() {
		t.Error("an explicit transfer_bytes of 0 must disable the budget")
	}
}

func TestGarbageAndUnknownEntriesDegradeToDefaults(t *testing.T) {
	for _, raw := range []string{
		"",
		"not json at all",
		`{"nosuchprovider":{"free":{"max_file_bytes":1}}}`,
		`{"fourshared":{"platinum":{"max_file_bytes":1}}}`,
		`{"fourshared":{"free":{"max_file_bytes":-5,"window_hours":-2}}}`,
	} {
		got := Parse(raw).For("fourshared", TierFree)
		want := Defaults().For("fourshared", TierFree)
		if got != want {
			t.Errorf("Parse(%q) = %+v, want the defaults %+v", raw, got, want)
		}
	}
}

func TestRoundTripThroughMarshalIsStable(t *testing.T) {
	original := Defaults()
	original["fourshared"][TierPaid] = Limit{MaxFileBytes: 5_000_000_000, TransferBytes: 0, WindowHours: 24}

	raw, err := original.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	back := Parse(raw)

	for _, p := range original.Providers() {
		for _, tier := range Tiers() {
			if back.For(p, tier) != original.For(p, tier) {
				t.Errorf("%s/%s round-tripped to %+v, want %+v", p, tier, back.For(p, tier), original.For(p, tier))
			}
		}
	}
}

// A stored value must be self-describing, not a sparse diff against whatever the
// defaults happened to be when it was written.
func TestMarshalWritesEveryProviderAndTier(t *testing.T) {
	raw, err := Defaults().Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var decoded map[string]map[string]map[string]any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, p := range []string{"mega", "fourshared"} {
		for _, tier := range []string{"free", "paid"} {
			fields, ok := decoded[p][tier]
			if !ok {
				t.Fatalf("%s/%s missing from the serialized table", p, tier)
			}
			for _, f := range []string{"max_file_bytes", "transfer_bytes", "window_hours"} {
				if _, ok := fields[f]; !ok {
					t.Errorf("%s/%s is missing %s", p, tier, f)
				}
			}
		}
	}
}

// Guessing "paid" for an unrecognised value would let an oversized file through
// to a provider that rejects it; guessing "free" only splits more than needed.
func TestUnknownTierNormalizesToFree(t *testing.T) {
	for _, in := range []string{"", "FREE", "premium", "pro", "gibberish"} {
		if got := NormalizeTier(in); got != TierFree {
			t.Errorf("NormalizeTier(%q) = %q, want free", in, got)
		}
	}
	if got := NormalizeTier("paid"); got != TierPaid {
		t.Errorf("NormalizeTier(\"paid\") = %q, want paid", got)
	}
}

func TestUnknownProviderIsUnlimitedRatherThanBlocked(t *testing.T) {
	l := Defaults().For("somefutureprovider", TierFree)
	if l.Splits() || l.Budgeted() {
		t.Errorf("unknown provider = %+v, want unlimited: refusing every upload to it would be worse than not splitting", l)
	}
}
