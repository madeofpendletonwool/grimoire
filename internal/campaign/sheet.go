package campaign

// The sheet block (MAD-418, stage 1 of MAD-417): the typed 5e character
// sheet as a payload block, the place-block pattern applied to pcs. The
// model, the validation, the importers and the SQL projection live in
// internal/sheet; this file is the campaign side of the seam — the
// Entity-shaped codec, and the one read that had to change.
//
// That read is PartyBlockOf. The party block owned the pc payload's top
// level from MAD-378 ("the block is the payload"), and everything that asks
// what the party is — encounter budgets, the continuity engine, the loot
// curves — goes through it. The typed sheet is now the definition, so the
// party block becomes a view over whichever source a payload carries: the
// sheet when there is one, the legacy top-level keys when there are only
// those. A campaign that has written no sheets reads exactly as it did
// before this file existed — the same fields, the same tolerance, the same
// problems — and a campaign that has writes the numbers once, in one place.
//
// The mapping is deliberately partial. The sheet is the definition; the
// party block's remaining-slots keys (resources.spell_slots, hit_dice) are
// state, and state is the resource ledger's (MAD-419) — a fresh sheet does
// not silently declare a character at full slots. Conditions and save
// bonuses stay unmapped for the same reason: one is tonight's state, the
// other is a derivation (ability + proficiency) the ledger and the dice
// surfaces will compute from the definition rather than store twice.

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/madeofpendletonwool/grimoire/internal/sheet"
)

// SheetOf decodes a pc entity's typed sheet. The bool reports whether the
// payload carries a sheet block at all — the unstructured marker: a pc with
// legacy party keys or nothing gets (zero, false), and the caller surfaces
// that as "unstructured sheet", never as an error.
func SheetOf(e *Entity) (sheet.Sheet, bool, error) {
	if e == nil || e.Kind != KindPC {
		return sheet.Sheet{}, false, nil
	}
	return sheet.FromPayload(e.Payload)
}

// WithSheet returns a copy of the payload with the typed sheet replaced,
// preserving every other key — the party block's legacy top-level keys and
// a DM's own notes above all. The sheet itself is normalized and validated
// by the server path that calls this; the splice is all that happens here.
func WithSheet(payload map[string]any, s sheet.Sheet) map[string]any {
	return sheet.WithSheet(payload, s)
}

// partyBlockOfSheet maps a typed sheet onto the party block's view: the
// numbers the encounter surfaces budget against. See the file comment for
// what deliberately does not map.
func partyBlockOfSheet(e *Entity, s sheet.Sheet) (PartyBlock, []PartyProblem) {
	b := PartyBlock{
		Level: s.TotalLevel(),
		AC:    s.AC,
		MaxHP: s.MaxHP,
		Notes: s.Notes,
		Items: inventoryNames(s),
	}
	if len(s.Resistances) > 0 {
		b.DamageResistances = s.Resistances
	}
	if len(s.Classes) > 0 {
		b.Class = s.Classes[0].Class
		b.Subclass = s.Classes[0].Subclass
	}
	return b, nil
}

// inventoryNames renders the inventory as the party block's item strings —
// "potion of healing x2" — the spelling the loot surfaces already read.
func inventoryNames(s sheet.Sheet) []string {
	if len(s.Inventory) == 0 {
		return nil
	}
	out := make([]string, 0, len(s.Inventory))
	for _, it := range s.Inventory {
		if it.Qty > 1 {
			out = append(out, it.Name+" x"+strconv.Itoa(it.Qty))
			continue
		}
		out = append(out, it.Name)
	}
	return out
}

/* ---------- the player-edit knob (MAD-488) ---------- */

// SheetEditSettingsKey is where the player-edit config lives on the
// campaign's settings payload: settings["sheet_edit"] = {"mechanics": bool}.
// The third typed settings key (after board and leveling); absence is the
// default — the conservative split, with the mechanical definition
// DM-writable and a player editing inventory and notes only.
const SheetEditSettingsKey = "sheet_edit"

// SheetEditConfig is the campaign's choice about what a seated player may
// write on their own sheet. Mechanics widens the player's write from
// inventory and notes to the whole sheet — the DM's explicit trust, off
// until the campaign says otherwise.
type SheetEditConfig struct {
	Mechanics bool `json:"mechanics"`
}

// ParseSheetEditConfig validates a raw settings value. nil (the key
// absent) is the default; anything present must spell the config exactly —
// the house rule: errors, never guesses.
func ParseSheetEditConfig(v any) (SheetEditConfig, error) {
	if v == nil {
		return SheetEditConfig{}, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return SheetEditConfig{}, fmt.Errorf("sheet edit config must be an object")
	}
	raw, ok := m["mechanics"]
	if !ok || raw == nil {
		return SheetEditConfig{}, nil
	}
	b, ok := raw.(bool)
	if !ok {
		return SheetEditConfig{}, fmt.Errorf("sheet edit mechanics must be a boolean")
	}
	return SheetEditConfig{Mechanics: b}, nil
}

// SheetEditConfigOf reads the config off a settings value, failing to the
// default. Reads never 500 on a hand-mangled payload.
func SheetEditConfigOf(v any) SheetEditConfig {
	cfg, err := ParseSheetEditConfig(v)
	if err != nil {
		return SheetEditConfig{}
	}
	return cfg
}

// SettingsValue is the raw JSON value to store for a validated config —
// the exact shape ParseSheetEditConfig reads back.
func (c SheetEditConfig) SettingsValue() map[string]any {
	return map[string]any{"mechanics": c.Mechanics}
}

/* ---------- the provenance of the last sheet write ---------- */

// SheetMetaKey is the pc payload key carrying the provenance of the last
// sheet write — a sibling of the sheet block (the place-block pattern),
// never inside it: the typed sheet is 5e data, and who edited it is
// campaign housekeeping. The player view's CharacterSheet read decodes
// exactly the "sheet" key, so this block stays invisible to player
// surfaces; the DM's read surfaces it so "who changed this" is answerable
// without an audit table.
const SheetMetaKey = "sheet_meta"

// SheetEditMeta records who wrote a pc's sheet last and when: the user id
// (the durable key), the username as the table knows them, and the
// perspective the write came through — dm or the bound player.
type SheetEditMeta struct {
	By   string `json:"by"`
	Name string `json:"name,omitempty"`
	Role string `json:"role"` // dm | player
	At   string `json:"at"`
}

// SheetEditMetaOf reads the last-write provenance off a payload,
// tolerantly: absent, null or mangled reads as nil, never an error.
func SheetEditMetaOf(payload map[string]any) *SheetEditMeta {
	if payload == nil {
		return nil
	}
	raw, ok := payload[SheetMetaKey]
	if !ok || raw == nil {
		return nil
	}
	blob, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var m SheetEditMeta
	if err := json.Unmarshal(blob, &m); err != nil || m.By == "" {
		return nil
	}
	return &m
}
