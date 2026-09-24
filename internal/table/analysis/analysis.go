// Package analysis is the Magic table's post-game coach (MAD-339,
// stage 7 of MAD-321): the deterministic read over a finished — or
// paused — game's event log that the model layer interprets.
//
// The split is the plan's own rule: analysis is an LLM read **over a
// deterministic log** — it interprets recorded facts and never invents
// state. Everything this package computes is a fold over rows the log
// already holds: turns, draws, cards left in hand, untapped mana
// sources at the end of a seat's final turn, damage dealt and taken,
// and the missed-trigger diff — today's trigger registry replayed over
// the log against the TRIGGER_FIRED rows that actually landed. No
// model in this path, no new storage, and the honesty rules hold:
// unknown stays unknown, floating mana is never modelled (unspent
// *sources* are a read; mana pools are not), and a trigger the
// registry did not know at the time is reported as "with today's
// registry", because that is exactly what the derivation knows.
//
// Input scope is the caller's: Summarize folds the event slice it is
// handed, and the server hands it the requesting viewer's scoped
// stream (ADR 13) — so a seat's report physically cannot contain
// another seat's rows, the same construction the pod's reads use.
package analysis

import (
	"fmt"
	"sort"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/table/engine"
)

// SeatLine is one seat's public line: the facts every viewer of the
// public stream may read.
type SeatLine struct {
	Seat      int    `json:"seat"`
	Name      string `json:"name"`
	Life      int    `json:"life"`
	Alive     bool   `json:"alive"`
	LeftCause string `json:"left_cause,omitempty"`
}

// GameFacts is the game's frame: lifecycle, turn count, why it ended.
type GameFacts struct {
	Status  string     `json:"status"`
	Format  string     `json:"format,omitempty"`
	Turns   int        `json:"turns"`
	HeadOrd int64      `json:"head_ord"`
	Reason  string     `json:"reason,omitempty"`
	Seats   []SeatLine `json:"seats"`
}

// FinalTurnSnapshot is the seat's last turn, read at the moment it
// ended — the resource-usage half of the coach's brief.
type FinalTurnSnapshot struct {
	Turn       int   `json:"turn"`
	EndOrd     int64 `json:"end_ord"`
	EndedTurn  bool  `json:"ended_turn"` // false: the game ended mid-turn
	Hand       int   `json:"hand"`
	HandKnown  bool  `json:"hand_known"`
	Lands      int   `json:"lands"`       // mana sources controlled, untapped
	LandsKnown bool  `json:"lands_known"` // false when no land was ever identified for this seat
	LandDrops  int   `json:"land_drops"`  // lands played on the final turn
}

// MissedTrigger is one registry match that fired nowhere: today's
// registry knows the card, the happening is in the log, and no
// TRIGGER_FIRED row landed for it.
type MissedTrigger struct {
	Card      string `json:"card"`
	Effect    string `json:"effect,omitempty"`
	At        int64  `json:"at"`
	Happening string `json:"happening,omitempty"`
}

// SeatFacts is the requested seat's report: everything the log holds
// about how they played, from their own scoped stream.
type SeatFacts struct {
	Seat           int               `json:"seat"`
	Name           string            `json:"name"`
	TurnsPlayed    int               `json:"turns_played"`
	FinalTurn      FinalTurnSnapshot `json:"final_turn"`
	Drawn          int               `json:"drawn"`
	HandAtEnd      int               `json:"hand_at_end"`
	HandKnownAtEnd bool              `json:"hand_known_at_end"`
	// HandKnown lists the identities the seat had seen of its own cards
	// when the log ends — knowledge, not contents, and present only on
	// the seat's own scoped stream.
	HandKnown      []string        `json:"hand_known,omitempty"`
	LandsPlayed    int             `json:"lands_played"`
	Casts          int             `json:"casts"`
	DamageDealt    int             `json:"damage_dealt"`
	DamageTaken    int             `json:"damage_taken"`
	LifeLost       int             `json:"life_lost"`
	LifeGained     int             `json:"life_gained"`
	Eliminated     bool            `json:"eliminated"`
	LeftCause      string          `json:"left_cause,omitempty"`
	MissedTriggers []MissedTrigger `json:"missed_triggers,omitempty"`
}

// Summary is the whole deterministic report: the game's frame, the
// seat's facts, and the table's public lines.
type Summary struct {
	Game GameFacts `json:"game"`
	Seat SeatFacts `json:"seat"`
	// ManaNote states the honesty rule the resource numbers carry:
	// floating mana is never modelled; unspent sources are a read.
	ManaNote string `json:"mana_note"`
}

// manaNote is phrased once, here, so the surface and the prompt can
// never disagree about what "unused mana" means.
const manaNote = "floating mana is never modelled — unspent mana sources (untapped lands) at the end of the final turn are the honest read"

// Summarize derives the seat's deterministic report over a scoped
// event log. Pure: same log in, same report out, byte for byte.
func Summarize(evs []engine.Event, reg engine.TriggerRegistry, seat int) *Summary {
	st := engine.Fold(evs)
	out := &Summary{ManaNote: manaNote}
	out.Game = gameFacts(st, evs)
	out.Seat = seatFacts(st, evs, seat)
	out.Seat.MissedTriggers = missedTriggers(evs, reg, seat)
	return out
}

/* ---------- the game's frame ---------- */

func gameFacts(st *engine.State, evs []engine.Event) GameFacts {
	g := GameFacts{Status: string(st.Status), Format: st.Format, Turns: st.Turn}
	if len(evs) > 0 {
		g.HeadOrd = evs[len(evs)-1].Ord
	}
	seats := make([]int, 0, len(st.Seats))
	for seat := range st.Seats {
		seats = append(seats, seat)
	}
	sort.Ints(seats)
	for _, seat := range seats {
		p := st.Seats[seat]
		line := SeatLine{Seat: seat, Name: seatLabel(p, seat), Life: p.Life, Alive: p.Alive}
		g.Seats = append(g.Seats, line)
	}
	left := map[int]string{}
	for _, e := range evs {
		if e.Kind == engine.EventPlayerLeft && e.TargetSeat != 0 {
			left[e.TargetSeat] = e.Cause
		}
	}
	for i := range g.Seats {
		g.Seats[i].LeftCause = left[g.Seats[i].Seat]
	}
	for _, e := range evs {
		if e.Kind == engine.EventGameEnded && g.Reason == "" {
			g.Reason = e.Reason
		}
	}
	return g
}

func seatLabel(p *engine.Player, seat int) string {
	if p != nil && p.Name != "" {
		return p.Name
	}
	return fmt.Sprintf("Seat %d", seat)
}

/* ---------- the seat's facts ---------- */

func seatFacts(st *engine.State, evs []engine.Event, seat int) SeatFacts {
	f := SeatFacts{Seat: seat}
	p := st.Seats[seat]
	if p != nil {
		f.Name = seatLabel(p, seat)
		f.HandAtEnd = p.Hand.N
		f.HandKnownAtEnd = p.Hand.Known
		f.HandKnown = append([]string{}, p.HandKnown...)
		f.Eliminated = !p.Alive
	}

	// The walk keeps a running fold so damage attribution can read the
	// source object's controller as it stood the moment damage landed.
	run := engine.NewState()
	lastStart := int64(0) // ord of the seat's last TURN_STARTED
	lastEnd := int64(0)   // ord of the TURN_ENDED that closed it
	endedTurn := false
	finalTurn := 0
	turnDrops := 0
	for _, e := range evs {
		switch e.Kind {
		case engine.EventTurnStarted:
			if e.TurnSeat == seat {
				lastStart = e.Ord
				lastEnd = 0
				endedTurn = false
				finalTurn = e.Turn
				f.TurnsPlayed++
				turnDrops = 0
			}
		case engine.EventTurnEnded:
			if lastStart > 0 && e.Ord > lastStart && lastEnd == 0 {
				lastEnd = e.Ord
				endedTurn = true
			}
		case engine.EventLandPlayed:
			if e.Controller == seat {
				f.LandsPlayed++
				if lastStart > 0 && e.Ord > lastStart && (lastEnd == 0 || e.Ord < lastEnd) {
					turnDrops++
				}
			}
		case engine.EventCast:
			if e.ActorSeat == seat {
				f.Casts++
			}
		case engine.EventCardDrawn:
			if e.TargetSeat == seat {
				f.Drawn += max(e.Count, 1)
			}
		case engine.EventDamageDealt:
			if e.TargetSeat == seat {
				f.DamageTaken += e.Amount
			}
			if e.SourceObj != 0 {
				if src, ok := run.Objects[e.SourceObj]; ok && src.Controller == seat {
					f.DamageDealt += e.Amount
				}
			}
		case engine.EventLifeChanged:
			if e.TargetSeat == seat && e.To == nil {
				if e.Delta < 0 {
					f.LifeLost += -e.Delta
				} else {
					f.LifeGained += e.Delta
				}
			}
		case engine.EventPlayerLeft:
			if e.TargetSeat == seat {
				f.LeftCause = e.Cause
				f.Eliminated = true
			}
		}
		run.FoldInto([]engine.Event{e})
	}

	f.FinalTurn = finalTurnSnapshot(f, evs, endOrd(lastStart, lastEnd, run.LastOrd), finalTurn, endedTurn, turnDrops)
	return f
}

// endOrd picks the snapshot's anchor: the anchor that closed the seat's
// last turn when it closed, the turn's start when the game ended
// mid-turn, else the head.
func endOrd(lastStart, lastEnd, head int64) int64 {
	if lastEnd > 0 {
		return lastEnd
	}
	if lastStart > 0 {
		return lastStart
	}
	return head
}

// finalTurnSnapshot folds the prefix up to the anchor and reads the
// resource picture the coach interprets.
func finalTurnSnapshot(f SeatFacts, evs []engine.Event, at int64, finalTurn int, endedTurn bool, turnDrops int) FinalTurnSnapshot {
	var upto []engine.Event
	for _, e := range evs {
		if e.Ord > at {
			break
		}
		upto = append(upto, e)
	}
	snap := FinalTurnSnapshot{Turn: finalTurn, EndOrd: at, EndedTurn: endedTurn, LandDrops: turnDrops}
	state := engine.Fold(upto)
	if p := state.Seats[f.Seat]; p != nil {
		snap.Hand = p.Hand.N
		snap.HandKnown = p.Hand.Known
	}
	// Untapped mana sources: what the fold can honestly name. A land
	// nobody ever identified is still a land if its declared types say
	// so; a permanent with no declared types counts nowhere, and
	// LandsKnown says whether the seat ever had one that did.
	sawLand := false
	for _, id := range objectIDs(state) {
		o, ok := state.Objects[id]
		if !ok || o.Zone != engine.ZoneBattlefield || o.Phased || o.Controller != f.Seat {
			continue
		}
		if !hasType(state.Characteristics(id).Types, "Land") {
			continue
		}
		sawLand = true
		if !o.Tapped {
			snap.Lands++
		}
	}
	snap.LandsKnown = sawLand || f.LandsPlayed > 0
	return snap
}

// objectIDs lists the fold's object ids in mint order — the same
// deterministic walk the registry's own matching uses.
func objectIDs(st *engine.State) []int64 {
	ids := make([]int64, 0, len(st.Objects))
	for id := range st.Objects {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func hasType(types []string, want string) bool {
	for _, t := range types {
		if strings.EqualFold(t, want) {
			return true
		}
	}
	return false
}
