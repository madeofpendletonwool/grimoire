# The encounter director panel: advisory tactics, cited

Mid-battle, the DM's hardest question is tactical: *what would these
monsters actually do next?* The director answers it with advice — never
with actions. Given the live battle it suggests plausible monster moves,
each one carrying the state that justifies it, and the DM decides
everything. Stage 3 of play mode (MAD-484); the engine is MAD-427
(`internal/director`), the screen it mounts on is
[MAD-485](dm-screen.md), the tracker it sits beside is
[MAD-487](combat.md).

## The panel

A window in the play layout (`web/static/js/director.js`), seeded behind
the board's tab in **Alt+2** — beside the tracker, one tap from it,
never crowding the controls the DM is mid-tap on:

- **The ask.** An optional focus question ("can the dragon breathe
  yet?") and one Suggest action. Blank is fine — the director advises
  on the battle as it stands, and asking again refreshes.
- **The suggestions.** Actor, action (the arm's-length line), the
  reasoning — and *the state that justifies it*: every basis citation
  in full, statblock text and live state told apart by their badges,
  never collapsed away.
- **The honest tail.** The `dropped` count (suggestions the citation
  gate caught), the grounding's `caveats`, and the model's name —
  rendered below the advice, not tucked into a tooltip.

## One call, no writes

The panel's only wired route is the engine's own:

```
POST /api/campaigns/{id}/combat/director   {question?}   (DM)
```

There is no write path in the panel — by construction, not policy: the
module imports no combat write wrapper, so no code path in it can
change battle state. Advisory means the suggestions exist in the
response body and nowhere else; the tracker, not the director, acts.

## The render gate

The server's citation gate (`internal/director`) drops any suggestion
that cites no basis, and rejects invented numbers — every number in a
suggestion must appear in the text of a basis line it cites. The panel
enforces the same rule again at its render boundary
(`web/static/js/directorvm.js`): a suggestion that arrives without
citations cannot become a card, whatever the wire said. Both halves are
tested — `jstest/directorvm.test.js` for the panel's.
