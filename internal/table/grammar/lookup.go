package grammar

// The state layer: everything a rule resolves against the current game.
// This is where "resolved against the current game state" lives — seats
// by spoken name, battlefield objects by card name, the land-versus-spell
// decision for "play X", token vocabularies, and the pronoun referent.
// Every lookup is deterministic: same state, same tokens, same answer,
// with the confidence cut when a tie-break chose among candidates.

import (
	"context"
	"strings"
	"unicode"

	"github.com/madeofpendletonwool/grimoire/internal/table/engine"
)

// game carries one parse's context: the state, the speaker, and the name
// seam. Rules are methods so the lookups stay in one place.
type game struct {
	ctx   context.Context
	st    *engine.State
	seat  int
	names Names
}

// resolveName runs the seam, memoizing nothing: one utterance, one name.
func (g *game) resolveName(spoken string) NameInfo {
	if g.names == nil || strings.TrimSpace(spoken) == "" {
		return NameInfo{}
	}
	return g.names.ResolveName(g.ctx, g.seat, spoken)
}

// foldName is the compare-under: lowercase, letters and digits only.
// "Atraxa, Praetors' Voice" and "atraXa praetors voice" agree.
func foldName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

/* ---------- seats ---------- */

// seatWords are the pronouns a speaker uses about seats: themselves.
var seatWords = map[string]bool{"me": true, "myself": true, "i": true}

// seatByToken resolves one spoken word to a seat: the pronouns for the
// speaker, then the seated names — exact (folded) first, then a unique
// prefix ("Bob" for "Bobby"). Two players whose names share the spoken
// prefix are ambiguous, and ambiguity here is a no-parse, never a guess.
func (g *game) seatByToken(w string) (int, bool) {
	if seatWords[w] {
		return g.seat, true
	}
	fw := foldName(w)
	if fw == "" {
		return 0, false
	}
	seats := make([]int, 0, len(g.st.Order))
	seats = append(seats, g.st.Order...)
	if len(seats) == 0 {
		for s := range g.st.Seats {
			seats = append(seats, s)
		}
		sortInts(seats)
	}
	for _, s := range seats {
		if p := g.st.Seats[s]; p != nil && foldName(p.Name) == fw {
			return s, true
		}
	}
	prefix := -1
	for _, s := range seats {
		p := g.st.Seats[s]
		if p == nil || p.Name == "" {
			continue
		}
		if strings.HasPrefix(foldName(p.Name), fw) {
			if prefix != -1 {
				return 0, false // two seats answer to that prefix
			}
			prefix = s
		}
	}
	if prefix != -1 {
		return prefix, true
	}
	return 0, false
}

func sortInts(v []int) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}

/* ---------- battlefield objects ---------- */

// SeatBySpoken resolves a spoken seat reference against the state —
// the exported half of seatByToken, for the model fallback's gate
// (MAD-331): a model that names a target by name is resolved by the
// same deterministic lookup the grammar trusts, never by a guess.
func SeatBySpoken(st *engine.State, seat int, spoken string) (int, bool) {
	if st == nil {
		return 0, false
	}
	g := &game{ctx: context.Background(), st: st, seat: seat}
	return g.seatByToken(spoken)
}

// ObjectBySpoken finds the battlefield object a spoken card name means —
// the exported half of objectBySpan with no state preference, for the
// model fallback's gate: the model references objects by name and the
// engine resolves the name to an id. multi reports a name that matched
// several objects; the caller decides whether that is its business.
func ObjectBySpoken(st *engine.State, seat int, spoken string) (obj *engine.Object, multi bool, ok bool) {
	if st == nil {
		return nil, false, false
	}
	g := &game{ctx: context.Background(), st: st, seat: seat}
	ref, found := g.objectBySpan(spoken, nil)
	if !found {
		return nil, false, false
	}
	return ref.obj, ref.multi, true
}

// objRef is one name's answer from the battlefield: the object, whether
// the name matched several (confidence territory), and whether it matched
// at all.
type objRef struct {
	obj   *engine.Object
	multi bool
}

// objectBySpan finds a battlefield object by spoken name. The spoken form
// is tried folded against every object's card name and token name; if
// nothing matches, the span goes through the name seam and the canonical
// name is tried the same way. Ranking is deterministic: the speaker's own
// objects beat everyone else's, the preferred state wins ties ("tap Sol
// Ring" prefers an untapped one, because tapping a tapped one is not an
// action), unphased beats phased, and lowest id breaks what is left.
func (g *game) objectBySpan(spoken string, prefer func(*engine.Object) bool) (objRef, bool) {
	candidates := g.foldCandidates(spoken)
	if len(candidates) == 0 {
		return objRef{}, false
	}
	matches := 0
	var best *engine.Object
	for id := int64(1); id < g.st.NextObject; id++ {
		o, ok := g.st.Objects[id]
		if !ok || o.Zone != engine.ZoneBattlefield {
			continue
		}
		folded := foldName(o.Identity.Card)
		tokenName := ""
		if o.Identity.Token != nil {
			tokenName = foldName(o.Identity.Token.Name)
		}
		hit := false
		for _, c := range candidates {
			if (folded != "" && folded == c) || (tokenName != "" && tokenName == c) {
				hit = true
				break
			}
		}
		if !hit {
			continue
		}
		matches++
		if best == nil || g.before(o, best, prefer) {
			best = o
		}
	}
	if best == nil {
		return objRef{}, false
	}
	return objRef{obj: best, multi: matches > 1}, true
}

// before reports whether a sorts ahead of b for a spoken-name match:
// the preferred state first (tapping an untapped one, milling an
// existing one), then the speaker's own objects, then unphased, then
// lowest id. Pure function, no weights to mis-tune.
func (g *game) before(a, b *engine.Object, prefer func(*engine.Object) bool) bool {
	pa, pb := prefer != nil && prefer(a), prefer != nil && prefer(b)
	if pa != pb {
		return pa
	}
	ca, cb := a.Controller == g.seat, b.Controller == g.seat
	if ca != cb {
		return ca
	}
	if a.Phased != b.Phased {
		return !a.Phased
	}
	return a.ID < b.ID
}

// foldCandidates is the spoken span plus its resolved canonical name.
func (g *game) foldCandidates(spoken string) []string {
	out := []string{foldName(spoken)}
	if res := g.resolveName(spoken); res.Card != "" {
		if f := foldName(res.Card); f != out[0] {
			out = append(out, f)
		}
	}
	return out
}

// referent resolves "it"/"this"/"that": the speaker's most recent
// battlefield object — the thing they just put down is the thing "this"
// means at a table. No objects, no referent, no parse.
func (g *game) referent() (*engine.Object, bool) {
	var best *engine.Object
	for id := int64(1); id < g.st.NextObject; id++ {
		o, ok := g.st.Objects[id]
		if !ok || o.Zone != engine.ZoneBattlefield || o.Phased || o.Controller != g.seat {
			continue
		}
		if best == nil || o.ID > best.ID {
			best = o
		}
	}
	return best, best != nil
}

// isBattlefieldCreature reports whether an object is a creature the
// engine's computed characteristics still call one.
func (g *game) isBattlefieldCreature(o *engine.Object) bool {
	return o != nil && o.Zone == engine.ZoneBattlefield && g.st.IsCreature(o.ID)
}

/* ---------- the land decision ---------- */

// canonicalBasics is the proper-case spelling of the six basics — a
// "Forest" spoken with no deck and no index is still a Forest, by rule.
var canonicalBasics = map[string]string{
	"forest": "Forest", "island": "Island", "mountain": "Mountain",
	"plains": "Plains", "swamp": "Swamp", "wastes": "Wastes",
}

// canonicalBasic returns the proper-case basic for a folded spoken span,
// snow-covered included, or "".
func canonicalBasic(folded string) string {
	if c, ok := canonicalBasics[folded]; ok {
		return c
	}
	if rest := strings.TrimPrefix(folded, "snowcovered"); rest != folded {
		if c, ok := canonicalBasics[rest]; ok {
			return "Snow-Covered " + c
		}
	}
	return ""
}

// isBasicLandSpan matches a basic land, snow-covered included.
func isBasicLandSpan(folded string) bool {
	return canonicalBasic(folded) != ""
}

// isLandType reports whether a type-line word set contains Land.
func isLandType(types []string) bool {
	for _, t := range types {
		if strings.EqualFold(strings.TrimSpace(t), "land") {
			return true
		}
	}
	return false
}

// landVerdict decides "play X": is it a land drop or a cast? The answer,
// in order of certainty: the basics (by rule), a same-named object on the
// battlefield whose declared types say Land (by state), the resolved
// card's types (by card data, when the seam knows them). Types unknown
// means the cast reading stands — at reduced confidence, because the
// tracker could not verify it — and never silently the land reading,
// because a land pushed onto the stack is the wrong parse.
// The second return is the confidence the verdict earned.
func (g *game) landVerdict(spoken string) (isLand bool, conf float64, types []string) {
	folded := foldName(spoken)
	if isBasicLandSpan(folded) {
		return true, ConfCertain, []string{"Land"}
	}
	cands := g.foldCandidates(spoken)
	for id := int64(1); id < g.st.NextObject; id++ {
		o, ok := g.st.Objects[id]
		if !ok || o.Zone != engine.ZoneBattlefield {
			continue
		}
		if o.Identity.Card == "" {
			continue
		}
		f := foldName(o.Identity.Card)
		for _, c := range cands {
			if f == c && isLandType(o.Base.Types) {
				return true, ConfDerived, o.Base.Types
			}
		}
	}
	res := g.resolveName(spoken)
	if len(res.Types) > 0 {
		if isLandType(res.Types) {
			return true, res.Confidence, res.Types
		}
		return false, res.Confidence, res.Types
	}
	if res.Card == "" {
		// Unresolved and not a basic: the shape is honest but the card is
		// not known. No parse — land-versus-spell cannot be decided.
		return false, 0, nil
	}
	// Resolved, types unknown (deck-scoped resolution): cast, held down
	// to the multi band because the land reading could not be excluded.
	return false, ConfMulti, nil
}

/* ---------- token vocabulary ---------- */

// artifactTokens are the artifact-token nouns common enough to say with
// no power/toughness: matched against singular and plural.
var artifactTokens = map[string]bool{
	"treasure": true, "treasures": true, "food": true, "clue": true,
	"clues": true, "blood": true, "bloods": true, "gold": true,
	"golds": true, "map": true, "maps": true, "powerstone": true,
	"powerstones": true,
}

// isArtifactTokenNoun reports a known artifact-token noun.
func isArtifactTokenNoun(w string) bool { return artifactTokens[w] }

// tokenKeywords are the keywords a token spec can carry when spoken with
// "with ..." — first/double strike arrive as two tokens.
var tokenKeywords = []string{
	"flying", "trample", "haste", "vigilance", "deathtouch", "lifelink",
	"menace", "indestructible", "ward", "reach", "defender", "prowess",
	"first strike", "double strike",
}

// parsePT reads a "P/T" token ("1/1", "4/4").
func parsePT(t string) (power, toughness int, ok bool) {
	i := strings.IndexByte(t, '/')
	if i <= 0 || i+1 >= len(t) {
		return 0, 0, false
	}
	p, tok := t[:i], t[i+1:]
	if !isDigits(p) || !isDigits(tok) {
		return 0, 0, false
	}
	return atoi(p), atoi(tok), true
}

func atoi(s string) int {
	v := 0
	for _, r := range s {
		v = v*10 + int(r-'0')
	}
	return v
}

// isPTCounter reports a "+1/+1"-style counter token, and normalizes it:
// signs explicit when spoken, assumed positive when not ("1/1 counter").
func isPTCounter(t string) (string, bool) {
	sign := ""
	s := t
	if s != "" && (s[0] == '+' || s[0] == '-') {
		sign = s[:1]
		s = s[1:]
	}
	i := strings.IndexByte(s, '/')
	if i <= 0 || i+1 >= len(s) {
		return "", false
	}
	p, tok := s[:i], s[i+1:]
	pSign, tSign := "+", "+"
	if p != "" && (p[0] == '+' || p[0] == '-') {
		pSign, p = p[:1], p[1:]
	} else if sign != "" {
		pSign = sign
	}
	if tok != "" && (tok[0] == '+' || tok[0] == '-') {
		tSign, tok = tok[:1], tok[1:]
	} else if sign != "" {
		tSign = sign
	}
	if !isDigits(p) || !isDigits(tok) {
		return "", false
	}
	return pSign + p + "/" + tSign + tok, true
}

// singularize handles the plural token nouns a table says: ies→y,
// ves→f, s/es off the end.
func singularize(w string) string {
	switch {
	case strings.HasSuffix(w, "ies") && len(w) > 3:
		return w[:len(w)-3] + "y"
	case strings.HasSuffix(w, "ves") && len(w) > 3:
		return w[:len(w)-3] + "f"
	case strings.HasSuffix(w, "es") && len(w) > 3:
		stem := w[:len(w)-2]
		if strings.HasSuffix(stem, "s") || strings.HasSuffix(stem, "x") ||
			strings.HasSuffix(stem, "z") || strings.HasSuffix(stem, "ch") ||
			strings.HasSuffix(stem, "sh") {
			return stem
		}
		return w[:len(w)-1]
	case strings.HasSuffix(w, "s") && len(w) > 1:
		return w[:len(w)-1]
	}
	return w
}

// playerCounters are the counter names spoken without the word
// "counter": the ones a whole product decided are first-class.
var playerCounters = map[string]bool{
	"energy": true, "experience": true, "poison": true, "rad": true,
	"rads": true, "ticket": true, "tickets": true,
}

// normalizePlayerCounter folds a spoken counter name to its singular
// engine spelling.
func normalizePlayerCounter(w string) (string, bool) {
	if playerCounters[w] {
		return singularize(w), true
	}
	return "", false
}

/* ---------- cast helpers ---------- */

// castAction builds the CAST for a resolved or spoken name: canonical
// card when the seam resolved it, the spoken span otherwise (never a
// guess at a different card), base characteristics when types are known,
// and the command zone as the source when the speaker's commander sits
// there under that name.
func (g *game) castAction(spoken string, targets []engine.Target) (engine.Action, bool) {
	card, conf := strings.TrimSpace(spoken), ConfUnresolved
	var types []string
	if res := g.resolveName(spoken); res.Card != "" {
		card, conf, types = res.Card, res.Confidence, res.Types
	} else if card == "" || !likelyName(card) {
		return engine.Action{}, false
	}
	a := engine.Action{Kind: engine.ActionCast, Card: card, Targets: targets, Confidence: conf}
	if len(types) > 0 {
		a.Base = &engine.BaseChars{Name: card, Types: types}
	}
	if p := g.st.Seats[g.seat]; p != nil && p.Commander != "" && foldName(p.Commander) == foldName(card) {
		for _, o := range g.commandZoneObjects(g.seat) {
			if foldName(o.Identity.Card) == foldName(card) {
				a.FromZone = engine.ZoneCommand
				break
			}
		}
	}
	return a, true
}

func (g *game) commandZoneObjects(seat int) []*engine.Object {
	return g.st.ZoneObjects(seat, engine.ZoneCommand)
}

// likelyName holds a spoken span to the shape of a card name: two or
// three words of letters. A cast whose object is not name-shaped is not
// a sentence anyone said.
func likelyName(s string) bool {
	words := 0
	for _, w := range strings.Fields(s) {
		if w == "" {
			continue
		}
		letters := false
		for _, r := range w {
			if unicode.IsLetter(r) {
				letters = true
				break
			}
		}
		if !letters {
			return false
		}
		words++
	}
	return words >= 1 && words <= 5
}
