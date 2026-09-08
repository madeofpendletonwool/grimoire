# The DM screen: play mode's layout

The three hours the game is actually running get their own surface — not
the chat view with extras. **Alt+2** ("At the table") composes the whole
thing in one keypress: the screen strip, the combat tracker, the party
board (with dice, the encounter director and the encounter builder
behind its tab). Nothing needs rearranging; that is the acceptance
criterion, and the layout is seeded as the preset so it is true the
first time and every reset. Stage 2 of play mode (MAD-485); the tracker
it composes is [MAD-487](combat.md), the board [MAD-423](board.md), the
director panel [MAD-484](director.md).

## The screen strip

The thin column on the left (`web/static/js/screen.js`), four panels and
a mount point:

- **The clock.** The live session's `started_at`, ticked client-side —
  the server never counts. No session live? A picker seats one ("go
  live" is the existing session PATCH, no new write path) and the clock
  starts from the stamp the server already keeps.
- **The scene.** The read-only "live context": the active scenes
  (status `active`) with their cast, the one seated in the live session
  first. Kind, name, purpose, setting — the Planner owns every write;
  the screen only answers *where are we?*
- **On stage.** The current scene's cast as chips, focus first. A chip
  opens the entity in the drawer without leaving play mode.
- **Notes.** One line, one tap: a session event of kind `note` against
  the live session — the mid-play "don't forget" parking lot, newest
  first, the same immutable log the post-session canon run reads.
- **Ask Grimoire.** The session copilot — see below.

## The session copilot

Stage 5 of play mode (MAD-486): the big **Ask Grimoire** box at the
bottom of the strip. *"They ask the innkeeper about the murders"* — type
it, the answer streams, and if it names an NPC on stage (the scene's
cast, then the fight's entities), the answer arrives in their voice:
a DM-facing reaction and speakable dialogue, grounded in that NPC's
actual knowledge state.

```
POST /api/campaigns/{id}/ask          → SSE: meta, delta, done, error
POST /api/campaigns/{id}/ask/release  → 201 { event, staged }
```

Both DM-only. The ask grounds in the **live table** — the live session,
the current scene with its cast, the active fight, recent session
events, the party's vitals — braided with the npcask machinery
(`internal/server/npcask.go`) when the question names an NPC: the mind
at the DM scope, the record scope-filtered at `npc:<id>` in SQL. What
the record does not contain, the NPC does not know, and the model
cannot leak it because it is never retrieved — the load-bearing test
asserts it on the request body the model received. The scene's secrets
in play ride the prompt as the DM's *release material*, marked hidden,
and the system prompt carries the rule the scope filter cannot express:
an NPC never speaks a clue their record does not carry.

**Clue-discovery awareness** rides both ends: the meta frame lists the
clues the table already holds (public facts plus accepted party
discoveries) and the clues still hidden in play; the prompt forbids
re-releasing a discovered clue as new. Where the answer invents
something, the `done` frame carries it as a **suggested release**, and
the box renders it with **Accept as canon | Modify | Discard**.

The release is the copilot's only write, and only on the DM's explicit
tap: it logs the discovery event against the live session (the
in-play-capture anchor — the event is the immutable record) and stages
the canon item in the review queue — an `npc_reveal` when the answer had
a voice, a `session_capture` proposed fact otherwise. Nothing writes a
fact or anything player-visible directly; the queue's decision remains
the only gate, exactly as for every other machine proposal. Discard
writes nothing at all.

## The live-context read

One route serves the strip:

```
GET /api/campaigns/{id}/live   → { live: { session, scenes } }
```

DM-only, like every spine read. `session` is the campaign's live session
(or null); `scenes` are the active scenes with cast attached. The payload
is a struct with no secrets field — the card never shows them, so the
wire cannot carry them (`internal/server/live.go`; the test asserts the
absence on the raw body of a scene that has one). Scenes change at
planning speed, not combat speed, so they ride this quiet read on a slow
poll rather than every board frame — the board snapshot stays what it
was.

## Legibility

The MAD-318 constraint, verbatim: every panel readable at arm's length
in a dim room. The clock is the biggest number on the strip, the scene
name the biggest word, chips are finger-sized, contrast is parchment on
stone with gold accents — the board's palette, not a dashboard's.
