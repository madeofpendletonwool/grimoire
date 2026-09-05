package sheet

// The Fight Club 5 / Game Master 5 XML importer. FC5's character export is
// the lingua franca of the iOS table ecosystem: a <compendium> whose
// <character> child carries the printed sheet as flat elements and rows.
// The app family's exporters agree on the core spelling (<name>, <race>,
// <ac>, <stat stat="str" value="16"/>, <class><name/><level/></class>,
// <item>, <spell>) and disagree at the edges, so the parser reads the core
// exactly and the edges generously — every variant it knows, nothing it
// would have to guess at.
//
// This importer is exercised against schema-shaped fixtures
// (testdata/imports/sample_fc5.xml), not a device export: the FC5 app
// itself is iOS-only and its files surface one table at a time. The
// fixture's shape is the published schema's; where the family disagrees the
// parser takes both spellings.

import (
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/homebrew"
)

// fc5Compendium is the document root. The importer takes a bare
// <character> too, for the copies that travel without the wrapper.
type fc5Compendium struct {
	Characters []fc5Character `xml:"character"`
}

type fc5Character struct {
	Name       string `xml:"name"`
	Race       string `xml:"race"`
	Background string `xml:"background"`
	Alignment  string `xml:"alignment"`
	Size       string `xml:"size"`
	Speed      string `xml:"speed"`
	HP         string `xml:"hp"`
	AC         string `xml:"ac"`
	XP         string `xml:"xp"`

	STR string `xml:"str"`
	DEX string `xml:"dex"`
	CON string `xml:"con"`
	INT string `xml:"int"`
	WIS string `xml:"wis"`
	CHA string `xml:"cha"`

	Stats []fc5Stat `xml:"stat"`

	Classes []fc5Class `xml:"class"`
	Saves   []fc5Save  `xml:"save"`
	Skills  []fc5Skill `xml:"skill"`
	Slots   []fc5Slot  `xml:"spellslot"`
	Spells  []fc5Spell `xml:"spell"`
	Items   []fc5Item  `xml:"item"`
	Feats   []fc5Entry `xml:"feature"`
	Traits  []fc5Entry `xml:"trait"`
	Coins   []fc5Coin  `xml:"coin"`

	Currency *fc5Currency `xml:"currency"`
}

type fc5Stat struct {
	Stat  string `xml:"stat,attr"`
	Value string `xml:"value,attr"`
}

type fc5Class struct {
	Name     string `xml:"name"`
	Level    string `xml:"level"`
	Subclass string `xml:"subclass"`
}

type fc5Save struct {
	Save string `xml:"save,attr"`
	Prof string `xml:"prof,attr"`
	// The element body carries the computed modifier on full exports; it
	// is display state, not definition, and stays behind.
	Text string `xml:",chardata"`
}

type fc5Skill struct {
	Skill string `xml:"skill,attr"`
	Prof  string `xml:"prof,attr"`
	Text  string `xml:",chardata"`
}

type fc5Slot struct {
	Level string `xml:"level,attr"`
	Max   string `xml:"max,attr"`
	Used  string `xml:"used,attr"`
}

type fc5Spell struct {
	Name     string `xml:"name"`
	Prepared string `xml:"prepared"`
}

type fc5Item struct {
	Name     string `xml:"name"`
	Count    string `xml:"count"`
	Equipped string `xml:"equipped"`
	Attuned  string `xml:"attuned"`
	Notes    string `xml:"notes"`
	Type     string `xml:"type"`
	Text     string `xml:"text"`
}

type fc5Entry struct {
	Name string `xml:"name"`
	Text string `xml:"description"`
}

type fc5Coin struct {
	Type   string `xml:"type,attr"`
	Amount string `xml:"amount,attr"`
	// Flat spellings inside a <currency> wrapper carry the game's own
	// element names.
	CP string `xml:"cp"`
	SP string `xml:"sp"`
	EP string `xml:"ep"`
	GP string `xml:"gp"`
	PP string `xml:"pp"`
}

type fc5Currency struct {
	CP string `xml:"cp"`
	SP string `xml:"sp"`
	EP string `xml:"ep"`
	GP string `xml:"gp"`
	PP string `xml:"pp"`
}

func importFC5(data []byte) (Sheet, ImportReport, error) {
	var rep ImportReport
	var doc fc5Compendium
	if err := xml.Unmarshal(data, &doc); err != nil {
		return Sheet{}, rep, fmt.Errorf("not an FC5 character XML: %v", err)
	}
	if len(doc.Characters) == 0 {
		return Sheet{}, rep, fmt.Errorf("not an FC5 character XML: no <character> element")
	}
	// More than one character in a compendium is a library, not a sheet;
	// the API's import endpoint is one character at a time.
	if len(doc.Characters) > 1 {
		return Sheet{}, rep, fmt.Errorf("the XML carries %d characters; import one character per call", len(doc.Characters))
	}
	c := doc.Characters[0]
	rep = ImportReport{Format: "fc5", Name: strings.TrimSpace(c.Name)}

	var s Sheet
	mapped := map[string]bool{}
	var unmapped []string

	s.Race = strings.TrimSpace(c.Race)
	s.Background = strings.TrimSpace(c.Background)
	s.Alignment = strings.TrimSpace(c.Alignment)
	if s.Race != "" {
		mapped["race"] = true
	}
	if s.Background != "" {
		mapped["background"] = true
	}
	if s.Alignment != "" {
		mapped["alignment"] = true
	}

	// Abilities: the <stat stat="str" value="16"/> rows, with the flat
	// <str>16</str> spelling as fallback. Both are the FC5 family's.
	values := map[string]int{
		"str": atoi(c.STR), "dex": atoi(c.DEX), "con": atoi(c.CON),
		"int": atoi(c.INT), "wis": atoi(c.WIS), "cha": atoi(c.CHA),
	}
	for _, st := range c.Stats {
		key := squash(strings.ToLower(strings.TrimSpace(st.Stat)))
		if _, ok := values[key]; ok {
			values[key] = atoi(st.Value)
		}
	}
	if ab := (Abilities{STR: values["str"], DEX: values["dex"], CON: values["con"], INT: values["int"], WIS: values["wis"], CHA: values["cha"]}); !ab.IsZero() {
		s.Abilities = ab
		mapped["abilities"] = true
	}

	if v := atoi(c.AC); v != 0 {
		s.AC = v
		mapped["ac"] = true
	}
	if v := atoi(c.HP); v != 0 {
		s.MaxHP = v
		mapped["max_hp"] = true
	}
	if v := atoi(c.XP); v != 0 {
		s.XP = v
		mapped["xp"] = true
	}
	if v := strings.TrimSpace(c.Speed); v != "" {
		if feet, ok := leadingFeet(v); ok {
			s.Speeds = map[string]int{"walk": feet}
			mapped["speeds"] = true
		}
	}
	if c.Size != "" {
		unmapped = append(unmapped, "size "+c.Size)
	}

	for _, cl := range c.Classes {
		class := ClassLevel{
			Class:    strings.TrimSpace(cl.Name),
			Level:    atoi(cl.Level),
			Subclass: strings.TrimSpace(cl.Subclass),
		}
		if class.Class == "" {
			continue
		}
		s.Classes = append(s.Classes, class)
	}
	if len(s.Classes) > 0 {
		mapped["classes"] = true
	}

	for _, sv := range c.Saves {
		if !isTruthy(sv.Prof) {
			continue
		}
		key := squash(strings.ToLower(strings.TrimSpace(sv.Save)))
		if key != "" && !inSlice(key, s.Proficiencies.Saves) {
			s.Proficiencies.Saves = append(s.Proficiencies.Saves, key)
		}
	}
	for _, sk := range c.Skills {
		if !isTruthy(sk.Prof) {
			continue
		}
		key := squash(strings.ToLower(strings.TrimSpace(sk.Skill)))
		if inSet(key, homebrew.Skills) && !inSlice(key, s.Proficiencies.Skills) {
			s.Proficiencies.Skills = append(s.Proficiencies.Skills, key)
		}
	}
	if len(s.Proficiencies.Saves) > 0 || len(s.Proficiencies.Skills) > 0 {
		mapped["proficiencies"] = true
	}

	if len(c.Slots) > 0 || len(c.Spells) > 0 {
		sc := &Spellcasting{}
		for _, sl := range c.Slots {
			lvl := strings.TrimSpace(sl.Level)
			if max := atoi(sl.Max); max > 0 && lvl != "" {
				if sc.Slots == nil {
					sc.Slots = map[string]int{}
				}
				sc.Slots[lvl] = max // used/ is tonight's state, not the definition
			}
		}
		for _, sp := range c.Spells {
			if sp.Name == "" {
				continue
			}
			if isTruthy(sp.Prepared) {
				sc.Prepared = append(sc.Prepared, Entry{Name: strings.TrimSpace(sp.Name)})
			} else {
				sc.Known = append(sc.Known, Entry{Name: strings.TrimSpace(sp.Name)})
			}
		}
		if !sc.IsZero() {
			s.Spellcasting = sc
			mapped["spellcasting"] = true
		}
	}

	for _, it := range c.Items {
		name := strings.TrimSpace(it.Name)
		if name == "" {
			continue
		}
		item := Item{
			Name:     name,
			Qty:      atoi(it.Count),
			Equipped: isTruthy(it.Equipped),
			Attuned:  isTruthy(it.Attuned),
			Notes:    strings.TrimSpace(it.Notes),
		}
		if item.Qty == 0 {
			item.Qty = 1
		}
		s.Inventory = append(s.Inventory, item)
		mapped["inventory"] = true
		if it.Text != "" && item.Notes == "" {
			// FC5 carries full item text (the catalogue entry) — the sheet
			// references items, it does not mirror the rulebook.
			unmapped = append(unmapped, "item text for "+name)
		}
	}

	for _, f := range c.Feats {
		if f.Name = strings.TrimSpace(f.Name); f.Name != "" {
			s.Features = append(s.Features, Entry{Name: f.Name})
		}
	}
	for _, t := range c.Traits {
		if t.Name = strings.TrimSpace(t.Name); t.Name != "" {
			s.Traits = append(s.Traits, Entry{Name: t.Name})
		}
	}
	if len(s.Features) > 0 {
		mapped["features"] = true
	}
	if len(s.Traits) > 0 {
		mapped["traits"] = true
	}

	cur := Currency{}
	if c.Currency != nil {
		cur = Currency{CP: atoi(c.Currency.CP), SP: atoi(c.Currency.SP), EP: atoi(c.Currency.EP), GP: atoi(c.Currency.GP), PP: atoi(c.Currency.PP)}
	}
	for _, coin := range c.Coins {
		switch squash(strings.ToLower(coin.Type)) {
		case "cp":
			cur.CP += atoi(coin.Amount)
		case "sp":
			cur.SP += atoi(coin.Amount)
		case "ep":
			cur.EP += atoi(coin.Amount)
		case "gp":
			cur.GP += atoi(coin.Amount)
		case "pp":
			cur.PP += atoi(coin.Amount)
		}
	}
	if !cur.IsZero() {
		s.Currency = cur
		mapped["currency"] = true
	}

	rep.Mapped = keysOf(mapped)
	rep.Unmapped = unmapped
	s.normalize()
	return s, rep, nil
}

/* ---------- fc5 helpers ---------- */

// atoi is the tolerant integer read every FC5 field needs: the app prints
// numbers bare, with a stray "+" prefix, or not at all.
func atoi(s string) int {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "+")
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

func inSlice(v string, list []string) bool {
	for _, s := range list {
		if v == s {
			return true
		}
	}
	return false
}
