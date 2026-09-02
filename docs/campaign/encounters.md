# Encounters: one record, and the party block

A planned encounter used to live in two unrelated places. The builder saved
`encounters` rows — owner-scoped, a party of bare levels, a roster, notes.
Session prep wrote `encounter` session events whose payload carried a thin
roster, and that is what the continuity engine read. The builder could
produce an encounter the engine could not see; the engine could flag one the
builder could not open. `dungeon_rooms.encounter_id` has carried no foreign
key since MAD-373 for exactly this reason.

MAD-378 makes it one record. Migration `0026` grows `encounters` into the
long form of a planned fight:

- `campaign_id` — soft, empty keeps every existing owner-scoped encounter
  working untouched (the same precedent `conversations.campaign_id` set in
  the baseline).
- `session_event_id` — the `encounter` session event this record is the long
  form of, when it has one.
- `scene_id` — the spine scene whose roster it is (MAD-360).
- `objective` and `terrain` — reserved here so the objectives issue needs no
  migration of its own; nothing writes them yet.
- `status` — `planned | run | discarded`, with a CHECK. A discarded
  encounter is not planned and the engine skips it.

**The session event stays the canonical marker** that an encounter is
planned for a session — the event log is the only writer (ADR 9). What
changes is that the event can now name the record that holds the full
design, instead of duplicating a thin roster.

The table is rebuilt rather than ALTERed, because `internal/encounter` still
declares its own DDL and the schema-compat test builds databases in both
orders. A rebuild that copies only the old columns is correct under both,
and a migration test round-trips every pre-existing encounter unchanged.

## The engine reads both, findings do not move

`canon.LoadSnapshot` folds campaign-scoped `encounters` rows into the same
list the session events produced. A row that names a session event *is* that
event's long form: it replaces the event's ref in place, contributing its
fuller roster, and the finding keeps anchoring to the event — so a campaign
that adopts the record does not see its findings move. A row naming no event
stands on its own and its findings point at the record. Either way,
`stat_block_unresolved` and `party_level_drift` fire on a builder-saved
roster exactly as they always fired on an event payload.

## The party block

Designing against a campaign needs to know who is at the table, and a bare
`"level"` key — all the engine ever read off a pc — is too thin to design
against. The party block formalises what a pc entity's payload may declare,
the same approach the place block (MAD-370) and the npc agent block (MAD-313)
take: a typed view over the existing `entities.payload` JSON, no new table.
Unlike those two it is **not** nested under its own key — `"level"` has been
read from the payload's top level since the campaign core shipped, and moving
it would silently re-band every planned encounter in every existing campaign.
The block is the payload:

    {"level": 5, "class": "wizard", "subclass": "evocation",
     "ac": 15, "max_hp": 32, "passive_perception": 14,
     "saves": {"str": -1, "dex": 3, "con": 2, "int": 7, "wis": 4, "cha": 1},
     "resources": {"spell_slots": {"1": 4, "2": 3, "3": 2}, "hit_dice": 5},
     "damage_resistances": [], "conditions": [],
     "items": ["<entity-id>", ...], "notes": ""}

Every key optional. A campaign that declares nothing degrades to exactly
today's behaviour — the rule the awareness layer already follows.

Three properties carry the whole design:

- **`campaign.PartySnapshot(...).Levels()` is bit-for-bit what
  `canon.LoadSnapshot` used to compute for `Snapshot.Party`** — live pcs, a
  level only when it parses to 1..20, name order. The engine now asks the
  party table instead of doing it by hand; a test pins the two together
  against the pre-block loader, copied verbatim. No finding moves.
- **A malformed block is reported, never dropped and never fatal.** Each key
  is read on its own — a payload whose `"ac"` is the string `"high"` still
  yields the level, the class and the saves — and the unreadable keys come
  back as `problems` naming the entity, the field and what is wrong. A
  hand-edited payload never fails the snapshot load.
- **Party reads are DM-only.** A party sheet names what the characters carry
  and what they can still cast; a player-scope request is a 403, not a
  filtered list (ADR 6 applied to a new surface).

## The builder reads a campaign when it has one

`POST /api/encounter/budget` and `/api/encounter/design` take an optional
`campaign_id`; the caller must hold that campaign's DM perspective. With
one, the party boxes in the encounter builder prefill from the party block
and carry a *"from your campaign — 4 characters, level 5"* line with an
edit affordance — the first manual edit takes the table back. Without one,
the surface is exactly what MAD-299 shipped: two number boxes and a default
table. The non-campaign builder is the fallback for a DM who has no
campaign, and it does not regress.

`GET`/`POST /api/campaigns/{id}/encounters` list and create campaign-scoped
encounters — DM only, per ADR 2, because a roster is DM material by
definition. A create with no party falls back to the campaign's declared
levels: the DM who wrote their party down once does not type it again to
save a fight against it. `session_event_id` and `scene_id` are not writable
from the request body — the layers that own those ids link a record to one,
not the other way round.

Campaign encounters still appear in the builder's own saved picker (they are
still their author's), and updating or deleting them goes through the same
owner-scoped endpoints, scope intact.

## Out of scope, deliberately

Objectives, terrain and hazards as *behaviour* — the next split of MAD-317
fills the columns this one reserves. Tactical analysis. Any designer.
Running the encounter at the table (MAD-318).
