package odds

// The question gate (MAD-336): a small deterministic parse of the
// probability questions a table actually asks, with the refusal rule
// in front. Library order is never modelled — not here, not in the
// engine, not anywhere — so any question whose answer depends on
// order is declined out loud with universe.ErrLibraryOrder rather
// than answered plausibly. Grammar first, no model: the shapes are
// few, fixed and unit-tested, and what does not parse says so.
//
// The line the gate walks: draw-count questions are answerable
// ("a land in the next three" is composition), card-position questions
// are not ("the next card", "when will I draw a Wrath" need an order
// that does not exist). Aggregate horizons yes, positions no.

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/table/universe"
)

// ErrOrderDependent is the out-loud refusal every order-dependent
// library question gets — universe.ErrLibraryOrder, the same sentinel
// the known-card universe returns, so every surface reports the one
// refusal.
var ErrOrderDependent = universe.ErrLibraryOrder

// ErrNoParse reports a question the gate has no shape for. A wrong
// parse is worse than no parse: the gate declines rather than guess.
var ErrNoParse = fmt.Errorf("odds: not a question the odds engine has a shape for")

// Query is one parsed probability question — either a draw-odds shape
// (Category/Card with Draws and AtLeast) or an outs shape (Target).
// ByTurn set (with Draws zero) resolves the draw count against the
// game's current turn.
type Query struct {
	Kind     string // "odds" | "outs"
	Category string
	Card     string
	Target   string // outs: the card, type or free text to answer
	AtLeast  int
	Draws    int
	ByTurn   int
}

// The order-dependent markers. Matching is on phrases, not words:
// "a land in the next three draws" is a composition question, while
// "is the next card a land" asks about one position in an order the
// engine refuses to model. The marker list is the refusal, written
// down — MAD-336's mandated rule.
var orderMarkers = []string{
	"next card", "next draw", "next land", "top card", "card on top",
	"on top of my library", "on top of the library", "top of my library",
	"top of the library", "off the top", "first card", "second card",
	"third card", "fourth card", "fifth card", "next flip", "first draw",
	"second draw", "when will", "how many cards until", "how many draws until",
	"how deep is", "what position", "which position", "in what order",
	"order of my library", "library order", "before i draw", "before you draw",
	"before my", "shuffle", "scry", "arrange the top", "look at the top",
	"reveal the top",
}

// orderDependent reports whether the question asks about a specific
// position in an unmodelled order. Case-insensitive, phrase-level.
// "before turn N" names a horizon (every draw until then) and is an
// aggregate; "before <anything else>" is a competing-risks question —
// draw X before Y — which needs order the engine refuses to model.
func orderDependent(q string) bool {
	lower := strings.ToLower(q)
	for _, marker := range orderMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return strings.Contains(lower, " before ") && !strings.Contains(lower, "before turn")
}

// The category vocabulary — the words a table uses, mapped onto the
// categories the matcher knows.
var categoryWords = map[string]string{
	"land": CatLands, "lands": CatLands,
	"board wipe": CatWipes, "board wipes": CatWipes, "wipe": CatWipes,
	"wipes": CatWipes, "wrath": CatWipes, "wraths": CatWipes,
	"sweeper": CatWipes, "sweepers": CatWipes,
	"ramp":      CatRamp,
	"card draw": CatDraw, "draw spell": CatDraw, "draw spells": CatDraw,
	"interaction": CatInteraction, "removal": CatInteraction,
	"answer": CatInteraction, "answers": CatInteraction,
	"counterspell": CatInteraction, "counterspells": CatInteraction,
}

// wordNumbers carries the small ordinals a spoken question uses —
// "by turn nine" — so the gate takes the words the table says.
var wordNumbers = map[string]int{
	"one": 1, "two": 2, "three": 3, "four": 4, "five": 5,
	"six": 6, "seven": 7, "eight": 8, "nine": 9, "ten": 10,
	"eleven": 11, "twelve": 12, "fifteen": 15, "twenty": 20,
}

// subjectTail is the shared subject grammar: optional article,
// optional draw verb, optional article again — "of drawing a land" and
// "of a land" and "of Rhystic Study" all leave a clean subject.
const subjectTail = `(?:(?:an|a|one|any|my|another)\s+)?(?:(?:drawing|draw|finding|find|hitting|hit|seeing|see)\s+)?(?:(?:an|a|one|any|my|another)\s+)?`

var (
	// "chance of a land in the next three (draws|cards|turns)".
	reOddsNext = regexp.MustCompile(`(?i)(?:chance|odds|probability|likelihood|likely)[^\n]*?\b(?:of|for|that|to (?:draw|find|hit|see))\s+` + subjectTail + `(.+?)\s*(?:\bcard\b)?\s+in\s+(?:the\s+)?next\s+([0-9]+|one|two|three|four|five|six|seven|eight|nine|ten)\s*(?:draws?|cards?|turns?)?\s*[?.!]*$`)
	// "chance of finding a board wipe by turn nine" ("before turn"
	// names the same horizon: every draw from here to that turn).
	reOddsTurn = regexp.MustCompile(`(?i)(?:chance|odds|probability|likelihood|likely)[^\n]*?\b(?:of|for|that|to (?:draw|find|hit|see))\s+` + subjectTail + `(.+?)\s*(?:\bcard\b)?\s+(?:by|before)\s+turn\s+([0-9]+|one|two|three|four|five|six|seven|eight|nine|ten|eleven|twelve|fifteen|twenty)\s*[?.!]*$`)
	// Bare "chance of drawing a land" — no horizon names one draw.
	reOddsOne = regexp.MustCompile(`(?i)(?:chance|odds|probability|likelihood|likely)[^\n]*?\b(?:of|for|that|to (?:draw|find|hit|see))\s+` + subjectTail + `(.+?)\s*(?:\bcard\b)?\s*[?.!]*$`)
	// "what are my outs against (that enchantment)".
	reOuts = regexp.MustCompile(`(?i)(?:^|\b)outs\b\s*(?:(?:against|for|to|vs\.?)\s+(.+?))\s*[?.!]*$`)
	// "what answers / kills / deals with / removes an X".
	reAnswers = regexp.MustCompile(`(?i)^\s*(?:what|which)\s+(?:answers|kills|deals\s+with|handles|removes|beats)\s+(?:(?:an|a|the|my|that|this)\s+)?(.+?)\s*[?.!]*$`)
)

// Parse classifies one question. The order-dependent refusal runs
// first — before any shape has a chance to answer something adjacent
// — then the odds shapes, then outs. What matches nothing is a clean
// no-parse.
func Parse(question string) (Query, error) {
	q := strings.TrimSpace(question)
	if q == "" {
		return Query{}, ErrNoParse
	}
	if orderDependent(q) {
		return Query{}, fmt.Errorf("%w: %q", ErrOrderDependent, q)
	}
	if m := reOddsNext.FindStringSubmatch(q); m != nil {
		return oddsQuery(m[1], 0, numberWord(m[2], 1), 1)
	}
	if m := reOddsTurn.FindStringSubmatch(q); m != nil {
		return oddsQuery(m[1], numberWord(m[2], 1), 0, 1)
	}
	if m := reOuts.FindStringSubmatch(q); m != nil {
		target := strings.TrimSpace(m[1])
		if target == "" {
			return Query{}, ErrNoParse // outs against what?
		}
		return Query{Kind: "outs", Target: target}, nil
	}
	if m := reAnswers.FindStringSubmatch(q); m != nil {
		target := strings.TrimSpace(m[1])
		if target == "" {
			return Query{}, ErrNoParse
		}
		return Query{Kind: "outs", Target: target}, nil
	}
	if m := reOddsOne.FindStringSubmatch(q); m != nil {
		return oddsQuery(m[1], 0, 1, 1)
	}
	return Query{}, ErrNoParse
}

// oddsQuery maps the captured subject onto a category or a named
// card. A subject that is neither vocabulary nor a plausible card name
// is a no-parse — never a guess at what was meant.
func oddsQuery(subject string, byTurn, draws, atLeast int) (Query, error) {
	subject = strings.Trim(subject, " .?!")
	if cat, ok := categoryWords[strings.ToLower(subject)]; ok {
		return Query{Kind: "odds", Category: cat, AtLeast: atLeast, Draws: draws, ByTurn: byTurn}, nil
	}
	// A capitalised or quoted subject is a card name; a lowercase word
	// that is not vocabulary is not silently reinterpreted.
	if looksLikeName(subject) {
		return Query{Kind: "odds", Category: CatCard, Card: subject, AtLeast: atLeast, Draws: draws, ByTurn: byTurn}, nil
	}
	return Query{}, ErrNoParse
}

// looksLikeName reports whether a subject plausibly names a card: any
// capitalised word, or a quoted span.
func looksLikeName(s string) bool {
	if strings.HasPrefix(s, `"`) || strings.HasPrefix(s, "'") {
		return true
	}
	for _, word := range strings.Fields(s) {
		letters := strings.TrimFunc(word, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z')
		})
		if letters == "" {
			continue
		}
		if first := rune(letters[0]); first >= 'A' && first <= 'Z' {
			return true
		}
	}
	return false
}

// numberWord parses a digit or small ordinal capture.
func numberWord(s string, d int) int {
	s = strings.ToLower(strings.TrimSpace(s))
	if n, ok := wordNumbers[s]; ok {
		return n
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return d
	}
	return n
}
