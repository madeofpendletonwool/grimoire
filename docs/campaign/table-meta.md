# Table meta: custom pools, inspiration, and campaign stats

The mechanical layer's stage 11 (MAD-428, the last of MAD-417's
approved stretch): the table's own numbers. DM-invented resources on
the same grammar everything else uses, inspiration as a first-class
5e pool, and the campaign's stats — **derived, never stored**.

## Custom pools, zero special cases

A custom pool is a pool. Luck points, a homebrew class currency,
"sorrow dice" — the DM registers them with
`POST .../resources` exactly like ki or arrows
([the ledger](ledger.md) already documented this stage's promise), and
everything downstream is the grammar's own behavior:

- they **rest by their recovery** — a short-recovery custom pool resets
  beside pact magic on a short rest, a manual one never moves, the same
  `RestPlan` both directions pin for the built-ins;
- they **track as transactions** — every award and spend an append-only
  row with provenance, the balance always folded from the log;
- they **display everywhere pools display** — the resources read, the
  party board's warnings, the sheet's own surfaces.

`TestCustomPoolsRestLikeBuiltIns` is the acceptance: a custom pool and
a built-in of the same recovery, spent and rested in the same batch,
must land on the same numbers.

## Inspiration

Inspiration is the one pool the 2014 rules name for everyone: the DM
awards it for good play, a character **holds at most one** — it does
not stack — and spending it grants **advantage on one attack roll,
saving throw or ability check**.

It registers through the same grammar: `feature:inspiration`, size 1,
recovery manual — no rest refills it (2014: it lasts until spent). The
registration happens at the **first award**, and the pool is seeded
with an explicit set-to-none transaction so the fold reads "does not
hold it" from day one — a pool is born full, and inspiration is *held*,
not owned.

```
POST /api/campaigns/{id}/characters/{eid}/inspiration    award (DM)
```

- Awarding a character who already holds it is refused — the rule, not
  a policy.
- The award is a visible `set` transaction ("inspiration awarded —
  <note>"), correctable by the DM's ordinary pool tools like any other.

### The spend, through the roll flow

The roll request grows one flag:

```
POST /api/campaigns/{id}/rolls
{"formula": "1d20+5", "context": "attack", "spend_inspiration": true}
```

The flow enforces the 2014 rule as it reads:

1. **A character, holding it** — a player spends their own (the roll's
   own permission line), the DM spends anyone's; nobody spends what is
   not held, and the spend is atomic (two tabs cannot both spend the
   last inspiration).
2. **An attack roll, saving throw or ability check** — `initiative` and
   `damage` contexts refuse.
3. **A roll that does not already carry a mode** — inspiration is not
   spent to stack advantage on advantage.
4. **A formula advantage can apply to** — exactly one plain d20 term,
   the engine's own rule, checked *before* anything is spent.

Then the roll lands with advantage — the row carries
`inspiration: true` beside `mode: "advantage"` (migration 0037), the
session event's payload and summary say so, and the ledger's spend
transaction is linked to that event: one story, three rows.

### Who sees it

The board's strips carry an `inspired` mark — a boolean the whole
table may know, so word mode keeps it too. The table screen inherits
it through the board's members.

## Campaign stats, derived not stored

The logs Stages 2–5 wrote are the whole input: `dice_rolls`, the
combat journal (`combat_log` + combatants), the ledger's inspiration
spends. The fold (`internal/stats`) is pure — **identical log in,
identical stats out**, byte-stable, pinned by tests that shuffle the
rows' arrival order and diff nothing.

What it answers, fun-first:

- **Damage taken and dealt per character**, with party ratios — *"the
  barbarian took 68% of the party's damage"* — a party shot, not a
  brag board. Damage *dealt* rides the journal's source attribution
  (`source_id` on the damage write, journaled flat since this stage;
  hits the table did not attribute stay unattributed, honestly).
- **Crit counts and natural 1s** — attack rolls that kept a 20.
- **Roll distributions** — kept d20s bucketed 1–5 / 6–10 / 11–15 /
  16–20, with the mean against the fair 10.5: *"the dice ran hot."*
  Fewer than ten d20s claims nothing.
- **Most-targeted enemies** — the targets attack rolls named, ties
  broken by name, top three.
- **Inspiration spends** — how many the table burned.

### The surfaces

```
GET /api/campaigns/{id}/stats                 the campaign endcap
GET /api/campaigns/{id}/stats?session={sid}   one sitting's numbers
```

Any campaign member may read the party shot. The fold's queries are
the feed's own: **the DM's stats see secret rolls, a player's stats
cannot** — a secret roll is absent from the fold, not unprinted (the
leak test asserts the difference is exactly the secret).

The session export carries the same fold as its final section —
"The numbers" — after the log and the fights, rendered by the stats
package through a one-method interface, so the export owns no math.

## Where the code lives

- `internal/ledger/inspiration.go` — the pool, the award, the atomic
  spend, the event link.
- `internal/dice` — `Input.Inspiration` / the `inspiration` column
  (migration `0037_inspiration_rolls.sql`) / the event payload mark.
- `internal/combat` — damage source attribution in the journal.
- `internal/stats` — the pure fold, the reads, the markdown.
- `internal/server/tablemeta.go` — the routes; the roll flow's spend
  gate lives in `internal/server/dice.go`.
