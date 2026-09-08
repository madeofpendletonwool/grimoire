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
- **Ask Grimoire.** The mount point, warm and honest: the session
  copilot (Stage 5, MAD-486) lands here.

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
