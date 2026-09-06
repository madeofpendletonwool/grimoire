// Package dice is the dice engine (MAD-420, stage 3 of MAD-417): a real
// formula parser, kept-dice expressions, advantage and disadvantage, and a
// counter-based seeded RNG that makes every roll reproducible from
// (seed, nonce, formula) — the property Stage 9's replay needs and the
// golden files pin.
//
// The grammar (whitespace-tolerant, `d`/`kh`/`kl` case-insensitive):
//
//	expr   := ['+'|'-'] term (('+'|'-') term)*
//	term   := dice | number
//	dice   := count? 'd' sides keep?
//	keep   := ('kh' | 'kl') number?
//	count  := 1..100          sides := 2..1000
//	number := an integer, magnitude 10000 at most
//
// Addition and subtraction are the whole operator set — 5e formulas are
// sums, and a tight grammar is what lets malformed input be an error
// instead of a guess. Advantage and disadvantage are not syntax: they are
// a mode the engine applies to a formula carrying exactly one plain d20
// term, rewriting it to 2d20kh1 / 2d20kl1 (WithMode). `4d6kh3` — roll
// four, keep the highest three — is the stat-roll spelling; `2d20kl1` is
// disadvantage written out.
//
// The package is pure: no database, no wall clock, no network. Same seed,
// same nonce, same formula — same dice, forever.
package dice

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

/* ---------- the vocabulary ---------- */

// Roll visibility. A public roll lands in the shared party feed; a secret
// roll is DM-only, and its absence from every player-scoped read is the
// leak test's assertion. The words are the knowledge layer's own — dice
// become facts with visibility, not a widget bolted on.
const (
	VisibilityPublic = "public"
	VisibilitySecret = "secret"
)

// Roll modes — the advantage grammar.
const (
	ModeNone         = ""
	ModeAdvantage    = "advantage"
	ModeDisadvantage = "disadvantage"
)

// Context kinds — what the roll was for. A declared vocabulary, not free
// text: the feed groups by it, the export prints it, and "attack" being a
// keyword is what lets later stages hang automation off a roll.
const (
	ContextAttack     = "attack"
	ContextSave       = "save"
	ContextCheck      = "check"
	ContextDamage     = "damage"
	ContextInitiative = "initiative"
	ContextTable      = "table"
	ContextOther      = "other"
)

// validContexts is the declared set.
var validContexts = map[string]bool{
	ContextAttack: true, ContextSave: true, ContextCheck: true,
	ContextDamage: true, ContextInitiative: true, ContextTable: true,
	ContextOther: true,
}

// ValidContext reports whether kind is one of the declared context kinds.
func ValidContext(kind string) bool { return validContexts[kind] }

// Grammar limits. Generous for every real 5e formula, tight enough that a
// pasted paragraph is an error, not a computation.
const (
	maxTerms   = 20
	maxCount   = 100
	maxSides   = 1000
	maxFlat    = 10000
	maxFormula = 200
)

/* ---------- the parsed formula ---------- */

// Expr is a parsed formula: signed terms in source order.
type Expr struct {
	Terms []Term
}

// Term is one summand: a dice group or a flat number, with the sign that
// joined it to the expression. The first term may carry a leading minus.
type Term struct {
	Negative bool
	Dice     *DiceTerm // nil when the term is Flat
	Flat     int
}

// DiceTerm is one dice group: count dice of sides, keeping the highest
// (KeepLow false) or lowest count when Keep > 0.
type DiceTerm struct {
	Count   int
	Sides   int
	Keep    int  // 0 keeps every die
	KeepLow bool // kl instead of kh
}

// IsD20 reports whether the term is a single plain d20 — the one shape
// advantage and disadvantage may rewrite.
func (t Term) IsD20() bool {
	return t.Dice != nil && t.Dice.Count == 1 && t.Dice.Sides == 20 && t.Dice.Keep == 0
}

// String renders the normalized formula: lowercase, counts explicit,
// keeps spelled out. What the feed prints and the golden files pin.
func (e *Expr) String() string {
	var b strings.Builder
	for i, t := range e.Terms {
		if i == 0 {
			if t.Negative {
				b.WriteByte('-')
			}
		} else if t.Negative {
			b.WriteString(" - ")
		} else {
			b.WriteString(" + ")
		}
		if t.Dice != nil {
			fmt.Fprintf(&b, "%dd%d", t.Dice.Count, t.Dice.Sides)
			if t.Dice.Keep > 0 && t.Dice.Keep < t.Dice.Count {
				if t.Dice.KeepLow {
					b.WriteString("kl")
				} else {
					b.WriteString("kh")
				}
				b.WriteString(strconv.Itoa(t.Dice.Keep))
			}
		} else {
			b.WriteString(strconv.Itoa(t.Flat))
		}
	}
	return b.String()
}

// DiceCount is the total number of physical dice the expression rolls —
// capped by the grammar, checked before rolling.
func (e *Expr) DiceCount() int {
	n := 0
	for _, t := range e.Terms {
		if t.Dice != nil {
			n += t.Dice.Count
		}
	}
	return n
}

// Parse reads one formula. Every malformed input is an error naming the
// position and the problem — never a guess, never a silent repair.
func Parse(input string) (*Expr, error) {
	s := strings.TrimSpace(input)
	if s == "" {
		return nil, fmt.Errorf("empty formula")
	}
	if len(s) > maxFormula {
		return nil, fmt.Errorf("formula longer than %d characters", maxFormula)
	}
	p := &parser{src: s}
	return p.expr()
}

type parser struct {
	src string
	pos int
}

func (p *parser) expr() (*Expr, error) {
	e := &Expr{}
	sign := p.maybeSign()
	t, err := p.term(sign)
	if err != nil {
		return nil, err
	}
	e.Terms = append(e.Terms, t)
	for {
		p.ws()
		sign, ok := p.maybeSignInfix()
		if !ok {
			break
		}
		t, err := p.term(sign)
		if err != nil {
			return nil, err
		}
		e.Terms = append(e.Terms, t)
	}
	p.ws()
	if p.pos < len(p.src) {
		return nil, p.errf("unexpected %q", p.src[p.pos:])
	}
	if len(e.Terms) > maxTerms {
		return nil, fmt.Errorf("more than %d terms", maxTerms)
	}
	if e.DiceCount() > maxCount {
		return nil, fmt.Errorf("more than %d dice in one roll", maxCount)
	}
	return e, nil
}

// maybeSign consumes an optional leading sign.
func (p *parser) maybeSign() bool {
	p.ws()
	if p.pos < len(p.src) && (p.src[p.pos] == '+' || p.src[p.pos] == '-') {
		neg := p.src[p.pos] == '-'
		p.pos++
		return neg
	}
	return false
}

// maybeSignInfix consumes a +/- that joins two terms, reporting whether
// one was there.
func (p *parser) maybeSignInfix() (negative, ok bool) {
	if p.pos < len(p.src) && (p.src[p.pos] == '+' || p.src[p.pos] == '-') {
		neg := p.src[p.pos] == '-'
		p.pos++
		return neg, true
	}
	return false, false
}

func (p *parser) term(negative bool) (Term, error) {
	p.ws()
	start := p.pos
	for p.pos < len(p.src) && unicode.IsDigit(rune(p.src[p.pos])) {
		p.pos++
	}
	digits := p.src[start:p.pos]
	p.ws()
	if p.pos < len(p.src) && (p.src[p.pos] == 'd' || p.src[p.pos] == 'D') {
		p.pos++ // the d
		count := 1
		if digits != "" {
			n, err := strconv.Atoi(digits)
			if err != nil || n < 1 || n > maxCount {
				return Term{}, p.errAt(start, "dice count %q is not 1..%d", digits, maxCount)
			}
			count = n
		}
		sides, err := p.number("sides")
		if err != nil {
			return Term{}, err
		}
		if sides < 2 || sides > maxSides {
			return Term{}, p.errAt(start, "%dd%d: sides must be 2..%d", count, sides, maxSides)
		}
		dt := &DiceTerm{Count: count, Sides: sides}
		p.ws()
		k, low, ok, err := p.maybeKeep()
		if err != nil {
			return Term{}, err
		}
		if ok {
			if k < 1 || k > count {
				return Term{}, p.errAt(start, "%dd%d keep %d: keep must be 1..%d", count, sides, k, count)
			}
			dt.Keep, dt.KeepLow = k, low
		}
		return Term{Negative: negative, Dice: dt}, nil
	}
	if digits == "" {
		return Term{}, p.errAt(p.pos, "expected dice or number")
	}
	n, err := strconv.Atoi(digits)
	if err != nil || n > maxFlat || n < -maxFlat {
		return Term{}, p.errAt(start, "number %q is out of range ±%d", digits, maxFlat)
	}
	return Term{Negative: negative, Flat: n}, nil
}

// number reads one integer token, for sides counts and keep counts.
func (p *parser) number(what string) (int, error) {
	p.ws()
	start := p.pos
	for p.pos < len(p.src) && unicode.IsDigit(rune(p.src[p.pos])) {
		p.pos++
	}
	if p.src[start:p.pos] == "" {
		return 0, p.errAt(p.pos, "expected %s after d", what)
	}
	n, err := strconv.Atoi(p.src[start:p.pos])
	if err != nil || n > maxSides {
		return 0, p.errAt(start, "%s %q is out of range", what, p.src[start:p.pos])
	}
	return n, nil
}

// maybeKeep reads a kh/kl suffix with its optional count (kh alone keeps
// one — "2d20kh" is "2d20kh1", the advantage spelling).
func (p *parser) maybeKeep() (keep int, low, ok bool, err error) {
	rest := p.src[p.pos:]
	lower := strings.ToLower(rest)
	switch {
	case strings.HasPrefix(lower, "kh"):
		p.pos += 2
	case strings.HasPrefix(lower, "kl"):
		low = true
		p.pos += 2
	default:
		return 0, false, false, nil
	}
	keep = 1
	p.ws()
	start := p.pos
	for p.pos < len(p.src) && unicode.IsDigit(rune(p.src[p.pos])) {
		p.pos++
	}
	if digits := p.src[start:p.pos]; digits != "" {
		n, err := strconv.Atoi(digits)
		if err != nil {
			return 0, false, true, p.errAt(start, "keep count %q is out of range", digits)
		}
		keep = n
	}
	return keep, low, true, nil
}

func (p *parser) ws() {
	for p.pos < len(p.src) {
		switch p.src[p.pos] {
		case ' ', '\t':
			p.pos++
		default:
			return
		}
	}
}

func (p *parser) errAt(pos int, format string, args ...any) error {
	mark := pos
	if mark > len(p.src) {
		mark = len(p.src)
	}
	return fmt.Errorf("at %d: %s", mark, fmt.Sprintf(format, args...))
}

func (p *parser) errf(format string, args ...any) error {
	return p.errAt(p.pos, format, args...)
}

/* ---------- advantage and disadvantage ---------- */

// WithMode applies a mode to a parsed formula: advantage rewrites the
// single plain d20 term to 2d20kh1, disadvantage to 2d20kl1. A formula
// without exactly one plain d20 term cannot take a mode — "fireball
// damage with advantage" is not a thing, and the error says which term
// was in the way. The input expression is not mutated; the rewritten copy
// is returned.
func WithMode(e *Expr, mode string) (*Expr, error) {
	switch mode {
	case ModeNone:
		return e, nil
	case ModeAdvantage, ModeDisadvantage:
	default:
		return nil, fmt.Errorf("mode %q", mode)
	}
	d20 := -1
	for i, t := range e.Terms {
		if t.IsD20() {
			if d20 >= 0 {
				return nil, fmt.Errorf("advantage applies to one d20 term; %s has two", e.String())
			}
			d20 = i
		}
	}
	if d20 < 0 {
		return nil, fmt.Errorf("advantage applies to a d20 term; %s has none", e.String())
	}
	out := &Expr{Terms: append([]Term(nil), e.Terms...)}
	dt := *out.Terms[d20].Dice
	dt.Count, dt.Keep = 2, 1
	dt.KeepLow = mode == ModeDisadvantage
	out.Terms[d20].Dice = &dt
	return out, nil
}

/* ---------- the seeded RNG ---------- */

// splitmix64 is the mixer the whole engine stands on: one round steps the
// stream, and (seed, nonce) pair through it first so consecutive nonces
// start from unrelated states.
func splitmix64(x uint64) uint64 {
	x += 0x9E3779B97F4A7C15
	z := x
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}

// stream is one roll's deterministic value source. A roll never draws
// from a shared long-lived stream — replay must not depend on what was
// rolled before it — so each roll mints its stream from the campaign seed
// and its own nonce (the per-campaign seq the store assigns).
type stream struct{ state uint64 }

func newStream(seed, nonce int64) *stream {
	return &stream{state: splitmix64(uint64(seed) ^ splitmix64(uint64(nonce)))}
}

// die draws one value in 1..sides. The top 32 bits of a splitmix64 round
// are used because the low bits of a multiplicative hash are the weak
// ones; modulo bias at 2^32 values over at most 1000 sides is ~2e-7 of a
// percent, below any die's manufacturing tolerance.
func (s *stream) die(sides int) int {
	s.state = splitmix64(s.state)
	return int((s.state>>32)%uint64(sides)) + 1
}

/* ---------- the roll ---------- */

// Die is one physical die: its natural value and whether the keep clause
// kept it. Dropped dice stay in the record — the feed shows the 4 the
// advantage discarded, because the discard is half the drama.
type Die struct {
	Value int  `json:"value"`
	Kept  bool `json:"kept"`
}

// TermResult is one term's outcome: the dice (all of them, kept flagged),
// the subtotal of kept dice, and the running total's direction.
type TermResult struct {
	Kind     string `json:"kind"` // "dice" | "flat"
	Sign     int    `json:"sign"` // +1 | -1
	Count    int    `json:"count,omitempty"`
	Sides    int    `json:"sides,omitempty"`
	Keep     int    `json:"keep,omitempty"`
	KeepLow  bool   `json:"keep_low,omitempty"`
	Formula  string `json:"formula"`        // "2d20kh1", "3"
	Dice     []Die  `json:"dice,omitempty"` // natural dice, rolled order
	Subtotal int    `json:"subtotal"`       // kept dice sum, or the flat value
}

// RollResult is the complete outcome: every term, the modifier the flat
// terms add up to, the total, and the two flourishes a d20 can earn.
// Same seed, nonce and formula produce the same bytes — the golden files
// pin exactly that.
type RollResult struct {
	Formula   string       `json:"formula"`        // normalized, mode applied
	Mode      string       `json:"mode,omitempty"` // advantage | disadvantage
	Terms     []TermResult `json:"terms"`
	Modifier  int          `json:"modifier"` // sum of signed flat terms
	Total     int          `json:"total"`
	Natural20 bool         `json:"natural_20,omitempty"` // a kept d20 showed 20
	Natural1  bool         `json:"natural_1,omitempty"`  // a kept d20 showed 1
}

// Roll executes a formula on the (seed, nonce) stream. The formula is the
// mode-applied expression; mode is echoed for the record. Dice roll in
// term order, index order within a term — the order replay assumes.
func Roll(seed, nonce int64, e *Expr, mode string) *RollResult {
	res := &RollResult{Formula: e.String(), Mode: mode, Terms: make([]TermResult, 0, len(e.Terms))}
	st := newStream(seed, nonce)
	for _, t := range e.Terms {
		tr := TermResult{Sign: 1}
		if t.Negative {
			tr.Sign = -1
		}
		switch {
		case t.Dice != nil:
			d := t.Dice
			tr.Kind = "dice"
			tr.Count, tr.Sides, tr.Keep, tr.KeepLow = d.Count, d.Sides, d.Keep, d.KeepLow
			tr.Formula = diceFormula(d)
			tr.Dice = make([]Die, d.Count)
			for i := range tr.Dice {
				tr.Dice[i] = Die{Value: st.die(d.Sides), Kept: true}
			}
			keep := d.Keep
			if keep > 0 && keep < d.Count {
				markKept(tr.Dice, keep, d.KeepLow)
			}
			for _, die := range tr.Dice {
				if die.Kept {
					tr.Subtotal += die.Value
				}
			}
			if d.Sides == 20 && d.Count >= 1 {
				for _, die := range tr.Dice {
					if !die.Kept {
						continue
					}
					if die.Value == 20 {
						res.Natural20 = true
					}
					if die.Value == 1 {
						res.Natural1 = true
					}
				}
			}
		default:
			tr.Kind = "flat"
			tr.Formula = strconv.Itoa(t.Flat)
			tr.Subtotal = t.Flat
			res.Modifier += tr.Sign * t.Flat
		}
		res.Total += tr.Sign * tr.Subtotal
		res.Terms = append(res.Terms, tr)
	}
	return res
}

// diceFormula renders one dice term's own spelling.
func diceFormula(d *DiceTerm) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%dd%d", d.Count, d.Sides)
	if d.Keep > 0 && d.Keep < d.Count {
		if d.KeepLow {
			b.WriteString("kl")
		} else {
			b.WriteString("kh")
		}
		b.WriteString(strconv.Itoa(d.Keep))
	}
	return b.String()
}

// markKept flags the kept dice in place. Deterministic under ties: dice
// are ordered ascending by value with index order preserved among equals,
// the highest (kh) or lowest (kl) k positions win — so kh prefers the
// later of two equal dice and kl the earlier, and the same input always
// produces the same kept flags.
func markKept(dice []Die, keep int, low bool) {
	order := make([]int, len(dice))
	for i := range order {
		order[i] = i
	}
	// insertion sort, stable by construction: equal values keep index order
	for i := 1; i < len(order); i++ {
		for j := i; j > 0; j-- {
			a, b := dice[order[j-1]].Value, dice[order[j]].Value
			if a < b || (a == b && order[j-1] < order[j]) {
				break
			}
			order[j-1], order[j] = order[j], order[j-1]
		}
	}
	winners := order[len(order)-keep:]
	if low {
		winners = order[:keep]
	}
	for i := range dice {
		dice[i].Kept = false
	}
	for _, w := range winners {
		dice[w].Kept = true
	}
}
