# Rumours: statements with truth values, and who is repeating them

*"The dragon eats children"* — false. *"It lives beneath the old
monastery"* — true. A rumour is a statement in circulation plus a truth
value the DM holds and nobody else ever reads, and a list of who in town is
repeating it, in their own drifted wording. That is the whole feature, and
the whole difficulty: the graph had no slot for a statement that is untrue.

`facts.confidence` is `proposed | canon | derived | contested | retconned`.
None of those means *untrue*, and `proposed` is unreadable by every
perspective including the DM's chat. So a false rumour cannot be a `facts`
row, and a rumour has no fact to hang an `awareness` row on when it
contradicts nothing. Rumours get their own tables (migration `0024`) and
reach into the knowledge layer only through the heard path.

## The tables

`rumors` — `id, campaign_id, statement, truth, about_entity, fact_id,
origin, spread, status, dm_only, created_by, created_at, updated_at`.

- `truth` is `true | false | distorted`. `fact_id` is the canon fact the
  rumour attests (when true) or the one it distorts (when distorted), and
  empty for a rumour invented whole. This is the column that never
  travels: see below.
- `spread` is `local | regional | widespread` — how far the talk has
  gone. The generator proposes holders weighted by it.
- `status` is the rumour's life: `circulating | debunked | confirmed |
  dormant`. Debunked and confirmed are the DM's verdicts once the table
  settles it; dormant is one nobody has repeated in a long while, the
  companion of `dormant_region`.
- `dm_only` marks the rumour the DM planted for their own eyes. It never
  reaches a player scope at all — not blanked, absent.

`rumor_holders` — `rumor_id, entity_id, variant, since_event`. `variant`
is the drifted wording *this* NPC repeats, which is the whole charm of a
rumour and costs one column. `entity_id` may be the literal `party` — a
fact-less rumour a knower hears is carried here, because `awareness`'s
primary key is `(campaign_id, knower, fact_id)` with a real foreign key to
`facts`; making the target polymorphic would touch every scoped query and
both leak tests to buy one column. That limit is documented rather than
engineered around.

## Truth is DM-only, always

The rule is the same one every scoped read here runs under — perspective is
authorization, not instruction. A player-scope SELECT does not filter truth
values out of the result; it selects `'' AS truth`, so the column never
loads, and adds `dm_only = 0`, so the DM's private mill never leaves the
DM. A player reading any rumour endpoint receives the statement, the
spread, the status, and who said it — never the column, never the fact
link, never the DM-only row. The reflection leak test sweeps every scoped
rumour retrieval the same way it sweeps facts, and asserts it.

## Hearing a rumour writes a stance

`POST .../rumors/{id}/heard` with a knower routes through
`knowledge.SetAwareness`, so the existing transition table governs every
move:

- `truth='true'` with a `fact_id` → `suspects` on that fact. Hearsay is not
  knowledge; the DM upgrades it when the party confirms it.
- `truth='false'` or `distorted` naming the fact it contradicts →
  `believes_false` on that fact — the case the feature exists for. Under
  the settled semantics ([epistemics.md](epistemics.md), "What
  `believes_false` means, exactly"), the fact column carries the true
  statement the belief contradicts; the wrong content is the rumour.
- No `fact_id` named → the holding lives on `rumor_holders` alone, and
  renders as a rumour the character carries.
- A knower who already `knows` the fact is left alone. Gossip never
  downgrades knowledge.

Every stance write goes through `CanTransition`; an illegal stance move is
refused rather than written, and a repeat of the current stance is a
no-op, not an error.

## Generation

`POST .../rumors/generate` about an entity or a location stages a proposal
batch — nothing enters the mill until it is decided on the review queue,
like every generator. Deterministic before the prompt:

- **The truth mix is a parameter**, not the model's mood: how many true,
  how many false, how many distorted. The model fills words into exactly
  that many slots, no more.
- **The true ones are drawn from the campaign's own secret facts.** A
  rumour that is true and about nothing in the graph is a rumour that
  leads nowhere — the single most common way a generated rumour table
  wastes a session. Asking for more true rumours than secret facts exist
  is refused with the count, never padded.
- **The false ones are filtered** against facts the party already holds a
  granting stance on — a false rumour the party can instantly disprove is
  noise. When the contradictable pool runs dry the remaining false rumours
  are invented whole; the mix is still honoured exactly. Distorted rumours
  must name their fact and draw from the pool's tail.
- **Distribution is a join, not a model call**: candidate holders are the
  NPCs located in the area — the subject and every place that contains it,
  up the `located_in` chain — plus, for a non-location subject, the NPCs
  who know them or march in their factions. First-k in stable order,
  weighted by `spread`.

The model's whole job is the wording: one field per computed slot, each
slot's description carrying the fact it attests, contradicts or twists.

## The health checks

Two deterministic checks join the engine, and one gains a clause:

- `rumor_orphan` (warn): a `circulating` rumour nobody repeats. A rumour
  with no holders is a row, not a rumour — the party can never hear it.
- `rumor_dead_end` (info): a rumour about an entity with no live
  storyline — no live fact, relationship or event touches the subject.
  Pairs with `dormant_region`.
- A rumour attached to a secret fact is a clue path: the cheapest fix for
  an unfindable secret. A live rumour (circulating or confirmed) attesting
  a secret clears its `unreachable_secret` finding — the health report's
  "this secret is unreachable" gains an actual button.

## Surfaces

- CRUD under `/api/campaigns/{id}/rumors`, `POST .../heard`, `POST
  .../generate`; holders under `.../holders`.
- The rumour mill panel in the Campaign tool: the DM writes rumours by
  hand, generates batches, marks rumours heard, gives them voices, and
  debunks or confirms them; players see what is circulating and who says
  it.
- "What are people saying about this?" from the location dossier — the
  `rumours` field the dossier reserved from day one — and from the NPC
  sheet.

## Out of scope, deliberately

Rumours propagating on their own over time — that is the simulation tick
(MAD-367). Rumours heard on the road (MAD-375 consumes this).
