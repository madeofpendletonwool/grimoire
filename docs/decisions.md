# Decisions

Architecture decisions that are settled, with the reasoning that settled them
and the alternatives that were rejected. The point of writing them down is that
nobody has to relitigate them from scratch a year later — including whoever
wrote them.

Each entry is dated, has a status, and names the issue it landed under. A
decision is only reopened by a new entry that supersedes it, never by editing
one in place.

---

## ADR 1 — `pressly/goose` is the migration library

**Status:** accepted · **Date:** 2026-08-22 · **Issue:** MAD-302

### Context

Every package created its own tables in `New()` with `CREATE TABLE IF NOT
EXISTS`, and patched later drift with hand-rolled `addColumnIfMissing()`
helpers. That is genuinely fine for fourteen tables owned by fourteen
independent packages that never reference each other.

The campaign core is roughly twenty tables that *do* reference each other, with
enums, indexes and backfills. On a self-hosted box a half-applied schema is not
an inconvenience, it is a data-loss event — and the person it happens to is
someone running this for their D&D group on a NAS, not an operator with a
backup rotation.

### Decision

Adopt [`pressly/goose`](https://github.com/pressly/goose) v3, wrapped in
`internal/migrate/` so nothing else in the codebase imports goose directly.

Verified against this exact stack before it was chosen — goose `v3.27.3` +
`modernc.org/sqlite v1.56.0` + `embed.FS`, built `CGO_ENABLED=0`: migrations
applied in order, a second run was a clean no-op, WAL survived, `ALTER TABLE
ADD COLUMN` behaved.

What makes it fit Grimoire specifically:

- **Library-first.** `goose.Up(db, dir)` takes an already-open `*sql.DB`, so
  the pure-Go driver keeps working and **no CGO is introduced**. The static
  binary with no CGO is a non-negotiable property of this project, and it is
  the main reason goose wins.
- **`embed.FS` native**, via `goose.SetBaseFS` — migrations ship *inside* the
  one binary. No migrations directory to deploy, no second tool to install, no
  way for the schema and the code to arrive separately.
- Up / Down / Status / Version, one transaction per migration, plain `.sql`
  files with `-- +goose Up` / `-- +goose Down` markers, and `-- +goose NO
  TRANSACTION` for the rare statement that needs it.
- Actively maintained, and adds only four transitive modules
  (`mfridman/interpolate`, `sethvargo/go-retry`, `go.uber.org/multierr`,
  `golang.org/x/sync`) — all small and pure Go.

### Alternatives rejected

| Option | Why not |
|---|---|
| `golang-migrate` | Its best-known SQLite driver is CGO (`mattn/go-sqlite3`). The pure-Go path is possible but fiddly, and the dependency tree is large for a repo with three direct dependencies. |
| `dbmate` | CLI-first. Shipping a second binary contradicts the single-binary deployment story. |
| `rubenv/sql-migrate` | Fine and small, but less active, with no advantage here. |
| `atlas` | Schema-as-code. Far more machinery than a twenty-table SQLite app needs. |
| A hand-rolled runner | ~200 lines reimplementing what goose already has tested, where the failure mode is silent data loss on someone else's machine. |

### The gotcha worth pinning

Call `goose.SetDialect("sqlite3")` even though the registered driver name is
`sqlite`. The **dialect** picks the SQL flavour for goose's own
`goose_db_version` bookkeeping table; the **driver** comes from the `*sql.DB`
you hand it. Getting this wrong produces an unknown-dialect error that reads
like a driver problem and is not one — a confusing five-minute detour, pinned
here so it costs nobody a sixth minute.

### Consequences

- `migrate.Up` runs before the server serves a request. **A failure is fatal**
  — the binary refuses to serve rather than serving a half-schema.
- Migration `0001` is a **baseline that adopts the existing schema without
  touching it**: `CREATE TABLE IF NOT EXISTS` throughout, so it *creates* on a
  fresh database and is a *no-op* on an existing one. No stamping logic, no
  data movement, nobody loses a conversation by upgrading.
- Operators get `grimoire migrate status | up | down`.
- Version numbers must start at 1 and increase by one. A duplicate or a gap
  fails `go test`, because a numbering collision discovered mid-upgrade is much
  worse than one discovered in CI.
- Existing packages keep their own `New()` DDL for now. Stripping it is a
  deliberate follow-up. A test builds the schema both ways and asserts they are
  identical, so the copy cannot drift while the duplication lasts.

---

## ADR 2 — Perspective is authorization, not instruction

**Status:** accepted · **Date:** 2026-08-22 · **Issue:** MAD-302, enforced in MAD-304

### Context

The campaign layer serves the same data to a DM and to players who must not see
most of it. The tempting implementation is to put the campaign in the model's
context and tell it what to withhold.

### Decision

Every retrieval carries a **perspective** — `dm`, `party`, `character:<id>`,
`npc:<id>` — and the perspective decides **which rows come back from SQLite**.
The filter is in the SQL. It is never expressed to a model as an instruction.

### Why

A player-facing chat handed the whole campaign and asked to keep the vampire
secret **will** leak it — to a rephrase, to a hypothetical, to a long enough
conversation. Prompt-level secrecy is not secrecy; it is a request. The player
Grimoire must be *physically incapable* of retrieving a fact the party has not
discovered.

### Consequences

- Every retrieval function in `internal/campaign` and `internal/knowledge`
  takes a `knowledge.Scope`. There is no unscoped read path outside the DM
  scope.
- Filtering happens in SQL, not in Go, and not in a prompt. A filter applied
  after the rows are loaded is the same bug with extra steps.
- The rules corpora stay **unscoped** — D&D rules are not a secret. Only
  campaign rows pass through the filter.
- A reflection-driven leak test walks every exported retrieval function and
  proves it (MAD-304). A new function that forgets its filter fails the test
  rather than shipping.

---

## ADR 3 — Stage proposals; never write-then-downgrade

**Status:** accepted · **Date:** 2026-08-22 · **Issue:** MAD-302, built in MAD-307/310

### Context

The canon engine is a port of the fact-validation pipeline built for *The
History of Arda*. Arda writes extracted claims straight into the graph and lets
later passes downgrade the ones that do not hold up.

### Decision

Grimoire does not. Extracted candidates land in a review queue with confidence
`proposed`, and **`proposed` rows are never retrievable by any perspective —
not even the DM's chat** — until a human accepts them. Canon is only ever
written by a human decision.

### Why

Arda's reader sees the book months later, so a bad claim has a long correction
window. Grimoire's DM is running a game on Thursday. A wrong fact in canon
poisons live play immediately and invisibly: it gets used, it gets built on,
and by the time anyone notices, the table has three sessions of story resting
on it.

The pipeline's job is to make the accept decision cheap and well-evidenced. It
is not to make the decision.

### Consequences

- `proposed` is a genuine state in the confidence vocabulary, not a flag.
- The **monotonicity rule** applies to everything downstream: a machine pass
  may only downgrade confidence or flag for review — never upgrade, never
  delete. Upgrades require a human. See
  [Epistemics](campaign/epistemics.md).
- Because downgrades only ever move strictly downward, the adversarial pass and
  the deterministic engine compose in any order and are safe to re-run.

---

## ADR 4 — Player accounts extend `internal/auth`

**Status:** accepted · **Date:** 2026-08-22 · **Issue:** MAD-302, built in MAD-303/305

### Context

Players need accounts. The campaign layer could mint its own lighter identity
for them, or reuse the one that already exists.

### Decision

A player is an ordinary `internal/auth` user, narrowed by a
`campaign_members(campaign_id, user_id, role)` row. The only new machinery is
that **invites gain an optional campaign and role**, so a DM mints a player
invite the same way the keeper mints an account invite today.

### Why

A separate player identity means a second password path, a second session
cookie, and a second set of ways to get authentication wrong — for a user who
is, in the end, just an account with a narrower scope. One auth surface is
easier to keep correct than two, and this is the surface where being correct
matters most.

### Consequences

- A user with no keeper flag and no membership row sees nothing. **Default deny
  is a missing row, not a missing route** — there is no handler to forget to
  guard.
- Roles are `dm | player | observer`, carried on the membership row.
- A membership may point at a character entity, which is what makes
  `character:<id>` perspectives resolvable.

---

## ADR 5 — Transcription is an optional configured endpoint

**Status:** accepted · **Date:** 2026-08-22 · **Issue:** MAD-302, built in MAD-320

### Context

The canon engine's best input is a session recording. Turning audio into text
has to happen somewhere.

### Decision

The session layer ingests **text only**: pasted transcripts, uploaded
`.txt`/`.md`/`.srt`/`.vtt`, DM notes, player journals. Transcription is a
**hook** — `TRANSCRIBE_BASE_URL` + `TRANSCRIBE_MODEL`, speaking the
OpenAI-compatible `POST /v1/audio/transcriptions` shape that `whisper.cpp`'s
server, `faster-whisper-server`, LocalAI and OpenAI itself all already
implement. Unset means the audio upload affordance simply is not there —
exactly how `EMBEDDINGS_BASE_URL` already behaves.

### Why

Bundling whisper into the default image would multiply the image size for a
feature many DMs will not use, and it makes a deployment decision on the
operator's behalf. It would also break the design invariant that **nothing
loads from a third party unless the operator pointed it there on purpose**.

### Consequences

- `docker-compose.yml` gets a `profiles: ["transcribe"]` service, so
  `docker compose --profile transcribe up` gets you a local one and the default
  `up` stays small.
- Transcription is skippable and off the critical path. The canon engine is
  fully usable with pasted text.

---

## ADR 6 — The player portal shares the binary, and the leak is a compile error

**Status:** accepted · **Date:** 2026-08-22 · **Issue:** MAD-302, built in MAD-304/319

### Context

The player portal serves the same database as the DM's Grimoire, to people who
must see a small fraction of it. The safe-looking answer is a second service
with its own process and its own narrower database access.

### Decision

Same binary, same database, scope enforced in SQL (ADR 2) — **plus** a type-level
guarantee:

> Player-facing handlers are wired to a `knowledge.PlayerView` interface that
> **has no method capable of returning a `secret`-visibility or `proposed`
> row.** The DM store satisfies a wider interface; the player router group
> never receives it.

### Why

A second service doubles the operational burden for every self-hoster — two
containers, two configs, two things to upgrade in step — to buy process
isolation that the actual defenses do not need. The single-file deployment is a
real product property, not a convenience.

But "we remembered to filter" is a weak defense when it is repeated across
dozens of handlers. Narrowing the interface moves the guarantee from *review
caught it* to *it does not compile*.

### Consequences

Three independent layers, none of which is a prompt instruction:

1. The `PlayerView` interface — a leak is a compile error.
2. The reflection leak test (MAD-304) — a leak inside the wide DM store is a
   test failure.
3. The `spoiler_leak` deterministic check — a leak that reaches rendered output
   is an error-severity finding, and it is a join, not a model call, so it is
   free.

---

## ADR 7 — MAD-286 is superseded by the Stage 3 session layer

**Status:** accepted · **Date:** 2026-08-22 · **Issue:** MAD-302, absorbed by MAD-306

### Context

MAD-286 specified an `internal/table` package with `game_sessions` /
`session_events` tables and its own FTS ruling log. That is roughly 60% of the
session layer this plan needs, and the two would collide head-on.

### Decision

Fold MAD-286 into MAD-306 and close it as superseded. `internal/table` is never
created. The ruling log becomes one `kind` of session event on the shared
session record instead of a parallel system, and the prior-ruling FTS surfacing
ships verbatim as MAD-286 specified it.

### Why

Two session tables with two ingestion paths would mean the canon engine has to
read both, and every later feature has to know which one a given ruling lives
in. This is flagged rather than silently overridden — MAD-286 was a written
plan, and this contradicts it.

### Consequences

- Nothing from MAD-286 is lost; it is relocated.
- **Naming:** `auth.sessions` (login sessions) already exists. Game sessions are
  `game_sessions` everywhere, no exceptions.

---

## ADR 8 — The testing policy for the campaign work

**Status:** accepted · **Date:** 2026-08-22 · **Issue:** MAD-302

### Decision

Standing acceptance criteria for every issue under MAD-301:

- **Unit tests throughout.** **Mandatory** for the deterministic consistency
  engine and all wire parsing — an untested consistency rule is worse than no
  rule, because it is trusted.
- **End-to-end over a temp SQLite database with a fake LLM client replaying
  fixtures**, for every canon-engine stage. This is how Arda tests its pipeline,
  and it is why that pipeline is trustworthy.
- **Handler tests** in the existing `internal/server` style for scope
  enforcement, asserting on the HTTP response and on the assembled prompt — not
  on the store. A store-level test proves the filter works; a handler test
  proves the filter is *reached*.
- **The reflection leak test** (MAD-304) as a hard gate.
- **No browser or UI end-to-end tests.** The front end has no build step and no
  framework; the harness would cost more than it catches.

### Consequences

CI needs no changes. `.github/workflows/ci.yml` already runs `gofmt`, `go vet`,
`go build` and `go test -race -count=1 ./...` on every push and pull request,
which covers all of the above.

---

## ADR 9 — The event log is the only writer; state is a fold

**Status:** accepted · **Date:** 2026-08-27 · **Issue:** MAD-322

### Context

Four players, one game, several screens, and a product constraint that
decides everything: correction must be cheaper than manual entry. The
obvious implementations — shared mutable state with locking, or per-client
state with a sync protocol, or undo as compensating transactions — each make
correction an engineered feature instead of a natural operation.

One naming note while we are here: ADR 7's "`internal/table` is never
created" was about MAD-286's D&D session tracker, folded into the session
layer. The MTG engine package (MAD-323) takes that name for something else;
this entry records that the reuse is deliberate, not a violation.

### Decision

The game state model is event-sourced with a single writer:

- `apply(state, action) → ([]Event, error)` is a **pure function**. No I/O,
  no clock, no model, no randomness that is not passed in.
- Events are appended to `mtg_events` with **monotonic per-game ordinals**,
  assigned by one writer in one transaction. Between rewinds the log is
  append-only.
- **State is a pure fold over the log** — deterministic, total, and **never
  stored as truth**. Nothing else mutates state; there is nothing else to
  mutate.
- **Undo and amend are truncate-and-refold.** Rewind to *N* deletes events
  after *N* and re-folds. No compensating transactions exist anywhere.

### Why

Everything the plan asks for falls out of this one shape: undo is free,
replay and post-game analysis are viewers over data Stage 2 already writes,
the reducer is exhaustively unit-testable, and multiplayer is log
replication — append-only ordinals over SSE, no conflict resolution, no
CRDT, because a single writer assigns ordinals.

### Consequences

- The SSE contract is "replay from the last ordinal you saw"; contiguity of
  ordinals per game is an invariant, and a gap is corruption.
- A rewind is announced on the stream as a control frame so attached clients
  re-fold; no client ever folds a log that no longer exists.
- `mtg_rulings` rows survive a rewind — human records are never clobbered by
  a truncate, the same semantics as a decided canon review.
- Board rendering, traces ("why is this 7/7?"), and per-seat views are all
  folds. A materialized projection, if ever added, is a cache with the fold
  as its contract — never a second writer.

---

## ADR 10 — The engine is the authority; the LLM is a parser

**Status:** accepted · **Date:** 2026-08-27 · **Issue:** MAD-322, built across MAD-330/331

### Context

The table surface accepts natural language: "attack Sarah for six", "make
two Treasures", "-3". The tempting architecture is a model with the game in
its context, updating state as it chats.

### Decision

The model's only job is **natural language → a candidate typed Action**. It
never writes state, and it never decides a rules question unaided. The
deterministic engine validates and applies every Action; rules answers are
grounded through `internal/resolver` against the live board, citing rules
and card text, and say plainly where they cannot verify something.

Grammar before model, specifically: a hand-written deterministic parser
(MAD-330) covers the phrasings tables actually use — instant, free, offline,
consistent — and the LLM (MAD-331) is the fallback for what the grammar
returns no-parse, never the front door.

### Why

A model writing state produces a plausible board, and plausibility is the
failure mode: it is trusted, built on, and wrong silently. This is the
campaign's "stage proposals, never write-then-downgrade" (ADR 3) in a
different costume — the parser *proposes* a typed Action, the engine
*disposes*. Consistency is the property the user flagged as the catch, and
consistency is exactly what a deterministic grammar has and a model does not.

### Consequences

- One Action type from every source: grammar, model fallback, and the
  manual board UI produce the same values and walk the same reducer.
- Parse output is **Action plus confidence, never applied state**; the
  confidence feeds the confirmation ladder (`auto` / `confirm` / `ask`), and
  a wrong parse is designed to be impossible — no-parse is returned cleanly
  rather than guessed.
- Resolved names are cached per game (`mtg_name_resolutions`) so the same
  mumble is never re-inferred and never re-billed.

---

## ADR 11 — Card behaviour is declared, never simulated

**Status:** accepted · **Date:** 2026-08-27 · **Issue:** MAD-322

### Context

Magic has roughly 28,000 unique cards with effectively arbitrary text. The
tempting feature is reading oracle text and simulating what a card does.

### Decision

The engine enforces **structure fixed by the Comprehensive Rules** — zones,
turn and phase order, priority, state-based actions, the stack, combat
arithmetic, commander rules — none of which depends on card text. Card-
specific effects are **recorded as declared or confirmed deltas**: a player
says what happened ("Anthem gives my creatures +1/+1"), or the model
proposes it once and a human confirms, and the mapping is cached by card
name so the second occurrence is automatic. Oracle text is never parsed into
simulated behaviour.

### Why

The only complete engines in existence — MTGO, Arena, Forge, XMage — are
per-card scripted implementations representing thousands of
contributor-years; Forge and XMage each carry a Java class per card. That is
the actual cost of simulation, and it cannot be cut with a model: a model
that is 97% right about a card's behaviour produces a board state that is
**silently wrong, which is worse than no board state**, because it is
trusted and built upon. A declared delta is honest about where the knowledge
came from — a human said so, at this ordinal, correctable in two taps.

### Consequences

- The engine's promises are exactly the structural ones, all fully
  deterministic and unit-testable.
- Modifiers are attached (source + layer + duration + delta), never derived
  from card text; the trigger registry fires only on structural events the
  engine already emits.
- Rules questions are answered from the real board with citations — an
  assistant, not a Comprehensive Rules oracle — and the UI keeps saying so.
- Combo discovery, when it comes (MAD-340), works over registered shapes and
  declared effects, and says so rather than implying completeness.

---

## ADR 12 — Decklist-scoped identity before global

**Status:** accepted · **Date:** 2026-08-27 · **Issue:** MAD-322, built in MAD-329

### Context

"I'll play my Rhystic" has to become *Rhystic Study* before it can be an
Action. Matching a mumbled name against all of Magic is error-prone.

### Decision

Names resolve against the **loaded decks first** and `carddb` second:
exact match in the speaking seat's deck → fuzzy match in that deck → any
attached deck → the global card database → unresolved. Decks are strongly
encouraged but optional — a game without them works, identification is just
worse, and the UI says so rather than blocking setup.

### Why

Matching against ~400 known cards is near-exact; matching against ~28,000 is
not. Loading decklists collapses the identification problem for one paste
per player per game — the highest-value-per-effort decision in the plan —
and `internal/deck.ParseDecklist` plus the existing credibility-gated fuzzy
matchers already exist; no second scoring scheme is invented.

### Consequences

- Library **composition** becomes known when a deck is attached; **order
  never does**, and the engine refuses order-dependent questions out loud.
- Resolution confidence feeds the confirmation ladder — a global fuzzy hit
  lands on `confirm` or `ask`, an exact deck hit on `auto`.
- Resolutions cache per game (`mtg_name_resolutions`): the same mumble is
  never re-inferred, never re-billed, and survives rewind.

---

## ADR 13 — Hidden zones are authorization in SQL, not instruction

**Status:** accepted · **Date:** 2026-08-27 · **Issue:** MAD-322, enforced in MAD-337

### Context

A Magic game has hidden zones, and this is precisely the campaign side's
perspective problem (ADR 2) in a different costume: hands are opaque to
other seats, private notes are private, and the tempting implementation is
telling the model what to withhold.

### Decision

Event rows carry `visibility` — `public`, or `seat` with a `visible_seat` —
and **a seat's view of the log is a WHERE clause**. The per-seat stream is
selected, never post-filtered, never entrusted to a prompt. A seat-visible
row is invisible to other seats as a row; its public consequences (a hand
count moving) are separate public events.

### Why

Prompt-level secrecy is a request, not a mechanism — the player Grimoire must
be **physically incapable** of retrieving a card another seat has not
revealed. The game's owner is entitled to everything (the DM analog, and the
common solo-tracker case); every other account sees the public stream plus
its own seat's rows.

### Consequences

- The engine's fold includes seat-visible rows — it must, to serve a seat
  its own view — exactly as the campaign store holds secrets no player
  retrieval can reach. The gate is the query.
- The reflection leak test pattern (MAD-304) is the hard gate here: walk the
  response types and assert no hidden-zone field can reach a seat not
  entitled to it (MAD-337), plus a `hidden_zone_leak` deterministic check —
  a join, not a model call, so it is free.
- Identity-bearing events (`CARD_KNOWN`, `LOOK`) choose their visibility at
  submission: spoken at the table is public, typed on your own screen is
  seat-visible.

---

## ADR 14 — `node --test` for DOM-free front-end modules

**Status:** accepted · **Date:** 2026-08-28 · **Issue:** MAD-366

### Context

[ADR 8](#adr-8--the-testing-policy-for-the-campaign-work) ruled out browser and
UI end-to-end tests: the front end has no build step and no framework, so a
harness would cost more than it catches. That reasoning is about **rendering** —
asserting on a DOM the browser builds — and it still holds.

The Campaign OS window manager introduces something ADR 8 did not anticipate: a
**layout tree** (`web/static/js/wm/tree.js`) that is pure data in, data out. It
splits containers, collapses them when their last child leaves, renormalises
fractions, moves focus directionally through a tree, and parses hostile stored
JSON. None of it touches the document.

It is also the exact case ADR 8 makes *for* testing the consistency engine: an
untested rule is worse than no rule, because it is trusted. A tiling bug that
drops a window or corrupts a tree is invisible until the user reloads and finds
their workspace gone, and the corrupted tree is by then in the database.

### Decision

`node --test` may run front-end modules that **import no DOM**. Tests live in
`jstest/`, outside `web/static/` so they are not embedded in the binary by
`//go:embed all:static`.

A root `package.json` declares `"type": "module"` — nothing more. It has **no
dependencies and no install step**; it exists so Node reads the same `.js` ES
modules the browser loads directly.

```bash
node --test "jstest/**/*.test.js"
```

The rule stays narrow: **if a module imports from `dom.js`, or touches
`document` or `window`, it does not get tests.** That is still ADR 8's
territory and ADR 8 still governs it.

### Why

- The property under test is algebraic, not visual. "Removing a window never
  loses another window" is a statement about a tree, provable in milliseconds
  with no browser.
- Node's runner is built in. No Jest, no Vitest, no jsdom, no bundler, no
  `node_modules` — the objection ADR 8 raised (harness cost) does not apply.
- Node is a **dev and CI** dependency only. The binary is unchanged, ships the
  same files, and still runs on a machine with no route to the internet.
- The layout tree is also the one front-end module whose output is persisted
  and re-parsed later, so a defect outlives the session that caused it.

### Consequences

- `.github/workflows/ci.yml` gains a Node step. It is fast and needs no cache.
- `web/static/js/wm/tree.js` must stay DOM-free. Rendering lives in `wm.js`,
  which is not tested — the split is the point, and it is load-bearing rather
  than stylistic.
- `internal/uistate` validates the same tree independently, in Go, with its own
  tests. The duplication is deliberate: the client repairs what it can so a
  stale layout costs one window, the server refuses what is malformed so the
  table never holds a tree the shell cannot parse. Neither trusts the other.
- The precedent is bounded. A future module wanting tests must first be
  DOM-free; "make it testable" is a design constraint, not a licence to add a
  harness.

---

## ADR 15 — The typed character sheet: payload block plus a narrow derived projection

**Status:** accepted · **Date:** 2026-09-05 · **Issue:** MAD-418

### Context

The mechanical layer (MAD-417) starts from a `pc` that is a typed entity node
with a freeform JSON payload. Its first stage must make the character sheet —
abilities, proficiencies, class levels, spellcasting, inventory — first-class
data, and decide where it lives. The house pattern (`docs/campaign/model.md`)
says payload JSON beats a wide table of mostly-NULL columns; but the party
board and the encounter surfaces want queryable numbers, and a JSON blob per
row answers no `ORDER BY level` at all.

### Decision

The sheet is a **typed Go struct** (`internal/sheet`) serialized under the pc
payload's `"sheet"` key — the place-block pattern, not the party block's
top-level ownership, because a pc payload now carries both spellings and a
block that owned the top level would collide with its own legacy keys.

`PartyBlockOf` reads **sheet-first**: a payload carrying a sheet is the
definition (level totals, class, AC, max HP, resistances, inventory names);
the legacy top-level keys remain the fallback, so every existing campaign
reads exactly as it did. The mapping is deliberately partial — remaining
slots, conditions and save bonuses do not map, because they are state or
derivations, and state belongs to the resource ledger (MAD-419), never to
the definition.

A **narrow projection table** (`pc_sheet_projection`, migration `0029`)
carries the queryable numbers — level, a classes label, max hp, ac, and
`structured`, the unstructured marker made queryable. It is maintained, never
trusted: every pc entity write refreshes its row, and server start re-derives
the table whole. Legacy payloads project from their existing keys (reading
keys that exist is derivation, not invention); a dropped table costs one
boot, not data. Backfill happens in Go, not SQL, because turning a payload
into numbers is the typed reader's job with its tolerance rules.

Player scope gets **one deliberate widening**: a character-scoped player may
read exactly their own bound character's sheet through the player view
(`knowledge.PlayerView.CharacterSheet`) — no other pc, no other payload key,
no facts. Writes and imports stay DM-only; the player-edit question is the
portal stage's (MAD-319), not a default set here.

### Alternatives rejected

| Alternative | Why rejected |
|---|---|
| A wide `character_sheets` table, one column per field | Twenty-plus mostly-NULL columns against the house pattern; a sheet and a place block are the same kind of thing, and JSON is the honest container for both |
| Sheet as the payload's top level (the party block's spelling) | Would collide with the legacy keys every existing campaign carries, and force a rewrite of every pc payload to adopt |
| SQLite generated columns / `json_extract` views over the payload | The tolerant-read rules (number-or-string, per-key problems) are not expressible in SQL without inventing numbers the typed reader rejects |
| Computing save bonuses and remaining slots into the party view | Saves are a derivation (Stage 3 computes them); remaining slots are ledger state (MAD-419). Storing either twice is the drift this split exists to prevent |

---

## ADR 16 — The resource ledger: transactions, not balances

**Status:** accepted · **Date:** 2026-09-05 · **Issue:** MAD-419

### Context

Stage 1 ([ADR 15](#adr-15--the-typed-character-sheet-payload-block-plus-a-narrow-derived-projection))
made the sheet the character's definition and explicitly left "what is
left" out of it. Stage 2 must build that state: spell slots (pact magic
included), hit dice, ki, rage, ammunition, the purse — and the rests that
reset them, which are also the moment a campaign's clock moves while the
party sleeps. The design question is where current values live.

### Decision

**Every tracked thing is a pool** — `{owner, kind, size, recovery:
short|long|dawn|manual, granularity}` — and **every change is an
append-only transaction** (spend, regain, set, reset) linked to the session
event and actor that caused it. There is no balance column anywhere:
current values are folded from the log on every read (`internal/ledger`),
byte-stable for the same pools and the same log. The same rule that makes
a sim tick re-runnable makes "why is the wizard out of 3rd levels?"
resolve to the actual spends, in order, with provenance.

Pool definitions are **derived from the sheet** where the sheet carries
numbers (slot levels, total level as hit dice, the purse), re-synced on
every sheet write with row ids preserved; the DM registers what the sheet
cannot hold (ki, arrows) through the same grammar. Rests are batch
transactions whose resets are decided by the recovery grammar alone —
short rest resets `short`; long rest resets `short`, `long` and `dawn`;
hit dice are regained by an explicit transaction implementing the 2014
"up to half, minimum one" rule rather than a grammar special case. A long
rest advances the campaign clock one day under a new `rest` reason on the
`clock_advances` ledger (migration `0030`'s CHECK rebuild).

A rest proposed by a machine stages behind the review gate as one canon
batch (source `rest`, the finalizer pattern the tick, downtime and journey
set); the DM's live button executes immediately. Players spend their own
(spend/regain only); the DM adjusts anyone, always as a visible
transaction.

### Alternatives rejected

| Alternative | Why rejected |
|---|---|
| A `current` column updated in place | Two writers and a stale read make "why" unanswerable; the append-only log is the whole point, and derivation is cheap at table scale |
| Balances as a payload block on the pc | The sheet/definition split ADR 15 pinned; state that edits in place is exactly the drift the split prevents |
| Hit dice as `long` recovery with a half-size reset | Wrong per the 2014 PHB (they return *up to* half, minimum one); the explicit regain transaction keeps the grammar pure and the number correct |
| Per-feature rest rules (a `resets_on` table per feature) | The recovery grammar is the one rule; pact magic is just a `short` slot pool, and a special case per feature is the maintenance burden the grammar exists to avoid |
| Hooking dawn recovery into every clock advance | Would couple the campaign store to the ledger for a vocabulary nothing in the 2014 core uses; dawns reset on the rest that crosses them, and other advances take an explicit, visible transaction |
