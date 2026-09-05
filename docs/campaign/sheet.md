# The typed character sheet: 5e PCs as first-class data

A player character in Grimoire is a typed entity node (`pc`) whose payload
now carries a **typed sheet** — the character's *definition* as data:
abilities, proficiencies, class and subclass levels, spellcasting, features,
inventory, currency. Stage 1 of the mechanical layer ([MAD-418][]); the
storage decision is [ADR 15](../decisions.md#adr-15--the-typed-character-sheet-payload-block-plus-a-narrow-derived-projection).

[MAD-418]: https://multica.collinpendleton.com

## The one split

> **The sheet is the definition; the ledger is the state.**

Levels, known spells, proficiencies, max HP — slow-changing *configuration*,
edited deliberately and rarely. Current HP, spent slots, burned ammo —
fast-changing *state*, an append-only event log, never edited (the resource
ledger, MAD-419, builds that next). The sheet has no field for "what is
left" of anything, on purpose: a fresh sheet does not silently declare a
character at full slots, and the party block's remaining-resource keys stay
whatever the campaign set them to.

## Where it lives

The sheet is a payload block under the `"sheet"` key of the pc entity — the
place-block pattern. Everything else a pc payload carries (the party block's
legacy top-level keys, a DM's own notes) survives a sheet write untouched.

`PartyBlockOf` reads **sheet-first**: a pc with a sheet serves its party
view from the sheet (total level, first class, AC, max HP, resistances,
inventory names, notes); a pc without one reads the legacy keys exactly as
before. Nothing about a campaign that has written no sheets changes.

A narrow projection table (`pc_sheet_projection`, migration `0029`) carries
the queryable numbers — level, a classes label (`"fighter 8/wizard 2"`),
max hp, ac, and `structured`. Every pc entity write refreshes its row and
server start re-derives the table whole, so it is a cache that never
outlives a boot. Legacy payloads project from their existing keys;
`structured: 0` is the visible **unstructured sheet** marker.

## The model

The wire JSON is the contract — snake_case keys, lowercase squashed ability
and skill names, slot levels keyed `"1"`..`"9"`:

```json
{
  "version": 1,
  "race": "mountain dwarf",
  "background": "soldier",
  "alignment": "lawful good",
  "xp": 34000,
  "abilities": {"str": 17, "dex": 10, "con": 16, "int": 8, "wis": 13, "cha": 12},
  "ac": 18,
  "max_hp": 49,
  "speeds": {"walk": 25},
  "proficiencies": {
    "saves": ["str", "con"],
    "skills": ["athletics", "intimidation"],
    "tools": ["smith's tools"],
    "languages": ["common", "dwarvish"],
    "armor": ["light", "medium", "shields"],
    "weapons": ["simple", "martial"]
  },
  "classes": [
    {"class": "fighter", "subclass": "champion", "level": 8},
    {"class": "wizard", "subclass": "war magic", "level": 2}
  ],
  "spellcasting": {
    "ability": "int",
    "dc": 13,
    "attack_bonus": 4,
    "slots": {"1": 3},
    "known": [{"name": "fire bolt"}],
    "prepared": [{"name": "shield"}]
  },
  "resistances": ["poison"],
  "immunities": [],
  "vulnerabilities": [],
  "features": [{"name": "Second Wind", "ref": "", "note": ""}],
  "traits": [{"name": "Dwarven Resilience"}],
  "inventory": [
    {"name": "plate armor", "qty": 1, "equipped": true},
    {"name": "potion of healing", "qty": 3},
    {"name": "flame tongue warhammer", "qty": 1, "equipped": true, "attuned": true}
  ],
  "attunement_max": 3,
  "currency": {"cp": 0, "sp": 12, "ep": 0, "gp": 55, "pp": 0},
  "notes": "the shield of the party"
}
```

Multiclassing is a list, not a field. Slots are maxima. `ref` fields point
into the indexed SRD (the reader-node key form, e.g. `"spells/0003/0042"`)
where a reference exists — resolution is read-time work, and a homebrew
name with no ref is legal. Every field is optional; the zero sheet is the
pc nobody has written numbers for, and it validates.

Normalization is part of the contract: writes are stored through the typed
struct (strings trimmed, blanks pruned, zero counts dropped, version
stamped), so `PUT → GET → PUT` is a no-op and the round trip is
byte-stable. An empty sheet removes the block rather than storing a husk.

## Validation

Writes validate before storing, and every problem names its field and the
window or vocabulary it fell outside of — `abilities.str`, "ability score
42 is outside 1-30"; `resistances[0]`, `"spooky" is not a damage type; the
game's thirteen are …`. The vocabularies are the game's own sets, declared
once in `internal/homebrew` (the homebrew linter's, MAD-385's) and reused:
the same grammar a statblock holds, mirrored for the player's side of the
screen. The whole vocabulary contract is pinned by a golden file
(`internal/sheet/testdata/vocab_golden.json`) so a change to any set or
window is a deliberate diff.

Windows: abilities 1–30, AC 1–40, max HP 1–999, speeds 1–120 ft, class
levels 1–20 each and in total, save DC 5–35, attack bonus −10..+20, slots
0–12 per level, attunement capped at `attunement_max` (default 3).

## The API

Under `/api/campaigns/{id}`:

| Route | Who | What |
|---|---|---|
| `GET /characters/{eid}/sheet` | DM, or the player bound to `eid` | The sheet, `structured`, and problems. Unstructured pcs read `"structured": false` and no `sheet` — the marker, never an invention |
| `PUT /characters/{eid}/sheet` | DM | Replace the sheet. 400 with every problem named on invalid input; 200 with the stored (normalized) sheet |
| `POST /characters/import` | DM | Create a pc from an export: `{"format": "auto\|grimoire\|roll20\|fc5", "data": <export>, "name": "…"}` — name only when the export carries none |

The player read is the one deliberate widening of the portal's surface
(ADR 15): a character-scoped player sees exactly their own sheet through
the player view — no other pc, no other payload key, no facts. Entity
payloads stay dropped at player scope everywhere else.

## Import

Import beats build for v1 — almost nobody hand-types a level-8 sheet when
their table's app can export one.

- **Grimoire sheet JSON** — the native format; the identity import. A GET
  body re-imports unchanged (the `sheet` field of the read envelope is
  accepted too).
- **Roll20 OGL export** — the character vault / API export: a character
  object with an `attribs` bag. Best-effort by declaration: fields map only
  when the export verifiably carries them (abilities, race, background,
  alignment, classes, AC, HP, speed, proficient saves and skills,
  spellcasting DC/attack/slots, spell rows, inventory rows, currency), and
  everything the exporter's shape prevented mapping is named in the report.
- **Fight Club 5 / Game Master 5 XML** — the `<compendium><character>`
  family. Core fields exact, edges generous; a compendium of many
  characters is refused with a count, not a guess.
- **`auto`** sniffs the payload when the caller does not know.

The importer is exercised against real exports, vendored verbatim under
`internal/sheet/testdata/imports/` — two Roll20 character JSONs pulled
unmodified from a public collection. The FC5 fixture is schema-shaped and
labeled as such in the file (the app is iOS-only; no canonical public
export exists). The report always tells the truth: `mapped`, `unmapped`,
`notes`.

## The 2014 rule, deliberately

5e 2014 SRD, the same edition every encounter surface pins. No 2024 rules,
no other systems, no speculative abstraction — "eventually, maybe" is out
of scope, the same line the roadmap drew.
