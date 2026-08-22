package encounter

// Turning a vague idea into a buildable encounter. The DM says "something
// creepy in the swamp" and a difficulty; everything in this file works out
// what that means in XP before a model is asked anything — the budget, the
// shapes that budget can take (one boss, a pair, a pack, a horde), and the
// challenge-rating window each shape allows. The model then picks creatures
// from a pool the catalog vouched for and fits them to a shape it was handed,
// which is why the result lands on the requested difficulty instead of
// somewhere near it.

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// DefaultParty is the table the builder assumes when the DM has not said.
// Four characters is the DMG's own baseline party, and 3rd level is where
// most tables actually play; it keeps "give me an encounter" a one-click
// request instead of a form.
var DefaultParty = []int{3, 3, 3, 3}

// DefaultBand is the difficulty aimed for when none is chosen. The DMG treats
// Medium as the workaday combat: a real fight, nobody expected to die.
const DefaultBand = BandMedium

// Shape is one way to spend an encounter budget: how many monsters are on the
// board, what the DMG multiplier does to their XP at that count, and how much
// raw XP therefore fits.
type Shape struct {
	Key        string  `json:"key"`
	Label      string  `json:"label"`
	Count      int     `json:"count"`
	Multiplier float64 `json:"multiplier"`
	RawXP      int     `json:"raw_xp"`  // total unadjusted XP this shape can spend
	EachXP     int     `json:"each_xp"` // raw XP per monster if they are all alike
	EachCR     string  `json:"each_cr"` // the CR that lands nearest EachXP
}

// Budget is everything the builder knows about what fits, computed from the
// party and the target band alone. It is handed to the model as fact and to
// the UI as the "you have this much room" readout.
type Budget struct {
	Band       string         `json:"band"`
	Party      []int          `json:"party"`
	PartySize  int            `json:"party_size"`
	AvgLevel   float64        `json:"avg_level"`
	Thresholds map[string]int `json:"thresholds"`
	// TargetXP is the adjusted-XP figure the encounter should land on, and
	// CeilingXP the point past which it tips into the next band (for Deadly,
	// the point past which it stops being a fight and becomes an execution).
	TargetXP  int     `json:"target_xp"`
	CeilingXP int     `json:"ceiling_xp"`
	MaxSoloCR string  `json:"max_solo_cr"`
	Shapes    []Shape `json:"shapes"`
}

// Plan works out the budget for a party and a target band. An empty or
// unknown band falls back to Medium; an empty party falls back to the default
// table, so a DM who typed nothing still gets a real answer.
func Plan(party []int, band string) Budget {
	if len(party) == 0 {
		party = append([]int(nil), DefaultParty...)
	}
	band = canonicalBand(band)

	b := Budget{Band: band, Party: party, PartySize: len(party), Thresholds: map[string]int{}}
	total := 0
	for _, lvl := range party {
		if lvl < 1 || lvl > 20 {
			continue
		}
		total += lvl
		for i, name := range Bands {
			b.Thresholds[name] += xpThresholds[lvl][i]
		}
	}
	if len(party) > 0 {
		b.AvgLevel = float64(total) / float64(len(party))
	}

	b.TargetXP = b.Thresholds[band]
	b.CeilingXP = ceilingFor(b.Thresholds, band)

	// The biggest single monster that still fits: one monster gets the ×1
	// rung (×1.5 for a duo of characters, ×0.5 for six or more), so its raw
	// XP allowance is the ceiling divided by that multiplier.
	soloMult := multiplierFor(1, b.PartySize)
	b.MaxSoloCR, _ = nearestCR(int(float64(b.CeilingXP) / soloMult))

	b.Shapes = shapesFor(b.TargetXP, b.PartySize)
	return b
}

// ceilingFor is where the band stops. Every band but Deadly ends at the next
// band's threshold; Deadly has no upper table entry, so it gets half again
// its own threshold — past that the DMG's own advice is that the party dies.
func ceilingFor(thresholds map[string]int, band string) int {
	for i, name := range Bands {
		if name != band {
			continue
		}
		if i+1 < len(Bands) {
			return thresholds[Bands[i+1]]
		}
		return int(math.Round(float64(thresholds[name]) * 1.5))
	}
	return thresholds[BandMedium]
}

// shapeCounts are the group sizes worth planning around: each sits inside a
// different rung of the DMG's Encounter Multipliers table, so they are
// genuinely different builds rather than the same build with a rounding
// difference.
var shapeCounts = []struct {
	key, label string
	count      int
}{
	{"solo", "One big monster", 1},
	{"duo", "A pair", 2},
	{"pack", "A small pack", 4},
	{"gang", "A gang", 6},
	{"horde", "A horde", 9},
}

// shapesFor computes the raw XP each group size may spend to land on the
// target adjusted total. Shapes whose per-monster budget falls below the
// smallest statblock in the game are dropped: a horde of nine is not a shape
// a 1st-level party can afford.
func shapesFor(targetXP, partySize int) []Shape {
	var out []Shape
	for _, sc := range shapeCounts {
		mult := multiplierFor(sc.count, partySize)
		raw := int(math.Round(float64(targetXP) / mult))
		each := raw / sc.count
		if each < 10 {
			continue // below CR 0; the group is too big for this budget
		}
		cr, _ := nearestCR(each)
		out = append(out, Shape{
			Key: sc.key, Label: sc.label, Count: sc.count,
			Multiplier: mult, RawXP: raw, EachXP: each, EachCR: cr,
		})
	}
	return out
}

// canonicalBand maps whatever the client sent onto a real band name.
func canonicalBand(band string) string {
	switch strings.ToLower(strings.TrimSpace(band)) {
	case "easy":
		return BandEasy
	case "medium", "":
		return BandMedium
	case "hard":
		return BandHard
	case "deadly":
		return BandDeadly
	}
	return DefaultBand
}

/* ---------- Reading the DM's idea ---------- */

// Hints is what a free-text idea deterministically implies: creature types to
// gate on, tags to prefer, and the leftover words to match against statblocks.
type Hints struct {
	Types []string `json:"types,omitempty"`
	Tags  []string `json:"tags,omitempty"`
	Terms []string `json:"terms,omitempty"`
}

// creatureTypes are the SRD's own type words, matched whole so "a dragon's
// hoard" gates on dragons but "dragonfly" does not.
var creatureTypes = []string{
	"aberration", "beast", "celestial", "construct", "dragon", "elemental",
	"fey", "fiend", "giant", "humanoid", "monstrosity", "ooze", "plant", "undead",
}

// ideaTags maps the words a DM actually types onto the tags the catalog
// derives from statblocks. The point is that "underwater" and "in the water"
// both mean "things that swim" without the DM knowing the tag exists.
var ideaTags = map[string][]string{
	"underwater": {"aquatic"}, "water": {"aquatic"}, "sea": {"aquatic"},
	"ocean": {"aquatic"}, "river": {"aquatic"}, "lake": {"aquatic"},
	"swamp": {"aquatic"}, "flooded": {"aquatic"},
	"sky": {"flying"}, "air": {"flying"}, "aerial": {"flying"}, "flying": {"flying"},
	"cliff": {"flying"}, "mountain": {"flying"},
	"cave": {"darkvision", "burrowing"}, "caves": {"darkvision", "burrowing"},
	"underdark": {"darkvision"}, "tunnel": {"burrowing"}, "mine": {"burrowing", "darkvision"},
	"dark": {"darkvision"}, "night": {"darkvision"}, "dungeon": {"darkvision"},
	"ambush": {"ambusher"}, "stealth": {"ambusher"}, "sneak": {"ambusher"},
	"assassin": {"ambusher"}, "trap": {"ambusher"},
	"boss": {"legendary"}, "solo": {"legendary"}, "climax": {"legendary"},
	"finale": {"legendary"}, "lair": {"legendary"},
	"caster": {"spellcaster"}, "magic": {"spellcaster"}, "spell": {"spellcaster"},
	"wizard": {"spellcaster"}, "cult": {"spellcaster"}, "ritual": {"spellcaster"},
	"archer": {"ranged"}, "ranged": {"ranged"}, "arrows": {"ranged"}, "snipe": {"ranged"},
	"horde": {"minion"}, "swarm": {"swarm", "minion"}, "mob": {"minion"},
	"scary": {"frightening"}, "creepy": {"frightening"}, "horror": {"frightening"},
	"terrifying": {"frightening"}, "dread": {"frightening"},
	"poison": {"poisoner"}, "venom": {"poisoner"}, "plague": {"poisoner"},
	"fire": {"fire"}, "volcano": {"fire"}, "burning": {"fire"},
	"ice": {"cold"}, "frozen": {"cold"}, "snow": {"cold"}, "arctic": {"cold"}, "cold": {"cold"},
	"storm": {"lightning"}, "thunder": {"thunder"},
	"graveyard": {"undead-caller"}, "crypt": {"undead-caller"}, "tomb": {"undead-caller"},
	"forest": {"climbing"}, "jungle": {"climbing"}, "trees": {"climbing"},
}

// ideaStopwords are the words that carry no monster signal. Without them, "I
// want" and "the party" outrank the actual idea in every statblock match.
var ideaStopwords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "that": true, "this": true,
	"they": true, "them": true, "their": true, "there": true, "here": true,
	"want": true, "like": true, "some": true, "something": true, "anything": true,
	"encounter": true, "encounters": true, "fight": true, "combat": true, "battle": true,
	"party": true, "players": true, "player": true, "characters": true, "character": true,
	"level": true, "levels": true, "session": true, "campaign": true, "game": true,
	"make": true, "build": true, "give": true, "need": true, "would": true, "could": true,
	"really": true, "very": true, "just": true, "maybe": true, "should": true,
	"monster": true, "monsters": true, "creature": true, "creatures": true,
	"idea": true, "ideas": true, "kind": true, "sort": true, "thing": true, "things": true,
	"easy": true, "medium": true, "hard": true, "deadly": true, "difficult": true,
	"about": true, "into": true, "from": true, "over": true, "than": true, "then": true,
}

var wordRE = regexp.MustCompile(`[a-z][a-z'-]+`)

// ReadIdea pulls the deterministic signal out of a free-text idea: which
// creature types it names, which tags its words imply, and what is left over
// to match against statblock text.
func ReadIdea(idea string) Hints {
	low := strings.ToLower(idea)
	words := wordRE.FindAllString(low, -1)

	var h Hints
	seenType := map[string]bool{}
	seenTag := map[string]bool{}
	seenTerm := map[string]bool{}

	for _, w := range words {
		singular := strings.TrimSuffix(w, "s")
		for _, t := range creatureTypes {
			if w == t || singular == t {
				if !seenType[t] {
					seenType[t] = true
					h.Types = append(h.Types, t)
				}
			}
		}
		for _, tag := range ideaTags[w] {
			if !seenTag[tag] {
				seenTag[tag] = true
				h.Tags = append(h.Tags, tag)
			}
		}
		if len(w) < 4 || ideaStopwords[w] || seenTerm[w] {
			continue
		}
		seenTerm[w] = true
		h.Terms = append(h.Terms, w)
	}
	sort.Strings(h.Types)
	sort.Strings(h.Tags)
	return h
}

/* ---------- Candidate pool ---------- */

// Pool is the shortlist handed to the model: creatures the catalog vouches
// for, split by the slot each can fill so the model has minions to swarm with
// and something big to build around rather than forty creatures of one size.
type Pool struct {
	Boss     []Creature `json:"boss"`
	Standard []Creature `json:"standard"`
	Minion   []Creature `json:"minion"`
	Flavour  []Creature `json:"flavour"`
}

// All flattens the pool for validation and counting.
func (p Pool) All() []Creature {
	out := make([]Creature, 0, len(p.Boss)+len(p.Standard)+len(p.Minion)+len(p.Flavour))
	out = append(out, p.Boss...)
	out = append(out, p.Standard...)
	out = append(out, p.Minion...)
	out = append(out, p.Flavour...)
	return out
}

// Len reports how many distinct creatures the pool offers.
func (p Pool) Len() int { return len(p.All()) }

// perTier is how many creatures each slot contributes. Enough for a real
// choice, few enough that the whole pool still fits comfortably in a prompt.
const perTier = 14

// BuildPool assembles the shortlist for a budget and an idea. Each tier is
// filtered against the CR window that slot can afford, so nothing in the pool
// is a creature the encounter could not actually pay for.
//
// The flavour tier is the escape hatch from the DM's own words: it ignores
// the CR windows entirely and returns whatever best matches the idea, so a
// request for "goblins" surfaces every goblin even when the budget wants
// something bigger. The model is told which tier is which.
func BuildPool(cat *Catalog, b Budget, h Hints, exclude map[string]bool) Pool {
	if cat == nil {
		return Pool{}
	}
	soloCap := crValue(b.MaxSoloCR)

	// Slot windows. A boss may reach the solo cap; a standard monster is
	// roughly what a pack of four can afford each; a minion is anything the
	// party can drop in a round or two.
	standardCR := 0.0
	minionCR := 0.5
	for _, s := range b.Shapes {
		switch s.Key {
		case "pack":
			standardCR = crValue(s.EachCR)
		case "horde":
			minionCR = math.Max(minionCR, crValue(s.EachCR))
		}
	}
	if standardCR <= 0 {
		standardCR = math.Max(1, soloCap/3)
	}

	tier := func(minCR, maxCR float64, limit int) []Creature {
		return cat.Filter(Filter{
			MinCR: minCR, MaxCR: maxCR,
			Types: h.Types, Tags: h.Tags, Terms: h.Terms,
			Exclude: exclude, Limit: limit,
		})
	}

	p := Pool{
		Boss:     tier(math.Max(standardCR, 1), soloCap, perTier),
		Standard: tier(math.Max(0.5, standardCR/3), standardCR*1.5, perTier),
		Minion:   tier(0, math.Max(minionCR, 1), perTier),
	}
	if len(h.Terms) > 0 || len(h.Types) > 0 {
		p.Flavour = cat.Filter(Filter{
			MaxCR: 30, Types: h.Types, Tags: h.Tags, Terms: h.Terms,
			Exclude: exclude, Limit: perTier,
		})
	}
	return dedupePool(p)
}

// dedupePool drops a creature from the lower tiers once a higher tier has
// already offered it, so the prompt lists each statblock once.
func dedupePool(p Pool) Pool {
	seen := map[string]bool{}
	keep := func(in []Creature) []Creature {
		var out []Creature
		for _, c := range in {
			k := squash(c.Name)
			if seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, c)
		}
		return out
	}
	p.Flavour = keep(p.Flavour)
	p.Boss = keep(p.Boss)
	p.Standard = keep(p.Standard)
	p.Minion = keep(p.Minion)
	return p
}

/* ---------- Parsing the model's answer ---------- */

// rosterLineRE matches the roster block's lines: an optional bullet, a count,
// an optional multiplication sign, then the name. "3 × Goblin", "- 3x Goblin",
// "3 Goblin" and "Goblin" (implicitly one) all parse.
var rosterLineRE = regexp.MustCompile(`^\s*(?:[-*+]\s*)?(?:(\d{1,3})\s*[x×*]?\s+)?(.+?)\s*$`)

// trailingCountRE catches the other way people write it: "Goblin ×6".
var trailingCountRE = regexp.MustCompile(`^(.+?)\s*[x×*]\s*(\d{1,3})$`)

// headingRE matches a Markdown heading and captures its text.
var headingRE = regexp.MustCompile(`^#{1,6}\s+(.*)$`)

// Design is a parsed encounter proposal: the title and roster pulled out for
// the machinery, the whole reply kept for the reader.
type Design struct {
	Name       string    `json:"name"`
	Monsters   []Monster `json:"monsters"`
	Unverified []string  `json:"unverified,omitempty"`
	Prose      string    `json:"prose"`
}

// ParseDesign reads the model's Markdown reply: the first heading is the
// encounter's name, the "Roster" section is the machine-parsed part, and
// every name in it is resolved against the catalog. Names the catalog does
// not carry are reported as unverified rather than silently kept — the same
// discipline the deck builder applies to fabricated cards.
func ParseDesign(cat *Catalog, md string) Design {
	d := Design{Prose: md}
	lines := strings.Split(md, "\n")

	inRoster := false
	var rosterLines []string
	for _, raw := range lines {
		line := strings.TrimRight(raw, " \t\r")
		if m := headingRE.FindStringSubmatch(line); m != nil {
			heading := strings.TrimSpace(m[1])
			if d.Name == "" && !isSectionHeading(heading) {
				d.Name = strings.Trim(heading, "*_ ")
			}
			inRoster = strings.EqualFold(strings.Trim(heading, "*_ :"), "roster")
			continue
		}
		if inRoster && strings.TrimSpace(line) != "" {
			rosterLines = append(rosterLines, line)
		}
	}

	counts := map[string]int{}
	var order []string
	for _, line := range rosterLines {
		name, count := parseRosterLine(line)
		if name == "" {
			continue
		}
		key := squash(name)
		if key == "" {
			continue
		}
		if _, seen := counts[key]; !seen {
			order = append(order, key)
		}
		counts[key] += count
	}

	names := map[string]string{}
	for _, line := range rosterLines {
		if n, _ := parseRosterLine(line); n != "" {
			if _, ok := names[squash(n)]; !ok {
				names[squash(n)] = n
			}
		}
	}

	for _, key := range order {
		written := names[key]
		cr, ok := cat.Lookup(written)
		if !ok {
			d.Unverified = append(d.Unverified, written)
			continue
		}
		d.Monsters = append(d.Monsters, Monster{
			Name: cr.Name, CR: cr.CR, XP: cr.XP, Count: clampCount(counts[key]),
		})
	}
	return d
}

// clampCount keeps a parsed count inside what the store will accept.
func clampCount(n int) int {
	if n < 1 {
		return 1
	}
	if n > 200 {
		return 200
	}
	return n
}

// isSectionHeading reports whether a heading is one of the design's own
// section labels rather than the encounter's title.
func isSectionHeading(h string) bool {
	switch strings.ToLower(strings.Trim(h, "*_ :")) {
	case "roster", "the pitch", "pitch", "tactics", "terrain", "setup", "the setup",
		"treasure", "reward", "rewards", "scaling", "notes", "concept", "read-aloud",
		"the read-aloud", "what they want", "how it plays":
		return true
	}
	return false
}

// parseRosterLine reads one roster entry into a name and a count.
func parseRosterLine(line string) (string, int) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "|") || strings.HasPrefix(line, ">") {
		return "", 0
	}
	// Anything after an em dash, en dash or " - " is the model's note on the
	// entry ("2 × Goblin — lookouts on the ledge"), not part of the name.
	for _, sep := range []string{" — ", " – ", " -- ", " - ", ": ", " ("} {
		if i := strings.Index(line, sep); i > 0 {
			line = line[:i]
		}
	}
	line = strings.TrimSpace(strings.Trim(line, "*_`"))
	if line == "" {
		return "", 0
	}
	if m := trailingCountRE.FindStringSubmatch(line); m != nil {
		n, _ := strconv.Atoi(m[2])
		return strings.TrimSpace(m[1]), clampCount(n)
	}
	m := rosterLineRE.FindStringSubmatch(line)
	if m == nil {
		return "", 0
	}
	count := 1
	if m[1] != "" {
		n, err := strconv.Atoi(m[1])
		if err == nil {
			count = clampCount(n)
		}
	}
	name := strings.TrimSpace(strings.Trim(m[2], "*_`.,"))
	if !looksLikeAName(name) {
		return "", 0
	}
	return name, count
}

// looksLikeAName rejects the stray prose a model sometimes leaves inside the
// roster block ("Total: 600 XP", "That is the whole fight."). Without it,
// every such line would be reported to the DM as a monster the SRD lacks.
func looksLikeAName(name string) bool {
	if name == "" || len(name) > 60 {
		return false
	}
	if len(strings.Fields(name)) > 6 {
		return false
	}
	for _, r := range name {
		if unicode.IsLetter(r) {
			return true
		}
	}
	return false
}

// Describe renders a roster the way a prompt or a saved note wants it.
func Describe(monsters []Monster) string {
	if len(monsters) == 0 {
		return "(empty)"
	}
	parts := make([]string, 0, len(monsters))
	for _, m := range monsters {
		parts = append(parts, fmt.Sprintf("%d × %s (CR %s)", m.Count, m.Name, m.CR))
	}
	return strings.Join(parts, ", ")
}
