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
