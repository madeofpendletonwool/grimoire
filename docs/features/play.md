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
  the log there and the state re-folds. Every entry is also correctable:
  its `✎` prefills the composer from the entry's own recorded cause, and
  submitting rewrites the entry — amend, two taps, everything after the
  entry undone with it. The server does the truncate and the re-apply in
  one transaction, so a rejected correction changes nothing and no other
  client ever sees the halfway state.
- **Current action** — what the engine last applied, highlighted until
  you acknowledge it (`✓ yes`) and correctable one tap away (`✎ not it`
  prefills the composer from the entry's own recorded cause; submitting
  rewrites it). `⟲ undo` in the log header rewinds the whole last action.

## Tracking a game by tap

- **Setup** — new game, seat players (name + commander, and optionally
  one of your saved decks), start. Seats, life, commanders and every
  attached deck's composition are echoed into the log, which is what
  makes it self-describing.
- **Decks are the identification universe.** Attach one per seat and a
  mumbled name matches the few hundred cards actually on the table
  instead of the whole index: "Rhystic" means *your* Rhystic Study, and
  the speaking seat's copy always wins a two-deck tie. Attachment is
  encouraged, never required — a deckless table works, identification
  just falls back to the global index and gets worse. The engine knows a
  library's *composition* once its deck is attached; **order is never
  modelled**, and order-dependent questions are refused out loud rather
  than answered plausibly. `POST /api/games/{id}/resolve` with a seat
  and a spoken name is the surface the intent pipeline inherits: it
  answers with the card, how it matched (deck exact, deck fuzzy, global,
  or the game's own cache of prior corrections) and a confidence the
  confirmation ladder keys on. The same mumble is never re-resolved —
  the first answer is cached per game, and a human correction ("no,
  *Smothering Tithe*") becomes that name's answer from then on.
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
  misidentified card is two (`✎` on its log entry, then the composer's
  submit — prefilled, so the fix is usually typing the right name), and
  no correction path ever covers the log: the edit rides a strip above
  the composer, not a modal.
- **Multi-client by construction.** One writer assigns ordinals; the
  stream replays from the last ordinal a dropped connection saw, and
  announces rewinds and amends so every client re-folds rather than
  holding rows that no longer exist.

## Where the truth lives

The board you see is the server's fold of the game's append-only event
log — never a client-side guess. The stream's only job is to say "an
ordinal landed"; the panes re-read. See [the table data
model](../table/model.md) for the contract and [Interaction at the
table](../table/interaction.md) for the product bar this surface is the
first half of.
