# The duration and condition engine: effects that count themselves down

Every ongoing effect is a row: target entity, effect reference (a spell, a
condition, a feature), source, a `concentration` flag, and a remaining
amount in **rounds, minutes, hours, days, until-dispelled or until-rest**.
The bookkeeping nobody does by hand, done by the engine instead — travel
expires the mage armor, the long rest ends Hex, the tenth turn kills
Bless, and every one of those is a fact with provenance, not a sticky
note. Stage 4 of the mechanical layer (MAD-421); the design decision is
[ADR 18](../decisions.md#adr-18--durations-are-canonical-seconds-judged-by-two-clocks).

## One engine, two clocks

A round is 6 seconds — the 2014 SRD's own arithmetic — so every timed
duration converts exactly into **canonical seconds** and the two clocks
are two ways of spending the same number:

- **The combat clock** decrements on turn advance: every timed effect in
  the campaign wears `rounds × 6` seconds per tick, and what reaches zero
  ends (reason `expired`). Stage 5 wires the round counter to this pass;
  until then the DM ticks it by hand (`POST .../effects/advance`).
- **The world clock** is the [campaign clock](clock.md). Every read
  derives what the days since an effect's anchor have worn away — a day
  of travel spends 24 hours of everything timed — and an applied rest
  (the [ledger](ledger.md)'s own rest rows) ends `until-rest` effects:
  short or long, the rest is the event. Nothing is written on the read;
  the clock and the rests table are the truth, `remaining_seconds` is the
  state as of the row's anchor day. The same derivation-not-storage rule
  the ledger folds balances under.

The round/minute boundary is exact, not approximate: `{10 rounds}` and
`{1 minute}` are the same sixty seconds, count down identically on both
clocks, and render the same bytes at every step — the case the golden
file pins (`internal/effects/testdata/countdown_golden.json`). Rendering
speaks the game's own vocabulary: sixty seconds is "1 minute", nine days
and twenty-three hours is "9 days 23 hours", forty-two seconds is
"7 rounds".

## The grammar

```
effect    := {target, kind, name, ref?, source?, concentration?, duration}
duration  := {amount, unit}
unit      := round | minute | hour | day | until_dispelled | until_rest
kind      := spell | condition | feature | other
```

- The condition vocabulary is **the game's fifteen, declared** in
  `internal/homebrew` and enforced exactly the way MAD-383 enforces
  damage types: `poisoned` is a condition, `dizzy` is a 400. Names
  canonicalize to the vocabulary's own spelling.
- Conditions ground in the **indexed SRD**: applying or reading one
  surfaces the real rules text beside the row (`srd: {ref, body}`),
  resolved through the index at read time — never a paraphrase stored
  away from its source.
- Concentration is a first-class link: an effect flagged `concentration`
  names its source, and a source holds **one link at a time** — applying
  the new concentration ends the old one (reason
  `concentration_broken`), recorded like every other ending.
  Concentration on a bare condition is refused: the *spell* carries the
  concentration, not the condition it leaves behind. Damage-triggered
  concentration prompts arrive with Stage 5's combat state.
- Re-applying the same effect on the same target **supersedes** the
  standing row (the game's conditions do not stack; the latest
  application wins) — the replaced row stays in history with its reason.
- Overlapping effects on one target are independent rows that compose
  without corruption: each counts down its own seconds on its own anchor,
  byte-stable whatever its neighbours are doing.

## Ending is a state change, not a delete

A row ends with a reason — `expired` (either clock), `dispelled`,
`concentration_broken`, `rest`, `superseded`, `manual` — and the hand
that ended it. The history read (`?ended=1`) keeps every row with its
story: *why is Thalia still poisoned* resolves the same way *why does
Grimoire think the Duke is a vampire* does.

## The surface

```
GET  /api/campaigns/{id}/effects                        the campaign's effects (DM; ?target=, ?ended=1)
GET  /api/campaigns/{id}/effects/concentrations         who is concentrating on what (DM)
GET  /api/campaigns/{id}/effects/vocabulary             kinds, units, the fifteen conditions (members)
POST /api/campaigns/{id}/effects                        apply an effect (DM)
POST /api/campaigns/{id}/effects/advance                the combat clock ticks N rounds (DM)
POST /api/campaigns/{id}/effects/{fxid}/end             dispel or end by hand (DM)
GET  /api/campaigns/{id}/characters/{eid}/effects       one character's effects (player: own, DM: any)
```

Applying, ending and ticking are the DM's — the table's referee decides
what is on whom. A player reads **exactly** their bound character's
effects, the same `requireOwnCharacter` gate the resource ledger uses;
another character's rows are unreachable (403), not hidden after the
fact. The world clock needs no endpoint: travel, downtime and rests
expire things whether or not anyone was watching, because the next read
derives against the clock and the rest ledger.

## Where the code lives

- `internal/effects/effects.go` — the pure engine: the duration grammar,
  canonical seconds, both clocks, rendering, the declared vocabularies.
- `internal/effects/store.go` — the rows: apply with the concentration
  rule and supersession, the read-time world-clock derivation, the
  combat tick's persisting pass, SRD grounding.
- Migration `0032_ongoing_effects.sql` — `ongoing_effects` and its
  target/concentration indexes.
