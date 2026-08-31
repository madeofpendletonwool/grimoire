# Dungeons: the seeded room graph, its map, and the dressing pass

A dungeon is the one campaign artefact that is *mostly arithmetic*. The
DM hands in a theme, a size band, a level, an expected session count and
three density knobs; what comes back is a room graph with a guaranteed
critical path, a boss behind a locked door, wings, hazards and secret
rooms — and an editable map that is a rendering of that graph, not a
second thing to maintain. Same seed, same params, same dungeon, forever.

The design rule is the one the whole planning layer follows: structure
is computed, prose is proposed. `internal/dungeon` is a pure package —
no database, no network, no clock, and a test that fails the build if
that ever changes — so a whole dungeon can be generated, laid out and
drawn without an API key. The model is asked for exactly one thing:
naming the rooms it was handed.

## The topology is arithmetic, not a prompt

`dungeon.Layout(params, seed)` computes, in order:

- **The room count** from the size band and the expected sessions. The
  DMG's adventuring-day arithmetic — six to eight meaningful encounters
  a session — is read as rooms: a delve burns its whole budget in one
  session, a megadungeon pays twenty-two rooms up front and eleven a
  session after, capped at sixty because past that the map stops
  reading.
- **The purpose quotas** from the density knobs. Combat, puzzle and
  exploration weights become integer room counts by largest-remainder
  allocation — a DM asking for high puzzle density gets the puzzle
  rooms counted out before a model sees anything — and the four quotas
  (combat, puzzle, exploration, secret) sum to the room count exactly.
  A property test over a hundred seeds and the full parameter range
  asserts it.
- **A guaranteed critical path** entrance → boss, with every other room
  reachable off it. Wings hang off inner path rooms and cluster toward
  the boss; secret rooms hang off the critical path behind secret doors,
  so finding one is a reward and missing one is survivable. Loops are
  added to the branchiness knob (0–3), never to taste — and a loop edge
  is only added when it crosses nothing already drawn and passes
  through no room's cell.
- **A grid assignment** that keeps the graph drawable without crossing
  edges: the critical path runs down one column, each wing is a gallery
  along its own row, secrets sit in their own columns to the left. The
  generator's own output is certified crossing-free by the same checker
  the tests use (`Graph.Crossings`).

The boss's door on the critical path is a `locked_door`; the key item
that opens it is part of the dressing.

Re-rolling means changing the seed, which is a recorded decision in the
`dungeons` row — there is no refresh button, for the same reason the
campaign clock has none.

## The dressing pass dresses rooms it was handed

`POST /api/campaigns/{id}/dungeons/{did}/dress` runs one
structured-generation call. The fillable schema is keyed by the computed
room keys — every room's name and detail, the boss's name and motive,
the dungeon's secret, the key item's name — so the model cannot add a
room, remove a room or reconnect anything: a fill that returns an extra
room, a missing room or a new edge is an undeclared field, and the
validation pass rejects the mismatch outright rather than repairing it.
The tests assert the rejection with a fake client that keeps returning
a changed topology: the exchange fails and nothing is written.

## The map is drawn, not generated

The map renders from the stored grid in vanilla JS, the way the
neighbourhood map and the quest machine chart already render: no
library, no physics, whole-pixel geometry, the pixel font for labels.
Dragging a room to another cell writes `x` and `y` back
(`PATCH .../dungeons/{did}/rooms/{rid}`); adding or cutting an edge
writes the `dungeon_edges` rows (`POST`/`DELETE .../dungeons/{did}/edges`).
**An edit never re-rolls the dungeon** — the layout's rooms, purposes
and depths are immutable through the edit surface; only names, detail,
positions, connections and the soft encounter link change.

No image model and no tileset: every asset under `web/static/assets/`
is a licensed adaptation recorded in `ATTRIBUTIONS.md`, and an SVG the
app draws itself has no licensing question attached to it.

## A dungeon is planning material until it is placed

Like a quest machine, a dungeon lives in its own tables
(`dungeons`, `dungeon_rooms`, `dungeon_edges` — migration `0023`) and
touches the campaign graph only on request.
`POST .../dungeons/{did}/place` stages one proposal batch through the
MAD-359 review gate:

- the dungeon becomes a `location` entity carrying a structured place
  block (`kind: dungeon`, the secret as its private truth);
- each room becomes a child location with a `contains` edge;
- the boss and the dressing's named hazards become `creature` entities
  in their rooms; the treasure rooms' loot becomes `item` entities;
- the key item becomes an `item` entity, and its id lands on the locked
  door's `key_item_entity` when the batch is decided;
- the dungeon's secret becomes a `facts` row with a clue path — the
  boss holds it, so it is not born `unreachable_secret`.

Accepting the batch is what marks the dungeon `placed` and records each
room's entity id; a partial acceptance marks nothing — the entities the
DM kept stay in the graph, and the dungeon remains placeable again. A
DM can design, map, dress and run a whole dungeon without ever writing
to the campaign graph. That is the point of the separation, not an
omission.

## The surface

DM only, like every planning surface ([ADR 8](../decisions.md)):

| Route | What it does |
| --- | --- |
| `GET`/`POST /api/campaigns/{id}/dungeons` | list, design (no model needed) |
| `GET`/`PATCH`/`DELETE .../dungeons/{did}` | read, rename, delete the design |
| `POST .../dungeons/{did}/dress` | the model pass (model key required) |
| `PATCH .../dungeons/{did}/rooms/{rid}` | name, detail, drag a cell, encounter link |
| `POST`/`DELETE .../dungeons/{did}/edges` | add, cut a connection |
| `POST .../dungeons/{did}/place` | stage the placement batch |

`dungeon_rooms.encounter_id` deliberately carries no foreign key:
`encounters` is owner-scoped, not campaign-scoped, and making it
campaign-aware is the encounter layer's to do — the precedent is the
soft `session_id` columns of `0002` and `0003`, which waited for the
layer that owned them.

## Out of scope

Battle maps and tactical grids; campaign, region and city maps (the
route graph rendered — geography's problem); encounter rosters per room
(this layer stores the link and no more); running the dungeon at the
table.
