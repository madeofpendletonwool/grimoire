package place

// The settlement shape arithmetic (MAD-372): every kind and scale band's
// counts, the sub-location kinds each settlement type requires, the item
// band, and the refusals. All of it asserted with no model anywhere in
// sight — the structure is computed before the prompt.

import (
	"strings"
	"testing"
)

func TestShapeFor_EveryKindAndScale(t *testing.T) {
	for _, kind := range Kinds() {
		for _, scale := range Scales() {
			s, err := ShapeFor(kind, scale)
			if err != nil {
				t.Fatalf("%s/%s: %v", kind, scale, err)
			}
			if s.Kind != kind || s.Scale != scale {
				t.Errorf("%s/%s returned %+v", kind, scale, s)
			}
			if s.NPCs < 1 {
				t.Errorf("%s/%s: a settlement with no notable people", kind, scale)
			}
			if len(s.Subs) < 1 {
				t.Errorf("%s/%s: a settlement with no sub-locations", kind, scale)
			}
			if s.Services < 1 {
				t.Errorf("%s/%s: a settlement that sells nothing", kind, scale)
			}
			// The issue's scope: two or three hooks, one or two secrets.
			if s.Hooks < 2 || s.Hooks > 3 {
				t.Errorf("%s/%s: hooks = %d, outside 2-3", kind, scale, s.Hooks)
			}
			if s.Secrets < 1 || s.Secrets > 2 {
				t.Errorf("%s/%s: secrets = %d, outside 1-2", kind, scale, s.Secrets)
			}
			// Bands grow (or hold) with scale.
			if s.MinItems() == 0 {
				t.Errorf("%s/%s: zero items", kind, scale)
			}
		}
	}
}

func TestShapeFor_DefaultsAndRefusals(t *testing.T) {
	s, err := ShapeFor(KindVillage, "")
	if err != nil {
		t.Fatal(err)
	}
	if s.Scale != ScaleMedium {
		t.Errorf("empty scale = %q, want medium", s.Scale)
	}
	if _, err := ShapeFor("megacity", ""); err == nil || !strings.Contains(err.Error(), "hamlet, village, town, city") {
		t.Errorf("unknown kind refusal = %v", err)
	}
	if _, err := ShapeFor(KindVillage, "colossal"); err == nil || !strings.Contains(err.Error(), "small, medium, large") {
		t.Errorf("unknown scale refusal = %v", err)
	}
}

func TestShape_SubKindsFitTheSettlement(t *testing.T) {
	// A village has no cathedral district; a city is nothing but districts.
	village, _ := ShapeFor(KindVillage, ScaleMedium)
	for _, s := range village.Subs {
		if s.Role == "district" {
			t.Error("a village generated a district")
		}
	}
	if village.Districts {
		t.Error("a village marked as districts")
	}
	city, _ := ShapeFor(KindCity, ScaleMedium)
	if !city.Districts {
		t.Error("a city not marked as districts")
	}
	for _, s := range city.Subs {
		if s.Role != "district" {
			t.Errorf("city sub %s is a %s, want a district", s.ID, s.Role)
		}
	}
}

func TestShape_ItemBand(t *testing.T) {
	village, _ := ShapeFor(KindVillage, ScaleMedium)
	// 1 place + 3 subs × 2 + 3 npcs × 2 + 2 hooks + 2 secrets × 2.
	wantMin := 1 + 2*village.SubLocations() + 2*village.NPCs + village.Hooks + 2*village.Secrets
	if village.MinItems() != wantMin {
		t.Fatalf("MinItems = %d, want %d", village.MinItems(), wantMin)
	}
	if village.MaxItems() != wantMin+village.NPCs {
		t.Fatalf("MaxItems = %d, want %d (one faction edge per npc)", village.MaxItems(), wantMin+village.NPCs)
	}
	if !village.InsideBand(wantMin) || !village.InsideBand(village.MaxItems()) {
		t.Error("the band does not contain its own bounds")
	}
	if village.InsideBand(wantMin-1) || village.InsideBand(village.MaxItems()+1) {
		t.Error("the band admits counts outside it")
	}
	// Bigger places stage more items, never fewer, across the kinds.
	hamlet, _ := ShapeFor(KindHamlet, ScaleSmall)
	town, _ := ShapeFor(KindTown, ScaleMedium)
	if hamlet.MinItems() >= village.MinItems() || village.MinItems() >= town.MinItems() {
		t.Errorf("bands do not grow: hamlet %d, village %d, town %d",
			hamlet.MinItems(), village.MinItems(), town.MinItems())
	}
}
