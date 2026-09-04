package homebrew

// The linter's tests (MAD-385). The acceptance the issue sets, in order:
// every finding carries a basis; the structural checks run with no model
// and no network and are deterministic; the recharge-cycle check fires on
// hand-built cyclic abilities and stays silent on the SRD's own
// recharging abilities (the corpus harness holds that side);
// nearest-mechanic retrieval returns sensible neighbours with working
// deep links; the model pass cannot originate or alter a finding; and no
// response, at any layer, expresses a legal/illegal verdict.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/items"
	"github.com/madeofpendletonwool/grimoire/internal/statblock"
)

/* ---------- fixtures ---------- */

// cleanMonster is the hand-built CR 7 the calculator agrees with: 168
// effective hit points at the assumed AC 15, 47 damage at +4 — the same
// anchor the monster designer's tests use.
func cleanMonster() statblock.Statblock {
	return statblock.Statblock{
		Name: "Vashk, the Grave Marshal", Size: "Medium", Type: "undead",
		AC: 15, HP: 168, HitDice: "21d8+74",
		Abilities: statblock.Abilities{Str: 18, Dex: 14, Con: 18, Int: 12, Wis: 14, Cha: 16},
		ProfBonus: 3,
		Saves:     map[string]int{"wis": 5},
		Speeds:    map[string]int{"walk": 30},
		Actions: []statblock.Action{
			{Name: "Graveblade", Kind: "ACTION", Parsed: true,
				Attack: statblock.Attack{
					Name: "Graveblade", Kind: "melee", ToHit: 4, Reach: 5,
					Damage: []statblock.Damage{{Dice: "6d8+20", Avg: 47, Type: "slashing"}},
				}},
		},
	}
}

// brokenMonster trips the structural rules one way each: unknown size,
// unknown creature type, a missing and an out-of-band ability score, an
// alien save key, an unparseable usage string, a damage clause naming no
// game type, and a save against an ability that does not exist.
func brokenMonster() statblock.Statblock {
	return statblock.Statblock{
		Name: "The Abomination", Size: "Colossal", Type: "abomination",
		AC: 40, HP: 0,
		Abilities: statblock.Abilities{Str: 30, Dex: 10, Con: 14, Int: 0, Wis: 12, Cha: 8},
		Saves:     map[string]int{"luck": 3},
		Speeds:    map[string]int{"walk": 30, "teleport": 60},
		Resist:    []string{"spoon damage"},
		Actions: []statblock.Action{
			{Name: "Gaze", Kind: "ACTION", Usage: "whenever it feels like it",
				Parsed: true,
				Attack: statblock.Attack{Kind: "save", SaveDC: 15, SaveAbility: "Coolness",
					Damage: []statblock.Damage{{Avg: 22, Type: "spoon"}}}},
		},
	}
}

// corpusShelf is a hand-checked slice of the SRD shelf: +3 weapons exist
// only at Legendary, +1 at Uncommon, +2 at Rare; the highest save DC on
// the shelf is 18.
func corpusShelf() []items.Item {
	return []items.Item{
		{Name: "Longsword +1", Type: "weapon", Rarity: "Uncommon", Base: "Longsword",
			Text: "You have a +1 bonus to attack and damage rolls made with this magic weapon."},
		{Name: "Longbow +1", Type: "weapon", Rarity: "Uncommon", Base: "Longbow",
			Text: "You have a +1 bonus to attack and damage rolls made with this magic weapon."},
		{Name: "Scimitar +2", Type: "weapon", Rarity: "Rare", Base: "Scimitar",
			Text: "You have a +2 bonus to attack and damage rolls made with this magic weapon."},
		{Name: "Dagger of Venom", Type: "weapon", Rarity: "Rare", Base: "Dagger",
			Text: "You have a +1 bonus to attack and damage rolls made with this magic weapon. Once per day you can unleash a 2d6 poison cloud, DC 15 Constitution save or take the poison damage."},
		{Name: "Flameshaft +3", Type: "weapon", Rarity: "Legendary", Base: "Longsword",
			Text: "You have a +3 bonus to attack and damage rolls made with this magic weapon. Its blade sheathes in 2d6 fire."},
		{Name: "Worldsplitter +3", Type: "weapon", Rarity: "Legendary", Base: "Greataxe",
			Text: "You have a +3 bonus to attack and damage rolls made with this magic weapon. Deals an extra 1d12 thunder damage."},
		{Name: "Wand of Holds", Type: "wand", Rarity: "Legendary", Charges: 7, Recharge: "1d6+1 daily at dawn",
			Text: "The target must succeed on a DC 18 Wisdom saving throw or be paralyzed for 1 minute. It can repeat the save at the end of each of its turns."},
		{Name: "Potion of Climbing", Type: "potion", Rarity: "Common",
			Text: "You gain a climbing speed for 1 hour."},
	}
}

/* ---------- the basis contract ---------- */

// TestEveryFindingCarriesABasis is the acceptance the issue sets first: a
// finding cannot exist without its computed arithmetic, its declared rule,
// or its corpus citation. The fixture battery covers every origin.
func TestEveryFindingCarriesABasis(t *testing.T) {
	e := &Engine{} // no index, no model: structural + computed only
	inputs := []struct {
		name string
		run  func() []Finding
	}{
		{"broken monster", func() []Finding {
			return e.LintMonster(context.Background(),
				MonsterInput{Statblock: brokenMonster(), RequestedCR: "12"}).Findings
		}},
		{"cyclic monster", func() []Finding {
			sb := cleanMonster()
			sb.Actions = append(sb.Actions, statblock.Action{
				Name: "Siphon", Kind: "ACTION", Usage: "at will",
				Desc: "Vashk regains 15 hit points at the start of its next turn.",
			})
			return e.LintMonster(context.Background(), MonsterInput{Statblock: sb}).Findings
		}},
		{"disagreeing item", func() []Finding {
			d := items.Design{Name: "Shard +3", Type: "weapon", Base: "Dagger", Rarity: "Uncommon", Bonus: 3}
			return e.LintItem(context.Background(), ItemInput{Design: d, Corpus: corpusShelf()}).Findings
		}},
		{"unmatched item", func() []Finding {
			d := items.Design{
				Name: "Orb of Dread", Type: "wondrous item", Rarity: "Rare",
				Effects: []items.Effect{{
					Text: "The target must succeed on a DC 30 Wisdom saving throw or be frightened.",
					Save: &items.EffectSave{DC: 30, Ability: "wisdom", OnFail: "frightened"},
				}},
			}
			return e.LintItem(context.Background(), ItemInput{Design: d, Corpus: corpusShelf()}).Findings
		}},
	}
	for _, in := range inputs {
		findings := in.run()
		if len(findings) == 0 {
			t.Errorf("%s: expected findings, got none — the battery must exercise the basis contract", in.name)
		}
		for _, f := range findings {
			if !f.hasBasis() {
				t.Errorf("%s: finding %s carries no basis: %+v", in.name, f.Check, f)
			}
			if f.Message == "" || f.Check == "" || f.Severity == "" {
				t.Errorf("%s: finding %s is missing its shape: %+v", in.name, f.Check, f)
			}
			switch f.Severity {
			case SeverityError, SeverityWarning, SeverityNote:
			default:
				t.Errorf("%s: finding %s carries unknown severity %q", in.name, f.Check, f.Severity)
			}
			switch f.Basis.Origin {
			case OriginComputed, OriginStructural, OriginRetrieved:
			default:
				t.Errorf("%s: finding %s carries unknown origin %q", in.name, f.Check, f.Basis.Origin)
			}
		}
	}
}

/* ---------- structural determinism, no model, no network ---------- */

// TestStructuralDeterministicNoModel runs the whole engine with neither
// index nor model — the structural and computed checks must answer alone,
// identically, twice.
func TestStructuralDeterministicNoModel(t *testing.T) {
	e := &Engine{}
	in := MonsterInput{Statblock: brokenMonster(), RequestedCR: "12"}
	first, err := json.Marshal(e.LintMonster(context.Background(), in))
	if err != nil {
		t.Fatal(err)
	}
	second, err := json.Marshal(e.LintMonster(context.Background(), in))
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("lint is not deterministic:\n%s\n%s", first, second)
	}
	var rep Report
	if err := json.Unmarshal(first, &rep); err != nil {
		t.Fatal(err)
	}
	structural := 0
	for _, f := range rep.Findings {
		if f.Basis.Origin == OriginStructural {
			structural++
		}
	}
	if structural == 0 {
		t.Fatal("the broken monster tripped no structural rule")
	}
}

// TestCleanMonsterStaysClean: a monster the calculator agrees with, in
// the game's own vocabulary, raises no structural and no CR findings.
func TestCleanMonsterStaysClean(t *testing.T) {
	e := &Engine{}
	rep := e.LintMonster(context.Background(), MonsterInput{Statblock: cleanMonster(), RequestedCR: "7"})
	for _, f := range rep.Findings {
		t.Errorf("clean monster raised %s/%s: %s", f.Severity, f.Check, f.Message)
	}
}

/* ---------- the structural rules, one each ---------- */

func findingChecks(rep *Report) map[string]int {
	out := map[string]int{}
	for _, f := range rep.Findings {
		out[f.Check]++
	}
	return out
}

func TestBrokenMonsterTripsEachStructuralRule(t *testing.T) {
	e := &Engine{}
	checks := findingChecks(e.LintMonster(context.Background(), MonsterInput{Statblock: brokenMonster()}))
	for _, want := range []string{
		CheckMonsterIdentity,    // Colossal / abomination
		CheckMonsterScores,      // Int 0 missing, Str 30 boundary ok, AC 40 defense
		CheckMonsterSaves,       // luck save key
		CheckMonsterDefense,     // AC 40, HP 0
		CheckMonsterMovement,    // teleport speed
		CheckMonsterDamage,      // "spoon damage", "spoon"
		CheckMonsterSaveAbility, // Coolness
		CheckMonsterUsage,       // "whenever it feels like it"
	} {
		if checks[want] == 0 {
			t.Errorf("expected a %s finding; got %+v", want, checks)
		}
	}
	// Severity: structural means error.
	for _, f := range e.LintMonster(context.Background(), MonsterInput{Statblock: brokenMonster()}).Findings {
		if f.Basis.Origin == OriginStructural && f.Severity != SeverityError {
			t.Errorf("%s: structural finding is %s, want error", f.Check, f.Severity)
		}
	}
}

/* ---------- the recharge cycle ---------- */

func TestRechargeCycleFiresOnHandBuiltCycles(t *testing.T) {
	cases := []struct {
		name   string
		action statblock.Action
		want   string // the resource the message names
	}{
		{"at-will self-heal", statblock.Action{
			Name: "Siphon", Kind: "ACTION", Usage: "at will",
			Desc: "Vashk regains 15 hit points at the start of its next turn.",
		}, "hit points"},
		{"unbounded slot engine", statblock.Action{
			Name: "Well of Magic", Kind: "ACTION",
			Desc: "Vashk regains one expended spell slot.",
		}, "spell slots"},
		{"spends and restores its own resource", statblock.Action{
			Name: "Consume and Renew", Kind: "ACTION", Usage: "2/day",
			Desc: "The adept expends one spell slot and then regains one expended spell slot.",
		}, "spell slots"},
	}
	for _, tc := range cases {
		sb := cleanMonster()
		sb.Actions = append(sb.Actions, tc.action)
		findings := lintMonsterRechargeCycle(sb)
		if len(findings) != 1 {
			t.Errorf("%s: expected exactly one cycle finding, got %d (%+v)", tc.name, len(findings), findings)
			continue
		}
		f := findings[0]
		if f.Check != CheckMonsterCycle || f.Severity != SeverityError {
			t.Errorf("%s: wrong finding %+v", tc.name, f)
		}
		if !strings.Contains(f.Message, tc.want) {
			t.Errorf("%s: message %q does not name the resource %q", tc.name, f.Message, tc.want)
		}
	}
}

// TestRechargeCycleStaysSilentOnBoundedAbilities is half the contract:
// the SRD's own recharging abilities — the recharge roll, the per-day
// budget, the regeneration trait, the healing word — raise nothing.
func TestRechargeCycleStaysSilentOnBoundedAbilities(t *testing.T) {
	sb := cleanMonster()
	sb.Traits = append(sb.Traits, statblock.Action{
		Name: "Regeneration", Desc: "Vashk regains 15 hit points at the start of its turn if it has at least 1 hit point.",
	})
	sb.Traits = append(sb.Traits, statblock.Action{
		Name: "Healing Touch", Desc: "The target regains 4d8+4 hit points.",
	})
	sb.Actions = append(sb.Actions,
		statblock.Action{
			Name: "Fire Breath", Kind: "ACTION", Usage: "recharge 5-6",
			Desc: "Each creature in a 30-foot cone takes 3d6 fire damage. Vashk regains nothing; it simply cannot use this again until the roll succeeds.",
		},
		statblock.Action{
			Name: "Chill Touch", Kind: "ACTION", Usage: "3/day",
			Desc: "Ranged spell attack: +7 to hit. Hit: 9 (2d8) necrotic damage.",
		},
		statblock.Action{
			Name: "Crypt Claw (Costs 2 Actions)", Kind: "LEGENDARY_ACTION", Cost: 2,
			Desc: "Vashk makes one Graveblade attack and regains 5 hit points.",
		},
	)
	if findings := lintMonsterRechargeCycle(sb); len(findings) != 0 {
		t.Fatalf("bounded abilities tripped the cycle check: %+v", findings)
	}
	// And the healing handed to others never counts, even unbounded.
	sb.Actions = append(sb.Actions, statblock.Action{
		Name: "Gift of the Grave", Kind: "ACTION", Usage: "at will",
		Desc: "One friendly creature within 30 feet regains 10 hit points.",
	})
	// Wait: that text reads as restoration aimed elsewhere — covered by
	// the other-subject guard, so still silent.
	if findings := lintMonsterRechargeCycle(sb); len(findings) != 0 {
		t.Fatalf("other-directed healing tripped the cycle check: %+v", findings)
	}
}

/* ---------- the CR arithmetic findings ---------- */

func TestCRDisagreementCarriesTheShortfall(t *testing.T) {
	e := &Engine{}
	sb := cleanMonster() // computes CR 7
	sb.HP = 30           // defensive half collapses
	rep := e.LintMonster(context.Background(), MonsterInput{Statblock: sb, RequestedCR: "7"})
	var found *Finding
	for i := range rep.Findings {
		if rep.Findings[i].Check == CheckMonsterCR {
			found = &rep.Findings[i]
		}
	}
	if found == nil {
		t.Fatal("expected a cr_disagrees finding")
	}
	if found.Severity != SeverityWarning {
		t.Errorf("disagreement severity %q, want warning", found.Severity)
	}
	if !strings.Contains(found.Message, "CR 7") || !strings.Contains(found.Message, "computes CR") {
		t.Errorf("message does not name the disagreement: %s", found.Message)
	}
	if !strings.Contains(found.Basis.Arithmetic, "defensive CR") ||
		!strings.Contains(found.Basis.Arithmetic, "offensive CR") {
		t.Errorf("arithmetic does not carry the split: %s", found.Basis.Arithmetic)
	}
}

func TestLowConfidenceIsANote(t *testing.T) {
	e := &Engine{}
	sb := cleanMonster()
	sb.Actions = append(sb.Actions, statblock.Action{
		Name: "Weird Rite", Kind: "ACTION",
		Desc: "The rites of the grave fill the room with nameless dread.",
	})
	rep := e.LintMonster(context.Background(), MonsterInput{Statblock: sb})
	var found *Finding
	for i := range rep.Findings {
		if rep.Findings[i].Check == CheckMonsterConfidence {
			found = &rep.Findings[i]
		}
	}
	if found == nil {
		t.Fatal("an unparsed action should lower confidence into a finding")
	}
	if found.Severity != SeverityNote {
		t.Errorf("confidence finding severity %q, want note", found.Severity)
	}
	if !strings.Contains(found.Message, "Weird Rite") {
		t.Errorf("confidence note does not name the unparsed action: %s", found.Message)
	}
}

/* ---------- the item's numbers ---------- */

func TestRarityDisagreementIsACheckableClaim(t *testing.T) {
	e := &Engine{}
	d := items.Design{Name: "Shard of the Flame", Type: "weapon", Base: "Dagger",
		Rarity: "Uncommon", Bonus: 3}
	rep := e.LintItem(context.Background(), ItemInput{Design: d, Corpus: corpusShelf()})
	var found *Finding
	for i := range rep.Findings {
		if rep.Findings[i].Check == CheckItemRarity {
			found = &rep.Findings[i]
		}
	}
	if found == nil {
		t.Fatalf("expected a rarity disagreement; got %+v", rep.Findings)
	}
	if found.Severity != SeverityWarning {
		t.Errorf("rarity disagreement severity %q, want warning", found.Severity)
	}
	// The claim must be checkable: named counts and rarities.
	if !strings.Contains(found.Basis.Arithmetic, "of 8 SRD items") {
		t.Errorf("arithmetic does not state its counts: %s", found.Basis.Arithmetic)
	}
	if !strings.Contains(found.Message, "Legendary") {
		t.Errorf("message does not name the rarities that carry it: %s", found.Message)
	}
}

func TestRarityAgreementStaysSilent(t *testing.T) {
	e := &Engine{}
	d := items.Design{Name: "Longsword +1", Type: "weapon", Base: "Longsword",
		Rarity: "Uncommon", Bonus: 1}
	rep := e.LintItem(context.Background(), ItemInput{Design: d, Corpus: corpusShelf()})
	for _, f := range rep.Findings {
		if f.Check == CheckItemRarity || f.Check == CheckItemUnmatched {
			t.Errorf("an on-shelf design tripped %s: %s", f.Check, f.Message)
		}
	}
}

func TestUnmatchedMetricIsANote(t *testing.T) {
	e := &Engine{}
	d := items.Design{
		Name: "Orb of Dread", Type: "wondrous item", Rarity: "Rare",
		Effects: []items.Effect{{
			Text: "The target must succeed on a DC 30 Wisdom saving throw or be frightened.",
			Save: &items.EffectSave{DC: 30, Ability: "wisdom", OnFail: "frightened"},
		}},
	}
	rep := e.LintItem(context.Background(), ItemInput{Design: d, Corpus: corpusShelf()})
	var found *Finding
	for i := range rep.Findings {
		if rep.Findings[i].Check == CheckItemUnmatched {
			found = &rep.Findings[i]
		}
	}
	if found == nil {
		t.Fatalf("expected an unmatched note; got %+v", rep.Findings)
	}
	if found.Severity != SeverityNote {
		t.Errorf("unmatched severity %q, want note", found.Severity)
	}
}

/* ---------- the item's structural rules ---------- */

func TestItemStructuralFindings(t *testing.T) {
	e := &Engine{}
	d := items.Design{
		Name: "The Weakened Blade", Type: "weapon", Base: "Longsword", Rarity: "Rare",
		Charges: 3, Recharge: "3 daily at dawn",
		Effects: []items.Effect{
			{Text: "The target is weakened.", Save: &items.EffectSave{DC: 14, Ability: "constitution", OnFail: "its weapons turn to lead"}},
			{Text: "On a hit, deals 2d6 spoondamage.", Damage: "2d6 spoondamage"},
			{Text: "When you score a critical hit, the weapon regains 3 expended charges."},
		},
	}
	checks := findingChecks(e.LintItem(context.Background(), ItemInput{Design: d, Corpus: corpusShelf()}))
	for _, want := range []string{
		CheckItemDesign,    // "The target is weakened." — no game vocabulary
		CheckItemDamage,    // spoondamage
		CheckItemCondition, // "its weapons turn to lead"
		CheckItemCycle,     // an effect restoring its own charges
	} {
		if checks[want] == 0 {
			t.Errorf("expected a %s finding; got %+v", want, checks)
		}
	}
}

func TestItemCycleStaysSilentOnDeclaredRecharge(t *testing.T) {
	e := &Engine{}
	d := items.Design{
		Name: "Wand of the Vigil", Type: "wand", Rarity: "Rare",
		Charges: 7, Recharge: "1d6+1 daily at dawn",
		Effects: []items.Effect{{
			Text: "The target must succeed on a DC 17 Wisdom saving throw or be restrained.",
			Save: &items.EffectSave{DC: 17, Ability: "wisdom", OnFail: "restrained for 1 minute"},
		}},
	}
	rep := e.LintItem(context.Background(), ItemInput{Design: d, Corpus: corpusShelf()})
	for _, f := range rep.Findings {
		if f.Basis.Origin == OriginStructural {
			t.Errorf("a sound declared-recharge design tripped %s: %s", f.Check, f.Message)
		}
	}
}

/* ---------- retrieval ---------- */

func TestRetrievalReturnsSensibleNeighbours(t *testing.T) {
	store := openLintIndex(t)
	e := &Engine{Index: store}
	rep := e.LintMonster(context.Background(), MonsterInput{Statblock: statblockVampire()})
	if len(rep.Neighbours) == 0 {
		t.Fatal("retrieval found no neighbours for a vampire-shaped monster")
	}
	if rep.Neighbours[0].Title != "Vampire" {
		t.Errorf("top neighbour %q, want the corpus's Vampire", rep.Neighbours[0].Title)
	}
	// The deep link: the citation carries the corpus and the record number
	// the reader API resolves.
	var nearest *Finding
	for i := range rep.Findings {
		if rep.Findings[i].Check == CheckNearest {
			nearest = &rep.Findings[i]
		}
	}
	if nearest == nil {
		t.Fatal("expected a retrieved finding")
	}
	c := nearest.Basis.Citation
	if c == nil || c.Corpus != "dnd" || c.Number == "" || c.Title == "" {
		t.Fatalf("citation is not a deep link: %+v", nearest.Basis)
	}
	if !strings.Contains(c.Snippet, "Legendary Resistance") {
		t.Errorf("snippet is not from the corpus record: %q", c.Snippet)
	}
}

func TestRetrievalDegradesToANotice(t *testing.T) {
	e := &Engine{} // no index wired
	rep := e.LintMonster(context.Background(), MonsterInput{Statblock: cleanMonster()})
	if len(rep.Neighbours) != 0 {
		t.Fatal("no index wired, yet neighbours appeared")
	}
}

/* ---------- the model pass ---------- */

type fakeModel struct {
	responses []string
	calls     []string
}

func (m *fakeModel) ModelName() string { return "fake-linter" }

func (m *fakeModel) Complete(ctx context.Context, system, user string) (Completion, error) {
	m.calls = append(m.calls, system+"\n---\n"+user)
	i := len(m.calls) - 1
	if i >= len(m.responses) {
		return Completion{}, context.DeadlineExceeded
	}
	return Completion{Text: m.responses[i], InputTokens: 50, OutputTokens: 50}, nil
}

// TestModelCannotOriginateOrAlterFindings is the acceptance with teeth:
// the fake model hands back a findings array, a verdict, and an invented
// number — and the report's findings are still exactly the engine's, with
// the write-up rejected in full.
func TestModelCannotOriginateOrAlterFindings(t *testing.T) {
	engineFindings := (&Engine{}).LintMonster(context.Background(),
		MonsterInput{Statblock: cleanMonster(), RequestedCR: "7"}).Findings
	if len(engineFindings) != 0 {
		t.Fatal("the clean fixture should raise no findings; the fixture is part of the test")
	}

	sb := cleanMonster()
	sb.HP = 30 // force a disagreement so the findings are non-trivial
	model := &fakeModel{responses: []string{
		`Findings: [{"check":"monster.structure.identity","severity":"error","message":"invented"}] This creature is totally illegal. Its damage per round of 9999 is far too high.`,
	}}
	e := &Engine{Model: model}
	rep := e.LintMonster(context.Background(), MonsterInput{Statblock: sb, RequestedCR: "7"})

	if rep.WrittenUp != WriteUpRejected {
		t.Fatalf("write-up state %q, want rejected", rep.WrittenUp)
	}
	if rep.WriteUp != "" {
		t.Fatalf("a rejected write-up must not be shown: %q", rep.WriteUp)
	}
	if !strings.Contains(rep.WriteUpNote, "illegal") {
		t.Errorf("the rejection note does not name the verdict language: %s", rep.WriteUpNote)
	}
	if !strings.Contains(rep.WriteUpNote, "9999") {
		t.Errorf("the rejection note does not name the invented figure: %s", rep.WriteUpNote)
	}
	// The findings are the engine's, unaltered: the model's JSON blob
	// appears nowhere.
	if len(rep.Findings) != 1 || rep.Findings[0].Check != CheckMonsterCR {
		t.Fatalf("the model's invented finding reached the report: %+v", rep.Findings)
	}
	for _, f := range rep.Findings {
		if strings.Contains(f.Message, "invented") {
			t.Fatalf("model text reached the findings: %+v", f)
		}
	}
	// And the model was shown the engine context, not asked for findings.
	if len(model.calls) != 1 || !strings.Contains(model.calls[0], writeUpSystemPrompt[:40]) {
		t.Fatalf("the model was not prompted as expected: %+v", model.calls)
	}
}

// TestModelWriteUpPassesWhenGrounded: prose built from the engine's own
// numbers and the neighbours' titles passes the gate and is shown.
func TestModelWriteUpPassesWhenGrounded(t *testing.T) {
	sb := cleanMonster()
	sb.HP = 30
	model := &fakeModel{responses: []string{
		`This reads like a crypt commander whose armour outlives its sword: the defensive half holds, the offensive half falls 1 step short. Against the shelf, the nearest material is Vashk's own printed relatives — expect the DM to add a second Graveblade rather than more hit points.`,
	}}
	e := &Engine{Model: model}
	rep := e.LintMonster(context.Background(), MonsterInput{Statblock: sb, RequestedCR: "7"})
	if rep.WrittenUp != WriteUpWritten {
		t.Fatalf("write-up state %q (%s), want written", rep.WrittenUp, rep.WriteUpNote)
	}
	if rep.WriteUp == "" {
		t.Fatal("a written write-up must be shown")
	}
}

func TestWriteUpUnavailableWithoutModel(t *testing.T) {
	e := &Engine{}
	rep := e.LintMonster(context.Background(), MonsterInput{Statblock: cleanMonster()})
	if rep.WrittenUp != WriteUpUnavailable || rep.WriteUp != "" {
		t.Fatalf("no model configured, yet state %q with prose %q", rep.WrittenUp, rep.WriteUp)
	}
}

func TestCheckWriteUpGate(t *testing.T) {
	sb := cleanMonster()
	sb.HP = 30
	rep := (&Engine{}).LintMonster(context.Background(), MonsterInput{Statblock: sb, RequestedCR: "7"})
	engineJSON := contextJSON(rep, "7", statblock.ComputeCR(sb))

	good := "The defensive half is CR 1/8 — 30 hit points against AC 15 — while the offense is CR 6 on 47 damage per round, so the calculator lands the whole creature at CR 3 against the requested CR 7."
	if v := CheckWriteUp(engineJSON, good); len(v) != 0 {
		t.Errorf("grounded prose rejected: %+v", v)
	}
	for _, bad := range []string{
		"This monster is perfectly legal for any table.",
		"The save DC of 27 is unusual for the CR.",
		"Its 2d12 rider damage (average 15) is fine, but the item grants 9999 charges.",
	} {
		if v := CheckWriteUp(engineJSON, bad); len(v) == 0 {
			t.Errorf("ungrounded prose passed the gate: %s", bad)
		}
	}
	if v := CheckWriteUp(engineJSON, ""); len(v) == 0 {
		t.Error("empty prose passed the gate")
	}
}

/* ---------- no verdict, anywhere ---------- */

// TestNoVerdictAtAnyLayer walks the marshalled response and asserts there
// is no key — and no shape — by which a legal/illegal verdict could be
// expressed.
func TestNoVerdictAtAnyLayer(t *testing.T) {
	e := &Engine{Model: &fakeModel{responses: []string{
		"This is legal, enjoy. It deals 47 damage per round.",
	}}}
	reports := []*Report{
		e.LintMonster(context.Background(), MonsterInput{Statblock: brokenMonster(), RequestedCR: "12"}),
		e.LintItem(context.Background(), ItemInput{Design: items.Design{
			Name: "Shard +3", Type: "weapon", Base: "Dagger", Rarity: "Uncommon", Bonus: 3,
		}, Corpus: corpusShelf()}),
	}
	for _, rep := range reports {
		if rep.WrittenUp == WriteUpWritten {
			t.Errorf("verdict prose passed the gate: %q", rep.WriteUp)
		}
		raw, err := json.Marshal(rep)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		for k := range m {
			low := strings.ToLower(k)
			if strings.Contains(low, "legal") || strings.Contains(low, "verdict") ||
				strings.Contains(low, "valid") {
				t.Errorf("response carries a verdict-shaped key: %s", k)
			}
		}
		// The findings never call anything legal or illegal either.
		for _, f := range rep.Findings {
			low := strings.ToLower(f.Message)
			if strings.Contains(low, "legal") {
				t.Errorf("finding %s expresses a verdict: %s", f.Check, f.Message)
			}
		}
	}
}
