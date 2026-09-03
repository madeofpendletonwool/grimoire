package encounter

// The tactical analysis (MAD-381): what these monsters will actually do, and
// to whom. Everything before this file answered "is this fight too hard";
// this answers the question a DM asks at 6pm — which pc the pack's output
// lands on, how long each side takes to drop the other, what answers the
// party does and does not have, and whether the ground makes movement matter.
//
// It is arithmetic first, and pure like the rest of the package: no database,
// no network, no clock, no randomness. Identical input produces an identical
// analysis, across restarts, because nothing here reads anything but the
// values it was handed.
//
// Two disciplines run through the whole file:
//
//   - Every number that leaves as a Figure carries its derivation in How —
//     the trace-on-click spirit of the difficulty gauge. A test walks the
//     output and refuses any figure without one, and the prose gate at the
//     bottom refuses any model-written number that is not one of these.
//   - The analysis never invents what the party block does not declare. A pc
//     with no armour class cannot be aimed at and the readout says so; a
//     party that declares nothing degrades the whole analysis to
//     statblock-only output with a caveat, rather than a fictional party to
//     point at.

import (
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/statblock"
)

/* ---------- inputs ---------- */

// Figure is one number the analysis surfaces, with the derivation that
// produced it. Value is the number, Unit says what it is measured in, and How
// is the arithmetic, spelled out for the trace-on-click view. No analysis
// number travels any other way.
type Figure struct {
	Value float64 `json:"value"`
	Unit  string  `json:"unit,omitempty"`
	How   string  `json:"how"`
}

// fig builds a whole-number figure.
func fig(value int, unit, how string) Figure {
	return Figure{Value: float64(value), Unit: unit, How: how}
}

// figf builds a fractional figure, rounded to one decimal for display.
func figf(value float64, unit, how string) Figure {
	return Figure{Value: math.Round(value*10) / 10, Unit: unit, How: how}
}

// figp builds a percentage figure from a probability.
func figp(p float64, how string) Figure {
	return fig(chancePct(p), "%", how)
}

// PCFact is one pc as the analysis reads it: whatever the party block
// declares, plus where they stand. Every field is optional; a pc that
// declares nothing simply cannot be aimed at, dropped, or benched, and the
// caveats say which.
//
// Distance is feet from the monsters — 0 means engaged, which is how a
// settled 5e melee actually sits. The builder's live read passes nothing and
// every pc counts as engaged; the field is here so reach is arithmetically
// honest when a caller knows better.
type PCFact struct {
	Name     string         `json:"name"`
	Class    string         `json:"class,omitempty"`
	Level    int            `json:"level,omitempty"`
	AC       int            `json:"ac,omitempty"`
	MaxHP    int            `json:"max_hp,omitempty"`
	Saves    map[string]int `json:"saves,omitempty"` // "str"..."cha" -> bonus
	Resists  []string       `json:"resists,omitempty"`
	Distance int            `json:"distance,omitempty"` // feet from the monsters; 0 = engaged
}

// Combatant is one roster kind: a statblock the catalog vouched for, and how
// many of it are on the board.
type Combatant struct {
	Creature Creature `json:"creature"`
	Count    int      `json:"count"`
}

// TacticsInput is everything one analysis reads. Waves carries a survive
// objective's wave count so the read can say the pressure arrives over
// rounds rather than all at once; it changes no arithmetic on the board.
type TacticsInput struct {
	Party   []PCFact
	Roster  []Combatant
	Terrain *Terrain
	Waves   int
}

/* ---------- output ---------- */

// Analysis is the whole tactical read. Sections are independent: a party
// that declares only saves still gets save-based aiming and counterplay, and
// a section that cannot be computed is absent rather than padded.
type Analysis struct {
	// PartyKnown reports that at least one pc declared something usable.
	// When it is false the analysis is statblock-only and the caveat says so.
	PartyKnown bool     `json:"party_known"`
	Caveats    []string `json:"caveats,omitempty"`

	Threat      []GroupThreat `json:"threat,omitempty"`
	Focus       []Focus       `json:"focus,omitempty"`
	Attrition   Attrition     `json:"attrition,omitempty"`
	Counterplay []Counterplay `json:"counterplay,omitempty"`
	Spotlight   []PCSpotlight `json:"spotlight,omitempty"`
	Movement    *Movement     `json:"movement,omitempty"`
}

// GroupThreat is one roster kind's action economy: what it can do in a
// round, before anyone's armour or saves blunt it.
type GroupThreat struct {
	Name string `json:"name"`
	// Count is how many of them the roster fields.
	Attacks  Figure `json:"attacks"`   // uses of the priced round per monster
	Damage   Figure `json:"damage"`    // average the round deals to one target
	PerRound Figure `json:"per_round"` // Count × Attacks × Damage
	// ToHit or SaveDC+SaveAbility says how the round's main effect lands.
	// Exactly one of the two is set.
	ToHit       *Figure `json:"to_hit,omitempty"`
	SaveDC      *Figure `json:"save_dc,omitempty"`
	SaveAbility string  `json:"save_ability,omitempty"`
	// Notes carry the round's disclosed approximations: legendary-action
	// spending, recharge effects amortised over the fight, and the like.
	Notes []string `json:"notes,omitempty"`
}

// Focus is one roster kind's predicted target: the pc its arithmetic wants to
// kill first, or the reason it cannot pick one.
type Focus struct {
	Monster string `json:"monster"`
	// Target is the pc's name, empty when nothing is aimable — Holdouts then
	// says why each pc was passed over.
	Target   string   `json:"target"`
	Reason   string   `json:"reason,omitempty"`
	Mode     string   `json:"mode,omitempty"`      // "attack" or "save"
	Chance   *Figure  `json:"chance,omitempty"`    // to-hit, or save-failure, chance
	PerRound *Figure  `json:"per_round,omitempty"` // expected damage aimed at the target
	Holdouts []string `json:"holdouts,omitempty"`
}

// Attrition is the two-sided clock: how long the encounter takes to put each
// pc at zero, and how long the party takes to put each monster kind there.
// The asymmetry is the encounter's real difficulty in a way adjusted XP
// never shows.
type Attrition struct {
	Party    []PCDrop      `json:"party,omitempty"`
	Monsters []MonsterDrop `json:"monsters,omitempty"`
}

// PCDrop is one pc under the encounter's focused fire.
type PCDrop struct {
	PC      string `json:"pc"`
	AimedAt Figure `json:"aimed_at"`
	Rounds  Figure `json:"rounds"`
}

// MonsterDrop is one roster kind under the party's whole answer. Rounds is
// nil when nothing the party brings touches it — Hopeless says why.
type MonsterDrop struct {
	Monster  string  `json:"monster"`
	HP       Figure  `json:"hp"`
	Answer   Figure  `json:"answer_per_round"`
	Rounds   *Figure `json:"rounds,omitempty"`
	Hopeless string  `json:"hopeless,omitempty"`
}

// Counterplay is one answer-shaped fact: an immunity that blanks a pc, a
// flyer the melee cannot touch, a save the soft pc keeps failing.
type Counterplay struct {
	Monster string `json:"monster,omitempty"`
	PC      string `json:"pc,omitempty"`
	Detail  string `json:"detail"`
}

// PCSpotlight is the roadmap's two questions made checkable: how much of the
// encounter's threat is aimed at this pc, and how much of the party's answer
// they supply. A pc low on both axes is Benched, with the specific reason.
type PCSpotlight struct {
	PC          string  `json:"pc"`
	ThreatShare *Figure `json:"threat_share,omitempty"`
	AnswerShare *Figure `json:"answer_share,omitempty"`
	Benched     bool    `json:"benched,omitempty"`
	Reason      string  `json:"reason,omitempty"`
}

// Movement is the "an encounter where movement matters" property, as a
// 0–100 score built from the terrain, the ranged/melee mix, speeds and
// reach — each driver listed with its points.
type Movement struct {
	Score   Figure   `json:"score"`
	Label   string   `json:"label"` // "low", "some" or "high"
	Drivers []string `json:"drivers,omitempty"`
}

// spotlightFloor is the share under which both axes count as benched. Ten
// percent of a four-pc party is under half one character's fair share.
const spotlightFloor = 0.10

// amortiseRounds spreads a limited-use effect (a recharge, a per-day power)
// over the expected length of a fight, so a dragon's breath prices into its
// average round without pretending it happens every round.
const amortiseRounds = 4

/* ---------- the analysis ---------- */

// Analyze computes the tactical read for a roster against a party. It is
// total: any input produces an analysis, and identical input produces an
// identical one.
func Analyze(in TacticsInput) *Analysis {
	a := &Analysis{}

	for _, pc := range in.Party {
		if pc.AC > 0 || pc.MaxHP > 0 || len(pc.Saves) > 0 || pc.Class != "" {
			a.PartyKnown = true
			break
		}
	}
	if !a.PartyKnown {
		a.Caveats = append(a.Caveats,
			"no usable party block — this is the monsters' side of the arithmetic only; a campaign whose pcs declare armour, hits, saves or a class fills it in")
	}
	if len(in.Roster) == 0 {
		a.Caveats = append(a.Caveats, "no monsters on the board")
		return a
	}

	groups := make([]pricedGroup, 0, len(in.Roster))
	unparsed := 0
	for _, g := range in.Roster {
		if g.Count < 1 {
			continue
		}
		groups = append(groups, priceGroup(g))
		sb := g.Creature.Statblock()
		for _, act := range sb.Actions {
			if !act.Parsed {
				unparsed++
			}
		}
	}
	if unparsed > 0 {
		a.Caveats = append(a.Caveats, fmt.Sprintf(
			"%d actions across the roster could not be parsed and are not priced — their prose is intact on the statblocks", unparsed))
	}
	if len(groups) == 0 {
		a.Caveats = append(a.Caveats, "no monsters on the board")
		return a
	}
	if in.Waves > 1 {
		a.Caveats = append(a.Caveats, fmt.Sprintf(
			"the roster arrives in %d waves — the per-round reads price the whole roster as one board, so the pressure builds rather than lands at once", in.Waves))
	}

	party := usableParty(in.Party)

	/* threat: the action economy */
	for _, p := range groups {
		a.Threat = append(a.Threat, threatOf(p))
	}

	/* focus: who gets hit */
	a.Focus = focusFor(groups, party)
	appendFocusCaveats(a, party)

	/* attrition: the two clocks */
	a.Attrition = attritionOf(a, groups, party, a.Focus)

	/* counterplay: which answer blanks which monster */
	a.Counterplay = counterplay(groups, party)

	/* spotlight: the two shares per pc */
	if a.PartyKnown {
		a.Spotlight = spotlight(groups, party, a.Focus)
	}

	/* movement: does the ground matter */
	a.Movement = movementScore(in, groups)

	return a
}

// usableParty returns the pcs the arithmetic can aim at with something: a
// named pc that declared any number. The order is the input's order, which
// the server keeps in name order, so ties resolve deterministically.
func usableParty(party []PCFact) []PCFact {
	var out []PCFact
	for _, pc := range party {
		if pc.Name != "" && (pc.AC > 0 || pc.MaxHP > 0 || len(pc.Saves) > 0 || pc.Class != "") {
			out = append(out, pc)
		}
	}
	return out
}

/* ---------- pricing one roster kind's round ---------- */

// pricedGroup is one roster kind with its round already priced.
type pricedGroup struct {
	g         Combatant
	round     roundPlan
	speed     int  // best board speed: walk or fly
	hasRanged bool // any parsed effect that reaches without closing
}

// priceGroup prices a creature's round and its reach.
func priceGroup(g Combatant) pricedGroup {
	sb := g.Creature.Statblock()
	p := pricedGroup{g: g, round: planRound(sb)}
	p.speed = int(math.Max(float64(sb.Speeds["walk"]), float64(sb.Speeds["fly"])))
	for _, sa := range sb.Actions {
		if !sa.Parsed || !sa.Attack.Parsed() {
			continue
		}
		atk := sa.Attack
		if atk.Kind == statblock.KindRanged || atk.Kind == statblock.KindMeleeOrRanged ||
			atk.Range >= 30 || atk.Area ||
			(atk.SaveDC > 0 && atk.Range > 0) {
			p.hasRanged = true
			break
		}
	}
	return p
}

// roundPlan is one roster kind's average round: how many uses of what, at
// what to-hit or save DC, dealing how much to a single target.
type roundPlan struct {
	uses    float64 // attack uses per round
	perUse  int     // average damage to one target per use
	perRd   int     // uses × perUse, before armour and saves
	dom     domAttack
	notes   []string
	limited bool // the whole round rests on a once-per-fight effect
}

// domAttack is the round's main effect — the one whose to-hit or save the
// aiming arithmetic reads.
type domAttack struct {
	name    string
	kind    string
	toHit   int
	saveDC  int
	saveAb  string
	reach   int
	rng     int
	area    bool
	dmgType string
	rider   string
}

// planRound prices a creature's round from its parsed actions:
//
//   - a parsed multiattack is the round — its components at the counts the
//     prose names, choices priced at the strongest option (which is what the
//     creature would actually do);
//   - otherwise the best every-round attack;
//   - a recharge or per-day effect on top adds one use amortised over the
//     fight's expected length;
//   - a legendary creature spends its budget on extra attacks, three
//     legendary actions' worth per round.
//
// Everything approximate is said in Notes, never silently assumed.
func planRound(sb statblock.Statblock) roundPlan {
	var p roundPlan

	byName := map[string]statblock.Attack{}
	for _, sa := range sb.Actions {
		if sa.Parsed && sa.Attack.Parsed() && sa.Attack.Kind != statblock.KindMultiattack {
			byName[squash(sa.Name)] = sa.Attack
		}
	}
	bestOf := func(names []string) (statblock.Attack, bool) {
		var best statblock.Attack
		found := false
		for _, n := range names {
			atk, ok := byName[squash(n)]
			if !ok {
				continue
			}
			if !found || atk.Avg(false) > best.Avg(false) {
				best, found = atk, true
			}
		}
		return best, found
	}
	isLimited := func(sa statblock.Action) bool {
		u := strings.ToLower(strings.TrimSpace(sa.Usage))
		return u != "" && u != "at will"
	}

	var every []statblock.Attack
	var limitedAtk []statblock.Attack
	var legendary []statblock.Attack
	legendaryCost := map[string]int{}
	for _, sa := range sb.Actions {
		if !sa.Parsed || !sa.Attack.Parsed() {
			continue
		}
		switch {
		case sa.Attack.Kind == statblock.KindMultiattack:
			// read below, from the components
		case sa.Legendary():
			legendary = append(legendary, sa.Attack)
			cost := sa.Cost
			if cost < 1 {
				cost = 1
			}
			legendaryCost[squash(sa.Name)] = cost
		case isLimited(sa):
			limitedAtk = append(limitedAtk, sa.Attack)
		default:
			every = append(every, sa.Attack)
		}
	}

	// The multiattack, when the parse found one with resolvable components.
	for _, sa := range sb.Actions {
		if !sa.Parsed || sa.Attack.Kind != statblock.KindMultiattack || len(sa.Attack.Components) == 0 {
			continue
		}
		uses, total := 0.0, 0
		var dom domAttack
		domScore := -1
		complete := true
		for _, comp := range sa.Attack.Components {
			var atk statblock.Attack
			var ok bool
			if len(comp.Options) > 0 {
				atk, ok = bestOf(comp.Options)
			} else {
				atk, ok = byName[squash(comp.Name)]
			}
			if !ok {
				complete = false
				continue
			}
			n := comp.Count
			if n < 1 {
				n = 1
			}
			uses += float64(n)
			total += n * atk.Avg(false)
			if score := n * atk.Avg(false); score > domScore {
				domScore = score
				dom = domFrom(atk)
			}
		}
		if complete && uses > 0 && total > 0 {
			p.uses = uses
			p.perUse = int(math.Round(float64(total) / uses))
			p.perRd = total
			p.dom = dom
			break
		}
	}

	// No usable multiattack: the best single every-round attack.
	if p.perRd == 0 && len(every) > 0 {
		best := every[0]
		for _, atk := range every[1:] {
			if atk.Avg(false) > best.Avg(false) {
				best = atk
			}
		}
		if best.Avg(false) > 0 {
			p.uses = 1
			p.perUse = best.Avg(false)
			p.perRd = best.Avg(false)
			p.dom = domFrom(best)
		}
	}

	// Still nothing every round: a once-per-fight swing is the honest
	// answer, flagged as limited rather than amortised into fiction.
	if p.perRd == 0 && len(limitedAtk) > 0 {
		best := limitedAtk[0]
		for _, atk := range limitedAtk[1:] {
			if atk.Avg(false) > best.Avg(false) {
				best = atk
			}
		}
		if best.Avg(false) > 0 {
			p.uses = 1
			p.perUse = best.Avg(false)
			p.perRd = best.Avg(false)
			p.dom = domFrom(best)
			p.limited = true
			p.notes = append(p.notes, fmt.Sprintf(
				"the whole round rests on %s, which the statblock rations — treat this as its big turn, not every turn", best.Name))
		}
	}

	// A limited-use effect on top of a sustainable round: one use spread
	// over the fight's expected length.
	if p.perRd > 0 && !p.limited {
		for _, atk := range limitedAtk {
			if atk.Avg(false) <= p.perRd {
				continue // only the ones worth waiting for
			}
			share := int(math.Round(float64(atk.Avg(false)) / amortiseRounds))
			if share > 0 {
				p.perRd += share
				p.notes = append(p.notes, fmt.Sprintf(
					"%s adds about %d per round — one use spread over the %d rounds a fight is expected to last",
					atk.Name, share, amortiseRounds))
			}
		}
	}

	// Legendary actions: three actions' worth of extra attacks per round.
	if len(legendary) > 0 {
		var bestAtk statblock.Attack
		found := false
		minCost := math.MaxInt
		for _, atk := range legendary {
			cost := legendaryCost[squash(atk.Name)]
			if cost < minCost {
				minCost = cost
			}
			if !found || atk.Avg(false) > bestAtk.Avg(false) {
				bestAtk, found = atk, true
			}
		}
		if found && bestAtk.Avg(false) > 0 {
			cost := legendaryCost[squash(bestAtk.Name)]
			if cost < 1 {
				cost = 1
			}
			if extra := 3 / cost; extra > 0 {
				p.perRd += extra * bestAtk.Avg(false)
				p.uses += float64(extra)
				p.perUse = int(math.Round(float64(p.perRd) / p.uses))
				if p.dom.name == "" {
					p.dom = domFrom(bestAtk)
				}
				p.notes = append(p.notes, fmt.Sprintf(
					"%d of the 3 legendary actions per round buy %d extra %s — %d more damage",
					cost, extra, bestAtk.Name, extra*bestAtk.Avg(false)))
			}
		}
	}

	return p
}

// domFrom copies the aiming-relevant half of a parsed attack.
func domFrom(atk statblock.Attack) domAttack {
	d := domAttack{
		name:   atk.Name,
		kind:   atk.Kind,
		toHit:  atk.ToHit,
		saveDC: atk.SaveDC,
		saveAb: strings.ToLower(atk.SaveAbility),
		reach:  atk.Reach,
		rng:    atk.Range,
		area:   atk.Area,
		rider:  strings.TrimSpace(atk.Rider),
	}
	if len(atk.Damage) > 0 {
		d.dmgType = strings.ToLower(atk.Damage[0].Type)
	}
	return d
}

// threatOf renders one group's round as the action-economy row.
func threatOf(p pricedGroup) GroupThreat {
	name := p.g.Creature.Name
	count := p.g.Count
	notes := p.round.notes
	nothing := p.round.perRd == 0 && p.round.uses == 0
	if nothing && len(notes) == 0 {
		notes = []string{"no parsed damage — nothing on this statblock's sheet prices"}
	}
	t := GroupThreat{
		Name:    name,
		Notes:   notes,
		Attacks: figf(p.round.uses, "uses", fmt.Sprintf("uses of %s per round, from the parsed actions", attackName(p.round))),
		Damage:  fig(p.round.perUse, "avg damage", fmt.Sprintf("average damage of %s to one target", attackName(p.round))),
	}
	if !nothing {
		t.PerRound = fig(p.round.perRd*count, "damage per round",
			fmt.Sprintf("%d × %.1f uses × %d damage, before anyone's armour or saves", count, p.round.uses, p.round.perUse))
		if p.round.dom.saveDC > 0 {
			dc := fig(p.round.dom.saveDC, "save DC", fmt.Sprintf("%s's save DC, from the parsed action", attackName(p.round)))
			t.SaveDC = &dc
			t.SaveAbility = strings.ToUpper(p.round.dom.saveAb)
		} else {
			th := fig(p.round.dom.toHit, "to hit", fmt.Sprintf("%s's attack bonus, from the parsed action", attackName(p.round)))
			t.ToHit = &th
		}
	}
	return t
}

func attackName(p roundPlan) string {
	if p.dom.name != "" {
		return p.dom.name
	}
	return "its actions"
}

/* ---------- the dice ---------- */

// hitChance is the d20 chance an attack bonus hits an armour class, floored
// at 5% and capped at 95% the way the game treats natural 1s and 20s.
// Critical hits are not priced separately.
func hitChance(toHit, ac int) float64 {
	return clampP(float64(21+toHit-ac) / 20.0)
}

// failChance is the d20 chance a save of the given bonus fails a DC.
func failChance(dc, save int) float64 {
	return clampP(float64(dc-save-1) / 20.0)
}

func clampP(p float64) float64 {
	if p < 0.05 {
		return 0.05
	}
	if p > 0.95 {
		return 0.95
	}
	return p
}

func chancePct(p float64) int { return int(math.Round(p * 100)) }

/* ---------- focus: who gets hit ---------- */

// focusFor picks each group's target: the softest pc its main effect can
// reach, scored on expected damage per round against what the pc declared —
// per hit point when the whole party declares hit points, raw expected
// damage when it does not. Ties fall to the lower armour class, then to name
// order, so the same party always sees the same prediction.
func focusFor(groups []pricedGroup, party []PCFact) []Focus {
	var out []Focus
	for _, p := range groups {
		out = append(out, focusOne(p, party))
	}
	return out
}

// appendFocusCaveats says, once, the party-wide gaps the aiming pass ran
// into — a pc no attack can aim at is a property of the party block, not of
// any one monster.
func appendFocusCaveats(a *Analysis, party []PCFact) {
	noAC := 0
	for _, pc := range party {
		if pc.AC <= 0 {
			noAC++
		}
	}
	if noAC == len(party) && len(party) > 0 {
		a.Caveats = append(a.Caveats, "no pc declares an armour class — attack-based aiming is blind")
	} else if noAC > 0 {
		a.Caveats = append(a.Caveats, fmt.Sprintf(
			"%d of the party declare no armour class — attack-based monsters cannot aim at them", noAC))
	}
}

// targetCand is one pc under consideration for a group's focus.
type targetCand struct {
	pc       PCFact
	chance   float64
	expected float64
	holdout  string // why the pc was passed over, when they were
}

// score is a candidate's vulnerability: expected damage per round, per hit
// point when the whole party declared hit points.
func (c targetCand) score(allHP bool) float64 {
	if allHP && c.pc.MaxHP > 0 {
		return c.expected / float64(c.pc.MaxHP)
	}
	return c.expected
}

// betterTarget reports whether a beats b under the focus rules: higher
// vulnerability, then lower armour class, then name order.
func betterTarget(a, b targetCand, allHP bool) bool {
	as, bs := a.score(allHP), b.score(allHP)
	if as != bs {
		return as > bs
	}
	if a.pc.AC > 0 && b.pc.AC > 0 && a.pc.AC != b.pc.AC {
		return a.pc.AC < b.pc.AC
	}
	return a.pc.Name < b.pc.Name
}

func focusOne(p pricedGroup, party []PCFact) Focus {
	name := p.g.Creature.Name
	f := Focus{Monster: name}
	if p.round.perRd == 0 {
		f.Reason = "no parsed damage — nothing to aim"
		return f
	}
	saveMode := p.round.dom.saveDC > 0
	if saveMode {
		f.Mode = "save"
	} else {
		f.Mode = "attack"
	}

	cands := make([]targetCand, 0, len(party))
	allHP := len(party) > 0
	for _, pc := range party {
		c := targetCand{pc: pc}
		if saveMode {
			ab := p.round.dom.saveAb
			save, ok := pc.Saves[ab]
			if !ok {
				if ab == "" {
					c.holdout = "the save it tests is not parsed"
				} else {
					c.holdout = fmt.Sprintf("no %s save declared", strings.ToUpper(ab))
				}
			} else {
				c.chance = failChance(p.round.dom.saveDC, save)
				if !saveReaches(p.round.dom, pc) {
					c.holdout = fmt.Sprintf("%d ft. away — beyond the effect's reach", pc.Distance)
				}
			}
		} else {
			if pc.AC <= 0 {
				c.holdout = "no armour class declared"
			} else {
				c.chance = hitChance(p.round.dom.toHit, pc.AC)
				if ok, why := attackReaches(p, pc); !ok {
					c.holdout = why
				}
			}
		}
		if pc.MaxHP <= 0 {
			allHP = false
		}
		if c.holdout == "" {
			c.expected = float64(p.round.perRd) * float64(p.g.Count) * c.chance
			// The pc's own resistances blunt the monster's main damage type.
			if mult, resisted := resistMult(pc.Resists, p.round.dom.dmgType); resisted {
				c.expected *= mult
			}
		}
		cands = append(cands, c)
	}
	if len(cands) == 0 {
		f.Reason = "the party block declares nothing this attack can test"
		return f
	}

	var best *targetCand
	for i := range cands {
		c := &cands[i]
		if c.holdout != "" {
			f.Holdouts = append(f.Holdouts, fmt.Sprintf("%s: %s", c.pc.Name, c.holdout))
			continue
		}
		if best == nil || betterTarget(*c, *best, allHP) {
			best = c
		}
	}
	if best == nil {
		f.Reason = "nothing is aimable — " + strings.Join(f.Holdouts, "; ")
		return f
	}

	f.Target = best.pc.Name
	chance := figp(best.chance, chanceHow(p, best.pc))
	f.Chance = &chance
	per := figf(best.expected, "damage per round", fmt.Sprintf(
		"%d on the board × %d damage in the round × the %s chance above%s",
		p.g.Count, p.round.perRd, modeWord(f.Mode), resistNote(pcResisted(p, best.pc))))
	f.PerRound = &per

	vuln := "expected damage"
	if allHP && best.pc.MaxHP > 0 {
		vuln = fmt.Sprintf("expected damage against %d max hp", best.pc.MaxHP)
	}
	f.Reason = fmt.Sprintf("the softest target its %s can reach — highest %s among the party it can get at",
		attackName(p.round), vuln)
	return f
}

func chanceHow(p pricedGroup, pc PCFact) string {
	if p.round.dom.saveDC > 0 {
		save := pc.Saves[p.round.dom.saveAb]
		return fmt.Sprintf("d20 maths: the %s save (%+d declared) against DC %d — a roll of %d or better holds",
			strings.ToUpper(p.round.dom.saveAb), save, p.round.dom.saveDC, p.round.dom.saveDC-save)
	}
	return fmt.Sprintf("d20 maths: attack %+d against AC %d needs a %d or better; natural 1 and 20 set the 5%% floor and cap",
		p.round.dom.toHit, pc.AC, pc.AC-p.round.dom.toHit)
}

func modeWord(mode string) string {
	if mode == "save" {
		return "save-failure"
	}
	return "to-hit"
}

func pcResisted(p pricedGroup, pc PCFact) bool {
	_, ok := resistMult(pc.Resists, p.round.dom.dmgType)
	return ok
}

func resistNote(resisted bool) string {
	if resisted {
		return ", halved by the pc's resistance to its damage type"
	}
	return ""
}

// resistMult reports how much a pc's declared resistances blunt a damage
// type: half when resisted, unchanged (1, false) otherwise.
func resistMult(resists []string, dmgType string) (float64, bool) {
	if dmgType == "" || len(resists) == 0 {
		return 1, false
	}
	for _, group := range resists {
		if strings.Contains(strings.ToLower(group), dmgType) {
			return 0.5, true
		}
	}
	return 1, false
}

// attackReaches reports whether a group's attack gets at a pc standing
// pc.Distance feet away: a melee attack closes on its speed, a ranged attack
// reaches on range. A pc the group cannot get at is a holdout, not a target.
func attackReaches(p pricedGroup, pc PCFact) (bool, string) {
	dom := p.round.dom
	if pc.Distance <= 0 {
		return true, ""
	}
	if dom.rng > 0 || dom.kind == statblock.KindRanged || dom.kind == statblock.KindMeleeOrRanged {
		span := int(math.Max(float64(dom.reach), float64(dom.rng)))
		if pc.Distance <= span+p.speed {
			return true, ""
		}
		return false, fmt.Sprintf("%d ft. away — beyond its %d ft. of reach and %d ft. of speed",
			pc.Distance, span, p.speed)
	}
	if pc.Distance <= dom.reach+p.speed {
		return true, ""
	}
	return false, fmt.Sprintf("%d ft. away — a %d ft. reach and %d ft. of speed never closes",
		pc.Distance, dom.reach, p.speed)
}

// saveReaches applies the same test to a save effect. An effect whose range
// the parser could not read is treated as reaching — the common case for
// area saves — and never silently the other way.
func saveReaches(dom domAttack, pc PCFact) bool {
	if pc.Distance <= 0 {
		return true
	}
	if dom.rng > 0 {
		return pc.Distance <= dom.rng
	}
	return true
}

/* ---------- the party's answer ---------- */

// answerOf prices one pc's contribution against one roster kind: the
// canonical SRD output for their class and level, adjusted by what the
// monster is immune or resistant to and whether the pc can get at it. The
// note says which adjustment, if any, applied.
func answerOf(pc PCFact, p pricedGroup) (dpr int, note string) {
	base, dmg, how, ok := baselineFor(pc.Class, pc.Level)
	if !ok {
		return 0, "no class or level declared — nothing to price their answer with"
	}
	if matchesDamage(p.g.Creature.Immune, dmg) {
		return 0, fmt.Sprintf("immune to %s — their whole baseline (%s) does nothing", dmg, how)
	}
	if matchesDamage(p.g.Creature.Resist, dmg) {
		base = int(math.Round(float64(base) / 2))
		note = fmt.Sprintf("resistant to %s — their baseline halves (%s)", dmg, how)
	}
	// A flyer that never closes takes the melee off the board against it.
	if base > 0 && dmg == "weapon" && p.g.Creature.Speeds["fly"] > 0 && p.hasRanged {
		return 0, fmt.Sprintf("stays airborne out of melee reach — their baseline is melee (%s)", how)
	}
	// A pc the board never closes on cannot spend a melee baseline.
	if base > 0 && dmg == "weapon" && pc.Distance > p.speed+p.round.dom.reach {
		return 0, fmt.Sprintf("%d ft. away and it never closes — their baseline is melee (%s)", pc.Distance, how)
	}
	return base, note
}

// matchesDamage reports whether a mirror damage display ("bludgeoning,
// piercing, and slashing from nonmagical attacks; cold") covers a baseline
// damage type. "weapon" covers the bludgeoning/piercing/slashing group — the
// nonmagical clause included, which is the honest reading for a party whose
// items list has not promised magic weapons.
func matchesDamage(display, dmg string) bool {
	if display == "" || dmg == "" {
		return false
	}
	for _, group := range damageTypes(display) {
		g := strings.ToLower(group)
		if dmg == "weapon" {
			if strings.Contains(g, "bludgeoning") && strings.Contains(g, "piercing") && strings.Contains(g, "slashing") {
				return true
			}
			continue
		}
		if strings.Contains(g, dmg) {
			return true
		}
	}
	return false
}

/* ---------- attrition: the two clocks ---------- */

func attritionOf(a *Analysis, groups []pricedGroup, party []PCFact, focus []Focus) Attrition {
	var at Attrition

	// Party side: the expected damage the focused picks put on each pc, and
	// how long that takes to empty them.
	aimed := map[string]float64{}
	for _, f := range focus {
		if f.Target != "" && f.PerRound != nil {
			aimed[f.Target] += f.PerRound.Value
		}
	}
	noHP := 0
	for _, pc := range party {
		d := aimed[pc.Name]
		if d <= 0 {
			continue
		}
		if pc.MaxHP <= 0 {
			noHP++
			continue
		}
		at.Party = append(at.Party, PCDrop{
			PC:      pc.Name,
			AimedAt: figf(d, "damage per round", "the focused output of every monster whose arithmetic wants them down first"),
			Rounds: figf(float64(pc.MaxHP)/d, "rounds",
				fmt.Sprintf("%d max hp ÷ %.1f expected damage per round", pc.MaxHP, d)),
		})
	}
	if noHP > 0 {
		a.Caveats = append(a.Caveats, fmt.Sprintf(
			"%d pc the focus aims at declares no max hp — how long they last cannot be priced", noHP))
	}

	// Monster side: the party's whole answer against each kind's pool of
	// hit points.
	for _, p := range groups {
		hp := p.g.Creature.HP * p.g.Count
		if hp <= 0 {
			continue
		}
		drop := MonsterDrop{
			Monster: p.g.Creature.Name,
			HP:      fig(hp, "hp", fmt.Sprintf("%d hp each × %d", p.g.Creature.HP, p.g.Count)),
		}
		total, notes := 0, []string{}
		for _, pc := range party {
			d, note := answerOf(pc, p)
			if note != "" {
				notes = append(notes, fmt.Sprintf("%s: %s", pc.Name, note))
			}
			total += d
		}
		drop.Answer = fig(total, "damage per round", fmt.Sprintf(
			"the party's class baselines summed, each adjusted by what %s is immune or resistant to and who can get at it", p.g.Creature.Name))
		if total <= 0 {
			drop.Hopeless = strings.Join(notes, "; ")
			if drop.Hopeless == "" {
				drop.Hopeless = "the party declares nothing that prices against it"
			}
		} else {
			r := figf(float64(hp)/float64(total), "rounds",
				fmt.Sprintf("%d hp ÷ %d expected damage per round from the party's baselines", hp, total))
			drop.Rounds = &r
		}
		at.Monsters = append(at.Monsters, drop)
	}
	return at
}

/* ---------- counterplay ---------- */

func counterplay(groups []pricedGroup, party []PCFact) []Counterplay {
	var out []Counterplay
	for _, p := range groups {
		name := p.g.Creature.Name
		// The save-or-suck line: the pc likeliest to fail it, and what the
		// rider does when they do.
		if p.round.dom.saveDC > 0 && p.round.dom.saveAb != "" && len(party) > 0 {
			worst, worstPC := 0.0, ""
			for _, pc := range party {
				save, ok := pc.Saves[p.round.dom.saveAb]
				if !ok {
					continue
				}
				if f := failChance(p.round.dom.saveDC, save); f > worst {
					worst, worstPC = f, pc.Name
				}
			}
			if worstPC != "" && worst >= 0.5 {
				detail := fmt.Sprintf("its %s tests a %s save at DC %d — %s fails %d%% of the time",
					attackName(p.round), strings.ToUpper(p.round.dom.saveAb), p.round.dom.saveDC, worstPC, chancePct(worst))
				if p.round.dom.rider != "" {
					detail += " — on a failure: " + p.round.dom.rider
				}
				out = append(out, Counterplay{Monster: name, PC: worstPC, Detail: detail})
			}
		}
		for _, pc := range party {
			d, note := answerOf(pc, p)
			if d == 0 && note != "" {
				out = append(out, Counterplay{Monster: name, PC: pc.Name, Detail: note})
			}
		}
	}
	return out
}

/* ---------- spotlight: the two shares ---------- */

func spotlight(groups []pricedGroup, party []PCFact, focus []Focus) []PCSpotlight {
	if len(party) < 2 {
		return nil // benching a lone pc says nothing
	}
	aimed := map[string]float64{}
	totalAimed := 0.0
	for _, f := range focus {
		if f.Target != "" && f.PerRound != nil {
			aimed[f.Target] += f.PerRound.Value
			totalAimed += f.PerRound.Value
		}
	}
	answer := map[string]float64{}
	totalAnswer := 0
	for _, pc := range party {
		for _, p := range groups {
			d, _ := answerOf(pc, p)
			answer[pc.Name] += float64(d)
			totalAnswer += d
		}
	}

	out := make([]PCSpotlight, 0, len(party))
	for _, pc := range party {
		s := PCSpotlight{PC: pc.Name}
		if totalAimed > 0 {
			share := figf(aimed[pc.Name]/totalAimed*100, "%", fmt.Sprintf(
				"the %.1f expected damage per round the focus puts on %s, over the %.1f the whole encounter puts on the party",
				aimed[pc.Name], pc.Name, totalAimed))
			s.ThreatShare = &share
		}
		if totalAnswer > 0 {
			share := figf(answer[pc.Name]/float64(totalAnswer)*100, "%", fmt.Sprintf(
				"their class baseline summed across the roster and adjusted by the monsters' handling of it, over the party's whole answer of %d",
				totalAnswer))
			s.AnswerShare = &share
		}
		threat := aimed[pc.Name] / math.Max(totalAimed, 1)
		ans := answer[pc.Name] / math.Max(float64(totalAnswer), 1)
		if s.ThreatShare != nil && s.AnswerShare != nil && threat < spotlightFloor && ans < spotlightFloor {
			s.Benched = true
			var why []string
			if aimed[pc.Name] == 0 {
				why = append(why, "no monster's best line of attack aims at them")
			} else {
				why = append(why, fmt.Sprintf("only %.0f%% of the threat is theirs", threat*100))
			}
			if r := benchAnswerReason(pc, groups); r != "" {
				why = append(why, r)
			} else {
				why = append(why, fmt.Sprintf("they supply only %.0f%% of the party's answer", ans*100))
			}
			s.Reason = strings.Join(why, " — ")
		}
		out = append(out, s)
	}
	return out
}

// benchAnswerReason explains a zero answer axis with the specific reason:
// the immunity that blanks the pc's damage, the board that never closes, or
// the missing declaration that leaves nothing to price. Empty when the pc
// does contribute something.
func benchAnswerReason(pc PCFact, groups []pricedGroup) string {
	_, dmg, how, ok := baselineFor(pc.Class, pc.Level)
	if !ok {
		return "the party block declares no class or level to price their answer with"
	}
	immuneAll, outOfReach := true, true
	for _, p := range groups {
		d, note := answerOf(pc, p)
		if d > 0 {
			return ""
		}
		if !matchesDamage(p.g.Creature.Immune, dmg) {
			immuneAll = false
		}
		if !strings.Contains(note, "airborne") && !strings.Contains(note, "never closes") {
			outOfReach = false
		}
	}
	if immuneAll {
		return fmt.Sprintf("every monster on the board is immune to %s — %s", dmg, how)
	}
	if outOfReach {
		return "nothing on the board comes within their reach, and their baseline is melee"
	}
	return "nothing they bring prices against this roster"
}

/* ---------- movement ---------- */

// movementWeights is what each terrain feature adds to the movement score,
// heaviest first for a stable readout order.
var movementWeights = []struct {
	kind   string
	points int
}{
	{"difficult_ground", 20},
	{"chokepoint", 20},
	{"water", 15},
	{"darkness", 15},
	{"elevation", 15},
	{"concealment", 10},
	{"cover", 10},
}

// movementScore turns the design's shape into the roadmap's second question:
// does movement matter here. Terrain, the ranged/melee mix, speeds and reach
// each add points, every one of them listed in the derivation.
func movementScore(in TacticsInput, groups []pricedGroup) *Movement {
	var parts []string
	points := 0
	add := func(pts int, why string) {
		if pts <= 0 {
			return
		}
		points += pts
		parts = append(parts, fmt.Sprintf("%s +%d", why, pts))
	}

	if in.Terrain != nil {
		for _, w := range movementWeights {
			for _, f := range in.Terrain.Features {
				if f.Kind == w.kind {
					add(w.points, FeatureLabel(f.Kind))
				}
			}
		}
	}

	// Ranged/melee mix: the more split the board's answer is between
	// archers and closers, the more positioning decides the fight. A fully
	// ranged board still scores — kiting is movement too.
	ranged := 0
	for _, p := range groups {
		if p.hasRanged {
			ranged++
		}
	}
	if frac := float64(ranged) / float64(len(groups)); frac > 0 {
		mix := 25
		if frac < 1 {
			mix = int(math.Round(40 * (1 - math.Abs(0.5-frac)*2)))
		}
		add(mix, fmt.Sprintf("ranged/melee mix (%d of %d kinds reach at range)", ranged, len(groups)))
	}

	for _, p := range groups {
		if p.speed >= 40 {
			add(10, fmt.Sprintf("%s moves 40 ft. or more a turn", p.g.Creature.Name))
			break
		}
	}
	for _, p := range groups {
		if p.round.dom.reach >= 10 {
			add(10, fmt.Sprintf("%s reaches %d ft.", p.g.Creature.Name, p.round.dom.reach))
			break
		}
	}

	if points > 100 {
		points = 100
	}
	m := &Movement{
		Score:   fig(points, "of 100", strings.Join(parts, "; ")+" — out of 100"),
		Drivers: parts,
	}
	if len(parts) == 0 {
		m.Score.How = "a flat room and a fully melee board: nobody has to move to make their attacks matter"
	}
	switch {
	case points < 25:
		m.Label = "low"
	case points < 55:
		m.Label = "some"
	default:
		m.Label = "high"
	}
	return m
}

/* ---------- the party's baseline answer ---------- */

// baselineFor prices a pc's canonical damage output for their class and
// level, from the SRD's own progressions: Extra Attack at 5th, the damage
// cantrip dice at 5th/11th/17th, ability scores at +3/+4/+5 as 4th and 8th
// grant them. The party block does not carry attack routines, so the answer
// axis prices each pc at what their class canonically does — and every
// figure derived from it says exactly that in its derivation.
func baselineFor(class string, level int) (dpr int, dmg string, how string, ok bool) {
	if level < 1 || level > 20 {
		return 0, "", "", false
	}
	c := classWord(class)
	floor := func() (int, string) {
		d := int(math.Max(3, float64(level)))
		return d, fmt.Sprintf("no usable class declared — priced at %d, the builder's floor of roughly their level in weapon damage", d)
	}

	abil := 3
	if level >= 8 {
		abil = 5
	} else if level >= 4 {
		abil = 4
	}
	cantripDice := 1
	if level >= 5 {
		cantripDice = 2
	}
	if level >= 11 {
		cantripDice = 3
	}
	if level >= 17 {
		cantripDice = 4
	}
	switch c {
	case "fighter":
		attacks := 1
		if level >= 5 {
			attacks = 2
		}
		if level >= 11 {
			attacks = 3
		}
		if level >= 20 {
			attacks = 4
		}
		return attacks * (4 + abil), "weapon",
			fmt.Sprintf("a %dth-level fighter: %d attack(s) of 1d8+%d, melee (Extra Attack at 5th, 11th and 20th)", level, attacks, abil), true
	case "barbarian", "paladin", "ranger":
		attacks := 1
		if level >= 5 {
			attacks = 2
		}
		return attacks * (4 + abil), "weapon",
			fmt.Sprintf("a %dth-level %s: %d attack(s) of 1d8+%d, melee (Extra Attack at 5th)", level, c, attacks, abil), true
	case "rogue":
		dice := (level + 1) / 2
		return 3 + abil + int(3.5*float64(dice)), "weapon",
			fmt.Sprintf("a %dth-level rogue: 1d6+%d with sneak attack at %d dice, once landed", level, abil, dice), true
	case "monk":
		die := 2
		if level >= 5 {
			die = 3
		}
		if level >= 11 {
			die = 4
		}
		if level >= 17 {
			die = 5
		}
		attacks := 1
		if level >= 5 {
			attacks = 2
		}
		return attacks * (die + abil), "weapon",
			fmt.Sprintf("a %dth-level monk: %d martial-arts strike(s) of 1d%d+%d", level, attacks, die*2, abil), true
	case "wizard", "sorcerer", "warlock", "artificer":
		return int(5.5 * float64(cantripDice)), "fire",
			fmt.Sprintf("a %dth-level %s: fire bolt at %d dice (the cantrip's progression at 5th/11th/17th)", level, c, cantripDice), true
	case "cleric":
		return int(4.5 * float64(cantripDice)), "radiant",
			fmt.Sprintf("a %dth-level cleric: sacred flame at %d dice", level, cantripDice), true
	case "druid":
		return int(4.5 * float64(cantripDice)), "fire",
			fmt.Sprintf("a %dth-level druid: produce flame at %d dice", level, cantripDice), true
	case "bard":
		return int(2.5 * float64(cantripDice)), "psychic",
			fmt.Sprintf("a %dth-level bard: vicious mockery at %d dice", level, cantripDice), true
	}
	d, how := floor()
	return d, "weapon", how, true
}

// classWord normalises a free-typed class to its head word, so "Fighter",
// "fighter (battle master)" and "fighter" all read the same way.
func classWord(class string) string {
	f := strings.FieldsFunc(strings.ToLower(class), func(r rune) bool {
		return !(r >= 'a' && r <= 'z')
	})
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

/* ---------- the prose gate ---------- */

// The model writes the tactics prose from the derived numbers it is handed;
// it may not assert a number of its own. Two rules enforce it: the analysis
// collects every integer it can legitimately print, and the gate rejects any
// prose number outside that set.

// ProseViolation is one number in the model's tactics prose that traces to
// nothing the server computed.
type ProseViolation struct {
	Token  string `json:"token"`
	Detail string `json:"detail"`
}

var (
	diceTokenRE = regexp.MustCompile(`(?i)\b\d{1,2}\s*d\s*(4|6|8|10|12|20)\b`)
	// numberTokenRE matches numeric tokens but not ones inside words: the
	// "10" of "5d10" is dice grammar, already lifted out above; the "5" of
	// "5th level" checks like any number.
	numberTokenRE = regexp.MustCompile(`\d[\d,]*(?:\.\d+)?`)
)

// AllowedNumbers collects every integer the analysis and its inputs can
// legitimately put in prose, mapped to what derives it. Figure values count
// at their whole and rounded forms; so do the input numbers the prose may
// restate — pc levels, monster counts, XP, hazard DCs and dice.
func AllowedNumbers(in TacticsInput, a *Analysis) map[int]string {
	allowed := map[int]string{}
	add := func(v float64, how string) {
		if v <= 0 || math.IsInf(v, 0) || math.IsNaN(v) {
			return
		}
		whole := int(v)
		rounded := int(math.Round(v))
		if _, ok := allowed[whole]; !ok {
			allowed[whole] = how
		}
		if _, ok := allowed[rounded]; !ok {
			allowed[rounded] = how
		}
	}
	for _, f := range collectFigures(a) {
		add(f.Value, f.How)
	}
	for _, pc := range in.Party {
		add(float64(pc.Level), fmt.Sprintf("%s's declared level", pc.Name))
	}
	for _, g := range in.Roster {
		add(float64(g.Count), fmt.Sprintf("the roster's count of %s", g.Creature.Name))
		add(float64(g.Creature.XP), fmt.Sprintf("%s's XP from the challenge-rating table", g.Creature.Name))
	}
	if in.Terrain != nil {
		for _, h := range in.Terrain.Hazards {
			add(float64(h.DC), fmt.Sprintf("hazard %s DC from the DMG's tier tables", h.Name))
			for _, n := range diceNumbers(h.Damage) {
				add(float64(n), fmt.Sprintf("the dice of hazard %s", h.Name))
			}
		}
	}
	return allowed
}

// CheckTacticsProse reads the model's tactics prose and returns every number
// in it that traces to nothing the server computed. An empty result means
// the prose is allowed; a non-empty one is the rejection, with each invented
// figure named. This is the pass a fake model cannot talk its way past: any
// figure it invents has no derivation, and no derivation, no number.
func CheckTacticsProse(in TacticsInput, a *Analysis, prose string) []ProseViolation {
	allowed := AllowedNumbers(in, a)
	cleaned := diceTokenRE.ReplaceAllString(prose, " ")
	var out []ProseViolation
	seen := map[string]bool{}
	for _, tok := range numberTokenRE.FindAllString(cleaned, -1) {
		v, err := strconv.ParseFloat(strings.ReplaceAll(tok, ",", ""), 64)
		if err != nil {
			continue
		}
		if _, ok := allowed[int(v)]; ok {
			continue
		}
		if seen[tok] {
			continue
		}
		seen[tok] = true
		out = append(out, ProseViolation{
			Token:  tok,
			Detail: fmt.Sprintf("%s traces to nothing the server computed — the tactics prose may not assert a number", tok),
		})
	}
	return out
}

// collectFigures walks an analysis by reflection and returns every Figure it
// carries, however nested. The derivation test runs over this; so does the
// prose gate's allowlist.
func collectFigures(a *Analysis) []Figure {
	var out []Figure
	var walk func(v reflect.Value)
	walk = func(v reflect.Value) {
		switch v.Kind() {
		case reflect.Pointer, reflect.Interface:
			if !v.IsNil() {
				walk(v.Elem())
			}
		case reflect.Struct:
			if v.Type() == reflect.TypeOf(Figure{}) {
				out = append(out, v.Interface().(Figure))
				return
			}
			for i := 0; i < v.NumField(); i++ {
				walk(v.Field(i))
			}
		case reflect.Slice, reflect.Array:
			for i := 0; i < v.Len(); i++ {
				walk(v.Index(i))
			}
		case reflect.Map:
			for _, k := range v.MapKeys() {
				walk(v.MapIndex(k))
			}
		}
	}
	if a != nil {
		walk(reflect.ValueOf(*a))
	}
	return out
}

// diceNumbers pulls the two integers out of a dice expression like "4d10".
func diceNumbers(expr string) []int {
	expr = strings.ToLower(strings.TrimSpace(expr))
	i := strings.IndexByte(expr, 'd')
	if i < 0 {
		return nil
	}
	n, err1 := strconv.Atoi(strings.TrimSpace(expr[:i]))
	sides, err2 := strconv.Atoi(strings.TrimSpace(expr[i+1:]))
	if err1 != nil || err2 != nil {
		return nil
	}
	return []int{n, sides}
}

/* ---------- section extraction ---------- */

// SectionOf returns the body of the named Markdown section (any heading
// level, case-insensitive) up to the next heading, trimmed. It is how the
// design flow lifts the model's Tactics prose out of the write-up so the
// prose gate can judge it alone.
func SectionOf(md, name string) string {
	lines := strings.Split(md, "\n")
	var body []string
	in := false
	for _, line := range lines {
		if m := headingRE.FindStringSubmatch(line); m != nil {
			if in {
				break
			}
			in = strings.EqualFold(strings.TrimSpace(strings.Trim(m[1], "*_ :")), name)
			continue
		}
		if in {
			body = append(body, line)
		}
	}
	return strings.TrimSpace(strings.Join(body, "\n"))
}
