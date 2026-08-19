package carddb

import (
	"context"
	"strings"
)

// Name resolution. A pasted decklist names cards the way a person (or an
// exporter) writes them: a curly apostrophe instead of a straight one, a
// missing comma, a front face standing in for a double-faced card, an
// occasional typo. Resolve walks from the cheapest, most certain match to the
// loosest, and refuses anything that is not actually close — a wrong match is
// worse than an honest miss, because the reader trusts the analysis.

// Resolve finds the real card behind a written name. It returns the card and
// true when a match is certain enough to show; otherwise false, and the caller
// reports the name as unresolved rather than substituting something else.
func (s *Store) Resolve(ctx context.Context, name string) (*Card, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, false
	}
	// 1. The name as written.
	if c, err := s.Get(name); err == nil {
		return c, true
	}
	// 2. A double-faced card written as one of its faces, either way round.
	if front, _, ok := strings.Cut(name, "//"); ok {
		if c, err := s.Get(strings.TrimSpace(front)); err == nil {
			return c, true
		}
	}
	if c, ok := s.firstLike(ctx, name+" //%"); ok {
		return c, true
	}

	want := NormalizeName(name)
	if want == "" {
		return nil, false
	}
	// 3. Punctuation and accents only — "Kodamas Reach", "Lim-Dul's Vault".
	best, score := (*Card)(nil), 0.0
	consider := func(cards []*Card) {
		for _, c := range cards {
			got := NormalizeName(c.Name)
			if got == want {
				best, score = c, 1
				return
			}
			// A card written as its front face alone matches that face.
			if front, _, ok := strings.Cut(got, " // "); ok && front == want {
				best, score = c, 1
				return
			}
			if sim := similarity(want, got); sim > score {
				best, score = c, sim
			}
		}
	}

	consider(s.like(ctx, likePattern(name)+"%", 60))
	if score < 1 {
		if hits, err := s.SearchNames(ctx, name, 20); err == nil {
			consider(hits)
		}
	}
	// 4. The bar. Below it the name is reported unresolved, not guessed at.
	if best == nil || score < 0.86 {
		return nil, false
	}
	return best, true
}

// likePattern escapes the LIKE wildcards in a written name so a card called
// "100%" cannot turn into a scan of the whole table.
func likePattern(s string) string {
	r := strings.NewReplacer("%", "", "_", "?")
	return r.Replace(strings.TrimSpace(s))
}

func (s *Store) firstLike(ctx context.Context, pattern string) (*Card, bool) {
	cards := s.like(ctx, pattern, 1)
	if len(cards) == 0 {
		return nil, false
	}
	return cards[0], true
}

func (s *Store) like(ctx context.Context, pattern string, limit int) []*Card {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+cardColumns+` FROM cards
		 WHERE name LIKE ? COLLATE NOCASE
		 ORDER BY LENGTH(name) LIMIT ?`, pattern, limit)
	if err != nil {
		return nil
	}
	cards, err := scanCards(rows)
	if err != nil {
		return nil
	}
	return cards
}

// NormalizeName reduces a card name to its comparable form: lower case, common
// accents folded, curly quotes straightened, punctuation dropped, whitespace
// collapsed. "Lim-Dûl's Vault" and "lim duls vault" normalize alike.
func NormalizeName(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		if folded, ok := accentFolds[r]; ok {
			b.WriteString(folded)
			space = false
			continue
		}
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			space = false
		case r == '/':
			// Keep the double-faced separator so a face can be compared.
			b.WriteRune('/')
			space = false
		case r == '\'' || r == '\u2019' || r == '\u2018' || r == '.':
			// Apostrophes and full stops close up rather than split, so
			// "Kodama's Reach" and "Kodamas Reach" are the same name.
		default:
			if !space && b.Len() > 0 {
				b.WriteByte(' ')
				space = true
			}
		}
	}
	out := strings.TrimSpace(b.String())
	// "x  //  y" collapses to "x // y" for a stable face split.
	return strings.Join(strings.Fields(out), " ")
}

// accentFolds maps the accented letters that appear in Magic card names to
// their ASCII equivalents, so a name typed without them still resolves.
var accentFolds = map[rune]string{
	'á': "a", 'à': "a", 'â': "a", 'ä': "a", 'ã': "a", 'å': "a",
	'é': "e", 'è': "e", 'ê': "e", 'ë': "e",
	'í': "i", 'ì': "i", 'î': "i", 'ï': "i",
	'ó': "o", 'ò': "o", 'ô': "o", 'ö': "o", 'õ': "o",
	'ú': "u", 'ù': "u", 'û': "u", 'ü': "u",
	'ñ': "n", 'ç': "c", 'ý': "y",
	'æ': "ae", 'œ': "oe", 'ß': "ss",
}

// similarity scores two normalized names between 0 and 1 using edit distance
// over the longer of the two. It is the gate that keeps "Sol Ring" from
// resolving into "Solfatara".
func similarity(a, b string) float64 {
	if a == b {
		return 1
	}
	if a == "" || b == "" {
		return 0
	}
	longer := len(a)
	if len(b) > longer {
		longer = len(b)
	}
	// A large length gap can never clear the bar; skip the distance matrix.
	if float64(min(len(a), len(b)))/float64(longer) < 0.86 {
		return 0
	}
	d := editDistance(a, b)
	return 1 - float64(d)/float64(longer)
}

// editDistance is Levenshtein distance over two ASCII-normalized names, with a
// two-row buffer rather than a full matrix.
func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min(prev[j]+1, min(curr[j-1]+1, prev[j-1]+cost))
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
}
