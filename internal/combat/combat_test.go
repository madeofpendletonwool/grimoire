package combat

// The pure state machine's tests (MAD-422): the hit-point grammar,
// death saves, the order, the count-20 crossing, and the snapshot
// derivations — no database, no clock, no network.

import (
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/encounter"
	"github.com/madeofpendletonwool/grimoire/internal/sheet"
	"github.com/madeofpendletonwool/grimoire/internal/statblock"
)

func pc(name string, maxHP, ac int) Combatant {
	return Combatant{Name: name, Kind: KindPC, Side: SideParty, MaxHP: maxHP, HP: maxHP, AC: ac}
}

func monster(name string, maxHP int) Combatant {
	return Combatant{Name: name, Kind: KindMonster, Side: SideFoe, MaxHP: maxHP, HP: maxHP}
}

// damage runs one application, failing the test on the grammar's own
// refusal — the cases here are all legal hits.
func damage(t *testing.T, c Combatant, amount int, dtype string, reduceMax bool) (Combatant, DamageOutcome) {
	t.Helper()
	c, out, err := ApplyDamage(c, amount, dtype, reduceMax)
	if err != nil {
		t.Fatalf("damage %d %q: %v", amount, dtype, err)
	}
	return c, out
}

func save(t *testing.T, c Combatant, result string) (Combatant, SaveOutcome) {
	t.Helper()
	c, out, err := RecordDeathSave(c, result)
	if err != nil {
		t.Fatalf("death save %s: %v", result, err)
	}
	return c, out
}

func temp(t *testing.T, c Combatant, n int) Combatant {
	t.Helper()
	c, err := GrantTempHP(c, n)
	if err != nil {
		t.Fatalf("grant temp %d: %v", n, err)
	}
	return c
}

/* ---------- the hit-point grammar ---------- */

func TestTempHPDrinksFirst(t *testing.T) {
	c := temp(t, pc("Velren", 30, 15), 8)
	c, out := damage(t, c, 10, "", false)
	if out.Absorbed != 8 || out.Effective != 10 {
		t.Fatalf("absorption: %+v", out)
	}
	if c.TempHP != 0 || c.HP != 28 || out.Overflow != 0 || out.Died || out.WentDown {
		t.Fatalf("velren after: hp %d temp %d out %+v", c.HP, c.TempHP, out)
	}
	// Temp hp never stacks; the larger coat wins.
	c = temp(t, c, 5)
	if c.TempHP != 5 {
		t.Fatalf("smaller coat replaced: %d", c.TempHP)
	}
}

func TestResistanceHalvesDownAndImmunityZeroes(t *testing.T) {
	c := pc("Evendur", 40, 18)
	c.Snapshot.Resist = []string{"fire"}
	c.Snapshot.Immune = []string{"poison"}

	if _, out := damage(t, c, 11, "fire", false); out.Effective != 5 {
		t.Fatalf("resistance: 11 fire -> %d, want 5", out.Effective)
	}
	if _, out := damage(t, c, 30, "poison", false); out.Effective != 0 || out.After != 40 {
		t.Fatalf("immunity: %+v", out)
	}
	// A qualified resistance matches its leading type; other types pass.
	c.Snapshot.Resist = []string{"bludgeoning from nonmagical attacks"}
	if _, out := damage(t, c, 10, "bludgeoning", false); out.Effective != 5 {
		t.Fatalf("qualified resistance: %+v", out)
	}
	if _, out := damage(t, c, 10, "slashing", false); out.Effective != 10 {
		t.Fatalf("other type: %+v", out)
	}
}

func TestVulnerabilityDoubles(t *testing.T) {
	c := pc("Nyx", 26, 14)
	c.Snapshot.Vulnerable = []string{"fire"}
	if _, out := damage(t, c, 10, "fire", false); out.Effective != 20 || out.After != 6 {
		t.Fatalf("vulnerability: %+v", out)
	}
}

func TestMonsterDiesAtZero(t *testing.T) {
	g, out := damage(t, monster("Goblin", 7), 9, "slashing", false)
	if !out.Died || !g.Dead || g.HP != 0 {
		t.Fatalf("goblin: %+v %+v", g, out)
	}
	// The dead take no further damage.
	if _, _, err := ApplyDamage(g, 3, "", false); err == nil {
		t.Fatalf("the dead were hit again")
	}
}

func TestPCGoesDownAndDamageAtZeroFailsSaves(t *testing.T) {
	v := pc("Velren", 30, 15)

	// The hit that drops does not fail a save.
	v, out := damage(t, v, 30, "slashing", false)
	if !out.WentDown || !v.Downed || v.Dead || v.DeathFailures != 0 {
		t.Fatalf("dropping hit: %+v", v)
	}

	// Damage while at 0 is a failed save, one per hit; the third kills.
	v, out = damage(t, v, 5, "slashing", false)
	if !out.DeathFail || v.DeathFailures != 1 || v.Dead {
		t.Fatalf("damage at zero: %+v", v)
	}
	v, _ = damage(t, v, 5, "slashing", false)
	if v.DeathFailures != 2 || v.Dead {
		t.Fatalf("second hit at zero: %+v", v)
	}
	v, out = damage(t, v, 5, "slashing", false)
	if !out.Died || !v.Dead {
		t.Fatalf("third failure should kill: %+v", v)
	}
}

func TestMassiveDamageKillsOutright(t *testing.T) {
	v, out := damage(t, pc("Velren", 30, 15), 61, "radiant", false) // 30 past zero = max
	if !out.Died || !v.Dead || out.Overflow != 31 {
		t.Fatalf("massive damage: %+v", out)
	}

	// Carried to the max is death outright; one short of it is down.
	w, out := damage(t, pc("Nyx", 30, 15), 59, "radiant", false)
	if out.Died || !w.Downed || out.Overflow != 29 {
		t.Fatalf("not massive: %+v %+v", out, w)
	}
}

func TestMaxHPReductionClampsAndKillsAtZeroMax(t *testing.T) {
	v := pc("Velren", 30, 15)
	v, out := damage(t, v, 10, "necrotic", true)
	if out.ReducedMax != 10 || v.EffectiveMax() != 20 {
		t.Fatalf("reduction: %+v %+v", out, v)
	}
	// The bite again: hp lands on the falling ceiling, still standing
	// at 2 of 2.
	v, out = damage(t, v, 18, "necrotic", true)
	if v.HP != 2 || v.EffectiveMax() != 2 || out.ReducedMax != 18 || v.Downed {
		t.Fatalf("clamp: %+v %+v", out, v)
	}
	// The last of the ceiling is death outright — the max itself fell
	// to zero.
	w, _ := damage(t, pc("Nyx", 12, 14), 12, "necrotic", true)
	if !w.Dead {
		t.Fatalf("max zero should kill: %+v", w)
	}
}

func TestHealWakesAndClearsTheLedger(t *testing.T) {
	v, _ := damage(t, pc("Velren", 30, 15), 30, "", false)
	v, _ = save(t, v, SaveFail)
	v, _ = save(t, v, SaveSuccess)

	v, err := ApplyHeal(v, 12)
	if err != nil {
		t.Fatalf("heal: %v", err)
	}
	if v.HP != 12 || v.Downed || v.Stable || v.DeathSuccesses != 0 || v.DeathFailures != 0 {
		t.Fatalf("woke wrong: %+v", v)
	}

	v, err = ApplyHeal(v, 99)
	if err != nil || v.HP != 30 {
		t.Fatalf("cap: %d %v", v.HP, err)
	}

	// The dead are past healing.
	d, _ := damage(t, monster("Goblin", 7), 9, "", false)
	if _, err := ApplyHeal(d, 5); err == nil {
		t.Fatalf("the dead were healed")
	}
}

func TestDeathSaveThresholds(t *testing.T) {
	v, _ := damage(t, pc("Velren", 30, 15), 30, "", false)

	// A natural 1 is two failures.
	if _, out := save(t, v, SaveCritFail); out.Failures != 2 {
		t.Fatalf("nat 1: %+v", out)
	}
	// A save on a pc who is not down is a refusal.
	if _, _, err := RecordDeathSave(pc("Thalia", 30, 15), SaveSuccess); err == nil {
		t.Fatalf("a standing pc saved")
	}

	// Three successes stabilize, still down, and a stable pc saves no
	// further.
	s, _ := damage(t, pc("Nyx", 30, 15), 30, "", false)
	s, _ = save(t, s, SaveSuccess)
	s, _ = save(t, s, SaveSuccess)
	s, out := save(t, s, SaveSuccess)
	if !out.BecameStable || !s.Stable || s.Dead || !s.Downed {
		t.Fatalf("stable: %+v", s)
	}
	if _, _, err := RecordDeathSave(s, SaveSuccess); err == nil {
		t.Fatalf("a stable pc kept saving")
	}

	// A natural 20 wakes at 1 hp with the ledger cleared.
	w, out := save(t, mustDown(t, "Mira"), SaveCritSuccess)
	if !out.Woke || w.HP != 1 || w.Downed || w.DeathSuccesses != 0 {
		t.Fatalf("nat 20: %+v %+v", out, w)
	}

	// Three failures die.
	u := mustDown(t, "Thalia")
	u, _ = save(t, u, SaveFail)
	u, _ = save(t, u, SaveFail)
	u, out = save(t, u, SaveFail)
	if !out.Died || !u.Dead {
		t.Fatalf("dead: %+v", u)
	}
}

func mustDown(t *testing.T, name string) Combatant {
	t.Helper()
	c, _ := damage(t, pc(name, 30, 15), 30, "", false)
	return c
}

func TestLegendaryBudget(t *testing.T) {
	c := monster("Adult Black Dragon", 195)
	c.Snapshot.LegendaryMax = 3

	var err error
	if c, err = c.SpendLegendary(2); err != nil || c.LegendaryUsed != 2 {
		t.Fatalf("spend 2: %v %+v", err, c)
	}
	if _, err = c.SpendLegendary(2); err == nil {
		t.Fatalf("over budget allowed")
	}
	// Turn end replenishes; turn start returns the reaction.
	c = c.EndTurn()
	if c.LegendaryUsed != 0 {
		t.Fatalf("replenish: %+v", c)
	}
	c.ReactionSpent = true
	c = c.StartTurn()
	if c.ReactionSpent {
		t.Fatalf("reaction: %+v", c)
	}
	// A combatant with no legendary actions cannot spend any.
	if _, err := monster("Goblin", 7).SpendLegendary(1); err == nil {
		t.Fatalf("a goblin took a legendary action")
	}
}

/* ---------- conditions ---------- */

func TestLocalConditionsTickAndSupersede(t *testing.T) {
	g := monster("Goblin", 7)
	g, cond, err := ApplyLocalCondition(g, "poisoned", 2, "spider")
	if err != nil || cond.Rounds != 2 || len(g.Conditions) != 1 {
		t.Fatalf("apply: %v %+v", err, g.Conditions)
	}

	// Re-apply supersedes: one row, the new duration, the same identity.
	g, again, _ := ApplyLocalCondition(g, "Poisoned", 4, "spider")
	if len(g.Conditions) != 1 || again.ID != cond.ID || again.Rounds != 4 {
		t.Fatalf("supersede: %+v %+v", g.Conditions, again)
	}

	g, expired := TickConditions(g)
	if len(expired) != 0 || g.Conditions[0].Rounds != 3 {
		t.Fatalf("tick 1: %+v %v", g.Conditions, expired)
	}
	for i := 0; i < 3; i++ {
		g, expired = TickConditions(g)
	}
	// The store canonicalizes the name; the pure list keeps the
	// spelling it was handed.
	if len(g.Conditions) != 0 || len(expired) != 1 || expired[0].Name != "Poisoned" {
		t.Fatalf("expired: %+v %v", g.Conditions, expired)
	}
}

func TestPCConditionsRefusedLocally(t *testing.T) {
	if _, _, err := ApplyLocalCondition(pc("Velren", 30, 15), "poisoned", 3, ""); err == nil {
		t.Fatalf("pc condition went local instead of to the engine")
	}
}

/* ---------- the order and the count-20 crossing ---------- */

func TestOrderSortsAndTiesBreakByName(t *testing.T) {
	cs := []Combatant{
		{Name: "Goblin A", Initiative: 9},
		{Name: "Velren", Initiative: 22},
		{Name: "Goblin B", Initiative: 15},
		{Name: "Nyx", Initiative: 22},
	}
	order := Order(cs)
	want := []string{"Nyx", "Velren", "Goblin B", "Goblin A"}
	for i, name := range want {
		if order[i].Name != name {
			t.Fatalf("order[%d] = %s, want %s", i, order[i].Name, name)
		}
	}
}

func TestNextAliveSkipsTheDead(t *testing.T) {
	order := []Combatant{
		{Name: "Velren", Initiative: 22},
		{Name: "Goblin A", Initiative: 15, Dead: true},
		{Name: "Goblin B", Initiative: 9},
		{Name: "Nyx", Initiative: 5},
	}
	// The battle's opening: the order's own top, no wrap.
	if next, wrapped := NextAlive(order, -1); next != 0 || wrapped {
		t.Fatalf("opening: next %d wrapped %v", next, wrapped)
	}
	if next, wrapped := NextAlive(order, 0); next != 2 || wrapped {
		t.Fatalf("skip dead: next %d wrapped %v", next, wrapped)
	}
	if next, wrapped := NextAlive(order, 2); next != 3 || wrapped {
		t.Fatalf("to nyx: next %d wrapped %v", next, wrapped)
	}
	if next, wrapped := NextAlive(order, 3); next != 0 || !wrapped {
		t.Fatalf("wrap: next %d wrapped %v", next, wrapped)
	}
	// Everyone else dead: the same combatant steps again.
	solo := []Combatant{{Name: "Velren", Initiative: 22}, {Name: "Goblin", Dead: true}}
	if next, wrapped := NextAlive(solo, 0); next != 0 || !wrapped {
		t.Fatalf("solo: next %d wrapped %v", next, wrapped)
	}
}

func TestLairFiresAtCountTwentyLosingTies(t *testing.T) {
	cases := []struct {
		prev, next int
		wrapped    bool
		want       bool
	}{
		{25, 20, false, true},  // above 20 done, the 20 acts after the lair
		{21, 20, false, true},  // the tie itself loses to the lair
		{25, 21, false, false}, // both above: not yet
		{20, 19, false, false}, // the 20 already acted after the lair
		{19, 5, false, false},  // both under: already gone
		{5, 25, true, false},   // wrap to an above-20 act: the crossing comes mid-order
		{5, 18, true, true},    // wrap to an at-or-under act: the lair leads the round
	}
	for _, tc := range cases {
		if got := LairFires(tc.prev, tc.next, tc.wrapped); got != tc.want {
			t.Fatalf("LairFires(%d, %d, %v) = %v, want %v", tc.prev, tc.next, tc.wrapped, got, tc.want)
		}
	}
}

/* ---------- snapshots ---------- */

func TestSnapshotOfSheet(t *testing.T) {
	s := sheet.Sheet{
		Classes:     []sheet.ClassLevel{{Class: "Ranger", Level: 3}},
		Abilities:   sheet.Abilities{STR: 12, DEX: 17, CON: 14},
		AC:          15,
		MaxHP:       28,
		Resistances: []string{"Fire"},
	}
	snap := SnapshotOfSheet(s)
	if snap.InitBonus != 3 { // (17-10)/2
		t.Fatalf("init bonus: %d", snap.InitBonus)
	}
	if len(snap.Resist) != 1 || snap.Resist[0] != "fire" {
		t.Fatalf("resistances lowered: %v", snap.Resist)
	}
	if snap.Label == "" {
		t.Fatalf("label: %q", snap.Label)
	}
}

func TestSnapshotOfCreatureDerivesLegendaryAndRecharge(t *testing.T) {
	c := encounter.Creature{
		Slug: "wolf", Name: "Wolf", CR: "1/4", XP: 50, AC: 13, HP: 11,
		Abilities: &statblock.Abilities{Str: 12, Dex: 15, Con: 12},
		Actions: []encounter.NamedText{
			{Name: "Bite", Kind: "ACTION"},
			{Name: "Howl", Kind: "LEGENDARY_ACTION", Cost: 1},
			{Name: "Pounce", Kind: "LEGENDARY_ACTION", Cost: 2},
			{Name: "Breath Weapon", Kind: "ACTION", Usage: "Recharge 5-6"},
		},
	}
	snap := SnapshotOfCreature(c)
	if snap.InitBonus != 2 {
		t.Fatalf("dex mod: %d", snap.InitBonus)
	}
	if snap.LegendaryMax != 2 {
		t.Fatalf("legendary count: %d", snap.LegendaryMax)
	}
	if len(snap.Recharge) != 1 || snap.Recharge[0].Name != "Breath Weapon" {
		t.Fatalf("recharge: %+v", snap.Recharge)
	}

	dragon := encounter.Creature{Name: "Adult Black Dragon", LairAction: true,
		Resist: "nonmagical bludgeoning, piercing, and slashing"}
	snapD := SnapshotOfCreature(dragon)
	if !snapD.Lair {
		t.Fatalf("lair flag")
	}
	if len(snapD.Resist) != 3 || snapD.Resist[2] != "slashing" {
		t.Fatalf("resist split: %v", snapD.Resist)
	}
}

func TestDamageSummaryReadsLikeTheTable(t *testing.T) {
	v := pc("Velren", 30, 15)
	_, out := damage(t, v, 12, "slashing", false)
	line := out.Summary(v)
	for _, want := range []string{"Velren takes 12 slashing", "18/30 hp"} {
		if !strings.Contains(line, want) {
			t.Fatalf("summary %q missing %q", line, want)
		}
	}
}
