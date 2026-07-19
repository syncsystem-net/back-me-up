package scanner

import "strings"

// foldMap maps accented Latin runes to their unaccented equivalents. This exists
// instead of golang.org/x/text/unicode/norm because that module is not in the
// module cache, and pulling it in would require a network fetch for what amounts
// to a lookup table (the same reasoning behind the hand-rolled internal/ratelimit
// token bucket). The set covers Latin-1 Supplement and the Latin Extended-A runes
// that appear in the languages this tool is used with; anything outside it is
// left as-is, which degrades to case-insensitive matching rather than breaking.
var foldMap = map[rune]rune{
	'á': 'a', 'à': 'a', 'â': 'a', 'ã': 'a', 'ä': 'a', 'å': 'a', 'ā': 'a', 'ă': 'a', 'ą': 'a',
	'é': 'e', 'è': 'e', 'ê': 'e', 'ë': 'e', 'ē': 'e', 'ĕ': 'e', 'ė': 'e', 'ę': 'e', 'ě': 'e',
	'í': 'i', 'ì': 'i', 'î': 'i', 'ï': 'i', 'ī': 'i', 'ĭ': 'i', 'į': 'i', 'ı': 'i',
	'ó': 'o', 'ò': 'o', 'ô': 'o', 'õ': 'o', 'ö': 'o', 'ø': 'o', 'ō': 'o', 'ŏ': 'o', 'ő': 'o',
	'ú': 'u', 'ù': 'u', 'û': 'u', 'ü': 'u', 'ū': 'u', 'ŭ': 'u', 'ů': 'u', 'ű': 'u', 'ų': 'u',
	'ý': 'y', 'ÿ': 'y', 'ŷ': 'y',
	'ñ': 'n', 'ń': 'n', 'ņ': 'n', 'ň': 'n',
	'ç': 'c', 'ć': 'c', 'ĉ': 'c', 'ċ': 'c', 'č': 'c',
	'ß': 's', 'ś': 's', 'ŝ': 's', 'ş': 's', 'š': 's',
	'ź': 'z', 'ż': 'z', 'ž': 'z',
	'ğ': 'g', 'ĝ': 'g', 'ġ': 'g', 'ģ': 'g',
	'ł': 'l', 'ĺ': 'l', 'ļ': 'l', 'ľ': 'l',
	'ŕ': 'r', 'ŗ': 'r', 'ř': 'r',
	'ţ': 't', 'ť': 't', 'ŧ': 't',
	'ď': 'd', 'đ': 'd', 'ð': 'd',
	'ĥ': 'h', 'ħ': 'h',
	'ĵ': 'j', 'ķ': 'k', 'ŵ': 'w', 'þ': 'p',
}

// Fold lowercases s and strips accents so "Conteúdo" and "conteudo" compare
// equal. Used for both exclude-term matching and global tree search, so the two
// features agree on what "matches" means.
//
// It handles both Unicode forms an accented name can arrive in: precomposed
// (NFC, a single rune like U+00E9 é) via foldMap, and decomposed (NFD, a base
// letter followed by a combining mark) by dropping the combining marks. Both
// matter — Windows hands back NFC while macOS (APFS/HFS+) hands back NFD, so
// folding only one form would silently fail to match on the other platform.
func Fold(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		if isCombiningMark(r) {
			continue
		}
		if folded, ok := foldMap[r]; ok {
			b.WriteRune(folded)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// isCombiningMark reports whether r is in the Combining Diacritical Marks block,
// which is what an NFD-decomposed accent leaves trailing its base letter.
func isCombiningMark(r rune) bool {
	return r >= 0x0300 && r <= 0x036F
}

// Matches reports whether name contains term, ignoring case and accents. An
// empty term never matches, so a blank entry in the exclude list can't silently
// exclude every directory.
func Matches(name, term string) bool {
	if strings.TrimSpace(term) == "" {
		return false
	}
	return strings.Contains(Fold(name), Fold(strings.TrimSpace(term)))
}

// Excluded reports whether name matches any of the terms.
func Excluded(name string, terms []string) bool {
	for _, t := range terms {
		if Matches(name, t) {
			return true
		}
	}
	return false
}
