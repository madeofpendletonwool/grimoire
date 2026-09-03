package statblock

import (
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ComputeCR derives a challenge rating from a statblock, following the 2014
// DMG's "Creating a Monster Stat Block" procedure (p. 274): a defensive CR
// from effective hit points against effective AC, an offensive CR from
// damage per round against attack bonus or save DC, and the printed CR as
// the average of the two — with every adjustment applied reported, and a
// confidence that never overstates what the input supported.
//
// The function is deterministic and total: the same Statblock always yields
// the same Rating, and a zero-value Statblock yields a zero rating, not a
// panic. Where the procedure needs a number the statblock does not carry,
// the rating says so in Notes and its Confidence drops.
func ComputeCR(s Statblock) Rating {
	r := Rating{HP: s.HP, AC: s.AC, Confidence: ConfidenceHigh}

	/* ---------- defensive: effective hit points against AC ---------- */

	hp := s.HP
	if hp <= 0 {
		hp = hitDiceAvg(s.HitDice)
		if hp > 0 {
			r.Notes = append(r.Notes, "hit points taken from hit dice \""+s.HitDice+"\" (printed total missing)")
		}
	}
	r.HP = hp

	baseIdx := crFromHP(hp)
	effHP := float64(hp)

	// Resistances, immunities and vulnerabilities to nonmagical bludgeoning,
	// piercing and slashing shift effective hit points (DMG p. 274 table).
	// Resistances: ×1.5 at CR 0–4, ×2 above. The DMG prices no immunity; we
	// price it as ×2, the multiplier that reproduces the SRD's own low-CR
	// immune monsters (the werewolf computes to its printed CR 3 exactly so),
	// and docs/development/statblock.md says this openly. Vulnerabilities
	// halve. Damage types outside b/p/s are not adjusted: the DMG says their
	// effect is too situational to price.
	if res := hasBPSClause(s.Resist); res != "" {
		mult := 2.0
		if crLevels[baseIdx] <= 4 {
			mult = 1.5
		}
		before := effHP
		effHP *= mult
		r.Adjustments = append(r.Adjustments, Adjustment{
			Kind: "resistance", Detail: res, Value: int(math.Round(effHP - before))})
	}
	if imm := hasBPSClause(s.Immune); imm != "" {
		before := effHP
		effHP *= 2.0
		r.Adjustments = append(r.Adjustments, Adjustment{
			Kind: "immunity", Detail: imm, Value: int(math.Round(effHP - before))})
	}
	if vuln := hasBPSClause(s.Vulnerable); vuln != "" {
		before := effHP
		effHP *= 0.5
		r.Adjustments = append(r.Adjustments, Adjustment{
			Kind: "vulnerability", Detail: vuln, Value: int(math.Round(effHP - before))})
	}

	// Regeneration is priced at three rounds' worth (the DMG's own horizon
	// for effective hit points).
	regen := regenerationPerRound(s)
	if regen > 0 {
		effHP += float64(3 * regen)
		r.Adjustments = append(r.Adjustments, Adjustment{
			Kind: "regeneration", Detail: "regains " + strconv.Itoa(regen) + " hit points per round",
			Value: 3 * regen})
	}

	r.EffectiveHP = int(math.Round(effHP))
	defIdx := crFromHP(r.EffectiveHP)

	if s.AC > 0 {
		steps := int(math.Round(float64(s.AC-defensiveAC[defIdx]) / 2))
		if steps != 0 {
			r.Adjustments = append(r.Adjustments, Adjustment{
				Kind: "armor",
				Detail: "AC " + strconv.Itoa(s.AC) + " against the assumed AC " +
					strconv.Itoa(defensiveAC[defIdx]) + " for the effective hit points",
				Value: steps})
		}
		defIdx = clampIdx(defIdx + steps)
	} else {
		r.Notes = append(r.Notes, "armor class missing: no AC shift applied, confidence capped")
	}
	r.Defensive = crLevels[defIdx]

	/* ---------- offensive: damage per round against attack bonus ---------- */

	rounds, source := standardRounds(s, &r)
	round1 := rounds[0] + limitedRoundOne(s, source)
	legendary := legendaryPerRound(s)
	if legendary > 0 {
		round1 += legendary
		rounds[1] += legendary
		rounds[2] += legendary
	}
	dpr := int(math.Round(float64(round1+rounds[1]+rounds[2]) / 3))
	r.DPR = dpr

	offIdx := crFromDPR(dpr)
	r.SaveBased = source.saveBased
	if source.saveBased {
		if source.dc > 0 {
			r.AttackBonus = source.dc
			steps := int(math.Round(float64(source.dc-offensiveDC[offIdx]) / 2))
			if steps != 0 {
				r.Adjustments = append(r.Adjustments, Adjustment{
					Kind:   "save DC",
					Detail: "DC " + strconv.Itoa(source.dc) + " against the assumed DC " + strconv.Itoa(offensiveDC[offIdx]) + " for the damage dealt",
					Value:  steps})
			}
			offIdx = clampIdx(offIdx + steps)
		}
	} else if source.bonus > 0 {
		r.AttackBonus = source.bonus
		steps := int(math.Round(float64(source.bonus-offensiveAB[offIdx]) / 2))
		if steps != 0 {
			r.Adjustments = append(r.Adjustments, Adjustment{
				Kind:   "attack bonus",
				Detail: "+" + strconv.Itoa(source.bonus) + " to hit against the assumed +" + strconv.Itoa(offensiveAB[offIdx]) + " for the damage dealt",
				Value:  steps})
		}
		offIdx = clampIdx(offIdx + steps)
	} else {
		r.Notes = append(r.Notes, "no attack bonus or save DC to price: offense read from damage alone")
	}
	r.Offensive = crLevels[offIdx]

	/* ---------- the printed rating ---------- */

	r.CR = snapCR((r.Defensive + r.Offensive) / 2)
	r.Label = Label(r.CR)

	// The diagnosis: which half is short, in one line a DM can act on.
	if defIdx != offIdx {
		hi, lo := "defense", "offense"
		if defIdx < offIdx {
			hi, lo = lo, hi
		}
		r.Notes = append(r.Notes, hi+" is the stronger half: defensive CR "+
			Label(r.Defensive)+" against offensive CR "+Label(r.Offensive)+
			" — "+lo+" is what falls short of the printed rating")
	}

	applyConfidence(s, &r)
	return r
}

/* ---------- defensive helpers ---------- */

// hasBPSClause reports the clause that resists/immunizes/vulnerabilizes
// against bludgeoning, piercing and slashing damage together — the form the
// DMG's table prices. Specific types ("fire") do not qualify; qualifiers
// ("from nonmagical attacks", "that aren't silvered") ride along in the
// returned text, since they describe the same b/p/s gate.
func hasBPSClause(list []string) string {
	for _, clause := range list {
		low := strings.ToLower(clause)
		if strings.Contains(low, "bludgeoning") && strings.Contains(low, "piercing") && strings.Contains(low, "slashing") {
			return strings.TrimSpace(clause)
		}
	}
	return ""
}

var reRegeneration = regexp.MustCompile(`regains (\d+) hit points`)

// regenerationPerRound reads the trait prose for a regeneration rate. Only
// the printed per-turn figure counts; riders ("...unless it takes acid or
// fire damage") are not discounted — the DMG does not price them either.
func regenerationPerRound(s Statblock) int {
	best := 0
	for _, t := range s.Traits {
		if m := reRegeneration.FindStringSubmatch(normalize(t.Name + " " + t.Desc)); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil && n > best {
				best = n
			}
		}
	}
	return best
}

/* ---------- offensive helpers ---------- */

// roundTotals carries the damage of rounds 2 and 3 (the sustainable output)
// separately from round 1, which also gets the limited-use actions.
type roundTotals [3]int

// offenseSource records where the priced damage came from, so the attack
// bonus adjustment reads the right column.
type offenseSource struct {
	index     int  // action index, -1 when none
	bonus     int  // to-hit bonus, 0 when absent
	dc        int  // save DC, 0 when absent
	saveBased bool // the priced attack delivers through a save DC
	isLimited bool // the priced attack is itself a once-per-fight action
}

// standardRounds computes the monster's sustainable output for rounds 2 and
// 3, and seeds round 1: a resolved multiattack if one resolves, else the
// single most damaging at-will action. Limited-use actions (recharges,
// per-day powers) are saved for round 1, which is the only round the DMG's
// three-round horizon can rely on them for. Area effects count two targets,
// per the DMG.
func standardRounds(s Statblock, r *Rating) (roundTotals, offenseSource) {
	var totals roundTotals
	src := offenseSource{index: -1}

	// The multiattack, when every component resolves against a parsed
	// action, is the standard round the DMG means.
	for i, a := range s.Actions {
		if !a.Parsed || a.Attack.Kind != KindMultiattack {
			continue
		}
		dmg, top, complete := multiattackDamage(s, a.Attack)
		if !complete {
			r.Notes = append(r.Notes, "multiattack \""+a.Name+"\" names an action this statblock does not parse; the standard round falls back to the strongest single action")
			continue
		}
		if dmg == 0 {
			continue // a multiattack of control actions: fall through to singles
		}
		totals[0], totals[1], totals[2] = dmg, dmg, dmg
		if top.index >= 0 {
			src = top
			src.index = i
		}
		return totals, src
	}

	// No resolvable multiattack: the strongest single at-will action is the
	// standard round. Recharges and per-day powers wait for round 1.
	best, bestAvg := -1, 0
	bestLimited, bestLimitedAvg := -1, 0
	for i, a := range s.Actions {
		if !a.Parsed {
			continue
		}
		avg := a.Attack.Avg(true)
		if isLimited(a) {
			if avg > bestLimitedAvg {
				bestLimited, bestLimitedAvg = i, avg
			}
			continue
		}
		if avg > bestAvg {
			best, bestAvg = i, avg
		}
	}
	if best >= 0 && bestAvg > 0 {
		totals[0], totals[1], totals[2] = bestAvg, bestAvg, bestAvg
		a := s.Actions[best].Attack
		src = offenseSource{index: best, bonus: a.ToHit, dc: a.SaveDC, saveBased: a.SaveDC > 0 && a.ToHit == 0}
	} else if bestLimited >= 0 {
		// Nothing sustainable at all: the once-per-fight attack is the only
		// damage there is. It seeds round 1 and the three-round average pays
		// for it — rounds 2 and 3 go hungry, which is the honest price of a
		// creature with no at-will attack.
		a := s.Actions[bestLimited].Attack
		totals[0] = bestLimitedAvg
		src = offenseSource{index: bestLimited, bonus: a.ToHit, dc: a.SaveDC,
			saveBased: a.SaveDC > 0 && a.ToHit == 0, isLimited: true}
	}
	return totals, src
}

// isLimited reports whether the action cannot be relied on every round.
func isLimited(a Action) bool {
	return a.Usage != "" && a.Usage != "at will"
}

// multiattackDamage sums a resolved multiattack's component damage. A
// choice component prices its strongest resolvable option — "in any
// combination" includes the one that hits hardest. The second return names
// the single action doing the most damage, whose bonus the offense
// adjustment prices.
func multiattackDamage(s Statblock, m Attack) (dmg int, top offenseSource, complete bool) {
	complete = true
	better := func(i int) {
		if avg := s.Actions[i].Attack.Avg(true); top.index == -1 || avg > s.Actions[top.index].Attack.Avg(true) {
			a := s.Actions[i].Attack
			top = offenseSource{index: i, bonus: a.ToHit, dc: a.SaveDC, saveBased: a.SaveDC > 0 && a.ToHit == 0}
		}
	}
	for _, c := range m.Components {
		if len(c.Options) > 0 {
			best, bestAvg := -1, 0
			for _, name := range c.Options {
				if i := ResolveComponent(s, name); i >= 0 {
					if avg := s.Actions[i].Attack.Avg(true); avg > bestAvg {
						best, bestAvg = i, avg
					}
				}
			}
			if best < 0 {
				complete = false
				continue
			}
			dmg += c.Count * bestAvg
			better(best)
			continue
		}
		i := ResolveComponent(s, c.Name)
		if i < 0 {
			complete = false
			continue
		}
		a := s.Actions[i].Attack
		dmg += c.Count * a.Avg(true)
		if a.Avg(true) > 0 {
			better(i)
		}
	}
	return dmg, top, complete
}

// limitedRoundOne adds the once-per-fight actions to round 1: recharge
// breaths, per-day powers. When the whole offense is one limited action it
// is already counted as the standard round's round-1 seed, so the source
// index is skipped to avoid paying for it twice.
func limitedRoundOne(s Statblock, src offenseSource) int {
	added := 0
	for i, a := range s.Actions {
		if !a.Parsed || !isLimited(a) {
			continue
		}
		if src.index == i && src.isLimited {
			continue
		}
		added += a.Attack.Avg(true)
	}
	return added
}

// legendaryPerRound prices the legendary actions at three points per round:
// the most damaging options the budget can pay for, cost-aware (a
// two-cost option displaces two one-cost ones). Assumed damage applies to
// every round — the DMG's expectation that a legendary monster keeps its
// extra attacks up all fight.
func legendaryPerRound(s Statblock) int {
	type opt struct {
		avg  int
		cost int
	}
	var opts []opt
	for _, a := range s.Actions {
		if !a.Legendary() || !a.Parsed {
			continue
		}
		if dmg := a.Attack.Avg(true); dmg > 0 {
			cost := a.Cost
			if cost <= 0 {
				cost = 1
			}
			opts = append(opts, opt{avg: dmg, cost: cost})
		}
	}
	if len(opts) == 0 {
		return 0
	}
	sort.SliceStable(opts, func(i, j int) bool { return opts[i].avg > opts[j].avg })
	budget, total := 3, 0
	for _, o := range opts {
		if o.cost <= budget {
			budget -= o.cost
			total += o.avg
		}
		if budget == 0 {
			break
		}
	}
	return total
}

/* ---------- confidence ---------- */

// applyConfidence grades the rating. The rule the issue pins: a statblock
// whose parse was incomplete never rates high — and a statblock the
// arithmetic knows it is guessing for (spell damage it cannot price, no
// numbers to read) rates low.
func applyConfidence(s Statblock, r *Rating) {
	r.Confidence = ConfidenceHigh

	var unparsed []string
	for _, a := range s.Actions {
		if !a.Parsed {
			unparsed = append(unparsed, a.Name)
		}
	}
	if len(unparsed) > 0 {
		r.Confidence = ConfidenceLow
		r.Notes = append(r.Notes, "incomplete parse: "+strconv.Itoa(len(unparsed))+
			" action(s) unread ("+strings.Join(unparsed, ", ")+")")
	}

	if len(s.Actions) == 0 {
		r.Confidence = ConfidenceLow
		r.Notes = append(r.Notes, "no actions to price: offense is assumed to be zero")
	} else if r.DPR == 0 {
		r.Confidence = ConfidenceLow
		r.Notes = append(r.Notes, "no readable damage: offense is assumed to be zero")
	}

	if r.HP <= 0 {
		r.Confidence = ConfidenceLow
		r.Notes = append(r.Notes, "no hit points or hit dice: defense is assumed to be zero")
	}
	if r.AC <= 0 {
		r.Confidence = ConfidenceLow
	}

	if s.Spellcasting && r.Confidence == ConfidenceHigh {
		r.Confidence = ConfidenceMedium
		r.Notes = append(r.Notes, "the statblock casts spells; spell damage is not priced and the printed CR may lean on it")
	}
	if s.Legendary && r.Confidence == ConfidenceHigh {
		// Legendary monsters are priced with assumed legendary-action output;
		// the books' own figures lean on tactics the arithmetic cannot see.
		r.Confidence = ConfidenceMedium
		r.Notes = append(r.Notes, "legendary actions priced at three points of the strongest options per round")
	}
}

func clampIdx(i int) int {
	if i < 0 {
		return 0
	}
	if i >= len(crLevels) {
		return len(crLevels) - 1
	}
	return i
}
