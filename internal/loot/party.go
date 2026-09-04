package loot

// The three reads a DM actually wants (MAD-384): the distribution — who
// has received what, over time; the power curve — the party's items
// against the game's expectation for its tier; and the concentration
// warning — "this item complements the fighter too strongly".
//
// Everything here is pure. The database fold that feeds it lives in the
// server layer; this package owns what the fold means: how holdings are
// dated, what counts as comparable, and where the expectation numbers
// come from. Identical input gives identical output.

import (
	"fmt"
	"sort"
	"strings"
)

/* ---------- inputs ---------- */

// HoldingSources name where a holding is known from, in the fold's own
// vocabulary. A pc's party block, a possession fact, or a dated
// relationship.
const (
	SourcePartyBlock = "party block"
	SourceFact       = "possession fact"
	SourceRelation   = "relationship"
)

// Holding is one item one pc holds, as the fold found it. Ordinal is the
// event-log play position the holding is dated at — a relationship's
// since_event, or the event position of the session a possession fact was
// extracted from. Nil means undated: the party block declares the item
// but the log never dated it, and the view says so rather than guessing.
type Holding struct {
	ItemID   string
	ItemName string
	Rarity   string   // "" when the catalog cannot classify the entity
	Tags     []string // the catalog's derived tags, when classified
	Source   string   // one of the Source* constants
	Ordinal  *int64   // play position in the event log, when dated
	Session  *int64   // session ordinal, when the provenance names one
	// Statement is the possession fact's own words, for fact-sourced rows.
	Statement string
}

// PC is one pc as the reads see them: the party block's mechanical half
// (level, class) plus the fold's holdings.
type PC struct {
	EntityID string
	Name     string
	Level    int    // 0 when the block declares none
	Class    string // "" when undeclared
	Items    []Holding
}

/* ---------- the distribution ---------- */

// Distribution is who has received what, over time, in one view. Dated
// holdings run in event-log order; undated ones follow, visibly
// undated — most "the rogue never gets anything" problems are invisible
// until they are plotted, and this is the plot.
type Distribution struct {
	PCs   []PCDistribution `json:"pcs"`
	Notes []string         `json:"notes,omitempty"`
}

// PCDistribution is one pc's column of the plot.
type PCDistribution struct {
	EntityID string          `json:"entity_id"`
	Name     string          `json:"name"`
	Total    int             `json:"total"`
	SharePct int             `json:"share_pct"`
	Timeline []TimelineEntry `json:"timeline"`
}

// TimelineEntry is one holding in the fold's order.
type TimelineEntry struct {
	ItemName string `json:"item_name"`
	Source   string `json:"source"`
	// Ordinal is the event-log play position, when the log dated it.
	Ordinal *int64 `json:"ordinal,omitempty"`
	// Session is the session ordinal, when the provenance names one.
	Session *int64 `json:"session,omitempty"`
	// Dated reports whether the log dates this holding. Undated rows sort
	// last and read as declared rather than received.
	Dated     bool   `json:"dated"`
	Statement string `json:"statement,omitempty"`
}

// DistributionOf folds the party's holdings into the distribution view.
// The ordering rule is the event log's: dated rows by play ordinal, then
// session order, then name; undated rows last. Shares are of the party's
// total holdings, so "the rogue has 4%" is one line of arithmetic, not a
// vibe.
func DistributionOf(pcs []PC) *Distribution {
	d := &Distribution{}
	total := 0
	for _, pc := range pcs {
		total += len(pc.Items)
	}
	for _, pc := range pcs {
		pcd := PCDistribution{EntityID: pc.EntityID, Name: pc.Name, Total: len(pc.Items)}
		if total > 0 {
			pcd.SharePct = 100 * len(pc.Items) / total
		}
		entries := make([]TimelineEntry, 0, len(pc.Items))
		for _, h := range pc.Items {
			entries = append(entries, TimelineEntry{
				ItemName: h.ItemName, Source: h.Source,
				Ordinal: h.Ordinal, Session: h.Session,
				Dated:     h.Ordinal != nil,
				Statement: h.Statement,
			})
		}
		sort.SliceStable(entries, func(i, j int) bool {
			a, b := entries[i], entries[j]
			switch {
			case a.Dated != b.Dated:
				return a.Dated // dated first, undated last
			case a.Dated && b.Dated && *a.Ordinal != *b.Ordinal:
				return *a.Ordinal < *b.Ordinal
			case a.Session != nil && b.Session != nil && *a.Session != *b.Session:
				return *a.Session < *b.Session
			default:
				return a.ItemName < b.ItemName
			}
		})
		pcd.Timeline = entries
		d.PCs = append(d.PCs, pcd)
	}
	sort.SliceStable(d.PCs, func(i, j int) bool { return d.PCs[i].Name < d.PCs[j].Name })
	// The party-level note names the pc or pcs who have received nothing —
	// the plot's whole point, stated once rather than buried in rows.
	var empty []string
	for _, pcd := range d.PCs {
		if pcd.Total == 0 {
			empty = append(empty, pcd.Name)
		}
	}
	switch {
	case len(empty) == 1:
		d.Notes = append(d.Notes, empty[0]+" has received nothing")
	case len(empty) > 1:
		d.Notes = append(d.Notes, strings.Join(empty, ", ")+" have received nothing")
	}
	return d
}

/* ---------- the power curve ---------- */

// RarityOrder is the game's rarity ladder, in display order. The power
// curve's expectation is keyed by it.
var RarityOrder = []string{"Common", "Uncommon", "Rare", "Very Rare", "Legendary"}

// expectedByTier is the game's expectation for a party's accumulated
// items, cumulative at each tier's end, by rarity. Source: Xanathar's
// Guide to Everything p.135, "Magic Items Awarded by Tier" and its two
// "Magic Items Awarded by Rarity" tables — the minor and major tables
// merged here by rarity, since a holding's rarity is what the catalog
// classifies and minor/major is table membership the corpus does not
// carry. The tables speak of a party: the numbers are the party's, not a
// per-character quota.
var expectedByTier = map[string][4]int{
	"Common":    {6, 16, 19, 19},
	"Uncommon":  {4, 21, 28, 28},
	"Rare":      {1, 7, 18, 23},
	"Very Rare": {0, 1, 8, 19},
	"Legendary": {0, 0, 2, 11},
}

// expectedTotal is the merged tables' all-items total per tier, cumulative.
var expectedTotal = [4]int{11, 45, 75, 100}

// PowerCurve is the party's items against the game's expectation for its
// tier — over-equipped, under-equipped, or on the line — with the
// arithmetic that produced the verdict, never the verdict alone.
type PowerCurve struct {
	Computable bool   `json:"computable"`
	Reason     string `json:"reason,omitempty"` // why not, when not

	Tier      Tier   `json:"tier,omitempty"`
	TierLabel string `json:"tier_label,omitempty"`
	PartySize int    `json:"party_size,omitempty"`
	AvgLevel  int    `json:"avg_level,omitempty"`

	Held         map[string]int `json:"held"` // rarity → count, classified only
	Unclassified int            `json:"unclassified"`
	HeldTotal    int            `json:"held_total"`

	// Expectation and Floor are cumulative per-rarity counts at this
	// tier's end and the previous tier's end. Floor is nil at tier 1.
	Expectation map[string]int `json:"expectation"`
	Floor       map[string]int `json:"floor,omitempty"`
	// ExpectationTotal and FloorTotal are the all-items numbers the
	// verdict compares.
	ExpectationTotal int `json:"expectation_total"`
	FloorTotal       int `json:"floor_total"`

	Arithmetic []string `json:"arithmetic"`
	Verdict    string   `json:"verdict"`
}

// PowerCurveOf reads the party's curve. Levels name the tier; no usable
// level means no read, and Reason says why rather than the surface
// inventing a band.
func PowerCurveOf(pcs []PC) *PowerCurve {
	pc := &PowerCurve{Held: map[string]int{}, Expectation: map[string]int{}}
	var levels []int
	for _, p := range pcs {
		for _, h := range p.Items {
			pc.HeldTotal++
			if h.Rarity == "" {
				pc.Unclassified++
				continue
			}
			pc.Held[h.Rarity]++
		}
		if p.Level >= 1 && p.Level <= 20 {
			levels = append(levels, p.Level)
		}
	}
	pc.PartySize = len(levels)
	tier, ok := TierForLevels(levels)
	if !ok {
		pc.Computable = false
		pc.Reason = "no pc declares a usable level — the curve needs the party's tier"
		pc.Verdict = "not computable"
		return pc
	}
	pc.Computable = true
	pc.Tier = tier
	pc.TierLabel = tier.Label()
	sum := 0
	for _, l := range levels {
		sum += l
	}
	pc.AvgLevel = sum / len(levels)
	for _, r := range RarityOrder {
		pc.Expectation[r] = expectedByTier[r][tier-1]
	}
	pc.ExpectationTotal = expectedTotal[tier-1]
	if tier > Tier1 {
		pc.Floor = map[string]int{}
		for _, r := range RarityOrder {
			pc.Floor[r] = expectedByTier[r][tier-2]
		}
		pc.FloorTotal = expectedTotal[tier-2]
	}

	/* ---------- the arithmetic, written out ---------- */
	pc.Arithmetic = append(pc.Arithmetic, fmt.Sprintf(
		"%d pcs, declared levels averaging %d — tier %d (%s)", len(levels), pc.AvgLevel, tier, pc.TierLabel))
	var parts []string
	for _, r := range RarityOrder {
		if pc.Held[r] > 0 || pc.Expectation[r] > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", r, pc.Held[r]))
		}
	}
	heldLine := fmt.Sprintf("held: %d", pc.HeldTotal)
	if len(parts) > 0 {
		heldLine += " (" + strings.Join(parts, ", ") + ")"
	}
	if pc.Unclassified > 0 {
		heldLine += fmt.Sprintf(", plus %d the catalog cannot classify", pc.Unclassified)
	}
	pc.Arithmetic = append(pc.Arithmetic, heldLine)
	floorWord := "0"
	if tier > Tier1 {
		floorWord = comma(pc.FloorTotal)
	}
	pc.Arithmetic = append(pc.Arithmetic, fmt.Sprintf(
		"expectation at this tier's end: %s items (XGE p.135, minor and major merged) — %s at the previous tier's end",
		comma(pc.ExpectationTotal), floorWord))
	pc.Verdict = pc.curveVerdict()
	pc.Arithmetic = append(pc.Arithmetic, "verdict: "+pc.Verdict)
	return pc
}

// curveVerdict compares the party's total against the band the
// expectation defines: below the previous tier's end is behind even the
// band's floor; between floor and expectation is under for the tier; at
// or past expectation is on the line or over. The words carry the
// numbers.
func (p *PowerCurve) curveVerdict() string {
	switch {
	case p.HeldTotal < p.FloorTotal:
		return fmt.Sprintf("under-equipped: %d items is behind even the %d the party would have gathered by where this tier began",
			p.HeldTotal, p.FloorTotal)
	case p.HeldTotal < p.ExpectationTotal:
		pct := 0
		if p.ExpectationTotal > 0 {
			pct = 100 * p.HeldTotal / p.ExpectationTotal
		}
		return fmt.Sprintf("under-equipped: %d of the %d items the party would have gathered by this tier's end (%d%%)",
			p.HeldTotal, p.ExpectationTotal, pct)
	case p.HeldTotal == p.ExpectationTotal:
		return fmt.Sprintf("on the line: %d items, exactly the expectation for this tier's end", p.HeldTotal)
	default:
		over := 0
		if p.ExpectationTotal > 0 {
			over = 100 * (p.HeldTotal - p.ExpectationTotal) / p.ExpectationTotal
		}
		return fmt.Sprintf("over-equipped: %d items against an expectation of %d — %d%% over",
			p.HeldTotal, p.ExpectationTotal, over)
	}
}

/* ---------- the concentration warning ---------- */

// ConcentrationWarning is one "this item complements that pc too
// strongly" — a suggestion with its reasoning attached, never a refusal.
// The DM overrules it and the tool says nothing further.
type ConcentrationWarning struct {
	Item   string `json:"item"`
	PC     string `json:"pc"`
	Axis   string `json:"axis"`
	Reason string `json:"reason"`
}

// Concentrations evaluates one candidate item against the party: the
// warning fires when the candidate's tag axis is one a pc is already the
// party's strict strongest on, while some other pc has received nothing
// comparable — no item at all, or nothing sharing any of the candidate's
// tags. An even party is silent; a tie at the top is not dominance; a
// party that declares nothing is silent. It is arithmetic the DM can
// argue with, which is the point.
func Concentrations(itemName string, tags []string, pcs []PC) []ConcentrationWarning {
	var out []ConcentrationWarning
	seen := map[string]bool{}
	for _, axis := range tags {
		counts := make([]int, len(pcs))
		maxCount := 0
		maxAt := -1
		tie := false
		for i, pc := range pcs {
			n := 0
			for _, h := range pc.Items {
				for _, t := range h.Tags {
					if strings.EqualFold(t, axis) {
						n++
						break
					}
				}
			}
			counts[i] = n
			switch {
			case n > maxCount:
				maxCount, maxAt, tie = n, i, false
			case n == maxCount && n > 0:
				tie = true
			}
		}
		if maxAt < 0 || tie || maxCount == 0 {
			continue
		}
		// Another pc with nothing comparable: no holdings at all, or none
		// sharing any of the candidate's tags.
		nothing := ""
		for i, pc := range pcs {
			if i == maxAt {
				continue
			}
			comparable := false
			for _, h := range pc.Items {
				for _, t := range h.Tags {
					for _, ct := range tags {
						if strings.EqualFold(t, ct) {
							comparable = true
							break
						}
					}
				}
			}
			if !comparable {
				nothing = pc.Name
				break
			}
		}
		if nothing == "" {
			continue
		}
		axisTotal := 0
		for _, n := range counts {
			axisTotal += n
		}
		key := pcs[maxAt].Name + "|" + strings.ToLower(axis)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, ConcentrationWarning{
			Item: itemName, PC: pcs[maxAt].Name, Axis: axis,
			Reason: fmt.Sprintf("%s already holds %d of the party's %d %s items and %s has received nothing comparable — consider replacing this item or reassigning it",
				pcs[maxAt].Name, maxCount, axisTotal, axis, nothing),
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Axis < out[j].Axis })
	return out
}
