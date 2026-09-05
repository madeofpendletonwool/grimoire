package sheet

import (
	"encoding/json"
	"os"
	"testing"
)

// The import contract (MAD-418): the native format is the identity, the
// foreign formats are best-effort with their misses reported, and nothing
// is invented. The Roll20 fixtures are real exports, vendored verbatim from
// a public collection of Roll20 character JSONs (github.com/palikhov/
// CnM_Palant_Roll20, roll20_resources_master) — the acceptance criterion
// is exercised against exports as they actually are, not as the parser
// wishes they were. The FC5 fixture is schema-shaped and labeled as such
// inside the file: the FC5 app is iOS-only and no canonical public export
// exists.

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// The e2e path against a real export: a Roll20 character JSON, imported,
// validating, and carrying exactly the fields the export verifiably holds.
func TestImportRoll20RealExport(t *testing.T) {
	data := mustRead(t, "testdata/imports/velren_roll20.json")
	s, rep, err := Import("roll20", data)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if rep.Format != "roll20" || rep.Name != "Velren" {
		t.Fatalf("report = %+v", rep)
	}
	// The abilities the sheet verifiably carries: 10/16/14/16/10/8.
	want := Abilities{STR: 10, DEX: 16, CON: 14, INT: 16, WIS: 10, CHA: 8}
	if s.Abilities != want {
		t.Fatalf("abilities = %+v, want %+v", s.Abilities, want)
	}
	// The export marks int and wis saves proficient (@{PB}); dex carries 0.
	for _, save := range []string{"int", "wis"} {
		if !inSet(save, s.Proficiencies.Saves) {
			t.Errorf("save %q not mapped; got %+v", save, s.Proficiencies.Saves)
		}
	}
	if inSet("dex", s.Proficiencies.Saves) {
		t.Error("an unproficient save was invented")
	}
	if problems := Validate(s); len(problems) > 0 {
		t.Fatalf("real import does not validate: %+v", problems)
	}
}

// The second real export: the spell-row carrying sheet. Its attribs bag
// holds 534 entries; the importer consumes the ones that map and reports
// the shape of what did not.
func TestImportRoll20RealExportWithSpells(t *testing.T) {
	data := mustRead(t, "testdata/imports/ulvara_roll20.json")
	s, rep, err := Import("roll20", data)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if rep.Name != "Ulvara" {
		t.Fatalf("report name = %q", rep.Name)
	}
	if s.Alignment == "" && s.Speeds == nil && s.Spellcasting == nil {
		t.Fatal("the export's carrying fields (alignment, speed, spell rows) all missed")
	}
	if s.Speeds["walk"] != 30 {
		t.Errorf("walk speed = %d, want 30 (the export's flat speed attrib)", s.Speeds["walk"])
	}
	if problems := Validate(s); len(problems) > 0 {
		t.Fatalf("real import does not validate: %+v", problems)
	}
}

func TestImportFC5Sample(t *testing.T) {
	data := mustRead(t, "testdata/imports/sample_fc5.xml")
	s, rep, err := Import("fc5", data)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if rep.Name != "Brannor Steelhelm" {
		t.Fatalf("report name = %q", rep.Name)
	}
	if s.Race != "Mountain Dwarf" || s.Background != "Soldier" {
		t.Errorf("identity fields: %+v", s)
	}
	if s.Abilities.STR != 17 || s.Abilities.INT != 8 {
		t.Errorf("abilities = %+v", s.Abilities)
	}
	if s.AC != 18 || s.MaxHP != 49 || s.XP != 34000 {
		t.Errorf("ac/hp/xp = %d/%d/%d", s.AC, s.MaxHP, s.XP)
	}
	if s.Speeds["walk"] != 25 {
		t.Errorf("walk = %d, want 25", s.Speeds["walk"])
	}
	if len(s.Classes) != 1 || s.Classes[0].Class != "Fighter" || s.Classes[0].Level != 8 || s.Classes[0].Subclass != "Champion" {
		t.Errorf("classes = %+v", s.Classes)
	}
	if !inSet("str", s.Proficiencies.Saves) || !inSet("con", s.Proficiencies.Saves) {
		t.Errorf("saves = %+v", s.Proficiencies.Saves)
	}
	if !inSet("athletics", s.Proficiencies.Skills) || !inSet("intimidation", s.Proficiencies.Skills) {
		t.Errorf("skills = %+v", s.Proficiencies.Skills)
	}
	if s.Currency.GP != 55 || s.Currency.SP != 12 {
		t.Errorf("currency = %+v", s.Currency)
	}
	if len(s.Inventory) != 3 {
		t.Fatalf("inventory = %+v", s.Inventory)
	}
	for _, it := range s.Inventory {
		if it.Name == "Flame tongue warhammer" && (!it.Attuned || !it.Equipped) {
			t.Errorf("flame tongue flags: %+v", it)
		}
		if it.Name == "Potion of healing" && it.Qty != 3 {
			t.Errorf("potion qty = %d", it.Qty)
		}
	}
	if !inSet("Second Wind", entryNames(s.Features)) {
		t.Errorf("features = %+v", s.Features)
	}
	if problems := Validate(s); len(problems) > 0 {
		t.Fatalf("sample does not validate: %+v", problems)
	}
}

// The native format is the identity: import of a marshaled sheet is the
// sheet, and import of a GET envelope re-imports its sheet unchanged.
func TestImportGrimoireIsIdentity(t *testing.T) {
	s := Sheet{
		Version:   Version,
		Abilities: Abilities{STR: 16, CON: 14},
		Classes:   []ClassLevel{{Class: "fighter", Level: 8}},
		Inventory: []Item{{Name: "rope", Qty: 1}},
	}
	blob, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	got, rep, err := Import("grimoire", blob)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Format != "grimoire" {
		t.Fatalf("report = %+v", rep)
	}
	if got.Abilities.STR != 16 || len(got.Classes) != 1 || got.Classes[0].Level != 8 || len(got.Inventory) != 1 {
		t.Fatalf("identity import lost fields: %+v", got)
	}

	// The GET envelope: {"entity_id":..., "sheet": {...}}.
	envelope, err := json.Marshal(map[string]any{
		"entity_id": "e1", "name": "Thalia", "structured": true, "sheet": s,
	})
	if err != nil {
		t.Fatal(err)
	}
	got2, _, err := Import("grimoire", envelope)
	if err != nil {
		t.Fatal(err)
	}
	if got2.Abilities.STR != 16 || len(got2.Classes) != 1 {
		t.Fatalf("envelope import lost fields: %+v", got2)
	}
}

func TestImportAutoSniffs(t *testing.T) {
	xml, rep, err := Import("auto", mustRead(t, "testdata/imports/sample_fc5.xml"))
	if err != nil || rep.Format != "fc5" {
		t.Fatalf("auto: format=%s err=%v", rep.Format, err)
	}
	if xml.Race != "Mountain Dwarf" {
		t.Fatal("sniffed fc5 did not parse")
	}
	r20, rep, err := Import("auto", mustRead(t, "testdata/imports/velren_roll20.json"))
	if err != nil || rep.Format != "roll20" {
		t.Fatalf("auto: format=%s err=%v", rep.Format, err)
	}
	if r20.Abilities.DEX != 16 {
		t.Fatal("sniffed roll20 did not parse")
	}
	native, rep, err := Import("auto", []byte(`{"race":"elf","ac":15}`))
	if err != nil || rep.Format != "grimoire" {
		t.Fatalf("auto: format=%s err=%v", rep.Format, err)
	}
	if native.Race != "elf" {
		t.Fatal("sniffed native did not parse")
	}
	if _, _, err := Import("auto", []byte("\x00binary noise")); err == nil {
		t.Fatal("unsniffable data must be an error, never a guess")
	}
	if _, _, err := Import("lotus123", []byte("{}")); err == nil {
		t.Fatal("unknown format must be an error")
	}
}

// A compendium of many characters is a library, not a sheet — the endpoint
// imports one character, and says so rather than picking a winner.
func TestImportFC5RefusesLibraries(t *testing.T) {
	library := []byte(`<compendium><character><name>A</name></character><character><name>B</name></character></compendium>`)
	if _, _, err := Import("fc5", library); err == nil {
		t.Fatal("a two-character compendium must be refused")
	}
}

func entryNames(es []Entry) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.Name)
	}
	return out
}
