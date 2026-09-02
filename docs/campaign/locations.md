# Locations: the place block and the dossier

A location is an entity (`kind='location'`) plus very little else. Most of
what the word "location" suggests is already expressible on the campaign
graph and simply read nowhere — so the dossier reads it live instead of
copying it into a payload blob that would drift from the graph inside one
session.

No model is involved. Stage 1 is deterministic all the way down, and there
is **no new table and no new column** — that is the point.

## The dossier is a read, not a write

| Asked for | Already in the graph |
| --- | --- |
| NPCs present | `located_in` / `contains` edges |
| Connections | the `contains` hierarchy + the `travel.routes` block on this same payload |
| Secrets | `facts` with `visibility='secret'`, the place as subject or object |
| Loot | `item` entities with `located_in` / `contains` edges |
| History | `events` with `location_entity` set |
| Encounters | quests sited here (`quest_entities`, role `site`) |

`place.Dossier(snap, entityID)` (internal/place) assembles all of it over an
already-loaded snapshot — no DB access inside the assembly, the same shape
the integrity rules follow — so the whole thing is testable as data. Adding
a `located_in` edge or a secret fact changes the dossier with **no write to
the location entity**.

## The place block

The one stored thing is the payload's `place` block (`campaign.PlaceOf`,
under the `"place"` key, so it cannot collide with the `travel` block or a
DM's own notes), decoded exactly the way the NPC mind is: tolerant of a
missing block, a malformed block, or an entity that is not a location — all
yield the zero block, which is a valid, unwritten place. It holds only what
the graph cannot say:

- `kind`, `scale`, `population`, `government` — the settlement's shape
- `services` — notable services, one per line
- `defences`, `state` (current state), `danger` (0–5-ish, deliberately unnumbered)
- `climate` — the tag `clock.Weather` reads; the clock's `?location=` query
  resolves it through the block first, then the bare top-level `climate` tag
  a payload may carry from before the block existed
- `senses` — sensory notes: what the party hears and smells
- `private_truth` — the DM-only half

The public/private split mirrors `NPCAgent.PublicIdentity`/`PrivateTruth`:
a town's scale and smells are player-facing; what is really going on is not.

### The description lives in `summary`, never in the payload

The campaign prose index is built from `NEW.name || ' ' || NEW.summary`
(migration `0003`, trigger `campaign_prose_entity_upd`), so a description
written into `payload` is invisible to campaign search and to every
retrieval path the chat uses. The one-paragraph read-aloud belongs in
`entities.summary` (PATCH the entity); the structure belongs in the block.
This is stated here because it is not guessable.

## Two dossiers, one function

The DM dossier is assembled over `campaign.LoadSnapshot` — every fact,
secrets included. The player dossier is assembled over the scoped snapshot
`knowledge.PlayerView.PlaceSnapshot` builds from the scope's own reads: met
entities, held facts, witnessed events, aware edges, public quests.
**Secrets, private truth and unmet NPCs are absent because the rows were
never loaded, not because a field was blanked** (ADR 2 — perspective is
authorization, not instruction). The block's public half crosses through a
decode struct with no `private_truth` field to fill, the same trick
`FactionFacade` pulls with the agent block's two public fields. The travel
block is DM payload and does not cross at all — a player's roads are what
they have walked.

The knowledge leak test's shape holds the line: a player-scope dossier body
contains no secret fact (even one the party has been granted — the
PlayerView rule), no private truth, and no NPC the party has not met.

## Surfaces

- **REST**: `GET /api/campaigns/{id}/locations` (the places, with scale and
  a live child count), `GET .../locations/{eid}` (the dossier,
  scope-resolved like every campaign route), `PUT .../locations/{eid}/place`
  (the block; body is the block itself, DM-only, every other payload key
  preserved).
- **UI**: the location sheet in the campaign page — the block as an editable
  form, everything else as read-only chips linking into the entity browser,
  so it is visibly a *view of the graph* rather than a second place to type
  things. Vanilla JS, no build step.
- No CLI and no migration: nothing new to administer, nothing new to store.

## What is deliberately next

Generation (MAD-372). Dungeon interiors and maps (MAD-373). Rumours
circulating here — the dossier ships the empty list and MAD-374 fills it.
The route graph and travel time themselves are MAD-365's; this page reads
the `travel` block if it is there and renders nothing if it is not.
