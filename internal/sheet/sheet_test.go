package sheet

import (
	"encoding/json"
	"testing"
)

// The codec contract (MAD-418): a sheet written through WithSheet and read
// back through FromPayload is byte-stable, foreign payload keys survive,
// and the zero sheet is "no block", not a block full of zeros.

func TestWithSheetPreservesForeignKeys(t *testing.T) {
	payload := map[string]any{
		"level":     float64(5), // the party block's legacy key
		"class":     "fighter",
		"resources": map[string]any{"hit_dice": float64(5)},
		"dm_notes":  "player is moving abroad next month",
	}
	s := Sheet{
		Abilities: Abilities{STR: 16, CON: 14},
		Classes:   []ClassLevel{{Class: "Fighter", Subclass: "Champion", Level: 8}},
		AC:        18,
	}
	out := WithSheet(payload, s)
	if _, ok := out["level"]; !ok {
		t.Fatal("the legacy party-block level key must survive a sheet write")
	}
	if _, ok := out["dm_notes"]; !ok {
		t.Fatal("a foreign payload key was dropped")
	}
	if _, ok := out[PayloadKey]; !ok {
		t.Fatal("the sheet block was not written")
	}

	// And reading back yields what was written.
	got, has, err := FromPayload(out)
	if err != nil || !has {
		t.Fatalf("read back: has=%v err=%v", has, err)
	}
	if got.Abilities.STR != 16 || got.AC != 18 || len(got.Classes) != 1 || got.Classes[0].Level != 8 {
		t.Fatalf("round trip lost fields: %+v", got)
	}
	if got.Version != Version {
		t.Fatalf("version = %d, want %d", got.Version, Version)
	}
}

func TestFromPayloadTolerance(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]any
		has     bool
	}{
		{"nil payload", nil, false},
		{"no sheet key", map[string]any{"level": float64(3)}, false},
		{"null sheet", map[string]any{"sheet": nil}, false},
		{"empty sheet", map[string]any{"sheet": map[string]any{}}, true},
		{"wrong shape", map[string]any{"sheet": "a string"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, has, err := FromPayload(tc.payload)
			if has != tc.has {
				t.Fatalf("has = %v, want %v", has, tc.has)
			}
			// Tolerance means no error on absent blocks; a wrong-shaped
			// block reports itself but never panics.
			if err != nil && tc.has == false {
				t.Fatalf("absent block errored: %v", err)
			}
		})
	}
}

// The round-trip stability rule the API promises: WithSheet of a sheet,
// marshaled, decoded, normalized again, marshaled — identical bytes. This
// is what makes PUT -> GET -> PUT a no-op.
func TestSheetJSONRoundTripStable(t *testing.T) {
	s := Sheet{
		Race:       "mountain dwarf",
		Background: "soldier",
		Alignment:  "lawful good",
		XP:         34000,
		Abilities:  Abilities{STR: 17, DEX: 10, CON: 16, INT: 8, WIS: 13, CHA: 12},
		AC:         18,
		MaxHP:      49,
		Speeds:     map[string]int{"walk": 25},
		Proficiencies: Proficiencies{
			Saves:  []string{"str", "con"},
			Skills: []string{"athletics", "intimidation"},
			Tools:  []string{"smith's tools", "brewer's supplies"},
		},
		Classes:     []ClassLevel{{Class: "fighter", Subclass: "champion", Level: 8}},
		Resistances: []string{"poison"},
		Features:    []Entry{{Name: "Second Wind"}, {Name: "Action Surge"}},
		Traits:      []Entry{{Name: "Dwarven Resilience"}},
		Inventory: []Item{
			{Name: "plate armor", Qty: 1, Equipped: true},
			{Name: "potion of healing", Qty: 3},
			{Name: "flame tongue warhammer", Qty: 1, Equipped: true, Attuned: true},
		},
		Currency: Currency{SP: 12, GP: 55},
	}
	first, err := json.Marshal(WithSheet(nil, s)["sheet"].(map[string]any))
	if err != nil {
		t.Fatal(err)
	}
	var again Sheet
	if err := json.Unmarshal(first, &again); err != nil {
		t.Fatal(err)
	}
	second, err := json.Marshal(WithSheet(nil, again)["sheet"].(map[string]any))
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("round trip is not byte-stable:\nfirst:  %s\nsecond: %s", first, second)
	}
}

func TestTotalLevelAndClassesLabel(t *testing.T) {
	multiclass := Sheet{Classes: []ClassLevel{
		{Class: "fighter", Level: 8},
		{Class: "wizard", Subclass: "war magic", Level: 2},
	}}
	if got := multiclass.TotalLevel(); got != 10 {
		t.Fatalf("total level = %d, want 10", got)
	}
	if got, want := multiclass.ClassesLabel(), "fighter 8/wizard 2"; got != want {
		t.Fatalf("label = %q, want %q", got, want)
	}
	var empty Sheet
	if empty.TotalLevel() != 0 || empty.ClassesLabel() != "" {
		t.Fatal("empty sheet derives no level and no label")
	}
}

func TestAttunementAccounting(t *testing.T) {
	s := Sheet{Inventory: []Item{
		{Name: "a"}, {Name: "b", Attuned: true}, {Name: "c", Attuned: true},
	}}
	if got := len(s.AttunedItems()); got != 2 {
		t.Fatalf("attuned = %d, want 2", got)
	}
	if limit := s.AttunementLimit(); limit != 3 {
		t.Fatalf("default limit = %d, want 3", limit)
	}
	s.AttunementMax = 5
	if limit := s.AttunementLimit(); limit != 5 {
		t.Fatalf("declared limit = %d, want 5", limit)
	}
}

// The empty sheet writes no block at all: a PUT of {} clears the sheet
// rather than storing {"version":1} husks.
func TestEmptySheetWritesNoBlock(t *testing.T) {
	payload := WithSheet(map[string]any{"sheet": map[string]any{"ac": float64(18)}}, Sheet{})
	if _, ok := payload[PayloadKey]; ok {
		t.Fatal("an empty sheet must remove the block, not store a husk")
	}
}
