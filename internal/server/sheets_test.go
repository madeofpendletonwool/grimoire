package server

// The typed character sheet surface (MAD-418): round-trip, validation,
// scoping, and the import door — over the real HTTP stack the campaign
// tests always use, because the contract being pinned is the wire's.

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
)

func TestCharacterSheetRoundTripsLosslessly(t *testing.T) {
	s, _, _, _ := newCampaignServer(t)
	f := buildFixture(t, s)
	dm := dmSession(t, s)
	base := "/api/campaigns/" + f.campaignID

	sheetJSON := `{
		"race": "mountain dwarf",
		"background": "soldier",
		"alignment": "lawful good",
		"xp": 34000,
		"abilities": {"str": 17, "dex": 10, "con": 16, "int": 8, "wis": 13, "cha": 12},
		"ac": 18,
		"max_hp": 49,
		"speeds": {"walk": 25},
		"proficiencies": {"saves": ["str", "con"], "skills": ["athletics", "intimidation"], "tools": ["smith's tools"]},
		"classes": [{"class": "fighter", "subclass": "champion", "level": 8}],
		"resistances": ["poison"],
		"features": [{"name": "Second Wind"}, {"name": "Action Surge"}],
		"traits": [{"name": "Dwarven Resilience"}],
		"inventory": [
			{"name": "plate armor", "qty": 1, "equipped": true},
			{"name": "potion of healing", "qty": 3},
			{"name": "flame tongue warhammer", "qty": 1, "equipped": true, "attuned": true}
		],
		"currency": {"sp": 12, "gp": 55},
		"notes": "the shield of the party"
	}`
	rec := hit(t, s, http.MethodPut, base+"/characters/"+f.pcID+"/sheet", sheetJSON, dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("put sheet: status %d, body %s", rec.Code, rec.Body)
	}

	get := hit(t, s, http.MethodGet, base+"/characters/"+f.pcID+"/sheet", "", dm)
	if get.Code != http.StatusOK {
		t.Fatalf("get sheet: status %d", get.Code)
	}
	var first struct {
		EntityID   string          `json:"entity_id"`
		Structured bool            `json:"structured"`
		Sheet      json.RawMessage `json:"sheet"`
	}
	if err := json.Unmarshal(get.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if !first.Structured || first.EntityID != f.pcID || len(first.Sheet) == 0 {
		t.Fatalf("read = structured:%v id:%s sheet:%s", first.Structured, first.EntityID, first.Sheet)
	}

	// The round trip: PUT the GET's sheet back, GET again — byte-identical.
	if rec = hit(t, s, http.MethodPut, base+"/characters/"+f.pcID+"/sheet", string(first.Sheet), dm); rec.Code != http.StatusOK {
		t.Fatalf("put back: status %d, body %s", rec.Code, rec.Body)
	}
	again := hit(t, s, http.MethodGet, base+"/characters/"+f.pcID+"/sheet", "", dm)
	if again.Code != http.StatusOK {
		t.Fatalf("get again: status %d", again.Code)
	}
	var second struct {
		Sheet json.RawMessage `json:"sheet"`
	}
	if err := json.Unmarshal(again.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if string(first.Sheet) != string(second.Sheet) {
		t.Fatalf("round trip is not lossless:\n%s\n%s", first.Sheet, second.Sheet)
	}
}

func TestCharacterSheetValidationRejectsNonsense(t *testing.T) {
	s, _, _, _ := newCampaignServer(t)
	f := buildFixture(t, s)
	dm := dmSession(t, s)
	base := "/api/campaigns/" + f.campaignID

	body := `{
		"abilities": {"str": 42},
		"classes": [{"class": "wizard", "level": 25}, {"class": "wizard", "level": 4}],
		"resistances": ["spooky"],
		"proficiencies": {"saves": ["luck"]},
		"speeds": {"tunnel": 30},
		"spellcasting": {"ability": "broodiness", "slots": {"11": 1}}
	}`
	rec := hit(t, s, http.MethodPut, base+"/characters/"+f.pcID+"/sheet", body, dm)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body)
	}
	var res struct {
		Error    string `json:"error"`
		Problems []struct {
			Field  string `json:"field"`
			Detail string `json:"detail"`
		} `json:"problems"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	fields := map[string]string{}
	for _, p := range res.Problems {
		fields[p.Field] = p.Detail
	}
	for _, want := range []string{
		"abilities.str", "classes[0].level", "classes[1].class", "classes",
		"resistances[0]", "proficiencies.saves[0]", "speeds.tunnel",
		"spellcasting.ability", "spellcasting.slots.11",
	} {
		if _, ok := fields[want]; !ok {
			t.Errorf("no problem named %s; got %+v", want, fields)
		}
	}
	if !strings.Contains(fields["resistances[0]"], "thirteen") {
		t.Errorf("resistance detail does not name the vocabulary: %q", fields["resistances[0]"])
	}

	// The rejected sheet was not stored.
	get := hit(t, s, http.MethodGet, base+"/characters/"+f.pcID+"/sheet", "", dm)
	var after struct {
		Structured bool `json:"structured"`
	}
	_ = json.Unmarshal(get.Body.Bytes(), &after)
	if after.Structured {
		t.Fatal("a rejected sheet must not be stored")
	}
}

// The visible marker: a pc whose payload predates the typed sheet reads
// structured:false — an honest "unstructured sheet", never a silent
// invention of one.
func TestCharacterSheetUnstructuredMarker(t *testing.T) {
	s, _, _, _ := newCampaignServer(t)
	f := buildFixture(t, s)
	dm := dmSession(t, s)
	base := "/api/campaigns/" + f.campaignID

	// The fixture's pc carries the party block's legacy shape.
	if rec := hit(t, s, http.MethodPatch, base+"/entities/"+f.pcID,
		`{"payload":{"level":5,"class":"wizard"}}`, dm); rec.Code != http.StatusOK {
		t.Fatalf("patch legacy payload: %d %s", rec.Code, rec.Body)
	}
	rec := hit(t, s, http.MethodGet, base+"/characters/"+f.pcID+"/sheet", "", dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"structured":false`) {
		t.Fatalf("the unstructured marker is missing: %s", rec.Body)
	}
	if strings.Contains(rec.Body.String(), `"sheet"`) {
		t.Fatalf("an unstructured read must not carry a sheet: %s", rec.Body)
	}
}

func TestPlayerReadsOwnSheetOnly(t *testing.T) {
	s, _, _, _ := newCampaignServer(t)
	f := buildFixture(t, s)
	dm := dmSession(t, s)
	base := "/api/campaigns/" + f.campaignID

	// The DM gives Mira a real sheet, and the fixture's secret stays secret.
	sheetJSON := `{"abilities":{"int":16},"classes":[{"class":"wizard","level":5}],"ac":13,"max_hp":32}`
	if rec := hit(t, s, http.MethodPut, base+"/characters/"+f.pcID+"/sheet", sheetJSON, dm); rec.Code != http.StatusOK {
		t.Fatalf("put sheet: %d %s", rec.Code, rec.Body)
	}
	// A second pc that is not the player's.
	other := hit(t, s, http.MethodPost, base+"/entities", `{"kind":"pc","name":"Someone Else"}`, dm)
	otherID := idFrom(t, other, "entity")

	player := addPlayerMember(t, s, f, "mira", true)

	rec := hit(t, s, http.MethodGet, base+"/characters/"+f.pcID+"/sheet", "", player)
	if rec.Code != http.StatusOK {
		t.Fatalf("own sheet read: status %d, body %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"structured":true`) || !strings.Contains(rec.Body.String(), `"wizard"`) {
		t.Fatalf("own sheet read body: %s", rec.Body)
	}
	if strings.Contains(rec.Body.String(), "vampire") {
		t.Fatal("the campaign's secret leaked through the sheet read")
	}

	// Anyone else's sheet: refused, with no hint beyond the refusal.
	if rec = hit(t, s, http.MethodGet, base+"/characters/"+otherID+"/sheet", "", player); rec.Code != http.StatusForbidden {
		t.Fatalf("other pc's sheet: status %d, want 403", rec.Code)
	}
	// And the player's entity read still drops payloads — the sheet route
	// is the one deliberate widening, not a general one.
	if rec = hit(t, s, http.MethodGet, base+"/entities/"+f.pcID, "", player); rec.Code == http.StatusOK {
		if strings.Contains(rec.Body.String(), `"payload"`) {
			t.Fatal("the entity read started carrying payloads at player scope")
		}
	}

	// A player bound to no character (party scope) reads nobody's sheet.
	unbound := addPlayerMember(t, s, f, "wanderer", false)
	if rec = hit(t, s, http.MethodGet, base+"/characters/"+f.pcID+"/sheet", "", unbound); rec.Code != http.StatusForbidden {
		t.Fatalf("party-scope sheet read: status %d, want 403", rec.Code)
	}
}

// The import door, exercised e2e on a real exported sheet: the Roll20
// fixture vendored verbatim in internal/sheet/testdata, posted through the
// API, creating a structured pc whose fields are exactly the export's.
func TestImportCharacterEndToEnd(t *testing.T) {
	s, _, _, _ := newCampaignServer(t)
	f := buildFixture(t, s)
	dm := dmSession(t, s)
	base := "/api/campaigns/" + f.campaignID

	real, err := os.ReadFile("../sheet/testdata/imports/velren_roll20.json")
	if err != nil {
		t.Fatal(err)
	}
	// The export travels as the data field of the import envelope.
	envelope := map[string]any{"format": "roll20", "data": json.RawMessage(real)}
	body, _ := json.Marshal(envelope)
	rec := hit(t, s, http.MethodPost, base+"/characters/import", string(body), dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("import: status %d, body %s", rec.Code, rec.Body)
	}
	var created struct {
		Entity struct {
			ID   string `json:"id"`
			Kind string `json:"kind"`
			Name string `json:"name"`
		} `json:"entity"`
		Sheet struct {
			Abilities struct {
				STR int `json:"str"`
				DEX int `json:"dex"`
				CON int `json:"con"`
				INT int `json:"int"`
			} `json:"abilities"`
			Proficiencies struct {
				Saves []string `json:"saves"`
			} `json:"proficiencies"`
		} `json:"sheet"`
		Report struct {
			Format string `json:"format"`
			Name   string `json:"name"`
		} `json:"report"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Entity.Kind != "pc" || created.Entity.Name != "Velren" {
		t.Fatalf("entity = %+v", created.Entity)
	}
	if created.Report.Format != "roll20" {
		t.Fatalf("report = %+v", created.Report)
	}
	if created.Sheet.Abilities.DEX != 16 || created.Sheet.Abilities.INT != 16 {
		t.Fatalf("imported abilities = %+v", created.Sheet.Abilities)
	}
	if len(created.Sheet.Proficiencies.Saves) != 2 {
		t.Fatalf("imported saves = %+v", created.Sheet.Proficiencies.Saves)
	}

	// The created pc reads structured through the sheet route, and its
	// projection row landed with the import.
	get := hit(t, s, http.MethodGet, base+"/characters/"+created.Entity.ID+"/sheet", "", dm)
	if !strings.Contains(get.Body.String(), `"structured":true`) {
		t.Fatalf("created pc reads unstructured: %s", get.Body)
	}

	// Garbage refuses with a reason, not a guess.
	if rec = hit(t, s, http.MethodPost, base+"/characters/import",
		`{"format":"roll20","data":{"nope":true}}`, dm); rec.Code == http.StatusOK {
		t.Fatal("a bag with no attribs must be refused")
	}
	if rec = hit(t, s, http.MethodPost, base+"/characters/import", `{"format":"lotus123","data":{}}`, dm); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown format: status %d", rec.Code)
	}

	// XML travels wrapped as a string — the FC5 door.
	xmlBody := `{"format":"fc5","data":"<compendium><character><name>Test XML</name><race>Elf</race><ac>15</ac></character></compendium>"}`
	if rec = hit(t, s, http.MethodPost, base+"/characters/import", xmlBody, dm); rec.Code != http.StatusCreated {
		t.Fatalf("fc5 import: status %d, body %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"race":"Elf"`) {
		t.Fatalf("fc5 import body: %s", rec.Body)
	}
}

// The projection table tracks the payloads (MAD-418): every entity write
// on a pc refreshes its row — the store hooks the create and update paths,
// so a legacy payload patched by hand projects too — and a sheet write
// turns the row structured the same moment the sheet lands.
func TestSheetProjectionTracksWrites(t *testing.T) {
	s, campaigns, _, _ := newCampaignServer(t)
	f := buildFixture(t, s)
	dm := dmSession(t, s)
	base := "/api/campaigns/" + f.campaignID

	// The fixture pc, given the party block's legacy shape by hand.
	if rec := hit(t, s, http.MethodPatch, base+"/entities/"+f.pcID,
		`{"payload":{"level":5,"class":"wizard"}}`, dm); rec.Code != http.StatusOK {
		t.Fatalf("patch: %d", rec.Code)
	}
	var level, structured int
	if err := campaigns.DB().QueryRow(
		`SELECT level, structured FROM pc_sheet_projection WHERE entity_id = ?`, f.pcID,
	).Scan(&level, &structured); err != nil {
		t.Fatalf("projection row after the entity write: %v", err)
	}
	if level != 5 || structured != 0 {
		t.Fatalf("legacy projection = level:%d structured:%d, want 5/0", level, structured)
	}

	if rec := hit(t, s, http.MethodPut, base+"/characters/"+f.pcID+"/sheet",
		`{"classes":[{"class":"wizard","level":6}],"ac":14,"max_hp":36}`, dm); rec.Code != http.StatusOK {
		t.Fatalf("put sheet: %d %s", rec.Code, rec.Body)
	}
	var lvl2, structured2 int
	if err := campaigns.DB().QueryRow(
		`SELECT level, structured FROM pc_sheet_projection WHERE entity_id = ?`, f.pcID,
	).Scan(&lvl2, &structured2); err != nil {
		t.Fatal(err)
	}
	if lvl2 != 6 || structured2 != 1 {
		t.Fatalf("projection after sheet write = level:%d structured:%d", lvl2, structured2)
	}
}
