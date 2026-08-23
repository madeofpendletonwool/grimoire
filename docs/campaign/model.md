# The campaign data model

The contract for the campaign core, written **before** any campaign code
exists. The campaign core itself (MAD-303) now implements the tables below —
see `internal/campaign` and migration `0002_campaign_core.sql`. The knowledge
layer (MAD-304) and Stage 3 (MAD-306) are still held to this shape.

The rest of Grimoire is a rules reference: fourteen independent tables, none of
which point at each other. The campaign core is the opposite — about twenty
tables that are mostly *edges*, because the interesting questions are all
relational. "What does the party believe about the Duke, and why do they
believe it?" is four joins, not a lookup.

## The one idea

> A persistent, structured model of **what is true**, **who knows it**, **what
> everyone wants**, and **what has already happened** — with every fact
> traceable to where it came from.

Encounter builders and NPC generators are commodity. A campaign that knows its
own epistemic state is not. So the graph comes first and the tools consume it;
a tool built before the graph would have to be rewritten onto it.

## Shape

```
                        campaigns
                            │
        ┌───────────────────┼───────────────────┐
        │                   │                   │
  campaign_members      entities             quests
        │              (typed nodes)            │
        │                   │             quest_transitions
        │        ┌──────────┼──────────┐
        │        │          │          │
        │  entity_aliases facts   relationships
        │                   │
        │        ┌──────────┼──────────┐
        │        │          │          │
        │ fact_provenance  contradictions ── fact_versions
        │        │
        │        └────────► game_sessions ── session_sources
        │                        │           session_events
        │                     events
        │                        │
        │            event_participants / event_links
        │
        └──────► awareness ──── discoveries
                (who knows what)
```

Three groups, in dependency order:

1. **Canon** — `entities`, `facts`, `relationships`, `events`. What is true.
2. **Knowledge** — `awareness`, `discoveries`. Who knows it, and how they found
   out.
3. **Provenance** — `fact_provenance`, `game_sessions`, `session_sources`. Where
   every one of those rows came from.

The third group is what makes the first two trustworthy. A fact with no
provenance row is a bug, and integrity checks it.

---

## Tables

### Campaign

| Table | Columns | Notes |
|---|---|---|
| `campaigns` | `id, owner_id, name, system, premise, clock, settings` | `system` and a JSON `settings` payload keep the core system-agnostic by construction. Only 5e is wired up; do not generalize speculatively. `clock` is the in-world date the campaign currently sits at. |
| `campaign_members` | `campaign_id, user_id, role, character_id` | `role` is `dm \| player \| observer`. **This is the whole player-identity story** ([ADR 4](../decisions.md#adr-4-player-accounts-extend-internalauth)): a player is an ordinary `auth` user narrowed by this row. No row, no access — default deny is a missing row, not a missing route. `character_id` points at the `pc` entity that makes a `character:<id>` perspective resolvable. |

### Entities and edges

| Table | Columns | Notes |
|---|---|---|
| `entities` | `id, campaign_id, kind, name, summary, payload, status` | Typed nodes: `pc \| npc \| faction \| location \| item \| deity \| organization \| creature \| concept`. `payload` is JSON, because a location and a deity have almost nothing in common and a wide table of mostly-NULL columns would be worse than honest JSON. |
| `entity_aliases` | `entity_id, name, kind` | Aliases and epithets — `canonical \| alias \| epithet`. One entity, many names: "Tom the innkeeper" is also "Thomas Vane". Without this, the extraction pass creates a second Tom, and the `entity_merge_candidate` check exists to catch exactly that. (Named `entity_aliases`, not the `entity_names` sketched here, because `entity_names` is already the rules-index's SRD name dictionary.) |
| `relationships` | `from_entity, rel_type, to_entity, strength, justified_by_fact, since_event` | Typed edges from a **controlled list** — `knows, serves, owns, located_in, worships, betrayed, allied_with, secretly_controls`, … A free-text edge type is a graph nobody can query. `justified_by_fact` is what makes an edge auditable: the relationship is not asserted, it is *derived from* a fact that has provenance. |

### Facts

| Table | Columns | Notes |
|---|---|---|
| `facts` | `id, campaign_id, subject_entity, predicate, object_entity, object_literal, statement, confidence, visibility, created_by, superseded_by` | Atomic statements. Object is an entity **or** a literal, never both. `statement` is the prose rendering, kept alongside the triple so retrieval has something to put in a prompt. `confidence` is `proposed \| canon \| derived \| contested \| retconned` and `visibility` is `public \| secret` — see [Epistemics](epistemics.md). `superseded_by` is how a retcon keeps its history instead of deleting it. |
| `fact_provenance` | `fact_id, session_id, source_id, span_start, span_end, quote, method` | **Every fact carries at least one provenance row.** `method` is `dm_authored \| ai_proposed \| extracted \| imported`. The span offsets resolve to actual words someone actually said, which is what makes the "why does Grimoire think this?" button real rather than decorative. |
| `contradictions` | `campaign_id, subject, predicate, status, resolution_note` | The register. `status` is `open \| resolved_by_review`. |
| `fact_versions` | `fact_id, contradiction_id, label, statement` | Two credibly-sourced facts that conflict are both downgraded to `contested` and linked here. **Nothing picks a winner** — ported from Arda's contradiction preservation. Smoothing a contradiction destroys the most interesting thing in the campaign. |

### Timeline

| Table | Columns | Notes |
|---|---|---|
| `events` | `id, campaign_id, session_id, summary, clock_at, real_ordinal, location_entity` | **Dual ordering, on purpose.** `clock_at` is in-world time; `real_ordinal` is the order the table played through it. They genuinely diverge — flashbacks, split parties, a session that covers three in-world weeks — and collapsing them into one column loses a question people actually ask. |
| `event_participants` | `event_id, entity_id, role` | Who was there and in what capacity. |
| `event_links` | `from_event, to_event, link` | `caused \| enabled \| revealed`. `caused` is what the `cause_after_effect` check runs over. |

### Knowledge

| Table | Columns | Notes |
|---|---|---|
| `awareness` | `campaign_id, knower, fact_id, stance, confidence, since_event, discovery_id` | `knower` is an entity id or the literal `party`. `stance` is `knows \| suspects \| believes_false \| unaware`. `believes_false` is the valuable one and the one nobody models — see [Epistemics](epistemics.md). |
| `discoveries` | `id, fact_id, discovered_by, session_id, method, span ref, confidence, accepted_by, accepted_at` | First-class, not a column on `awareness`. `method` is prose — "read the mining ledger". This is the audit trail behind "why does Grimoire think Mira knows this?" |

### Quests

| Table | Columns | Notes |
|---|---|---|
| `quests` | `id, campaign_id, name, state_machine, current_state` | A quest is a **state machine**, not a checkbox. The machine is JSON so a DM can describe a quest that branches. |
| `quest_transitions` | `quest_id, from_state, to_state, event_id` | Every move is tied to the event that caused it. A move along an edge the machine does not have is an error-severity finding (`quest_transition_invalid`). |

### Sessions and sources

| Table | Columns | Notes |
|---|---|---|
| `game_sessions` | `id, campaign_id, ordinal, name, started_at, ended_at, status` | **`game_sessions`, never `sessions`** — `auth.sessions` already exists and means login sessions. No exceptions ([ADR 7](../decisions.md#adr-7-mad-286-is-superseded-by-the-stage-3-session-layer)). |
| `session_sources` | `id, session_id, kind, author, content, checksum` | `transcript \| dm_notes \| player_journal \| chat_log \| live_mark`. Content is stored verbatim so span offsets stay valid forever. The checksum makes extraction runs idempotent. |
| `session_events` | `id, session_id, kind, payload` | `qa \| ruling \| note \| discovery \| encounter`. MAD-286's ruling log lives here as one `kind`. |

### Canon engine

| Table | Columns | Notes |
|---|---|---|
| `canon_runs` | `id, campaign_id, session_id, kind, prompt_version, stats` | One row per pipeline run, with the token and cost stats the budget guard reads. |
| `canon_verdicts` | `candidate_id, prompt_version, checksum, verdict, agreement, rationale, raw_output_id` | Keyed by candidate + prompt version + content checksum, so re-runs are idempotent and resumable and **nothing is billed twice**. |
| `canon_flags` | `check_code, record_kind, record_id, severity, status` | Deterministic-engine findings. `status` is `open \| accepted \| dismissed \| cleared`. |
| `canon_reviews` | `id, kind, payload, status, decision, note, decided_at` | The DM's queue. `open → accepted \| dismissed`; a re-run refreshes findings but **never clobbers a human decision**; a finding the engine stops reporting is marked `cleared`; a decided item never resurrects — the same finding reappearing opens as a *new* item. |
| `model_outputs` | `run_id, prompt_version, model, tokens, raw` | Raw model responses stored verbatim. Prompts are versioned code, never edited in place. |

---

## The span rule

The rule that makes extraction checkable rather than aspirational, ported from
Arda's both-tier rule:

> Every extracted candidate must cite **(1)** the game session it came from and
> **(2)** a verbatim span — `source_id` plus byte offsets — of the transcript,
> note or journal that supports it. A candidate that cannot is **dropped and
> logged with its reason**, never staged.

Two things fall out of it. The DM gets a "why does Grimoire think this?" button
that resolves to actual words someone actually said. And the adversarial pass
gets something concrete to judge against, instead of judging a claim against
its own plausibility.

## Retrieval

```
question + perspective
  ├─ campaign graph retrieval  ── SCOPED BY PERSPECTIVE ──┐
  │    entities / facts / events / relationships          │
  ├─ FTS5 over campaign prose (notes, summaries) ─ scoped ┤
  ├─ FTS5 + vectors over the rules corpora (unscoped)     │
  └─ card / SRD entity mention detection                  │
                                                          ↓
              prompt: answer from this text only, cite your sources
                                                          ↓
                          existing internal/llm client (unchanged)
```

The rules corpora stay unscoped — D&D rules are not a secret. Only campaign
rows pass through the scope filter, and **the filter is in the SQL**
([ADR 2](../decisions.md#adr-2-perspective-is-authorization-not-instruction)).

## Integrity checks

Pure Go over a campaign snapshot. No LLM, no database access in the rule
itself, every rule unit-testable, run on every save and before every session.

| Check | Severity | Catches |
|---|---|---|
| `spoiler_leak` | **error** | a player-visible surface rendering a fact the party's awareness says they are `unaware` of |
| `knowledge_before_discovery` | error | an awareness row dated before the session that produced its discovery |
| `dangling_reference` | error | a fact, event or edge pointing at a deleted entity |
| `cause_after_effect` | error | an event caused by one dated later on the campaign clock |
| `quest_transition_invalid` | error | a quest moved along an edge its state machine does not have |
| `contradictory_facts` | review | same subject and predicate, conflicting objects, both canon → register both, downgrade both to `contested` |
| `entity_merge_candidate` | review | one name on two entities — the "Tom the innkeeper" problem |
| `duplicate_fact` | review | the same subject/predicate/object on two fact ids |
| `awareness_without_source` | review | a character knows something with no discovery trail |
| `orphan_thread` | warning | a hook, secret or clue introduced N sessions ago and never referenced since |
| `unreachable_secret` | warning | a secret with no clue path any character can currently reach |
| `stat_block_unresolved` | warning | an encounter monster with no bestiary entry |
| `party_level_drift` | warning | a planned encounter budget that no longer matches the party |

Two are worth calling out.

`spoiler_leak` is the invariant that makes the player portal trustworthy, and
it is **free** — it is a join, not a model call.

`orphan_thread` and `unreachable_secret` together are the "what did we forget?"
feature, delivered deterministically: no tokens, no hallucination, runs in
milliseconds. The LLM version of that feature is strictly worse.

## Migrations

All campaign schema ships as goose migrations under
`internal/migrate/migrations/` ([ADR 1](../decisions.md#adr-1-presslygoose-is-the-migration-library)).
`internal/campaign` is the first package built the new way: **no `CREATE TABLE
IF NOT EXISTS` in `New()`**, and it sets the pattern for everything after it.
