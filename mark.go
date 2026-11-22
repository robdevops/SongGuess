package main

import (
	"cmp"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// folds are the accented letters normalize writes plainly.
var folds = map[rune]string{
	'à': "a", 'á': "a", 'â': "a", 'ã': "a", 'ä': "a", 'å': "a", 'ā': "a", 'ă': "a", 'ą': "a",
	'ç': "c", 'ć': "c", 'č': "c", 'ď': "d", 'đ': "d", 'ð': "d",
	'è': "e", 'é': "e", 'ê': "e", 'ë': "e", 'ē': "e", 'ė': "e", 'ę': "e", 'ě': "e",
	'ğ': "g", 'ì': "i", 'í': "i", 'î': "i", 'ï': "i", 'ī': "i", 'į': "i", 'ı': "i",
	'ł': "l", 'ľ': "l", 'ñ': "n", 'ń': "n", 'ň': "n",
	'ò': "o", 'ó': "o", 'ô': "o", 'õ': "o", 'ö': "o", 'ø': "o", 'ō': "o", 'ő': "o",
	'ř': "r", 'ś': "s", 'š': "s", 'ş': "s", 'ș': "s", 'ť': "t", 'ţ': "t", 'ț': "t",
	'ù': "u", 'ú': "u", 'û': "u", 'ü': "u", 'ū': "u", 'ů': "u", 'ű': "u", 'ų': "u",
	'ý': "y", 'ÿ': "y", 'ź': "z", 'ż': "z", 'ž': "z",
	'ß': "ss", 'æ': "ae", 'œ': "oe", 'þ': "th",
}

// articles are the words normalize drops.
var articles = map[string]bool{"a": true, "an": true, "the": true}

// normalize is s as answers are compared: lower case, accents and
// punctuation gone, "&" written "and", the words a, an and the dropped,
// single spaces.
func normalize(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch f, ok := folds[r]; {
		case ok:
			b.WriteString(f)
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		case r == '\'' || r == '’': // don't, rock'n'roll
		case r == '&':
			b.WriteString(" and ")
		default:
			b.WriteByte(' ')
		}
	}
	words := strings.Fields(b.String())
	kept := words[:0]
	for _, w := range words {
		if !articles[w] {
			kept = append(kept, w)
		}
	}
	return strings.Join(kept, " ")
}

// What a title or an artist's name carries that isn't needed to name it:
// a featured artist, a version in parentheses, and a version after " - ".
var (
	featPart   = regexp.MustCompile(`(?i)\s(feat\.?|ft\.?|featuring)\s.*$`)
	parensPart = regexp.MustCompile(`\([^()]*\)`)
)

// stripVersion is a title without its version ("Remastered 2011", "Radio
// Edit"), its featured artists, and anything in parentheses; or the title
// as it is, if nothing would be left.
func stripVersion(s string) string {
	t := s
	if i := strings.Index(t, " - "); i > 0 {
		t = t[:i]
	}
	for {
		u := parensPart.ReplaceAllString(t, " ")
		if u == t {
			break
		}
		t = u
	}
	t = featPart.ReplaceAllString(t, "")
	t = strings.Join(strings.Fields(t), " ")
	if normalize(t) == "" {
		return strings.TrimSpace(s)
	}
	return t
}

// titleNames are the forms of t's title an answer may give.
func titleNames(t track) []string {
	return unique(stripVersion(cmp.Or(t.Short, t.Title)), stripVersion(t.Title))
}

// artistNames are the names an answer may give for t's artist: the whole
// name, without featured artists. It isn't split on "&" or ",", since for
// a band ("Earth, Wind & Fire", "Mumford & Sons") the parts are ordinary
// words.
func artistNames(t track) []string {
	return unique(featPart.ReplaceAllString(t.Artist, ""))
}

// unique is names without repeats or ones that normalise to nothing.
func unique(names ...string) []string {
	var out []string
	seen := map[string]bool{}
	for _, n := range names {
		if k := normalize(n); k != "" && !seen[k] {
			seen[k] = true
			out = append(out, n)
		}
	}
	return out
}

// matches reports whether reply names one of names: the whole reply, or
// some run of its words, is the name, normalised, within the quiz's edit
// distance (1 for a name of 5 to 9 characters, 2 for 10 or more; a name or
// reply with a number in it must match exactly). So "Bohemian Rapsody by
// Queen" names both Bohemian Rhapsody and Queen. Runs of words are only
// searched in a reply of at most 4 words more than the name, and a name
// of under 5 characters only at the reply's start or end ("Dancing Queen
// by ABBA"), so a list of guesses, or chat that happens to hold a short
// title ("no one knows"), doesn't score.
func matches(reply string, names []string) bool {
	got := normalize(reply)
	if got == "" {
		return false
	}
	words := strings.Fields(got)
	for _, n := range names {
		want := normalize(n)
		if want == "" {
			continue
		}
		if near(got, want) {
			return true
		}
		k := len(strings.Fields(want))
		if len(words) > k+4 {
			continue // a long reply must match whole
		}
		short := utf8.RuneCountInString(want) < 5
		for size := max(1, k-1); size <= k+1; size++ {
			for i := 0; i+size <= len(words); i++ {
				if short && i > 0 && i+size < len(words) {
					continue // a short name only at the start or the end
				}
				if near(strings.Join(words[i:i+size], " "), want) {
					return true
				}
			}
		}
	}
	return false
}

// near reports whether got, normalised, is want, normalised, or within its
// edit distance.
func near(got, want string) bool {
	if got == want {
		return true
	}
	if strings.IndexFunc(want, unicode.IsDigit) >= 0 || strings.IndexFunc(got, unicode.IsDigit) >= 0 {
		return false
	}
	allowed := 0
	switch n := utf8.RuneCountInString(want); {
	case n >= 10:
		allowed = 2
	case n >= 5:
		allowed = 1
	}
	return allowed > 0 && distance(got, want, allowed) <= allowed
}

// distance is the Levenshtein distance between a and b in runes, or
// limit+1 once it is surely over limit.
func distance(a, b string, limit int) int {
	ra, rb := []rune(a), []rune(b)
	if d := len(ra) - len(rb); d > limit || -d > limit {
		return limit + 1
	}
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		best := cur[0]
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
			best = min(best, cur[j])
		}
		if best > limit {
			return limit + 1
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}
