package intent

// The gate, unit-tested over a hand-built live state: every constraint
// the prompt requests is enforced here — kinds whitelisted, seats and
// objects resolved through the deterministic lookups, cards from the
// universe or the utterance's own words, confidence capped under the
// auto bar. A reply that fails is a no-parse, never a repair.

import (
	"errors"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/table/engine"
	"github.com/madeofpendletonwool/grimoire/internal/table/universe"
)

// gateState builds a minimal active state: Collin (seat 1) with a Sol
// Ring untapped on the battlefield, Bob (seat 2), Collin's turn,
// Collin holding priority.
func gateState() *engine.State {
	st := engine.NewState()
	st.Status = engine.StatusActive
	st.Turn, st.TurnSeat = 3, 1
	st.Phase, st.Step = "precombat_main", "main"
	st.PrioritySeat = 1
	st.Order = []int{1, 2}
	st.Seats[1] = &engine.Player{Seat: 1, Name: "Collin", Life: 37, Alive: true, Commander: "Atraxa, Praetors' Voice"}
	st.Seats[2] = &engine.Player{Seat: 2, Name: "Bob", Life: 28, Alive: true}
	st.Seats[1].Deck = map[string]int{"Rhystic Study": 1, "Sol Ring": 1, "Cultivate": 1}
	untapped := false
	st.NextObject = 2
	st.Objects[1] = &engine.Object{ID: 1, Zone: engine.ZoneBattlefield, Controller: 1, Owner: 1,
		Tapped: untapped, Identity: engine.Identity{Card: "Sol Ring"}}
	return st
}

func gateUniverse(st *engine.State) *universe.Universe {
	return universe.FromState(st)
}

func TestGateFencedAndBare(t *testing.T) {
	st := gateState()
	u := gateUniverse(st)
	for _, reply := range []string{
		`{"kind":"PASS_PRIORITY"}`,
		fenced(`{"kind":"PASS_PRIORITY"}`),
		"prose\n```json\n{\"kind\":\"ADVANCE\"}\n```\nmore prose",
	} {
		a, _, err := Gate(st, 1, "pass", u, reply)
		if err != nil {
			t.Fatalf("%q: %v", reply, err)
		}
		if a.Source != "llm" {
			t.Errorf("%q: source = %q, want llm", reply, a.Source)
		}
	}
}

func TestGateRefusalKinds(t *testing.T) {
	st := gateState()
	u := gateUniverse(st)
	if _, _, err := Gate(st, 1, "what is this", u, `{"kind":"NONE"}`); !errors.Is(err, ErrNone) {
		t.Errorf("NONE: err = %v, want ErrNone", err)
	}
	for name, reply := range map[string]string{
		"no kind":      `{}`,
		"unknown":      `{"kind":"DECLARE_ATTACKERS"}`,
		"not json":     `I think Collin should pass`,
		"empty":        ``,
		"invented":     `{"kind":"CAST","card":"Black Lotus"}`,
		"cast no card": `{"kind":"CAST"}`,
		"bad seat":     `{"kind":"CHANGE_LIFE","target":"Sarah","delta":-3}`,
		"no object":    `{"kind":"TAP","object":"Krenko"}`,
		"bad cause":    `{"kind":"MOVE_ZONE","object":"Sol Ring","cause":"yeet"}`,
		"zero delta":   `{"kind":"CHANGE_LIFE","delta":0}`,
		"no amount":    `{"kind":"DEAL_DAMAGE","target":"Bob"}`,
		"bad flag":     `{"kind":"SET_FLAG","flag":"the crown"}`,
	} {
		if _, _, err := Gate(st, 1, "utterance", u, reply); !errors.Is(err, ErrGate) {
			t.Errorf("%s: err = %v, want ErrGate", name, err)
		}
	}
}

func TestGateCardRule(t *testing.T) {
	st := gateState()
	u := gateUniverse(st)
	// Known universe: canonical spelling plus an identification.
	a, ident, err := Gate(st, 1, "gonna do the cultivate thing", u, `{"kind":"CAST","card":"cultivate","confidence":0.9}`)
	if err != nil {
		t.Fatalf("cast cultivate: %v", err)
	}
	if a.Card != "Cultivate" {
		t.Errorf("card = %q, want canonical Cultivate", a.Card)
	}
	if ident == nil || ident.Card != "Cultivate" || ident.Spoken != "cultivate" {
		t.Errorf("ident = %+v, want the canonical identification", ident)
	}
	if a.Confidence > universe.ConfLLM {
		t.Errorf("confidence %.2f above the model ceiling", a.Confidence)
	}
	// The utterance's own words, unresolvable: carried as spoken at the
	// unresolved band, for the ladder to ask about.
	a, ident, err = Gate(st, 1, "cast the weird jellyfish card", u, `{"kind":"CAST","card":"the weird jellyfish card","confidence":0.9}`)
	if err != nil {
		t.Fatalf("cast spoken span: %v", err)
	}
	if a.Card != "the weird jellyfish card" || a.Confidence != unresolvedBand {
		t.Errorf("span cast = %+v, want the spoken span at the unresolved band", a)
	}
	if ident != nil {
		t.Errorf("span cast identified %+v, want none", ident)
	}
	// A basic is itself by rule.
	a, _, err = Gate(st, 1, "drop a forest", u, `{"kind":"PLAY_LAND","card":"forest"}`)
	if err != nil || a.Card != "Forest" {
		t.Errorf("basic land = %+v err %v, want Forest", a, err)
	}
	// A card in the universe is admissible even when the utterance only
	// mumbled at it — canonicalizing mumbles onto real cards is the
	// fallback's job, and the ladder bounds the trust (confirm at most).
	// A card outside the universe and outside the utterance's own words
	// is an invention, and inventions are refused ("invented" above).
	if _, _, err := Gate(st, 1, "do the thing already", u, `{"kind":"CAST","card":"Cultivate"}`); err != nil {
		t.Errorf("universe card: err = %v, want the identification accepted", err)
	}
}

// unresolvedBand mirrors grammar.ConfUnresolved without importing the
// constant twice in assertions.
const unresolvedBand = 0.30

func TestGateSeatAndObjectLookups(t *testing.T) {
	st := gateState()
	u := gateUniverse(st)
	a, _, err := Gate(st, 1, "bob is going to four", u, `{"kind":"CHANGE_LIFE","target":"Bob","delta":-4}`)
	if err != nil {
		t.Fatalf("change life: %v", err)
	}
	if a.TargetSeat != 2 || a.Delta != -4 {
		t.Errorf("change life = %+v, want Bob −4", a)
	}
	a, _, err = Gate(st, 1, "tap the sol ring", u, `{"kind":"TAP","object":"Sol Ring","confidence":0.5}`)
	if err != nil {
		t.Fatalf("tap: %v", err)
	}
	if a.Object != 1 || a.Confidence > 0.5 {
		t.Errorf("tap = %+v, want object 1 with the model's own weaker claim kept", a)
	}
}

func TestGateCommanderRouting(t *testing.T) {
	st := gateState()
	u := gateUniverse(st)
	st.NextObject = 3
	st.Objects[2] = &engine.Object{ID: 2, Zone: engine.ZoneCommand, Controller: 1, Owner: 1,
		Identity: engine.Identity{Card: "Atraxa, Praetors' Voice"}}
	a, _, err := Gate(st, 1, "atriaxa time", u, `{"kind":"CAST","card":"Atraxa, Praetors' Voice"}`)
	if err != nil {
		t.Fatalf("cast commander: %v", err)
	}
	if a.FromZone != engine.ZoneCommand {
		t.Errorf("from zone = %q, want command", a.FromZone)
	}
}

func TestGatePromptSpellsTargetsAsStateDoes(t *testing.T) {
	st := gateState()
	u := gateUniverse(st)
	// The state lists "Collin"; the model must use exactly that.
	if _, _, err := Gate(st, 1, "x", u, `{"kind":"CHANGE_LIFE","target":"collin","delta":-3}`); err != nil {
		t.Fatalf("seat lookup should be case-insensitive on the exact name: %v", err)
	}
	if _, _, err := Gate(st, 1, "x", u, `{"kind":"CHANGE_LIFE","target":"Colin","delta":-3}`); !errors.Is(err, ErrGate) {
		t.Errorf("misspelled seat: err = %v, want ErrGate", err)
	}
	if !strings.Contains(SystemPrompt(), "NONE") {
		t.Error("the system prompt must document the NONE refusal")
	}
}
