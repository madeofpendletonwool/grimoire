package sheet

// The Roll20 OGL export importer. A Roll20 character exported through the
// character vault or the API is a JSON object whose sheet state is an
// attribute bag: {"char": {"name": ..., "attribs": [{"name": "strength",
// "current": "16"}, ...]}}. The OGL sheet spells its fields in a stable
// vocabulary (strength..charisma, class/base_level, npc_-prefixed mirror
// fields for NPC-mode sheets), and because the bag carries whatever the
// sheet ever wrote, the importer reads generously and reports everything it
// saw but could not use.
//
// Exercised against real exports (testdata/imports) — two sheets pulled
// verbatim from a public collection of Roll20 character JSONs, one PC-mode
// with abilities and save proficiencies, one carrying the spell rows. The
// fixtures are never edited: the importer meets exports as they are.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/homebrew"
)

// roll20Export is the envelope both spellings share: the vault export wraps
// the character in "char", a bare export is the character itself.
type roll20Export struct {
	Name    string         `json:"name"`
	Attribs []roll20Attrib `json:"attribs"`
	Char    *roll20Export  `json:"char"`
}

type roll20Attrib struct {
	Name string `json:"name"`
	// Current and Max are whatever the sheet wrote: the export API types
	// them as strings, but real exports carry bare numbers too (a hand-set
	// sheet value goes out as JSON number). Both spellings read.
	Current json.RawMessage `json:"current"`
	Max     json.RawMessage `json:"max"`
}

// attrString renders an attrib value as the string the bag maps: a quoted
// string unquotes, a bare number stringifies, anything else is empty.
func attrString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(string(raw))
}

func importRoll20(data []byte) (Sheet, ImportReport, error) {
	var rep ImportReport
	var export roll20Export
	if err := json.Unmarshal(data, &export); err != nil {
		return Sheet{}, rep, fmt.Errorf("not a Roll20 export: %v", err)
	}
	char := &export
	if export.Char != nil {
		char = export.Char
	}
	if len(char.Attribs) == 0 {
		return Sheet{}, rep, fmt.Errorf("not a Roll20 export: no attribs bag")
	}
	rep = ImportReport{Format: "roll20", Name: strings.TrimSpace(char.Name)}

	// The bag as a map. Repeated names (repeating_ rows are unique, but a
	// hand-rolled export may repeat a flat) keep their first spelling.
	bag := make(map[string]string, len(char.Attribs))
	for _, a := range char.Attribs {
		key := strings.TrimSpace(a.Name)
		if _, dup := bag[key]; !dup {
			bag[key] = attrString(a.Current)
		}
	}

	var s Sheet
	var notes []string
	mapped := map[string]bool{}

	// Abilities: the OGL sheet's flat names, PC fields first, the npc_
	// mirror as fallback (an NPC-mode sheet only fills the npc_ side).
	abilityKeys := [6][2]string{
		{"strength", "npc_str"}, {"dexterity", "npc_dex"},
		{"constitution", "npc_con"}, {"intelligence", "npc_int"},
		{"wisdom", "npc_wis"}, {"charisma", "npc_cha"},
	}
	var ab [6]int
	anyAbility := false
	for i, keys := range abilityKeys {
		v := firstInt(bag, keys[0], keys[1])
		ab[i] = v
		if v != 0 {
			anyAbility = true
		}
	}
	if anyAbility {
		s.Abilities = Abilities{STR: ab[0], DEX: ab[1], CON: ab[2], INT: ab[3], WIS: ab[4], CHA: ab[5]}
		mapped["abilities"] = true
	}

	// The one-line identity fields.
	if v := first(bag, "race", "npc_type"); v != "" {
		s.Race = v
		mapped["race"] = true
	}
	if v := first(bag, "background"); v != "" {
		s.Background = v
		mapped["background"] = true
	}
	if v := first(bag, "alignment"); v != "" {
		s.Alignment = v
		mapped["alignment"] = true
	}

	// Classes: the primary class block plus the multiclass1..6 blocks.
	var classes []ClassLevel
	if name := bag["class"]; name != "" {
		c := ClassLevel{Class: name, Level: firstInt(bag, "base_level", "level"), Subclass: bag["subclass1"]}
		classes = append(classes, c)
	}
	for i := 1; i <= 6; i++ {
		prefix := fmt.Sprintf("multiclass%d", i)
		name := bag[prefix]
		if name == "" {
			continue
		}
		classes = append(classes, ClassLevel{
			Class:    name,
			Level:    firstInt(bag, prefix+"_level"),
			Subclass: bag[prefix+"_subclass"],
		})
	}
	if len(classes) > 0 {
		s.Classes = classes
		mapped["classes"] = true
	} else {
		notes = append(notes, "no class levels on the export (an NPC sheet or a blank sheet)")
	}

	if v := firstInt(bag, "ac", "npc_ac"); v != 0 {
		s.AC = v
		mapped["ac"] = true
	}
	if v := firstInt(bag, "hp", "npc_hp", "hp_max"); v != 0 {
		s.MaxHP = v
		mapped["max_hp"] = true
	}
	if v := first(bag, "speed", "npc_speed"); v != "" {
		if feet, ok := leadingFeet(v); ok {
			s.Speeds = map[string]int{"walk": feet}
			mapped["speeds"] = true
		} else {
			notes = append(notes, "speed "+strconv.Quote(v)+" is not a number of feet; not mapped")
		}
	}

	// Proficient saves: {ability}_save_prof carrying anything but an empty
	// value or "0" (the sheet prints "@{PB}" when proficient). The OGL
	// family spells the ability both short (str_save_prof) and long
	// (strength_save_prof); both read.
	saveKeys := [6][2]string{
		{"str", "strength"}, {"dex", "dexterity"}, {"con", "constitution"},
		{"int", "intelligence"}, {"wis", "wisdom"}, {"cha", "charisma"},
	}
	for _, keys := range saveKeys {
		prof := first(bag, keys[0]+"_save_prof", keys[1]+"_saving_throw_prof", keys[1]+"_save_prof")
		if prof != "" && prof != "0" {
			s.Proficiencies.Saves = append(s.Proficiencies.Saves, keys[0])
		}
	}
	if len(s.Proficiencies.Saves) > 0 {
		mapped["proficiencies"] = true
	}

	// Skill rows: repeating_skill_{rowid}_name with a sibling _prof. The
	// row names print in title case ("Animal Handling"); the vocabulary is
	// squashed lower case.
	for name, value := range bag {
		if !strings.HasPrefix(name, "repeating_skill_") || !strings.HasSuffix(name, "_name") {
			continue
		}
		if value == "" {
			continue
		}
		row := strings.TrimSuffix(name, "_name")
		prof := bag[row+"_prof"]
		if prof == "" || prof == "0" {
			continue // not proficient, and the sheet records no half-profs
		}
		skill := squash(strings.ToLower(value))
		if inSet(skill, homebrew.Skills) {
			s.Proficiencies.Skills = append(s.Proficiencies.Skills, skill)
			mapped["proficiencies"] = true
		}
	}
	s.Proficiencies.Skills = dedupeSorted(s.Proficiencies.Skills)

	// Spellcasting: ability, DC, to-hit, slot totals, and the spell rows.
	sc := &Spellcasting{}
	if v := bag["spellcasting_ability"]; v != "" {
		sc.Ability = squash(strings.ToLower(v))
		mapped["spellcasting"] = true
	}
	if v := firstInt(bag, "spell_save_dc"); v != 0 {
		sc.DC = v
		mapped["spellcasting"] = true
	}
	if v := firstInt(bag, "spell_to_hit", "spell_attack_bonus"); v != 0 {
		sc.AttackBonus = v
		mapped["spellcasting"] = true
	}
	for lvl := 1; lvl <= 9; lvl++ {
		key := fmt.Sprintf("slot_total_%d", lvl)
		if v := firstInt(bag, key, fmt.Sprintf("spells_slot_total_%d", lvl), fmt.Sprintf("spell_slot_total_%d", lvl)); v > 0 {
			if sc.Slots == nil {
				sc.Slots = map[string]int{}
			}
			sc.Slots[strconv.Itoa(lvl)] = v
			mapped["spellcasting"] = true
		}
	}
	for name, value := range bag {
		if !strings.HasPrefix(name, "repeating_spell-") || !strings.HasSuffix(name, "_name") {
			continue
		}
		if value == "" {
			continue
		}
		row := strings.TrimSuffix(name, "_name")
		entry := Entry{Name: value}
		if strings.HasPrefix(name, "repeating_spell-l0") {
			sc.Known = append(sc.Known, entry) // cantrips are always known
		} else if isTruthy(bag[row+"_prepared"]) {
			sc.Prepared = append(sc.Prepared, entry)
		} else {
			sc.Known = append(sc.Known, entry)
		}
		mapped["spellcasting"] = true
	}
	if !sc.IsZero() {
		s.Spellcasting = sc
	}

	// Inventory rows: repeating_item_{rowid}_itemname with _itemcount.
	for name, value := range bag {
		if !strings.HasPrefix(name, "repeating_item_") || !strings.HasSuffix(name, "_itemname") {
			continue
		}
		if value == "" {
			continue
		}
		row := strings.TrimSuffix(name, "_itemname")
		item := Item{Name: value, Qty: firstInt(bag, row+"_itemcount")}
		if item.Qty == 0 {
			item.Qty = 1
		}
		s.Inventory = append(s.Inventory, item)
		mapped["inventory"] = true
	}

	// Currency: five flat fields.
	cur := Currency{
		CP: firstInt(bag, "cp"), SP: firstInt(bag, "sp"), EP: firstInt(bag, "ep"),
		GP: firstInt(bag, "gp"), PP: firstInt(bag, "pp"),
	}
	if !cur.IsZero() {
		s.Currency = cur
		mapped["currency"] = true
	}

	// What the export carries that no mapping consumes. Only the notable
	// flats are named — repeating_ rows are sheet plumbing.
	if _, ok := bag["bio"]; ok {
		notes = append(notes, "bio text is free prose; the sheet keeps no biography field (entities.summary is its home)")
	}
	if _, ok := bag["gmnotes"]; ok {
		notes = append(notes, "gm notes stayed behind: DM material does not ride a sheet import")
	}

	rep.Mapped = keysOf(mapped)
	rep.Notes = notes
	s.normalize()
	return s, rep, nil
}

/* ---------- roll20 helpers ---------- */

func first(bag map[string]string, keys ...string) string {
	for _, k := range keys {
		if v, ok := bag[k]; ok && v != "" {
			return v
		}
	}
	return ""
}

func firstInt(bag map[string]string, keys ...string) int {
	v := first(bag, keys...)
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		// "@{strength_mod + 3}" and friends are formulas, not numbers.
		return 0
	}
	return n
}

// leadingFeet reads "30 ft." / "30 ft., fly 60 ft." — the OGL speed print —
// as the walking number.
func leadingFeet(v string) (int, bool) {
	fields := strings.FieldsFunc(v, func(r rune) bool { return r == ' ' || r == ',' || r == ';' })
	if len(fields) == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, false
	}
	return n, true
}

func isTruthy(v string) bool {
	v = strings.ToLower(strings.TrimSpace(v))
	switch v {
	case "", "0", "false", "no", "@{0}":
		return false
	}
	return true
}

// dedupeSorted returns the list without repeats in its first-seen order —
// the OGL bag can carry a skill row twice when a sheet was rebuilt.
func dedupeSorted(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
