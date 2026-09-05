# The resource ledger: pools, spends, and rests that advance the clock

The sheet ([MAD-418](sheet.md)) is a character's *definition*; the ledger
is their *state*. Every tracked thing a character owns — spell slots, hit
dice, ki, rage, ammunition, the purse — is a **pool**, every change to it
is an append-only **transaction**, and the current value of anything is
always **derived** by folding the log, never stored. Stage 2 of the
mechanical layer (MAD-419); the storage decision is
[ADR 16](../decisions.md#adr-16--the-resource-ledger-transactions-not-balances).

## The one grammar

> `{ owner, kind, size, recovery: short | long | dawn | manual, granularity }`

| Kind | What it tracks | Recovery | Bounded |
|---|---|---|---|
| `slot` | one spell level's slots (`slot:3`) | `long` — or `short` when the caster is pact magic | yes |
| `hit_dice` | the level-sized die pool | `manual` (see below) | yes |
| `feature` | ki, rage, Action Surge, Channel Divinity… | as the 2014 rules say | yes |
| `item` | ammunition, rations | `manual` | no |
| `currency` | cp / sp / ep / gp / pp | `manual` | no |

One grammar, no special cases. Pact magic is not a feature with a bespoke
rule — it is a slot pool whose recovery happens to be `short`. Inspiration
and DM-invented meta-currencies (a later stage) will register through the
same grammar unchanged.

*Bounded* pools (slots, hit dice, feature uses) can neither overspend nor
fill past their size; *unbounded* ones (a quiver, a purse) may grow past
what the sheet declared — a spend still never goes below zero.

### Where definitions come from

- **The sheet** (`source: "sheet"`): every slot level in the spellcasting
  table, one hit-die pool sized to the total level, the purse. Re-derived
  on every sheet write and at every boot — the same cache contract the
  sheet projection holds. Rows survive re-sync by id, so transaction
  references never orphan.
- **The DM** (`source: "dm"`): `POST .../resources` registers a feature or
  item pool — ki points, arrows — because the sheet carries no numbers for
  them. A sync never touches a DM's registrations.

Pact magic reads as `short` recovery when the sheet's caster is a warlock
with no other spellcasting class. A multiclass warlock's combined table
cannot carry the separate pact pool the 2014 rules keep (a known limit of
the sheet's single slot table, not of the grammar) — such a sheet reads as
`long`, and the DM registers the pact pool by hand.

## Transactions, not balances

Every change is one row — `spend`, `regain`, `set` (a DM correction to an
absolute value) or `reset` (a rest's grammar-driven refill) — linked to the
session event and the actor that caused it. There is no balance column
anywhere: `GET .../resources` folds the log in order and renders the
derived state, byte-stable — the same pools and the same log produce the
same bytes, forever, which is what makes *"why is the wizard out of 3rd
levels?"* answerable: the question resolves to the actual spends, in order,
each with provenance.

Permissions are one rule: **players spend their own** (spend and regain on
exactly their bound character's pools); **the DM adjusts anyone**, and the
adjustment is a visible transaction, never a silent edit. A `set` is
refused to players outright.

## Rests

A rest is a batch transaction with 2014 PHB semantics, computed by the
recovery grammar alone — no feature gets a special case:

- **Short rest** resets pools with recovery `short`.
- **Long rest** resets pools with recovery `short`, `long` *or* `dawn`
  (the night crosses dawn), and **advances the campaign clock one day**
  under reason `rest` — the schedule, the faction plans and the sim all
  answer from the new day, because everything attached to the clock
  reacts. The enemies move while the party sleeps.
- **Hit dice** are the one number the grammar does not auto-reset, because
  the 2014 long rest does not refill them: it returns *up to half of the
  total, minimum one die*. The rest engine emits that as an explicit
  `regain` transaction (half the total, floored, minimum one, capped by
  what is spent) — visible in the batch like every other change, and
  correctable by a DM `set`.

The two directions the grammar must both get right — pinned by
`TestRestRecoveryBothDirections` and the golden file:

- pact magic (`short`) **does** reset on a long rest;
- regular slots (`long`) do **not** reset on a short rest.

### Live vs proposed

- `POST /api/campaigns/{id}/rests` is the DM's live button — executed
  immediately, no gate. A DM pressing the button is the confirmation every
  machine proposal waits for.
- `POST /api/campaigns/{id}/rests/propose` is the machine path (a
  transcribed *"we take a long rest"*): the computed rest stages behind the
  review gate as one canon batch (source `rest`), one event item per
  character whose summary carries exactly what the rest would do. Deciding
  the batch through the ordinary proposals surface applies the mechanics —
  recomputed against the current ledger, since resets are idempotent and
  the PHB is the PHB however long the queue took — and moves the clock
  exactly once. A dismissed batch writes nothing.

## The surface

```
GET    /api/campaigns/{id}/characters/{eid}/resources                    balances (+ ?history=N)
POST   /api/campaigns/{id}/characters/{eid}/resources                    register a pool (DM)
PATCH  /api/campaigns/{id}/characters/{eid}/resources/{pid}              edit a registered pool (DM)
DELETE /api/campaigns/{id}/characters/{eid}/resources/{pid}              remove a registered pool (DM)
POST   /api/campaigns/{id}/characters/{eid}/resources/{pid}/transactions spend / regain (player: own; set: DM)
POST   /api/campaigns/{id}/rests                                         the live rest button (DM)
POST   /api/campaigns/{id}/rests/propose                                 a rest through the review gate (DM)
```

A player bound to a character reads and moves exactly that character's
pools; the DM reads and adjusts anyone. Rests move the clock and are
DM-only.

## Where the code lives

- `internal/ledger/ledger.go` — the pure grammar: pools, derivation, rest
  planning. No database, no wall clock, no network.
- `internal/ledger/store.go` — the rows: sync, apply, the live rest, the
  staged rest and its canon finalizer.
- Migration `0030_resource_ledger.sql` — `resource_pools`,
  `resource_transactions`, `rests`, and the `clock_advances` reason widened
  with `rest` (the same CHECK rebuild 0019 did for canon_reviews).
