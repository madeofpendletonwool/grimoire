package intent

// The gate (MAD-331): the constraint half of "constrained to emit a
// typed Action". The prompt requests; this file disposes. Everything
// the model says passes through deterministic lookups against the same
// state the grammar resolves against — seats by name, battlefield
// objects by name, cards against the known universe or the utterance's
// own words — and the model's confidence is capped below the auto bar
// no matter what it claims, because a parser that only sees fallback
// input has not earned immediate application of anything.
//
// A reply that fails the gate is a no-parse, reported cleanly. The gate
// never repairs a guess into an action: wrong is worse than none.

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/carddb"
	"github.com/madeofpendletonwool/grimoire/internal/table/engine"
	"github.com/madeofpendletonwool/grimoire/internal/table/grammar"
	"github.com/madeofpendletonwool/grimoire/internal/table/universe"
)

// ErrNone is the model's honest refusal ({"kind":"NONE"}) surfaced as a
// clean no-parse: not an error condition, just an answer.
var ErrNone = errors.New("the model declined to parse the utterance")

// ErrGate refuses a reply that could not become a valid action.
var ErrGate = errors.New("the model reply failed the gate")

// Ident is one name the model identified against the known universe:
// what was said (the model's own spelling) and the canonical card it
// means, at the confidence the identification earned. The pipeline
// records it in the per-game cache so the same name is never re-inferred
// and never re-billed. A reply that identified nothing carries nil.
type Ident struct {
	Spoken     string
	Card       string
	Confidence float64
}

// rawAction is the model's mini-schema: deliberately narrower than
// engine.Action — the fallback answers the shapes the grammar missed,
// and every field is validated before it becomes engine state.
type rawAction struct {
	Kind       string    `json:"kind"`
	Confidence float64   `json:"confidence"`
	Card       string    `json:"card"`
	Target     string    `json:"target"`
	Delta      *int      `json:"delta"`
	Amount     *int      `json:"amount"`
	Count      *int      `json:"count"`
	Counter    string    `json:"counter"`
	On         string    `json:"on"`
	Object     string    `json:"object"`
	Cause      string    `json:"cause"`
	Flag       string    `json:"flag"`
	Token      *rawToken `json:"token"`
}

type rawToken struct {
	Name      string `json:"name"`
	Power     *int   `json:"power"`
	Toughness *int   `json:"toughness"`
}

// numberCeiling is the sanity bound on any count, delta or amount the
// model emits. Real Magic numbers sit far below it; a number past it is
// a model failure, not a game event.
const numberCeiling = 1000

// Gate validates one model reply into a candidate action. utterance is
// the original speech; u is the game's known-card universe (may be nil
// for a deckless table). The returned action carries Source "llm" and a
// confidence already capped for the ladder; the Ident (nil when the
// reply identified no name) is the cache's business, not the action's.
func Gate(st *engine.State, seat int, utterance string, u *universe.Universe, reply string) (engine.Action, *Ident, error) {
	raw, err := decodeReply(reply)
	if err != nil {
		return engine.Action{}, nil, err
	}
	if strings.EqualFold(strings.TrimSpace(raw.Kind), "NONE") {
		return engine.Action{}, nil, ErrNone
	}
	switch engine.ActionKind(raw.Kind) {
	case "":
		return engine.Action{}, nil, fmt.Errorf("%w: no kind in reply", ErrGate)
	case engine.ActionPassPriority, engine.ActionAdvance:
		return stamped(engine.Action{Kind: engine.ActionKind(raw.Kind), Seat: seat}, raw), nil, nil
	case engine.ActionConcede:
		seatOut, err := actorSeat(st, seat, raw)
		if err != nil {
			return engine.Action{}, nil, err
		}
		return stamped(engine.Action{Kind: engine.ActionConcede, Seat: seatOut}, raw), nil, nil
	case engine.ActionCast:
		return gateCast(st, seat, utterance, u, raw)
	case engine.ActionPlayLand:
		a := stamped(engine.Action{Kind: engine.ActionPlayLand, Seat: seat}, raw)
		if raw.Card != "" {
			card, conf, ident, err := gateCard(utterance, u, raw.Card)
			if err != nil {
				return engine.Action{}, nil, err
			}
			a.Card, a.Confidence = card, minConf(conf, a.Confidence)
			return a, ident, nil
		}
		return a, nil, nil
	case engine.ActionDraw, engine.ActionMill:
		a := stamped(engine.Action{Kind: engine.ActionKind(raw.Kind), Seat: seat}, raw)
		a.Count = 1
		if raw.Count != nil {
			if *raw.Count < 1 || *raw.Count > numberCeiling {
				return engine.Action{}, nil, fmt.Errorf("%w: count %d", ErrGate, *raw.Count)
			}
			a.Count = *raw.Count
		}
		if raw.Kind == string(engine.ActionMill) && raw.Target != "" {
			target, err := targetSeat(st, seat, raw.Target)
			if err != nil {
				return engine.Action{}, nil, err
			}
			a.TargetSeat = target
		}
		return a, nil, nil
	case engine.ActionChangeLife:
		return gateChangeLife(st, seat, utterance, u, raw)
	case engine.ActionDealDamage:
		return gateDamage(st, seat, utterance, u, raw)
	case engine.ActionTap, engine.ActionUntap:
		a := stamped(engine.Action{Kind: engine.ActionKind(raw.Kind), Seat: seat}, raw)
		if raw.Object == "" {
			return engine.Action{}, nil, fmt.Errorf("%w: %s needs an object", ErrGate, raw.Kind)
		}
		obj, multi, err := gateObject(st, seat, raw.Object)
		if err != nil {
			return engine.Action{}, nil, err
		}
		a.Object = obj.ID
		if multi {
			a.Confidence = minConf(grammar.ConfMulti, a.Confidence)
		}
		return a, nil, nil
	case engine.ActionCreateToken:
		return gateToken(st, seat, raw)
	case engine.ActionMoveZone:
		return gateMove(st, seat, raw)
	case engine.ActionAdjustCounters:
		return gateCounters(st, seat, raw)
	case engine.ActionSetFlag:
		return gateFlag(st, seat, raw)
	}
	return engine.Action{}, nil, fmt.Errorf("%w: kind %q is not in the fallback vocabulary", ErrGate, raw.Kind)
}

// decodeReply pulls the fenced json block out of the reply and decodes
// it. Tolerates missing fences; refuses anything that is not one JSON
// object.
func decodeReply(reply string) (*rawAction, error) {
	text := strings.TrimSpace(reply)
	if i := strings.Index(text, "```"); i >= 0 {
		rest := text[i+3:]
		if j := strings.Index(rest, "```"); j >= 0 {
			text = rest[:j]
		} else {
			text = rest
		}
		text = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), "json"))
	}
	start, end := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("%w: no json object in reply", ErrGate)
	}
	var raw rawAction
	if err := json.Unmarshal([]byte(text[start:end+1]), &raw); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrGate, err)
	}
	return &raw, nil
}

// stamped copies the validated basics onto the action: seat, source,
// and the confidence the ladder will see — the model's claim capped at
// universe.ConfLLM, under the auto bar by construction.
func stamped(a engine.Action, raw *rawAction) engine.Action {
	a.Source = "llm"
	a.Confidence = raw.Confidence
	if a.Confidence > universe.ConfLLM {
		a.Confidence = universe.ConfLLM
	}
	if a.Confidence < 0 {
		a.Confidence = 0
	}
	return a
}

// minConf keeps the weaker of two bounds.
func minConf(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

// actorSeat resolves the acting seat for kinds where the model may name
// someone other than the speaker ("Bob concedes", said by Collin).
func actorSeat(st *engine.State, seat int, raw *rawAction) (int, error) {
	if raw.Target == "" {
		return seat, nil
	}
	return targetSeat(st, seat, raw.Target)
}

// targetSeat resolves a spoken seat name through the grammar's own
// deterministic lookup — the model never addresses a seat the state
// cannot confirm.
func targetSeat(st *engine.State, seat int, spoken string) (int, error) {
	if spoken == "" {
		return seat, nil
	}
	if s, ok := grammar.SeatBySpoken(st, seat, spoken); ok {
		return s, nil
	}
	return 0, fmt.Errorf("%w: no seat answers to %q", ErrGate, spoken)
}

// gateCard enforces the card rule: the name is canonical when it is in
// the known universe (or a verbatim known spelling of it), a proper-case
// basic, or copied verbatim from the utterance at unresolved
// confidence. Never a free invention. The Ident returned is the
// identification the cache records — nil when the reply only carried
// the utterance's own words, which identified nothing.
func gateCard(utterance string, u *universe.Universe, card string) (string, float64, *Ident, error) {
	card = strings.TrimSpace(card)
	if card == "" {
		return "", 0, nil, fmt.Errorf("%w: empty card name", ErrGate)
	}
	if u != nil {
		for _, tier := range knownTiers(u) {
			cards := u.Cards(tier)
			for name := range cards {
				if carddb.NormalizeName(name) == carddb.NormalizeName(card) {
					return name, universe.ConfLLM, &Ident{Spoken: card, Card: name, Confidence: universe.ConfLLM}, nil
				}
			}
		}
	}
	// A basic is itself by rule, no index needed.
	if basic := basicCard(card); basic != "" {
		return basic, universe.ConfLLM, nil, nil
	}
	// Verbatim from the utterance: the spoken span, carried as spoken,
	// unresolved — the ladder will ask, never write it silently.
	if spokenSpan(utterance, card) {
		return strings.TrimSpace(card), grammar.ConfUnresolved, nil, nil
	}
	return "", 0, nil, fmt.Errorf("%w: %q is not a known card nor the utterance's own words", ErrGate, card)
}

// knownTiers is the universe walk order for exact-name checks: the
// speaker first, then the table in seating order.
func knownTiers(u *universe.Universe) []int {
	return u.Seats()
}

// basicCard canonicalizes the six basics (snow-covered included) by
// folded name.
func basicCard(name string) string {
	folded := carddb.NormalizeName(name)
	switch folded {
	case "forest":
		return "Forest"
	case "island":
		return "Island"
	case "mountain":
		return "Mountain"
	case "plains":
		return "Plains"
	case "swamp":
		return "Swamp"
	case "wastes":
		return "Wastes"
	case "snow covered forest":
		return "Snow-Covered Forest"
	case "snow covered island":
		return "Snow-Covered Island"
	case "snow covered mountain":
		return "Snow-Covered Mountain"
	case "snow covered plains":
		return "Snow-Covered Plains"
	case "snow covered swamp":
		return "Snow-Covered Swamp"
	case "snow covered wastes":
		return "Snow-Covered Wastes"
	}
	return ""
}

// spokenSpan reports whether card's words all appear in the utterance —
// the "copied verbatim" rule, word-wise rather than substring-wise so
// punctuation and case never matter.
func spokenSpan(utterance, card string) bool {
	words := strings.Fields(strings.ToLower(card))
	if len(words) == 0 {
		return false
	}
	utter := " " + strings.ToLower(utterance) + " "
	for _, w := range words {
		if !strings.Contains(utter, " "+w+" ") {
			return false
		}
	}
	return true
}

// gateObject resolves a battlefield reference through the grammar's own
// object lookup. multi means several objects answered to the name: the
// deterministic tie-break chose one and the ladder hears about it.
func gateObject(st *engine.State, seat int, spoken string) (*engine.Object, bool, error) {
	obj, multi, ok := grammar.ObjectBySpoken(st, seat, spoken)
	if !ok {
		return nil, false, fmt.Errorf("%w: no battlefield object answers to %q", ErrGate, spoken)
	}
	return obj, multi, nil
}

func gateCast(st *engine.State, seat int, utterance string, u *universe.Universe, raw *rawAction) (engine.Action, *Ident, error) {
	if strings.TrimSpace(raw.Card) == "" {
		return engine.Action{}, nil, fmt.Errorf("%w: a cast needs a card", ErrGate)
	}
	card, conf, ident, err := gateCard(utterance, u, raw.Card)
	if err != nil {
		return engine.Action{}, nil, err
	}
	a := stamped(engine.Action{Kind: engine.ActionCast, Seat: seat, Card: card}, raw)
	a.Confidence = minConf(conf, a.Confidence)
	// A commander cast from the command zone: same fold the grammar
	// applies, case-insensitive on the seat's recorded commander.
	if p := st.Seats[seat]; p != nil && p.Commander != "" && strings.EqualFold(p.Commander, card) {
		for _, o := range st.ZoneObjects(seat, engine.ZoneCommand) {
			if strings.EqualFold(o.Identity.Card, card) {
				a.FromZone = engine.ZoneCommand
				break
			}
		}
	}
	return a, ident, nil
}

func gateChangeLife(st *engine.State, seat int, utterance string, u *universe.Universe, raw *rawAction) (engine.Action, *Ident, error) {
	if raw.Delta == nil || *raw.Delta == 0 {
		return engine.Action{}, nil, fmt.Errorf("%w: a life change needs a nonzero delta", ErrGate)
	}
	if abs(*raw.Delta) > numberCeiling {
		return engine.Action{}, nil, fmt.Errorf("%w: delta %d", ErrGate, *raw.Delta)
	}
	target, err := targetSeat(st, seat, raw.Target)
	if err != nil {
		return engine.Action{}, nil, err
	}
	a := stamped(engine.Action{Kind: engine.ActionChangeLife, Seat: seat,
		TargetSeat: target, Delta: *raw.Delta}, raw)
	if raw.Card != "" {
		// The source is bookkeeping, not identity: keep the canonical
		// spelling when it is known, the spoken words when it is not,
		// and refuse nothing over it.
		if card, _, _, err := gateCard(utterance, u, raw.Card); err == nil {
			a.SourceCard = card
		} else {
			a.SourceCard = strings.TrimSpace(raw.Card)
		}
	}
	return a, nil, nil
}

func gateDamage(st *engine.State, seat int, utterance string, u *universe.Universe, raw *rawAction) (engine.Action, *Ident, error) {
	if raw.Amount == nil || *raw.Amount < 1 || *raw.Amount > numberCeiling {
		return engine.Action{}, nil, fmt.Errorf("%w: damage needs an amount", ErrGate)
	}
	target, err := targetSeat(st, seat, raw.Target)
	if err != nil {
		return engine.Action{}, nil, err
	}
	a := stamped(engine.Action{Kind: engine.ActionDealDamage, Seat: seat,
		TargetSeat: target, Amount: *raw.Amount}, raw)
	if strings.TrimSpace(raw.Card) != "" {
		// A source that is on the battlefield is the object that dealt
		// it; otherwise a named card; never a repair.
		if obj, _, err := gateObject(st, seat, raw.Card); err == nil {
			a.SourceObj = obj.ID
		} else if card, _, _, err := gateCard(utterance, u, raw.Card); err == nil {
			a.SourceCard = card
		} else {
			a.SourceCard = strings.TrimSpace(raw.Card)
		}
	}
	return a, nil, nil
}

func gateToken(st *engine.State, seat int, raw *rawAction) (engine.Action, *Ident, error) {
	if raw.Token == nil || strings.TrimSpace(raw.Token.Name) == "" {
		return engine.Action{}, nil, fmt.Errorf("%w: a token needs a name", ErrGate)
	}
	a := stamped(engine.Action{Kind: engine.ActionCreateToken, Seat: seat}, raw)
	spec := &engine.TokenSpec{Name: strings.TrimSpace(raw.Token.Name)}
	if raw.Token.Power != nil && raw.Token.Toughness != nil {
		spec.Power, spec.Toughness = raw.Token.Power, raw.Token.Toughness
		spec.Types = []string{"Creature"}
	}
	a.Token = spec
	a.Count = 1
	if raw.Count != nil {
		if *raw.Count < 1 || *raw.Count > numberCeiling {
			return engine.Action{}, nil, fmt.Errorf("%w: count %d", ErrGate, *raw.Count)
		}
		a.Count = *raw.Count
	}
	return a, nil, nil
}

var moveCauses = map[string]bool{
	"sacrifice": true, "destroy": true, "bounce": true, "exile": true,
}

func gateMove(st *engine.State, seat int, raw *rawAction) (engine.Action, *Ident, error) {
	cause := strings.TrimSpace(raw.Cause)
	if !moveCauses[cause] {
		return engine.Action{}, nil, fmt.Errorf("%w: a zone move needs a cause of sacrifice|destroy|bounce|exile", ErrGate)
	}
	if raw.Object == "" {
		return engine.Action{}, nil, fmt.Errorf("%w: a zone move needs an object", ErrGate)
	}
	obj, multi, err := gateObject(st, seat, raw.Object)
	if err != nil {
		return engine.Action{}, nil, err
	}
	zone := engine.ZoneGraveyard
	switch cause {
	case "exile":
		zone = engine.ZoneExile
	case "bounce":
		zone = engine.ZoneHand
	}
	a := stamped(engine.Action{Kind: engine.ActionMoveZone, Seat: seat, Object: obj.ID,
		ToZone: zone, Cause: cause}, raw)
	if multi {
		a.Confidence = minConf(grammar.ConfMulti, a.Confidence)
	}
	return a, nil, nil
}

func gateCounters(st *engine.State, seat int, raw *rawAction) (engine.Action, *Ident, error) {
	name := strings.TrimSpace(raw.Counter)
	if name == "" {
		return engine.Action{}, nil, fmt.Errorf("%w: a counter needs a name", ErrGate)
	}
	if raw.Delta == nil || *raw.Delta == 0 || abs(*raw.Delta) > numberCeiling {
		return engine.Action{}, nil, fmt.Errorf("%w: a counter needs a nonzero delta", ErrGate)
	}
	a := stamped(engine.Action{Kind: engine.ActionAdjustCounters, Seat: seat,
		CounterName: name, Delta: *raw.Delta}, raw)
	switch {
	case raw.On != "":
		obj, multi, err := gateObject(st, seat, raw.On)
		if err != nil {
			return engine.Action{}, nil, err
		}
		a.OnObject = obj.ID
		if multi {
			a.Confidence = minConf(grammar.ConfMulti, a.Confidence)
		}
	case raw.Target != "":
		target, err := targetSeat(st, seat, raw.Target)
		if err != nil {
			return engine.Action{}, nil, err
		}
		a.TargetSeat = target
	default:
		a.TargetSeat = seat
	}
	return a, nil, nil
}

var flags = map[string]bool{"monarch": true, "initiative": true, "city's blessing": true}

func gateFlag(st *engine.State, seat int, raw *rawAction) (engine.Action, *Ident, error) {
	flag := strings.TrimSpace(raw.Flag)
	if !flags[flag] {
		return engine.Action{}, nil, fmt.Errorf("%w: unknown flag %q", ErrGate, raw.Flag)
	}
	target, err := targetSeat(st, seat, raw.Target)
	if err != nil {
		return engine.Action{}, nil, err
	}
	return stamped(engine.Action{Kind: engine.ActionSetFlag, Seat: seat,
		TargetSeat: target, Flag: flag, Value: "true"}, raw), nil, nil
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
