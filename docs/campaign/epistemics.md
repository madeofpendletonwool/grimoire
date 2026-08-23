# Epistemics

How Grimoire represents truth, belief, and the difference between them.

This is the part of the campaign core that is genuinely novel, and the part
where a shortcut is a real defect rather than a rough edge. It is written down
before the code exists so that "we'll sort out confidence later" is not
available as an option.

## Three layers of truth

Modelled explicitly, as rows — not as a prompt convention, and not as a
free-text `notes` field somebody greps.

| Layer | Question it answers | Where it lives |
|---|---|---|
| **Canon** | What is true in the world? | `facts`, `events`, `relationships` — DM-owned |
| **Knowledge** | What does each *character* actually know? | `awareness` + `discoveries`, with a discovery trail |
| **Belief** | What do the players *think* is true, including when they are wrong? | `awareness.stance = believes_false` |

Belief is the layer nobody models, and it is the most valuable of the three.
Knowing that the party is *confidently wrong about the Duke* is the single most
useful thing a campaign tool can tell a DM. It is the difference between a tool
that stores your notes and a tool that understands your table.

## Stance

`awareness` carries a `stance` per `(knower, fact)` pair, where `knower` is an
entity id or the literal `party`:

| Stance | Meaning |
|---|---|
| `knows` | The character has it right. |
| `suspects` | Believes it, not confirmed. |
| `believes_false` | **Believes something that is not true.** |
| `unaware` | Has not encountered it. |

`unaware` is stored rather than inferred from absence. A missing row and a
deliberate "they walked past the ledger and did not read it" are different
facts about the campaign, and only one of them is interesting.

The four-bucket summary — `knowledge.Summarize(scope, subject)`, answering
"what does the party know about the Duke?" as **confirmed / suspected /
incorrect beliefs / unknown** — is deterministic. No model call. That one
function is worth more than most of the generative features on the roadmap.

## Confidence

Confidence is a property of the **fact**, orthogonal to who knows it:

| Value | Meaning |
|---|---|
| `proposed` | Extracted or AI-suggested, awaiting a human decision. **Never retrievable by any perspective, including the DM's chat.** |
| `canon` | True in the world. Only ever written by a human decision. |
| `derived` | Inferred from canon facts, and re-derivable. Traceable to what it was derived from. |
| `contested` | Two credibly-sourced facts conflict. Both are downgraded here and linked in the register. Nothing picks a winner. |
| `retconned` | Was canon; superseded. Kept, with `superseded_by` pointing at what replaced it, because a campaign's history of its own retcons is part of the campaign. |

Nothing is ever deleted. `retconned` exists so that changing your mind is
recorded rather than erased.

### `proposed` is a real state

Unlike Arda — which writes extracted claims into the graph and lets later
passes downgrade them — Grimoire stages. Candidates land in the review queue
and are invisible to every retrieval path until a human accepts them.

The reason is timing. Arda's reader sees the book months later, so a bad claim
has a long correction window. Grimoire's DM is running a game on Thursday, and
a wrong fact in canon poisons live play immediately and invisibly: it gets
used, it gets built on, and by the time anyone notices, three sessions of story
are resting on it.

The pipeline's job is to make the accept decision **cheap and well-evidenced**.
It is not to make the decision. See
[ADR 3](../decisions.md#adr-3-stage-proposals-never-write-then-downgrade).

## The monotonicity rule

The load-bearing invariant of the whole canon engine:

> **A machine pass may only downgrade confidence or flag for review — never
> upgrade, never delete.**

Downgrades apply only when strictly downward from the current value. Upgrades
require a human.

Two properties fall out of it, and both matter more than they look:

- **The passes compose in any order.** The adversarial validator and the
  deterministic engine can run in either sequence, concurrently, or twice, and
  the result is the same. Nobody has to reason about pipeline ordering.
- **Re-running is always safe.** A resumed or repeated run cannot undo a human
  decision or promote something nobody approved. That is what makes an
  interrupted four-hour transcript run something you just restart.

## Perspective

Every retrieval carries a perspective — `dm`, `party`, `character:<id>`,
`npc:<id>` — and **the perspective decides which rows come back from SQLite**.

> Perspective is authorization, not instruction.

It is never expressed to a model as "do not tell the players about the
vampire." A player-facing chat handed the whole campaign and asked to withhold
the secrets **will** leak them — to a rephrase, to a hypothetical, to a long
enough conversation. Prompt-level secrecy is a request, not a mechanism.

Three independent layers enforce it, none of which is a prompt instruction:

1. **The type system.** `knowledge.PlayerView` has no method capable of
   returning a `secret`-visibility or `proposed` row. Player-facing handlers
   receive only that interface, so a leak is a **compile error**.
2. **The leak test.** A reflection-driven test enumerates every exported
   retrieval function in `internal/campaign` and `internal/knowledge`, calls
   each with a non-DM scope, and asserts no secret and no proposed row appears.
   A new function that forgets its filter fails the test rather than shipping.
3. **The `spoiler_leak` check.** A player-visible surface rendering a fact the
   party is `unaware` of is an error-severity finding. It is a join, not a
   model call, so it costs nothing to run constantly.

The rules corpora stay unscoped throughout — D&D rules are not a secret. Only
campaign rows pass through the filter, and the filter is in the SQL. See
[ADR 2](../decisions.md#adr-2-perspective-is-authorization-not-instruction)
and [ADR 6](../decisions.md#adr-6-the-player-portal-shares-the-binary-and-the-leak-is-a-compile-error).

## Evidence

Every claim resolves to something somebody actually said.

**The span rule:** every extracted candidate must cite the game session it came
from *and* a verbatim span — `source_id` plus byte offsets — of the transcript,
note or journal supporting it. A candidate that cannot is **dropped and logged
with its reason**, never staged.

**The adversarial pass** re-checks each candidate against only its own cited
evidence, in an explicitly skeptical stance, returning `agree` / `downgrade` /
`flag_review` with a 0–1 agreement score. Its prime directive, ported from
Arda:

> Do not use your own knowledge to rescue a claim the passage does not support.
> External knowledge may only deepen suspicion, never confirmation.

**Contradictions are preserved, never smoothed.** When two claims conflict and
both are credibly sourced, both are downgraded to `contested`, a register entry
links them, and per-source versions keep the accounts separable. The pipeline
never picks a winner — the DM does, or nobody does, and "the sources disagree"
stays a visible property of the campaign.

## The review queue

`open → accepted | dismissed`, with honest semantics:

- A re-run refreshes findings but **never clobbers a human decision**.
- A finding the engine stops reporting is marked `cleared`, not deleted.
- **A decided item never resurrects.** If the same finding reappears it opens as
  a *new* item, so "I already dismissed this" stays true.

## Cost

The canon engine is budget-guarded from day one — `CANON_BUDGET_USD`,
`CANON_MAX_CANDIDATES`, `CANON_BATCH_SIZE`, plus an `--offline` mode that runs
the deterministic engine only.

Self-hosters pay for their own tokens. A runaway extraction over a four-hour
transcript is a real way to make someone hate this feature, and the guard is
cheaper than the apology.
