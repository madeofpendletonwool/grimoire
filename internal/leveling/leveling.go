// Package leveling is stage 7 of MAD-417: XP, level-ups through the
// review gate, and the post-session mechanical reconciliation pass.
//
// Three halves:
//
//   - config.go — the campaign's leveling mode (xp | milestone) under the
//     settings payload's "leveling" key, the board-config pattern: data
//     on the campaign, never a code path.
//   - leveling.go (this file) — the pure engine: the level-up diff
//     computed from internal/progression's 2014 tables, and the XP award
//     split. No database.
//   - store.go — the rows and orchestration: encounter XP awards, the
//     staged level-up behind the canon review gate, and the
//     reconciliation pass that flags drift and proposes corrections a
//     human decides on. Nothing in this package edits a sheet silently:
//     an award is a DM's confirmed write, a level-up and a correction are
//     proposals, and the gate is the gate.
package leveling

import (
	"fmt"
	"sort"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/progression"
	"github.com/madeofpendletonwool/grimoire/internal/sheet"
)

/* ---------- the XP award split ---------- */

// Split divides an encounter's XP total across the characters that fought
// it, per the 2014 DMG: each monster's value divides across the party.
// The remainder distributes one XP at a time in entity-id order —
// deterministic, so identical inputs produce byte-identical awards.
func Split(totalXP int, entityIDs []string) map[string]int {
	out := make(map[string]int, len(entityIDs))
	ids := append([]string(nil), entityIDs...)
	sort.Strings(ids)
	n := len(ids)
	if n == 0 || totalXP <= 0 {
		for _, id := range ids {
			out[id] = 0
		}
		return out
	}
	share := totalXP / n
	remainder := totalXP % n
	for i, id := range ids {
		out[id] = share
		if i < remainder {
			out[id]++
		}
	}
	return out
}

/* ---------- the level-up diff ---------- */

// SlotGain is one slot level the diff raises the maxima for.
type SlotGain struct {
	Level int `json:"level"`
	From  int `json:"from"`
	To    int `json:"to"`
}

// Diff is what one level-up changes: the review item's payload, the diff
// view's data, and the finalizer's recomputed truth. Every field is
// derived — the sheet and the 2014 tables in, the delta out — so staging
// and finalizing share one function and can never disagree about what a
// level grants.
type Diff struct {
	Entity      string     `json:"entity"`
	Name        string     `json:"name"`
	Class       string     `json:"class"`
	Subclass    string     `json:"subclass,omitempty"`
	FromClass   int        `json:"from_class_level"`
	ToClass     int        `json:"to_class_level"`
	FromTotal   int        `json:"from_total_level"`
	ToTotal     int        `json:"to_total_level"`
	NewClass    bool       `json:"new_class"`
	NewFeatures []string   `json:"new_features,omitempty"`
	NewSaves    []string   `json:"new_saves,omitempty"`
	Slots       []SlotGain `json:"slots,omitempty"`
	SlotsNote   string     `json:"slots_note,omitempty"`
	HitDie      int        `json:"hit_die"`
	HPFrom      int        `json:"hp_from"`
	HPGain      int        `json:"hp_gain"`
	HPTotal     int        `json:"hp_total"`
	ConMod      int        `json:"con_mod"`
	ConDeclared bool       `json:"con_declared"`
	XPBefore    int        `json:"xp_before"`
	XPForNext   int        `json:"xp_for_next,omitempty"`
	Mode        string     `json:"mode"`
}

// DiffInput is everything the pure diff needs: the character's current
// sheet, its name, the class being advanced, and the campaign's mode (the
// xp mode stamps the advancement numbers; milestone proposes the same
// numbers without the eligibility gate).
type DiffInput struct {
	Entity string
	Name   string
	Sheet  sheet.Sheet
	Class  string // canonical, from progression.Canonical
	Mode   string
}

// ComputeDiff derives the level-up diff for one character advancing one
// class by one level — adding a level to a class the sheet already
// carries, or taking a first level in a new one. It computes; it never
// mutates: applying the diff is ApplyDiff's job, and only the finalizer
// calls it after a human decision.
func ComputeDiff(in DiffInput) (Diff, error) {
	className, err := progression.Canonical(in.Class)
	if err != nil {
		return Diff{}, err
	}
	class := progression.Classes[className]
	s := in.Sheet

	var (
		fromClass int
		subclass  string
	)
	for _, c := range s.Classes {
		if strings.EqualFold(strings.TrimSpace(c.Class), className) {
			fromClass = c.Level
			subclass = strings.TrimSpace(c.Subclass)
			break
		}
	}
	toClass := fromClass + 1
	fromTotal := s.TotalLevel()
	toTotal := fromTotal + 1
	if toTotal > progression.MaxLevel {
		return Diff{}, fmt.Errorf("%s is level %d — the 2014 cap is %d", in.Name, fromTotal, progression.MaxLevel)
	}

	d := Diff{
		Entity: in.Entity, Name: in.Name, Class: className, Subclass: subclass,
		FromClass: fromClass, ToClass: toClass,
		FromTotal: fromTotal, ToTotal: toTotal,
		NewClass: fromClass == 0,
		HitDie:   class.HitDie,
		XPBefore: s.XP,
		Mode:     in.Mode,
	}

	// Features: the names the new class level grants.
	d.NewFeatures = progression.FeaturesAt(className, toClass)

	// Saves: only a first level in a class grants its save proficiencies.
	if d.NewClass {
		d.NewSaves = append([]string(nil), class.Saves...)
		sort.Strings(d.NewSaves)
	}

	// Hit points: the 2014 fixed average (die/2 + 1) plus the CON
	// modifier. An undeclared CON proposes +0 and says so — the number is
	// on the diff to be seen and the gate's modify exists to fix it.
	conMod := 0
	if s.Abilities.CON > 0 {
		conMod = (s.Abilities.CON - 10) / 2
		d.ConDeclared = true
	}
	d.ConMod = conMod
	d.HPFrom = s.MaxHP
	d.HPGain = progression.HitPointsGained(class.HitDie, conMod)
	d.HPTotal = s.MaxHP + d.HPGain

	// Slots. The single-class case reads the class's own table: every
	// slot level whose maximum rises appears in the diff. Multiclass
	// tables are combined per the 2014 rounding rules the sheet's single
	// slot table cannot carry (a warlock's pact slots beside the combined
	// table above all) — so a multiclass diff proposes the class's
	// numbers and names the table question for the reviewer instead of
	// computing a wrong total.
	switch {
	case len(s.Classes) > 1:
		d.SlotsNote = "multiclass slot tables combine by the 2014 rules; review the sheet's slot maxima by hand"
	case class.Casting == progression.CastingPact:
		count, lvl := progression.PactSlotsAt(className, toClass)
		if count > 0 {
			from := slotMax(s, lvl)
			if count > from {
				d.Slots = append(d.Slots, SlotGain{Level: lvl, From: from, To: count})
			}
		}
		d.SlotsNote = fmt.Sprintf("pact magic: %d slot(s) casting at %dth level, back on a short rest", count, lvl)
	case class.Casting == progression.CastingFull || class.Casting == progression.CastingHalf:
		table := progression.SlotsAt(className, toClass)
		for lvl, n := range table {
			from := slotMax(s, lvl)
			if n > from {
				d.Slots = append(d.Slots, SlotGain{Level: lvl, From: from, To: n})
			}
		}
		sort.Slice(d.Slots, func(i, j int) bool { return d.Slots[i].Level < d.Slots[j].Level })
	case class.Casting == progression.CastingNone:
		// No slots from this class; a hand-registered table is the DM's.
	}

	// The advancement numbers, stamped so the review item shows what the
	// xp gate would ask for (milestone mode proposes the same numbers).
	if toTotal < progression.MaxLevel {
		if next, err := progression.XPForLevel(toTotal + 1); err == nil {
			d.XPForNext = next
		}
	}

	if !d.ConDeclared {
		if d.SlotsNote != "" {
			d.SlotsNote += "; "
		}
		d.SlotsNote += "constitution undeclared: the HP gain assumes a +0 modifier"
	}
	return d, nil
}

// slotMax reads the sheet's declared maximum for one slot level (0 when
// undeclared).
func slotMax(s sheet.Sheet, level int) int {
	if s.Spellcasting == nil {
		return 0
	}
	return s.Spellcasting.Slots[fmt.Sprintf("%d", level)]
}

// ApplyDiff writes the diff into a copy of the sheet — the finalizer's
// one write, after a human decision. Idempotent by construction: a sheet
// already at the target class level comes back unchanged.
func ApplyDiff(s sheet.Sheet, d Diff) sheet.Sheet {
	className, err := progression.Canonical(d.Class)
	if err != nil {
		return s
	}
	// Already there: the guard that makes a re-run a no-op.
	for _, c := range s.Classes {
		if strings.EqualFold(strings.TrimSpace(c.Class), className) && c.Level >= d.ToClass {
			return s
		}
	}
	applied := false
	classes := make([]sheet.ClassLevel, 0, len(s.Classes)+1)
	for _, c := range s.Classes {
		if !applied && strings.EqualFold(strings.TrimSpace(c.Class), className) {
			c.Level = d.ToClass
			if d.Subclass != "" && c.Subclass == "" {
				c.Subclass = d.Subclass
			}
			applied = true
		}
		classes = append(classes, c)
	}
	if !applied {
		classes = append(classes, sheet.ClassLevel{Class: className, Subclass: d.Subclass, Level: d.ToClass})
	}
	s.Classes = classes

	if len(d.NewFeatures) > 0 {
		existing := map[string]bool{}
		for _, e := range s.Features {
			existing[strings.ToLower(e.Name)] = true
		}
		for _, name := range d.NewFeatures {
			if !existing[strings.ToLower(name)] {
				s.Features = append(s.Features, sheet.Entry{Name: name})
			}
		}
	}
	if len(d.NewSaves) > 0 {
		existing := map[string]bool{}
		for _, sv := range s.Proficiencies.Saves {
			existing[sv] = true
		}
		for _, sv := range d.NewSaves {
			if !existing[sv] {
				s.Proficiencies.Saves = append(s.Proficiencies.Saves, sv)
			}
		}
		sort.Strings(s.Proficiencies.Saves)
	}
	if len(d.Slots) > 0 {
		if s.Spellcasting == nil {
			s.Spellcasting = &sheet.Spellcasting{}
		}
		if s.Spellcasting.Slots == nil {
			s.Spellcasting.Slots = map[string]int{}
		}
		for _, g := range d.Slots {
			key := fmt.Sprintf("%d", g.Level)
			if s.Spellcasting.Slots[key] < g.To {
				s.Spellcasting.Slots[key] = g.To
			}
		}
	}
	if d.HPGain > 0 && d.HPTotal > s.MaxHP {
		s.MaxHP = d.HPTotal
	}
	return s
}

// Summary renders the diff in the campaign's own vocabulary — the review
// item's one-line headline: concrete, no numbers invented.
func Summary(d Diff) string {
	var parts []string
	if d.NewClass {
		parts = append(parts, fmt.Sprintf("takes a first level in %s", d.Class))
	} else {
		parts = append(parts, fmt.Sprintf("%s goes from level %d to %d", d.Class, d.FromClass, d.ToClass))
	}
	parts = append(parts, fmt.Sprintf("total level %d", d.ToTotal))
	parts = append(parts, fmt.Sprintf("max hp %d -> %d", d.HPFrom, d.HPTotal))
	if len(d.Slots) > 0 {
		gains := make([]string, 0, len(d.Slots))
		for _, g := range d.Slots {
			gains = append(gains, fmt.Sprintf("%s slots %d -> %d", ordinal(g.Level), g.From, g.To))
		}
		parts = append(parts, strings.Join(gains, ", "))
	}
	if len(d.NewFeatures) > 0 {
		parts = append(parts, fmt.Sprintf("gains %s", strings.Join(d.NewFeatures, ", ")))
	}
	return strings.Join(parts, "; ")
}

// ordinal renders a slot level's ordinal: 1st, 2nd, 3rd, 4th...
func ordinal(lvl int) string {
	switch lvl {
	case 1:
		return "1st"
	case 2:
		return "2nd"
	case 3:
		return "3rd"
	default:
		return fmt.Sprintf("%dth", lvl)
	}
}

// EligibleXP reports whether the sheet's XP carries the level-up the diff
// proposes, per the advancement table. Milestone campaigns do not ask.
func EligibleXP(s sheet.Sheet, d Diff) bool {
	need, err := progression.XPForLevel(d.ToTotal)
	if err != nil {
		return false
	}
	return s.XP >= need
}
