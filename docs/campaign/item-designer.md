# The item designer: a design compared against the shelf, never a rarity computed

*"A +1 longsword that remembers being a blacksmith's pry bar"* — the item
designer turns that into a structured design — type, base item, bonus,
attunement, charges and recharge, and every mechanical effect in the
game's own vocabulary — and then places it against the SRD's whole magic
item shelf. What it does **not** do is tell you the rarity. A CR
calculator has a procedure to check against; the DMG gives magic items
rarity guidance, not a formula, and any tool claiming to *compute* an
item's rarity is inventing authority. So the validation here is
explicitly comparative, and says so in every shape it travels in.

## What "validated" honestly means

1. **Hard structural rules, which are real.** Attunement grammar (a
   condition is meaningless without the requirement; a potion is drunk,
   never attuned), the charge-and-recharge grammar (charges state their
   recovery, in the printed shapes — `1d6+1 daily at dawn`), item types
   that must name a base item (a weapon or armor design is meaningless
   without the thing it modifies — *a +1 what?*), and the vocabulary rule:
   every mechanical effect is expressed in the game's own terms. *"The
   target must make a DC 13 Constitution saving throw"* is checkable;
   *"the target is weakened"* names nothing the server can compare,
   filter or loot against, and is refused.
2. **Rarity bands derived from the corpus.** Bonus size, charge counts,
   recharge rates, save DCs and die expressions, distributed across the
   real SRD items at each rarity. A designed item is placed against that
   distribution as a set of checkable claims: *"every SRD item carrying a
   +3 bonus is Legendary — nothing at Uncommon or below reaches it."*
   That claim can be verified by looking. A computed rarity score cannot.
3. **Nearest neighbours from the corpus.** The three official items
   closest to the design — same kind of item, same base, shared derived
   tags, metrics in the same neighbourhood — each carrying what it
   shares, so the DM can judge rarity themselves with the shelf open.

The response shape is the honesty: bands, notes and neighbours — and no
field anywhere that computes, implies or labels a rarity. The `rarity`
the design carries is the DM's own label, echoed, never judged.

## The catalog underneath

The SRD shelf is mirrored into the shared database (`magic_items`,
migration `0028`) — the exact precedent the bestiary set for the
encounter builder, because "choosing what fits here needs the whole shelf
at once": the rarity bands and the nearest-neighbour read are
distributions over every item, not one lookup at a time. The mirror is
replaced wholesale on sync, held in memory for filtering, refreshed when
missing or stale, with the same background-refresh-on-cold-start
behaviour — a cold start serves immediately and fetches behind.

Items carry derived tags (`weapon`, `armor`, `consumable`, `offensive`,
`defensive`, `utility`, `movement`, `save-boost`, `damage-rider`), the
vocabulary read off the corpus the way the creature tags were: a tag is
present because the item's category or its own text says so, never
because a model guessed. Charge counts and recharge rules are parsed from
the printed text too — the API carries no structured charge field, so a
charge count in the catalog is a claim the printed item actually makes.

## Homebrew lives beside the SRD, never inside it

Designed items are saved to their own table, `homebrew_items` (migration
`0028`) — `id, owner_id, campaign_id, name, slug, design,
requested_rarity, tags, notes, source, created_at, updated_at`. The
`design` column is the full structured design; `requested_rarity` is the
DM's own label. There is no `computed_rarity` column and must never be
one.

The table is deliberately **not** the `magic_items` mirror. The mirror is
a cache of an upstream sync (`Catalog.Sync` replaces its contents
wholesale); homebrew written there would be destroyed on the next refresh
and would silently become "SRD" to every surface that trusts the mirror.
Instead the catalog's `Filter`, `Lookup` and `Search` take an explicit
homebrew overlay the caller loads, so a designed item:

- appears in the picker's search and the filter browse, leading the list
  and flagged at every hop,
- resolves by name when a brief or a loot pass names it (the overlay is
  asked first — where a homebrew name collides with an SRD one, the DM's
  own design is the more specific answer about their table, in their
  scope only),
- competes in the same type and rarity gates under the same tags,

— never presented as SRD.

**There is no path to save an item that fails the structural rules.**
`HomebrewStore.Save` runs the validation itself over the design it is
handed; bad attunement, an ungrammatical recharge and a vocabulary-less
effect are all refused, with the specific rule named. The save endpoint
also accepts `source: "hand"` for a design the DM entered themselves,
under the same contract.

## Campaign-aware, when there is a campaign

A homebrew item can carry a `campaign_id` — the artefact a faction wants,
the sword an NPC carried — so the campaign's own material is designed
against the graph it belongs to. Placing a designed item *into* the
campaign — making it an `item` entity the graph can reference — goes
through the proposal batch like every other generated write:
`POST /api/items/homebrew/{id}/place` stages one entity behind the review
gate, and nothing is written until the batch is accepted. Designing
without placing writes nothing to the campaign.

## The API

| Route | What it does |
|---|---|
| `GET /api/items?q=` | The SRD shelf: name search for the picker, or a filter browse (`type`, `rarity`, `tag`) without a query. The caller's homebrew leads either way; `campaign=` scopes which designs ride along. |
| `POST /api/items/design` | The comparison. Body: the design (flat). A structurally broken design is rejected — 400, the rules it broke named. A sound one is placed against the shelf: metrics, bands, notes, neighbours. No model needed. |
| `POST /api/items/homebrew` | Save a design (or a hand-entered one, `source: "hand"`). Runs the structural rules server-side, always. Optional `campaign_id` scopes the row. |
| `GET /api/items/homebrew?campaign=` | The caller's shelf, newest edit first; a named campaign's own items lead. |
| `GET /api/items/homebrew/{id}` / `DELETE /api/items/homebrew/{id}` | One item, by id. |
| `POST /api/items/homebrew/{id}/place` | Stage the campaign placement as a proposal batch. DM only. |

## Out of scope

Deciding *who gets what and when* — that is the loot generator's job, and
it is the stage after this one. Pricing items in gold. Character sheets
and inventory management. Any feature that claims to know an item's
rarity.
