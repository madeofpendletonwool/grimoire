# The simulation tick: advance the world by N days

Part of [MAD-315](../decisions.md): the campaign clock and faction plans keep
moving between sessions, and the DM should be able to ask "what happens if
two weeks pass?" without inventing the answer from scratch every time. The
tick answers that — deterministically, offline, and behind the same review
gate as everything else the machine proposes.

## The tick is a function, not a prompt

```go
sim.Tick(snap *canon.Snapshot, cal *clock.Calendar, plans []faction.Plan,
         entries []campaign.ScheduledEvent, days int, seed int64) Result
```

Pure. In memory. No DB, no wall clock, no model. Given the same inputs — the
same snapshot, the same plans, the same schedule, the same day count and the
same seed — it produces byte-identical JSON, forever. That is what makes a
tick **re-runnable**: staging a preview re-derives its outcomes from the
stored window and seed rather than caching them, and it is what makes two
ticks **diffable** — a different seed, or a changed campaign, is a visibly
different question.

In order, one window `[from_day, from_day+days)` produces:

1. **Plan advances** — `faction.Advance` run over every plan, with its active
   step's preconditions (entity liveness, edge existence, a credible fact, an
   enemy plan's position) evaluated against the *snapshot*, not a second
   database pass — the same rule `internal/faction`'s store applies to live
   rows, but read from exactly the inputs the digest covers.
2. **Scheduled entries that fell due**, in day order, from `clock.Due` — the
   one recurrence expansion in the codebase, unchanged.
3. **NPC goal actions** — the first goal from `campaign.NPCAgentOf`, one
   action per NPC per window, for NPCs who belong to a faction whose plan
   moved (`member_of` / `has_member` / `leads` / `led_by`, either direction).
   Goal order is the priority (MAD-313); whether a goal is *blocked* is
   knowledge-layer state a later stage decides, not something guessed here.
4. **Derived consequences** — an `enemy_of` faction reacting to a rival plan
   that reached a **publicly visible** step (the plan's own `visibility`).
   The reaction mode is the seed's one honest job: a fixed vocabulary, picked
   deterministically from the seed and the two factions involved.

A pending schedule entry the clock has already passed is **reported** in
`Missed`, never fired late.

## Flavour is a second, optional pass

The deterministic `Result` is the whole truth; a preview also gets one
optional pass through `canon`'s structured-generation harness, handing the
model the already-decided outcomes and asking for one sentence of prose
each, keyed by outcome id. It may not add, remove or reorder an outcome — the
harness's own validation enforces exactly that — and any failure (no
`ANTHROPIC_API_KEY` configured, a garbage reply, two failed validation
attempts) degrades silently to no flavour: the tick still answers, plainly.

## The clock does not move until the batch is accepted

`POST /api/campaigns/{id}/simulate` is the preview: `{"days": 14, "seed": 7}`
(seed optional — a deterministic default is derived from the campaign, the
day and the window). It writes one `sim_ticks` row (`preview | staged |
applied | discarded`) carrying the window, the seed and a SHA-256 digest of
every input the tick read — and nothing else. A preview is a question:
row counts on `entities`, `facts`, `events` and `relationships` are the same
before and after, asserted directly in the test suite.

`POST /api/campaigns/{id}/simulate/{tid}/stage` turns a preview into one
[proposal batch](../decisions.md) (MAD-359, `source: "tick"`): every moved
plan becomes an event (dated on the clock) and one `proposed_plan_transition`
item — the whole advance, state and carried progress and all, applied
atomically per plan through `faction.Store.ApplyAdvance` — plus, when the
plan is public, a `proposed_fact` that makes the move visible to the world
(`predicate: "plan_reached"`). Due occurrences, NPC actions and consequences
each become their own event, `depends_on` wired so dismissing a plan's move
refuses the NPC actions it triggered and dismissing the publicizing fact
refuses the reaction to it — MAD-359's cascade, consumed exactly as designed.
Staging refuses a preview whose window the clock has since left, or whose
digest no longer matches — "the campaign changed since this preview" is
detectable, not silently stale.

**The batch renders and decides on the ordinary review queue — no second
review screen.** Accepting it (in full or in part) hands off to the tick's
one remaining write: the campaign clock advances by exactly the window,
exactly once, `reason='tick'`. Dismissing it discards the window; time did
not pass. Both paths are idempotent against a retried decision.

## Offline, always

`ANTHROPIC_API_KEY` unset: the tick still computes, still stages, still
applies — the flavour pass is simply absent. `grimoire campaign simulate
<campaign-id> --days 14 --seed N` runs the same computation with **no
database write at all**, beside `campaign check`: the offline, no-key path,
for a DM who wants the answer without touching the review queue.

## What it does not do

No timer. The world advances when the DM asks it to, on the DM's clock,
never on a schedule of its own — a campaign that moves while nobody is
looking is a campaign the DM has to re-read before every session. Downtime
resolution (players spending days on their own projects) is the next stage;
this issue is the world's half of "time passes."
