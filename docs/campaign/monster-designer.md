# The monster designer: a brief becomes a statblock the calculator agrees with

*"A CR 7 undead boss that fights like a battlefield commander"* — the
monster designer turns that into a complete statblock: traits, actions,
legendary actions, tactics, lore and an encounter role — and then checks
its own work. `statblock.ComputeCR` (the DMG challenge-rating calculator
MAD-379 built) runs on the result, and the draft is revised until the
maths agrees, or surfaced with the disagreement shown. A homebrew monster
that the calculator cannot certify is still savable; hiding that would
make the number worse than not having one.

## The loop is the feature

1. **The server derives the envelope before the model is asked anything.**
   `statblock.EnvelopeFor` reads the same DMG p. 274 tables ComputeCR
   prices against: the expected hit points, armor class, damage per round,
   attack bonus and save DC bands for the requested CR, the proficiency
   bonus, and the legendary-action budget (three points a round) a
   legendary creature at that CR is priced with.
2. **The model designs inside the envelope** through the structured
   generation harness every generator uses: flat declared fields,
   validated vocabularies for size, type and role, and action prose in
   the exact printed SRD shapes the deterministic parser reads
   (`Melee Weapon Attack: +7 to hit, reach 5 ft., one target. Hit: 13
   (2d8 + 4) slashing damage.`).
3. **ComputeCR runs on the assembled draft.** A miss goes back with its
   specific wording — *"offensive CR 4 against a defensive CR 9; damage
   per round is 22 short"* — and the model revises the half that is off,
   keeping the identity it wrote. One revision pass, then the draft
   stands. A draft that still disagrees is returned with the shortfall
   attached, never silently retuned and never rejected for a miss the DM
   can see.

## Homebrew lives beside the SRD, never inside it

Designed monsters are saved to their own table, `homebrew_monsters`
(migration `0027`) — `id, owner_id, campaign_id, name, slug, statblock,
requested_cr, computed_cr, computed_detail, tactics, lore,
encounter_role, source, created_at, updated_at`. The `statblock` column
is the full structured statblock; `computed_detail` is the calculator's
whole `Rating` — both halves, every adjustment with its reason, the
confidence.

The table is deliberately **not** the `bestiary` mirror. The mirror is a
cache of an upstream sync (`Catalog.Sync` replaces its contents
wholesale); homebrew written there would be destroyed on the next refresh
and would silently become "SRD" to every surface that trusts the mirror.
Instead the catalog's `Filter`, `Lookup` and `Search` take an explicit
homebrew overlay the caller loads, so a designed creature:

- appears in the builder's pool and search, competing in the same tiers
  under the same CR windows,
- resolves by name when a design's roster names it (the overlay is asked
  first — where a homebrew name collides with an SRD one, the DM's own
  design is the more specific answer about their table, in their scope
  only),
- renders its full statblock from the statblock endpoint,
- is priced by the existing XP arithmetic off its computed CR,

— always tagged `homebrew`, in the wire shape and in the prompt.

**There is no path to save a homebrew monster without its computed CR.**
`HomebrewStore.Save` runs `ComputeCR` itself over the statblock it is
handed and ignores any computed value the caller supplies; the label a
homebrew monster carries is always this server's arithmetic. The save
endpoint also accepts `source: "hand"` for a statblock the DM typed in
themselves, under the same contract.

## Campaign-aware, when there is a campaign

With a `campaign_id` the brief plays against the campaign's own material:
the generator's structure carries the campaign's factions, NPCs and
places, so *"a soldier of the Duke's faction"* is written against the
real graph. A saved campaign-scoped monster stops `stat_block_unresolved`
from firing on a planned encounter that uses it — the continuity engine
resolves a campaign's roster against that campaign's own designs first.

Placing a designed monster *into* the campaign — making it a `creature`
entity the graph can reference — goes through the proposal batch like
every other generated write: `POST /api/monsters/{id}/place` stages one
entity behind the review gate, and nothing is written until the batch is
accepted. A DM can design a monster and never touch the graph.

## The API

| Route | What it does |
|---|---|
| `POST /api/monsters/generate` | The design loop. Body: `brief`, `cr` (printed form, e.g. `"7"`, `"1/4"`), `legendary`, optional `campaign_id` (DM-gated). Returns the draft, the envelope, the computed rating and any shortfall. Needs the model. |
| `POST /api/monsters` | Save a draft (or a hand-entered statblock). Recomputes the CR server-side, always. Optional `campaign_id` scopes the row. |
| `GET /api/monsters?campaign=` | The caller's shelf, newest edit first; a named campaign's own monsters lead. |
| `GET /api/monsters/{id}` / `DELETE /api/monsters/{id}` | One monster, by id. |
| `POST /api/monsters/{id}/place` | Stage the campaign placement as a proposal batch. DM only. |

## Out of scope

The linter's *"is this legal / is this ambiguous"* pass is its own issue
and consumes this one. Player-facing anything. Importing homebrew from
third-party formats.
