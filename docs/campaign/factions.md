# Factions: the dossier and the plans

A faction is an entity (`kind='faction'`) plus two things the graph cannot
say for itself: an authored interior, and a plan that progresses on its own.
Everything else the word "faction" suggests — territory, leaders, members,
allies, enemies — is already a set of edges on the controlled vocabulary,
and the dossier **reads them live** rather than copying them anywhere.

No model is involved. Stage 1 is deterministic arithmetic all the way down,
exactly like the clock.

## The dossier is a read, not a write

| Dossier field | Where it already lives |
| --- | --- |
| Territory | outgoing `owns` / `owned_by` / `contains` edges |
| Leaders | `leads` / `led_by` edges |
| Members | `has_member` / `member_of` edges |
| Allies / enemies | `allied_with` / `enemy_of` edges |
| Pulled strings | `secretly_controls` / `secretly_controlled_by` |
| Secrets | `facts` rows with `visibility='secret'`, the faction as subject |
| Relationship with the party | the party's `awareness` rows over those facts, plus edges to `pc` entities |

Adding an `owns` edge changes the dossier's territory with **no write to the
faction entity** — a test asserts the entity row does not move. A faction
page that disagreed with the graph view of the same campaign would be worse
than no faction page, so there is no second source of truth to disagree.

The one stored thing is the payload's `agent` block, decoded exactly the way
an NPC's mind is (`campaign.FactionAgentOf`, under the same `"agent"` key),
and it holds only what the graph cannot say:

- `public_face` — what the realm believes it is
- `private_truth` — what it actually is
- `doctrine`, `goals` (ordered; first listed, first pursued), `reputation`
- `military` / `economic` / `reach` — deliberately unnumbered 0–5-ish scales
- `internal_conflicts` — the fault lines a clever party can pull

The public/private split mirrors `NPCAgent.PublicIdentity`/`PrivateTruth`
for the same reason: a faction's reputation is player-facing and its actual
aim is not.

**Player scope** (`GET /api/campaigns/{id}/factions/{eid}` as a player) gets
the public face, the reputation, the facts that character is aware of, and
the edges that character is aware of — and **no plan, no `private_truth`,
and no secret fact, by construction**: the player read runs through
`knowledge.PlayerView`, whose `FactionFacade` method can return exactly two
payload fields and no others. A leak test asserts the player's dossier body
contains none of the forbidden material.

## A plan is the quest state machine plus four things

`campaign.StateMachine` — the same type quests use — already parses,
validates and enforces legal edges. A faction plan is that machine plus:

- an **owner** (a live `kind='faction'` entity),
- a **rate** (`rate_per_day` progress per in-world day),
- a **progress counter** (work paid toward the active step), and
- a **reaction rule** (what the author says happens when a step's world
  goes away).

The rows live in migration `0017`: `faction_plans`, `faction_plan_steps`
(in pursuit order; a step is the work to *enter* its state, `cost` progress
at a time), and `faction_plan_transitions` (every move, the clock day it
happened on, and the arithmetic that caused it).

## Progression is arithmetic, not mood

`faction.Advance(plan, days, mods)` is pure — no DB, no clock, no randomness:

```
gain = rate × days + Σ modifiers (each term: rate × days × (factor − 1))
```

A modifier carries a **sign** and a **reason**: factor 2 doubles the pace
(the cult accelerates), 0 halts it, negative undoes work. Progress pays the
active step's cost; **overflow carries into the next step**, so one 30-day
advance and thirty 1-day advances land on the same state (a test pins it).
A plan never moves along an edge its own machine does not declare — the
`TransitionQuest` rule reused; when the edge is missing the advance halts
with the work banked rather than forcing an illegal move.

Interference is a rule the plan's author writes, not something the engine
guesses. A step's `requires` names its preconditions — an entity that must
be live, an edge that must exist, a fact that must hold, an enemy plan that
must not have reached a named state — each with the author's chosen
`if_broken` reaction (a signed factor and a reason). The store derives the
modifier set from live graph state at advance time; every recorded advance
carries its terms in the transition's reason, so *"why is the Cult at 62%?"*
is answered from the ledger: `base 300; shrine_destroyed 150 (the martyrdom
inflames the cells); entered seed_the_mines (carry 12)`.

## Integrity

Four checks read the plan rows through the campaign snapshot:

- `plan_illegal_state` (error) — the plan sits in a state its machine does
  not declare.
- `plan_without_faction` (error) — the owner is not a `kind='faction'`
  entity, or is destroyed.
- `plan_stalled` (warn) — active, positive rate, no advance in 30 in-world
  days.
- `faction_no_antagonist` (info) — an active plan with no `enemy_of` edge
  and no opposing precondition. Nobody is in its way.

## Surfaces

- **REST**: `GET /api/campaigns/{id}/factions`, `GET .../factions/{eid}`
  (the scope-filtered dossier), `GET`/`POST .../factions/{eid}/plans`,
  `PATCH .../plans/{pid}`, `POST .../plans/{pid}/transition`. Plan routes
  are DM-only; the dossier serves both scopes.
- **CLI**: `grimoire campaign plans <campaign-id>` — a read-only listing
  (owner, status, state, the checklist) beside `campaign check`.
- **UI**: the faction entity's sheet grows the dossier — the face it shows,
  the live graph position, and (DM only) the plans with a progress bar,
  their step list, and buttons for exactly the moves the machine declares.

## What is deliberately next

Advancing plans as time passes — the simulation tick that calls
`AdvancePlan` over the clock's delta — is the next stage. Generated faction
content is MAD-361's skeleton generator, which will write into this shape.
