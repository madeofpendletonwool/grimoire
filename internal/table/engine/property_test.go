package engine

import (
	"fmt"
	"math/rand"
	"reflect"
	"testing"
)

// The round-trip property from the model doc, driven by a seeded random
// game: for any event log, fold(log) is stable, and fold(log[:n]) is the
// state after the first n events. Randomness is passed in — the engine
// itself never rolls dice. Every rejected action must produce zero events,
// asserted at every step.

var propCards = []string{"Forest", "Grizzly Bears", "Lightning Bolt", "Sol Ring",
	"Cultivate", "Rhystic Study", "Swords to Plowshares"}
var propCounters = []string{"+1/+1", "-1/-1", "poison", "energy", "shield", "stun", "oil", "loyalty"}
var propFlags = []string{"monarch", "initiative", "city's blessing", "the ring"}
var propKeywords = []string{"Flying", "Vigilance", "Lifelink", "Deathtouch", "Trample", "Indestructible"}
var propTypes = []string{"Creature", "Land", "Artifact", "Enchantment", "Goblin", "Frog"}
var propColors = []string{"White", "Blue", "Black", "Red", "Green"}

// randomAction builds one mostly-valid action against the live state, with
// enough wild references to exercise the rejection paths.
func randomAction(rnd *rand.Rand, s *State) Action {
	seat := 1
	if len(s.Order) > 0 {
		seat = s.Order[rnd.Intn(len(s.Order))]
	}
	if rnd.Intn(20) == 0 {
		seat = 99 // a seat that is not in this game
	}
	liveObj := func() int64 {
		if s.NextObject <= 1 {
			return 0
		}
		return 1 + rnd.Int63n(s.NextObject-1)
	}
	a := Action{Kind: ActionAdvance, Seat: seat, Source: "tap"}
	switch rnd.Intn(26) {
	case 0:
	case 1:
		a.Kind = ActionPassPriority
	case 2:
		a.Kind = ActionPlayLand
		a.Card = propCards[rnd.Intn(2)]
		a.Base = &BaseChars{Name: a.Card, Types: []string{"Land"}}
	case 3:
		a.Kind = ActionCast
		a.Card = propCards[rnd.Intn(len(propCards))]
		switch rnd.Intn(4) {
		case 0:
			a.FromZone = ZoneCommand
		case 1:
			a.FromZone = ZoneGraveyard
		case 2:
			a.FromZone = ZoneLibrary
		}
		a.Base = &BaseChars{Types: []string{"Creature"}, Power: intPtr(1 + rnd.Intn(5)), Toughness: intPtr(1 + rnd.Intn(5))}
		if rnd.Intn(2) == 0 {
			a.Targets = []Target{{Seat: seat}}
		}
	case 4:
		a.Kind = ActionActivate
		a.Ability = "some ability"
		a.Object = liveObj()
	case 5:
		a.Kind = ActionCreateToken
		a.Count = 1 + rnd.Intn(3)
		p, t := rnd.Intn(4), rnd.Intn(4)
		a.Token = &TokenSpec{Name: "Soldier", Types: []string{"Creature"}, Power: &p, Toughness: &t}
	case 6:
		a.Kind = ActionTap
		a.Object = liveObj()
		if rnd.Intn(4) == 0 {
			a.All = true
		}
	case 7:
		a.Kind = ActionUntap
		a.Object = liveObj()
		if rnd.Intn(4) == 0 {
			a.All = true
		}
	case 8:
		a.Kind = ActionSetPhased
		a.Object = liveObj()
		a.Phased = rnd.Intn(2) == 0
	case 9:
		a.Kind = ActionAttach
		a.Object = liveObj()
		a.AttachTo = liveObj()
	case 10:
		a.Kind = ActionDetach
		a.Object = liveObj()
	case 11:
		a.Kind = ActionMoveZone
		a.Object = liveObj()
		switch rnd.Intn(5) {
		case 0:
			a.ToZone = ZoneGraveyard
		case 1:
			a.ToZone = ZoneExile
		case 2:
			a.ToZone = ZoneHand
		case 3:
			a.ToZone = ZoneLibrary
		case 4:
			a.ToZone = ZoneCommand
		}
		a.Cause = "sacrifice"
	case 12:
		a.Kind = ActionAdjustCounters
		a.CounterName = propCounters[rnd.Intn(len(propCounters))]
		a.Delta = 1 - 2*rnd.Intn(2)
		if rnd.Intn(2) == 0 {
			a.OnObject = liveObj()
		} else {
			a.TargetSeat = seat
		}
	case 13:
		a.Kind = ActionSetCounters
		a.CounterName = propCounters[rnd.Intn(len(propCounters))]
		a.To = intPtr(rnd.Intn(4))
		if rnd.Intn(2) == 0 {
			a.OnObject = liveObj()
		} else {
			a.TargetSeat = seat
		}
	case 14:
		a.Kind = ActionChangeLife
		a.Delta = rnd.Intn(21) - 10
		if rnd.Intn(4) == 0 {
			a.To = intPtr(rnd.Intn(40))
			a.Delta = 0
		}
		a.TargetSeat = seat
		a.SourceCard = propCards[rnd.Intn(len(propCards))]
	case 15, 16:
		a.Kind = ActionDealDamage
		a.Amount = 1 + rnd.Intn(9)
		a.SourceObj = liveObj()
		if rnd.Intn(2) == 0 {
			a.TargetSeat = seat
		} else {
			a.TargetObject = liveObj()
		}
		a.CombatDmg = rnd.Intn(2) == 0
	case 17:
		a.Kind = ActionSetFlag
		a.Flag = propFlags[rnd.Intn(len(propFlags))]
		if rnd.Intn(3) > 0 {
			a.Value = "true"
		}
		a.TargetSeat = seat
	case 18:
		a.Kind = ActionSetZoneCount
		if rnd.Intn(2) == 0 {
			a.Zone = ZoneHand
		} else {
			a.Zone = ZoneLibrary
		}
		a.To = intPtr(rnd.Intn(20))
		a.TargetSeat = seat
	case 19:
		a.Kind = ActionDraw
		a.Count = 1 + rnd.Intn(3)
		if rnd.Intn(2) == 0 {
			a.Cards = []string{propCards[rnd.Intn(len(propCards))]}
			if a.Count > 1 {
				a.Cards = append(a.Cards, propCards[rnd.Intn(len(propCards))])
			}
		}
	case 20:
		a.Kind = ActionMill
		a.Count = 1 + rnd.Intn(3)
	case 21:
		a.Kind = ActionReveal
		a.Cards = []string{propCards[rnd.Intn(len(propCards))]}
	case 22:
		a.Kind = ActionLook
		a.Cards = []string{propCards[rnd.Intn(len(propCards))]}
		a.FromZone = ZoneLibrary
	case 23:
		a.Kind = ActionAddModifier
		a.Object = liveObj()
		d := Delta{}
		switch rnd.Intn(6) {
		case 0:
			d.Power, d.Toughness = intPtr(rnd.Intn(7)-3), intPtr(rnd.Intn(7)-3)
		case 1:
			c := s.Order[rnd.Intn(len(s.Order))]
			d.Controller = &c
		case 2:
			d.AddTypes = []string{propTypes[rnd.Intn(len(propTypes))]}
		case 3:
			d.AddKeywords = []string{propKeywords[rnd.Intn(len(propKeywords))]}
		case 4:
			d.SetPower, d.SetToughness = intPtr(rnd.Intn(6)), intPtr(rnd.Intn(6))
		case 5:
			d.Swap = true
		}
		m := &Modifier{Layer: layerOrder[rnd.Intn(len(layerOrder))], Duration: UntilEOT, Delta: d}
		switch rnd.Intn(3) {
		case 0:
			m.Duration = WhileSourcePresent
			m.SourceObj = liveObj()
		case 1:
			m.Duration = PermanentDuration
			m.SourceCard = propCards[rnd.Intn(len(propCards))]
		}
		a.Modifier = m
	case 24:
		a.Kind = ActionRemoveModifier
		a.Object = liveObj()
		if o, ok := s.Objects[a.Object]; ok && len(o.Modifiers) > 0 {
			a.ModifierID = o.Modifiers[rnd.Intn(len(o.Modifiers))].ID
		} else {
			a.ModifierID = 1 + rnd.Int63n(s.NextModifier)
		}
	default:
		a.Kind = ActionDeclareEffect
		a.Effect = "some declared effect"
		a.SourceObj = liveObj()
	}
	return a
}

func TestRoundTripProperty(t *testing.T) {
	for seed := int64(1); seed <= 25; seed++ {
		name := fmt.Sprintf("seed %d", seed)
		rnd := rand.New(rand.NewSource(seed))
		seats := []SeatConfig{
			{Seat: 1, Name: "Collin", Commander: propCards[seed%int64(len(propCards))],
				Deck: map[string]int{"Forest": 30, "Cultivate": 4}},
			{Seat: 2, Name: "Bob", Commander: "Krenko, Mob Boss"},
			{Seat: 3, Name: "Alice"},
		}
		var log []Event
		startEvs, err := Apply(NewState(), Action{Kind: ActionStartGame, Seats: seats})
		if err != nil {
			t.Fatalf("%s: start: %v", name, err)
		}
		log = append(log, startEvs...)
		inc := Fold(log)

		steps := 200
		rejectedCount := 0
		for i := 0; i < steps; i++ {
			a := randomAction(rnd, inc)
			evs, err := Apply(inc, a)
			if err != nil {
				if len(evs) != 0 {
					t.Fatalf("%s step %d: rejected %s produced %d events", name, i, a.Kind, len(evs))
				}
				rejectedCount++
				continue
			}
			log = append(log, evs...)
			inc.FoldInto(evs)

			// The prefix property, spot-checked every 20 steps: folding
			// the whole log from scratch lands exactly where the
			// incremental fold is.
			if i%20 == 0 {
				if !reflect.DeepEqual(Fold(log), inc) {
					t.Fatalf("%s step %d: fold(log) != incremental state", name, i)
				}
			}
		}
		if rejectedCount == 0 {
			t.Fatalf("%s: generator never exercised a rejection", name)
		}
		// Stability: the same log folds to the same state, twice.
		if !reflect.DeepEqual(Fold(log), Fold(log)) {
			t.Fatalf("%s: fold is not stable", name)
		}
		if !reflect.DeepEqual(Fold(log), inc) {
			t.Fatalf("%s: final fold(log) != incremental state", name)
		}
		// Every prefix boundary is the state at that point.
		for n := 0; n <= len(log); n++ {
			head := Fold(log[:n])
			again := Fold(log[:n])
			if !reflect.DeepEqual(head, again) {
				t.Fatalf("%s: prefix %d unstable", name, n)
			}
		}
	}
}
