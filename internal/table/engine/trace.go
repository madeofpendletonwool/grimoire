package engine

// Provenance answers (MAD-334, stage 5 of MAD-321): the deterministic
// half of the reasoning layer. "Why is this creature 7/7?", "why did my
// creature die?" and "what happened on turn 5?" are queries over rows
// the log already holds — no model calls, no tokens, milliseconds. Like
// knowledge.Summarize on the campaign side, this is a query worth more
// than most generative features, and it is the never-stored-flattened
// rule made visible: because characteristics are base + modifiers +
// counters computed at read time, the computation can always be spelled
// out row by row.

import (
	"fmt"
	"sort"
	"strings"
)

/* ---------- "why is this creature 7/7?" ---------- */

// TraceChange is one non-P/T characteristic change row: the control,
// type, color, ability and copy effects the layer walk applies. The P/T
// half of the trace is PTLine; together they cover every computed
// characteristic the board renders.
type TraceChange struct {
	Label    string `json:"label"`
	Layer    string `json:"layer"`
	Duration string `json:"duration,omitempty"`
	Change   string `json:"change"`
	// SourceGone marks a while_source_present effect whose source object
	// is no longer on the battlefield the trace renders against — the
	// honest spelling for a row the fold has not (or, at a historical
	// ordinal, had not yet) retired.
	SourceGone bool `json:"source_gone,omitempty"`
}

// CharTrace is the full provenance of one object's computed
// characteristics at a moment: the P/T stack (base, modifiers in CR 613
// order, counters, the switch, the total) plus every other layer's
// changes. A SELECT over modifier and counter rows the log already
// holds — the acceptance property is that every computed value on the
// board traces through here with nothing inferred.
type CharTrace struct {
	Object  int64         `json:"object"`
	Name    string        `json:"name"`
	Zone    Zone          `json:"zone,omitempty"`
	PT      []PTLine      `json:"pt"`
	Changes []TraceChange `json:"changes,omitempty"`
}

// CharTrace renders the object's characteristic provenance against this
// state. The state may be a historical fold (the death report folds to
// the moment before a DIED row), so no clock, no write, no guess: what
// the rows say is what the trace spells.
func (s *State) CharTrace(id int64) *CharTrace {
	o, ok := s.Objects[id]
	if !ok {
		return nil
	}
	name := objectName(o)
	t := &CharTrace{Object: id, Name: name, Zone: o.Zone, PT: s.PTTrace(id)}
	for _, layer := range layerOrder {
		for _, mod := range o.Modifiers {
			if mod.Layer != layer {
				continue
			}
			change, ok := changePhrase(s, mod)
			if !ok {
				continue
			}
			row := TraceChange{Label: mod.label(s), Layer: string(mod.Layer),
				Duration: string(mod.Duration), Change: change}
			if modifierSourceGone(s, mod) {
				row.SourceGone = true
			}
			t.Changes = append(t.Changes, row)
		}
	}
	return t
}

// sourceOnBattlefield reports whether a modifier's source object is
// still a battlefield presence — the question while_source_present
// effects hang on, and the case the trace must spell honestly rather
// than silently keep counting: "the source has left the battlefield".
func sourceOnBattlefield(s *State, src int64) bool {
	o, ok := s.Objects[src]
	return ok && o.Zone == ZoneBattlefield && !o.Phased
}

// changePhrase spells one non-P/T modifier's delta as the sentence the
// trace renders. pt layers are the PT lines' to spell; text-layer prose
// rides in EFFECT_DECLARED rows, not deltas, so it is absent here too.
func changePhrase(s *State, mod Modifier) (string, bool) {
	d := mod.Delta
	var parts []string
	switch mod.Layer {
	case LayerCopy:
		if d.CopyOf != 0 {
			if src, ok := s.Objects[d.CopyOf]; ok {
				return fmt.Sprintf("becomes a copy of %s", objectName(src)), true
			}
			return "becomes a copy (source gone)", true
		}
	case LayerControl:
		if d.Controller != nil {
			return fmt.Sprintf("controller → %s", s.seatName(*d.Controller)), true
		}
	case LayerType:
		if n := len(d.RemoveTypes); n > 0 {
			parts = append(parts, "loses "+strings.Join(d.RemoveTypes, ", "))
		}
		if n := len(d.AddTypes); n > 0 {
			parts = append(parts, "gains "+strings.Join(d.AddTypes, ", "))
		}
	case LayerColor:
		if n := len(d.RemoveColors); n > 0 {
			parts = append(parts, "loses "+strings.Join(d.RemoveColors, ", "))
		}
		if n := len(d.AddColors); n > 0 {
			parts = append(parts, "gains "+strings.Join(d.AddColors, ", "))
		}
	case LayerAbility:
		if n := len(d.RemoveKeywords); n > 0 {
			parts = append(parts, "loses "+strings.Join(d.RemoveKeywords, ", "))
		}
		if n := len(d.AddKeywords); n > 0 {
			parts = append(parts, "gains "+strings.Join(d.AddKeywords, ", "))
		}
	default:
		return "", false // pt layers spell in the PT lines
	}
	if len(parts) == 0 {
		return "", false
	}
	return strings.Join(parts, "; "), true
}

// seatName names a seat for change phrases — the seated name, else the
// number, never a blank.
func (s *State) seatName(seat int) string {
	if p, ok := s.Seats[seat]; ok && p.Name != "" {
		return p.Name
	}
	return fmt.Sprintf("seat %d", seat)
}

/* ---------- "why did my creature die?" ---------- */

// DamageLine is one source's marked damage at the moment of a death —
// the rows the lethal-damage check read, each with its source named so
// the log entry that says why is the one a table argues with.
type DamageLine struct {
	Source     string `json:"source"`
	Amount     int    `json:"amount"`
	Deathtouch bool   `json:"deathtouch,omitempty"`
}

// DeathReport walks backwards from one DIED row: the state-based action
// that caused it (with its CR citation), the full P/T stack in force
// the instant before — which is why the report folds the log itself —
// the marked damage with sources, and the last table action that
// preceded the sweep. The whole report is a fold plus renders; viewers
// never re-run CR 704.
type DeathReport struct {
	Object      int64        `json:"object"`
	Name        string       `json:"name"`
	Ord         int64        `json:"ord"`
	Turn        int          `json:"turn,omitempty"`
	Cause       string       `json:"cause"`
	CauseNote   string       `json:"cause_note"`
	PT          []PTLine     `json:"pt"`
	Damage      []DamageLine `json:"damage,omitempty"`
	DamageTotal int          `json:"damage_total,omitempty"`
	Toughness   int          `json:"toughness,omitempty"`
	// TriggeredBy is the last non-system row before the death: the
	// happening at the table that put the state-based check over the
	// line. Engine-asserted sweep rows carry source "system", so the
	// walk skips them — the trigger is a player's act, not the rules'.
	TriggeredBy *Event `json:"triggered_by,omitempty"`
}

// DeathTrace answers "why did my creature die?" for the DIED row at an
// ordinal. It folds the log up to (not including) that row, so the
// modifiers, counters and damage in the report are exactly the ones in
// force the moment before the death — same-batch sweep predecessors
// included, because a chain of deaths is folded in order.
func DeathTrace(log []Event, ord int64) (*DeathReport, error) {
	idx := -1
	var row Event
	for i := range log {
		if log[i].Ord == ord {
			idx, row = i, log[i]
			break
		}
	}
	if idx < 0 {
		return nil, fmt.Errorf("%w: no event at ordinal %d", ErrInvalid, ord)
	}
	if row.Kind != EventDied {
		return nil, fmt.Errorf("%w: ordinal %d is a %s row, not a death", ErrInvalid, ord, row.Kind)
	}
	pre := Fold(log[:idx])
	o, ok := pre.Objects[row.Object]
	if !ok {
		return nil, fmt.Errorf("%w: object %d is not in the log before ordinal %d", ErrInvalid, row.Object, ord)
	}
	rep := &DeathReport{
		Object:    row.Object,
		Name:      objectName(o),
		Ord:       ord,
		Turn:      pre.Turn,
		Cause:     row.Cause,
		CauseNote: deathCauseNote(pre, o, row.Cause),
		PT:        pre.PTTrace(row.Object),
	}
	if c := pre.Characteristics(row.Object); c.Toughness != nil {
		rep.Toughness = *c.Toughness
	}
	rep.DamageTotal = o.Damage
	for _, src := range sortedInt64Keys(o.DamageBySource) {
		n := o.DamageBySource[src]
		if n <= 0 {
			continue
		}
		line := DamageLine{Amount: n}
		if srcObj, ok := pre.Objects[src]; ok {
			line.Source = objectName(srcObj)
			line.Deathtouch = !srcObj.Phased && hasString(pre.Keywords(src), "Deathtouch")
		} else {
			line.Source = fmt.Sprintf("object %d", src)
		}
		rep.Damage = append(rep.Damage, line)
	}
	for i := idx - 1; i >= 0; i-- {
		if log[i].Source != "system" {
			trigger := log[i]
			rep.TriggeredBy = &trigger
			break
		}
	}
	return rep, nil
}

// deathCauseNote spells the state-based action's rule, with the
// deathtouch exception the lethal check folds in (CR 702.2c: any
// nonzero damage from a deathtouch source is lethal).
func deathCauseNote(s *State, o *Object, cause string) string {
	switch cause {
	case causeZeroToughness:
		return "toughness was 0 or less (CR 704.5f)"
	case causeLethalDamage:
		if s.deathtouchMarked(o) {
			return "damage from a deathtouch source — any amount is lethal (CR 704.5g, 702.2c)"
		}
		return "marked damage met or exceeded toughness (CR 704.5g)"
	case causeLegendRule:
		return "the legend rule — the same legendary name under one controller twice (CR 704.5j)"
	case causeNoLoyalty:
		return "no loyalty counters remained (CR 704.5i)"
	}
	return cause
}

/* ---------- "what happened on turn 5?" ---------- */

// TurnSlice returns one turn's rows: from its TURN_STARTED anchor up to
// (not including) the next turn's — TURN_ENDED included. The turn
// number is fold state every row lands inside, but only the anchors
// carry it, so the slice is the two anchors' span. An absent turn is an
// empty slice, which the caller may honestly report as "no such turn".
func TurnSlice(log []Event, turn int) []Event {
	start := -1
	for i := range log {
		if log[i].Kind == EventTurnStarted && log[i].Turn == turn {
			start = i
			break
		}
	}
	if start < 0 {
		return nil
	}
	out := []Event{}
	for i := start; i < len(log); i++ {
		if i > start && log[i].Kind == EventTurnStarted {
			break
		}
		out = append(out, log[i])
	}
	return out
}

// sortedInt64Keys lists an int64-keyed map's keys ascending, for
// deterministic damage order.
func sortedInt64Keys(m map[int64]int) []int64 {
	out := make([]int64, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
