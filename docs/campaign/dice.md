# Dice with provenance: the roller and the shared roll feed

Rolls are facts. Every roll carries who made it, the formula, the natural
dice, the modifiers, the total, what it was for — and a **visibility**.
Public rolls land in the shared party feed, live, on every screen
watching the campaign; secret rolls are the DM's alone, and their absence
from player-scoped reads is enforced in the query, not hidden in the UI.
Stage 3 of the mechanical layer (MAD-420); the design decision is
[ADR 17](../decisions.md#adr-17--dice-are-facts-with-visibility-a-counter-based-rng).

## The formula grammar

```
expr   := ['+'|'-'] term (('+'|'-') term)*
term   := dice | number
dice   := count? 'd' sides keep?
keep   := ('kh' | 'kl') number?
```

- `2d6+3`, `1d20+5`, `8d6-2`, `1d8+2d6+4` — sums of dice and constants.
- `4d6kh3` — roll four, **k**eep the **h**ighest three (the stat-roll
  spelling); `kl` keeps the lowest. `kh` alone keeps one.
- `2d20kh1` is advantage written out; `2d20kl1` is disadvantage.
- Whitespace-tolerant, case-insensitive. Everything else — `1d20*2`,
  `2d6+`, `banana` — is a **400 with the reason**, never a guess.
- **Advantage and disadvantage are a mode, not syntax**: the roll bar's
  `adv`/`dis` toggles (or `mode` in the API) apply to a formula with
  exactly one plain `d20` term, rewriting it to `2d20kh1` / `2d20kl1`.
  Advantage on `2d6` is refused with a clear error — damage does not take
  advantage.

The declared context vocabulary is the game's own: `attack`, `save`,
`check`, `damage`, `initiative`, `table`, `other` — with free text in
`detail` ("Fireball at the goblins").

## Rolls are session events

A roll made while a session is live mirrors into the session log as a
kind `roll` event — the notation and every die in the payload — so the
[session export](sessions.md) prints the roll's line and later stages
replay it. A campaign with no live sitting still gets its roll (the
roller is always usable); the log simply was not open.

## Visibility

| | DM | player |
|---|---|---|
| rolls | anyone, optionally `secret` | public, as their bound character |
| reads | the whole feed | public rolls only |

- A player cannot roll `secret` (403), cannot roll as another character
  (403), and their rolls land public under their bound pc's name without
  asking.
- The feed query filters visibility itself — `internal/dice` leak tests
  assert a secret roll's id and content are absent from player reads,
  REST and stream alike. Same word as the knowledge layer: dice are facts
  with visibility.

## The seeded RNG

Each campaign owns a 64-bit seed, minted once; each roll's per-campaign
seq doubles as the nonce. `(seed, nonce, formula)` reproduces the dice
exactly — pinned by golden files at the engine and re-derived in tests at
the store. A roll never draws from a shared stream, so replaying roll #7
does not depend on rolls #1–6: the property Stage 9's session replay
stands on. The rolled values are stored too — the feed renders without
re-deriving, and the stored bytes are the replay's oracle.

## The surface

- **The Dice window** (`Ctrl+G i`): the stage — where a roll is *revealed*
  on a timeline (tumble, settle, modifiers, the total stamps; natural 20s
  and 1s earn their flourish) — above the roll bar, quick dice, and the
  live feed.
- **Quick rolls**: one-tap chips derived server-side from the typed sheet
  (checks, proficient saves and skills, initiative, spell attack) — no
  surface re-derives proficiency bonuses of its own. A player's chips are
  their bound character's; the DM picks a pc from the party.
- **The roll curtain**: every public roll drops a compact plate from the
  top of the app on *every* screen watching the campaign — the dice, the
  total, whose it was — whether or not the Dice window is open. The
  anticipation is shared by construction.
- **`/r 3d6+2`** in the campaign chat composer (players included — dice
  need no model key) posts through the same surface and lays an inline
  card in the transcript.
- **The combat tracker and the party board** (Stages 5–6) get their roll
  bars and quick buttons from the same API: `POST /rolls` is the one
  door.

## API

```
POST /api/campaigns/{id}/rolls          {formula, mode?, visibility?, context?, detail?, target_id?, character_id?, session_id?}
GET  /api/campaigns/{id}/rolls?after=&limit=      the feed window + latest cursor
GET  /api/campaigns/{id}/rolls/stream?after=      SSE: open | roll | ping
```

The stream resumes from the feed's cursor, filters visibility with the
same query the feed uses, and cannot leak what the feed would not show.
