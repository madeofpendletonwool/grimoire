package homebrew

// The structural checks (MAD-385, check 2): deterministic rules over the
// declared schemas the monster and item designers already enforce. These
// run with no model and no network — they are plain functions over the
// structured forms, and every finding names the rule it enforced.
//
// The vocabularies below are the game's own sets, declared here in one
// place. They are deliberately the 2014 SRD's sets, the same edition every
// encounter surface in the app keeps to.

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/items"
	"github.com/madeofpendletonwool/grimoire/internal/statblock"
)

/* ---------- the game's own vocabulary, declared ---------- */

// MonsterSizes is the 2014 SRD size vocabulary.
var MonsterSizes = []string{"tiny", "small", "medium", "large", "huge", "gargantuan"}

// CreatureTypes is the 2014 SRD creature-type vocabulary. Printed forms
// carry modifiers — "humanoid (any race)" — so the check reads the leading
// type word.
var CreatureTypes = []string{
	"aberration", "beast", "celestial", "construct", "dragon", "elemental",
	"fey", "fiend", "giant", "humanoid", "monstrosity", "ooze", "plant", "undead",
}

// Abilities is the six-ability save vocabulary, lower-case — the same keys
// the statblock's Saves map carries.
var Abilities = []string{"str", "dex", "con", "int", "wis", "cha"}

// abilityTitles maps the parser's title-case save abilities onto the keys.
var abilityTitles = map[string]string{
	"str": "str", "dex": "dex", "con": "con", "int": "int", "wis": "wis", "cha": "cha",
	"strength": "str", "dexterity": "dex", "constitution": "con",
	"intelligence": "int", "wisdom": "wis", "charisma": "cha",
}

// Skills is the 2014 SRD skill vocabulary, squashed the way the mirror
// squashes it.
var Skills = []string{
	"acrobatics", "animalhandling", "arcana", "athletics", "deception",
	"history", "insight", "intimidation", "investigation", "medicine",
	"nature", "perception", "performance", "persuasion", "religion",
	"sleightofhand", "stealth", "survival",
}

// DamageTypes is the game's thirteen damage types.
var DamageTypes = []string{
	"acid", "bludgeoning", "cold", "fire", "force", "lightning", "necrotic",
	"piercing", "poison", "psychic", "radiant", "slashing", "thunder",
}

// Conditions is the game's fifteen conditions.
var Conditions = []string{
	"blinded", "charmed", "deafened", "exhaustion", "frightened", "grappled",
	"incapacitated", "invisible", "paralyzed", "petrified", "poisoned",
	"prone", "restrained", "stunned", "unconscious",
}

// SpeedWords is the movement vocabulary the mirror flattens speeds into.
var SpeedWords = []string{"walk", "fly", "swim", "burrow", "climb"}

// inVocabulary is the case-insensitive membership test.
func inVocabulary(value string, vocab []string) bool {
	v := strings.ToLower(strings.TrimSpace(value))
	for _, ok := range vocab {
		if v == ok {
			return true
		}
	}
	return false
}

// vocabularyList renders a vocabulary for a finding message.
func vocabularyList(vocab []string) string {
	return strings.Join(vocab, ", ")
}

// matchDamageType returns the first game damage type a clause names, if
// any. Clauses print as groups — "bludgeoning, piercing, and slashing from
// nonmagical attacks" — so the test is containment, not equality.
func matchDamageType(clause string) string {
	low := strings.ToLower(clause)
	for _, dt := range DamageTypes {
		if strings.Contains(low, dt) {
			return dt
		}
	}
	return ""
}

// matchCondition returns the first game condition a clause names, if any.
func matchCondition(clause string) string {
	low := strings.ToLower(clause)
	for _, c := range Conditions {
		if strings.Contains(low, c) {
			return c
		}
	}
	return ""
}

/* ---------- the monster checks ---------- */

// lintMonsterStructure runs every deterministic rule over a statblock.
// It is pure: same input, same findings, no model, no network.
func lintMonsterStructure(sb statblock.Statblock) []Finding {
	var out []Finding
	out = append(out, lintMonsterIdentity(sb)...)
	out = append(out, lintMonsterScores(sb)...)
	out = append(out, lintMonsterSavesAndSkills(sb)...)
	out = append(out, lintMonsterDefense(sb)...)
	out = append(out, lintMonsterMovement(sb)...)
	out = append(out, lintMonsterDamageVocabulary(sb)...)
	out = append(out, lintMonsterSaveAbilities(sb)...)
	out = append(out, lintMonsterUsageGrammar(sb)...)
	out = append(out, lintMonsterRechargeCycle(sb)...)
	return out
}

func structural(check, subject, rule, format string, args ...any) Finding {
	return Finding{
		Check: check, Severity: SeverityError, Subject: subject,
		Message: fmt.Sprintf(format, args...),
		Basis:   Basis{Origin: OriginStructural, Rule: rule},
	}
}

// lintMonsterIdentity checks size and creature type against the game's
// sets.
func lintMonsterIdentity(sb statblock.Statblock) []Finding {
	var out []Finding
	if strings.TrimSpace(sb.Size) == "" {
		out = append(out, structural(CheckMonsterIdentity, "size",
			"the statblock declares a size vocabulary ("+vocabularyList(MonsterSizes)+")",
			"no size is declared — the statblock does not read as a creature"))
	} else if !inVocabulary(sb.Size, MonsterSizes) {
		out = append(out, structural(CheckMonsterIdentity, "size",
			"the statblock declares a size vocabulary ("+vocabularyList(MonsterSizes)+")",
			"size %q is not one of the game's sizes [%s]",
			sb.Size, vocabularyList(MonsterSizes)))
	}
	t := strings.TrimSpace(sb.Type)
	if t == "" {
		out = append(out, structural(CheckMonsterIdentity, "type",
			"the statblock declares the game's creature types ("+vocabularyList(CreatureTypes)+")",
			"no creature type is declared"))
	} else {
		// Printed types carry modifiers: "humanoid (any race)". The check
		// reads the leading type word.
		typeWord := strings.ToLower(strings.TrimSpace(t))
		if i := strings.IndexAny(typeWord, "(,"); i > 0 {
			typeWord = strings.TrimSpace(typeWord[:i])
		}
		if !inVocabulary(typeWord, CreatureTypes) {
			out = append(out, structural(CheckMonsterIdentity, "type",
				"the statblock declares the game's creature types ("+vocabularyList(CreatureTypes)+")",
				"creature type %q is not one of the game's types [%s]",
				t, vocabularyList(CreatureTypes)))
		}
	}
	return out
}

// lintMonsterScores checks the six ability scores: present, and inside the
// game's 1–30 band.
func lintMonsterScores(sb statblock.Statblock) []Finding {
	var out []Finding
	scores := map[string]int{
		"str": sb.Abilities.Str, "dex": sb.Abilities.Dex, "con": sb.Abilities.Con,
		"int": sb.Abilities.Int, "wis": sb.Abilities.Wis, "cha": sb.Abilities.Cha,
	}
	keys := make([]string, 0, len(scores))
	for k := range scores {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := scores[k]
		switch {
		case v == 0:
			out = append(out, structural(CheckMonsterScores, "abilities."+k,
				"a statblock declares all six ability scores, each from 1 to 30",
				"ability score %s is missing — every statblock carries all six", strings.ToUpper(k)))
		case v < 0 || v > 30:
			out = append(out, structural(CheckMonsterScores, "abilities."+k,
				"a statblock declares all six ability scores, each from 1 to 30",
				"ability score %s is %d, outside the game's 1–30 band", strings.ToUpper(k), v))
		}
	}
	return out
}

// lintMonsterSavesAndSkills checks that the save and skill maps speak the
// game's vocabulary.
func lintMonsterSavesAndSkills(sb statblock.Statblock) []Finding {
	var out []Finding
	for _, k := range sortedKeys(sb.Saves) {
		if !inVocabulary(k, Abilities) {
			out = append(out, structural(CheckMonsterSaves, "saves."+k,
				"saving throws are keyed to the six abilities ("+vocabularyList(Abilities)+")",
				"saving throw %q is not one of the game's abilities [%s]",
				k, vocabularyList(Abilities)))
		}
	}
	for _, k := range sortedKeys(sb.Skills) {
		if !inVocabulary(strings.ReplaceAll(strings.ToLower(k), " ", ""), Skills) {
			out = append(out, structural(CheckMonsterSaves, "skills."+k,
				"skills are keyed to the game's skill list ("+vocabularyList(Skills)+")",
				"skill %q is not one of the game's skills", k))
		}
	}
	return out
}

// lintMonsterDefense checks the two numbers everything else prices against.
func lintMonsterDefense(sb statblock.Statblock) []Finding {
	var out []Finding
	if sb.AC < 1 || sb.AC > 30 {
		out = append(out, structural(CheckMonsterDefense, "ac",
			"armor class runs from 1 to 30 — the DMG's tables price nothing outside it",
			"armor class %d is outside the game's 1–30 band", sb.AC))
	}
	if sb.HP < 1 {
		out = append(out, structural(CheckMonsterDefense, "hp",
			"a statblock declares its hit-point total — the defensive half of the CR arithmetic prices it directly",
			"hit points are %d — the defensive arithmetic has nothing to price", sb.HP))
	}
	return out
}

// lintMonsterMovement checks the speed keys against the movement
// vocabulary.
func lintMonsterMovement(sb statblock.Statblock) []Finding {
	var out []Finding
	for _, k := range sortedKeys(sb.Speeds) {
		if !inVocabulary(k, SpeedWords) {
			out = append(out, structural(CheckMonsterMovement, "speeds."+k,
				"movement modes are one of ("+vocabularyList(SpeedWords)+")",
				"movement mode %q is not one the game's statblocks carry", k))
		}
	}
	return out
}

// lintMonsterDamageVocabulary checks every damage clause and every parsed
// attack's damage type against the game's thirteen.
func lintMonsterDamageVocabulary(sb statblock.Statblock) []Finding {
	var out []Finding
	check := func(subject string, clauses []string) {
		for _, clause := range clauses {
			if strings.TrimSpace(clause) == "" {
				continue
			}
			dt := matchDamageType(clause)
			cond := matchCondition(clause)
			if dt == "" && cond == "" {
				out = append(out, structural(CheckMonsterDamage, subject,
					"damage and condition handling name the game's own sets ("+
						vocabularyList(DamageTypes)+"; "+vocabularyList(Conditions)+")",
					"%s clause %q names no game damage type or condition — the game has no such resistance to price",
					subject, strings.TrimSpace(clause)))
			}
		}
	}
	check("resist", sb.Resist)
	check("immune", sb.Immune)
	check("vulnerable", sb.Vulnerable)
	for i, a := range sb.Actions {
		for j, d := range a.Attack.Damage {
			t := strings.ToLower(strings.TrimSpace(d.Type))
			if t == "" {
				continue
			}
			if !inVocabulary(t, DamageTypes) {
				out = append(out, structural(CheckMonsterDamage,
					fmt.Sprintf("actions[%d].damage[%d]", i, j),
					"attacks deal the game's thirteen damage types ("+vocabularyList(DamageTypes)+")",
					"action %q deals %q damage, which is not one of the game's damage types",
					a.Name, d.Type))
			}
		}
	}
	return out
}

// lintMonsterSaveAbilities checks that every parsed save attack saves
// against one of the six abilities.
func lintMonsterSaveAbilities(sb statblock.Statblock) []Finding {
	var out []Finding
	for i, a := range sb.Actions {
		if a.Attack.SaveDC <= 0 {
			continue
		}
		if _, ok := abilityTitles[strings.ToLower(strings.TrimSpace(a.Attack.SaveAbility))]; !ok {
			out = append(out, structural(CheckMonsterSaveAbility,
				fmt.Sprintf("actions[%d]", i),
				"a saving throw is made against one of the six abilities ("+vocabularyList(Abilities)+")",
				"action %q imposes a DC %d save against %q, which is not one of the game's abilities",
				a.Name, a.Attack.SaveDC, a.Attack.SaveAbility))
		}
	}
	return out
}

/* ---------- usage grammar ---------- */

var (
	reUsageRecharge = regexp.MustCompile(`^recharge (\d+)(-(6|\d+))?$`)
	reUsagePerDay   = regexp.MustCompile(`^(\d{1,2})/day$`)
	// reDiceGrammar matches a dice expression anywhere in a string —
	// "3d6", "2d10+2" — the shape the game prints damage in.
	reDiceGrammar = regexp.MustCompile(`(?i)\b\d{1,3}\s*d\s*(4|6|8|10|12|20|100)\b([+-]\s*\d{1,3})?`)
)

// usageParses reports whether a usage string is one the declared grammar
// carries: "", "at will", "recharge 5-6", "recharge after rest", "3/day".
func usageParses(usage string) bool {
	u := strings.ToLower(strings.TrimSpace(usage))
	switch u {
	case "", "at will", "recharge after rest":
		return true
	}
	if reUsageRecharge.MatchString(u) || reUsagePerDay.MatchString(u) {
		return true
	}
	return false
}

// usageBounded reports whether a usage string declares a cost: a recharge
// roll or a per-day budget. Empty and "at will" are unbounded — usable
// every round, forever.
func usageBounded(usage string) bool {
	u := strings.ToLower(strings.TrimSpace(usage))
	return reUsageRecharge.MatchString(u) || reUsagePerDay.MatchString(u) ||
		u == "recharge after rest"
}

// lintMonsterUsageGrammar checks every action's usage string against the
// declared grammar.
func lintMonsterUsageGrammar(sb statblock.Statblock) []Finding {
	var out []Finding
	for i, a := range sb.Actions {
		if usageParses(a.Usage) {
			continue
		}
		out = append(out, structural(CheckMonsterUsage, fmt.Sprintf("actions[%d]", i),
			`usage limits read as "recharge 5-6", "recharge after rest", "3/day", "at will", or nothing (usable every round)`,
			"action %q carries usage %q, which does not parse as the game's usage grammar — its cost is unreadable",
			a.Name, a.Usage))
	}
	return out
}

/* ---------- the recharge cycle ---------- */

// The bounded version of "infinite loop" (MAD-385, check 2, last rule):
// an ability that restores the resource it spends, detectable because the
// resource grammar is declared. This is a narrow structural check with a
// named scope, not general loop detection:
//
//   - Only actions are read, never traits — the SRD's regeneration trait
//     is a published mechanic, and flagging it would be a false positive
//     by construction. Only actions with unbounded usage (empty, or "at
//     will") are candidates, and legendary actions are out of scope —
//     they are budgeted per round by definition.
//   - The resources are the declared ones: the creature's own hit points,
//     spell slots, and charges. An unbounded action that restores any of
//     them is a cycle with no cost.
//   - The one printed exception the grammar still catches: an ability
//     that both spends and restores the same resource class in its own
//     text, at any usage — the literal "restores the resource it spends".
//
// The false-positive fixture matters more than the true-positive one: the
// corpus harness runs this over the whole SRD bestiary and requires
// silence, so the SRD's own recharging abilities stay lint-clean.

var (
	// reRestoreHP matches self-restoration of hit points: "regains 10 hit
	// points", "heals itself ... hit points". Restoration aimed at
	// another creature ("the target regains") does not match.
	reRestoreHP = regexp.MustCompile(`(?i)\b(regains?|heals?|restores?|recovers?)\b[^.]{0,60}?\b(\d+ )?\bhit points\b`)
	// reOtherSubject excludes restoration the prose hands to someone else.
	reOtherSubject = regexp.MustCompile(`(?i)\b(target|creature|ally|wielder|another|other)\b[^.]{0,40}?(regains?|heals?|restores?|recovers?)`)
	// reRestoreSlot / reRestoreCharge match declared-resource recovery.
	reRestoreSlot   = regexp.MustCompile(`(?i)\b(regains?|restores?|recovers?|expends?)\b[^.]{0,60}?(expended )?(spell )?slots?\b`)
	reRestoreCharge = regexp.MustCompile(`(?i)\b(regains?|restores?|recovers?)\b[^.]{0,60}?\bcharges?\b`)
	// reSpend matches declared-resource spending.
	reSpendSlot   = regexp.MustCompile(`(?i)\b(expends?|spends?|consumes?)\b[^.]{0,40}?(spell )?slots?\b`)
	reSpendHP     = regexp.MustCompile(`(?i)\b(costs?|expends?|spends?|consumes?|loses?)\b[^.]{0,40}?\bhit points\b`)
	reSpendCharge = regexp.MustCompile(`(?i)\b(expends?|spends?|consumes?)\b[^.]{0,40}?\bcharges?\b`)
	// reDamageCoupled marks restoration written as "equal to the damage
	// taken" — the vampire's bite, the classic lifesteal. That restore
	// is bounded by the enemy's own hit points, which is the game's own
	// cost, not a cycle. The corpus harness holds this exclusion against
	// the whole bestiary.
	reDamageCoupled = regexp.MustCompile(`(?i)\bequal to\b`)
	// reNegatedRestore strips "the target can't regain hit points" — a
	// denial of restoration, which the naive word match reads as one.
	// The bearded devil's beard is the corpus's standing fixture for it.
	reNegatedRestore = regexp.MustCompile(`(?i)\b(can'?t|cannot|can not|never|unable to|fails? to)\b[^.]{0,40}?\b(regains?|restores?|heals?|recovers?)\b`)
	// reRestoreToOther strips restoration the prose hands to someone else
	// with the beneficiary after the verb — "a spell restores Hit Points
	// to the target", the infernal wound's cure condition.
	reRestoreToOther = regexp.MustCompile(`(?i)\b(regains?|restores?|heals?|recovers?)\b[^.]{0,60}?\b(to|for) (the |a |an |any )?(target|creature|ally|wielder|another|other|user)\b`)
)

// cycleResource classifies what an action's prose restores, for the
// finding's message.
func restoredResource(desc string) string {
	// Denials first: "can't regain Hit Points" names restoration only to
	// forbid it. Then restoration handed to someone else.
	desc = reNegatedRestore.ReplaceAllString(desc, " ")
	desc = reRestoreToOther.ReplaceAllString(desc, " ")
	if reDamageCoupled.MatchString(desc) {
		// Restoration coupled to damage dealt is priced by the damage
		// itself — bounded, published, and out of the check's scope.
		return ""
	}
	if reOtherSubject.MatchString(desc) {
		// Restoration the prose hands elsewhere is not a self-sustaining
		// cycle for this creature. The spend-then-restore rule below
		// still applies when the same text takes and gives back.
		if !(reSpendSlot.MatchString(desc) || reSpendHP.MatchString(desc) || reSpendCharge.MatchString(desc)) {
			return ""
		}
	}
	switch {
	case reRestoreSlot.MatchString(desc):
		return "spell slots"
	case reRestoreCharge.MatchString(desc):
		return "charges"
	case reRestoreHP.MatchString(desc):
		return "hit points"
	}
	return ""
}

// spentResource classifies what an action's prose spends.
func spentResource(desc string) string {
	switch {
	case reSpendSlot.MatchString(desc):
		return "spell slots"
	case reSpendCharge.MatchString(desc):
		return "charges"
	case reSpendHP.MatchString(desc):
		return "hit points"
	}
	return ""
}

// lintMonsterRechargeCycle is the named-scope check.
func lintMonsterRechargeCycle(sb statblock.Statblock) []Finding {
	var out []Finding
	for i, a := range sb.Actions {
		if a.Legendary() {
			continue // budgeted per round; out of the check's scope
		}
		subject := fmt.Sprintf("actions[%d]", i)
		gives := restoredResource(a.Desc)
		takes := spentResource(a.Desc)
		switch {
		case gives != "" && gives == takes:
			out = append(out, structural(CheckMonsterCycle, subject,
				"a recharge cycle has a cost — no ability restores the same resource it spends (the check's scope: actions, not traits; the declared resources: hit points, spell slots, charges)",
				"action %q restores the %s it spends — a cycle with no cost",
				a.Name, gives))
		case gives != "" && !usageBounded(a.Usage):
			out = append(out, structural(CheckMonsterCycle, subject,
				"a recharge cycle has a cost — an unlimited-use action may not restore the creature's own resources (the check's scope: actions with no usage cost; the declared resources: hit points, spell slots, charges)",
				"action %q restores %s with no usage cost (%q) — the creature can sustain it forever",
				a.Name, gives, usageDisplay(a.Usage)))
		}
	}
	return out
}

func usageDisplay(usage string) string {
	if strings.TrimSpace(usage) == "" {
		return "usable every round"
	}
	return usage
}

/* ---------- the item checks ---------- */

// lintItemStructure runs every deterministic rule over a design. The item
// designer's own validator already refuses broken designs at save time;
// the linter re-reports any problem it finds (defence in depth — nothing
// that reaches the shelf should trip these) and adds the checks the
// designer does not run.
func lintItemStructure(d items.Design) []Finding {
	var out []Finding
	for _, problem := range d.Validate() {
		out = append(out, structural(CheckItemDesign, "design",
			"the item designer's declared structural rules (attunement grammar, charge grammar, required base items, effects in the game's vocabulary)",
			"%s", problem))
	}
	out = append(out, lintItemDamageVocabulary(d)...)
	out = append(out, lintItemConditions(d)...)
	out = append(out, lintItemRechargeCycle(d)...)
	return out
}

// lintItemDamageVocabulary checks effect damage expressions against the
// game's damage types. The designer validates the dice; the type word
// after them is the linter's rule.
func lintItemDamageVocabulary(d items.Design) []Finding {
	var out []Finding
	for i, e := range d.Effects {
		eDamage := strings.TrimSpace(e.Damage)
		if eDamage == "" {
			continue
		}
		fields := strings.Fields(strings.ToLower(eDamage))
		if len(fields) < 2 {
			continue // the designer's validator already flagged the dice
		}
		if !inVocabulary(fields[1], DamageTypes) {
			out = append(out, structural(CheckItemDamage, fmt.Sprintf("effects[%d]", i),
				"items deal the game's thirteen damage types ("+vocabularyList(DamageTypes)+")",
				"effect %d (%q) deals %q damage, which is not one of the game's damage types",
				i+1, strings.TrimSpace(e.Text), fields[1]))
		}
	}
	return out
}

// lintItemConditions checks that an effect's declared outcome names the
// game's vocabulary: a condition, a damage type, or damage in dice. "The
// target is weakened" names nothing the game can price — the one thing an
// outcome may not be.
func lintItemConditions(d items.Design) []Finding {
	var out []Finding
	for i, e := range d.Effects {
		if e.Save == nil {
			continue
		}
		onFail := strings.TrimSpace(e.Save.OnFail)
		if onFail == "" {
			continue
		}
		if matchCondition(onFail) != "" || matchDamageType(onFail) != "" ||
			reDiceGrammar.MatchString(strings.ToLower(onFail)) {
			continue
		}
		out = append(out, structural(CheckItemCondition, fmt.Sprintf("effects[%d]", i),
			"a save's outcome names the game's own sets — a condition ("+vocabularyList(Conditions)+
				"), a damage type ("+vocabularyList(DamageTypes)+"), or damage in dice",
			"effect %d (%q) saves against %q, which names no game condition or damage — no comparison can price it",
			i+1, strings.TrimSpace(e.Text), onFail))
	}
	return out
}

// lintItemRechargeCycle is the item-side cycle check, the same named
// scope: the only legitimate way a design regains charges is the declared
// recharge grammar. An effect that restores charges on its own trigger is
// a cycle with no cost.
func lintItemRechargeCycle(d items.Design) []Finding {
	var out []Finding
	for i, e := range d.Effects {
		if !reRestoreCharge.MatchString(e.Text) {
			continue
		}
		out = append(out, structural(CheckItemCycle, fmt.Sprintf("effects[%d]", i),
			"charges are regained only through the declared recharge grammar — an effect that restores its own charges is a cycle with no cost",
			"effect %d (%q) restores the item's charges on its own trigger, outside the declared recharge — use the recharge field for recovery, and never both",
			i+1, strings.TrimSpace(e.Text)))
	}
	// A declared recharge the grammar cannot read is already the
	// designer's rule; nothing extra here.
	return out
}

/* ---------- small helpers ---------- */

// sortedKeys returns a string map's keys in sorted order, so findings are
// deterministic however Go randomises map iteration.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
