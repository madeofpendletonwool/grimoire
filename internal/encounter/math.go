// Package encounter implements the D&D encounter builder: the DMG's
// encounter-difficulty math as pure functions, a per-user store for saved
// encounters, and an Open5e-backed monster search.
//
// The math half is deliberately free of I/O so every constant can be asserted
// straight against the indexed rulebooks. The tables below follow the 2014
// Dungeon Master's Guide, chapter 3 ("Combat Encounter Difficulty" /
// "Evaluating Encounter Difficulty") and the Monster Manual introduction
// ("Experience Points by Challenge Rating") — the editions indexed in
// dnd-books/. Where the OCR of the indexed text is damaged, the tables were
// pinned against the books' own worked examples: the DMG's sample party
// (three 3rd-level + one 2nd-level character), the bugbear-plus-hobgoblins
// example, and the XP values printed in the Monster Manual's stat blocks.
package encounter

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Difficulty bands, lowest to highest. The names are the 2014 DMG's four
// categories; an adjusted XP total below the party's Easy threshold is
// reported as Trivial.
const (
	BandTrivial = "Trivial"
	BandEasy    = "Easy"
	BandMedium  = "Medium"
	BandHard    = "Hard"
	BandDeadly  = "Deadly"
)

// Bands lists the DMG difficulty categories from lowest to highest.
var Bands = []string{BandEasy, BandMedium, BandHard, BandDeadly}

// xpByCR maps a challenge rating to the XP a single monster of that rating is
// worth. Source: Monster Manual introduction, "Experience Points by Challenge
// Rating" (CR 0 = 10 XP for monsters with attacks; the book gives 0 XP only to
// monsters with no effective attacks, which do not belong in a combat
// encounter). Spot-verified against indexed stat blocks: Goblin CR 1/4 (50
// XP), Bugbear CR 1 (200 XP), Azer/Ankheg CR 2 (450 XP), Planetar CR 16
// (15,000 XP), Pit Fiend CR 20 (25,000 XP), and the DMG chapter 3 examples
// (CR 3 = 700 XP, dire wolf CR 1 = 200 XP).
var xpByCR = map[string]int{
	"0": 10, "1/8": 25, "1/4": 50, "1/2": 100,
	"1": 200, "2": 450, "3": 700, "4": 1100, "5": 1800,
	"6": 2300, "7": 2900, "8": 3900, "9": 5000, "10": 5900,
	"11": 7200, "12": 8400, "13": 10000, "14": 11500, "15": 13000,
	"16": 15000, "17": 18000, "18": 20000, "19": 22000, "20": 25000,
	"21": 33000, "22": 41000, "23": 50000, "24": 62000, "25": 75000,
	"26": 90000, "27": 105000, "28": 120000, "29": 135000, "30": 155000,
}

// xpThresholds holds the per-character XP thresholds by level (index 1-20)
// and difficulty band. Source: DMG chapter 3, "XP Thresholds by Character
// Level". Rows 12-20 are printed legibly in the indexed text; rows 1-11 are
// pinned by the DMG's worked example (2nd and 3rd level) and the visible
// Hard/Deadly columns, all matching this table.
var xpThresholds = [21][4]int{
	{},
	{25, 50, 75, 100},         // 1st
	{50, 100, 150, 200},       // 2nd
	{75, 150, 225, 400},       // 3rd
	{125, 250, 375, 500},      // 4th
	{250, 500, 750, 1100},     // 5th
	{300, 600, 900, 1400},     // 6th
	{350, 750, 1100, 1700},    // 7th
	{450, 1000, 1400, 2100},   // 8th
	{550, 1200, 1600, 2400},   // 9th
	{600, 1400, 1900, 2800},   // 10th
	{800, 1600, 2400, 3600},   // 11th
	{1000, 2000, 3000, 4500},  // 12th
	{1100, 2200, 3400, 5100},  // 13th
	{1250, 2500, 3800, 5700},  // 14th
	{1400, 2800, 4300, 6400},  // 15th
	{1600, 3200, 4800, 7200},  // 16th
	{2000, 3900, 5900, 8800},  // 17th
	{2100, 4200, 6300, 9500},  // 18th
	{2400, 4900, 7300, 10900}, // 19th
	{2800, 5700, 8500, 12700}, // 20th
}

// multiplierLadder is the DMG's Encounter Multipliers table as an ordered
// ladder, extended at both ends by the party-size adjustments the book spells
// out in its "Party Size" note: a party of fewer than three moves one rung up
// (so a single monster is x1.5 and fifteen-plus monsters x5), a party of six
// or more moves one rung down (a single monster x0.5).
var multiplierLadder = []float64{0.5, 1, 1.5, 2, 2.5, 3, 4, 5}

// baseMultipliers indexes the ladder (rung of x1 = 1) by monster count:
// 1 monster x1, 2 monsters x1.5, 3-6 x2, 7-10 x2.5, 11-14 x3, 15+ x4.
func baseMultiplier(monsters int) float64 {
	switch {
	case monsters <= 1:
		return 1
	case monsters == 2:
		return 1.5
	case monsters <= 6:
		return 2
	case monsters <= 10:
		return 2.5
	case monsters <= 14:
		return 3
	default:
		return 4
	}
}

// multiplierFor applies the Encounter Multipliers table, then adjusts one
// rung for party size per the DMG's Party Size note (fewer than three
// characters: next highest multiplier; six or more: next lowest).
func multiplierFor(monsters, partySize int) float64 {
	base := baseMultiplier(monsters)
	rung := 0
	for i, m := range multiplierLadder {
		if m == base {
			rung = i
			break
		}
	}
	switch {
	case partySize > 0 && partySize < 3:
		rung++
	case partySize >= 6:
		rung--
	}
	if rung < 0 {
		rung = 0
	}
	if rung >= len(multiplierLadder) {
		rung = len(multiplierLadder) - 1
	}
	return multiplierLadder[rung]
}

// shiftedMultiplier is multiplierFor with the objective layer's lever
// applied: rungShift moves the result one rung up or down the DMG's ladder
// after the party-size adjustment, clamped at the ladder's ends. A zero
// shift is exactly multiplierFor.
func shiftedMultiplier(monsters, partySize, rungShift int) float64 {
	base := multiplierFor(monsters, partySize)
	if rungShift == 0 {
		return base
	}
	for i, m := range multiplierLadder {
		if m != base {
			continue
		}
		rung := i + rungShift
		if rung < 0 {
			rung = 0
		}
		if rung >= len(multiplierLadder) {
			rung = len(multiplierLadder) - 1
		}
		return multiplierLadder[rung]
	}
	return base
}

// Monster is one entry in an encounter: a statblock name, its challenge
// rating label ("1/4", "2"), the XP a single monster of that rating is worth,
// and how many of them the encounter includes. XP is always derived from CR
// via the book's table — never taken from a client payload.
type Monster struct {
	Name  string `json:"name"`
	CR    string `json:"cr"`
	XP    int    `json:"xp"`
	Count int    `json:"count"`
}

// WaveVerdict prices one wave of a survive encounter: the monsters on the
// board for that wave, at that wave's own multiplier. The DMG's multiplier
// table assumes every monster is on the board at once, so waves are never
// priced as if they were.
type WaveVerdict struct {
	Monsters   int     `json:"monsters"`
	TotalXP    int     `json:"total_xp"`
	AdjustedXP int     `json:"adjusted_xp"`
	Multiplier float64 `json:"multiplier"`
}

// Verdict is the server-computed difficulty assessment for an encounter.
// Margins carries, per band, how much adjusted XP separates the encounter
// from that band's threshold: negative means the encounter sits above it.
// A Verdict for an empty party keeps zero thresholds and no difficulty.
// Waves is present only for a survive encounter; then AdjustedXP is the sum
// across waves — the whole fight, checked against the party.
type Verdict struct {
	TotalXP      int            `json:"total_xp"`
	AdjustedXP   int            `json:"adjusted_xp"`
	Multiplier   float64        `json:"multiplier"`
	MonsterCount int            `json:"monster_count"`
	PartySize    int            `json:"party_size"`
	Difficulty   string         `json:"difficulty,omitempty"`
	Thresholds   map[string]int `json:"thresholds"`
	Margins      map[string]int `json:"margins"`
	Waves        []WaveVerdict  `json:"waves,omitempty"`
}

// Evaluate computes an encounter's difficulty per the DMG's five-step method:
// sum the party's per-band thresholds, total the monsters' XP, scale it by the
// encounter multiplier (adjusted for party size), and read off the highest
// band whose threshold the adjusted total meets. The DMG's advice to ignore
// monsters far below the group's average CR is a judgment call left to the
// builder — every monster counts here.
func Evaluate(party []int, monsters []Monster) Verdict {
	v := Verdict{Thresholds: map[string]int{}, Margins: map[string]int{}}
	for _, m := range monsters {
		v.TotalXP += m.XP * m.Count
		v.MonsterCount += m.Count
	}
	v.PartySize = len(party)
	v.Multiplier = multiplierFor(v.MonsterCount, v.PartySize)
	v.AdjustedXP = int(math.Round(float64(v.TotalXP) * v.Multiplier))
	v.read(party)
	return v
}

// EvaluateWaves computes a survive encounter's difficulty: each wave at its
// own multiplier, and the encounter's adjusted XP is the sum across all of
// them — the total the party actually has to survive, not the first wave
// alone.
func EvaluateWaves(party []int, waves [][]Monster) Verdict {
	v := Verdict{Thresholds: map[string]int{}, Margins: map[string]int{}}
	for _, wave := range waves {
		raw, count := 0, 0
		for _, m := range wave {
			raw += m.XP * m.Count
			count += m.Count
		}
		mult := multiplierFor(count, len(party))
		adjusted := int(math.Round(float64(raw) * mult))
		v.Waves = append(v.Waves, WaveVerdict{
			Monsters: count, TotalXP: raw, AdjustedXP: adjusted, Multiplier: mult,
		})
		v.TotalXP += raw
		v.MonsterCount += count
		v.AdjustedXP += adjusted
	}
	v.PartySize = len(party)
	if v.TotalXP > 0 {
		v.Multiplier = math.Round(float64(v.AdjustedXP)/float64(v.TotalXP)*100) / 100
	}
	v.read(party)
	return v
}

// read fills the thresholds, margins and difficulty from the adjusted XP
// already computed. "The closest threshold that is lower than the adjusted
// XP value of the monsters determines the encounter's difficulty."
func (v *Verdict) read(party []int) {
	for _, level := range party {
		if level < 1 || level > 20 {
			continue
		}
		for i, band := range Bands {
			v.Thresholds[band] += xpThresholds[level][i]
		}
	}
	if v.PartySize == 0 {
		return
	}
	v.Difficulty = BandTrivial
	for _, band := range Bands {
		v.Margins[band] = v.Thresholds[band] - v.AdjustedXP
		if v.AdjustedXP >= v.Thresholds[band] {
			v.Difficulty = band
		}
	}
}

// ParseCR normalizes a challenge rating to its canonical table label. It
// accepts the fraction forms the books print ("1/4") and the decimal forms
// APIs return ("0.25", "2.0"), and rejects anything that is not a rating in
// the Monster Manual's table.
func ParseCR(s string) (string, error) {
	t := strings.TrimSpace(strings.ToLower(s))
	if t == "" {
		return "", fmt.Errorf("empty challenge rating")
	}
	if xp, ok := xpByCR[t]; ok && xp >= 0 {
		return t, nil
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil {
		return "", fmt.Errorf("challenge rating %q: not a number or fraction", s)
	}
	label := CRLabel(f)
	if _, ok := xpByCR[label]; !ok {
		return "", fmt.Errorf("challenge rating %q: not in the XP table", s)
	}
	return label, nil
}

// CRLabel renders a numeric challenge rating the way the books print it:
// 0.125 -> "1/8", 0.25 -> "1/4", 0.5 -> "1/2", whole numbers bare.
func CRLabel(f float64) string {
	switch f {
	case 0.125:
		return "1/8"
	case 0.25:
		return "1/4"
	case 0.5:
		return "1/2"
	}
	if f == math.Trunc(f) {
		return strconv.Itoa(int(f))
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// CRXP returns the XP value for a canonical CR label.
func CRXP(cr string) (int, bool) {
	xp, ok := xpByCR[cr]
	return xp, ok
}

// XPForCR returns the book XP for a challenge rating in any accepted form,
// rejecting ratings the table does not carry.
func XPForCR(cr string) (int, error) {
	label, err := ParseCR(cr)
	if err != nil {
		return 0, err
	}
	xp, ok := xpByCR[label]
	if !ok {
		return 0, fmt.Errorf("challenge rating %q: no XP value", cr)
	}
	return xp, nil
}

// ThresholdFor returns the per-character XP threshold for a level and band.
func ThresholdFor(level int, band string) (int, bool) {
	if level < 1 || level > 20 {
		return 0, false
	}
	for i, b := range Bands {
		if b == band {
			return xpThresholds[level][i], true
		}
	}
	return 0, false
}
