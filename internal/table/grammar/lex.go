package grammar

// The lexer: one utterance to lowercase tokens, the question guard, the
// intent-prefix strips, and the number words tables say out loud. There is
// no formal grammar here on purpose — the shapes are a fixed vocabulary a
// hand writes and a table test pins, not a generative language.

import (
	"strings"
	"unicode"
)

// tokenize lowercases and splits an utterance. Letters, digits, '/' and
// the signed-counter punctuation stay glued ("+1/+1", "-3", "city's");
// every other separator becomes a space, except '?' which survives as its
// own token — a trailing question mark is the cheapest possible question
// guard, and questions are never actions.
func tokenize(utterance string) []string {
	var (
		out   []string
		cur   strings.Builder
		flush = func() {
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		}
	)
	for _, r := range strings.ToLower(strings.TrimSpace(utterance)) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '/' || r == '\'' || r == '+' || r == '-' || r == '’':
			cur.WriteRune(r)
		case r == '?':
			flush()
			out = append(out, "?")
		default:
			flush()
		}
	}
	flush()
	for i, t := range out {
		out[i] = strings.Trim(t, "'’")
	}
	// A detached sign and the number it belongs to ("− 3" said with a
	// pause) merge back into one token.
	merged := out[:0]
	for i := 0; i < len(out); i++ {
		if (out[i] == "+" || out[i] == "-") && i+1 < len(out) && isDigits(out[i+1]) {
			merged = append(merged, out[i]+out[i+1])
			i++
			continue
		}
		if out[i] != "" {
			merged = append(merged, out[i])
		}
	}
	if len(merged) > 0 && merged[len(merged)-1] == "?" {
		return merged
	}
	// Standalone "?" mid-utterance ("what was that? anyway, pass") is
	// filler here; the trailing case above already carries the guard.
	return merged
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// questionWords are the openers that make an utterance a question —
// rules questions, asks about the game, anything the resolver (5a) owns.
// A question is never an action, however action-shaped its words are
// ("does this resolve?").
var questionWords = map[string]bool{
	"what": true, "whats": true, "what's": true, "where": true, "when": true,
	"why": true, "how": true, "who": true, "whom": true, "whose": true,
	"which": true, "can": true, "could": true, "should": true, "would": true,
	"do": true, "does": true, "did": true, "is": true, "are": true,
	"was": true, "were": true, "will": true, "may": true, "might": true,
	"must": true, "shall": true, "am": true,
}

func isQuestion(toks []string) bool {
	if toks[len(toks)-1] == "?" {
		return true
	}
	return questionWords[toks[0]]
}

// intentPrefixes are the throat-clearings that carry no meaning of their
// own. Stripping them here keeps every rule's vocabulary to the words
// that matter. "i'm" is deliberately absent — "i'm the monarch" and
// "i'm at twenty" are shapes in their own right.
var intentPrefixes = [][]string{
	{"i", "am", "going", "to"}, {"i'm", "going", "to"}, {"im", "going", "to"},
	{"i'm", "gonna"}, {"im", "gonna"}, {"imma"},
	{"i", "will"}, {"i'll"}, {"i'd", "like", "to"}, {"i", "would", "like", "to"},
	{"i", "want", "to"}, {"let", "me"}, {"i"}, {"we"},
	{"in", "response"}, {"respond", "with"}, {"respond"}, {"response"},
	{"okay"}, {"ok"}, {"and"}, {"so"}, {"well"}, {"then"},
}

// fillers can appear anywhere; they are noise, not words.
var fillers = map[string]bool{"um": true, "uh": true, "erm": true, "er": true, "ah": true}

// politeness trails the same way.
var trailing = map[string]bool{"please": true, "now": true, "thanks": true, "you": true}

// stripIntent removes the noise: leading intent prefixes (repeatedly),
// filler tokens anywhere, and a trailing politeness word. It keeps at
// least the shape recognizable — "okay, um, i'll go ahead and pass, thanks"
// is just "pass" said by someone with manners.
func stripIntent(toks []string) []string {
	again := true
	for again {
		again = false
		for _, p := range intentPrefixes {
			if len(toks) > len(p) && matchPrefix(toks, p) {
				toks = toks[len(p):]
				again = true
				break
			}
		}
	}
	kept := toks[:0]
	for _, t := range toks {
		if isFiller(t) || t == "?" {
			continue
		}
		kept = append(kept, t)
	}
	toks = kept
	for len(toks) > 1 && trailing[toks[len(toks)-1]] {
		toks = toks[:len(toks)-1]
	}
	return toks
}

func matchPrefix(toks, prefix []string) bool {
	for i, w := range prefix {
		if toks[i] != w {
			return false
		}
	}
	return true
}

func isFiller(t string) bool {
	if fillers[t] {
		return true
	}
	// "ummm" / "uhhh": ASR stretches these without inventing new words.
	if len(t) > 2 && (t[0] == 'u') && (t[1] == 'm' || t[1] == 'h') {
		stretched := strings.Trim(t, "umh")
		return len(stretched) == 0
	}
	return false
}

/* ---------- numbers ---------- */

// smallNumbers and tens are the counts a table says; anything larger than
// ninety-nine is not a count anyone speaks and does not parse.
var smallNumbers = map[string]int{
	"zero": 0, "one": 1, "two": 2, "three": 3, "four": 4, "five": 5,
	"six": 6, "seven": 7, "eight": 8, "nine": 9, "ten": 10,
	"eleven": 11, "twelve": 12, "thirteen": 13, "fourteen": 14,
	"fifteen": 15, "sixteen": 16, "seventeen": 17, "eighteen": 18,
	"nineteen": 19, "twenty": 20, "thirty": 30, "forty": 40,
	"fifty": 50, "sixty": 60, "seventy": 70, "eighty": 80, "ninety": 90,
}

// numberAt reads a spoken or typed number at position i: digits, a
// number word, or a tens-unit compound ("twenty-one", "twenty one").
// Returns the value and the position after it.
func numberAt(toks []string, i int) (n int, next int, ok bool) {
	if i >= len(toks) {
		return 0, i, false
	}
	t := toks[i]
	if isDigits(t) {
		v := 0
		for _, r := range t {
			v = v*10 + int(r-'0')
		}
		if v > 999 {
			return 0, i, false
		}
		return v, i + 1, true
	}
	// Hyphenated compounds arrive as one token; split them.
	parts := strings.Split(t, "-")
	if len(parts) > 2 {
		return 0, i, false
	}
	v, ok := smallNumbers[parts[0]]
	if !ok {
		return 0, i, false
	}
	if len(parts) == 2 {
		u, oku := smallNumbers[parts[1]]
		if !oku || u >= 10 || v < 20 || v%10 != 0 {
			return 0, i, false
		}
		return v + u, i + 1, true
	}
	// Tens followed by a bare unit ("twenty one" said with a space).
	if v >= 20 && v%10 == 0 && i+1 < len(toks) {
		if u, oku := smallNumbers[toks[i+1]]; oku && u > 0 && u < 10 {
			return v + u, i + 2, true
		}
	}
	return v, i + 1, true
}

// signedNumberAt reads a signed number: "-3", "+2", or a bare count.
// sign defaults positive for bare values.
func signedNumberAt(toks []string, i int) (n int, next int, ok bool) {
	if i >= len(toks) {
		return 0, i, false
	}
	if len(toks[i]) > 1 && (toks[i][0] == '-' || toks[i][0] == '+') && isDigits(toks[i][1:]) {
		v := 0
		for _, r := range toks[i][1:] {
			v = v*10 + int(r-'0')
		}
		if toks[i][0] == '-' {
			v = -v
		}
		return v, i + 1, true
	}
	v, next, ok := numberAt(toks, i)
	return v, next, ok
}

// articleCount reads "a"/"an" as the count one, in the positions that
// take an article ("make a Treasure", "put a +1/+1 counter on Atraxa").
func articleCount(toks []string, i int) (n int, next int, ok bool) {
	if i < len(toks) && (toks[i] == "a" || toks[i] == "an") {
		return 1, i + 1, true
	}
	return numberAt(toks, i)
}
