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
