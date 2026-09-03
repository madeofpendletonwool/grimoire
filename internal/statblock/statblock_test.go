package statblock

import (
	"reflect"
	"testing"
)

func TestParseAttack2014Melee(t *testing.T) {
	atk, ok := ParseAttack("Scimitar",
		"Melee Weapon Attack: +4 to hit, reach 5 ft., one target. Hit: 5 (1d6+2) slashing damage.")
	if !ok {
		t.Fatal("a plain 2014 melee attack must parse")
	}
	want := Attack{
		Name: "Scimitar", Kind: KindMelee, ToHit: 4, Reach: 5,
		Targets: "one target",
		Damage:  []Damage{{Dice: "1d6+2", Avg: 5, Type: "slashing"}},
	}
	if !reflect.DeepEqual(atk, want) {
		t.Errorf("got %+v\nwant %+v", atk, want)
	}
}

func TestParseAttack2024Weapon(t *testing.T) {
	atk, ok := ParseAttack("Rend",
		"Melee Attack Roll: +14, reach 10 ft. 13 (1d10 + 8) Slashing damage plus 5 (2d4) Fire damage.")
	if !ok {
		t.Fatal("a 2024 weapon attack must parse")
	}
	if atk.Kind != KindMelee || atk.ToHit != 14 || atk.Reach != 10 {
		t.Errorf("delivery = %+v", atk)
	}
	if len(atk.Damage) != 2 {
		t.Fatalf("damage = %+v, want two components", atk.Damage)
	}
	if atk.Damage[0].Avg != 13 || atk.Damage[0].Dice != "1d10+8" || atk.Damage[0].Type != "slashing" {
		t.Errorf("primary = %+v", atk.Damage[0])
	}
	if atk.Damage[1].Avg != 5 || atk.Damage[1].Dice != "2d4" || atk.Damage[1].Type != "fire" {
		t.Errorf("secondary = %+v", atk.Damage[1])
	}
}

func TestParseAttackRangedRange(t *testing.T) {
	atk, ok := ParseAttack("Shortbow",
		"Ranged Weapon Attack: +4 to hit, range 30/120 ft., one target. Hit: 5 (1d6+2) piercing damage.")
	if !ok {
		t.Fatal("ranged must parse")
	}
	if atk.Kind != KindRanged || atk.Range != 30 || atk.ToHit != 4 {
		t.Errorf("got %+v", atk)
	}
}

func TestParseAttackSave2014(t *testing.T) {
	atk, ok := ParseAttack("Poison Breath",
		"The dragon exhales poison gas in a 30-foot cone. Each creature in that area must succeed on a DC 15 Constitution saving throw, taking 42 (12d6) poison damage on a failed save, or half as much damage on a successful one.")
	if !ok {
		t.Fatal("a 2014 breath must parse")
	}
	if atk.Kind != KindSave || atk.SaveDC != 15 || atk.SaveAbility != "Con" {
		t.Errorf("delivery = %+v", atk)
	}
	if !atk.Area {
		t.Error("a cone must be marked area")
	}
	if atk.Targets != "each creature in that area" {
		t.Errorf("targets = %q, want the phrase without the save clause", atk.Targets)
	}
	if len(atk.Damage) != 1 || atk.Damage[0].Avg != 42 || atk.Damage[0].Type != "poison" {
		t.Errorf("damage = %+v", atk.Damage)
	}
}

func TestParseAttackSave2024(t *testing.T) {
	atk, ok := ParseAttack("Fire Breath",
		"Dexterity Saving Throw: DC 21, each creature in a 60-foot Cone. Failure: 59 (17d6) Fire damage. Success: Half damage.")
	if !ok {
		t.Fatal("a 2024 breath must parse")
	}
	if atk.Kind != KindSave || atk.SaveAbility != "Dex" || atk.SaveDC != 21 {
		t.Errorf("delivery = %+v", atk)
	}
	if !atk.Area || atk.Targets != "each creature in a 60-foot cone" {
		t.Errorf("area/targets = %v/%q", atk.Area, atk.Targets)
	}
	if len(atk.Damage) != 1 || atk.Damage[0].Avg != 59 {
		t.Errorf("damage = %+v", atk.Damage)
	}
	// The "Success: Half damage." clause is bookkeeping, not a rider.
	if atk.Rider != "" {
		t.Errorf("rider = %q, want empty", atk.Rider)
	}
}

func TestParseAttackControlSave(t *testing.T) {
	// A save with no damage is a complete parse of a control effect; the
	// effect text rides in Rider.
	atk, ok := ParseAttack("Frightening Gaze",
		"The lich targets one creature it can see within 120 ft. of it. The target must succeed on a DC 18 Wisdom saving throw or be frightened for 1 minute.")
	if !ok {
		t.Fatal("a damageless control save must parse")
	}
	if atk.SaveDC != 18 || atk.SaveAbility != "Wis" || len(atk.Damage) != 0 {
		t.Errorf("got %+v", atk)
	}
	if atk.Rider == "" {
		t.Error("the control effect must ride in Rider")
	}
}

func TestParseAttackRider(t *testing.T) {
	atk, ok := ParseAttack("Tentacle",
		"Melee Weapon Attack: +9 to hit, reach 10 ft., one target. Hit: 12 (2d6 + 5) bludgeoning damage. If the target is a creature, it must succeed on a DC 14 Constitution saving throw or become diseased.")
	if !ok {
		t.Fatal("tentacle must parse")
	}
	if atk.Damage[0].Type != "bludgeoning" {
		t.Errorf("type = %q", atk.Damage[0].Type)
	}
	if atk.Rider != "if the target is a creature, it must succeed on a dc 14 constitution saving throw or become diseased" {
		t.Errorf("rider = %q", atk.Rider)
	}
}

func TestParseMultiattack(t *testing.T) {
	cases := []struct {
		desc string
		want []Component
	}{
		{"The dragon makes three attacks: one with its bite and two with its claws.",
			[]Component{{1, "bite", nil}, {2, "claws", nil}}},
		{"The dragon makes three Rend attacks.",
			[]Component{{3, "rend", nil}}},
		{"The hobgoblin makes two longsword attacks.",
			[]Component{{2, "longsword", nil}}},
		{"The gorgon makes two attacks: one with its gore and one with its hooves.",
			[]Component{{1, "gore", nil}, {1, "hooves", nil}}},
		// The 2024 SRD's choice form: N attacks, picking among options.
		{"The golem makes two attacks, using Bladed Arm or Fiery Bolt in any combination.",
			[]Component{{2, "", []string{"bladed arm", "fiery bolt"}}}},
		{"The lich makes three attacks, using Eldritch Burst or Paralyzing Touch in any combination.",
			[]Component{{3, "", []string{"eldritch burst", "paralyzing touch"}}}},
		// Named attack plus a rider action in the same round.
		{"The marilith makes six Pact Blade attacks and uses Constrict.",
			[]Component{{6, "pact blade", nil}, {1, "constrict", nil}}},
		{"The werewolf makes two attacks, using Scratch or Longbow in any combination. It can replace one attack with a Bite attack.",
			[]Component{{2, "", []string{"scratch", "longbow"}}}},
	}
	for _, tc := range cases {
		atk, ok := ParseAttack("Multiattack", tc.desc)
		if !ok {
			t.Errorf("%q must parse", tc.desc)
			continue
		}
		if atk.Kind != KindMultiattack || !reflect.DeepEqual(atk.Components, tc.want) {
			t.Errorf("%q -> %+v, want %+v", tc.desc, atk.Components, tc.want)
		}
	}
}

func TestParseAttackUnparsed(t *testing.T) {
	cases := map[string]string{
		"no attack marker": "The aboleth makes a Wisdom (Perception) check.",
		"references only":  "The aboleth makes one tail attack.",
		"spellcasting":     "The dragon casts one of the following spells, requiring no Material components and using Charisma as the spellcasting ability (spell save DC 20, +12 to hit with spell attacks): At Will: Command, Detect Magic",
		"movement":         "The sphinx magically teleports, along with any equipment it is wearing or carrying, up to 120 feet to an unoccupied space it can see.",
	}
	for label, desc := range cases {
		if _, ok := ParseAttack("Some Action", desc); ok {
			t.Errorf("%s must stay unparsed", label)
		}
	}
}

func TestParseAttackDeterministic(t *testing.T) {
	desc := "Melee Weapon Attack: +9 to hit, reach 10 ft., one target. Hit: 12 (2d6 + 5) bludgeoning damage."
	first, _ := ParseAttack("Tail", desc)
	for i := 0; i < 50; i++ {
		again, ok := ParseAttack("Tail", desc)
		if !ok || !reflect.DeepEqual(first, again) {
			t.Fatalf("parse %d differs: %+v vs %+v", i, first, again)
		}
	}
}

/* ---------- ComputeCR ---------- */

// goblin mirrors the SRD goblin closely enough to pin the arithmetic: the
// DMG's own procedure famously does not reproduce the printed CR 1/4
// exactly — see the corpus pins — but the halves are stable and asserted
// here so any change to the maths is a deliberate diff.
func goblinStatblock() Statblock {
	scimitar, okA := ParseAttack("Scimitar", "Melee Weapon Attack: +4 to hit, reach 5 ft., one target. Hit: 5 (1d6+2) slashing damage.")
	shortbow, okB := ParseAttack("Shortbow", "Ranged Weapon Attack: +4 to hit, range 30/120 ft., one target. Hit: 5 (1d6+2) piercing damage.")
	if !okA || !okB {
		panic("goblin attacks must parse")
	}
	return Statblock{
		Name: "Goblin", AC: 15, HP: 7,
		Actions: []Action{
			{Name: "Scimitar", Parsed: true, Attack: scimitar},
			{Name: "Shortbow", Parsed: true, Attack: shortbow},
		},
	}
}

func TestComputeCRGoblinHalves(t *testing.T) {
	r := ComputeCR(goblinStatblock())
	// HP 7 -> CR 1/8 band; AC 15 against assumed 13 lifts defense two
	// half-steps to CR 1/4. Damage 5 reads CR 1/4; +4 to hit against the
	// assumed +3 adds a half-step of offense. The final average snaps to a
	// table CR one step above the printed one — the DMG procedure's own
	// bias for mundane low-CR humanoids, documented in the corpus pins.
	if r.Defensive != 0.25 {
		t.Errorf("defensive = %v, want 0.25", r.Defensive)
	}
	if r.CR != 0.5 {
		t.Errorf("cr = %v (label %q), want the documented 0.5", r.CR, r.Label)
	}
	if r.Label != "1/2" {
		t.Errorf("label = %q", r.Label)
	}
	if r.Confidence != ConfidenceHigh {
		t.Errorf("confidence = %q, want high for a fully parsed mundane statblock", r.Confidence)
	}
}

func TestComputeCRImmuneWerewolf(t *testing.T) {
	// Werewolf, printed CR 3: HP 58, AC 11, immune to nonmagical b/p/s. The
	// ×2 immunity pricing lifts effective HP to 116 and the defense to CR 3
	// — but the DMG's own procedure then averages in a CR 1 offense (one
	// claw, 7 damage) and lands on CR 2, one step below the printed rating.
	// That gap is a property of the source procedure — the books lean on the
	// shapechanger's tactics the arithmetic cannot see — and it is pinned as
	// such in the corpus harness rather than tuned away here.
	claw, _ := ParseAttack("Claw", "Melee Weapon Attack: +4 to hit, reach 5 ft., one target. Hit: 7 (2d4+2) slashing damage.")
	r := ComputeCR(Statblock{
		Name: "Werewolf", AC: 11, HP: 58,
		Immune:    []string{"bludgeoning, piercing, and slashing from nonmagical attacks that aren't silvered"},
		Abilities: Abilities{Str: 15, Dex: 13, Con: 14, Int: 10, Wis: 11, Cha: 10},
		Actions:   []Action{{Name: "Claw", Parsed: true, Attack: claw}},
	})
	if r.EffectiveHP != 116 {
		t.Errorf("effective HP = %d, want 58 doubled by the immunity multiplier", r.EffectiveHP)
	}
	if r.Defensive != 3 {
		t.Errorf("defensive = %v, want 3", r.Defensive)
	}
	if r.CR != 2 {
		t.Errorf("cr = %v, want the procedure's 2 — the printed 3 is the documented disagreement", r.CR)
	}
	for _, adj := range r.Adjustments {
		if adj.Kind == "immunity" && adj.Value != 58 {
			t.Errorf("immunity adjustment = %+v, want +58", adj)
		}
	}
}

func TestComputeCRSaveBasedOffense(t *testing.T) {
	// A save-only attacker is read against the assumed save DC column, not
	// the attack bonus column.
	breath, ok := ParseAttack("Poison Breath",
		"The creature exhales poison gas in a 15-foot cone. Each creature in that area must succeed on a DC 14 Constitution saving throw, taking 21 (6d6) poison damage on a failed save.")
	if !ok {
		t.Fatal("breath must parse")
	}
	r := ComputeCR(Statblock{
		Name: "Breather", AC: 13, HP: 60, Spellcasting: false,
		Actions: []Action{{Name: "Poison Breath", Usage: "recharge 5-6", Parsed: true, Attack: breath}},
	})
	if !r.SaveBased {
		t.Error("offense must be marked save-based")
	}
	if r.AttackBonus != 14 {
		t.Errorf("priced DC = %d, want 14", r.AttackBonus)
	}
	// A creature whose only attack is the recharge breath: it fires in round
	// 1 and the three-round average pays for it (42/3 = 14). Recharges are
	// not assumed to return — the documented conservative reading of the
	// DMG's three-round horizon.
	if r.DPR != 14 {
		t.Errorf("dpr = %d, want 14", r.DPR)
	}
}

func TestComputeCRRegeneration(t *testing.T) {
	claw, _ := ParseAttack("Claw", "Melee Weapon Attack: +4 to hit, reach 5 ft., one target. Hit: 7 (2d4+2) slashing damage.")
	r := ComputeCR(Statblock{
		Name: "Regenerator", AC: 13, HP: 90,
		Traits:  []Action{{Name: "Regeneration", Desc: "The troll regains 10 hit points at the start of its turn."}},
		Actions: []Action{{Name: "Claw", Parsed: true, Attack: claw}},
	})
	if r.EffectiveHP != 120 {
		t.Errorf("effective HP = %d, want 90 + 3x10", r.EffectiveHP)
	}
	found := false
	for _, adj := range r.Adjustments {
		if adj.Kind == "regeneration" && adj.Value == 30 {
			found = true
		}
	}
	if !found {
		t.Errorf("adjustments = %+v, want regeneration +30", r.Adjustments)
	}
}

func TestComputeCRLegendaryBudget(t *testing.T) {
	bite, _ := ParseAttack("Bite", "Melee Weapon Attack: +8 to hit, reach 5 ft., one target. Hit: 15 (2d8+6) piercing damage.")
	lclaw, _ := ParseAttack("Claw", "Melee Weapon Attack: +8 to hit, reach 5 ft., one target. Hit: 9 (1d10+4) slashing damage.")
	ltail, _ := ParseAttack("Tail", "Melee Weapon Attack: +8 to hit, reach 10 ft., one target. Hit: 11 (2d6+4) bludgeoning damage.")
	lroar, _ := ParseAttack("Roar", "Each creature of the manticore's choice within 120 feet must succeed on a DC 15 Wisdom saving throw or be frightened.")
	r := ComputeCR(Statblock{
		Name: "Legendary Cat", AC: 17, HP: 200, Legendary: true,
		Actions: []Action{
			{Name: "Bite", Parsed: true, Attack: bite},
			{Name: "Roar", Parsed: true, Attack: lroar},
			{Name: "Claw", Kind: "LEGENDARY_ACTION", Parsed: true, Attack: lclaw},
			{Name: "Tail", Kind: "LEGENDARY_ACTION", Parsed: true, Attack: ltail},
			{Name: "Wing", Kind: "LEGENDARY_ACTION", Desc: "The manticore flies up to half its speed.", Parsed: false, Unparse: "no attack marker"},
		},
	})
	// Sustainable: bite 15. Round 1: bite + nothing limited. Legendary per
	// round: tail 11 + claw 9 (the fright save prices at zero damage) = 20.
	if r.DPR != 35 {
		t.Errorf("dpr = %d, want 15 + 20 legendary per round", r.DPR)
	}
	// The legendary option that is unparsed prose caps confidence low.
	if r.Confidence != ConfidenceLow {
		t.Errorf("confidence = %q, want low when an action is unparsed", r.Confidence)
	}
	if r.CR == 0 {
		t.Error("a complete defensive profile with real offense must not rate CR 0")
	}
}

func TestComputeCRIsTotalAndDeterministic(t *testing.T) {
	// The zero statblock: no panic, a zero rating, and honest low confidence.
	zero := ComputeCR(Statblock{})
	if zero.CR != 0 || zero.Confidence != ConfidenceLow {
		t.Errorf("zero statblock = %+v", zero)
	}
	// No actions at all — the mirror carries creatures like this — computes
	// defense and admits the offense is unknown.
	noActions := ComputeCR(Statblock{Name: "Wall", AC: 17, HP: 250})
	if noActions.CR == 0 {
		t.Error("defense must still rate when there are no actions")
	}
	if noActions.Confidence != ConfidenceLow {
		t.Errorf("confidence = %q, want low without actions", noActions.Confidence)
	}

	s := goblinStatblock()
	s.Actions = append(s.Actions, Action{Name: "Weird", Desc: "something unparseable"})
	a := ComputeCR(s)
	b := ComputeCR(s)
	if !reflect.DeepEqual(a, b) {
		t.Error("ComputeCR must be deterministic")
	}
	if a.Confidence != ConfidenceLow {
		t.Errorf("confidence = %q, want low with an unparsed action", a.Confidence)
	}
}

func TestComputeCRLabels(t *testing.T) {
	cases := map[float64]string{0: "0", 0.125: "1/8", 0.25: "1/4", 0.5: "1/2", 1: "1", 17: "17", 30: "30"}
	for cr, want := range cases {
		if got := Label(cr); got != want {
			t.Errorf("Label(%v) = %q, want %q", cr, got, want)
		}
	}
}

func TestHitDiceAverage(t *testing.T) {
	cases := map[string]int{
		"18d10+36": 135,
		"2d6":      7,
		"1d4-1":    1,
		"":         0,
		"garbage":  0,
	}
	for expr, want := range cases {
		if got := hitDiceAvg(expr); got != want {
			t.Errorf("hitDiceAvg(%q) = %d, want %d", expr, got, want)
		}
	}
}

func TestResolveComponentPlural(t *testing.T) {
	s := goblinStatblock()
	if got := ResolveComponent(s, "scimitars"); got != 0 {
		t.Errorf("ResolveComponent(scimitars) = %d, want 0", got)
	}
	if got := ResolveComponent(s, "greataxe"); got != -1 {
		t.Errorf("ResolveComponent(greataxe) = %d, want -1", got)
	}
}
