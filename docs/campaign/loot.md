# Campaign-aware loot: the party's power curve, and "this one is too good for the fighter"

A loot roll that does not know the party is a random-item generator, and
those already exist. The whole value of this surface is the app knowing
that the fighter already has a +1 longsword, that the wizard has found
nothing in nine sessions, and that the party is one tier past where its
treasure says it is. So the loot surface is three reads over records the
campaign already has, plus a hoard generator whose **amount** is the
DMG's and whose **choices** are the campaign's. Everything is DM-only per
ADR 2 — the party sheet and the event log are DM material, and there is
no filtered version of either that would be safe to hand a player.

## The three reads

### The distribution — who has received what, over time

`GET /api/campaigns/{id}/loot/distribution`. Most "the rogue never gets
anything" problems are invisible until they are plotted, and this is a
plot the app can draw from data it already has.

The view is **derived from the event log and the graph, not from a
separate ledger**. Every holding it shows comes from a record another
surface already wrote, folded in this preference order:

1. **A dated relationship** — `pc owns item` (or its inverse), dated by
   `since_event` to the event's play position. This is a hand-out with a
   place in the log.
2. **A possession fact** — the same predicates the canon engine's
   continuity checks read (`carried_by`, `held_by`, `bears`, `holds`,
   `has`, `keeps`, `possessed_by`), skipping superseded and proposed
   facts exactly as the engine does. The shared vocabulary lives in one
   place (`campaign.PossessionPredicates`), which is why the distribution
   reconciles with `grimoire canon check` by construction rather than by
   luck: a hand-out the engine sees is a hand-out the fold sees.
3. **The party block's `items` list** (MAD-378) — the pc's declared
   sheet, undated.

One (pc, item) pair is one row: the most-dated record wins, so a sword
named by both a relationship and a sheet is never counted twice. Dated
rows sort in event-log order; undated rows sort last and read as
*declared*, never as *received*. The party-level note names whoever has
received nothing.

### The power curve — the party against the game's expectation

`GET /api/campaigns/{id}/loot/power-curve`. The party's held items —
classified by rarity through the item catalog, counted as *unclassified*
when the catalog cannot name a rarity — against the expectation
Xanathar's Guide p.135 formalised from the DMG: a party gathers about a
hundred items by 20th level, 11 by the end of tier 1, 45 by the end of
tier 2, 75 by tier 3, 100 by tier 4. The per-rarity expectation is the
minor and major tables merged, since a holding's rarity is what the
catalog classifies and minor/major is table membership the SRD corpus
does not carry.

The read compares the party's total against the band — the previous
tier's end as the floor, this tier's end as the expectation — and the
verdict carries the numbers: *"under-equipped: 2 of the 45 items the
party would have gathered by this tier's end (4%)"*. The arithmetic
lines above the verdict name the tier derivation, the rarity split and
the expectation's source, so the DM can argue with the number instead of
being told it. Items the catalog cannot classify still count in the
total and are listed separately — the app says what it is unsure about
rather than hiding it.

### The concentration warning — "this item complements the fighter too strongly"

That roadmap example is computable, and this surface computes it: an
item whose tags align with a pc who is already the party's strongest on
that axis, in a party where another pc has received nothing comparable.
The axes are the item catalog's derived tags (`offensive`, `defensive`,
`save-boost`, and the rest), the same vocabulary the picker filters by.
The warning carries its arithmetic — *"the fighter already holds 3 of
the party's 4 offensive items and the rogue has received nothing
comparable — consider replacing this item or reassigning it"* — and it
is a **suggestion with its reasoning attached, never a refusal to
generate**. An even party is silent. A tie at the top is not dominance.
A party that declares no holdings is silent. The DM overrules it and the
tool says nothing further.

## Generating a hoard

`POST /api/campaigns/{id}/loot/hoard` — tier and context in, a hoard
out. Nothing is written; the hoard is a preview until it is placed.

**The amount is the book's.** Coins, gems and art objects roll the DMG's
four hoard tables verbatim (DMG p.133), transcribed as data in
`internal/loot` — the d100 row names the mundane bundle and the magic
item rolls, and the coin block is the book's expressions. The response
carries the rolled value next to the band the tables can produce, so
"3,412 gp" arrives as "3,412 gp of the 56–936 gp band tier 1 can roll".

**The choice is campaign-aware.** The DMG's magic item tables A–I are
item lists the SRD mirror has no membership for, so each table here
rides its printed rarity profile (A leans common and uncommon; I is
legendary), and the actual pick comes from the catalog — SRD and the
campaign's own homebrew alike, the same overlay the item picker serves.
With a party read, items whose tags sit where the party is thinnest win
the pick; rarity is never a party decision, so the tier stays the
book's. Every line carries **why it was chosen**, and any line can be
**rerolled individually without rerolling the hoard**: the hoard is a
pure function of (request, seed), each line's dice run on a source
derived from the seed and the line's key, and a reroll re-derives one
line with a fresh nonce. Nothing is stored between generate and reroll.

**Narrative weight, when there is a spine.** Generate with an `act_id`
and the hoard offers *the act's item* rather than a roll for the
headline slot: an item entity tied to the act's quests or scenes. If
the act names no item, the hoard says so and proceeds as rolled.

**Degraded mode.** A campaign whose pcs declare no levels still
generates: the tier must come from the request, the read is tier-only,
and the response says so — *"no pc declares a level — the hoard is
tier-only; no party read was applied"* — rather than inventing a party.

## Placing a hoard

`POST /api/campaigns/{id}/loot/hoard/place` stages a proposal batch
(MAD-359): the generated items become `item` entities, the hand-out
becomes an event with the party as participants, and the event depends
on the items so an accept cannot half-land. **Nothing is written until
the DM approves the batch** — consistent with every other generated
write in the app, per ADR 3. Rerolling, overruling a warning, or walking
away writes nothing at all. The act's own item is never staged twice: it
is already a campaign entity, and it rides the hand-out instead.

## What this is not

No gold-piece economies and no shop inventories. No encumbrance. No
attunement tracking during play (MAD-318's). No player-visible
inventory — the player portal is MAD-319's, and this surface is DM-only
per ADR 2. No pricing of magic items: the DMG gives no prices, so the
hoard's value is coin, gems and art objects, and the items stay
unpriced.
