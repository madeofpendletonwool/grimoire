package loot

// The loot arithmetic's own tests (MAD-384): the hoard tables tile and
// band exactly as the book's arithmetic says; every generated item carries
// its reason; a reroll moves one line and nothing else; the distribution
// folds in event-log order; the power curve shows its work; and the
// concentration warning fires on a hand-built over-concentrated party and
// stays silent on an even one — both pinned here as fixtures.

import (
	"encoding/json"
	"math/rand"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/items"
)

// testCatalog is a synthetic shelf with known tags and rarities — the
// loot package never opens the mirror or the network.
func testCatalog() []items.Item {
	mk := func(slug, name, rarity, typ string, tags ...string) items.Item {
		return items.Item{Slug: slug, Name: name, Rarity: rarity, Type: typ, Tags: tags}
	}
	return []items.Item{
		mk("sword-1", "Sword of the Sentinel", "Uncommon", "Weapon", "weapon", "offensive"),
		mk("sword-2", "Flamebrand", "Rare", "Weapon", "weapon", "offensive", "damage-rider"),
		mk("sword-3", "Doomblade", "Legendary", "Weapon", "weapon", "offensive", "damage-rider"),
		mk("armor-1", "Hauberk of Warding", "Uncommon", "Armor", "armor", "defensive"),
		mk("armor-2", "Plate of the Bulwark", "Rare", "Armor", "armor", "defensive"),
		mk("cloak-1", "Cloak of Shades", "Uncommon", "Wondrous Item", "defensive", "save-boost"),
		mk("boots-1", "Boots of the Strider", "Uncommon", "Wondrous Item", "movement", "utility"),
		mk("orb-1", "Orb of Depths", "Very Rare", "Wondrous Item", "utility", "save-boost"),
		mk("potion-1", "Potion of Mending", "Common", "Potion", "consumable"),
		mk("scroll-1", "Scroll of the Deep", "Common", "Scroll", "consumable", "utility"),
	}
}

/* ---------- the tables ---------- */

// The d100 rows tile 1..100 with no gap and no overlap, every named
// magic table has a rarity profile, and every mundane bundle is sane —
// the band arithmetic downstream is meaningless otherwise.
func TestHoardTablesTileTheD100(t *testing.T) {
	for tier := Tier1; tier <= Tier4; tier++ {
		rows := hoardRows[tier]
		want := 1
		for _, row := range rows {
			if row.lo != want {
				t.Errorf("tier %d row %d-%d breaks the tiling at %d", tier, row.lo, row.hi, want)
			}
			if row.hi < row.lo || row.hi > 100 {
				t.Errorf("tier %d row %d-%d is out of range", tier, row.lo, row.hi)
			}
			for _, m := range row.magic {
				if len(profileFor(m.Table)) == 0 {
					t.Errorf("tier %d rolls table %s, which has no rarity profile", tier, m.Table)
				}
			}
			if row.mundane.Kind != "" && row.mundane.UnitValue <= 0 {
				t.Errorf("tier %d row %d carries a %s bundle of no value", tier, row.lo, row.mundane.Kind)
			}
			want = row.hi + 1
		}
		if want != 101 {
			t.Errorf("tier %d rows end at %d, not 100", tier, want-1)
		}
	}
}

// The coin lines are the book's, pinned by their average gold value. The
// averages use the implementation's integer dice expectation (a d6
// averages 3, a d4 2, after flooring), so tier 1 lands at 191 gp where
// the exact-fraction figure the DMG summaries quote is 196 — the tables,
// not the summary, are the arithmetic floor here.
func TestCoinAveragesAreTheBooks(t *testing.T) {
	avgGP := func(tier Tier) int {
		total := 0
		for _, c := range hoardCoins[tier] {
			total += c.valueCP(c.dice.Avg())
		}
		return total / 100
	}
	got := map[Tier]int{Tier1: avgGP(Tier1), Tier2: avgGP(Tier2), Tier3: avgGP(Tier3), Tier4: avgGP(Tier4)}
	want := map[Tier]int{Tier1: 191, Tier2: 3807, Tier3: 6400, Tier4: 322000}
	for tier := range want {
		if got[tier] != want[tier] {
			t.Errorf("tier %d average coin value = %d gp, want %d gp", tier, got[tier], want[tier])
		}
	}
}

/* ---------- the band ---------- */

// Hoard value falls inside the DMG's tier band, asserted per tier: two
// thousand seeded hoards per tier, every value inside the band computed
// from the tables, and the observed mean agreeing with the band's
// expectation.
func TestHoardValueFallsInsideTheTierBand(t *testing.T) {
	const rolls = 2000
	for tier := Tier1; tier <= Tier4; tier++ {
		band := TierBand(tier)
		if band.MinGP <= 0 || band.MaxGP <= band.MinGP || band.AvgGP <= band.MinGP || band.AvgGP >= band.MaxGP {
			t.Fatalf("tier %d band %v is not an envelope", tier, band)
		}
		sum := 0
		rng := rand.New(rand.NewSource(int64(tier)))
		for i := 0; i < rolls; i++ {
			h, err := GenerateHoard(Request{Tier: tier}, testCatalog(), rng.Int63())
			if err != nil {
				t.Fatalf("tier %d hoard: %v", tier, err)
			}
			if h.ValueGP < band.MinGP || h.ValueGP > band.MaxGP {
				t.Fatalf("tier %d hoard value %d gp outside its band [%d, %d]",
					tier, h.ValueGP, band.MinGP, band.MaxGP)
			}
			sum += h.ValueGP
		}
		mean := sum / rolls
		if mean < band.AvgGP/2 || mean > band.AvgGP*3/2 {
			t.Errorf("tier %d observed mean %d gp drifts from the band's expectation %d gp",
				tier, mean, band.AvgGP)
		}
	}
}

// The tier 1 band's edges are the tables' own edges, pinned by hand: the
// barest row rolls 6d6×100 cp, 3d6×100 sp and 2d6×10 gp with no mundane
// treasure (56 gp); the richest rolls the 50 gp gems' dozen (936 gp).
func TestTierBandEdgesAreTheTables(t *testing.T) {
	band := TierBand(Tier1)
	if band.MinGP != 56 {
		t.Errorf("tier 1 band minimum = %d gp, want 56", band.MinGP)
	}
	if band.MaxGP != 936 {
		t.Errorf("tier 1 band maximum = %d gp, want 936", band.MaxGP)
	}
}

/* ---------- generation ---------- */

// Every generated item carries a recorded reason for its selection —
// across every tier and a stack of seeds, no exception.
func TestEveryGeneratedItemCarriesAReason(t *testing.T) {
	rng := rand.New(rand.NewSource(99))
	for tier := Tier1; tier <= Tier4; tier++ {
		for i := 0; i < 200; i++ {
			h, err := GenerateHoard(Request{Tier: tier}, testCatalog(), rng.Int63())
			if err != nil {
				t.Fatalf("tier %d hoard: %v", tier, err)
			}
			for _, line := range h.Items {
				if strings.TrimSpace(line.Reason) == "" {
					t.Fatalf("tier %d line %s (%s) carries no reason", tier, line.Key, line.Name)
				}
			}
		}
	}
}

// The same request and seed give the same hoard, byte for byte.
func TestGenerationIsDeterministic(t *testing.T) {
	req := Request{Tier: Tier2}
	a, err := GenerateHoard(req, testCatalog(), 4242)
	if err != nil {
		t.Fatal(err)
	}
	b, err := GenerateHoard(req, testCatalog(), 4242)
	if err != nil {
		t.Fatal(err)
	}
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	if string(ab) != string(bb) {
		t.Fatalf("identical seeds gave different hoards:\n%s\n%s", ab, bb)
	}
}

// The party read steers the choice without moving the rarity: an
// over-loaded offense still draws an item from the table's rarity
// profile, and the reason names the pc it sits with.
func TestPartyReadSteersTheChoice(t *testing.T) {
	party := []PC{
		{Name: "Fighter", Level: 6, Items: []Holding{
			{ItemName: "Sword of the Sentinel", Rarity: "Uncommon", Tags: []string{"weapon", "offensive"}},
			{ItemName: "Flamebrand", Rarity: "Rare", Tags: []string{"weapon", "offensive", "damage-rider"}},
			{ItemName: "Doomblade", Rarity: "Legendary", Tags: []string{"weapon", "offensive", "damage-rider"}},
		}},
		{Name: "Wizard", Level: 6},
	}
	// A weapon-heavy pool: every candidate is offensive, so the spread
	// read must name the pc holding the least — the Wizard.
	pool := []items.Item{testCatalog()[2], testCatalog()[1], testCatalog()[0]}
	rng := rand.New(rand.NewSource(7))
	sawWizard := false
	for i := 0; i < 50; i++ {
		_, why, ok := pickFromTable(pool, "A", rng, party)
		if !ok {
			t.Fatal("pickFromTable failed on a non-empty pool")
		}
		if strings.Contains(why, "Wizard") {
			sawWizard = true
		}
	}
	if !sawWizard {
		t.Errorf("the party read never sat an item with the pc who holds nothing")
	}
	// Rarity is never a party decision: table A is Common-to-Uncommon, so
	// a Rare item must never be the answer even with a starving party.
	rarePool := []items.Item{testCatalog()[1]}
	_, why, ok := pickFromTable(rarePool, "A", rng, party)
	if ok {
		t.Errorf("table A picked %q outside its rarity profile: %s", "Flamebrand", why)
	}
}

// A reroll moves exactly the named line. Coins, the d100 row and every
// other line come back identical, whatever the nonce.
func TestRerollMovesOneLineOnly(t *testing.T) {
	req := Request{Tier: Tier3}
	base, err := GenerateHoard(req, testCatalog(), 777)
	if err != nil {
		t.Fatal(err)
	}
	// Find an item line to reroll; a hoard with none has nothing to test.
	if len(base.Items) == 0 {
		t.Skip("the seed rolled no magic items")
	}
	key := base.Items[0].Key
	changed := false
	for nonce := int64(1); nonce <= 8; nonce++ {
		h, err := RegenerateHoard(req, testCatalog(), 777, map[string]int64{key: nonce})
		if err != nil {
			t.Fatal(err)
		}
		if h.Row != base.Row {
			t.Errorf("reroll moved the d100 row: %s != %s", h.Row, base.Row)
		}
		if len(h.Coins) != len(base.Coins) {
			t.Fatalf("reroll changed the coin count")
		}
		for i := range h.Coins {
			if h.Coins[i] != base.Coins[i] {
				t.Errorf("reroll moved coin line %d", i)
			}
		}
		if len(h.Items) != len(base.Items) {
			t.Fatalf("reroll changed the item count")
		}
		for i := range h.Items {
			same := h.Items[i].Name == base.Items[i].Name && h.Items[i].Slug == base.Items[i].Slug
			if h.Items[i].Key != base.Items[i].Key {
				t.Errorf("reroll renumbered line %d", i)
			}
			if h.Items[i].Key == key {
				if !same {
					changed = true
				}
				continue
			}
			if !same {
				t.Errorf("reroll moved %s (%s -> %s)", key, base.Items[i].Name, h.Items[i].Name)
			}
		}
	}
	if !changed {
		t.Errorf("eight rerolls never changed the line — the nonce is not reaching the dice")
	}
}

/* ---------- degraded mode ---------- */

// A campaign with an empty party block still generates a hoard —
// degraded to tier-only, and saying so.
func TestEmptyPartyBlockDegradesToTierOnly(t *testing.T) {
	h, err := GenerateHoard(Request{Tier: Tier2}, testCatalog(), 31)
	if err != nil {
		t.Fatal(err)
	}
	if !h.Degraded {
		t.Errorf("a hoard with no party read is not marked degraded")
	}
	said := false
	for _, n := range h.Notes {
		if strings.Contains(n, "tier-only") {
			said = true
		}
	}
	if !said {
		t.Errorf("the degraded hoard does not say it is tier-only: %v", h.Notes)
	}
	for _, line := range h.Items {
		if line.Reason == "" {
			t.Errorf("degraded line %s carries no reason", line.Key)
		}
		if len(line.Warnings) != 0 {
			t.Errorf("degraded line %s warns with no party to compare", line.Key)
		}
	}
	// And with neither a tier nor a level, the ask fails, named.
	if _, err := GenerateHoard(Request{}, testCatalog(), 31); err == nil {
		t.Errorf("a hoard with no tier and no level generated anyway")
	}
}

// Levels derive the tier when the caller has no explicit one.
func TestTierDerivesFromLevels(t *testing.T) {
	h, err := GenerateHoard(Request{Levels: []int{5, 6, 7, 8}}, testCatalog(), 11)
	if err != nil {
		t.Fatal(err)
	}
	if h.Tier != Tier2 || h.Degraded {
		t.Errorf("levels 5-8 gave tier %d (degraded=%v), want tier 2, not degraded", h.Tier, h.Degraded)
	}
}

/* ---------- the distribution ---------- */

// The distribution folds in event-log order, keeps undated holdings
// visibly undated, and names who has received nothing.
func TestDistributionFoldsInEventOrder(t *testing.T) {
	ord := func(n int64) *int64 { return &n }
	pcs := []PC{
		{Name: "Fighter", Level: 6, Items: []Holding{
			{ItemName: "Sword of the Sentinel", Source: SourcePartyBlock},
			{ItemName: "Flamebrand", Source: SourceRelation, Ordinal: ord(12)},
			{ItemName: "Doomblade", Source: SourceRelation, Ordinal: ord(3)},
		}},
		{Name: "Rogue", Level: 6},
		{Name: "Wizard", Level: 7, Items: []Holding{
			{ItemName: "Orb of Depths", Source: SourceFact, Session: ord(9)},
			{ItemName: "Cloak of Shades", Source: SourcePartyBlock},
		}},
	}
	d := DistributionOf(pcs)
	if len(d.PCs) != 3 {
		t.Fatalf("distribution has %d pcs, want 3", len(d.PCs))
	}
	var fighter, rogue PCDistribution
	for _, p := range d.PCs {
		switch p.Name {
		case "Fighter":
			fighter = p
		case "Rogue":
			rogue = p
		}
	}
	if fighter.Total != 3 || rogue.Total != 0 {
		t.Errorf("fighter=%d rogue=%d, want 3 and 0", fighter.Total, rogue.Total)
	}
	if !strings.Contains(d.Notes[0], "Rogue") {
		t.Errorf("the notes never name the pc who has received nothing: %v", d.Notes)
	}
	// Event-log order: ordinal 3 before ordinal 12; the undated party
	// block entry last and visibly undated.
	if fighter.Timeline[0].ItemName != "Doomblade" || fighter.Timeline[1].ItemName != "Flamebrand" {
		t.Errorf("dated rows are not in play order: %s then %s",
			fighter.Timeline[0].ItemName, fighter.Timeline[1].ItemName)
	}
	if fighter.Timeline[2].ItemName != "Sword of the Sentinel" || fighter.Timeline[2].Dated {
		t.Errorf("the undated row does not sort last as undated: %+v", fighter.Timeline[2])
	}
	// Shares are arithmetic: 3 of the party's 5.
	if fighter.SharePct != 60 {
		t.Errorf("fighter share = %d%%, want 60", fighter.SharePct)
	}
}

/* ---------- the power curve ---------- */

// The power curve shows its arithmetic and its expectation, not a bare
// verdict: the XGE numbers merged by rarity, the floor named, the verdict
// carrying the counts.
func TestPowerCurveShowsItsArithmetic(t *testing.T) {
	pcs := []PC{
		{Name: "Fighter", Level: 7, Items: []Holding{
			{ItemName: "Flamebrand", Rarity: "Rare", Tags: []string{"offensive"}},
		}},
		{Name: "Wizard", Level: 7, Items: []Holding{
			{ItemName: "A Strange Key", Tags: nil}, // unclassifiable
		}},
	}
	pc := PowerCurveOf(pcs)
	if !pc.Computable {
		t.Fatalf("a leveled party is computable: %s", pc.Reason)
	}
	if pc.Tier != Tier2 || pc.ExpectationTotal != 45 || pc.FloorTotal != 11 {
		t.Errorf("tier %d expectation %d floor %d, want tier 2 / 45 / 11",
			pc.Tier, pc.ExpectationTotal, pc.FloorTotal)
	}
	if pc.HeldTotal != 2 || pc.Unclassified != 1 || pc.Held["Rare"] != 1 {
		t.Errorf("held = %d (%v), unclassified %d, want 2, one Rare, one unclassified",
			pc.HeldTotal, pc.Held, pc.Unclassified)
	}
	if len(pc.Arithmetic) == 0 || !strings.Contains(pc.Verdict, "behind even the 11") {
		t.Errorf("verdict %q with arithmetic %v is a bare verdict", pc.Verdict, pc.Arithmetic)
	}

	// Between the floor and the expectation, the counts are in the words.
	mid := []PC{
		{Name: "Fighter", Level: 7, Items: []Holding{
			{ItemName: "Flamebrand", Rarity: "Rare", Tags: []string{"offensive"}},
		}},
		{Name: "Wizard", Level: 7},
	}
	for i := 0; i < 19; i++ {
		mid[0].Items = append(mid[0].Items, Holding{ItemName: "Item", Rarity: "Uncommon", Tags: []string{"utility"}})
	}
	pc2 := PowerCurveOf(mid)
	if !strings.Contains(pc2.Verdict, "20 of the 45") {
		t.Errorf("20 items at tier 2 reads %q, want the counts spelled out", pc2.Verdict)
	}

	// A stuffed party reads over-equipped, with the margin in the words.
	rich := []PC{{Name: "Fighter", Level: 5}}
	for i := 0; i < 50; i++ {
		rich[0].Items = append(rich[0].Items, Holding{ItemName: "Item", Rarity: "Uncommon", Tags: []string{"utility"}})
	}
	over := PowerCurveOf(rich)
	if !strings.Contains(over.Verdict, "over-equipped") {
		t.Errorf("50 items at tier 1 reads %q, want over-equipped", over.Verdict)
	}

	// And behind even the floor is its own line.
	poor := []PC{{Name: "Fighter", Level: 17, Items: []Holding{{ItemName: "Item", Rarity: "Rare", Tags: []string{"offensive"}}}}}
	behind := PowerCurveOf(poor)
	if !strings.Contains(behind.Verdict, "behind even") {
		t.Errorf("one item at tier 4 reads %q, want behind the floor", behind.Verdict)
	}
}

// No usable level means no read, with the reason said.
func TestPowerCurveNeedsLevels(t *testing.T) {
	pc := PowerCurveOf([]PC{{Name: "Fighter"}})
	if pc.Computable {
		t.Fatal("a party with no levels is not computable")
	}
	if pc.Reason == "" || pc.Verdict != "not computable" {
		t.Errorf("the read does not say why it cannot compute: %q / %q", pc.Reason, pc.Verdict)
	}
}

/* ---------- the concentration warning ---------- */

// The acceptance fixtures, pinned: an over-concentrated party fires; an
// even party stays silent.
func TestConcentrationFixtures(t *testing.T) {
	// Over-concentrated: the fighter holds three of the party's four
	// offensive items and the rogue has received nothing at all.
	lopsided := []PC{
		{Name: "Fighter", Level: 6, Items: []Holding{
			{ItemName: "A", Rarity: "Uncommon", Tags: []string{"weapon", "offensive"}},
			{ItemName: "B", Rarity: "Rare", Tags: []string{"weapon", "offensive", "damage-rider"}},
			{ItemName: "C", Rarity: "Very Rare", Tags: []string{"offensive"}},
		}},
		{Name: "Rogue", Level: 6},
		{Name: "Cleric", Level: 6, Items: []Holding{
			{ItemName: "D", Rarity: "Uncommon", Tags: []string{"armor", "defensive"}},
			{ItemName: "E", Rarity: "Common", Tags: []string{"offensive"}},
		}},
	}
	warn := Concentrations("Flamebrand", []string{"weapon", "offensive", "damage-rider"}, lopsided)
	if len(warn) == 0 {
		t.Fatal("the over-concentrated party stayed silent")
	}
	found := false
	for _, w := range warn {
		if w.PC == "Fighter" && w.Axis == "offensive" {
			found = true
			if !strings.Contains(w.Reason, "3 of the party's 4") || !strings.Contains(w.Reason, "Rogue") {
				t.Errorf("the warning does not carry its arithmetic: %s", w.Reason)
			}
		}
	}
	if !found {
		t.Errorf("no warning names the fighter's offensive dominance: %+v", warn)
	}

	// Even: every pc holds one offensive item — silent.
	even := []PC{
		{Name: "Fighter", Level: 6, Items: []Holding{{ItemName: "A", Rarity: "Uncommon", Tags: []string{"offensive"}}}},
		{Name: "Wizard", Level: 6, Items: []Holding{{ItemName: "B", Rarity: "Uncommon", Tags: []string{"offensive"}}}},
	}
	if warn := Concentrations("Flamebrand", []string{"offensive"}, even); len(warn) != 0 {
		t.Errorf("the even party fired: %+v", warn)
	}

	// A tie at the top is not dominance.
	tied := []PC{
		{Name: "Fighter", Level: 6, Items: []Holding{
			{ItemName: "A", Rarity: "Uncommon", Tags: []string{"offensive"}}}},
		{Name: "Wizard", Level: 6, Items: []Holding{
			{ItemName: "B", Rarity: "Uncommon", Tags: []string{"offensive"}}}},
		{Name: "Rogue", Level: 6},
	}
	if warn := Concentrations("Flamebrand", []string{"offensive"}, tied); len(warn) != 0 {
		t.Errorf("a tie at the top fired: %+v", warn)
	}

	// A party that declares nothing is silent.
	if warn := Concentrations("Flamebrand", []string{"offensive"}, nil); len(warn) != 0 {
		t.Errorf("an empty party fired: %+v", warn)
	}
}

// The hoard's own lines carry the warning when the party read cannot
// dodge it: a pool of only offensive items against the lopsided party.
func TestHoardLinesCarryTheWarning(t *testing.T) {
	lopsided := []PC{
		{Name: "Fighter", Level: 6, Items: []Holding{
			{ItemName: "A", Rarity: "Uncommon", Tags: []string{"weapon", "offensive"}},
			{ItemName: "B", Rarity: "Rare", Tags: []string{"weapon", "offensive", "damage-rider"}},
		}},
		{Name: "Rogue", Level: 6},
	}
	offensiveOnly := []items.Item{
		{Slug: "x", Name: "X", Rarity: "Uncommon", Type: "Weapon", Tags: []string{"weapon", "offensive"}},
		{Slug: "y", Name: "Y", Rarity: "Rare", Type: "Weapon", Tags: []string{"weapon", "offensive"}},
	}
	h, err := GenerateHoard(Request{Tier: Tier2, Party: lopsided, Levels: []int{6, 6}}, offensiveOnly, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Items) == 0 {
		t.Skip("the seed rolled no magic items")
	}
	for _, line := range h.Items {
		if len(line.Warnings) == 0 {
			t.Errorf("%s against a lopsided party never warns", line.Name)
		}
	}
}
