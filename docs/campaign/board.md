# The party board: the table's live view

The board is the surface mid-play actually looks at: one strip per
party member — hit points, conditions, concentration, the slot summary,
pool warnings (*"2 HP"*, *"out of 3rd-level slots"*) — plus who is at
the table, and when a fight runs, the public shape of the battle. It
never has a refresh button: every change lands live over the campaign
pub/sub, on every screen watching, phone included. Stage 6 of the
mechanical layer (MAD-423); the design decision is
[ADR 19](../decisions.md#adr-19--a-campaign-pubsub-and-visibility-built-by-construction).

## The push

One in-process broker, topics are campaigns (`internal/pubsub`). Every
mechanical store pings its campaign after a commit — a ledger spend, an
applied condition, a combat write, a membership change, a settings
change — and each open stream wakes, re-derives the snapshot **with its
own viewer's scope**, and sends it only if it changed. Pings carry no
data, so the push channel can never widen what a reader may see: it only
makes the allowed read arrive sooner. The dice feed rides the same
broker (its old private one became this one).

A stream that misses a ping (proxies, naps) is covered by a slow poll;
identical snapshots send nothing, so the wire stays quiet between real
changes.

## Where the numbers live

| Strip field | Source |
|---|---|
| classes, level, AC | the typed sheet (MAD-418) |
| hp / temp hp, mid-fight | the combat tracker's rows (MAD-422) |
| hp, between fights | the ledger's hp pool — derived from transactions, never stored |
| slots, pools, warnings | the resource ledger's derived balances (MAD-419) |
| conditions, concentration | the effect engine (MAD-421) |
| round / turn / order | the active combat, names and positions only |
| the monster side | the DM's read — every number |

**Hit points are a pool now** (`kind=hp`, size = the sheet's max,
recovery manual — the 2014 long rest returns hit dice, not health). A
battle starts from the ledger's truth, not the sheet's max, and a
battle's end writes each survivor's final hp back as a visible `set`
transaction: the tracker is the fast state during the fight, the ledger
is the truth between them, and neither invents the other's numbers.
Out-of-combat damage is an ordinary ledger transaction through the
resources surface.

## Visibility is data, enforced by construction

Tables differ, so the board's visibility is **configurable per
campaign**, set by the owner (`PUT …/board/settings`):

| | values | meaning |
|---|---|---|
| `hp` | `exact` \| `word` | numbers, or the table's own word |
| `slots` | `visible` \| `private` | everyone's summary, or each player's own |

The words are a declared vocabulary — *unhurt, hurt, bloodied* (half max
or less, the 2014 term), *down, dead* — never free text.

The enforcement point is the snapshot builder: a hidden field is **never
placed on the view struct**, so it is absent from the JSON — not zeroed,
not hidden in the client. The leak tests assert absence on the raw wire
body. Three parties read the same endpoints:

- **the DM** — every number, every member, plus the monster side;
- **a player** — the config's shape, with one exception: their own
  strip is always exact (5e players know their own numbers);
- **an observer** — the config's shape, no self exception to claim.

An unparseable stored config fails **closed** — word mode, private
slots — because a config nobody can parse should hide, not show.

## Presence

Holding the board stream open is being at the table. Presence is the set
of users with a stream on the campaign — in-memory, ephemeral by
design, counted once per user however many windows stream. The stream
starts at app boot (like the dice curtain), so arriving means opening
the book, not finding the right page; leaving closes the stream and
re-renders everyone else's board.

## The API

```
GET  /api/campaigns/{id}/board           the snapshot
GET  /api/campaigns/{id}/board/stream    SSE: open, board, ping
PUT  /api/campaigns/{id}/board/settings  the visibility config (owner)
```

Any member reads; the member row is the gate, as everywhere. The stream
sends the full snapshot per change — the payload is a party, not a
cursor — and the client applies it idempotently.
