# The table screen: the room's page

One URL, cast to the TV or projected: the public initiative order,
monster HP bars where the DM has chosen to reveal them, the turn
banner, big dice results from the shared feed — and nothing a player
could not already see. Each player's device keeps their private sheet;
the table screen is the *public* surface of the same state, driven by
the same campaign pub/sub as the party board, so it never needs a
refresh. Stage 8 of the mechanical layer (MAD-425), a stretch stage
built on the board (MAD-423) and the tracker (MAD-422).

## The access model

A screen link is a token: 128 bits from crypto/rand, minted and revoked
by the DM from the board window (`POST /api/campaigns/{id}/table-screen`).
The public routes live outside `/api` — the share page's rule — and no
session is required or consulted:

| Route | What it is |
|---|---|
| `GET /t/{token}` | the projector page (standalone, one script) |
| `GET /t/{token}/board` | the public snapshot, JSON |
| `GET /t/{token}/stream` | the screen, live (SSE) |

A revoked token answers 410 (`ErrRevoked`), an unknown one 404 — the
link says it was closed rather than pretending it never existed. An
open stream learns of a revocation on the next wake (the revoke pings
the campaign topic) and ends with a `gone` event. `last_seen_at` is the
projector's heartbeat, stamped where the token is resolved.

## What the room may read

Decided while the snapshot is built (`internal/table`), never filtered
after the fact — the same construction the board uses:

- **The party strips** — the board's *observer* standing: the
  campaign's visibility config (exact or word hp, visible or private
  slots) with no self exception. A word-mode table never spells a
  number, on the projector or anywhere.
- **The battle** — the public shape only: round, turn, the order.
  Initiative was rolled in the open; the names are the table's.
- **The other side** — nothing, until the DM reveals a foe. Then
  exactly what the reveal says: `hp` (numbers) or `word` (the table's
  health word). PCs cannot be revealed per-monster — their numbers are
  the config's business, not a toggle.
- **The dice** — the public feed, the same query a player's feed window
  runs. A secret roll is absent from the wire, not blanked.

The dice window is an adapter (`table.PublicRolls`) whose interface has
no `dm` parameter to pass — the screen's reads cannot ask for secrets,
by type.

## The reveal

`POST /api/campaigns/{id}/combats/{cid}/combatants/{ctid}/reveal` with
`{"mode":"off"|"hp"|"word"}` — DM-only, foe-only, journaled like every
other DM act on the battle (kind `reveal` in the combat log, mirrored
into the session log). The toggle rides the DM's board monster rows.
Reveal state lives on the combatant row, so it dies with the battle it
belonged to; the next fight starts hidden again, always.

## The stream

The board stream and the dice stream braided into one connection: an
`open` frame paints the whole screen and sets the roll cursor, board
events arrive only when the view changed (marshaled comparison — the
wire stays quiet between real changes), public rolls ride `roll` events
off the seq cursor, and a ping keeps proxies honest with a slow poll as
the safety net. A projector that drops and reconnects re-enters through
the open frame. The subscription is anonymous, like the dice feed's — a
projector is not a presence at the table.

## Legibility is the design problem

Dark room, couch distance: the projector page (`table.html`,
`table.css`) is near-black stone with parchment ink and a type scale
measured in viewport units — the turn banner leads, the party and the
revealed foes read at a glance, the newest die is the brightest thing
in its column. Nothing on the page hides anything; nothing hidden ever
arrives.
