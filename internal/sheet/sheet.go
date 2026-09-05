// Package sheet is the typed 5e character sheet (MAD-418, stage 1 of
// MAD-417): a pc's definition as first-class data.
//
// The sheet is the *definition* — abilities, proficiencies, class levels,
// known spells, max HP — the slow-changing configuration a DM (or an
// importer) edits deliberately and rarely. Everything fast-changing —
// current HP, spent slots, burned ammo — is state, and state belongs to the
// resource ledger (MAD-419) as an append-only event log. Neither pretends
// to be the other, and this package has no field for "what is left" of
// anything.
//
// Storage follows the house pattern every payload block set (ADR 15): the
// typed struct serializes under the pc entity payload's "sheet" key,
// spliced by WithSheet so every foreign payload key survives. Reads are
// tolerant — a missing, empty or wrong-shaped block is "no sheet yet", an
// unstructured marker, never an error — while writes are strict: the server
// runs Validate before storing, so a payload that carries a "sheet" key
// holds a sheet that passed validation at the moment it was written.
//
// The package is pure where it can be: model, codec, validation and the
// importers take and return data, never rows. The one exception is
// projection.go, which maintains the narrow SQL projection query surfaces
// read (migration 0029) — derivation, not a second source of truth.
//
// 5e, deliberately: the 2014 SRD, the same edition every encounter surface
// in the app pins. The vocabularies are the game's own sets, declared once
// in internal/homebrew and reused here — a PC sheet and a statblock share a
// grammar (MAD-379), and this package is its mirror image.
package sheet

import (
	"encoding/json"
	"fmt"
	"strings"
)

// PayloadKey is the entities.payload key the sheet is stored under — its own
// key, the place-block pattern, so it cannot collide with the legacy
// party-block keys (internal/campaign/party.go) or anything else a payload
// carries.
const PayloadKey = "sheet"

// Version is the sheet schema version this build writes. It exists so the
// level-up machinery (MAD-424) and future designers can evolve the shape
// without guessing what a stored sheet declares; v1 is the sheet as MAD-418
// shipped it.
const Version = 1

// Sheet is a 5e player character's definition. Every field is optional: the
// zero Sheet is a pc nobody has written numbers for, and it round-trips —
// the party block degraded to exactly that behaviour for years before the
// typed sheet existed.
//
// Field spelling follows the wire JSON: snake_case keys, lowercase ability
// and skill names (the homebrew vocabularies' squashed forms), slot levels
// as string keys "1".."9" because that is how JSON objects key numbers.
type Sheet struct {
	Version         int            `json:"version,omitempty"`
	Race            string         `json:"race,omitempty"`
	Background      string         `json:"background,omitempty"`
	Alignment       string         `json:"alignment,omitempty"`
	XP              int            `json:"xp,omitempty"`
	Abilities       Abilities      `json:"abilities,omitzero"`
	AC              int            `json:"ac,omitempty"`
	MaxHP           int            `json:"max_hp,omitempty"`
	Speeds          map[string]int `json:"speeds,omitempty"`
	Proficiencies   Proficiencies  `json:"proficiencies,omitzero"`
	Classes         []ClassLevel   `json:"classes,omitempty"`
	Spellcasting    *Spellcasting  `json:"spellcasting,omitempty"`
	Resistances     []string       `json:"resistances,omitempty"`
	Immunities      []string       `json:"immunities,omitempty"`
	Vulnerabilities []string       `json:"vulnerabilities,omitempty"`
	Features        []Entry        `json:"features,omitempty"`
	Traits          []Entry        `json:"traits,omitempty"`
	Inventory       []Item         `json:"inventory,omitempty"`
	AttunementMax   int            `json:"attunement_max,omitempty"`
	Currency        Currency       `json:"currency,omitzero"`
	Notes           string         `json:"notes,omitempty"`
}

// Abilities is the six ability scores. Zero means undeclared, not disabled:
// a sheet in progress may carry CON and nothing else, the same partiality
// the party block always tolerated.
type Abilities struct {
	STR int `json:"str,omitempty"`
	DEX int `json:"dex,omitempty"`
	CON int `json:"con,omitempty"`
	INT int `json:"int,omitempty"`
	WIS int `json:"wis,omitempty"`
	CHA int `json:"cha,omitempty"`
}

// IsZero reports an entirely undeclared abilities block.
func (a Abilities) IsZero() bool {
	return a == Abilities{}
}

// Proficiencies is what the character is trained in. Saves and skills are
// checked against the homebrew vocabularies at validation; tools, languages,
// armor and weapons are free strings because the game's own lists there are
// open-ended ("thieves' tools", "half-plate", "hand crossbows").
type Proficiencies struct {
	Saves     []string `json:"saves,omitempty"`
	Skills    []string `json:"skills,omitempty"`
	Tools     []string `json:"tools,omitempty"`
	Languages []string `json:"languages,omitempty"`
	Armor     []string `json:"armor,omitempty"`
	Weapons   []string `json:"weapons,omitempty"`
}

// IsZero reports an entirely undeclared proficiencies block.
func (p Proficiencies) IsZero() bool {
	return len(p.Saves) == 0 && len(p.Skills) == 0 && len(p.Tools) == 0 &&
		len(p.Languages) == 0 && len(p.Armor) == 0 && len(p.Weapons) == 0
}

// ClassLevel is one class's slice of the character. Multiclassing is a list,
// not a field, from day one — the level-up flow (MAD-424) adds to an entry
// or appends a new one, and nothing here assumes a single class.
type ClassLevel struct {
	Class    string `json:"class"`
	Subclass string `json:"subclass,omitempty"`
	Level    int    `json:"level"`
}

// Spellcasting is the character's casting definition: the ability it keys
// off, the DC and attack modifier as declared, the slot table, and the known
// versus prepared lists. Slots are MAXIMA — how many the character has, not
// how many are left; the remainder is ledger state.
type Spellcasting struct {
	Ability     string         `json:"ability,omitempty"`
	DC          int            `json:"dc,omitempty"`
	AttackBonus int            `json:"attack_bonus,omitempty"`
	Slots       map[string]int `json:"slots,omitempty"`
	Known       []Entry        `json:"known,omitempty"`
	Prepared    []Entry        `json:"prepared,omitempty"`
}

// Entry is a named reference into the indexed SRD where one exists: Name is
// what the table calls the thing, Ref is the rules-reference key (the
// reader-node path form, e.g. "spells/0003/0042") and Note carries the
// one-line detail an import could not fit anywhere else. A foreign or
// homebrew name with no Ref is legal — resolution is read-time work.
type Entry struct {
	Name string `json:"name"`
	Ref  string `json:"ref,omitempty"`
	Note string `json:"note,omitempty"`
}

// Item is one inventory line: quantity, whether it is worn or wielded, and
// the attunement flag. Attuned items are counted against AttunementMax at
// validation; the default maximum of three is the 2014 rule.
type Item struct {
	Name     string `json:"name"`
	Ref      string `json:"ref,omitempty"`
	Qty      int    `json:"qty,omitempty"`
	Equipped bool   `json:"equipped,omitempty"`
	Attuned  bool   `json:"attuned,omitempty"`
	Notes    string `json:"notes,omitempty"`
}

// Currency is the five-coin purse, each counted in its own denomination.
type Currency struct {
	CP int `json:"cp,omitempty"`
	SP int `json:"sp,omitempty"`
	EP int `json:"ep,omitempty"`
	GP int `json:"gp,omitempty"`
	PP int `json:"pp,omitempty"`
}

// IsZero reports an empty purse.
func (c Currency) IsZero() bool { return c == Currency{} }

/* ---------- derived reads ---------- */

// TotalLevel sums the class levels. A sheet with no classes declares no
// level — the party view leaves the encounter budgets alone rather than
// guessing zero.
func (s Sheet) TotalLevel() int {
	total := 0
	for _, c := range s.Classes {
		total += c.Level
	}
	return total
}

// AttunementLimit is the attunement ceiling in force: the declared maximum,
// or the 2014 default of three when none is.
func (s Sheet) AttunementLimit() int {
	if s.AttunementMax > 0 {
		return s.AttunementMax
	}
	return 3
}

// AttunedItems lists the inventory lines flagged attuned, in sheet order.
func (s Sheet) AttunedItems() []Item {
	var out []Item
	for _, it := range s.Inventory {
		if it.Attuned {
			out = append(out, it)
		}
	}
	return out
}

// ClassesLabel renders the class list as one human label — "fighter
// 8/wizard 2" — the spelling the projection table and the party surfaces
// use. Single-class sheets render without the slash.
func (s Sheet) ClassesLabel() string {
	if len(s.Classes) == 0 {
		return ""
	}
	parts := make([]string, 0, len(s.Classes))
	for _, c := range s.Classes {
		label := strings.TrimSpace(c.Class)
		if label == "" {
			continue
		}
		if c.Level > 0 {
			label = fmt.Sprintf("%s %d", label, c.Level)
		}
		parts = append(parts, label)
	}
	return strings.Join(parts, "/")
}

/* ---------- the payload codec ---------- */

// FromPayload decodes the sheet from an entity payload. The second return
// reports whether the payload carries a sheet block at all: absent, null,
// empty object and wrong-shaped all yield (zero Sheet, false) — the caller
// decides whether that is "unstructured legacy payload" or "nothing
// declared", both of which are states to surface, not errors to raise. A
// block that exists but cannot decode into the typed struct yields
// (zero, true) with the error: the marker an unparseable sheet is behind.
func FromPayload(payload map[string]any) (Sheet, bool, error) {
	if payload == nil {
		return Sheet{}, false, nil
	}
	raw, ok := payload[PayloadKey]
	if !ok || raw == nil {
		return Sheet{}, false, nil
	}
	// Re-marshal the stored any and decode into the typed struct — the
	// trick every payload block uses, because payloads come back from
	// SQLite as map[string]any and a direct type assertion is brittle
	// against hand-edited JSON.
	blob, err := json.Marshal(raw)
	if err != nil {
		return Sheet{}, true, fmt.Errorf("sheet block is not valid JSON: %w", err)
	}
	var s Sheet
	if err := json.Unmarshal(blob, &s); err != nil {
		return Sheet{}, true, fmt.Errorf("sheet block does not decode as a sheet: %w", err)
	}
	return s, true, nil
}

// WithSheet returns a copy of payload with the sheet replaced under its one
// key, preserving every other key — the party block's legacy top-level keys
// and a DM's own notes above all. nil payload is treated as an empty map.
// The sheet is written through its typed form, so what lands is normalized
// (unknown fields dropped, zero fields omitted) — the same bytes GET
// returns, which is what makes the API round-trip stable.
func WithSheet(payload map[string]any, s Sheet) map[string]any {
	out := make(map[string]any, len(payload)+1)
	for k, v := range payload {
		out[k] = v
	}
	s.normalize()
	if s.isZeroAfterNormalize() {
		delete(out, PayloadKey)
		return out
	}
	blob, err := json.Marshal(s)
	if err != nil {
		return out
	}
	var block map[string]any
	if json.Unmarshal(blob, &block) == nil {
		out[PayloadKey] = block
	}
	return out
}

// normalize cleans the sheet in place: strings trimmed, lists pruned of
// blanks, the version stamped, zero counts dropped from the slot and speed
// tables. Order is preserved everywhere it was authored — class order is
// the order the levels were taken, and inventory order is the sheet's own.
func (s *Sheet) normalize() {
	s.Version = Version
	s.Race = clean(s.Race)
	s.Background = clean(s.Background)
	s.Alignment = clean(s.Alignment)
	s.Notes = clean(s.Notes)
	s.Resistances = cleanList(s.Resistances)
	s.Immunities = cleanList(s.Immunities)
	s.Vulnerabilities = cleanList(s.Vulnerabilities)
	s.Features = cleanEntries(s.Features)
	s.Traits = cleanEntries(s.Traits)
	classes := make([]ClassLevel, 0, len(s.Classes))
	for _, c := range s.Classes {
		c.Class = clean(c.Class)
		c.Subclass = clean(c.Subclass)
		if c.Class == "" {
			continue
		}
		classes = append(classes, c)
	}
	s.Classes = classes
	if s.Spellcasting != nil {
		sc := s.Spellcasting
		sc.Ability = strings.ToLower(clean(sc.Ability))
		sc.Known = cleanEntries(sc.Known)
		sc.Prepared = cleanEntries(sc.Prepared)
		for lvl, n := range sc.Slots {
			if n <= 0 {
				delete(sc.Slots, lvl)
			}
		}
		if len(sc.Slots) == 0 {
			sc.Slots = nil
		}
		if sc.IsZero() {
			s.Spellcasting = nil
		}
	}
	for lvl, n := range s.Speeds {
		if n <= 0 {
			delete(s.Speeds, lvl)
		}
	}
	if len(s.Speeds) == 0 {
		s.Speeds = nil
	}
	p := s.Proficiencies
	p.Saves = cleanLowerList(p.Saves)
	p.Skills = cleanSquashedList(p.Skills)
	p.Tools = cleanList(p.Tools)
	p.Languages = cleanList(p.Languages)
	p.Armor = cleanList(p.Armor)
	p.Weapons = cleanList(p.Weapons)
	s.Proficiencies = p
	items := make([]Item, 0, len(s.Inventory))
	for _, it := range s.Inventory {
		it.Name = clean(it.Name)
		it.Notes = clean(it.Notes)
		if it.Name == "" {
			continue
		}
		if it.Qty < 0 {
			it.Qty = 0
		}
		items = append(items, it)
	}
	s.Inventory = items
	if s.AttunementMax < 0 {
		s.AttunementMax = 0
	}
	if s.XP < 0 {
		s.XP = 0
	}
	if s.AC < 0 {
		s.AC = 0
	}
	if s.MaxHP < 0 {
		s.MaxHP = 0
	}
	c := s.Currency
	if c.CP < 0 {
		c.CP = 0
	}
	if c.SP < 0 {
		c.SP = 0
	}
	if c.EP < 0 {
		c.EP = 0
	}
	if c.GP < 0 {
		c.GP = 0
	}
	if c.PP < 0 {
		c.PP = 0
	}
	s.Currency = c
}

// isZeroAfterNormalize reports the normalized-but-empty sheet: with the
// version stamped a fully-empty sheet is {"version":1}, which carries no
// information and should not be written at all. normalize has already run.
func (s Sheet) isZeroAfterNormalize() bool {
	return s.Race == "" && s.Background == "" && s.Alignment == "" && s.XP == 0 &&
		s.Abilities.IsZero() && s.AC == 0 && s.MaxHP == 0 && len(s.Speeds) == 0 &&
		s.Proficiencies.IsZero() && len(s.Classes) == 0 && s.Spellcasting == nil &&
		len(s.Resistances) == 0 && len(s.Immunities) == 0 && len(s.Vulnerabilities) == 0 &&
		len(s.Features) == 0 && len(s.Traits) == 0 && len(s.Inventory) == 0 &&
		s.AttunementMax == 0 && s.Currency.IsZero() && s.Notes == ""
}

func clean(s string) string { return strings.TrimSpace(s) }

// cleanList trims and drops blanks from a free-string list, keeping order
// and duplicates (a sheet may legitimately carry two "potion of healing"
// lines with different notes).
func cleanList(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = clean(s); s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// cleanLowerList is cleanList lower-cased, for lists the vocabulary holds
// in lower case (saves).
func cleanLowerList(in []string) []string {
	out := cleanList(in)
	if out == nil {
		return nil
	}
	for i, s := range out {
		out[i] = strings.ToLower(s)
	}
	return out
}

// cleanSquashedList is cleanList in the vocabulary's squashed spelling
// ("animal handling" -> "animalhandling"), for skills.
func cleanSquashedList(in []string) []string {
	out := cleanList(in)
	if out == nil {
		return nil
	}
	for i, s := range out {
		out[i] = squash(strings.ToLower(s))
	}
	return out
}

// squash removes the spaces a printed name carries: the form the Skills
// vocabulary and the mirror's lookups key on.
func squash(s string) string {
	return strings.ReplaceAll(s, " ", "")
}

func cleanEntries(in []Entry) []Entry {
	if len(in) == 0 {
		return nil
	}
	out := make([]Entry, 0, len(in))
	for _, e := range in {
		e.Name = clean(e.Name)
		e.Ref = clean(e.Ref)
		e.Note = clean(e.Note)
		if e.Name == "" {
			continue
		}
		out = append(out, e)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// IsZero lets encoding/json drop an empty Spellcasting via omitzero.
func (sc Spellcasting) IsZero() bool {
	return sc.Ability == "" && sc.DC == 0 && sc.AttackBonus == 0 &&
		len(sc.Slots) == 0 && len(sc.Known) == 0 && len(sc.Prepared) == 0
}
