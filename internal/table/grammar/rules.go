package grammar

// The shape families, most specific first. Every rule either claims the
// whole utterance — returning an Action — or declines, and the first
// claim wins; the default is no-parse. The vocabulary is deliberately
// small: these are the phrasings a Commander table actually says (the
// issue's list plus the forms heard around it), and anything outside it
// belongs to the LLM fallback (MAD-331), not to a guess here.

import (
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/table/engine"
)

/* ---------- flow: pass · go · resolves · take it · next ---------- */

// flowPhrases are the exact-shape flow utterances. "pass the turn" and
// "your turn" are held slightly under certain: one PASS_PRIORITY is the
// right first move, not the whole walk, and the log should be trusted
// about what it shows.
var flowPhrases = map[string]struct {
	kind engine.ActionKind
	conf float64
}{
	"pass":           {engine.ActionPassPriority, ConfCertain},
	"pass priority":  {engine.ActionPassPriority, ConfCertain},
	"go":             {engine.ActionPassPriority, ConfCertain},
	"resolves":       {engine.ActionPassPriority, ConfCertain},
	"resolve":        {engine.ActionPassPriority, ConfCertain},
	"resolve it":     {engine.ActionPassPriority, ConfCertain},
	"resolved":       {engine.ActionPassPriority, ConfCertain},
	"let it resolve": {engine.ActionPassPriority, ConfCertain},
	"that resolves":  {engine.ActionPassPriority, ConfCertain},
	"take it":        {engine.ActionPassPriority, ConfCertain},
	"no response":    {engine.ActionPassPriority, ConfCertain},
	"no responses":   {engine.ActionPassPriority, ConfCertain},
	"no effects":     {engine.ActionPassPriority, ConfCertain},
	"nothing":        {engine.ActionPassPriority, ConfCertain},
	"nope":           {engine.ActionPassPriority, ConfCertain},
	"no blocks":      {engine.ActionPassPriority, ConfCertain},
	"no blockers":    {engine.ActionPassPriority, ConfCertain},
	"no attacks":     {engine.ActionPassPriority, ConfCertain},
	"no attackers":   {engine.ActionPassPriority, ConfCertain},
	"your turn":      {engine.ActionPassPriority, ConfDerived},
	"pass the turn":  {engine.ActionPassPriority, ConfDerived},
	"next":           {engine.ActionAdvance, ConfReference},
	"next step":      {engine.ActionAdvance, ConfCertain},
	"move on":        {engine.ActionAdvance, ConfReference},
	"go to combat":   {engine.ActionAdvance, ConfDerived},
	"move to combat": {engine.ActionAdvance, ConfDerived},
	"concede":        {engine.ActionConcede, ConfCertain},
	"i'm dead":       {engine.ActionConcede, ConfCertain},
	"i'm out":        {engine.ActionConcede, ConfCertain},
}

func (g *game) parseFlow(toks []string) (engine.Action, bool) {
	// "[seat] takes it" — Bob waving the top of the stack through. The
	// passer is the named seat, so the action carries that seat.
	if len(toks) == 3 && (toks[1] == "takes" || toks[1] == "take") && toks[2] == "it" {
		if seat, ok := g.seatByToken(toks[0]); ok {
			return engine.Action{Kind: engine.ActionPassPriority, Seat: seat,
				Confidence: ConfReference}, true
		}
	}
	f, ok := flowPhrases[strings.Join(toks, " ")]
	if !ok {
		return engine.Action{}, false
	}
	return engine.Action{Kind: f.kind, Confidence: f.conf}, true
}

/* ---------- counters and flags ---------- */

// parseCounterFlag owns every utterance with an explicit counter or flag
// word in it — the most specific vocabulary at the table, so it runs
// before the number-shaped rules that would otherwise eat "two energy".
func (g *game) parseCounterFlag(toks []string) (engine.Action, bool) {
	if a, ok := g.parseObjectCounter(toks); ok {
		return a, true
	}
	if a, ok := g.parsePlayerCounter(toks); ok {
		return a, true
	}
	return g.parseFlag(toks)
}

// parseObjectCounter: "(put) (a|N) +1/+1 (counters)? on Atraxa" and the
// loyalty shorthands "N loyalty on Jace" / "+2 on Jace".
func (g *game) parseObjectCounter(toks []string) (engine.Action, bool) {
	if toks[0] == "put" && len(toks) > 1 {
		toks = toks[1:]
	}
	i := 0
	count := 1
	if n, next, ok := articleCount(toks, 0); ok && next < len(toks) {
		count, i = n, next
	}
	name := ""
	if i < len(toks) {
		if pt, ok := isPTCounter(toks[i]); ok {
			name, i = pt, i+1
		}
	}
	if name == "" && i < len(toks) {
		// "two loyalty on Jace" / "+2 on Jace": a counted or signed
		// loyalty add, the planeswalker's own arithmetic.
		if toks[i] == "loyalty" && count > 0 {
			name, i = "loyalty", i+1
		} else if len(toks[i]) > 1 && toks[i][0] == '+' && isDigits(toks[i][1:]) {
			count = atoi(toks[i][1:])
			name, i = "loyalty", i+1
			if i < len(toks) && toks[i] == "loyalty" {
				i++
			}
		}
	}
	if name == "" {
		return engine.Action{}, false
	}
	if i < len(toks) && (toks[i] == "counter" || toks[i] == "counters") {
		i++
	}
	if i >= len(toks) || toks[i] != "on" || i+1 >= len(toks) {
		return engine.Action{}, false
	}
	ref, ok := g.objectRef(g.stripArticles(toks[i+1:]), nil)
	if !ok {
		return engine.Action{}, false
	}
	return engine.Action{Kind: engine.ActionAdjustCounters, OnObject: ref.obj.ID,
		CounterName: name, Delta: count, Confidence: ref.confidence()}, true
}

// parsePlayerCounter: "two energy" · "gain two energy" · "3 poison on
// Bob" · "Bob takes two poison" — the counters that live on seats.
func (g *game) parsePlayerCounter(toks []string) (engine.Action, bool) {
	// "<seat> takes/gains/gets N <counter>"
	if len(toks) > 3 {
		if seat, ok := g.seatByToken(toks[0]); ok {
			if toks[1] == "takes" || toks[1] == "gains" || toks[1] == "gets" {
				if n, next, okN := numberAt(toks, 2); okN {
					if name, okC := normalizePlayerCounter(toks[next]); okC && next+1 == len(toks) {
						return engine.Action{Kind: engine.ActionAdjustCounters, TargetSeat: seat,
							CounterName: name, Delta: n, Confidence: ConfReference}, true
					}
				}
			}
		}
	}
	i := 0
	switch toks[0] {
	case "gain", "get", "add", "take":
		i = 1
	}
	n, next, ok := articleCount(toks, i)
	if !ok || next >= len(toks) {
		return engine.Action{}, false
	}
	name, ok := normalizePlayerCounter(toks[next])
	if !ok {
		return engine.Action{}, false
	}
	seat, conf := g.seat, ConfCertain
	if next+1 < len(toks) && (toks[next+1] == "on" || toks[next+1] == "to") && next+3 == len(toks) {
		if s, okS := g.seatByToken(toks[next+2]); okS {
			seat, conf = s, ConfReference
		}
	} else if next+1 != len(toks) {
		return engine.Action{}, false
	}
	return engine.Action{Kind: engine.ActionAdjustCounters, TargetSeat: seat,
		CounterName: name, Delta: n, Confidence: conf}, true
}

// parseFlag: monarch, initiative, city's blessing — the player statuses
// the engine keeps as exclusive flags.
func (g *game) parseFlag(toks []string) (engine.Action, bool) {
	flag := func(seat int, name string, conf float64) (engine.Action, bool) {
		return engine.Action{Kind: engine.ActionSetFlag, TargetSeat: seat,
			Flag: name, Value: "true", Confidence: conf}, true
	}
	switch strings.Join(toks, " ") {
	case "i'm the monarch", "i am the monarch", "im the monarch":
		return flag(g.seat, "monarch", ConfCertain)
	case "take the crown", "have the crown", "i'm the crown":
		return flag(g.seat, "monarch", ConfDerived)
	case "take the initiative", "have the initiative":
		return flag(g.seat, "initiative", ConfCertain)
	case "city's blessing", "have the city's blessing", "the city's blessing":
		return flag(g.seat, "city's blessing", ConfDerived)
	}
	// "<seat> is/has/takes the monarch|initiative"
	if len(toks) >= 3 {
		if seat, ok := g.seatByToken(toks[0]); ok {
			rest := strings.Join(toks[1:], " ")
			for _, lead := range []string{"is ", "has ", "takes "} {
				rest = strings.TrimPrefix(rest, lead)
			}
			switch rest {
			case "the monarch":
				return flag(seat, "monarch", ConfReference)
			case "the initiative":
				return flag(seat, "initiative", ConfReference)
			}
		}
	}
	return engine.Action{}, false
}

/* ---------- tokens ---------- */

// parseToken: "make two Treasures" · "three 1/1 soldiers" · "create a
// 4/4 Beast with trample". Bare (verb-less) forms parse only with a
// power/toughness or a known artifact-token noun — "two goblins" with no
// verb and no stats is a statement about the board, not an instruction.
func (g *game) parseToken(toks []string) (engine.Action, bool) {
	i := 0
	verb := false
	switch toks[0] {
	case "make", "create", "makes", "creates", "making", "creating":
		verb = true
		i = 1
	}
	if i >= len(toks) {
		return engine.Action{}, false
	}
	count := 1
	if n, next, ok := articleCount(toks, i); ok && next < len(toks) {
		count, i = n, next
	}
	if count < 1 {
		return engine.Action{}, false
	}
	power, toughness, hasPT := 0, 0, false
	if i < len(toks) {
		if p, t, ok := parsePT(toks[i]); ok {
			power, toughness, hasPT = p, t, true
			i++
		}
	}
	if i >= len(toks) {
		return engine.Action{}, false
	}
	noun := toks[i]
	i++
	if i < len(toks) && (toks[i] == "tokens" || toks[i] == "token") {
		i++
	}
	spec := &engine.TokenSpec{}
	switch {
	case hasPT:
		spec.Power, spec.Toughness = &power, &toughness
		spec.Types = []string{"Creature"}
		spec.Name = capitalize(singularize(noun))
	case isArtifactTokenNoun(noun):
		spec.Types = []string{"Artifact"}
		spec.Name = capitalize(singularize(noun))
	case verb:
		// A spoken noun with the verb but no stats and no vocabulary:
		// honest about what is unknown, which is everything but the name.
		spec.Name = capitalize(singularize(noun))
	default:
		return engine.Action{}, false
	}
	// "with flying and trample" — keywords said along with the token.
	if i < len(toks) && toks[i] == "with" {
		spec.Keywords = parseKeywordTail(toks[i+1:])
	}
	if spec.Name == "" {
		return engine.Action{}, false
	}
	conf := ConfCertain
	if !verb {
		conf = ConfDerived // "three 1/1 soldiers" with no verb: right, but quieter
	}
	return engine.Action{Kind: engine.ActionCreateToken, Count: count, Token: spec,
		Confidence: conf}, true
}

// parseKeywordTail greedily matches the shared keyword vocabulary,
// single words and the two strike phrases.
func parseKeywordTail(toks []string) []string {
	var out []string
	for i := 0; i < len(toks); i++ {
		if toks[i] == "and" {
			continue
		}
		if i+1 < len(toks) {
			two := toks[i] + " " + toks[i+1]
			if two == "first strike" || two == "double strike" {
				out = append(out, two)
				i++
				continue
			}
		}
		for _, k := range tokenKeywords {
			if k == toks[i] {
				out = append(out, toks[i])
				break
			}
		}
	}
	return out
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

/* ---------- tap and untap ---------- */

// parseTapUntap: "tap Sol Ring" · "untap everything" · "tap it". A
// trailing "for N" (the mana amount) is tolerated and dropped — the tap
// is the event; floating mana is not modelled.
func (g *game) parseTapUntap(toks []string) (engine.Action, bool) {
	tapped := false
	switch toks[0] {
	case "tap", "tapping":
		tapped = true
	case "untap", "untapping":
	default:
		return engine.Action{}, false
	}
	// "tap Sol Ring for 2": the for-tail is mana, not a target.
	if len(toks) >= 2 && toks[len(toks)-2] == "for" {
		if _, next, ok := numberAt(toks, len(toks)-1); ok && next == len(toks) {
			toks = toks[:len(toks)-2]
		}
	}
	if len(toks) == 1 {
		if tapped {
			return engine.Action{}, false // "tap" alone taps nothing in particular
		}
		// "untap" alone is the untap step's convenience.
		return engine.Action{Kind: engine.ActionUntap, All: true, Confidence: ConfDerived}, true
	}
	switch strings.Join(toks[1:], " ") {
	case "everything", "all", "all my stuff", "my stuff":
		kind := engine.ActionUntap
		if tapped {
			kind = engine.ActionTap
		}
		return engine.Action{Kind: kind, All: true, Confidence: ConfCertain}, true
	case "it", "this", "that":
		o, ok := g.referent()
		if !ok {
			return engine.Action{}, false
		}
		kind := engine.ActionUntap
		if tapped {
			kind = engine.ActionTap
		}
		return engine.Action{Kind: kind, Object: o.ID, Confidence: ConfReferent}, true
	}
	// Prefer the state change the verb asks for: tapping an untapped
	// one, untapping a tapped one — a no-op tap is a rejection, not an
	// action.
	var prefer func(*engine.Object) bool
	if tapped {
		prefer = func(o *engine.Object) bool { return !o.Tapped }
	} else {
		prefer = func(o *engine.Object) bool { return o.Tapped }
	}
	ref, ok := g.objectRef(g.stripArticles(toks[1:]), prefer)
	if !ok {
		return engine.Action{}, false
	}
	kind := engine.ActionUntap
	if tapped {
		kind = engine.ActionTap
	}
	return engine.Action{Kind: kind, Object: ref.obj.ID, Confidence: ref.confidence()}, true
}

/* ---------- zone moves: sac · destroy · exile · bounce · draw · mill ---------- */

// parseZone owns the leaves-and-cards shapes: the four ways a battlefield
// object goes somewhere ("sac this", "exile it", "bounce the Bear",
// "destroy Krenko") and the library traffic (draw, mill).
func (g *game) parseZone(toks []string) (engine.Action, bool) {
	if a, ok := g.parseMove(toks); ok {
		return a, true
	}
	if a, ok := g.parseDraw(toks); ok {
		return a, true
	}
	return g.parseMill(toks)
}

var moveVerbs = map[string]struct {
	zone  engine.Zone
	cause string
}{
	"sac":         {engine.ZoneGraveyard, "sacrifice"},
	"sacrifice":   {engine.ZoneGraveyard, "sacrifice"},
	"sacrificing": {engine.ZoneGraveyard, "sacrifice"},
	"destroy":     {engine.ZoneGraveyard, "destroy"},
	"destroying":  {engine.ZoneGraveyard, "destroy"},
	"exile":       {engine.ZoneExile, "exile"},
	"exiling":     {engine.ZoneExile, "exile"},
	"bounce":      {engine.ZoneHand, "bounce"},
	"bouncing":    {engine.ZoneHand, "bounce"},
	"return":      {engine.ZoneHand, "bounce"},
}

func (g *game) parseMove(toks []string) (engine.Action, bool) {
	mv, ok := moveVerbs[toks[0]]
	if !ok || len(toks) < 2 {
		return engine.Action{}, false
	}
	// "return ... to hand" tolerates a destination tail.
	if toks[0] == "return" {
		for i := len(toks) - 1; i > 1; i-- {
			if toks[i] == "hand" || toks[i] == "owner's" || toks[i] == "owners" {
				if toks[i-1] == "to" || toks[i-1] == "their" {
					toks = toks[:i-1]
					break
				}
			}
		}
		if len(toks) < 2 {
			return engine.Action{}, false
		}
	}
	rest := g.stripArticles(toks[1:])
	if len(rest) == 1 && (rest[0] == "it" || rest[0] == "this" || rest[0] == "that") {
		o, ok := g.referent()
		if !ok {
			return engine.Action{}, false
		}
		return engine.Action{Kind: engine.ActionMoveZone, Object: o.ID,
			ToZone: mv.zone, Cause: mv.cause, Confidence: ConfReferent}, true
	}
	ref, ok := g.objectRef(rest, nil)
	if !ok {
		return engine.Action{}, false
	}
	return engine.Action{Kind: engine.ActionMoveZone, Object: ref.obj.ID,
		ToZone: mv.zone, Cause: mv.cause, Confidence: ref.confidence()}, true
}

// parseDraw: "draw" · "draw two" · "draw a card" · "draw for turn" ·
// "Bob draws 3". Identities never ride along here; a draw spoken with
// its card named is 4c's business, not a guess about what was drawn.
func (g *game) parseDraw(toks []string) (engine.Action, bool) {
	seat, rest := g.seat, toks
	if len(toks) > 1 {
		if s, ok := g.seatByToken(toks[0]); ok {
			switch toks[1] {
			case "draw", "draws", "drawing":
				seat, rest = s, toks[1:]
			}
		}
	}
	if rest[0] != "draw" && rest[0] != "draws" && rest[0] != "drawing" {
		return engine.Action{}, false
	}
	rest = rest[1:]
	if len(rest) == 0 {
		return engine.Action{Kind: engine.ActionDraw, Seat: seat, Count: 1,
			Confidence: ConfCertain}, true
	}
	if n, next, ok := articleCount(rest, 0); ok && n > 0 {
		for next < len(rest) && (rest[next] == "cards" || rest[next] == "card") {
			next++
		}
		if next == len(rest) {
			return engine.Action{Kind: engine.ActionDraw, Seat: seat, Count: n,
				Confidence: ConfCertain}, true
		}
	}
	switch strings.Join(rest, " ") {
	case "for turn", "for the turn":
		return engine.Action{Kind: engine.ActionDraw, Seat: seat, Count: 1,
			Confidence: ConfCertain}, true
	}
	return engine.Action{}, false
}

// parseMill: "mill three" · "mill Bob for two" · "Bob mills 3".
func (g *game) parseMill(toks []string) (engine.Action, bool) {
	target, rest := g.seat, toks
	if len(toks) > 1 {
		if s, ok := g.seatByToken(toks[0]); ok {
			switch toks[1] {
			case "mill", "mills", "milling":
				target, rest = s, toks[1:]
			}
		}
	}
	if rest[0] != "mill" && rest[0] != "mills" && rest[0] != "milling" {
		return engine.Action{}, false
	}
	rest = rest[1:]
	if len(rest) == 0 {
		return engine.Action{}, false
	}
	// "mill Bob for two" / "mill Bob two" / "mill Bob"
	if seat, ok := g.seatByToken(rest[0]); ok && rest[0] != "me" && rest[0] != "myself" {
		if rest[1] == "for" {
			if len(rest) < 3 {
				return engine.Action{}, false
			}
			rest = rest[2:]
		}
		if len(rest) > 0 {
			if n, next, ok := numberAt(rest, 0); ok {
				for next < len(rest) && (rest[next] == "cards" || rest[next] == "card") {
					next++
				}
				if next == len(rest) {
					return engine.Action{Kind: engine.ActionMill, TargetSeat: seat,
						Count: n, Confidence: ConfReference}, true
				}
			}
		}
		return engine.Action{}, false
	}
	if rest[0] == "myself" || rest[0] == "me" {
		rest = rest[1:]
		if len(rest) > 0 && rest[0] == "for" {
			rest = rest[1:]
		}
	}
	if len(rest) == 0 {
		return engine.Action{}, false
	}
	if n, next, ok := numberAt(rest, 0); ok {
		for next < len(rest) && (rest[next] == "cards" || rest[next] == "card") {
			next++
		}
		if next == len(rest) {
			return engine.Action{Kind: engine.ActionMill, TargetSeat: target,
				Count: n, Confidence: ConfCertain}, true
		}
	}
	return engine.Action{}, false
}

/* ---------- land and cast ---------- */

// parseLandCast: "play a Forest" · "land: Forest" · "cast Rhystic Study"
// · "I'll play my Study". The land-versus-spell call for "play X" is
// landVerdict's; everything explicit lands where it is said to.
func (g *game) parseLandCast(toks []string) (engine.Action, bool) {
	i := 0
	verb := ""
	switch toks[0] {
	case "play", "playing", "drop", "dropping", "cast", "casting", "recast":
		verb = toks[0]
		i = 1
	case "land":
		if len(toks) == 1 {
			// "land" alone: the drop happened, the name was not said.
			return engine.Action{Kind: engine.ActionPlayLand, Confidence: ConfDerived}, true
		}
		if toks[1] == "drop" {
			if len(toks) == 2 {
				return engine.Action{Kind: engine.ActionPlayLand, Confidence: ConfDerived}, true
			}
			return g.landAction(g.stripArticles(toks[2:]), ConfReference)
		}
		return g.landAction(g.stripArticles(toks[1:]), ConfReference)
	}
	if verb == "" || i >= len(toks) {
		return engine.Action{}, false
	}
	span := g.stripArticles(toks[i:])
	switch verb {
	case "cast", "casting", "recast":
		return g.castAction(span2str(span), nil)
	case "drop", "dropping":
		// A drop is a land drop, whatever the name.
		return g.landAction(span, ConfReference)
	}
	// "play X" / "playing X"
	if len(span) == 1 && span[0] == "land" {
		return engine.Action{Kind: engine.ActionPlayLand, Confidence: ConfCertain}, true
	}
	isLand, conf, types := g.landVerdict(span2str(span))
	if isLand {
		return g.landActionWithTypes(span, conf, types)
	}
	if conf == 0 {
		return engine.Action{}, false // unresolved and not a basic: refuse
	}
	a, ok := g.castAction(span2str(span), nil)
	if !ok {
		return engine.Action{}, false
	}
	a.Confidence = conf // landVerdict already capped the unknown-types cast
	return a, true
}

// landAction builds PLAY_LAND for a spoken name: canonical when the seam
// resolved it, proper-case basic when that is what it is, as spoken
// otherwise, and honest about which of the three.
func (g *game) landAction(span []string, conf float64) (engine.Action, bool) {
	spoken := span2str(span)
	if spoken == "" || spoken == "land" {
		return engine.Action{Kind: engine.ActionPlayLand, Confidence: conf}, true
	}
	card := spoken
	if res := g.resolveName(spoken); res.Card != "" {
		card = res.Card
		if res.Confidence < conf {
			conf = res.Confidence
		}
	} else if c := canonicalBasic(foldName(spoken)); c != "" {
		card = c // a basic needs no index to be itself
	} else {
		conf = ConfUnresolved
	}
	if !likelyName(card) {
		return engine.Action{}, false
	}
	return engine.Action{Kind: engine.ActionPlayLand, Card: card, Confidence: conf}, true
}

func (g *game) landActionWithTypes(span []string, conf float64, types []string) (engine.Action, bool) {
	a, ok := g.landAction(span, conf)
	if !ok {
		return engine.Action{}, false
	}
	if a.Card != "" && len(types) > 0 {
		a.Base = &engine.BaseChars{Name: a.Card, Types: types}
	}
	return a, true
}

func span2str(toks []string) string { return strings.Join(toks, " ") }

/* ---------- damage and life ---------- */

// parseDamageLife: "-3" · "Bob takes 3" · "attack Sarah for six" ·
// "swing 6 at Bob" · "Lightning Bolt Alice for 3" · "Bolt deals 3 to
// Sarah" · "I'm at 20". Sources attach wherever one is spoken — a log
// entry that says why beats several that say what.
func (g *game) parseDamageLife(toks []string) (engine.Action, bool) {
	if a, ok := g.parseSignedLead(toks); ok {
		return a, true
	}
	if a, ok := g.parseLifeVerb(toks); ok {
		return a, true
	}
	if a, ok := g.parseAbsoluteLife(toks); ok {
		return a, true
	}
	if a, ok := g.parseDamageVerb(toks); ok {
		return a, true
	}
	return g.parseCardDamage(toks)
}

// parseSignedLead: "-3", "+2", "-3 to Bob". The sign must be spoken —
// a bare "3" is a count of something, not a life change.
func (g *game) parseSignedLead(toks []string) (engine.Action, bool) {
	t := toks[0]
	if len(t) < 2 || (t[0] != '-' && t[0] != '+') || !isDigits(t[1:]) {
		return engine.Action{}, false
	}
	n := atoi(t[1:])
	if t[0] == '-' {
		n = -n
	}
	if n == 0 {
		return engine.Action{}, false
	}
	if len(toks) == 1 {
		return engine.Action{Kind: engine.ActionChangeLife, TargetSeat: g.seat,
			Delta: n, Confidence: ConfCertain}, true
	}
	if (toks[1] == "to" || toks[1] == "on" || toks[1] == "for") && len(toks) == 3 {
		if seat, ok := g.seatByToken(toks[2]); ok {
			return engine.Action{Kind: engine.ActionChangeLife, TargetSeat: seat,
				Delta: n, Confidence: ConfReference}, true
		}
	}
	return engine.Action{}, false
}

// parseLifeVerb: "<seat>? takes/loses/gains N (life|damage)?" — target
// defaults to the speaker, the way a tracker works.
func (g *game) parseLifeVerb(toks []string) (engine.Action, bool) {
	seat, conf, rest := g.seat, ConfCertain, toks
	if len(toks) > 1 {
		if s, ok := g.seatByToken(toks[0]); ok && isLifeVerb(toks[1]) {
			seat, conf, rest = s, ConfReference, toks[1:]
		}
	}
	if len(rest) < 2 || !isLifeVerb(rest[0]) {
		return engine.Action{}, false
	}
	verb := rest[0]
	n, next, ok := numberAt(rest, 1)
	if !ok {
		return engine.Action{}, false
	}
	for next < len(rest) && (rest[next] == "life" || rest[next] == "damage") {
		next++
	}
	if next != len(rest) {
		return engine.Action{}, false
	}
	switch verb {
	case "take", "takes", "lose", "loses":
		return engine.Action{Kind: engine.ActionChangeLife, TargetSeat: seat,
			Delta: -n, Confidence: conf}, true
	case "gain", "gains":
		return engine.Action{Kind: engine.ActionChangeLife, TargetSeat: seat,
			Delta: n, Confidence: conf}, true
	}
	return engine.Action{}, false
}

func isLifeVerb(w string) bool {
	switch w {
	case "take", "takes", "lose", "loses", "gain", "gains":
		return true
	}
	return false
}

// parseAbsoluteLife: "I'm at 20" · "Bob is at 30" · "set Bob to 25" ·
// "set my life to 30".
func (g *game) parseAbsoluteLife(toks []string) (engine.Action, bool) {
	// "i'm|im|i am at N" — walk to the "at".
	if len(toks) == 3 && (toks[0] == "i'm" || toks[0] == "im" || toks[0] == "i") &&
		toks[1] == "at" {
		if n, next, ok := numberAt(toks, 2); ok && next == 3 {
			return engine.Action{Kind: engine.ActionChangeLife, TargetSeat: g.seat,
				To: &n, Confidence: ConfCertain}, true
		}
		return engine.Action{}, false
	}
	if len(toks) == 4 && toks[0] == "i" && toks[1] == "am" && toks[2] == "at" {
		if n, next, ok := numberAt(toks, 3); ok && next == 4 {
			return engine.Action{Kind: engine.ActionChangeLife, TargetSeat: g.seat,
				To: &n, Confidence: ConfCertain}, true
		}
		return engine.Action{}, false
	}
	// "<seat> (is)? at N"
	if len(toks) >= 3 {
		if seat, ok := g.seatByToken(toks[0]); ok {
			i := 1
			if toks[i] == "is" {
				i++
			}
			if i+1 < len(toks) && toks[i] == "at" {
				if n, next, ok := numberAt(toks, i+1); ok && next == len(toks) {
					return engine.Action{Kind: engine.ActionChangeLife, TargetSeat: seat,
						To: &n, Confidence: ConfReference}, true
				}
			}
		}
	}
	// "set <seat>'s life to N" / "set my life to N"
	if toks[0] == "set" && len(toks) >= 3 {
		seat, conf, i := g.seat, ConfDerived, 1
		if toks[i] == "my" || toks[i] == "myself" {
			i++
		} else if s, ok := g.seatByToken(toks[i]); ok && i+1 < len(toks) {
			seat, conf, i = s, ConfReference, i+1
		} else {
			return engine.Action{}, false
		}
		if i < len(toks) && toks[i] == "'s" {
			i++
		}
		if i < len(toks) && toks[i] == "life" {
			i++
		}
		if i+1 < len(toks) && toks[i] == "to" {
			if n, next, ok := numberAt(toks, i+1); ok && next == len(toks) {
				return engine.Action{Kind: engine.ActionChangeLife, TargetSeat: seat,
					To: &n, Confidence: conf}, true
			}
		}
	}
	return engine.Action{}, false
}

var combatDamageVerbs = map[string]bool{
	"swing": true, "swinging": true, "attack": true, "attacks": true,
	"attacking": true, "hit": true, "hits": true,
}

// parseDamageVerb: "attack Sarah for six" · "swing 6 at Bob" · "hit Bob
// for 4" · "Bolt deals 3 to Alice" · "deals 3 damage to Bob" · "3 damage
// to Bob" · "Atraxa attacks Bob for 7" (the leading name is the source).
func (g *game) parseDamageVerb(toks []string) (engine.Action, bool) {
	// "<span>? deals N (damage)? (to)? <seat>"
	for i, t := range toks {
		if t == "deals" || t == "deal" || t == "dealt" {
			if n, next, ok := numberAt(toks, i+1); ok {
				for next < len(toks) && toks[next] == "damage" {
					next++
				}
				if next < len(toks) && toks[next] == "to" {
					next++
				}
				if next+1 == len(toks) {
					if seat, ok := g.seatByToken(toks[next]); ok {
						a := engine.Action{Kind: engine.ActionDealDamage, Amount: n,
							TargetSeat: seat, Confidence: ConfReference}
						g.attachSource(&a, toks[:i])
						return a, true
					}
				}
			}
			return engine.Action{}, false
		}
	}
	// "N damage to <seat>", verb-less.
	if len(toks) == 4 {
		if n, next, ok := numberAt(toks, 0); ok && next == 1 &&
			toks[1] == "damage" && toks[2] == "to" {
			if seat, ok := g.seatByToken(toks[3]); ok {
				return engine.Action{Kind: engine.ActionDealDamage, Amount: n,
					TargetSeat: seat, Confidence: ConfReference}, true
			}
		}
	}
	// The combat verbs, with an optional leading source name.
	srcSpan := []string(nil)
	if !combatDamageVerbs[toks[0]] {
		found := -1
		for i := 1; i < len(toks); i++ {
			if combatDamageVerbs[toks[i]] {
				found = i
				break
			}
		}
		if found < 0 {
			return engine.Action{}, false
		}
		srcSpan, toks = toks[:found], toks[found:]
	}
	verb := toks[0]
	rest := toks[1:]
	if len(rest) == 0 {
		return engine.Action{}, false
	}
	// "swing 6 at Bob"
	if n, next, ok := numberAt(rest, 0); ok && next+2 == len(rest) &&
		(rest[next] == "at" || rest[next] == "to") {
		if seat, ok := g.seatByToken(rest[next+1]); ok {
			a := engine.Action{Kind: engine.ActionDealDamage, Amount: n, TargetSeat: seat,
				CombatDmg: true, Confidence: ConfReference}
			g.attachSource(&a, srcSpan)
			return a, true
		}
	}
	// "attack Bob for 6"
	if seat, ok := g.seatByToken(rest[0]); ok && len(rest) > 2 && rest[1] == "for" {
		if n, next, ok := numberAt(rest, 2); ok && next == len(rest) {
			combat := verb != "hit" || g.st.Step == "combat_damage"
			conf := ConfReference
			if verb == "hit" && !combat {
				conf = ConfDerived
			}
			a := engine.Action{Kind: engine.ActionDealDamage, Amount: n, TargetSeat: seat,
				CombatDmg: combat, Confidence: conf}
			g.attachSource(&a, srcSpan)
			return a, true
		}
	}
	// "swing at Bob for 6"
	if (rest[0] == "at" || rest[0] == "to") && len(rest) > 3 {
		if seat, ok := g.seatByToken(rest[1]); ok && rest[2] == "for" {
			if n, next, ok := numberAt(rest, 3); ok && next == len(rest) {
				a := engine.Action{Kind: engine.ActionDealDamage, Amount: n, TargetSeat: seat,
					CombatDmg: true, Confidence: ConfReference}
				g.attachSource(&a, srcSpan)
				return a, true
			}
		}
	}
	return engine.Action{}, false
}

// parseCardDamage: "Lightning Bolt Alice for 3" — card, victim, amount,
// no verb. The amountless cousin ("Lightning Bolt Alice") is a cast at a
// target, not a damage entry: the engine never reads oracle text to know
// what a Bolt deals (ADR 11), and a number nobody spoke is a guess.
func (g *game) parseCardDamage(toks []string) (engine.Action, bool) {
	if len(toks) < 4 || toks[len(toks)-2] != "for" {
		return engine.Action{}, false
	}
	n, next, ok := numberAt(toks, len(toks)-1)
	if !ok || next != len(toks) || n <= 0 {
		return engine.Action{}, false
	}
	seat, ok := g.seatByToken(toks[len(toks)-3])
	if !ok {
		return engine.Action{}, false
	}
	cardSpan := g.stripArticles(toks[:len(toks)-3])
	if len(cardSpan) == 0 {
		return engine.Action{}, false
	}
	a := engine.Action{Kind: engine.ActionDealDamage, Amount: n, TargetSeat: seat,
		Confidence: ConfReference}
	g.attachSource(&a, cardSpan)
	return a, true
}

// attachSource stamps a spoken source onto a damage action: a
// battlefield object by that name first (the thing that dealt it), then
// the resolved card name, then the span as spoken.
func (g *game) attachSource(a *engine.Action, span []string) {
	if len(span) == 0 {
		return
	}
	spoken := span2str(span)
	if ref, ok := g.objectRef(span, nil); ok {
		a.SourceObj = ref.obj.ID
		a.Confidence = ref.confidence()
		return
	}
	if res := g.resolveName(spoken); res.Card != "" {
		a.SourceCard = res.Card
		if res.Confidence < a.Confidence {
			a.Confidence = res.Confidence
		}
		return
	}
	a.SourceCard = spoken
}

/* ---------- combat declarations ---------- */

// parseCombat: the declaration shapes the engine validates step and
// actor for — "attack Bob with everything" · "attack Bob with Atraxa" ·
// "Atraxa attacks Bob" · "Krenko blocks the Bear". The "for N" damage
// forms were already claimed by parseDamageVerb before this runs.
func (g *game) parseCombat(toks []string) (engine.Action, bool) {
	if a, ok := g.parseAttackDecl(toks); ok {
		return a, true
	}
	return g.parseBlockDecl(toks)
}

func (g *game) parseAttackDecl(toks []string) (engine.Action, bool) {
	// Declarations happen in the declare_attackers step of the active
	// player's turn; anywhere else the shape is not a declaration, and
	// the state is what says so.
	if g.st.Phase != "combat" || g.st.Step != "declare_attackers" || g.st.TurnSeat != g.seat {
		return engine.Action{}, false
	}
	srcSpan := []string(nil)
	switch {
	case toks[0] == "attack" || toks[0] == "attacks":
	case len(toks) > 1 && (toks[1] == "attacks" || toks[1] == "attack"):
		// "Atraxa attacks Bob" — the leading name is the attacker.
		srcSpan, toks = toks[:1], toks[1:]
	default:
		return engine.Action{}, false
	}
	if len(toks) < 2 {
		return engine.Action{}, false
	}
	seat, ok := g.seatByToken(toks[1])
	if !ok {
		return engine.Action{}, false
	}
	// "attack Bob with everything" / "with all"
	if len(toks) >= 4 && toks[2] == "with" &&
		(toks[3] == "everything" || toks[3] == "all") {
		var attackers []engine.AttackAssignment
		for _, o := range g.st.Battlefield(g.seat) {
			if o.Phased || o.Tapped || !g.st.IsCreature(o.ID) {
				continue
			}
			attackers = append(attackers, engine.AttackAssignment{Object: o.ID, TargetSeat: seat})
		}
		if len(attackers) == 0 {
			return engine.Action{}, false
		}
		return engine.Action{Kind: engine.ActionDeclareAttackers, Attackers: attackers,
			Confidence: ConfDerived}, true
	}
	// "attack Bob with Atraxa" / "Atraxa attacks Bob"
	if len(toks) >= 4 && toks[2] == "with" {
		srcSpan = toks[3:]
	}
	if len(srcSpan) == 0 {
		return engine.Action{}, false
	}
	ref, ok := g.objectRef(g.stripArticles(srcSpan), nil)
	if !ok || !g.isBattlefieldCreature(ref.obj) || ref.obj.Controller != g.seat {
		return engine.Action{}, false
	}
	return engine.Action{Kind: engine.ActionDeclareAttackers,
		Attackers:  []engine.AttackAssignment{{Object: ref.obj.ID, TargetSeat: seat}},
		Confidence: ref.confidence()}, true
}

// parseBlockDecl: "<blocker> blocks <attacker>" — both ends named, the
// attacker among the declared attackers.
func (g *game) parseBlockDecl(toks []string) (engine.Action, bool) {
	for i := 1; i+1 < len(toks); i++ {
		if toks[i] != "blocks" && toks[i] != "block" && toks[i] != "blocking" {
			continue
		}
		ref, ok := g.objectRef(g.stripArticles(toks[:i]), nil)
		if !ok || !g.isBattlefieldCreature(ref.obj) {
			return engine.Action{}, false
		}
		atkName := span2str(g.stripArticles(toks[i+1:]))
		for _, at := range g.st.Attackers {
			o, okO := g.st.Objects[at.Object]
			if !okO {
				continue
			}
			f := foldName(o.Identity.Card)
			if o.Identity.Token != nil && f == "" {
				f = foldName(o.Identity.Token.Name)
			}
			for _, c := range g.foldCandidates(atkName) {
				if f != "" && f == c {
					return engine.Action{Kind: engine.ActionDeclareBlockers,
						Blockers: []engine.BlockAssignment{{Blocker: ref.obj.ID,
							Attackers: []int64{at.Object}}},
						Confidence: ref.confidence()}, true
				}
			}
		}
		return engine.Action{}, false
	}
	return engine.Action{}, false
}

/* ---------- bare names, last ---------- */

// parseBareName: "Rhystic" · "Forest" · "Lightning Bolt Alice". A bare
// name is a cast (the issue's own mapping), except the basics — "Forest"
// said with no verb is the land drop. A trailing seat token makes the
// cast targeted. Nothing resolves: no parse, because a bare unrecognized
// word is more likely table chatter than an action.
func (g *game) parseBareName(toks []string) (engine.Action, bool) {
	span := span2str(g.stripArticles(toks))
	if span == "" || !likelyName(span) {
		return engine.Action{}, false
	}
	if isBasicLandSpan(foldName(span)) {
		return g.landAction([]string{span}, ConfReference)
	}
	if res := g.resolveName(span); res.Card != "" {
		a := engine.Action{Kind: engine.ActionCast, Card: res.Card, Confidence: res.Confidence}
		if len(res.Types) > 0 {
			a.Base = &engine.BaseChars{Name: res.Card, Types: res.Types}
		}
		return a, true
	}
	// "<card> <seat>": a spell named at someone — "Lightning Bolt Alice".
	// The card must resolve; a name aimed at a seat is worth confirming.
	if len(toks) >= 2 {
		if seat, ok := g.seatByToken(toks[len(toks)-1]); ok {
			cardSpan := span2str(g.stripArticles(toks[:len(toks)-1]))
			if res := g.resolveName(cardSpan); res.Card != "" {
				a := engine.Action{Kind: engine.ActionCast, Card: res.Card,
					Targets: []engine.Target{{Seat: seat}}, Confidence: res.Confidence}
				if len(res.Types) > 0 {
					a.Base = &engine.BaseChars{Name: res.Card, Types: res.Types}
				}
				return a, true
			}
		}
	}
	return engine.Action{}, false
}

/* ---------- shared span helpers ---------- */

var articles = map[string]bool{
	"a": true, "an": true, "the": true, "my": true, "his": true,
	"her": true, "their": true, "some": true, "another": true,
}

// stripArticles drops the leading determiners and possessives from a
// name span — "the Sol Ring" is "Sol Ring", "Bob's Sol Ring" is too.
func (g *game) stripArticles(toks []string) []string {
	for len(toks) > 1 && articles[toks[0]] {
		toks = toks[1:]
	}
	if len(toks) > 1 && strings.HasSuffix(toks[0], "'s") {
		if _, ok := g.seatByToken(strings.TrimSuffix(toks[0], "'s")); ok {
			toks = toks[1:]
		}
	}
	return toks
}

// objectRef wraps objectBySpan with the confidence semantics: a clean
// unique reference is ConfReference and a tie-break among several is
// ConfMulti.
func (g *game) objectRef(span []string, prefer func(*engine.Object) bool) (objRef, bool) {
	if len(span) == 0 {
		return objRef{}, false
	}
	ref, ok := g.objectBySpan(span2str(span), prefer)
	if !ok {
		return objRef{}, false
	}
	return ref, true
}

// confidence maps a battlefield reference to its band.
func (r objRef) confidence() float64 {
	if r.multi {
		return ConfMulti
	}
	return ConfReference
}
