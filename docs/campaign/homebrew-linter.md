# The homebrew linter: a reviewer, not a referee

The roadmap called this *"the strongest reuse of the existing grounded
rules corpus"*. That framing needs correcting before the feature makes
sense, because the corpus is not what it sounds like:

- `internal/rulings` is **Scryfall — Magic: the Gathering**. It has
  nothing to do with D&D and is not a rules engine for anything.
- The D&D corpus in `internal/index` is the 5e SRD **as indexed prose**:
  FTS5 documents plus a reader tree, excellent for retrieval, with no
  machine-checkable model of any mechanic behind it.

Nothing in the app can decide whether a feat creates an infinite loop,
because nothing in the app models what a feat *does*. A linter that
claimed to would be the most confidently wrong surface Grimoire ships.
So the linter runs exactly the three checks the app genuinely can stand
behind, and says plainly — here, and in every response shape — that it
is a **reviewer and not a referee**.

## What it checks

### 1. Numbers, against the maths that exists

- **A monster** is run through `statblock.ComputeCR` (see
  [Statblocks & CR](../development/statblock.md)) — the DMG p. 274
  procedure, with the offensive/defensive split and the *specific*
  shortfall. "Your CR 7 boss computes to CR 4 — the defensive half
  holds, the offensive half is 22 damage per round short" is a finding.
  Where the parse was incomplete, the CR figure carries its confidence
  and says which actions did not parse.
- **An item** is placed against the corpus rarity distribution (the
  item designer's bands): *"every SRD item carrying a +3 bonus is
  Legendary — nothing at Uncommon or below reaches it."* These are
  checkable claims with the counts as their arithmetic, never a
  computed rarity.

### 2. Structure and vocabulary, against declared schemas

Deterministic rules over the structured forms the monster and item
designers already enforce — they run with no model and no network:

- A save against an ability the game does not have; a damage type that
  is not one of the game's thirteen; a skill, size, creature type or
  movement mode outside the game's own sets.
- Usage grammar that does not parse (`"whenever it feels like it"` has
  no cost the arithmetic can read).
- An effect whose declared outcome names no game condition, damage type
  or dice — "the target is weakened" is not a thing the game can price.
- **A recharge cycle with no cost** — the honest, bounded version of
  "infinite loop": an ability that restores the resource it spends,
  detectable because the resource grammar is declared. The scope is
  deliberately narrow and named in every finding: actions only (never
  traits — the SRD's regeneration is a published mechanic, and
  flagging it would be a false positive by construction), unlimited-use
  actions only, and restoration coupled to damage dealt ("equal to the
  damage taken" — the vampire's bite) is priced by the damage itself
  and stays silent. This is a *narrow structural check*, not general
  loop detection, and the wording says so.

### 3. "What is this closest to?", which is what retrieval is for

The strongest genuine use of the indexed corpus. The linter retrieves
the nearest official mechanics from `internal/index` — the spell, item
or monster the homebrew most resembles — and puts them side by side
with the numbers each one uses. *"This is Hold Person with a bigger
radius and no repeat save"* is more useful to a DM than any verdict,
and it is a retrieval result the app can justify by citation,
deep-linked into the reader the way search citations already are.

## The model writes the comparison up, and nothing else

The model's role is to write the comparison prose from the retrieved
passages and the computed findings. Two mechanisms hold that line:

- Findings are computed before the model runs and are never parsed out
  of its response. There is no shape by which its output could become a
  finding; the write-up is prose, or nothing.
- The prose gate reads what it did write: any figure that traces to
  nothing in the engine's own output — findings, neighbours, numbers —
  rejects the whole write-up, as does any legal/illegal verdict. The
  gate fails closed: a rejected write-up is not shown, and the findings
  stand unchanged.

## Findings, not a verdict

Each finding carries:

- a **severity** — `error` (structurally invalid), `warning`
  (disagrees with the maths), or `note` (worth a look) — the same
  vocabulary `grimoire canon check` speaks, so a DM learns one;
- an **origin** — `computed`, `structural`, or `retrieved` — what
  produced it; and
- a **basis** — its arithmetic, the rule it enforced, or its corpus
  citation. A test holds this at the type boundary: no finding reaches
  an API response without one.

The surface never returns "legal" or "illegal". There is no field for
a verdict to live in, at any layer.

## What it does not do

- No claim of rules completeness. The app models arithmetic and
  grammar, not mechanics.
- No adjudication of interactions between two homebrew pieces.
- No balance opinions without a computed or cited basis.
- No linting of official SRD content — where the calculator disagrees
  with a printed CR, that is documented in the statblock package's
  golden file, not reported as a finding. The corpus harness runs the
  structural checks over the whole mirrored bestiary and requires
  silence, so the SRD's own recharging abilities stay lint-clean.

## The API

| Route | What it does |
|---|---|
| `POST /api/monsters/{id}/lint` | Lint one of the caller's homebrew monsters. Findings, neighbours, and the write-up when a model is configured. |
| `POST /api/items/homebrew/{id}/lint` | Lint one of the caller's homebrew items against the SRD shelf. An empty mirror degrades into the report's notices. |

The response is a `report`: `findings` (each with its `basis`),
`neighbours` (each with the `corpus` and `number` the reader API
resolves into a deep link), `notices` (engine-level statements about
the run — an empty index, an empty mirror), and `write_up` with its
`write_up_state` (`written`, `rejected`, `unavailable`).

## The CLI

    grimoire homebrew lint [monster|item] [id ...]

Runs the same engine, next to `grimoire canon check`, in the same
format — one line per finding, severity and check code first — and
exits non-zero when anything structurally invalid turns up. With no
ids it lints every homebrew record; with a kind it restricts the
shelf. The structural and computed checks run with no model and no
network; the model pass runs only when chat is configured.

## Out of scope

Player-facing anything. Importing homebrew from third-party formats
(the linter will consume whatever lands, but the importer is its own
issue). Any general reasoning over what a mechanic *does* — that needs
a machine-checkable model of the game, which the corpus is not.
