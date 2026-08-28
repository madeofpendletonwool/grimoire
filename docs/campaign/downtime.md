# Downtime resolution: "I spend three weeks researching the cult"

Part of [MAD-315](../decisions.md). Downtime is the [simulation
tick](simulation.md) pointed at one character: the same window, the same
seed, one `sim.Tick` — plus what the character gets out of it. It is also
the first place in the campaign engine where **scope is load-bearing**.
What a character can find out is not what the DM knows; it is
`campaign.ScopeCharacter(entityID)` over the knowledge layer. A downtime
feature that asked a model "what do they find out?" would leak the campaign
on its first use — perspective is authorization, not instruction
([ADR 2](../decisions.md)).

So the answer is computed in three deterministic stages, before any prose
exists, and it lands as a proposal the DM decides — like everything else
the machine suggests.

## The three stages

```go
downtime.Resolve Inputs → *Result
```

Pure. In memory. No model in the decision path, no database, no wall clock.
The same inputs and seed produce a byte-identical result, forever —
reproducible exactly like a tick, and diffable the same way.

1. **What is reachable.** The character's position is a `located_in` edge
   on their pc entity. From it, the route graph (the same undirected
   day-cost roads travel reads) bounds the set of locations whose one-way
   cost fits inside the window — three weeks does not reach a library
   eleven weeks away — and the live NPCs standing at those locations
   (`located_in` / `contains`, either direction) are the window's
   **sources**: the people the character can talk to. Nothing outside that
   set can be consulted. A character with no recorded position reaches
   nowhere and nobody.
2. **What is discoverable.** Candidates are the live facts the activity's
   **subject** touches (subject or object end) that this character does not
   already hold a granting `awareness` stance over — read at the character
   scope, so something the party already knows is not a discovery. A
   `visibility='secret'` fact is a candidate **only if a reachable source
   could plausibly hold it**: some source NPC holds a granting stance on it
   themself. No reachable holder, no candidate — a die roll against
   nothing is a leak with extra steps.
3. **Whether it lands.** A published arithmetic, scored per candidate:

   ```
   score      = dayScore + sourceScore + proficiencyScore + roll
   dayScore        min(3, days / 7)
   sourceScore     min(3, Σ stance weights over distinct reachable granting
                    sources — knows 1, suspects 0.7, believes_false 0.5)
   proficiencyScore min(2, proficiencies from the pc payload's
                    "proficiencies" list relevant to the activity)
   roll            seeded 0..2 — the one honest die, folded from the seed,
                   the character and the fact, reproducible forever
   difficulty  = 4 (public) + 3 (secret) + 1 (contested confidence)
   ```

   `score >= difficulty + 1` lands as **knows**; `score >= difficulty − 1`
   lands as **suspects**; below that, nothing lands and nothing leaks.
   Secrets need real investment: weeks, good sources, the right skills.

**And what the cult did in those three weeks.** The same window is one
`sim.Tick` — plans advance, schedules come due, NPCs act, rivals react —
so the answer to "I researched them for three weeks" includes the plan
having moved, which may be precisely *why* there is something new to find.
The result carries both halves: the character's findings and the world's
movement.

## The activity vocabulary

`research | craft | train | carouse | work | travel | recuperate | scheme`.

Free text is mapped onto it the way `encounter.ReadIdea` maps a free-text
encounter idea onto creature types — words map, plain/plural/gerund forms
included ("I want to drink at the tavern" → carouse). Text that maps to
**none** of the vocabulary, or to **several**, is a clarifying question
with the candidates attached — never a guess. The request records nothing
until the question is answered.

`travel` is the activity whose subject is a destination: it is refused
**with the reason** when no route exists between the character's position
and the destination ("no route between Blackwater and Far Hold"), or when
the journey outlasts the window. Other activities carry their subject —
the thing researched, the crowd caroused with, the rival schemed against —
and the subject bounds the candidates: a public fact Tom knows about the
Duke is not findable by researching the cult.

## One proposal batch, one window

`POST /api/campaigns/{id}/downtime` records one request — whose downtime,
doing what, about what, over which window, under which seed — and computes
the deterministic answer. The seed is optional; a caller-omitted seed is
derived from the campaign, the day, the window, the character, the activity
and the subject, so the same question re-asked re-derives the same answer.
A request writes nothing but its `downtime_requests` row: it is a question.

**A player may request downtime for their own character** — the time is
theirs — and the response carries the request's identity and nothing else.
The computed result names secrets the character has no path to yet; what
they find out is the proposal the DM decides, and the DM decides it before
anyone reads it. A player's subject must resolve at their own perspective;
anything else is the same 404 a missing entity produces, so the endpoint
cannot be probed for hidden entities.

`POST /api/campaigns/{id}/downtime/{did}/stage` (DM-only) turns one
recorded request into one [proposal batch](../decisions.md)
(`source: "downtime"`):

- the **downtime event** — the anchor, dated on the clock at the window's
  end, the character its participant;
- the **activity fact** — the genuinely new record that the time was spent
  ("Thalia spends 21 days researching the Cult of the Root."), public,
  dependent on the event;
- one **proposed discovery** per finding — `knowledge.RecordDiscovery`
  through the review queue, awareness granted **for the requesting
  character and nobody else**, `since_event` wired to the downtime event;
- the window's **tick outcomes** — the same items a staged tick would
  stage, under their own item prefix so a separately staged tick of the
  same window never collides — plan transitions, due occurrences, NPC
  actions, consequences.

Staging refuses a request whose digest no longer matches ("the campaign
has changed since this request") or whose window the clock has left.
**Accepting the batch — in full or in part, on the ordinary review surface
— moves the campaign clock by exactly the window, exactly once, reason
`'downtime'`.** Dismissing it discards the window; the character's time
did not pass. Both paths are idempotent against a retried decision.

## What it does not do

No system-specific crafting or training tables — a 5e-rules follow-up, not
this stage. Nothing applies automatically without review. And travel moves
the clock and records the journey; re-pointing the character's
`located_in` edge stays the DM's call, because the review queue can add
edges but cannot delete them.
