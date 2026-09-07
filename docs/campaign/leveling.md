# Leveling and reconciliation (MAD-424)

Stage 7 of the mechanical layer (MAD-417): XP, level-ups through the
review gate, and the post-session truth pass. The pieces:

- `internal/progression` — the 2014 class tables as pure Go: the
  XP-to-level advancement table, per-class hit dice, saves, features
  (with each class's ASI schedule), the shared full-caster slot table,
  the paladin/ranger half-caster tables and the warlock pact table.
  Pinned by unit tests against the books; no database, no I/O.
- `internal/leveling` — the engine and store: the level-up diff computed
  from those tables, the XP award split, the campaign's mode config, the
  staged level-up behind the canon review gate, and the reconciliation
  pass. Migration `0035` adds `xp_awards` and `level_ups`.

## The mode is data

`campaigns.settings["leveling"] = {"mode": "xp" | "milestone"}` — the
board-config pattern (`internal/board/config.go`). XP is the default. In
milestone mode the award endpoint refuses ("this campaign does not track
XP") and level-up staging asks for no XP: the DM proposing the level IS
the milestone. Strict parse, default on unreadable.

## XP awards: the builder's oracle

`POST /api/campaigns/{id}/encounters/{eid}/award` (DM) prices an
encounter with `encounter.Evaluate(...).TotalXP` — the RAW total, every
monster's XP re-derived from its CR server-side; the adjusted number is
difficulty-only and never a currency. The total splits across the
characters named (all live pcs when none are), per the 2014 DMG's
divide-across-the-party rule, remainder distributed whole in entity-id
order. Each character's share bumps the sheet's own `xp` field and
writes an `xp_awards` row carrying both the encounter's total and the
sheet's total as the award left it — the provenance the reconciliation
pass reads back. The encounter moves to status `run` (the writer that
status was reserved for), and a second award for the same encounter
refuses.

## Level-ups: the gate is the gate

There is no ungated level-up path. `POST
/api/campaigns/{id}/level-ups/propose` (DM) computes the diff from the
2014 tables and stages it as a canon batch (source `level_up`): one
event item whose payload is the whole diff — class level and total
level, new features, new save proficiencies (a first level in a class
only), slot maxima gains, max HP (the fixed average, die/2+1, plus the
CON modifier; an undeclared CON proposes +0 and says so), and the
advancement numbers. Deciding happens at the ordinary batch surface
(`POST /api/campaigns/{id}/proposals/{bid}/decision`); the level-up
finalizer then recomputes the diff against the CURRENT sheet and applies
it once — `staged -> applied` on the `level_ups` row, idempotent on
re-runs (a sheet already at the proposed class level applies nothing).
Pool maxima and the projection re-sync automatically through the
sheet-write path.

In xp mode the sheet must carry the advancement table's threshold for
the level proposed. Multiclassing is adding a level to an existing class
or a first level in a new one — but multiclass SLOT tables are combined
by the 2014 rounding rules the sheet's single slot table cannot carry,
so a multiclass diff proposes the class's own numbers and names the
combined-table question for the reviewer rather than computing a wrong
total (`slots_note`).

Player-confirmed level-ups (the "as the campaign allows" clause) are a
portal-stage product decision; today the DM's acceptance stands in for
the table's.

## Reconciliation: the canon engine, applied to numbers

`POST /api/campaigns/{id}/reconcile` (DM) is the post-session pass —
deterministic, read-only until the gate says otherwise:

- `pool_bounds` — a bounded pool whose derived balance folds outside
  0..size (a spend the validated paths could not have written, or a
  sheet whose maxima shrank below what was already spent). Proposes the
  clamping `set`.
- `sheet_xp` — a sheet whose XP total sits below the newest award's
  recorded total. Proposes the recorded total.

Findings stage as a canon batch (source `reconcile`), one event item
each — the audit trail in the campaign log. The DM accepts, amends or
discards; the finalizer applies accepted corrections as the visible,
attributed writes they are (a ledger `set`, a sheet XP write). Nothing
auto-applies, ever — the store tests assert it twice.

## Surface

- `GET  /api/campaigns/{id}/leveling` — mode + every pc's level, XP,
  next threshold and eligibility (DM).
- `PUT  /api/campaigns/{id}/leveling/settings` — the mode (DM, owner).
- `POST /api/campaigns/{id}/encounters/{eid}/award`, `GET .../awards`.
- `POST /api/campaigns/{id}/level-ups/propose`, `GET .../level-ups`.
- `POST /api/campaigns/{id}/reconcile`.

All DM-only. Golden files: `internal/leveling/testdata/levelup_diffs_golden.json`
pins one single-class path (wizard 4→5) and one multiclass path (fighter
8 adds wizard 1); regenerate with `-update-golden` only after an
intentional table change.
