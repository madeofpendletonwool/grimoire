// Package stats is the campaign endcap (MAD-428, stage 11 of MAD-417):
// the table's numbers, derived — never stored.
//
// The mechanical layer's every stage writes append-only logs: dice_rolls
// (MAD-420), combat_log (MAD-422), resource_transactions (MAD-419).
// Stats fold those logs the way the ledger folds transactions and the
// replay folds the battle journal — same rows in, same numbers out,
// forever. Nothing here writes, caches, or invents; the fun-first
// rendering (party ratios, "how lucky was this session") is a view over
// the fold, not a second truth.
package stats

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

/* ---------- the inputs (one per folded row) ---------- */

// RollInput is one dice roll as the fold reads it. D20s carries the
// kept d20 values — the natural dice that decided the roll, drops
// included-in-record but excluded-from-luck.
type RollInput struct {
	CharacterID   string
	CharacterName string
	TargetID      string
	TargetName    string
	Context       string
	Mode          string
	Secret        bool
	D20s          []int
}

// DamageInput is one applied hit from a battle journal: who took it,
// who dealt it (empty when the table did not say), and the effective
// amount after resistance and immunity — the number the tracker's own
// rows recorded.
type DamageInput struct {
	TargetEntity string // the pc's entity id, when the target was a pc
	TargetName   string
	TargetSide   string // party | foe
	TargetKind   string // pc | monster | companion
	SourceEntity string // the attacker's entity id, empty when unattributed
	SourceName   string
	Amount       int
}

// Inputs is one fold's raw material: every roll, every hit, and the
// count of inspiration spends. The store builds it; the fold is pure.
type Inputs struct {
	Rolls    []RollInput
	Damage   []DamageInput
	Inspired int // inspiration spends (the roll flow's ledger rows)
}

/* ---------- the outputs ---------- */

// RollStats is the dice half: how many, how lucky, how many crits.
// Luck reads the kept d20s against the fair 10.5 — a full house claim
// only when the sample is big enough to make one (at least ten dice).
type RollStats struct {
	Total     int            `json:"total"`
	ByContext map[string]int `json:"by_context"`
	D20s      int            `json:"d20s"`
	MeanD20   float64        `json:"mean_d20"`
	Buckets   []int          `json:"buckets"` // 1-5, 6-10, 11-15, 16-20
	Crits     int            `json:"crits"`
	Natural1s int            `json:"natural_1s"`
	Luck      string         `json:"luck,omitempty"`
}

// CharacterStat is one pc's line in the party shot: the damage that
// reached them, the damage they dealt, and their share of what the
// party took — a party ratio, not a brag board.
type CharacterStat struct {
	ID    string  `json:"id"`
	Name  string  `json:"name"`
	Taken int     `json:"taken"`
	Dealt int     `json:"dealt"`
	Share float64 `json:"share"` // percent of party damage taken, one decimal
}

// TargetStat is one enemy the party kept aiming at.
type TargetStat struct {
	Name  string `json:"name"`
	Rolls int    `json:"rolls"`
}

// Stats is one fold's answer: the dice, the party, the enemies, the
// inspiration. Empty slices stay nil-free on the wire the way the board
// keeps its shapes — ordered by construction, deterministic in bytes.
type Stats struct {
	Rolls             RollStats       `json:"rolls"`
	Characters        []CharacterStat `json:"characters"`
	PartyDamageTaken  int             `json:"party_damage_taken"`
	PartyDamageDealt  int             `json:"party_damage_dealt"`
	Targets           []TargetStat    `json:"targets"`
	InspirationSpends int             `json:"inspiration_spends"`
}

/* ---------- the fold ---------- */

// luckThreshold is how far from 10.5 the mean must sit before the fold
// calls the dice hot or cold; luckMinimumDice is how many kept d20s a
// claim needs. A session of four dice proves nothing and says nothing.
const (
	luckThreshold   = 1.0
	luckMinimumDice = 10
)

// Fold derives the stats from one set of inputs. Pure: no clock, no
// database, no map-order escapes — identical inputs produce identical
// Stats, and identical Stats produce identical JSON, which is the
// acceptance the golden files pin.
func Fold(in Inputs) *Stats {
	st := &Stats{Rolls: RollStats{ByContext: map[string]int{}}}

	// The dice.
	var d20sum int
	targets := map[string]*TargetStat{}
	for _, r := range in.Rolls {
		st.Rolls.Total++
		st.Rolls.ByContext[r.Context]++
		for _, v := range r.D20s {
			st.Rolls.D20s++
			d20sum += v
			switch {
			case v <= 5:
				st.Rolls.Buckets = bucketAdd(st.Rolls.Buckets, 0)
			case v <= 10:
				st.Rolls.Buckets = bucketAdd(st.Rolls.Buckets, 1)
			case v <= 15:
				st.Rolls.Buckets = bucketAdd(st.Rolls.Buckets, 2)
			default:
				st.Rolls.Buckets = bucketAdd(st.Rolls.Buckets, 3)
			}
		}
		if r.Context == "attack" {
			for _, v := range r.D20s {
				if v == 20 {
					st.Rolls.Crits++
					break
				}
			}
			for _, v := range r.D20s {
				if v == 1 {
					st.Rolls.Natural1s++
					break
				}
			}
			if r.TargetName != "" {
				t, ok := targets[r.TargetName]
				if !ok {
					t = &TargetStat{Name: r.TargetName}
					targets[r.TargetName] = t
				}
				t.Rolls++
			}
		}
	}
	if st.Rolls.D20s > 0 {
		st.Rolls.MeanD20 = round1(float64(d20sum) / float64(st.Rolls.D20s))
	}
	if st.Rolls.D20s >= luckMinimumDice {
		switch d := st.Rolls.MeanD20 - 10.5; {
		case d >= luckThreshold:
			st.Rolls.Luck = "hot"
		case d <= -luckThreshold:
			st.Rolls.Luck = "cold"
		default:
			st.Rolls.Luck = "true"
		}
	}

	// The party. Damage taken counts every hit that reached a pc;
	// damage dealt counts the hits a pc's own hand is journal-attributed
	// to — attribution the table does not name stays unnamed.
	byID := map[string]*CharacterStat{}
	var order []string
	for _, d := range in.Damage {
		if d.TargetKind == "pc" && d.TargetEntity != "" {
			c, ok := byID[d.TargetEntity]
			if !ok {
				c = &CharacterStat{ID: d.TargetEntity, Name: d.TargetName}
				byID[d.TargetEntity] = c
				order = append(order, d.TargetEntity)
			}
			c.Taken += d.Amount
			st.PartyDamageTaken += d.Amount
		}
		if d.SourceEntity != "" {
			c, ok := byID[d.SourceEntity]
			if !ok {
				c = &CharacterStat{ID: d.SourceEntity, Name: d.SourceName}
				byID[d.SourceEntity] = c
				order = append(order, d.SourceEntity)
			}
			c.Dealt += d.Amount
			st.PartyDamageDealt += d.Amount
		}
	}
	sort.Slice(order, func(i, j int) bool { return byID[order[i]].Name < byID[order[j]].Name })
	for _, id := range order {
		c := byID[id]
		if st.PartyDamageTaken > 0 {
			c.Share = round1(100 * float64(c.Taken) / float64(st.PartyDamageTaken))
		}
		st.Characters = append(st.Characters, *c)
	}

	// The enemies: most-targeted first, ties by name — deterministic.
	for _, t := range targets {
		st.Targets = append(st.Targets, *t)
	}
	sort.Slice(st.Targets, func(i, j int) bool {
		if st.Targets[i].Rolls != st.Targets[j].Rolls {
			return st.Targets[i].Rolls > st.Targets[j].Rolls
		}
		return st.Targets[i].Name < st.Targets[j].Name
	})
	if len(st.Targets) > 3 {
		st.Targets = st.Targets[:3]
	}

	st.InspirationSpends = in.Inspired
	return st
}

// Empty reports whether the fold found anything worth saying — a
// session with no rolls and no fights carries no numbers section.
func (st *Stats) Empty() bool {
	return st.Rolls.Total == 0 && st.PartyDamageTaken == 0 && st.PartyDamageDealt == 0 && st.InspirationSpends == 0
}

// bucketAdd grows the four-bucket histogram in place.
func bucketAdd(buckets []int, i int) []int {
	if len(buckets) < 4 {
		buckets = append(buckets, 0, 0, 0, 0)
	}
	buckets[i]++
	return buckets
}

// round1 rounds to one decimal — the precision a party shot keeps.
func round1(v float64) float64 {
	return math.Round(v*10) / 10
}

/* ---------- the rendering ---------- */

// Markdown renders the stats as the export's "The numbers" section —
// fun-first: the dice's temperature, the party's ratios, the enemies
// the party would not leave alone. Empty stats render "".
func (st *Stats) Markdown() string {
	if st.Empty() {
		return ""
	}
	var b strings.Builder
	b.WriteString("## The numbers\n\n")

	var dice []string
	if st.Rolls.Total > 0 {
		line := fmt.Sprintf("%d rolls", st.Rolls.Total)
		if st.Rolls.D20s > 0 {
			line += fmt.Sprintf(" · %d d20s averaging %.1f", st.Rolls.D20s, st.Rolls.MeanD20)
			switch st.Rolls.Luck {
			case "hot":
				line += " — the dice ran hot"
			case "cold":
				line += " — the dice ran cold"
			case "true":
				line += " — the dice ran true"
			}
		}
		dice = append(dice, line)
	}
	if st.Rolls.Crits > 0 {
		dice = append(dice, fmt.Sprintf("%d crit%s", st.Rolls.Crits, plural(st.Rolls.Crits)))
	}
	if st.Rolls.Natural1s > 0 {
		dice = append(dice, fmt.Sprintf("%d natural 1%s", st.Rolls.Natural1s, plural(st.Rolls.Natural1s)))
	}
	if st.InspirationSpends > 0 {
		dice = append(dice, fmt.Sprintf("%d inspiration spent", st.InspirationSpends))
	}
	if len(dice) > 0 {
		fmt.Fprintf(&b, "_%s_\n\n", strings.Join(dice, " · "))
	}

	if len(st.Characters) > 0 && st.PartyDamageTaken > 0 {
		b.WriteString("The party took ")
		fmt.Fprintf(&b, "%d damage:\n\n", st.PartyDamageTaken)
		for _, c := range st.Characters {
			fmt.Fprintf(&b, "- **%s** took %.0f%% of it (%d) and dealt %d\n",
				c.Name, c.Share, c.Taken, c.Dealt)
		}
		b.WriteString("\n")
	} else if len(st.Characters) > 0 {
		for _, c := range st.Characters {
			fmt.Fprintf(&b, "- **%s** dealt %d\n", c.Name, c.Dealt)
		}
		b.WriteString("\n")
	}

	if len(st.Targets) > 0 {
		var parts []string
		for _, t := range st.Targets {
			parts = append(parts, fmt.Sprintf("%s (%d roll%s)", t.Name, t.Rolls, plural(t.Rolls)))
		}
		fmt.Fprintf(&b, "**Most targeted:** %s\n\n", strings.Join(parts, ", "))
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
