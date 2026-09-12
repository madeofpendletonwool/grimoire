# Play — the table, live

The play window is the Magic table's tracker: **board state, event log,
and current action, all visible at once**, live for everyone watching the
game. Open it from the tool picker (`Ctrl+G` then `P`) in the Magic shell.

It is a tracker with no AI anywhere in it — deliberately. The tap surface
is the correction UI the voice and grammar layers will lean on, it exercises
the deterministic engine with real hands before a model ever touches it,
and it is the fallback for the moment something is misheard. Every change
you make is an action the engine validated; there is no second write path.

## The three panes

- **Board** — one strip per seat: life (tap it), every counter the game
  has invented (tap one), commander damage, hand and library counts, and
  the battlefield — identical tokens collapse into one chip (`3× 1/1
  Soldier`), permanents show their *computed* power and toughness with
  counters and attachments. The stack rides above the seats, top first,
  one tap from resolving.
- **Log** — newest first, every entry reading like the table would say it
  ("Collin casts Rhystic Study", "Bob pays", "Atraxa dies"), every entry
  carrying its cause. Every entry is a rewind point: its `⟲` truncates
  the log there and the state re-folds.
- **Current action** — what the engine last applied, highlighted until
  you acknowledge it (`✓ yes`) and correctable one tap away (`✎ not it`
  prefills the composer from the entry's own recorded cause; submitting
  rewrites it). `⟲ undo` in the log header rewinds the whole last action.

## Tracking a game by tap

- **Setup** — new game, seat players (name + commander), start. Seats,
  life and commanders are echoed into the log, which is what makes it
  self-describing.
- **The turn strip** — whose turn, which step, who holds priority, and
  the controls: next step, pass, draw, untap all, resolve combat. The
  *acting as* picker is which seat the composer submits for; it follows
  priority unless you pin it, and `Give priority →` rotates priority to
  it with real passes.
- **Cards** — type a name (autocomplete against the card database), then
  **Cast** or **Play land**; the card's type line, colors and numbers
  ride along as declared base characteristics. Commanders cast from the
  command zone in one tap with their tax shown.
- **Values** — tap life for ± steppers, tap a counter chip to adjust it,
  tap hand or library for draws, mills and honest corrections of a
  count. Damage with a spoken source, any named counter on any seat or
  permanent, and zone moves (sacrifice, destroy, bounce, exile) from any
  permanent's sheet.
- **Combat** — during declare attackers, tap the attacking creatures and
  pick the target; during declare blockers, tap blockers onto each
  attacker; the engine owns the damage arithmetic (first strike,
  trample, deathtouch, lifelink and friends) when you resolve.

## Honesty rules the surface inherits

- **Unknown is a value.** A hand the tracker was never told about reads
  `?`, not `0`. A library's size is known when a deck is attached. A
  card nobody identified stays unidentified.
- **Nothing is applied twice.** A rejected action writes nothing; the
  pane says why.
- **Correction is cheaper than entry.** Undo is one tap, amending a
  misidentified card is two, and no correction path ever covers the log.
- **Multi-client by construction.** One writer assigns ordinals; the
  stream replays from the last ordinal a dropped connection saw, and
  announces rewinds so every client re-folds rather than holding rows
  that no longer exist.

## Where the truth lives

The board you see is the server's fold of the game's append-only event
log — never a client-side guess. The stream's only job is to say "an
ordinal landed"; the panes re-read. See [the table data
model](../table/model.md) for the contract and [Interaction at the
table](../table/interaction.md) for the product bar this surface is the
first half of.
