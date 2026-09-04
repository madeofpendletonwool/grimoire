package items

// The item designer (MAD-383). A CR calculator has a procedure to check
// against; magic items do not. The DMG gives rarity guidance, not a
// formula, and any tool claiming to *compute* an item's rarity is
// inventing authority. So the validation here is explicitly comparative,
// and says so in every shape it travels in:
//
//   - Rarity bands are derived from the corpus — bonus size, charge
//     counts, recharge rates, save DCs and die expressions, distributed
//     across the real SRD items at each rarity. A designed item is placed
//     against that distribution as a set of checkable claims ("every +3
//     weapon in the SRD is Legendary"), never a computed verdict.
//   - Hard structural rules, which are real: attunement grammar, the
//     charge-and-recharge grammar, item types that must name a base item,
//     and the requirement that every mechanical effect is expressed in the
//     game's own vocabulary — a save with a DC and an ability, not "the
//     target is weakened".
//   - Nearest neighbours from the corpus, so the DM sees the items closest
//     to what they designed and judges rarity themselves.
//
// Nowhere in the report is there a field that claims to know the item's
// rarity. That absence is the feature.

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

/* ---------- the designed item ---------- */

// DesignTypes is the item-type vocabulary the designer declares: the SRD's
// own categories, the same names the mirror carries.
var DesignTypes = []string{
	"weapon", "armor", "potion", "ring", "rod", "staff", "wand", "scroll", "wondrous item",
}

// Abilities is the save vocabulary an effect may draw from.
var Abilities = []string{"strength", "dexterity", "constitution", "intelligence", "wisdom", "charisma"}

// BonusTargets is what a numeric bonus may apply to — the game's own
// objects of a bonus, not free prose.
var BonusTargets = []string{
	"attack and damage rolls", "attack rolls", "damage rolls", "armor class",
	"saving throws", "saving throws against spells", "spell attack rolls",
	"spell save dc", "speed", "passive perception",
}

// Rarities is the corpus rarity vocabulary, common to legendary, with the
// ranks the bands order by.
var Rarities = []struct {
	Name string
	Rank int
}{
	{"Common", 0}, {"Uncommon", 1}, {"Rare", 2}, {"Very Rare", 3}, {"Legendary", 4},
}

// Design is one designed item: the structured, checkable statement of
// what it is and does. Text carries the flavour prose; every mechanical
// claim must live in the structured fields — that is the rule the
// validator enforces.
type Design struct {
	Name string `json:"name"`
	Type string `json:"type"`
	// Base is the base item the design modifies — required for weapons and
	// armor, whose power is meaningless without the thing they are built
	// on ("a +1 what?").
	Base string `json:"base,omitempty"`
	// Bonus is the item's +N, when it grants one. The game's ceiling is 3.
	Bonus int `json:"bonus,omitempty"`
	// Rarity is the DM's own label. The designer never overwrites it and
	// never derives one; the comparison report exists so the DM can judge.
	Rarity string `json:"rarity,omitempty"`
	// Attunement states whether the item requires attunement and by whom.
	Attunement Attunement `json:"attunement"`
	// Charges and Recharge are the spent-resource grammar: an item with
	// charges states how it regains them, or the design does not stand.
	Charges  int    `json:"charges,omitempty"`
	Recharge string `json:"recharge,omitempty"`
	// Effects are the mechanical effects, each in the game's own
	// vocabulary.
	Effects []Effect `json:"effects,omitempty"`
	// Text is the flavour — where it comes from, what it looks like. It
	// may say anything; it may say nothing mechanical, because anything
	// mechanical in it is invisible to the validator by design.
	Text string `json:"text,omitempty"`
}

// Attunement is the attunement grammar.
type Attunement struct {
	Required  bool   `json:"required"`
	Condition string `json:"condition,omitempty"` // "by a cleric", "by a warlock"
}

// Effect is one mechanical effect, expressed in the game's own
// vocabulary. Text carries the printed line verbatim; at least one of the
// structured fields must carry the same claim in checkable form.
type Effect struct {
	Text string `json:"text"`
	// Save is a saving throw the effect imposes — a DC and an ability.
	Save *EffectSave `json:"save,omitempty"`
	// Spell is a named spell the item lets its wielder produce.
	Spell string `json:"spell,omitempty"`
	// Damage is a damage expression in dice, e.g. "1d6 fire".
	Damage string `json:"damage,omitempty"`
	// Bonus is a numeric bonus and what it applies to.
	Bonus   int    `json:"bonus,omitempty"`
	BonusTo string `json:"bonus_to,omitempty"`
}

// EffectSave is the save half of an effect: the whole point is that "the
// target must make a DC 15 Dexterity saving throw" is checkable and "the
// target is weakened" is not.
type EffectSave struct {
	DC      int    `json:"dc"`
	Ability string `json:"ability"`
	OnFail  string `json:"on_fail,omitempty"` // the game word for the outcome: a condition or damage
}

/* ---------- structural validation ---------- */

var (
	reRechargeGrammar = regexp.MustCompile(`^(all|\d+d\d+([+-]\d+)?)?( ?daily)?( ?at (dawn|dusk|noon|midnight))?$`)
	reBonusIn         = regexp.MustCompile(`\+(\d+)`)
	reSaveDC          = regexp.MustCompile(`dc (\d+)`)
	reDice            = regexp.MustCompile(`^(\d+)d(\d+)([+-]\d+)?$`)
	reDiceIn          = regexp.MustCompile(`(\d+)d(\d+)([+-]\d+)?`)
)

// Validate applies the hard structural rules. It returns the problems,
// empty when the design stands. These are rules the game itself keeps —
// attunement grammar, charge grammar, base items, vocabulary — not a
// power score; nothing here says whether the item is too strong.
func (d *Design) Validate() []string {
	var problems []string

	name := strings.TrimSpace(d.Name)
	if name == "" {
		problems = append(problems, "a design needs a name")
	}
	if !validVocabulary(d.Type, DesignTypes) {
		problems = append(problems, fmt.Sprintf(
			"item type %q is not one of [%s]", d.Type, strings.Join(DesignTypes, ", ")))
	}
	if d.Type == "weapon" || d.Type == "armor" {
		if strings.TrimSpace(d.Base) == "" {
			problems = append(problems, fmt.Sprintf(
				"a %s design must name the base item it modifies — the power means nothing without the thing it is built on", d.Type))
		}
	}
	if d.Bonus < 0 || d.Bonus > 3 {
		problems = append(problems, fmt.Sprintf("bonus %+d is outside the game's 0–3 ceiling", d.Bonus))
	}

	// Attunement grammar: a condition is meaningless without the
	// requirement, and a consumable is drunk or read, never attuned.
	if d.Attunement.Condition != "" && !d.Attunement.Required {
		problems = append(problems, fmt.Sprintf(
			"attunement condition %q without requiring attunement — a condition only means something when attunement is required", d.Attunement.Condition))
	}
	if d.Attunement.Required && (d.Type == "potion" || d.Type == "scroll") {
		problems = append(problems, fmt.Sprintf(
			"a %s is consumed in use and cannot require attunement", d.Type))
	}

	// Charge and recharge grammar: charges state their recovery or the
	// design does not stand.
	if d.Charges < 0 {
		problems = append(problems, "charges cannot be negative")
	}
	if d.Charges > 0 {
		if strings.TrimSpace(d.Recharge) == "" {
			problems = append(problems, fmt.Sprintf(
				"the item holds %d charges but states no way to regain them — write the recharge like \"1d6+1 daily at dawn\"", d.Charges))
		} else if !rechargeReads(d.Recharge) {
			problems = append(problems, fmt.Sprintf(
				"recharge %q does not read as charge recovery — write it like \"1d6+1 daily at dawn\" or \"all daily at dusk\"", d.Recharge))
		}
	}
	if d.Charges <= 0 && strings.TrimSpace(d.Recharge) != "" {
		problems = append(problems, "a recharge is stated but the item holds no charges")
	}

	for i := range d.Effects {
		problems = append(problems, validateEffect(i, &d.Effects[i])...)
	}
	return problems
}

// validateEffect checks one effect against the vocabulary rule: a
// mechanical effect must be expressed in the game's own terms. "The
// target is weakened" names no DC, no ability, no dice and no bonus, so
// no comparison and no downstream tool can price it — that is the one
// thing an effect may not be.
func validateEffect(i int, e *Effect) []string {
	var problems []string
	label := strings.TrimSpace(e.Text)
	has := false
	if e.Save != nil {
		has = true
		if e.Save.DC < 1 || e.Save.DC > 30 {
			problems = append(problems, fmt.Sprintf(
				"effect %d (%q) carries a save with no readable DC — a save is a DC from 1 to 30", i+1, label))
		}
		if !validVocabulary(e.Save.Ability, Abilities) {
			problems = append(problems, fmt.Sprintf(
				"effect %d (%q) saves with ability %q, which is not one of [%s]", i+1, label, e.Save.Ability, strings.Join(Abilities, ", ")))
		}
	}
	if strings.TrimSpace(e.Spell) != "" {
		has = true
	}
	if strings.TrimSpace(e.Damage) != "" {
		has = true
		fields := strings.Fields(strings.ToLower(e.Damage))
		if len(fields) == 0 || !reDice.MatchString(fields[0]) {
			problems = append(problems, fmt.Sprintf(
				"effect %d (%q) states damage %q, which is not in dice — write it like \"1d6 fire\"", i+1, label, e.Damage))
		}
	}
	if e.Bonus != 0 || strings.TrimSpace(e.BonusTo) != "" {
		has = true
		if e.Bonus < 1 || e.Bonus > 3 {
			problems = append(problems, fmt.Sprintf(
				"effect %d (%q) grants a %+d bonus; the game's bonuses run +1 to +3", i+1, label, e.Bonus))
		}
		if !validVocabulary(e.BonusTo, BonusTargets) {
			problems = append(problems, fmt.Sprintf(
				"effect %d (%q) applies its bonus to %q, which is not one of [%s] — name what the bonus applies to", i+1, label, e.BonusTo, strings.Join(BonusTargets, ", ")))
		}
	}
	if !has {
		problems = append(problems, fmt.Sprintf(
			"effect %d (%q) states no game vocabulary — give it a save with a DC and an ability, a named spell, damage in dice, or a bonus and what it applies to", i+1, label))
	}
	return problems
}

// rechargeReads reports whether a recharge string follows the charge
// grammar: an amount ("1d6+1" or "all") plus a recovery time ("daily",
// "at dawn", "daily at dusk").
func rechargeReads(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return false
	}
	return reRechargeGrammar.MatchString(s) && strings.Contains(s, "daily") || strings.Contains(s, " at ")
}

func validVocabulary(value string, vocab []string) bool {
	v := strings.ToLower(strings.TrimSpace(value))
	if v == "" {
		return false
	}
	for _, ok := range vocab {
		if v == ok {
			return true
		}
	}
	return false
}

/* ---------- the metrics the bands read ---------- */

// Metrics is what the rarity bands read off an item — corpus or designed
// — every number derived from what the item itself says.
type Metrics struct {
	Bonus          int     `json:"bonus"`
	Charges        int     `json:"charges"`
	RechargePerDay float64 `json:"recharge_per_day"`
	SaveDC         int     `json:"save_dc"`
	DamagePerRoll  float64 `json:"damage_dice"`
}

// MetricsOfItem derives metrics from a corpus item's printed text.
func MetricsOfItem(it Item) Metrics {
	text := strings.ToLower(it.Name + "\n" + it.Text)
	m := Metrics{Bonus: maxBonusIn(text), Charges: it.Charges, RechargePerDay: rechargePerDay(it.Charges, it.Recharge)}
	for _, m2 := range reSaveDC.FindAllStringSubmatch(text, -1) {
		if v, err := strconv.Atoi(m2[1]); err == nil && v > m.SaveDC && v <= 30 {
			m.SaveDC = v
		}
	}
	m.DamagePerRoll = maxDiceIn(text)
	return m
}

// MetricsOfDesign derives the same metrics from a design's structured
// fields — the fields the validator just checked, so the numbers and the
// words can never disagree.
func MetricsOfDesign(d Design) Metrics {
	m := Metrics{Bonus: d.Bonus, Charges: d.Charges, RechargePerDay: rechargePerDay(d.Charges, d.Recharge)}
	for _, e := range d.Effects {
		if e.Save != nil && e.Save.DC > m.SaveDC {
			m.SaveDC = e.Save.DC
		}
		// The damage grammar is dice first, type after — "1d6 fire" — so
		// the dice lead the field.
		if fields := strings.Fields(strings.ToLower(e.Damage)); len(fields) > 0 {
			if exp := diceExpected(fields[0]); exp > m.DamagePerRoll {
				m.DamagePerRoll = exp
			}
		}
		if e.Bonus > m.Bonus {
			m.Bonus = e.Bonus
		}
	}
	return m
}

func maxBonusIn(text string) int {
	best := 0
	for _, m := range reBonusIn.FindAllStringSubmatch(text, -1) {
		if v, err := strconv.Atoi(m[1]); err == nil && v > best && v <= 5 {
			best = v
		}
	}
	return best
}

// diceExpected reads "3d6+2" into its average roll — the one number that
// compares fairly across die shapes.
func diceExpected(s string) float64 {
	m := reDice.FindStringSubmatch(strings.ToLower(strings.TrimSpace(s)))
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	faces, _ := strconv.Atoi(m[2])
	exp := float64(n) * float64(faces+1) / 2
	if m[3] != "" {
		if flat, err := strconv.Atoi(m[3][1:]); err == nil {
			if m[3][0] == '-' {
				exp -= float64(flat)
			} else {
				exp += float64(flat)
			}
		}
	}
	return exp
}

func maxDiceIn(text string) float64 {
	best := 0.0
	for _, m := range reDiceIn.FindAllStringSubmatch(text, -1) {
		if exp := diceExpected(m[0]); exp > best {
			best = exp
		}
	}
	return best
}

// rechargePerDay prices a recharge rule into expected charges a day — the
// rate the bands distribute across the corpus.
func rechargePerDay(charges int, recharge string) float64 {
	if charges <= 0 || strings.TrimSpace(recharge) == "" {
		return 0
	}
	fields := strings.Fields(strings.ToLower(recharge))
	if fields[0] == "all" {
		return float64(charges)
	}
	return diceExpected(fields[0])
}

/* ---------- the rarity bands ---------- */

// Span is one metric's distribution across the items of one rarity.
type Span struct {
	Min    float64 `json:"min"`
	Median float64 `json:"median"`
	Max    float64 `json:"max"`
}

// Band is the corpus's own footprint at one rarity: how the real SRD
// items at that rarity distribute across the metrics. It is a description
// of the shelf, not a rule.
type Band struct {
	Rarity         string `json:"rarity"`
	Rank           int    `json:"rank"`
	Count          int    `json:"count"`
	Bonus          Span   `json:"bonus"`
	Charges        Span   `json:"charges"`
	RechargePerDay Span   `json:"recharge_per_day"`
	SaveDC         Span   `json:"save_dc"`
	DamagePerRoll  Span   `json:"damage_dice"`
}

// Bands builds the corpus distribution, one band per rarity present in
// the list, ordered common to legendary. Items with no stated rarity are
// counted nowhere — they have no rung to stand on.
func Bands(list []Item) []Band {
	type acc struct {
		rank     int
		count    int
		bonus    []float64
		charges  []float64
		recharge []float64
		saveDC   []float64
		damage   []float64
	}
	bands := map[string]*acc{}
	for _, it := range list {
		if strings.TrimSpace(it.Rarity) == "" {
			continue
		}
		b, ok := bands[it.Rarity]
		if !ok {
			b = &acc{rank: rarityRankOf(it)}
			bands[it.Rarity] = b
		}
		b.count++
		m := MetricsOfItem(it)
		b.bonus = append(b.bonus, float64(m.Bonus))
		b.charges = append(b.charges, float64(m.Charges))
		b.recharge = append(b.recharge, m.RechargePerDay)
		b.saveDC = append(b.saveDC, float64(m.SaveDC))
		b.damage = append(b.damage, m.DamagePerRoll)
	}

	out := make([]Band, 0, len(bands))
	for name, b := range bands {
		band := Band{Rarity: name, Rank: b.rank, Count: b.count}
		band.Bonus = spanOf(b.bonus)
		band.Charges = spanOf(b.charges)
		band.RechargePerDay = spanOf(b.recharge)
		band.SaveDC = spanOf(b.saveDC)
		band.DamagePerRoll = spanOf(b.damage)
		out = append(out, band)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Rank < out[j].Rank })
	return out
}

func spanOf(vals []float64) Span {
	if len(vals) == 0 {
		return Span{}
	}
	sorted := append([]float64(nil), vals...)
	sort.Float64s(sorted)
	return Span{
		Min:    sorted[0],
		Median: sorted[len(sorted)/2],
		Max:    sorted[len(sorted)-1],
	}
}

// rarityRankOf reports where an item's rarity sits in the corpus order,
// falling back to the declared vocabulary when the corpus carried no rank.
func rarityRankOf(it Item) int {
	if it.RarityRank > 0 {
		return it.RarityRank
	}
	for _, r := range Rarities {
		if strings.EqualFold(r.Name, it.Rarity) {
			return r.Rank
		}
	}
	return 0
}

/* ---------- the comparison report ---------- */

// Neighbour is one corpus item close to the design — the DM's yardstick.
// It carries what it shares, never a score that pretends to rank power.
type Neighbour struct {
	Name     string   `json:"name"`
	Rarity   string   `json:"rarity,omitempty"`
	Type     string   `json:"type,omitempty"`
	Tags     []string `json:"tags,omitempty"`
	Homebrew bool     `json:"homebrew,omitempty"`
	// Shares names what the two have in common — type, base item, tags —
	// so the DM can see why this item surfaced as a comparison.
	Shares []string `json:"shares,omitempty"`
}

// Report is the designer's whole answer to a draft. Problems is the
// structural verdict — empty means the design stands. Everything else is
// comparison: the design's own metrics, the corpus bands, the checkable
// claims the metrics support, and the nearest items on the shelf. There
// is deliberately no field that states, computes or implies the item's
// rarity.
type Report struct {
	Problems []string `json:"problems,omitempty"`
	Metrics  Metrics  `json:"metrics"`
	// Rarity is the label the design itself carries — echoed, never
	// assigned. Empty when the DM labelled nothing.
	Rarity     string      `json:"rarity,omitempty"`
	Bands      []Band      `json:"bands"`
	Notes      []string    `json:"notes,omitempty"`
	Neighbours []Neighbour `json:"neighbours,omitempty"`
}

// Compare places a design against the corpus distribution and the nearest
// real items. It never judges: the notes are factual statements about the
// shelf ("every SRD item granting a +3 bonus is Legendary"), the bands
// are the shelf's own numbers, and the neighbours are the DM's yardsticks.
func Compare(d Design, corpus []Item) *Report {
	rep := &Report{Rarity: strings.TrimSpace(d.Rarity), Metrics: MetricsOfDesign(d)}
	rep.Problems = d.Validate()
	if len(rep.Problems) > 0 {
		// Nothing is compared against a non-item: a structurally broken
		// draft has no metrics worth placing, only problems to fix.
		rep.Notes = nil
		rep.Neighbours = nil
		return rep
	}

	rep.Bands = Bands(corpus)
	rep.Notes = comparisonNotes(d, rep.Metrics, rep.Rarity, rep.Bands)
	rep.Neighbours = NeighboursOf(d, corpus, 3)
	return rep
}

// comparisonNotes turns the placement into checkable claims. Each note is
// a statement of fact about the SRD shelf the DM can verify by looking —
// the honest substitute for a rarity formula.
func comparisonNotes(d Design, m Metrics, rarity string, bands []Band) []string {
	var notes []string
	bandOf := map[string]Band{}
	rankOfRarity := -1
	for _, b := range bands {
		bandOf[strings.ToLower(b.Rarity)] = b
		if strings.EqualFold(b.Rarity, rarity) {
			rankOfRarity = b.Rank
		}
	}

	// metricDesc describes one metric for the notes: what the design's
	// value claims, what a band endpoint reads as, and where the metric
	// lives in a band.
	type metricDesc struct {
		name  string                 // plural noun for the band range: "bonuses"
		value float64                // the design's own value
		val   string                 // the design's value in claim form: "a +3 bonus"
		num   func(v float64) string // a band endpoint, short: "+2"
		span  func(Band) Span
	}
	descs := []metricDesc{
		{"bonuses", float64(m.Bonus), fmt.Sprintf("a +%d bonus", m.Bonus),
			func(v float64) string { return "+" + trimFloat(v) },
			func(b Band) Span { return b.Bonus }},
		{"charge counts", float64(m.Charges), fmt.Sprintf("%s charges", trimFloat(float64(m.Charges))),
			trimFloat,
			func(b Band) Span { return b.Charges }},
		{"daily recharge", m.RechargePerDay, fmt.Sprintf("about %s charges a day", trimFloat(m.RechargePerDay)),
			trimFloat,
			func(b Band) Span { return b.RechargePerDay }},
		{"save DCs", float64(m.SaveDC), fmt.Sprintf("a save DC of %d", m.SaveDC),
			func(v float64) string { return strconv.Itoa(int(v)) },
			func(b Band) Span { return b.SaveDC }},
		{"die expressions", m.DamagePerRoll, fmt.Sprintf("a die roll worth about %s damage", trimFloat(m.DamagePerRoll)),
			trimFloat,
			func(b Band) Span { return b.DamagePerRoll }},
	}

	for _, md := range descs {
		if md.value <= 0 {
			continue
		}
		if b, atRarity := bandOf[strings.ToLower(rarity)]; atRarity {
			span := md.span(b)
			if md.value <= span.Max {
				notes = append(notes, fmt.Sprintf(
					"At %s the SRD items' %s run %s to %s; this design carries %s.",
					b.Rarity, md.name, md.num(span.Min), md.num(span.Max), md.val))
				continue
			}
		}
		// Where does the corpus carry this at all?
		var reaching, above []string
		for _, b := range bands {
			if md.span(b).Max >= md.value {
				reaching = append(reaching, b.Rarity)
				if rankOfRarity >= 0 && b.Rank > rankOfRarity {
					above = append(above, b.Rarity)
				}
			}
		}
		switch {
		case len(reaching) == 0:
			notes = append(notes, fmt.Sprintf("No SRD item in the mirror carries %s.", md.val))
		case rankOfRarity >= 0 && len(reaching) == len(above):
			notes = append(notes, fmt.Sprintf("Every SRD item carrying %s is %s — nothing at %s or below reaches it.",
				md.val, joinNames(above), rarity))
		default:
			notes = append(notes, fmt.Sprintf("The SRD items carrying %s are %s.", md.val, joinNames(reaching)))
		}
	}
	return notes
}

func joinNames(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	case 2:
		return names[0] + " or " + names[1]
	default:
		return strings.Join(names[:len(names)-1], ", ") + ", or " + names[len(names)-1]
	}
}

func trimFloat(v float64) string {
	if v == math.Trunc(v) {
		return strconv.Itoa(int(v))
	}
	return strconv.FormatFloat(v, 'f', 1, 64)
}

/* ---------- nearest neighbours ---------- */

// NeighboursOf returns the corpus items closest to the design, ranked by
// the things a DM would call "similar": the same kind of item, built on
// the same base, sharing the derived tags, with metrics in the same
// neighbourhood. The scoring is additive and explainable, and the result
// is a reading list, not a verdict.
func NeighboursOf(d Design, corpus []Item, n int) []Neighbour {
	if n <= 0 {
		n = 3
	}
	target := MetricsOfDesign(d)
	designTags := designTagsOf(d)
	lowType := strings.ToLower(d.Type)
	lowBase := squash(d.Base)

	type ranked struct {
		it     Item
		score  float64
		shares []string
	}
	var out []ranked
	for _, it := range corpus {
		score := 0.0
		var shares []string
		if strings.EqualFold(strings.TrimSpace(it.Type), lowType) {
			score += 3
			if lowType != "" {
				shares = append(shares, it.Type)
			}
		}
		if lowBase != "" && squash(it.Base) == lowBase {
			score += 2
			shares = append(shares, "base: "+d.Base)
		}
		sharedTags := 0
		for _, t := range designTags {
			if hasTag(it.Tags, t) {
				score += 1
				sharedTags++
				if sharedTags <= 2 {
					shares = append(shares, t)
				}
			}
		}
		m := MetricsOfItem(it)
		score -= math.Abs(float64(m.Bonus - target.Bonus))
		score -= math.Min(3, math.Abs(float64(m.Charges-target.Charges))/10)
		if (m.Charges > 0) != (target.Charges > 0) {
			score -= 1
		}
		score -= math.Min(3, math.Abs(float64(m.SaveDC-target.SaveDC))/5)
		out = append(out, ranked{it: it, score: score, shares: shares})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		return out[i].it.Name < out[j].it.Name
	})
	if len(out) > n {
		out = out[:n]
	}
	neighbours := make([]Neighbour, 0, len(out))
	for _, r := range out {
		neighbours = append(neighbours, Neighbour{
			Name: r.it.Name, Rarity: r.it.Rarity, Type: r.it.Type,
			Tags: r.it.Tags, Homebrew: r.it.Homebrew, Shares: r.shares,
		})
	}
	return neighbours
}

// designTagsOf derives the design's tags through the same rules the
// corpus items go through, so a design and its neighbours are comparable
// in one vocabulary.
func designTagsOf(d Design) []string {
	return deriveTags(d.Item("designed"))
}

// Item renders the design in the catalog's read shape. Doc labels the
// origin ("homebrew" for a designed row) and the caller tags it, so a
// designed item is never presented as SRD; the rendered text is the
// printed-style statement of exactly what the structured fields say.
func (d Design) Item(doc string) Item {
	it := Item{
		Name: strings.TrimSpace(d.Name), Doc: doc,
		Type: d.Type, Rarity: strings.TrimSpace(d.Rarity),
		RequiresAttunement:  d.Attunement.Required,
		AttunementCondition: d.Attunement.Condition,
		Base:                d.Base, Charges: d.Charges, Recharge: d.Recharge,
		Text: d.renderText(),
	}
	it.Tags = deriveTags(it)
	return it
}

// renderText prints the design as the SRD prints an item: each mechanical
// effect in the game's own words, the charge grammar, the attunement
// line, then the designer's prose.
func (d Design) renderText() string {
	var b strings.Builder
	if d.Bonus > 0 && (d.Type == "weapon" || d.Type == "armor") {
		if d.Type == "weapon" {
			fmt.Fprintf(&b, "You have a +%d bonus to attack and damage rolls made with this weapon.\n", d.Bonus)
		} else {
			fmt.Fprintf(&b, "You have a +%d bonus to Armor Class while wearing this armor.\n", d.Bonus)
		}
	}
	if d.Attunement.Required {
		if cond := strings.TrimSpace(d.Attunement.Condition); cond != "" {
			if !strings.HasPrefix(cond, "by ") {
				cond = "by " + cond
			}
			b.WriteString("Requires attunement " + cond + ".\n")
		} else {
			b.WriteString("Requires attunement.\n")
		}
	}
	if d.Charges > 0 {
		fmt.Fprintf(&b, "The item has %d charges.", d.Charges)
		if d.Recharge != "" {
			fmt.Fprintf(&b, " It regains %s expended charges.", d.Recharge)
		}
		b.WriteString("\n")
	}
	for _, e := range d.Effects {
		if line := strings.TrimSpace(e.Text); line != "" {
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
	if d.Text != "" {
		b.WriteString(strings.TrimSpace(d.Text))
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}
