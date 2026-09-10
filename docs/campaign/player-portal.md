# The Player Portal

How players sit inside a Grimoire campaign, and why the server can be
trusted to keep the DM's side of the screen dark.

The portal is not a second app. Players are ordinary accounts (ADR 4)
narrowed by a membership row, reading the same binary and the same
database the DM does — through an interface that cannot express a secret.
That last clause is the whole design: no layer of the defense is a prompt
instruction, and the portal does not ship unless all three are proven
against a real seated campaign.

## The three layers

| Layer | What it is | Where it lives | What a leak costs |
|---|---|---|---|
| **The compile boundary** | `knowledge.PlayerView` — the only store player-facing handlers can hold. No method on it can return a secret-visibility, proposed, or superseded row, and no method can see a draft handout. | `internal/knowledge/playerview.go` | A compile error. Wiring a leaky read into a player handler does not build. |
| **The reflection leak test** | Enumerates every exported retrieval on both stores by reflection and fires each at non-DM scopes against a seeded campaign with adversarial plants: a granted-but-secret fact, an awareness grant on a proposal, a retcon the party learned before it landed, rumours, handouts in every lifecycle status. | `internal/knowledge/leak_test.go` | A failing test in CI. A retrieval added later that forgets its scope filter ships no further than the next `go test`. |
| **The deterministic check** | `spoiler_leak` — a join, not a model call: a player-visible surface rendering a fact the party's awareness records as `unaware` is an error-severity finding, on every `canon check` run. | `internal/canon/engine.go` | An error in the DM's flag ledger the moment the state contradicts itself. |

Layer one is structural, layer two is behavioral, layer three is
state-level: a handler that somehow rendered an unlearned fact would still
be caught by the check over the campaign's awareness rows. Between the
three, "the portal leaked the twist" requires three independent failures.

Two behaviors worth knowing by name:

- **Granted but still secret.** The wide store serves granted secrets to
  their knower (the DM's simulation and NPC reasoning depend on that).
  The player view drops secret-visibility facts *even when the grant
  exists* — the party "does not know" it as far as any player surface can
  say. Whether a discovered secret transitions into the player-visible
  world is a deliberate product decision, not an accident of the join.
- **Proposed facts are invisible to everyone.** Extraction proposals live
  only in the review queue (ADR 3); no retrieval path serves them at any
  scope, DM included.

## The role model

| Role | How they get it | What they see | What they can do |
|---|---|---|---|
| **DM** | Creates the campaign; the keeper may also look into any campaign (the keeper flag outranks roles for reading, not for writing) | Everything, secrets and drafts included | Every write on the campaign |
| **Player** | Invited by the DM, with a character bound | The world the party has met: granted non-secret facts, met entities, witnessed events, the quest journal, their own sheet, published handouts, the board | Write their own journal, edit their own sheet (inventory and notes; mechanics behind the campaign's opt-in), chat at their scope, spend their own pools |
| **Observer** | Invited by the DM, no character | Watch: the board, the journal, party-scope chat | Nothing character-shaped reaches them; no sheet, no character-scoped chat |

Scoping is per-claim, not per-session: every campaign request resolves
through the membership row (`internal/server/campaign.go`,
`resolveCampaignAccess`), and a missing row is the same 404 as a wrong
campaign id — a non-member learns that a campaign exists nowhere. A
player with a bound character reads at `character:<id>` scope; unbound
members read at `party`. Chat threads pin their scope at creation and
refuse to continue if the member's standing changes underneath them.

## The journal belief loop

A player's journal is their account, and the engine treats it as belief,
never testimony:

1. **The write.** `POST /api/campaigns/{id}/sessions/{sid}/journal` — the
   author is the caller's bound character, bound server-side; the kind is
   `player_journal` and nothing else. A player reads their own entries;
   the DM reads the campaign's journals beside transcripts and notes.
2. **Extraction.** The DM's post-session canon run reads the journal like
   any source. A claim about something the campaign already records
   stages as a **belief candidate**, never a fact candidate — whether it
   matches or contradicts canon is resolved by a deterministic join, not
   the model's opinion.
3. **The queue.** An agreeing belief becomes a `proposed_belief` review
   item; a contradicting one is claimed by a `contradiction` item whose
   summary pairs the journal's claim with canon's statement, for the DM.
4. **Acceptance.** Accepting writes exactly one thing: a discovery whose
   provenance is the journal span, and its awareness row at the author's
   stance. A `knows` stance on a contradicting claim is coerced to
   `believes_false` — the honest record of a falsehood spoken as truth.
   Dismissing records nothing. Either way the journal stands as written,
   and the player is never corrected.

The belief then renders where beliefs live: the Player Grimoire's summary
shows the character *confidently wrong* ("what we know about the steward"
carries their belief), and the secret the belief orbits stays secret,
because the player view drops it regardless of grants.

## Seating a player: the operator's flow

1. **The DM mints an invite.** Campaign tool → members → invite: a code
   with a note, carrying the role (`player` or `observer`). The code is
   returned once, to the DM, who hands it to the human.
2. **The player redeems.** A fresh account registers with the code, or an
   existing signed-in account joins through `POST /api/campaigns/join`.
   Either path mints the membership row inside the same transaction as
   the account.
3. **The DM binds the character.** Members → pick the pc, or leave the
   member unbound (party scope — the observer-like read). Binding is
   what turns "a member" into "Mira Thorn's player": the sheet read, the
   character-scoped Grimoire, and the journal authorship all key off it.
4. **The player signs in and lands in the player seat** — the workspace
   whose rail carries the table, the world, their sheet, the journal and
   the Player Grimoire, and none of the DM's cockpit (ADR 22).

Re-invites are the DM's answer to a leaked code: mint a new one, delete
the old. Membership removal is the DM's answer to everything else.

## The rehearsal

`internal/server/shipgate_test.go` is the gate the portal ships against:
one campaign with a seated player, a secret architecture built to be
leaked (a granted-but-secret fact, a clue path, a retcon, a proposal, a
draft handout that names the secret, a belief from a real journal), and
the three layers fired together — the PlayerView reflection sweep over
every method on the final interface, the full player journey asserting on
assembled prompts and payloads at each step, and `canon check` with zero
`spoiler_leak` findings (plus proof the check fires when the state is
corrupted and clears when the DM repairs it). A DM-side teeth test keeps
every player-side emptiness honest: the same surfaces DO carry the secret
for the DM, so the emptiness is the scope line working.
