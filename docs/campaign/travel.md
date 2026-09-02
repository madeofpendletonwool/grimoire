# Travel: the road between two places, at the density the DM asked for

*"You travel for five days"* is a legitimate answer and must stay cheap.
The density knob is the feature: the same road can be a single line, a
light scatter of incidents, or a dense run of encounters — the DM decides
how much road they want to play, and the arithmetic lands exactly that
many incidents, not eleven scenes because the journey was eleven days.

The route graph, the distances, the calendar and the weather are the
campaign clock's ([The Clock](clock.md)); this layer is everything on top
of them: which days carry something, what that something is, and how the
journey lands back in the campaign.

Three pieces make it up:

- **`internal/journey`** — the pure planner (`journey.go`) and the rows
  and the review-gate orchestration (`store.go`). Migration `0025` owns
  the `journeys` and `journey_days` tables.
- **The REST surface** — `POST /api/campaigns/{id}/journeys` (plan),
  `GET`/`PATCH .../journeys/{jid}`, `POST .../journeys/{jid}/days/{n}/resolve`,
  `POST .../journeys/{jid}/resolve`. DM-only, every endpoint.
- **The UI** — the day table as a strip of cards, each with its weather
  and its incident, resolved days inked and unresolved ghosted — the quest
  board's taken-and-untaken grammar applied to a week on the road.

## The density knob

| density    | incident every | an 11-day crossing produces |
|------------|----------------|------------------------------|
| `none`     | never          | one line, zero day rows      |
| `light`    | 5–6 days       | 1–2 incidents                |
| `standard` | 3–4 days       | 2–3 incidents                |
| `dense`    | 2–3 days       | 3–5 incidents                |

The band is published arithmetic (`journey.Band`), and the count the roll
chooses always lands inside it — the test asserts this for every route
length from 1 to 60 days, at every density, with the model faked.
`none` is the hand-wave: **zero model calls, zero day rows**. The
five-sentence travel montage should not cost a token; the test's fake
client fails the run if it is invoked at all.

## Determinism, then prose

Which days carry something is a **seeded roll** against the density band
and the terrain of each leg — pure, re-runnable, diffable. The same seed,
the same density and the same route produce a byte-identical day table,
across calls and across process restarts: the table is a function of the
stored world, never of the moment it was asked for. (The keyed draws mix
the seed through a splitmix finalizer rather than a bare FNV over
seed-then-key — FNV is affine, and bare-FNV streams for different keys
correlate across seeds, which once made discovery days and rumour days
mutually exclusive on the same road. The finalizer decorrelates them; a
distribution test pins it.)

The model writes prose for the days the roll already chose, and may not
add a day, remove one, or move one — the structured-generation harness
validates exactly that, the same contract the simulation tick's flavour
pass carries. A model that answers garbage (or no model at all) degrades
to the deterministic summary; the journey never fails on prose.

## The encounters come from the world, not from nowhere

A day's candidate pool is assembled deterministically before any prompt:

- **locations** reachable from that leg, and their place blocks —
  landmark days pass places the DM wrote ([Locations](locations.md));
- **the factions whose territory the leg crosses** (`owns`/`owned_by`
  edges) — their patrols work the road;
- **rumours circulating along it** ([Rumours](rumors.md)) — a holder
  standing at a leg's location, or a rumour about the place itself.
  Hearing one on the road writes the same stance a rumour heard in a
  tavern does;
- **NPCs standing along the leg** — company on the road;
- **discoveries off the road** — child locations the route itself does
  not walk through: the shrine beside the pass, not the pass;
- and for combat, **`encounter.Plan`** with the party's level band
  ([Encounter Builder](../features/encounter-builder.md)) — a wandering
  monster is an encounter, and the DMG arithmetic for one already exists.
  The day carries its budget line: target XP, ceiling, the shape it
  rolled.

A travel day that invents a brand-new faction is the failure mode; a
travel day that finally brings the party past the shrine the DM built
three sessions ago is the feature. Every entity a day proposes exists in
the campaign graph (rumour days name a rumour the mill owns) — the test
walks hundreds of seeds and asserts it.

## Nothing lands until the journey resolves

A **planned** journey writes exactly two things: its own row and its day
table. No graph rows, no awareness, no clock movement — the acceptance
test snapshots the graph's row counts before and after planning and
asserts they are identical.

Playing the journey out is the DM's per-day work:
`POST .../journeys/{jid}/days/{n}/resolve` marks one day happened at the
table, optionally overriding its detail (the DM's own account of what the
day became) and attaching the encounter that actually ran. The first
resolved day moves the journey to `underway` — the party is on the road.

`POST .../journeys/{jid}/resolve` stages the whole road as **one proposal
batch** behind the review gate ([the batches every generator uses]
(model.md)): the travel event, one event per non-uneventful day, a
discovery per rumour day whose rumour carries a fact (the party
`suspects` a true one, `believes_false` a false or distorted one — the
same stances the heard path writes), and a fact per discovery day.
Accepting the batch is what makes it true, and the finalizer does the one
write the batch cannot carry itself: the campaign clock advances by
exactly the journey's days, **once**, with `reason='travel'` through the
`clock_advances` ledger — ADR 3 applied to a week on the road. Fact-less
rumours the road handed out land as holdings on the mill, the same write
`RumorHeard` makes. A dismissed batch puts the journey back to
`planned`; abandoning it is the DM's own verdict (`PATCH`), and time does
not pass.

Resolving refuses, with the reason, a journey whose window the clock has
left — something else moved time, so the road as planned is stale and a
new journey should be recorded.

## The rows

`journeys` carries the journey's identity: endpoints, the route as the
clock's shortest path computed it (the leg list, stored as JSON so the
journey stays reproducible when the route graph later changes), the start
day, the day count, the density, the pace, the seed, the status
(`planned | underway | done | abandoned`), and the session it was played
in. A DM-declared day override (a road the map does not hold) stores its
single synthetic leg the same way.

`journey_days` is the day table: the clock day, the leg, the weather
(`clock.Weather`'s answer for that day, recorded because a journey is a
record of what happened even though weather is otherwise computed and
unstored), the event kind (`uneventful | encounter | discovery | hazard |
social | rumour | landmark`), the detail, the entity the day is about, the
encounter that was run, and the resolved mark. `entity_id` carries no
foreign key on purpose: it names whatever the day is about — an entity
for most kinds, a rumour id for rumour days. The Go layer validates it.
