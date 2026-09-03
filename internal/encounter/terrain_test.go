package encounter

import (
	"strings"
	"testing"
)

// Terrain is generated with the objective, and every feature it hands out is
// a structured value: a kind from the declared vocabulary, the mechanical
// effect the vocabulary gives it, and where it sits. Defeat generates
// nothing at all.
func TestTerrainGeneratedWithTheObjective(t *testing.T) {
	empty := TerrainFor(Objective{Kind: Defeat}, BandMedium, 3)
	if len(empty.Features) != 0 || len(empty.Hazards) != 0 {
		t.Fatalf("defeat generated terrain: %+v", empty)
	}

	for _, k := range Kinds[1:] {
		tt := TerrainFor(Objective{Kind: k}, BandMedium, 3)
		if len(tt.Features) < 2 {
			t.Errorf("%s: %d features, want at least two — the objective needs ground to be true on", k, len(tt.Features))
		}
		for _, f := range tt.Features {
			effect, ok := featureEffects[f.Kind]
			if !ok {
				t.Errorf("%s: feature kind %q is outside the vocabulary", k, f.Kind)
				continue
			}
			if f.Effect != effect {
				t.Errorf("%s: feature %q carries %q, want the declared effect %q", k, f.Kind, f.Effect, effect)
			}
			if strings.TrimSpace(f.Area) == "" {
				t.Errorf("%s: feature %q has no place on the board", k, f.Kind)
			}
		}
		for _, h := range tt.Hazards {
			if _, ok := hazardKinds[h.Kind]; !ok {
				t.Errorf("%s: hazard kind %q is outside the vocabulary", k, h.Kind)
			}
			if h.DC < 10 || h.DC > 23 {
				t.Errorf("%s: hazard DC %d is outside every tier band", k, h.DC)
			}
			if !diceRE.MatchString(h.Damage) {
				t.Errorf("%s: hazard damage %q is not a dice expression", k, h.Damage)
			}
			if h.Trigger == "" || h.Area == "" || h.SaveAbility == "" || h.DamageType == "" {
				t.Errorf("%s: hazard %q is missing structure: %+v", k, h.Kind, h)
			}
		}
	}

	// Same objective, same party, same band: the same battlefield, every time.
	a := TerrainFor(Objective{Kind: Reach}, BandHard, 7)
	b := TerrainFor(Objective{Kind: Reach}, BandHard, 7)
	if len(a.Features) != len(b.Features) || len(a.Hazards) != len(b.Hazards) {
		t.Fatal("terrain generation is not deterministic")
	}
	for i := range a.Features {
		if a.Features[i] != b.Features[i] {
			t.Errorf("feature %d drifted: %+v vs %+v", i, a.Features[i], b.Features[i])
		}
	}
}

// Hazard DCs and damage sit inside the DMG's tier bands, asserted per tier:
// the DC column is the book's Trap Save DCs, the damage column its Damage
// Severity by Level.
func TestHazardPricingFollowsTheDMGTiers(t *testing.T) {
	for _, c := range []struct {
		avgLevel float64
		tier     int
	}{
		{1, 0}, {4, 0}, {5, 1}, {10, 1}, {11, 2}, {16, 2}, {17, 3}, {20, 3},
	} {
		if got := hazardTier(c.avgLevel); got != c.tier {
			t.Errorf("hazardTier(%v) = %d, want tier %d", c.avgLevel, got, c.tier)
		}
	}
	for _, c := range []struct {
		band string
		sev  int
	}{
		{BandEasy, 0}, {BandMedium, 1}, {BandHard, 2}, {BandDeadly, 2},
	} {
		if got := hazardSeverity(c.band); got != c.sev {
			t.Errorf("hazardSeverity(%q) = %d, want %d", c.band, got, c.sev)
		}
	}

	for tier := 0; tier < 4; tier++ {
		avgLevel := []float64{3, 7, 13, 18}[tier]
		for _, band := range Bands {
			tt := TerrainFor(Objective{Kind: Reach}, band, avgLevel)
			if len(tt.Hazards) == 0 {
				t.Fatalf("reach at %s generated no hazard", band)
			}
			h := tt.Hazards[0]
			sev := hazardSeverity(band)
			if want := trapDCs[tier][sev]; h.DC != want {
				t.Errorf("tier %d, %s: DC = %d, want the DMG's %d", tier, band, h.DC, want)
			}
			if want := trapDamage[tier][sev]; h.Damage != want {
				t.Errorf("tier %d, %s: damage = %s, want the DMG's %s", tier, band, h.Damage, want)
			}
		}
	}
}

// Terrain that arrives from outside must speak the declared vocabulary: an
// unknown feature kind or a hazard outside the game's grammar is refused,
// never stored as prose.
func TestTerrainValidate(t *testing.T) {
	valid := TerrainFor(Objective{Kind: Reach}, BandMedium, 3)
	if err := valid.Validate(); err != nil {
		t.Fatalf("generated terrain refused: %v", err)
	}

	badFeature := Terrain{Features: []Feature{{Kind: "lava", Effect: "it hurts", Area: "everywhere"}}}
	if err := badFeature.Validate(); err == nil {
		t.Error("an unknown feature kind was accepted")
	}
	effectless := Terrain{Features: []Feature{{Kind: "cover", Effect: "it looks nice", Area: "left"}}}
	if err := effectless.Validate(); err != nil {
		// The kind carries the effect, so a rewritten effect string is still
		// structurally fine — the vocabulary check is on the kind.
		t.Errorf("a cover feature is valid whatever its stored text: %v", err)
	}

	if err := (Terrain{Hazards: []Hazard{{Kind: "lava"}}}).Validate(); err == nil {
		t.Error("an unknown hazard kind was accepted")
	}
	badSave := valid
	badSave.Hazards = []Hazard{{Kind: "rockfall", Name: "Rockfall", SaveAbility: "luck", DC: 15, Damage: "2d10", DamageType: "bludgeoning", Trigger: "t", Area: "a"}}
	if err := badSave.Validate(); err == nil {
		t.Error("a hazard saving on an ability the game does not have was accepted")
	}
	badDC := valid
	badDC.Hazards = []Hazard{{Kind: "rockfall", Name: "Rockfall", SaveAbility: "dex", DC: 45, Damage: "2d10", DamageType: "bludgeoning", Trigger: "t", Area: "a"}}
	if err := badDC.Validate(); err == nil {
		t.Error("a DC 45 hazard was accepted")
	}
	badDice := valid
	badDice.Hazards = []Hazard{{Kind: "rockfall", Name: "Rockfall", SaveAbility: "dex", DC: 15, Damage: "lots", DamageType: "bludgeoning", Trigger: "t", Area: "a"}}
	if err := badDice.Validate(); err == nil {
		t.Error("hazard damage that is not a dice expression was accepted")
	}
	noTrigger := valid
	noTrigger.Hazards = []Hazard{{Kind: "rockfall", Name: "Rockfall", SaveAbility: "dex", DC: 15, Damage: "2d10", DamageType: "bludgeoning", Area: "a"}}
	if err := noTrigger.Validate(); err == nil {
		t.Error("a hazard with no trigger was accepted")
	}
}
