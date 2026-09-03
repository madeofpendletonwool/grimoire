package encounter

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

/* ---------- fixtures ---------- */

// goblinAt returns a scimitar-and-bow goblin: melee +4 (5 dmg), ranged +4
// (5 dmg, 80 ft.), the workhorse of every aiming fixture.
func goblinAt(name string, count int) Combatant {
	return Combatant{Count: count, Creature: Creature{
		Slug: "goblin", Name: name, CR: "1/4", CRNum: 0.25, XP: 50,
		Type: "humanoid", Size: "small", AC: 15, HP: 7,
		Actions: []NamedText{
			{Name: "Scimitar", Desc: "Melee Weapon Attack: +4 to hit, reach 5 ft., one target. Hit: 5 (1d6+2) slashing damage.", Kind: "ACTION"},
			{Name: "Shortbow", Desc: "Ranged Weapon Attack: +4 to hit, range 80/320 ft., one target. Hit: 5 (1d6+2) piercing damage.", Kind: "ACTION"},
		},
	}}
}

// wizardAt is the soft pc: AC 10, 20 hp, a Con save that fails DC 13 more
// often than not.
func wizardAt(name string) PCFact {
	return PCFact{
		Name: name, Class: "wizard", Level: 5, AC: 10, MaxHP: 20,
		Saves: map[string]int{"str": 0, "dex": 3, "con": 1, "int": 7, "wis": 1, "cha": 2},
	}
}

// fighterAt is the hard pc: AC 18, 44 hp, everything decent.
func fighterAt(name string) PCFact {
	return PCFact{
		Name: name, Class: "fighter", Level: 5, AC: 18, MaxHP: 44,
		Saves: map[string]int{"str": 4, "dex": 1, "con": 3, "int": 0, "wis": 1, "cha": 1},
	}
}

/* ---------- determinism and the derivation contract ---------- */

// The whole analysis is arithmetic over its input: identical input must give
// identical output — the acceptance that makes it quotable at the table.
func TestAnalyzeIsDeterministic(t *testing.T) {
	in := richInput()
	a1 := Analyze(in)
	a2 := Analyze(in)
	if !reflect.DeepEqual(a1, a2) {
		t.Fatal("two runs over identical input disagreed")
	}
	b1, err := json.Marshal(a1)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	b2, err := json.Marshal(a2)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b1) != string(b2) {
		t.Fatal("the JSON of two runs over identical input differs — map order is leaking into the output")
	}
}

// richInput exercises every section: multiattacks, a legendary, a save
// effect, resistances, terrain and a full party block.
func richInput() TacticsInput {
	brute := Creature{
		Slug: "ogre", Name: "Ogre", CR: "2", CRNum: 2, XP: 450,
		Type: "giant", AC: 11, HP: 59, Speeds: map[string]int{"walk": 40},
		Immune: "", Resist: "",
		Actions: []NamedText{
			{Name: "Multiattack", Desc: "The ogre makes two attacks: two with its greatclub.", Kind: "ACTION"},
			{Name: "Greatclub", Desc: "Melee Weapon Attack: +6 to hit, reach 10 ft., one target. Hit: 13 (2d8+4) bludgeoning damage.", Kind: "ACTION"},
			{Name: "Club Spin", Desc: "Melee Weapon Attack: +6 to hit, reach 5 ft., one target. Hit: 14 (2d10+3) bludgeoning damage.", Kind: "LEGENDARY_ACTION", Cost: 1},
		},
	}
	shaman := Creature{
		Slug: "shaman", Name: "Ember Shaman", CR: "1", CRNum: 1, XP: 200,
		Type: "humanoid", AC: 13, HP: 22,
		Actions: []NamedText{
			{Name: "Hurl Flame", Desc: "Each creature in a 30-foot line must make a DC 13 Dexterity saving throw, taking 9 (2d8) fire damage on a failed save, and the target is blinded until the end of its next turn on a failure.", Kind: "ACTION"},
		},
	}
	return TacticsInput{
		Party: []PCFact{
			fighterAt("Berold"),
			wizardAt("Meria"),
		},
		Roster: []Combatant{
			{Count: 4, Creature: goblinAt("Goblin", 4).Creature},
			{Count: 1, Creature: brute},
			{Count: 2, Creature: shaman},
		},
		Terrain: &Terrain{Features: []Feature{
			{Kind: "difficult_ground", Effect: "costs double", Area: "the crossing"},
			{Kind: "chokepoint", Effect: "one wide", Area: "the gap"},
		}},
	}
}

// No figure may reach the UI without the derivation that produced it.
func TestEveryFigureCarriesItsDerivation(t *testing.T) {
	a := Analyze(richInput())
	figs := collectFigures(a)
	if len(figs) < 10 {
		t.Fatalf("expected a rich analysis, got %d figures", len(figs))
	}
	for i, f := range figs {
		if strings.TrimSpace(f.How) == "" {
			t.Errorf("figure %d (value %v) reaches the UI with no derivation", i, f.Value)
		}
	}
	// The allowlist the prose gate reads must be non-empty for the same
	// analysis, and every allowed number must trace to something.
	allowed := AllowedNumbers(richInput(), a)
	if len(allowed) == 0 {
		t.Fatal("AllowedNumbers came back empty for a rich analysis")
	}
	for n, how := range allowed {
		if strings.TrimSpace(how) == "" {
			t.Errorf("allowed number %d carries no derivation", n)
		}
	}
}

/* ---------- threat: the action economy ---------- */

func TestThreatPricesTheRound(t *testing.T) {
	in := richInput()
	a := Analyze(in)

	var brute *GroupThreat
	for i := range a.Threat {
		if a.Threat[i].Name == "Ogre" {
			brute = &a.Threat[i]
		}
	}
	if brute == nil {
		t.Fatal("no ogre in the threat read")
	}
	// Two greatclub attacks at 13 each, plus 3 legendary club spins at 14:
	// 26 + 42 = 68 per round over 5 uses.
	if got := brute.PerRound.Value; got != 68 {
		t.Errorf("ogre per-round = %v, want 68 (2×13 + 3×14)", got)
	}
	if brute.Attacks.Value != 5 {
		t.Errorf("ogre attacks = %v, want 5 uses", brute.Attacks.Value)
	}
	if brute.ToHit == nil || brute.ToHit.Value != 6 {
		t.Errorf("ogre to hit = %+v, want +6", brute.ToHit)
	}
	if brute.SaveDC != nil {
		t.Errorf("ogre priced as save-based: %+v", brute.SaveDC)
	}

	var shaman *GroupThreat
	for i := range a.Threat {
		if a.Threat[i].Name == "Ember Shaman" {
			shaman = &a.Threat[i]
		}
	}
	if shaman == nil {
		t.Fatal("no shaman in the threat read")
	}
	if shaman.SaveDC == nil || shaman.SaveDC.Value != 13 || shaman.SaveAbility != "DEX" {
		t.Errorf("shaman save = %+v %s, want DC 13 DEX", shaman.SaveDC, shaman.SaveAbility)
	}
	if shaman.ToHit != nil {
		t.Errorf("shaman priced as attack-based: %+v", shaman.ToHit)
	}
}

func TestLegendaryAndRechargeNotes(t *testing.T) {
	c := Creature{
		Name: "Chieftain", CR: "2", CRNum: 2, XP: 450, AC: 14, HP: 30,
		Actions: []NamedText{
			{Name: "Multiattack", Desc: "The chieftain makes two attacks: two with its axe.", Kind: "ACTION"},
			{Name: "Axe", Desc: "Melee Weapon Attack: +5 to hit, reach 5 ft., one target. Hit: 7 (1d12+1) slashing damage.", Kind: "ACTION"},
			{Name: "War Cry", Desc: "Each creature of the chieftain's choice within 60 feet must succeed on a DC 14 Wisdom saving throw or become frightened.", Usage: "Recharge 5-6", Kind: "ACTION"},
		},
	}
	p := priceGroup(Combatant{Count: 1, Creature: c})
	if p.round.perRd != 14 {
		t.Fatalf("per round = %d, want 2×7=14", p.round.perRd)
	}
}

/* ---------- focus: hand-worked fixtures ---------- */

// One obviously soft pc: the pack wants the wizard, and says why.
func TestFocusPicksTheSoftPC(t *testing.T) {
	in := TacticsInput{
		Party:  []PCFact{fighterAt("Berold"), wizardAt("Meria")},
		Roster: []Combatant{goblinAt("Goblin", 4)},
	}
	a := Analyze(in)
	if len(a.Focus) != 1 {
		t.Fatalf("focus rows = %d, want 1", len(a.Focus))
	}
	f := a.Focus[0]
	if f.Target != "Meria" {
		t.Fatalf("focus target = %q, want Meria", f.Target)
	}
	// Four goblins, 5 damage each, 75% to hit AC 10: 4×5×0.75 = 15.
	if f.PerRound == nil || f.PerRound.Value != 15 {
		t.Errorf("aimed damage = %+v, want 15", f.PerRound)
	}
	if f.Chance == nil || f.Chance.Value != 75 {
		t.Errorf("hit chance = %+v, want 75%%", f.Chance)
	}
	// Berold is considered and passed over; only Meria is aimed at.
	if f.Reason == "" || !strings.Contains(f.Reason, "softest") {
		t.Errorf("focus reason = %q, want the softest-target rationale", f.Reason)
	}
}

// Nobody soft: identical numbers resolve deterministically by name order,
// never by map luck.
func TestFocusWithoutASoftPCIsDeterministic(t *testing.T) {
	party := []PCFact{
		{Name: "Dora", Class: "fighter", Level: 5, AC: 16, MaxHP: 33},
		{Name: "Bran", Class: "cleric", Level: 5, AC: 16, MaxHP: 33},
		{Name: "Ceda", Class: "rogue", Level: 5, AC: 16, MaxHP: 33},
		{Name: "Aldric", Class: "bard", Level: 5, AC: 16, MaxHP: 33},
	}
	in := TacticsInput{Party: party, Roster: []Combatant{goblinAt("Goblin", 2)}}
	first := Analyze(in).Focus[0].Target
	if first != "Aldric" {
		t.Fatalf("focus target = %q, want the name-order tie-break Aldric", first)
	}
	for i := 0; i < 20; i++ {
		if again := Analyze(in).Focus[0].Target; again != first {
			t.Fatalf("focus moved between identical runs: %q then %q", first, again)
		}
	}
}

// A party the monsters cannot reach at all: the focus says so instead of
// picking a fictional target.
func TestFocusWhenNobodyIsReachable(t *testing.T) {
	// Melee-only brutes: no ranged attack anywhere on the sheet.
	brute := Creature{
		Name: "Cave Bear", CR: "2", CRNum: 2, XP: 450, AC: 12, HP: 40,
		Speeds: map[string]int{"walk": 30},
		Actions: []NamedText{
			{Name: "Bite", Desc: "Melee Weapon Attack: +6 to hit, reach 5 ft., one target. Hit: 8 (1d8+4) piercing damage.", Kind: "ACTION"},
			{Name: "Claws", Desc: "Melee Weapon Attack: +6 to hit, reach 5 ft., one target. Hit: 11 (2d6+4) slashing damage.", Kind: "ACTION"},
		},
	}
	party := []PCFact{
		{Name: "Berold", Class: "fighter", Level: 5, AC: 18, MaxHP: 44, Distance: 100},
		{Name: "Meria", Class: "wizard", Level: 5, AC: 10, MaxHP: 20, Distance: 120},
	}
	a := Analyze(TacticsInput{Party: party, Roster: []Combatant{{Count: 1, Creature: brute}}})
	f := a.Focus[0]
	if f.Target != "" {
		t.Fatalf("focus target = %q, want nobody — the board never closes", f.Target)
	}
	if len(f.Holdouts) != 2 {
		t.Fatalf("holdouts = %v, want both pcs named as unreachable", f.Holdouts)
	}
	if !strings.Contains(f.Holdouts[0], "ft. away") {
		t.Errorf("holdout reason = %q, want the distance stated", f.Holdouts[0])
	}
}

/* ---------- attrition: the asymmetry ---------- */

func TestAttritionBothWays(t *testing.T) {
	in := TacticsInput{
		Party:  []PCFact{fighterAt("Berold"), wizardAt("Meria")},
		Roster: []Combatant{goblinAt("Goblin", 4)},
	}
	a := Analyze(in)

	// Meria: 20 hp under 15 aimed — under one and a half rounds.
	var meria *PCDrop
	for i := range a.Attrition.Party {
		if a.Attrition.Party[i].PC == "Meria" {
			meria = &a.Attrition.Party[i]
		}
	}
	if meria == nil {
		t.Fatal("no Meria in the party attrition")
	}
	if meria.AimedAt.Value != 15 {
		t.Errorf("aimed at Meria = %v, want 15", meria.AimedAt.Value)
	}
	if meria.Rounds.Value != 1.3 {
		t.Errorf("rounds to drop Meria = %v, want 1.3", meria.Rounds.Value)
	}

	// The goblins: 28 hp across four of them; the party's baselines are
	// Berold 16 (fighter 5) + Meria 11 (fire bolt 2d10) = 27 — but the
	// goblin resists nothing, so all of it lands: 28/27 ≈ 1.0.
	var goblins *MonsterDrop
	for i := range a.Attrition.Monsters {
		if a.Attrition.Monsters[i].Monster == "Goblin" {
			goblins = &a.Attrition.Monsters[i]
		}
	}
	if goblins == nil {
		t.Fatal("no goblin attrition")
	}
	if goblins.Rounds == nil || goblins.Rounds.Value != 1.0 {
		t.Errorf("rounds to drop goblins = %+v, want 1.0", goblins.Rounds)
	}
}

// Nothing touches it: the read says hopeless, never divides by zero. A
// weapon-and-weapon party against a wisp immune to weapons and airborne has
// no answer at all.
func TestAttritionWhenThePartyHasNoAnswer(t *testing.T) {
	wisp := Creature{
		Name: "Will-o'-Wisp", CR: "2", CRNum: 2, XP: 450, AC: 17, HP: 22,
		Speeds: map[string]int{"fly": 50, "walk": 0},
		Immune: "lightning, poison; bludgeoning, piercing, and slashing from nonmagical attacks",
		Actions: []NamedText{
			{Name: "Shock", Desc: "Ranged Spell Attack: +4 to hit, range 60 ft., one target. Hit: 9 (2d8) lightning damage.", Kind: "ACTION"},
		},
	}
	party := []PCFact{
		fighterAt("Berold"),
		{Name: "Sable", Class: "rogue", Level: 5, AC: 14, MaxHP: 30},
	}
	a := Analyze(TacticsInput{Party: party, Roster: []Combatant{{Count: 2, Creature: wisp}}})
	drop := a.Attrition.Monsters[0]
	if drop.Rounds != nil {
		t.Fatalf("rounds = %+v, want none — the party has no answer", drop.Rounds)
	}
	if drop.Hopeless == "" || !strings.Contains(drop.Hopeless, "Berold") {
		t.Errorf("hopeless = %q, want the blanked pcs named", drop.Hopeless)
	}
}

/* ---------- counterplay ---------- */

func TestCounterplayNamesTheBlanksAndTheSave(t *testing.T) {
	wisp := Creature{
		Name: "Will-o'-Wisp", CR: "2", CRNum: 2, XP: 450, AC: 17, HP: 22,
		Speeds: map[string]int{"fly": 50},
		Immune: "lightning, poison; bludgeoning, piercing, and slashing from nonmagical attacks",
		Actions: []NamedText{
			{Name: "Shock", Desc: "Ranged Spell Attack: +4 to hit, range 60 ft., one target. Hit: 9 (2d8) lightning damage.", Kind: "ACTION"},
		},
	}
	shaman := Creature{
		Name: "Ember Shaman", CR: "1", CRNum: 1, XP: 200, AC: 13, HP: 22,
		Actions: []NamedText{
			{Name: "Hurl Flame", Desc: "Each creature in a 30-foot line must make a DC 13 Constitution saving throw, taking 9 (2d8) fire damage on a failed save, and the target is blinded until the end of its next turn on a failure.", Kind: "ACTION"},
		},
	}
	party := []PCFact{fighterAt("Berold"), wizardAt("Meria")}
	a := Analyze(TacticsInput{Party: party, Roster: []Combatant{
		{Count: 1, Creature: wisp}, {Count: 1, Creature: shaman},
	}})

	// The wisp blanks Berold's weapon baseline entirely.
	var blanked bool
	for _, c := range a.Counterplay {
		if c.PC == "Berold" && c.Monster == "Will-o'-Wisp" && strings.Contains(c.Detail, "immune to weapon") {
			blanked = true
		}
	}
	if !blanked {
		t.Errorf("no counterplay entry for the blanked fighter: %+v", a.Counterplay)
	}

	// Meria's Con save (+1) fails DC 13 at (13-1-1)/20 = 55% — the readout
	// names her and carries the rider.
	var save bool
	for _, c := range a.Counterplay {
		if c.PC == "Meria" && c.Monster == "Ember Shaman" && strings.Contains(c.Detail, "55%") && strings.Contains(c.Detail, "blinded") {
			save = true
		}
	}
	if !save {
		t.Errorf("no save-or-suck counterplay for Meria: %+v", a.Counterplay)
	}
}

/* ---------- spotlight: the roadmap's two questions ---------- */

// A roster built to bench one pc: everything immune to weapons and aiming at
// the wizard leaves the fighter contributing nothing on either axis.
func TestSpotlightFiresOnABenchedPC(t *testing.T) {
	wisp := Creature{
		Name: "Will-o'-Wisp", CR: "2", CRNum: 2, XP: 450, AC: 17, HP: 22,
		Speeds: map[string]int{"fly": 50},
		Immune: "lightning, poison; bludgeoning, piercing, and slashing from nonmagical attacks",
		Actions: []NamedText{
			{Name: "Shock", Desc: "Ranged Spell Attack: +4 to hit, range 60 ft., one target. Hit: 9 (2d8) lightning damage.", Kind: "ACTION"},
		},
	}
	party := []PCFact{fighterAt("Berold"), wizardAt("Meria")}
	a := Analyze(TacticsInput{Party: party, Roster: []Combatant{{Count: 2, Creature: wisp}}})

	var berold *PCSpotlight
	for i := range a.Spotlight {
		if a.Spotlight[i].PC == "Berold" {
			berold = &a.Spotlight[i]
		}
	}
	if berold == nil {
		t.Fatal("no spotlight row for Berold")
	}
	if !berold.Benched {
		t.Fatalf("Berold not flagged: %+v", berold)
	}
	if !strings.Contains(berold.Reason, "immune to weapon") {
		t.Errorf("bench reason = %q, want the specific immunity named", berold.Reason)
	}
	// Meria, the pc the whole design points at, must stay unflagged.
	for _, s := range a.Spotlight {
		if s.PC == "Meria" && s.Benched {
			t.Errorf("Meria flagged on the roster built around her: %+v", s)
		}
	}
}

// A balanced roster flags nobody.
func TestSpotlightStaysSilentOnABalancedRoster(t *testing.T) {
	in := TacticsInput{
		Party:  []PCFact{fighterAt("Berold"), wizardAt("Meria")},
		Roster: []Combatant{goblinAt("Goblin", 4)},
	}
	a := Analyze(in)
	for _, s := range a.Spotlight {
		if s.Benched {
			t.Errorf("%s flagged on a balanced roster: %+v", s.PC, s)
		}
	}
	if len(a.Spotlight) != 2 {
		t.Fatalf("spotlight rows = %d, want both pcs", len(a.Spotlight))
	}
}

/* ---------- the empty party block ---------- */

func TestEmptyPartyDegradesAndSaysSo(t *testing.T) {
	a := Analyze(TacticsInput{Roster: []Combatant{goblinAt("Goblin", 4)}})
	if a.PartyKnown {
		t.Fatal("PartyKnown with no party at all")
	}
	found := false
	for _, c := range a.Caveats {
		if strings.Contains(c, "no usable party block") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no degrade caveat: %v", a.Caveats)
	}
	for _, f := range a.Focus {
		if f.Target != "" {
			t.Errorf("a target was invented with no party: %+v", f)
		}
	}
	if a.Spotlight != nil {
		t.Errorf("spotlight computed with no party: %+v", a.Spotlight)
	}
	// A pc that declares nothing is not a party either.
	a = Analyze(TacticsInput{
		Party:  []PCFact{{Name: "Nameless"}, {Name: "Silent"}},
		Roster: []Combatant{goblinAt("Goblin", 4)},
	})
	if a.PartyKnown {
		t.Fatal("pcs declaring nothing were counted as a party block")
	}
	// No panic, and the monster side still reads.
	if len(a.Threat) != 1 {
		t.Fatalf("threat rows = %d, want the goblin", len(a.Threat))
	}
}

/* ---------- movement ---------- */

func TestMovementScoresTheDesign(t *testing.T) {
	// A reach objective's battlefield: slow ground and a chokepoint, against
	// a half-ranged roster. Movement is the fight.
	bear := Creature{
		Name: "Cave Bear", CR: "2", CRNum: 2, XP: 450, AC: 12, HP: 40,
		Speeds: map[string]int{"walk": 30},
		Actions: []NamedText{
			{Name: "Bite", Desc: "Melee Weapon Attack: +6 to hit, reach 5 ft., one target. Hit: 8 (1d8+4) piercing damage.", Kind: "ACTION"},
		},
	}
	in := TacticsInput{
		Party: []PCFact{fighterAt("Berold"), wizardAt("Meria")},
		Roster: []Combatant{
			{Count: 4, Creature: goblinAt("Goblin", 4).Creature}, // ranged: shortbow parsed
			{Count: 1, Creature: bear},                           // melee
		},
		Terrain: &Terrain{Features: []Feature{
			{Kind: "difficult_ground", Effect: "", Area: ""},
			{Kind: "chokepoint", Effect: "", Area: ""},
		}},
	}
	m := Analyze(in).Movement
	// difficult_ground 20 + chokepoint 20 + a half-ranged mix 40 = 80.
	if m.Score.Value != 80 {
		t.Errorf("movement score = %v, want 80", m.Score.Value)
	}
	if m.Label != "high" {
		t.Errorf("movement label = %q, want high", m.Label)
	}
	if len(m.Drivers) != 3 {
		t.Errorf("drivers = %v, want the three contributions listed", m.Drivers)
	}

	flat := Analyze(TacticsInput{
		Party: []PCFact{fighterAt("Berold"), wizardAt("Meria")},
		Roster: []Combatant{{Count: 4, Creature: Creature{
			Name: "Ogre", AC: 11, HP: 59, Speeds: map[string]int{"walk": 40},
			Actions: []NamedText{
				{Name: "Greatclub", Desc: "Melee Weapon Attack: +6 to hit, reach 10 ft., one target. Hit: 13 (2d8+4) bludgeoning damage.", Kind: "ACTION"},
			},
		}}},
	}).Movement
	if flat.Score.Value == 0 {
		t.Errorf("a 40-ft. mover with 10 ft. reach should not score zero: %+v", flat)
	}
}

/* ---------- the prose gate ---------- */

func TestProseGateRejectsInventedFigures(t *testing.T) {
	in := TacticsInput{
		Party:  []PCFact{fighterAt("Berold"), wizardAt("Meria")},
		Roster: []Combatant{goblinAt("Goblin", 4)},
	}
	a := Analyze(in)

	// The fake client's tactics: confident, specific, and one figure made up.
	invented := SectionOf(`
## Tactics
The goblins open with their bows from the treeline, then close with scimitars.
Round one, all four aim at Meria — their expected output is 21 damage against her,
and the DMG says focus fire wins fights at 5th level.
`, "Tactics")
	violations := CheckTacticsProse(in, a, invented)
	if len(violations) != 1 || violations[0].Token != "21" {
		t.Fatalf("violations = %+v, want exactly the invented 21", violations)
	}
	if !strings.Contains(violations[0].Detail, "traces to nothing") {
		t.Errorf("violation detail = %q", violations[0].Detail)
	}

	// A figure the analysis actually derived is allowed through.
	traced := "Round one, all four aim at Meria — about 15 expected damage finds her."
	if v := CheckTacticsProse(in, a, traced); len(v) != 0 {
		t.Fatalf("a derived figure was rejected: %+v", v)
	}

	// Words carry no numbers; dice grammar is grammar, not a claim.
	prose := "The pack rushes the wizard while their archers hold the treeline, loosing volleys of 2d8 worth of arrows only when she casts."
	if v := CheckTacticsProse(in, a, prose); len(v) != 0 {
		t.Fatalf("numberless prose rejected: %+v", v)
	}

	// No prose, no violations.
	if v := CheckTacticsProse(in, a, ""); len(v) != 0 {
		t.Fatalf("empty prose rejected: %+v", v)
	}
}

func TestSectionOf(t *testing.T) {
	md := "# The Bridge\n\n## The pitch\nCross it.\n\n## Tactics\nHold the line.\n\n## Twist\nThe bridge burns.\n"
	if got := SectionOf(md, "tactics"); got != "Hold the line." {
		t.Errorf("SectionOf = %q", got)
	}
	if got := SectionOf(md, "missing"); got != "" {
		t.Errorf("SectionOf on a missing section = %q", got)
	}
}

/* ---------- waves and caveats ---------- */

func TestWaveNoteAndBaselineAdjustments(t *testing.T) {
	in := TacticsInput{
		Party:  []PCFact{fighterAt("Berold"), wizardAt("Meria")},
		Roster: []Combatant{goblinAt("Goblin", 4)},
		Waves:  3,
	}
	a := Analyze(in)
	found := false
	for _, c := range a.Caveats {
		if strings.Contains(c, "3 waves") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no wave caveat: %v", a.Caveats)
	}

	// Resistances halve a pc's answer, and the derivation says so.
	armored := Creature{
		Name: "Fire Guard", CR: "2", CRNum: 2, XP: 450, AC: 14, HP: 30,
		Resist: "fire",
		Actions: []NamedText{
			{Name: "Pike", Desc: "Melee Weapon Attack: +4 to hit, reach 10 ft., one target. Hit: 7 (1d10+2) piercing damage.", Kind: "ACTION"},
		},
	}
	a2 := Analyze(TacticsInput{Party: []PCFact{wizardAt("Meria"), {Name: "Sable", Class: "bard", Level: 5, AC: 14, MaxHP: 30}}, Roster: []Combatant{{Count: 1, Creature: armored}}})
	drop := a2.Attrition.Monsters[0]
	// Meria 11 halves to 6 (round of 5.5); Sable's mockery is 5. 6+5 = 11.
	if drop.Answer.Value != 11 {
		t.Errorf("answer vs fire resistance = %v, want 11", drop.Answer.Value)
	}
	if drop.Rounds == nil || drop.Rounds.Value != 2.7 {
		t.Errorf("rounds = %+v, want 30/11 = 2.7", drop.Rounds)
	}
}

/* ---------- the baseline table ---------- */

func TestBaselineFor(t *testing.T) {
	cases := []struct {
		class string
		level int
		want  int
		dmg   string
	}{
		{"fighter", 1, 7, "weapon"},
		{"fighter", 5, 16, "weapon"},  // 2 × (1d8+4)
		{"fighter", 11, 27, "weapon"}, // 3 × (1d8+5)
		{"wizard", 1, 5, "fire"},      // 1d10
		{"wizard", 5, 11, "fire"},     // 2d10
		{"cleric", 5, 9, "radiant"},   // 2d8
		{"rogue", 5, 17, "weapon"},    // 1d6+4 + 3d6 sneak
		{"bard", 5, 5, "psychic"},     // 2d4
		{"mystic", 3, 3, "weapon"},    // the disclosed floor
	}
	for _, tc := range cases {
		dpr, dmg, how, ok := baselineFor(tc.class, tc.level)
		if !ok {
			t.Errorf("%s %d: not priceable", tc.class, tc.level)
			continue
		}
		if dpr != tc.want || dmg != tc.dmg {
			t.Errorf("%s %d = %d (%s), want %d (%s) — how: %s", tc.class, tc.level, dpr, dmg, tc.want, tc.dmg, how)
		}
		if strings.TrimSpace(how) == "" {
			t.Errorf("%s %d: empty derivation", tc.class, tc.level)
		}
	}
	if _, _, _, ok := baselineFor("wizard", 0); ok {
		t.Error("a pc with no level must not price")
	}
	// Free-typed classes normalise: "Fighter (Battle Master)" is a fighter.
	if dpr, _, _, _ := baselineFor("Fighter (Battle Master)", 5); dpr != 16 {
		t.Errorf("free-typed class = %d, want 16", dpr)
	}
}

/* ---------- resistances on the incoming side ---------- */

func TestPCResistanceBluntsIncoming(t *testing.T) {
	brute := Creature{
		Name: "Ember Shaman", CR: "1", CRNum: 1, XP: 200, AC: 13, HP: 22,
		Actions: []NamedText{
			{Name: "Hurl Flame", Desc: "Each creature in a 30-foot line must make a DC 13 Dexterity saving throw, taking 9 (2d8) fire damage on a failed save.", Kind: "ACTION"},
		},
	}

	// Alone against the shamans, Meria's resistance halves the aimed damage
	// — 2×9 at a 45% failure rate, halved to about 4 — and the derivation
	// says so.
	meria := wizardAt("Meria")
	meria.Resists = []string{"fire"}
	a := Analyze(TacticsInput{Party: []PCFact{meria}, Roster: []Combatant{{Count: 2, Creature: brute}}})
	f := a.Focus[0]
	if f.Target != "Meria" {
		t.Fatalf("focus target = %q, want Meria", f.Target)
	}
	if got := f.PerRound.Value; got != 4.1 {
		t.Errorf("aimed damage = %v, want 4.1 (halved)", got)
	}
	if !strings.Contains(f.PerRound.How, "halved") {
		t.Errorf("per-round derivation = %q, want the halving stated", f.PerRound.How)
	}

	// In the full party the resistances move the aim: unresisted, the
	// shamans want Meria (0.405 vulnerability per hp vs Berold's 0.225);
	// resisted, her score drops under his and the aim follows the armour.
	plain := Analyze(TacticsInput{
		Party:  []PCFact{fighterAt("Berold"), wizardAt("Meria")},
		Roster: []Combatant{{Count: 2, Creature: brute}},
	}).Focus[0].Target
	resisted := Analyze(TacticsInput{
		Party:  []PCFact{fighterAt("Berold"), meria},
		Roster: []Combatant{{Count: 2, Creature: brute}},
	}).Focus[0].Target
	if plain != "Meria" || resisted != "Berold" {
		t.Fatalf("aim did not move with the resistance: %q then %q", plain, resisted)
	}
}
