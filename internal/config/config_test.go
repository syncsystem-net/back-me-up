package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	return p
}

func TestScanMaxDepthDefaults(t *testing.T) {
	tests := []struct {
		name string
		body string
		want int
	}{
		// An older config.yml with no scan section at all must still work.
		{"absent", "server:\n  port: 8080\n", 3},
		{"zero", "scan:\n  max_depth: 0\n", 3},
		// A negative value would make the walk record nothing; coerce it too.
		{"negative", "scan:\n  max_depth: -1\n", 3},
		{"explicit", "scan:\n  max_depth: 6\n", 6},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Load(writeConfig(t, tt.body))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.Scan.MaxDepth != tt.want {
				t.Errorf("Scan.MaxDepth = %d, want %d", cfg.Scan.MaxDepth, tt.want)
			}
		})
	}
}
