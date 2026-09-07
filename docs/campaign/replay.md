# Session replay: the mechanical event log, played back

By the time a fight ends it has already been written down. The
[tracker](combat.md) journals every turn, every application of damage
and healing, every death save and condition, in order — and the dice
are reproducible by construction, `(seed, nonce, formula)` spelling
every roll again ([ADR 20](../decisions.md#adr-20--replay-is-a-derivation-never-stored-state)).
Replay is therefore not new data and no new tables: it is a
*derivation*, the same pure grammar the tracker ran live, folded over
the journal a second time, up to any point. The never-stored-balances
rule makes every intermediate state free — the state at journal row N
is a function of the frozen lineup and the first N rows, nothing else,
and scrubbing is just folding again. Stage 9 of the mechanical layer
(MAD-426), built on the [dice engine](dice.md) (stage 3) and the
[tracker](combat.md) (stage 5) whose streams it replays.

## The fold

`internal/replay` runs `internal/combat`'s own pure functions over the
journal, row by row, in seq order:

- **Seeding.** The lineup's frozen fields (identity, initiative, AC,
  the statblock snapshot, position) never change mid-fight, so the
  recorded rows still hold them; every mutable field resets to its
  opening value. Opening hp — the one number a pc who walked in
  wounded cannot name from the rows alone — anchors on the journal's
  own `before` at the fighter's first damage or heal row. A combatant
  no row ever touched was never overwritten: their recorded row *is*
  the opening state.
- **Deriving.** Each row re-runs the rule it records: damage through
  the hit-point grammar (resistance, temp hp, the downed and dying
  rules), heals, death saves, conditions and their round wrap,
  legendary budgets, reactions, the turn engine's side effects. Turn
  rows carry their own round; the rows between inherit it.
- **Anchoring.** Where a row records the absolute it produced — the hp
  after a hit, the death-save ledger, the budget spent — the fold
  asserts the journal's number against the rule's derivation. A
  journal that disagrees with its own grammar is **drift**: replay
  refuses to invent state, the API answers 422 naming the row, and
  nothing is quietly patched.

### The checksum

`verify` folds the whole journal and compares the derived end-state
against the recorded combatant rows — canonical JSON (fixed field
order, combatants in initiative order), hashed with sha256. A replay
that matches shows the digest; one that does not lists every
disagreement, field by field. This is the acceptance rule, not a vibe:
the journal and the rows it underlies must agree **mid-battle and
after the end**, or the replay says so in words.

### Scrubbing

`GET .../replay/state?at=N` derives the frame at any journal position:
round, whose turn, every combatant's live numbers, the battle's
standing. The fold is pure and in-memory over a journal loaded once,
so every scrub position costs the same as the last — round 1 feels as
live as the final blow. `at=0` is the opening lineup; past the end is
the fight as it finished.

## The recap

The [session export](sessions.md) gains a **The fights** section — the
"how it actually went" beside the narrative log it already writes.
Every fight the session's kind `combat` events name is rendered from
the same journal: the table's own one-line accounts grouped under
round headers, then *where they stood* — each combatant's final
numbers as the rows recorded them (the same numbers the checksum
asserts).

## The surface

The replay is the DM's screen, like the tracker it reads — it spells
exact monster numbers and the context of secret rolls.

| Route | What it is |
|---|---|
| `GET /api/campaigns/{id}/combats/{cid}/replay` | one battle's journal, final frame, and the checksum assertion (DM) |
| `GET /api/campaigns/{id}/combats/{cid}/replay/state?at=N` | the derived frame at journal position N — the scrub (DM) |
| `GET /api/campaigns/{id}/sessions/{sid}/replay` | the session's whole replayable timeline: the log with its fights indexed (DM) |

Reads only: replay derives, it never writes — no broker ping, no
mutation, nothing to reconcile. The timeline carries the session's
events (rolls included, in the [feed's](dice.md) own notation) and one
index entry per fight: name, standing, rounds, journal length, the
combat id the battle endpoints scrub.

## Where the code lives

- `internal/replay/replay.go` — the pure fold: seeding, the row
  grammar, drift, the canonical form and its checksum. No database, no
  clock, no network.
- `internal/replay/store.go` — the reads, through narrow windows onto
  the combat and session stores (the standing every mechanical reader
  takes); `Battle` (journal + frames + verify) and `Session`
  (timeline).
- `internal/gamesession/recap.go` — the export's fights section,
  rendered from the journal and the standing rows.
- `internal/server/replay.go` — the three routes, DM-gated; a drifted
  journal answers 422.
- `internal/replay/testdata/battle_golden.json` — one scripted battle's
  canonical end-state, pinned: the fold must reproduce it byte for
  byte.
- No migration. The journal, the rows and the session log already
  existed — that was the point.
