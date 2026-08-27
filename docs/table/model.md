# The table data model

The contract for the Magic table core, written **before** any table code
exists, the way `docs/campaign/model.md` was for the campaign. Stage 2
(MAD-323) implements the engine against this shape in `internal/table`;
migration `0014_mtg_core.sql` owns the schema, and no package ever ships DDL
of its own. (That package name needs one note: ADR 7 said "`internal/table`
is never created" about MAD-286's D&D session tracker, which was folded into
the session layer. The name is reused here for the MTG engine per MAD-323 —
different package, same spelling. ADR 9 records the clarification.)

The campaign core is about twenty tables that are mostly edges, because the
interesting questions are relational. The table core is the opposite shape:
**one load-bearing table** — the event log — plus the game row, the seats,
and four small ledgers. Everything else about a game is a fold.

## The one idea

> A persistent, structured model of **what is on the board**, **how it got
> there**, and **who is allowed to see it** — where the authority is a
> deterministic engine, not a model, and every state change is traceable to
> the action that caused it.

The symmetry with the campaign is not decoration; it is why these are one
product:

| | D&D | Magic |
|---|---|---|
| What persists | world state | game state |
| Who decides truth | the DM | the rules engine |
| What the LLM does | proposes, cites, never writes | parses intent, never writes |
| Provenance | transcript spans | the event log |
| Scope enforcement | `awareness` → perspective | hidden zones → seat |
| The leak invariant | `spoiler_leak` | `hidden_zone_leak` |

And the constraint that decided this shape, from the plan:

> **Correction must be cheaper than manual entry.**

Undo as truncate-and-refold, the log as the primary surface, decklists as the
identification universe — every section below is downstream of that sentence.
See [Interaction](interaction.md) for the product side of it.

## Shape

```
  intent (voice / tap / typed)
        │
        ▼
     ACTION            a typed, validated proposal — from the grammar,
        │              the board UI, or (last, and never trusted) the model
        ▼
     REDUCER           pure Go. No I/O, no clock, no model.
        │
        ▼
    mtg_events         append-only, monotonically ordinaled — the only writer
        │
        ▼
      STATE            fold(events). Deterministic, total, never stored.
```

And the tables around it:

```
 mtg_games ──── mtg_seats ──── decks (4a attaches one per seat)
     │
     ├── mtg_events            the log. THE table. the fold runs over this
     ├── mtg_pending           open questions — the unresolved tray
     ├── mtg_name_resolutions  per-game identity cache (never re-infer a name)
     ├── mtg_rulings           judge log, anchored to an ordinal
     └── mtg_seat_notes        private scratch, never another seat's view

 mtg_trigger_registry          cross-game, keyed by card name (5c)
```

---

## Players and seats

A **game** is a row in `mtg_games`: an owner (the account that created it —
games are scoped per account through the existing session gate), a name, a
format, a starting life, and a lifecycle `setup → active → finished`. The row
carries **lifecycle metadata only**. Current turn, phase, step, priority
holder, who is alive — all of it is fold state and is **never stored**. This
is the same discipline that keeps `campaigns.clock` honest: the row is the
container, the fold is the game.

A **seat** is a row in `mtg_seats`: a position in turn order (1-based,
unique per game), a display name, optionally a bound `users` row (a pod;
unbound means a local seat tracked by the owner's client), and optionally an
attached deck. Attaching a deck (MAD-329) is what collapses name
identification from ~28,000 cards to the ~400 on the table, and it is what
makes library *composition* known — order never is. The seat's commander is
stored denormalized on the row, because commander damage and tax bookkeeping
need it on every cast.

> **Seats carry setup; the fold carries play.** Elimination, turn order
> position after a player leaves, commander tax paid so far — none of it is
> on the seat row. If a question is about the game rather than about its
> configuration, the answer lives in `fold(mtg_events)`.

## Zones and tracking levels

Every zone carries an explicit tracking level, and **`unknown` is a real
value** — collapsing "we don't know" into "empty" is the failure that makes a
tracker untrustworthy, and once it is untrustworthy nobody opens it again.

| Zone | Level | Notes |
|---|---|---|
| Battlefield | **tracked** | Objects, counters, modifiers, attachments, tapped and phased state. |
| Stack | **tracked** | Objects, controllers, targets, order. |
| Graveyard / exile / command | **tracked** | Public information. |
| Library | **composition, never order** | Composition is known when a deck is attached, minus everything the log has moved out. **Order is never modelled, and the engine refuses to answer as though it were.** |
| Hand | **count only** | Contents opaque unless explicitly revealed. Card identity enters the log only through a `CARD_DRAWN`/`CARD_KNOWN` event visible to that seat alone, or a `CARD_REVEALED` that makes it public. |

## Objects

An **object** is anything the log tracks by identity: a resolved card on the
stack or battlefield, a token, a copy. Object ids are minted by the engine,
monotonic per game, and stable across zone moves — object 88 on the stack is
object 88 when it resolves. An object's record (held by the fold, not a
table) is:

- **identity** — a card name resolved against the known-card universe, or a
  token spec (`{name, types, pt, ...}`) for tokens, which are never card
  references;
- **owner** and **controller** — controller changes are layer-2 modifiers,
  never writes to a stored field;
- **zone**, **tapped**, **phased**;
- **attachments** — an aura or equipment is itself an object; `ATTACHED` /
  `UNATTACHED` events make the edge, and the fold enforces that it stays
  legal (an attachment whose target left is unattached by state-based rule).

Base characteristics come from the card (via `carddb`) or the token spec.
**Everything else is a modifier or a counter.** Characteristics are computed
— base + modifiers — never stored flattened.

## Generic counters

Counters on players and objects are **named, arbitrary, unbounded**. `+1/+1`,
poison, loyalty, shield, stun, oil, energy, experience, storm — and whatever
the next set invents. A hardcoded enum is wrong on the day it ships, so the
counter name is data, not schema: `COUNTER_CHANGED {target, name, delta |
to}`. The handful of counters the engine understands structurally (poison at
ten, loyalty, storm count as casts this turn — the last is derived, not
stored) are rules over the same generic rows, not special columns.

Non-numeric player statuses — monarch, initiative, the Ring, city's blessing
— are **flags**: `FLAG_CHANGED {seat, flag, value}`, latest-wins semantics in
the fold, exclusivity (one monarch) enforced by the engine, the vocabulary
left open exactly like counters.

## Modifiers

A modifier is an ordered entry per object carrying **source + layer +
duration + delta**:

```
Giant Growth on a Grizzly Bears with a Glorious Anthem out and a +1/+1 counter:

  base                        2/2
  +1/+1 from Glorious Anthem      (layer pt_modify, source: obj 41, while_source_present)
  +1/+1 counter                   (counters — applied after pt_modify, before pt_switch)
  +3/+3 from Giant Growth         (layer pt_modify, source: obj 93, until_end_of_turn)
                              ────
                              6/6
```

- **Layer** follows CR 613's seven layers — `copy, control, text, type,
  color, ability, pt_set, pt_modify, pt_switch` — applied in order, timestamp
  within a layer. Counters are not modifiers; they apply after `pt_modify`
  and before `pt_switch`, which is exactly where the trace above renders them.
- **Duration** is one of `until_end_of_turn | while_source_present |
  permanent`. Expiry at cleanup is computed by the fold from `TURN_ENDED`
  anchors — no expiry events, because derived data stored twice is a drift
  bug. A `while_source_present` modifier whose source leaves is removed by
  the engine asserting `MODIFIER_REMOVED` (that is *not* derivable cheaply,
  and the log should show it).
- **Source** is the object or card the effect came from, so "why is this
  creature 7/7?" is a fold-render of the modifier stack — a trace, not an
  inference and not a model call.

The critical part, and it is the scope line
([ADR 11](../decisions.md#adr-11-card-behaviour-is-declared-never-simulated)):
**modifiers are attached, never derived from oracle text.** A player declares
"Anthem gives my creatures +1/+1", or the model proposes it once and a human
confirms, and the mapping is then cached in the trigger/effect registry by
card name so the second Anthem anyone ever plays is automatic. Grimoire does
not read *Glorious Anthem*'s text and simulate a static ability.

## Attachments

Auras and equipment are objects; the attach relation is an event stream
(`ATTACHED {obj, to}`, `UNATTACHED {obj}`), and the fold keeps the edge.
Attacking a planeswalker, fortifying a land, and Bestowing a creature are all
the same edge. Re-equipping is `UNATTACHED` + `ATTACHED` with a cause, so the
log reads correctly.

## The Action taxonomy

The input side. Actions are typed values; the grammar (MAD-330), the board UI
(MAD-327), and the model fallback (MAD-331) all produce the *same* type, and
the reducer is the only consumer. Each action carries the acting seat, and
each lists its default ladder disposition
([Interaction](interaction.md#the-confirmation-ladder)).

| Group | Actions | Notes |
|---|---|---|
| Setup | `CREATE_GAME`, `SEAT_PLAYER`, `ATTACH_DECK`, `START_GAME`, `END_GAME` | Setup actions write rows and the first log events; decks attach per seat, commander recorded. |
| Turn & priority | `ADVANCE`, `PASS_PRIORITY` | `ADVANCE` moves to the next step/phase/turn per CR order; "pass", "go", "resolves" parse here. |
| Zones & objects | `PLAY_LAND`, `CAST`, `ACTIVATE`, `MOVE_ZONE`, `CREATE_TOKEN`, `TAP`, `UNTAP`, `SET_PHASED`, `ATTACH`, `DETACH` | One land drop check per turn; `CAST` from command zone applies commander tax (derived from prior casts of that commander). |
| Combat | `DECLARE_ATTACKERS`, `DECLARE_BLOCKERS`, `RESOLVE_COMBAT` | Assignment order and keyword arithmetic (first/double strike, trample, deathtouch, lifelink, protection, indestructible) are engine-owned (MAD-325). |
| Numbers | `ADJUST_COUNTERS`, `SET_COUNTERS`, `CHANGE_LIFE`, `DEAL_DAMAGE`, `SET_FLAG` | `SET_*` exist so a manual board edit and an engine action share one path; sources attach where spoken ("Lightning Bolt → Collin"). |
| Hidden zones | `DRAW`, `MILL`, `REVEAL`, `LOOK` | `DRAW` with identities only when spoken/revealed; `LOOK` produces seat-visible `CARD_KNOWN` events. |
| Declared effects | `DECLARE_EFFECT`, `ADD_MODIFIER`, `REMOVE_MODIFIER` | The scope line's constructive half: known shapes decompose into structured events; the rest lands as `EFFECT_DECLARED` with prose. |
| Correction | `REWIND` | A **writer operation, not a reducer action** — it produces zero events by definition. Amend = `REWIND` + a new action. |

A rejected action produces **zero events** and an error. That invariant is
what makes optimistic application safe to build on.

## The Event taxonomy

The output side; the rows `mtg_events` stores. ★ marks the **structural**
kinds — the events the trigger registry (5c) can fire on, chosen because they
are fixed by the Comprehensive Rules and do not depend on card text.

| Group | Kinds | Notes |
|---|---|---|
| Lifecycle | `GAME_STARTED`, `GAME_ENDED` | Config echoed into `GAME_STARTED` so the log is self-describing. |
| Turn | `TURN_STARTED` {turn, seat}, `STEP_ENTERED` {phase, step} ★, `TURN_ENDED` | `TURN_ENDED` is the expiry anchor for `until_end_of_turn` modifiers and marked damage. Steps include the combat five and cleanup. |
| Priority & stack | `PRIORITY_PASSED`, `STACK_PUSHED` {mode: cast \| activated \| triggered, controller, targets}, `STACK_RESOLVED` | One object per push; the stack is its order in the fold. |
| Zone moves | `OBJECT_CREATED` {card \| token spec, controller, owner, zone}, `ZONE_CHANGED` {obj, from, to, cause}, `LAND_PLAYED` ★, `CREATURE_ETB` ★, `DIED` ★ {obj, cause}, `CAST` ★ {seat, card, targets} | The starred kinds are the informative ones the log renders by name; generic moves use `ZONE_CHANGED` with a cause (`sacrifice`, `destroy`, `bounce`, `exile`, `mill`, `counter`, …). `DIED` causes include `sba_zero_toughness`, `sba_lethal_damage` — engine-asserted so viewers never re-run state-based logic. |
| Combat & damage | `ATTACKERS_DECLARED` ★ {assignments}, `BLOCKERS_DECLARED`, `DAMAGE_DEALT` {source, target, amount, combat?}, `DAMAGE_MARKED` {obj, amount} | Damage on creatures persists until cleanup, hence `DAMAGE_MARKED`. Commander damage derives from `DAMAGE_DEALT` rows whose source is a commander — it is never a separate counter to drift. |
| Numbers | `LIFE_CHANGED` {seat, delta \| to, source}, `COUNTER_CHANGED`, `FLAG_CHANGED` | Damage produces both `DAMAGE_DEALT` and its `LIFE_CHANGED` consequence, asserted, so viewers fold trivially and prevention stays an engine concern. |
| Object state | `TAP_CHANGED`, `PHASE_CHANGED`, `ATTACHED`, `UNATTACHED`, `MODIFIER_ADDED`, `MODIFIER_REMOVED` | Control changes are layer-2 `MODIFIER_ADDED` — there is no `CONTROLLER_CHANGED` kind, on purpose. |
| Triggers | `TRIGGER_FIRED` {source, spec} | Into the trigger queue; it reaches the stack as `STACK_PUSHED` at the next priority grant, per APNAP (MAD-325). |
| Hidden information | `CARD_DRAWN` {seat, count, card?}, `CARD_KNOWN` {seat, cards, from}, `CARD_REVEALED` {seat, zone, cards} | The identity-bearing kinds. Visibility (below) decides who may fold them. |
| Declared effects | `EFFECT_DECLARED` {source, spec, prose} | Everything the scope line records without simulating. |

`kind` is deliberately **not** CHECK-constrained in SQL: the vocabulary lives
here and in the Go type system, the reducer is the single writer, and the
taxonomy grows with the engine. The stable, small vocabularies (`visibility`,
`source`, statuses) are CHECKed.

## The fold

```
apply(state, action) → ([]Event, error)     pure; rejected action → zero events
fold(events)         → State                deterministic, total
fold(events[:n])     = the state at ordinal n, for every n
```

Invariants — these are the acceptance properties MAD-323 tests:

1. **Ordinals are per-game, start at 1, contiguous.** Assigned by the single
   writer at persist time, in one transaction with the `mtg_games.updated_at`
   bump. A gap means corruption; a client's "replay from last ordinal seen"
   only works because contiguity holds.
2. **The log is append-only between rewinds.** Rewind to *N* is
   `DELETE WHERE ord > N` plus a refold, in one writer transaction, announced
   on the stream with a control frame so every attached client re-folds. That
   is the whole undo story: no compensating transactions, ever.
3. **State is never stored as truth.** Views, traces, and the board pane are
   folds — of everything, or of what a seat may see. A materialized
   projection, if one is ever worth it, is a cache with this section as its
   contract.
4. **Consequences fixed by rule are computed by the fold; happenings are
   events.** Expiry of `until_end_of_turn` is derivable from `TURN_ENDED`, so
   it is not an event. A death by state-based action depends on the whole
   modifier stack at that instant, so it is asserted (`DIED`, cause `sba_*`)
   and viewers never re-run CR 704.
5. **`cause` rides on the events.** Every event row carries the Action that
   produced it (`source`, confidence, disposition), so a log entry is
   self-contained and amend (3c) prefills from the row itself.

## Hidden information and visibility

Every event row carries `visibility` — `public` (default) or `seat` — and a
`visible_seat` when it is seat-visible. The rule:

> A seat-visible row is invisible to every other seat **as a row**. Its
> public consequences, if any, are separate public events.

A draw is therefore two events: `CARD_DRAWN` public (hand count moves — hand
size is public information) and, if the identity was spoken or typed where
only that seat's screen heard it, the identity in a seat-visible
`CARD_KNOWN`. A card revealed at the table is one public `CARD_REVEALED`.
The owner's account (the tracker who created the game) is entitled to
everything — the DM analog. The per-seat stream is a WHERE clause, never a
prompt instruction; see
[ADR 13](../decisions.md#adr-13-hidden-zones-are-authorization-in-sql-not-instruction).

The engine's own fold includes seat-visible rows (it must, to serve a seat
its own view and to answer that seat's probability questions), exactly as the
campaign's store holds secrets no player retrieval can reach. The gate is the
query, and 6a's reflection leak test is the hard CI version of it.

---

## Tables

### The game and its log

| Table | Columns | Notes |
|---|---|---|
| `mtg_games` | `id, owner_id, name, format, starting_life, status, settings, created_at, updated_at, started_at, ended_at` | The container. `format` is open vocabulary — Commander first, the engine is format-agnostic underneath; `settings` JSON carries variant config the way `campaigns.settings` does. `status` is `setup \| active \| finished` — lifecycle only. **Prefix is `mtg_`**: `game_sessions` already means a D&D session and `sessions` already means a login session. |
| `mtg_seats` | `id, game_id, position, user_id, name, deck_id, commander, starting_life, joined_at` | Turn order is `position` ascending. `user_id` nullable (local seat); `deck_id` attaches the known-card universe (4a) and sets library composition; `commander` denormalized for damage/tax bookkeeping. **Setup facts only — no play state.** |
| `mtg_events` | `id, game_id, ord, kind, actor_seat, source, cause, payload, visibility, visible_seat, created_at` | **The only writer of state** ([ADR 9](../decisions.md#adr-9-the-event-log-is-the-only-writer-state-is-a-fold)). `ord` contiguous per game; `kind` from the taxonomy above; `source` is `tap \| grammar \| llm \| voice \| manual \| system` (audit and telemetry — how did actions get entered); `cause` is the Action JSON that produced the row; `visibility` + `visible_seat` enforce hidden zones ([ADR 13](../decisions.md#adr-13-hidden-zones-are-authorization-in-sql-not-instruction)). `UNIQUE (game_id, ord)` is the multiplayer story: one writer, ordinal replication, no conflict resolution. |

### The ledgers around the log

| Table | Columns | Notes |
|---|---|---|
| `mtg_pending` | `id, game_id, question, options, context, status, answer, answered_seat, created_at, answered_at` | The unresolved tray (4c). A clarification the one-tap rule could not reduce to tappable answers is parked here, `open`, and play continues. On rewind, an open question whose context ordinal was truncated is auto-dismissed. |
| `mtg_name_resolutions` | `game_id, spoken, card_name, method, confidence, resolved_at` | The per-game identity cache (4a/4c): `spoken` (normalized) → canonical name, `method` `deck_exact \| deck_fuzzy \| global \| manual`. The same mumble is **never re-inferred, never re-billed**. Survives rewind — a corrected card is still the resolution of that mumble. No FK to `cards(name)`: the card index is bulk-replaced on re-index; this cache is game history. |
| `mtg_trigger_registry` | `card_name, event_kind, effect, origin, confirmed_by, created_at, updated_at` | Cross-game, install-wide (5c): card knowledge is universal, like the rules corpora. `event_kind` is a structural kind (`LAND_PLAYED`, `CREATURE_ETB`, `CAST`, `ATTACKERS_DECLARED`, `DIED`, `STEP_ENTERED` with a step condition in `effect`); `origin` is `declared` (a human typed it) or `confirmed` (model-proposed, human-confirmed) — the declared-vs-simulated line again. |
| `mtg_rulings` | `id, game_id, ord, ruled_by, note, created_at` | The judge log (6b): a ruling anchored to the ordinal it concerns. **Survives rewind** — a human record is never clobbered by a truncate, the same semantics as a decided canon review. |
| `mtg_seat_notes` | `game_id, seat, body, updated_at` | Private per-seat scratch (6a). Editable, latest-wins, and never enters another seat's view or another seat's prompt. |

## Migrations

All table schema ships as one goose migration, `0014_mtg_core.sql`, under
`internal/migrate/migrations/` — additive only, so it applies cleanly on a
fresh database and touches nothing on a populated one. `internal/table` (and
every later table-side package) creates no tables in `New()`, following the
pattern `internal/campaign` set. New event kinds and counter/flag names are
data; new columns and tables are migrations.
