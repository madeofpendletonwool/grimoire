# Play — the table, live

The play window is the Magic table's tracker: **board state, event log,
and current action, all visible at once**, live for everyone watching the
game. Open it from the tool picker (`Ctrl+G` then `P`) in the Magic shell.

It is a tracker whose core has no AI anywhere in it — deliberately. The
tap surface is the correction UI the voice and grammar layers lean on,
it exercises the deterministic engine with real hands before a model
ever touches it, and it is the fallback the moment something is
misheard. Every change you make is an action the engine validated; there
is no second write path.

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

## Talking to the table

The **say** strip under the current action takes table talk — typed, or
spoken through the **hold-to-talk mic** beside it. Every utterance walks
the same pipeline: the deterministic grammar first (instant, offline,
consistent), and what it refuses goes to the model fallback, which sees
the live game state and the known-card universe and is gated onto the
same deterministic lookups the grammar trusts. A model reply that
invents a card, misnames a seat, or reaches outside its small action
vocabulary is refused — a no-parse, never a guess.

**Push-to-talk, not ambient listening** — a deliberate v1 choice.
Cross-talk at a four-player table is unsolved, and holding the button
solves speaker attribution for free: **the seat the *acting as* picker
names is the seat that spoke**. Hold the mic, say it, release; the
transcript enters the say strip's pipeline marked as voice, and the
ladder lands it exactly as a typed utterance lands.

The mic takes whichever path this browser and install can offer, and is
simply absent when neither exists — the same "unset means not there"
contract the embeddings keep:

- **Web Speech** (Chrome/Edge over HTTPS) — the interim transcript
  streams into the current-action pane while you hold, so a misheard
  word is visible *before* it becomes an action.
- **The server endpoint** (Firefox, Safari — any browser that can record
  a clip, when `TRANSCRIBE_MODEL` is configured) — the clip is posted on
  release and transcribed through the same OpenAI-compatible endpoint
  the session audio hook uses ([ADR 5](../decisions.md)). The clip is
  never written to disk; it goes from the request to the endpoint and
  stops existing.

What comes back is the **confirmation ladder's** verdict, and the
verdict decides what you see:

- **auto** — high confidence on a cheap-to-undo shape (life,
  tap/untap, draws, land drops, damage, pass). Applied immediately; its
  log entry's `⟲` is the one-tap undo. Nothing the model emits ever
  lands here — the auto tier is the deterministic layers' alone.
- **confirm** — applied optimistically and marked *"applied with a
  look"* until you `✓` it. `✎ not it` corrects it in the usual two
  taps. Fuzzy card identification and everything the model parsed live
  here.
- **ask** — genuinely ambiguous, **not applied**. The question appears
  in the strip below with tappable answers (the deck's candidate
  cards); one tap applies the answer's action and caches the
  identification, so the same mumble is never re-asked. A question that
  cannot be reduced to tappable answers is *parked* instead — it waits
  in the strip with a typed-answer box while the log keeps moving.
  Rewinding past the entry a question was asked about closes it.

`POST /api/games/{id}/intent` is the pipeline's surface (`{seat,
text}`), and `GET /api/games/{id}/pending` plus `POST
/api/games/{id}/pending/{pid}` are the tray's. The mic's server path is
`POST /api/games/{id}/transcribe` — one short clip in, its transcript
out, in the same request. No path through the pipeline can block the
log: questions are rows beside it, never a modal over it.

## The rules judge

**⚖ judge** in the turn strip opens the judge panel — *ask Grimoire
anything about the game you're currently playing*. The question rides
the **live** board, stack, priority holder and step, folded from the
same event log every pane reads, so nobody types their board in again:
the board's permanents (with computed P/T, counters and attachments),
the waiting triggers and the stack in resolution order, whose turn it
is, which step, who holds priority, every seat's visible numbers, and
the tracked zones beyond the battlefield all travel with the question.

That position is what makes the questions a tracker could never answer
answerable at one tap: **what resolves next**, **can I respond to
this**, **what happens if I counter this**, **is that a legal target**.
Quick chips ask the first of these outright; anything else can be typed.

The answer is the interaction resolver's, verbatim: it grounds in real
card oracle text (Scryfall) and the interaction chapters — 117
timing/priority/stack, 603 triggered abilities, 613 layers, 616
replacement effects — walks the ruling step by step citing each rule,
and shows its citations under the answer exactly as the resolve mode
does. It remains an **assistant, not a Comprehensive Rules oracle**, and
the panel keeps saying so. What the table cannot verify it says plainly:
hands are count-only (only cards spoken or revealed are known, and only
the asking seat's), library order is never modelled, and unidentified
permanents stay unidentified — the answer is told to report the gap
rather than guess. Asking never touches the log; it is a read over the
fold, and the board and log keep moving under the panel while the
answer streams.

`POST /api/games/{id}/ask` is the surface (`{seat, question}`), streaming
the resolver's SSE framing (meta carries the citations, delta the
answer). It requires the LLM configured — unset means the judge button
stays honest about being unreachable rather than failing mid-question.

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
