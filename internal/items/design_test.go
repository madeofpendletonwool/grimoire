package items

// The item designer's tests (MAD-383). The structural rules are asserted
// one malformed design at a time; the comparison is asserted as
// comparison — bands, notes, neighbours — and the report is asserted to
// carry no computed verdict, because that absence is the feature.

import (
	"context"
	"strings"
	"testing"
)

// flamebrand is a sound weapon design: a base item, a bonus inside the
// game's ceiling, attunement, and a damage rider in the game's own
// vocabulary.
func flamebrand() Design {
	return Design{
		Name: "Emberbrand", Type: "weapon", Base: "longsword", Bonus: 1,
		Attunement: Attunement{Required: true},
		Effects: []Effect{{
			Text:   "When you hit with it, the target takes an extra 1d6 fire damage.",
			Damage: "1d6 fire",
		}},
		Text: "The blade remembers the forge it came from.",
	}
}

func TestDesignValidateAcceptsTheSound(t *testing.T) {
	d := flamebrand()
	if problems := d.Validate(); len(problems) != 0 {
		t.Fatalf("a sound design was rejected: %v", problems)
	}
}

func TestDesignValidateRejectsTheBroken(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Design)
		want string // a substring the specific problem must carry
	}{
		{"no name", func(d *Design) { d.Name = " " }, "needs a name"},
		{"unknown type", func(d *Design) { d.Type = "artifact" }, `item type "artifact"`},
		{"weapon without a base item", func(d *Design) { d.Base = "" }, "must name the base item"},
		{"armor without a base item", func(d *Design) {
			d.Type, d.Base = "armor", ""
		}, "must name the base item"},
		{"bonus past the ceiling", func(d *Design) { d.Bonus = 5 }, "ceiling"},
		{"attunement condition without the requirement", func(d *Design) {
			d.Attunement = Attunement{Required: false, Condition: "by a cleric"}
		}, "attunement condition"},
		{"attunement on a consumable", func(d *Design) {
			d.Type, d.Base, d.Bonus = "potion", "", 0
			d.Attunement = Attunement{Required: true}
			d.Effects = nil
		}, "cannot require attunement"},
		{"charges with no recovery", func(d *Design) {
			d.Charges, d.Recharge = 7, ""
		}, "states no way to regain"},
		{"ungrammatical recharge", func(d *Design) {
			d.Charges, d.Recharge = 7, "recharges whenever the moon is right"
		}, "does not read as charge recovery"},
		{"recharge without charges", func(d *Design) {
			d.Recharge = "1d6+1 daily at dawn"
		}, "holds no charges"},
		{"effect with no game vocabulary", func(d *Design) {
			d.Effects = []Effect{{Text: "The target is weakened."}}
		}, "states no game vocabulary"},
		{"save without a DC", func(d *Design) {
			d.Effects = []Effect{{Text: "A save.", Save: &EffectSave{DC: 0, Ability: "dexterity"}}}
		}, "no readable DC"},
		{"save with an unknown ability", func(d *Design) {
			d.Effects = []Effect{{Text: "A save.", Save: &EffectSave{DC: 15, Ability: "grit"}}}
		}, "is not one of"},
		{"damage not in dice", func(d *Design) {
			d.Effects = []Effect{{Text: "It burns.", Damage: "a lot of fire"}}
		}, "not in dice"},
		{"bonus with no target", func(d *Design) {
			d.Effects = []Effect{{Text: "It is better.", Bonus: 1}}
		}, "not one of"},
	}
	for _, tc := range cases {
		d := flamebrand()
		tc.mut(&d)
		problems := d.Validate()
		if len(problems) == 0 {
			t.Errorf("%s: the structural rules said nothing", tc.name)
			continue
		}
		if !strings.Contains(strings.Join(problems, " | "), tc.want) {
			t.Errorf("%s: problems %v, want one naming %q", tc.name, problems, tc.want)
		}
	}
}

// The grammar accepts exactly the printed shapes: amount plus recovery
// time.
func TestRechargeGrammar(t *testing.T) {
	yes := []string{"1d6+1 daily at dawn", "all daily at dusk", "1d10 at dawn", "1d6 daily", "daily", "all at midnight"}
	no := []string{"", "recharges whenever", "1d6+1 sometimes", "at the next full moon", "1d6+1"}
	for _, s := range yes {
		if !rechargeReads(s) {
			t.Errorf("rechargeReads(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if rechargeReads(s) {
			t.Errorf("rechargeReads(%q) = true, want false", s)
		}
	}
}

// The corpus fixture is the shelf the bands are derived from; the
// comparison test needs the same data the catalog tests hand-checked.
func corpusFor(t *testing.T) []Item {
	t.Helper()
	srv := catalogFixture(t)
	cat, _ := newCatalog(t, srv.URL)
	if err := cat.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	return cat.All()
}

func TestComparePlacesAgainstTheBands(t *testing.T) {
	corpus := corpusFor(t)

	// A +3 weapon labelled Uncommon: the note must state where the corpus
	// actually carries +3s — a checkable claim, not a verdict.
	d := flamebrand()
	d.Bonus = 3
	d.Rarity = "Uncommon"
	rep := Compare(d, corpus)
	if len(rep.Problems) != 0 {
		t.Fatalf("a sound design was rejected: %v", rep.Problems)
	}
	if rep.Rarity != "Uncommon" {
		t.Errorf("report rarity = %q, want the DM's own label echoed", rep.Rarity)
	}
	if len(rep.Bands) != 5 {
		t.Fatalf("bands = %d, want one per rarity", len(rep.Bands))
	}
	legendNote := false
	for _, n := range rep.Notes {
		if strings.Contains(n, "Every SRD item carrying a +3 bonus is Legendary") {
			legendNote = true
		}
	}
	if !legendNote {
		t.Errorf("notes %v, want the checkable claim about +3s", rep.Notes)
	}

	// Inside the band: the note states the band, still no verdict.
	d.Bonus = 1
	d.Rarity = "Rare"
	rep = Compare(d, corpus)
	inside := false
	for _, n := range rep.Notes {
		if strings.Contains(n, "At Rare the SRD items' bonuses run") {
			inside = true
		}
		if strings.Contains(strings.ToLower(n), "verdict") || strings.Contains(n, "computed") {
			t.Errorf("note %q claims authority the designer does not have", n)
		}
	}
	if !inside {
		t.Errorf("notes %v, want the inside-the-band statement", rep.Notes)
	}
}

// A broken design is not compared against anything: problems only. There
// is no rarity, no bands, no neighbours — nothing that would dress a
// non-item up as a design.
func TestCompareRefusesTheBroken(t *testing.T) {
	d := flamebrand()
	d.Effects = []Effect{{Text: "The target is weakened."}}
	rep := Compare(d, corpusFor(t))
	if len(rep.Problems) == 0 {
		t.Fatal("the structural rules said nothing")
	}
	if rep.Notes != nil || rep.Neighbours != nil {
		t.Errorf("a broken draft was compared: notes %v neighbours %v", rep.Notes, rep.Neighbours)
	}
}

// The neighbours are the DM's yardsticks: the items closest to the design
// on the things a DM would call similar. Pinned against the fixture
// shelf, a +1 fire-rider longsword must be measured against Flame Tongue
// — never against the potion.
func TestNeighboursPinTheYardsticks(t *testing.T) {
	corpus := corpusFor(t)
	d := flamebrand()
	neighbours := NeighboursOf(d, corpus, 3)
	if len(neighbours) != 3 {
		t.Fatalf("neighbours = %d, want 3", len(neighbours))
	}
	if neighbours[0].Name != "Flame Tongue" {
		t.Errorf("first neighbour = %q, want Flame Tongue", neighbours[0].Name)
	}
	for _, n := range neighbours {
		if !strings.EqualFold(n.Type, "weapon") {
			t.Errorf("neighbour %q is a %s — not a yardstick for a weapon", n.Name, n.Type)
		}
		if n.Homebrew {
			t.Errorf("neighbour %q is homebrew; the comparison names official items", n.Name)
		}
		if len(n.Shares) == 0 {
			t.Errorf("neighbour %q carries no reason", n.Name)
		}
	}
}

func TestMetricsComeFromWhatTheItemSays(t *testing.T) {
	srv := catalogFixture(t)
	cat, _ := newCatalog(t, srv.URL)
	if err := cat.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	staff, ok := cat.Lookup("Staff of Power", nil)
	if !ok {
		t.Fatal("staff missing")
	}
	m := MetricsOfItem(staff)
	if m.Bonus != 2 || m.Charges != 20 || m.SaveDC != 0 {
		t.Errorf("staff metrics = %+v, want bonus 2, 20 charges", m)
	}
	if m.RechargePerDay != 13 { // 2d8+4 averages 13
		t.Errorf("staff recharge per day = %v, want 13", m.RechargePerDay)
	}

	d := flamebrand()
	d.Charges = 10
	d.Recharge = "1d6+2 daily at dawn"
	dm := MetricsOfDesign(d)
	if dm.Bonus != 1 || dm.Charges != 10 || dm.RechargePerDay != 5.5 || dm.DamagePerRoll != 3.5 {
		t.Errorf("design metrics = %+v, want the structured fields' own numbers", dm)
	}
}
