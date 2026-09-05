package campaign

import (
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/sheet"
)

// The seam between the typed sheet and the party block (MAD-418): a payload
// carrying a sheet reads through it, a payload carrying only the legacy
// top-level keys reads exactly as it always did, and the mapping never
// crosses the definition/state line.

func pcEntity(payload map[string]any) *Entity {
	return &Entity{ID: "e1", CampaignID: "c1", Kind: KindPC, Name: "Thalia", Payload: payload}
}

func TestSheetIsThePartyBlockSourceWhenPresent(t *testing.T) {
	payload := WithSheet(map[string]any{
		// Legacy keys deliberately present and wrong: the sheet wins
		// because it is the definition, and a stale legacy key must not
		// override the sheet the DM wrote last.
		"level": float64(2), "class": "commoner", "ac": float64(10),
	}, sheet.Sheet{
		Classes: []sheet.ClassLevel{{Class: "fighter", Subclass: "champion", Level: 8}, {Class: "wizard", Level: 2}},
		AC:      18, MaxHP: 49,
		Resistances: []string{"poison"},
		Inventory:   []sheet.Item{{Name: "potion of healing", Qty: 3}, {Name: "rope"}},
		Notes:       "shield of the party",
	})
	block, problems := PartyBlockOf(pcEntity(payload))
	if len(problems) > 0 {
		t.Fatalf("problems: %+v", problems)
	}
	if block.Level != 10 {
		t.Fatalf("level = %d, want the sheet's total 10", block.Level)
	}
	if block.Class != "fighter" || block.Subclass != "champion" {
		t.Fatalf("class = %q/%q, want the sheet's first class", block.Class, block.Subclass)
	}
	if block.AC != 18 || block.MaxHP != 49 {
		t.Fatalf("ac/hp = %d/%d", block.AC, block.MaxHP)
	}
	if len(block.DamageResistances) != 1 || block.DamageResistances[0] != "poison" {
		t.Fatalf("resistances = %+v", block.DamageResistances)
	}
	if len(block.Items) != 2 || block.Items[0] != "potion of healing x3" || block.Items[1] != "rope" {
		t.Fatalf("items = %+v", block.Items)
	}
	if block.Notes != "shield of the party" {
		t.Fatalf("notes = %q", block.Notes)
	}
}

// The definition/state line: a sheet declares maxima, and the party block's
// remaining-resources keys stay whatever the payload's own keys say — a
// sheet alone must not silently declare a character at full slots.
func TestSheetDoesNotInventRemainingState(t *testing.T) {
	payload := WithSheet(nil, sheet.Sheet{
		Classes:      []sheet.ClassLevel{{Class: "wizard", Level: 5}},
		Spellcasting: &sheet.Spellcasting{Slots: map[string]int{"1": 4}},
	})
	block, _ := PartyBlockOf(pcEntity(payload))
	if len(block.Resources.SpellSlots) != 0 {
		t.Fatalf("remaining slots = %+v; a sheet must not fill them", block.Resources.SpellSlots)
	}
	if block.HasLevel() == false || block.Level != 5 {
		t.Fatalf("level = %d", block.Level)
	}
}

func TestLegacyKeysStillReadWithoutASheet(t *testing.T) {
	// The pre-MAD-418 shape every existing campaign carries, asserted
	// unchanged: same fields, same tolerance, byte for byte.
	payload := map[string]any{
		"level": float64(5), "class": "rogue", "subclass": "thief",
		"ac": float64(15), "max_hp": float64(33),
		"resources": map[string]any{"spell_slots": map[string]any{"1": float64(2)}, "hit_dice": float64(5)},
	}
	block, problems := PartyBlockOf(pcEntity(payload))
	if len(problems) != 0 {
		t.Fatalf("problems: %+v", problems)
	}
	if block.Level != 5 || block.Class != "rogue" || block.Subclass != "thief" || block.AC != 15 || block.MaxHP != 33 {
		t.Fatalf("legacy block = %+v", block)
	}
	if block.Resources.SpellSlots["1"] != 2 || block.Resources.HitDice != 5 {
		t.Fatalf("legacy resources = %+v", block.Resources)
	}
}

// A broken sheet block falls back to the legacy keys instead of taking the
// whole party read down — the tolerance rule the party block always held.
func TestUndecodableSheetFallsBackToLegacy(t *testing.T) {
	e := pcEntity(map[string]any{
		"sheet": []any{"not", "an", "object"},
		"level": float64(5),
		"class": "fighter",
	})
	block, _ := PartyBlockOf(e)
	if block.Level != 5 || block.Class != "fighter" {
		t.Fatalf("fallback block = %+v", block)
	}
}

func TestSheetOfMarksStructuredAndUnstructured(t *testing.T) {
	if _, has, err := SheetOf(pcEntity(map[string]any{"level": float64(5)})); has || err != nil {
		t.Fatalf("legacy payload: has=%v err=%v, want the unstructured marker", has, err)
	}
	s, has, err := SheetOf(pcEntity(WithSheet(nil, sheet.Sheet{AC: 15})))
	if !has || err != nil || s.AC != 15 {
		t.Fatalf("sheet payload: has=%v err=%v ac=%d", has, err, s.AC)
	}
	// Not a pc: no sheet, no error — the npc entity shape is not ours.
	if _, has, _ := SheetOf(&Entity{Kind: KindNPC, Payload: WithSheet(nil, sheet.Sheet{AC: 15})}); has {
		t.Fatal("an npc payload must not read as a character sheet")
	}
}

// The party table — the read every encounter budget goes through — serves
// both spellings in one campaign: the sheet-backed pc and the legacy pc
// side by side, in name order, levels intact.
func TestPartyTableOfMixesSheetAndLegacy(t *testing.T) {
	entities := []Entity{
		{ID: "a", Kind: KindPC, Name: "Alpha", Payload: WithSheet(nil, sheet.Sheet{Classes: []sheet.ClassLevel{{Class: "cleric", Level: 7}}})},
		{ID: "b", Kind: KindPC, Name: "Bravo", Payload: map[string]any{"level": float64(5)}},
		{ID: "c", Kind: KindNPC, Name: "Charlie", Payload: map[string]any{"level": float64(20)}},
		{ID: "d", Kind: KindPC, Name: "Delta", Status: StatusDeleted, Payload: map[string]any{"level": float64(9)}},
	}
	table := PartyTableOf("c1", entities)
	levels := table.Levels()
	if len(levels) != 2 || levels[0] != 7 || levels[1] != 5 {
		t.Fatalf("levels = %v, want [7 5]", levels)
	}
}
