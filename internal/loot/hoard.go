package loot

// Generating a hoard (MAD-384). Size, tier and context in; a hoard out —
// coin from the DMG tables, items chosen from the catalog against the
// party read, and mundane treasure as the row prints it. Each item comes
// with why it was chosen, and any item can be rerolled individually
// without rerolling the hoard.
//
// The mechanism that makes that last part true: a hoard is a pure
// function of (request, catalog, seed), and every line's dice run on a
// random source derived from (seed, line key). Rerolling a line derives
// that one line's source from (seed, line key, nonce) instead — every
// other line, and the coin and d100 rolls, cannot move. Nothing is
// stored; the server holds no state between generate and reroll.

import (
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/items"
)

// ErrNoTier is the one way a hoard ask fails: no tier given and no level
// to derive one from.
var ErrNoTier = errors.New("loot: no tier and no declared level to derive one from")

/* ---------- the request ---------- */

// ActContext is the narrative weight a hoard can carry when there is a
// spine (MAD-360): a hoard at the end of an act can carry the act's item
// rather than a random one. Item is what the spine names; nil means the
// act names no item and the hoard says so.
type ActContext struct {
	Name string // the act's name, for the reason text
	Item *NarrativeItem
}

// NarrativeItem is an item the campaign's spine already names — an item
// entity tied to the act's quests or scenes. The hoard includes it as a
// line but never generates it: it exists, and placement must not
// duplicate it.
type NarrativeItem struct {
	EntityID string
	Name     string
	Summary  string
	Where    string // "the quest <name>" or "the scene <name>"
}

// Request is one hoard ask. Tier is required — the caller derives it from
// the party when it can and demands it when it cannot. Levels and Party
// are the party read; both empty is the degraded, tier-only mode, which
// still generates and says so.
type Request struct {
	Tier   Tier
	Levels []int // declared levels, when known
	Party  []PC  // the fold's holdings, when known
	Act    *ActContext
}

/* ---------- the hoard ---------- */

// Hoard is one generated hoard. Nothing here is written anywhere;
// placing it stages a proposal batch, and nothing is written until the DM
// approves that.
type Hoard struct {
	Seed      int64    `json:"seed"`
	Tier      Tier     `json:"tier"`
	TierLabel string   `json:"tier_label"`
	Degraded  bool     `json:"degraded"`
	Notes     []string `json:"notes,omitempty"`

	// Row is the d100 roll that shaped the hoard, shown as the book's
	// arithmetic rather than hidden: "d100 47 — 1d4 rolls on Table C".
	Row string `json:"row"`

	Coins     []CoinLine     `json:"coins"`
	Mundane   []MundaneLine  `json:"mundane"`
	Items     []HoardItem    `json:"items"`
	Narrative *NarrativeLine `json:"narrative,omitempty"`

	// ValueGP is the coin-and-mundane value in gold pieces. Magic items
	// are deliberately unpriced — the DMG gives no prices and this
	// surface is not a gold-piece economy.
	ValueGP int  `json:"value_gp"`
	Band    Band `json:"band"`
}

// CoinLine is one rolled row of the coin block.
type CoinLine struct {
	Coin       string `json:"coin"`
	Expression string `json:"expression"`
	Amount     int    `json:"amount"`
}

// MundaneLine is one rolled bundle of gems or art objects.
type MundaneLine struct {
	Description string `json:"description"`
	Count       int    `json:"count"`
	UnitValueGP int    `json:"unit_value_gp"`
	ValueGP     int    `json:"value_gp"`
}

// HoardItem is one magic item in the hoard, with why it was chosen.
type HoardItem struct {
	Key      string   `json:"key"` // stable line key: "item-1"
	Slug     string   `json:"slug,omitempty"`
	Name     string   `json:"name"`
	Doc      string   `json:"doc,omitempty"`
	Rarity   string   `json:"rarity,omitempty"`
	Type     string   `json:"type,omitempty"`
	Tags     []string `json:"tags,omitempty"`
	Homebrew bool     `json:"homebrew,omitempty"`
	Reason   string   `json:"reason"`

	Warnings []ConcentrationWarning `json:"warnings,omitempty"`
}

// NarrativeLine is the act's item slot: included when the spine names an
// item, absent-with-a-reason when it does not.
type NarrativeLine struct {
	Act      string         `json:"act"`
	Included bool           `json:"included"`
	Item     *NarrativeItem `json:"item,omitempty"`
	Reason   string         `json:"reason"`
}

/* ---------- generation ---------- */

// GenerateHoard rolls one hoard from the seed. Identical inputs give
// identical hoards, across restarts and processes.
func GenerateHoard(req Request, cat []items.Item, seed int64) (*Hoard, error) {
	return buildHoard(req, cat, seed, nil)
}

// RegenerateHoard is GenerateHoard with named lines rerolled: each entry
// maps a line key to a fresh nonce, and only those lines' dice move.
func RegenerateHoard(req Request, cat []items.Item, seed int64, rerolls map[string]int64) (*Hoard, error) {
	return buildHoard(req, cat, seed, rerolls)
}

// lineSource derives one line's random source from the hoard seed, the
// line's key and, when present, its reroll nonce.
func lineSource(seed int64, key string, rerolls map[string]int64) *rand.Rand {
	h := fnv.New64a()
	fmt.Fprintf(h, "%d|%s", seed, key)
	if n, ok := rerolls[key]; ok {
		fmt.Fprintf(h, "|reroll:%d", n)
	}
	return rand.New(rand.NewSource(int64(h.Sum64())))
}

func buildHoard(req Request, cat []items.Item, seed int64, rerolls map[string]int64) (*Hoard, error) {
	tier := req.Tier
	if tier < Tier1 || tier > Tier4 {
		derived, ok := TierForLevels(req.Levels)
		if !ok {
			return nil, ErrNoTier
		}
		tier = derived
	}
	h := &Hoard{
		Seed: seed, Tier: tier, TierLabel: tier.Label(),
		Band: TierBand(tier),
	}

	/* ---------- the party read ---------- */
	party := req.Party
	if _, derivable := TierForLevels(req.Levels); !derivable && len(req.Levels) == 0 {
		h.Degraded = true
		h.Notes = append(h.Notes,
			"no pc declares a level — the hoard is tier-only; no party read was applied")
		party = nil
	} else if !anyHoldings(party) {
		h.Notes = append(h.Notes,
			"no pc declares holdings yet — the party read has nothing to compare against")
	}

	/* ---------- the d100 row ---------- */
	rowRng := lineSource(seed, "d100", nil)
	row, roll := pickRow(tier, rowRng)
	h.Row = fmt.Sprintf("d100 %d", roll)

	/* ---------- coins and mundane ---------- */
	treasure := lineSource(seed, "treasure", nil)
	valueCP := 0
	for _, c := range hoardCoins[tier] {
		if c.dice.N == 0 {
			continue
		}
		amount := c.dice.Roll(treasure)
		h.Coins = append(h.Coins, CoinLine{
			Coin: c.coin, Expression: c.dice.String(), Amount: amount,
		})
		valueCP += c.valueCP(amount)
	}
	if row.mundane.Kind != "" {
		count, v := row.mundane.Roll(treasure)
		h.Mundane = append(h.Mundane, MundaneLine{
			Description: row.mundane.String(),
			Count:       count,
			UnitValueGP: row.mundane.UnitValue,
			ValueGP:     v / 100,
		})
		valueCP += v
	}
	h.ValueGP = valueCP / 100

	/* ---------- the magic item slots ---------- */
	slot := 0
	for _, roll := range row.magic {
		count := roll.Count.Roll(treasure)
		for i := 0; i < count; i++ {
			slot++
			key := fmt.Sprintf("item-%d", slot)
			item, reason, ok := pickFromTable(cat, roll.Table, lineSource(seed, key, rerolls), party)
			if !ok {
				h.Notes = append(h.Notes, fmt.Sprintf(
					"table %s rolled an item the catalog cannot fill — the slot is empty rather than wrong", roll.Table))
				continue
			}
			line := HoardItem{
				Key: key, Slug: item.Slug, Name: item.Name, Doc: item.Doc,
				Rarity: item.Rarity, Type: item.Type, Tags: item.Tags,
				Homebrew: item.Homebrew, Reason: reason,
			}
			if !h.Degraded {
				line.Warnings = Concentrations(item.Name, item.Tags, party)
			}
			h.Items = append(h.Items, line)
		}
	}
	if len(row.magic) == 0 {
		h.Notes = append(h.Notes, "the d100 carried no magic items — coin and mundane treasure only")
	}

	/* ---------- the act's item ---------- */
	if req.Act != nil {
		nl := &NarrativeLine{Act: req.Act.Name}
		if req.Act.Item != nil {
			nl.Included = true
			nl.Item = req.Act.Item
			nl.Reason = fmt.Sprintf("carries the act's item — %s is named by %s, so the hoard offers it rather than a roll",
				req.Act.Item.Name, req.Act.Item.Where)
		} else {
			nl.Reason = fmt.Sprintf("the act \"%s\" names no item — the hoard is generated as rolled", req.Act.Name)
		}
		h.Narrative = nl
	}
	return h, nil
}

// anyHoldings reports whether any pc declares a single item.
func anyHoldings(party []PC) bool {
	for _, pc := range party {
		if len(pc.Items) > 0 {
			return true
		}
	}
	return false
}

/* ---------- the pick ---------- */

// pickFromTable chooses one item for one magic-item slot: the DMG table's
// rarity profile narrows the catalog, the party read — when there is one
// — chooses within it, and the reason records the whole chain. Rarity is
// never a party decision: the table's shape is the book's, and only the
// choice is campaign-aware.
func pickFromTable(cat []items.Item, table string, r *rand.Rand, party []PC) (items.Item, string, bool) {
	profile := profileFor(table)
	if len(profile) == 0 {
		return items.Item{}, "", false
	}
	// The book's tables lean toward the low end of their profile: a d100
	// taste of 70/30 when the profile names two rarities, applied before
	// the party read so the tier stays the book's.
	rarity := profile[0]
	if len(profile) > 1 && r.Float64() >= 0.7 {
		rarity = profile[1]
	}
	inRarity, inProfile := splitByRarity(cat, profile, rarity)
	pool := inRarity
	if len(pool) == 0 {
		pool = inProfile
	}
	if len(pool) == 0 {
		return items.Item{}, "", false
	}

	pick, why := chooseWithin(pool, party, r)
	reason := fmt.Sprintf("Magic Item Table %s rides %s; %s",
		table, strings.Join(profile, "-to-"), why)
	return pick, reason, true
}

// splitByRarity partitions the catalog into the profile's items and the
// preferred rarity's items, case-insensitively.
func splitByRarity(cat []items.Item, profile []string, preferred string) (pref, inProfile []items.Item) {
	want := map[string]bool{}
	for _, p := range profile {
		want[strings.ToLower(p)] = true
	}
	for _, it := range cat {
		if !want[strings.ToLower(it.Rarity)] {
			continue
		}
		inProfile = append(inProfile, it)
		if strings.EqualFold(it.Rarity, preferred) {
			pref = append(pref, it)
		}
	}
	return pref, inProfile
}

// chooseWithin picks one item from the pool. With a party read, items
// whose tags sit where the party is thinnest win — the spread between the
// party's widest and thinnest axis is what "complements the party"
// arithmetically means — with a random tie-break so the same party does
// not always draw the same sword. Without one, the pool rolls evenly.
func chooseWithin(pool []items.Item, party []PC, r *rand.Rand) (items.Item, string) {
	if len(party) == 0 || !anyHoldings(party) {
		pick := pool[r.Intn(len(pool))]
		return pick, fmt.Sprintf("%s chosen from %d %s items in the catalog",
			pick.Name, len(pool), displayRarity(pick.Rarity))
	}
	best := 0
	var tied []int
	for i, it := range pool {
		spread, _, _ := tagSpread(it, party)
		if spread > best {
			best = spread
			tied = []int{i}
		} else if spread == best {
			tied = append(tied, i)
		}
	}
	idx := tied[r.Intn(len(tied))]
	pick := pool[idx]
	spread, _, axis := tagSpread(pick, party)
	if spread == 0 || axis == "" {
		return pick, fmt.Sprintf("%s chosen from %d %s items in the catalog",
			pick.Name, len(pool), displayRarity(pick.Rarity))
	}
	thinnest := thinnestPC(axis, party)
	return pick, fmt.Sprintf("%s chosen from %d %s items — it sits with %s, who holds the least of the party's %s items",
		pick.Name, len(pool), displayRarity(pick.Rarity), thinnest, axis)
}

// tagSpread scores one candidate against the party: for the candidate's
// first tag the party declares any holding on, the spread between the
// party's strongest and thinnest pc on that axis. Returns the spread, the
// thinnest pc's name and the axis that produced it.
func tagSpread(it items.Item, party []PC) (int, string, string) {
	bestSpread := 0
	thinnest, bestAxis := "", ""
	for _, tag := range it.Tags {
		counts := make([]int, len(party))
		max, min := 0, -1
		minAt := -1
		for i, pc := range party {
			n := 0
			for _, h := range pc.Items {
				for _, t := range h.Tags {
					if strings.EqualFold(t, tag) {
						n++
						break
					}
				}
			}
			counts[i] = n
			if n > max {
				max = n
			}
			if min == -1 || n < min {
				min, minAt = n, i
			}
		}
		if minAt == -1 {
			continue
		}
		if spread := max - min; spread > bestSpread {
			bestSpread = spread
			thinnest, bestAxis = party[minAt].Name, tag
		}
	}
	return bestSpread, thinnest, bestAxis
}

// thinnestPC names the pc holding the fewest items on an axis, ties to
// name order — the same pc the spread's arithmetic named.
func thinnestPC(axis string, party []PC) string {
	best, bestName := -1, ""
	for _, pc := range party {
		n := 0
		for _, h := range pc.Items {
			for _, t := range h.Tags {
				if strings.EqualFold(t, axis) {
					n++
					break
				}
			}
		}
		if best == -1 || n < best || (n == best && pc.Name < bestName) {
			best, bestName = n, pc.Name
		}
	}
	return bestName
}

// displayRarity keeps an unclassified rarity readable in reasons.
func displayRarity(r string) string {
	if strings.TrimSpace(r) == "" {
		return "unclassified"
	}
	return r
}
