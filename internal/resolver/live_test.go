package resolver

// FromGame tests (MAD-333): the live folded state becomes the resolver's
// Input — board, sequence (resolution order), and the note that carries
// position, players, known hands and the question. These are pure
// assertions over a hand-built engine.State; no model, no store.

import (
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/table/engine"
)

// liveState builds a two-seat game mid-combat-magic: Collin controls a
// boosted creature and an anthem, Bob controls a tapped artifact, the
// stack holds Bob's burn spell targeting Collin's creature under Collin's
// countermagic, and one trigger waits in the queue. The positions the
// judge questions key on are all represented.
func liveState() *engine.State {
	st := engine.NewState()
	st.Status = engine.StatusActive
	st.Turn, st.TurnSeat = 7, 1
	st.Phase, st.Step = "precombat_main", "main"
	st.PrioritySeat = 2
	st.Order = []int{1, 2}
	st.Seats[1] = &engine.Player{Seat: 1, Name: "Collin", Life: 37, Alive: true,
		Counters: map[string]int{"poison": 2}, Hand: engine.KnownCount(4), Library: engine.KnownCount(83),
		HandKnown: []string{"Island"}}
	st.Seats[2] = &engine.Player{Seat: 2, Name: "Bob", Life: 28, Alive: true,
		Hand: engine.KnownCount(6), Library: engine.UnknownCount()}

	two := 2
	st.Objects[1] = &engine.Object{ID: 1, Zone: engine.ZoneBattlefield, Owner: 1, Controller: 1,
		Identity: engine.Identity{Card: "Atraxa, Praetors' Voice"},
		Base:     engine.BaseChars{Types: []string{"Creature"}, Power: &two, Toughness: &two},
		Counters: map[string]int{"+1/+1": 2}}
	st.Objects[2] = &engine.Object{ID: 2, Zone: engine.ZoneBattlefield, Owner: 1, Controller: 1,
		Identity: engine.Identity{Card: "Glorious Anthem"},
		Base:     engine.BaseChars{Types: []string{"Enchantment"}}}
	st.Objects[3] = &engine.Object{ID: 3, Zone: engine.ZoneBattlefield, Owner: 2, Controller: 2, Tapped: true,
		Identity: engine.Identity{Card: "Sol Ring"},
		Base:     engine.BaseChars{Types: []string{"Artifact"}}}
	st.NextObject = 4

	st.Stack = []engine.StackItem{
		{Mode: "cast", Card: "Lightning Bolt", Controller: 2, Targets: []engine.Target{{Object: 1}}},
		{Mode: "cast", Card: "Counterspell", Controller: 1, Targets: []engine.Target{{Card: "Lightning Bolt"}}},
	}
	st.TriggerQueue = []engine.TriggerItem{{Card: "Rhystic Study", Effect: "may draw a card", Controller: 1}}
	return st
}

func TestFromGame_BoardCarriesLiveObjects(t *testing.T) {
	in := FromGame(liveState(), 1, "why is Atraxa 7/7?")
	if len(in.Board.Permanents) != 3 {
		t.Fatalf("permanents = %d, want 3", len(in.Board.Permanents))
	}
	atraxa := in.Board.Permanents[0]
	if atraxa.Name != "Atraxa, Praetors' Voice" || atraxa.Controller != "Collin" {
		t.Errorf("first permanent = %+v, want Collin's Atraxa", atraxa)
	}
	if atraxa.Counters != "+1/+1×2" {
		t.Errorf("counters = %q, want +1/+1×2", atraxa.Counters)
	}
	// The computed P/T — base 2/2, two counters, and Glorious Anthem's
	// +1/+1 (a pt_modify modifier the base state omits) — rides the note.
	if !strings.Contains(atraxa.Note, "4/4") {
		t.Errorf("note should carry the computed P/T, got %q", atraxa.Note)
	}
	sol := in.Board.Permanents[2]
	if !sol.Tapped || sol.Controller != "Bob" {
		t.Errorf("Sol Ring = %+v, want Bob's tapped copy", sol)
	}
}

func TestFromGame_BoardComputesThroughModifiers(t *testing.T) {
	st := liveState()
	one := 1
	st.Objects[1].Modifiers = []engine.Modifier{{
		Layer:      engine.LayerPTModify,
		Delta:      engine.Delta{Power: &one, Toughness: &one},
		SourceCard: "Glorious Anthem",
	}}
	in := FromGame(st, 1, "why is Atraxa 7/7?")
	if !strings.Contains(in.Board.Permanents[0].Note, "5/5") {
		t.Errorf("note should carry the modifier-walked P/T 5/5, got %q", in.Board.Permanents[0].Note)
	}
}

func TestFromGame_SequenceIsResolutionOrder(t *testing.T) {
	in := FromGame(liveState(), 1, "what resolves next?")
	if len(in.Sequence.Steps) != 3 {
		t.Fatalf("steps = %d, want 3 (waiting trigger + two stack objects)", len(in.Sequence.Steps))
	}
	// The waiting trigger is on top of everything: it reads first.
	if !strings.Contains(in.Sequence.Steps[0].Text, "waiting trigger") || !strings.Contains(in.Sequence.Steps[0].Text, "Rhystic Study") {
		t.Errorf("step 0 = %q, want the waiting Rhystic Study trigger", in.Sequence.Steps[0].Text)
	}
	// Then the stack top-down: Counterspell (targeting the Bolt) before the Bolt.
	if !strings.Contains(in.Sequence.Steps[1].Text, "Counterspell") || !strings.Contains(in.Sequence.Steps[1].Text, "Lightning Bolt") {
		t.Errorf("step 1 = %q, want Collin casting Counterspell at Lightning Bolt", in.Sequence.Steps[1].Text)
	}
	if !strings.Contains(in.Sequence.Steps[2].Text, "Lightning Bolt") || !strings.Contains(in.Sequence.Steps[2].Text, "Atraxa") {
		t.Errorf("step 2 = %q, want the Bolt targeting Atraxa", in.Sequence.Steps[2].Text)
	}
	if in.Sequence.Steps[1].Controller != "Collin" || in.Sequence.Steps[2].Controller != "Bob" {
		t.Errorf("controllers = %q/%q, want Collin/Bob", in.Sequence.Steps[1].Controller, in.Sequence.Steps[2].Controller)
	}
}

func TestFromGame_NoteCarriesPositionPlayersQuestion(t *testing.T) {
	in := FromGame(liveState(), 1, "can I respond to this?")
	for _, want := range []string{
		"turn 7", "Collin's turn", "precombat main phase", "Bob holds priority",
		"Collin: 37 life", "poison 2", "hand 4", "library ?",
		"Island", "QUESTION: can I respond to this?",
	} {
		if !strings.Contains(in.Note, want) {
			t.Errorf("note missing %q:\n%s", want, in.Note)
		}
	}
}

func TestFromGame_TrackedZonesRideTheNote(t *testing.T) {
	st := liveState()
	st.Objects[4] = &engine.Object{ID: 4, Zone: engine.ZoneGraveyard, Owner: 2, Controller: 2,
		Identity: engine.Identity{Card: "Island"}}
	st.Objects[5] = &engine.Object{ID: 5, Zone: engine.ZoneGraveyard, Owner: 2, Controller: 2,
		Identity: engine.Identity{Card: "Island"}}
	st.NextObject = 6
	in := FromGame(st, 1, "what's in Bob's graveyard?")
	if !strings.Contains(in.Note, "Bob's graveyard: Island×2") {
		t.Errorf("note should collapse the graveyard tally, got:\n%s", in.Note)
	}
}

func TestFromGame_TokensAndUnknownsStayHonest(t *testing.T) {
	st := liveState()
	p, tq := 1, 1
	st.Objects[6] = &engine.Object{ID: 6, Zone: engine.ZoneBattlefield, Owner: 1, Controller: 1,
		Identity: engine.Identity{Token: &engine.TokenSpec{Name: "Soldier", Power: &p, Toughness: &tq}},
		// The fold derives a token's base characteristics from its spec
		// (apply.tokenBase) — the same shape a real CREATE_TOKEN holds.
		Base: engine.BaseChars{Types: []string{"Creature"}, Power: &p, Toughness: &tq}}
	st.Objects[7] = &engine.Object{ID: 7, Zone: engine.ZoneBattlefield, Owner: 2, Controller: 2}
	st.NextObject = 8
	in := FromGame(st, 1, "can I attack with everything?")
	var sawToken, sawUnknown bool
	for _, perm := range in.Board.Permanents {
		if perm.Name == "Soldier token" && strings.Contains(perm.Note, "1/1") {
			sawToken = true
		}
		if perm.Name == "unknown card" {
			sawUnknown = true
		}
	}
	if !sawToken {
		t.Errorf("board should spell the token as 'Soldier token' with its declared 1/1:\n%+v", in.Board.Permanents)
	}
	if !sawUnknown {
		t.Errorf("board should keep an unidentified object honestly unknown")
	}
}

func TestFromGame_NilStateCarriesTheQuestion(t *testing.T) {
	in := FromGame(nil, 1, "does this work?")
	if in.Note != "QUESTION: does this work?" {
		t.Errorf("note = %q, want the bare question", in.Note)
	}
}
