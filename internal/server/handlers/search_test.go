package handlers

import (
	"strings"
	"testing"

	"github.com/syncsystem-net/back-me-up/internal/scanner"
)

func tree() *scanner.Node {
	return &scanner.Node{
		Name: "bkup003",
		Children: []*scanner.Node{
			{Name: "docs", Children: []*scanner.Node{
				{Name: "Conteúdo Antigo"},
				{Name: "Faturas"},
			}},
			{Name: "media", Children: []*scanner.Node{
				{Name: "faturas digitalizadas"},
			}},
		},
	}
}

func hitPaths(hits []treeHit) string {
	var out []string
	for _, h := range hits {
		out = append(out, h.path)
	}
	return strings.Join(out, "|")
}

func TestSearchNodeReportsFullPath(t *testing.T) {
	hits := searchNode(tree(), nil, "faturas")
	// Both matches are reported, each with its own location in the tree, so the
	// user can tell the two "faturas" folders apart.
	want := "bkup003/docs/Faturas|bkup003/media/faturas digitalizadas"
	if got := hitPaths(hits); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// Search must fold accents exactly as exclude-term matching does, so the two
// features agree on what "matches" means.
func TestSearchNodeIsAccentInsensitive(t *testing.T) {
	for _, q := range []string{"conteudo", "Conteúdo", "CONTEUDO"} {
		hits := searchNode(tree(), nil, q)
		if got := hitPaths(hits); got != "bkup003/docs/Conteúdo Antigo" {
			t.Errorf("query %q: got %q", q, got)
		}
	}
}

func TestSearchNodeMatchesRoot(t *testing.T) {
	hits := searchNode(tree(), nil, "bkup")
	if got := hitPaths(hits); got != "bkup003" {
		t.Errorf("got %q, want the root itself", got)
	}
}

func TestSearchNodeNoMatches(t *testing.T) {
	if hits := searchNode(tree(), nil, "nothing-here"); len(hits) != 0 {
		t.Errorf("expected no hits, got %q", hitPaths(hits))
	}
}

// A sibling's accumulated path must not leak into the next branch — the classic
// append-aliasing bug when building paths during a recursive walk.
func TestSearchNodePathsDoNotAlias(t *testing.T) {
	hits := searchNode(tree(), nil, "a")
	for _, h := range hits {
		if strings.Contains(h.path, "docs/media") || strings.Contains(h.path, "media/docs") {
			t.Errorf("path aliasing between siblings: %q", h.path)
		}
	}
}
