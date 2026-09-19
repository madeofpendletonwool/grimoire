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

## Provenance — why is it 7/7, why did it die

Every computed value on the board opens the rows that produced it. The
deterministic half of the reasoning layer — no model, no tokens, a read
over the log that answers in milliseconds:

- **ⓘ on a permanent** — the characteristic stack, spelled row by row:

  ```
  base 2/2
  +1/+1 from Glorious Anthem      (pt_modify · while source present)
  +1/+1 +1/+1 counter
  +3/+3 from Giant Growth         (pt_modify · until end of turn)
  ────
  7/7
  ```

  The walk is CR 613's layer order — the same computation the board
  paints — so the trace and the number can never disagree. Non-P/T
  changes ride along with their sources too: control, types, colors,
  granted keywords. A `while source present` row whose source has since
  left the battlefield is shown but marked *not applied* — history, not
  arithmetic.
- **ⓘ why on a death in the log** — the walk back from that `DIED` row:
  the state-based action with its rule ("toughness was 0 or less — CR
  704.5f", "damage from a deathtouch source — CR 702.2c"), the full
  stack in force the *instant before* the death, the marked damage with
  each source named, and the last table act that set the sweep off. A
  rewind-and-refold to the moment, asserted rows only — viewers never
  re-run CR 704.
- **▸ on a turn row** — the turn's slice of the log, spelled by the
  same voice the log pane uses.

The panel is a row above the panes, never a modal — the board and the
log keep moving under it, and a rewind closes it because the rows it
spelled may no longer exist. The surfaces are `GET
/api/games/{id}/objects/{oid}/trace` (optionally `?at=` an ordinal for
a past moment), `GET /api/games/{id}/death/{ord}` and `GET
/api/games/{id}/turns/{n}` — pure folds, nothing materialized, so a
correction is reflected the moment it lands.

## Triggers — the don't-forget assistant

Commander turns stack triggers, and forgetting one is the most common
self-inflicted loss at the table. The play surface's answer is a
**trigger registry**: a trigger is registered once per card — typed by
a player at the object's strip (`⟡ trigger`), or proposed by the model
(`✨ propose`) and confirmed by the same register tap — against a
structural event the engine already emits: a land drop, a creature
entering, upkeep, an opponent's cast, an attack, a death, the end step.
The engine then fires it automatically, into the same queue a manually
declared trigger waits in, and it reaches the stack in APNAP order at
the next priority grant. No oracle text is ever parsed (ADR 11) — the
registration is declared knowledge, honest about where it came from,
and it is cached across games: register Rhystic Study once and every
future game knows.

- **The pending panel** — the stack column lists waiting triggers in
  resolution order (the row on top resolves first), each of the acting
  seat's own entries carrying ▲▼ controls, because CR 603.3b makes
  their order the controller's choice. A player may only reorder their
  own entries; the engine rejects anything else.
- **The nudge strip** — chips under the current action for what is
  unresolved: a trigger waiting to stack, a triggered ability still on
  the stack (the unpaid Rhystic), an attack trigger the active player
  has not used while attackers are still undeclared. Deterministic
  reads over the fold and the registry — no model in the path.
- **Honesty about firing** — a source that has left the battlefield,
  phased out, or left the game fires nothing. The one exception is the
  dying source's own "when this dies" trigger, which fires precisely
  because the source left.

The surfaces: `GET/POST/DELETE /api/games/{id}/triggers` (the registry
itself), `POST /api/games/{id}/triggers/propose` (the model's
candidate, gated onto the vocabulary — nothing is written until the
confirm tap), and `GET /api/games/{id}/nudges` (the strip's read).

## Deck odds — outs, exact probabilities, mulligan advice

With a deck attached and the log recording every card that left the
library — drawn, played, milled, exiled — the remaining library is
**derived, never guessed**: it is the fold's composition bookkeeping,
and the `🎴 odds` panel does exact arithmetic over it. No model in the
path; every answer is a hypergeometric with its rational shown, so the
same question over the same game answers byte-identically twice.

- **Draw probabilities** — "chance of a land in the next three",
  "chance of finding a board wipe by turn nine" (one draw per own
  turn, stated in the answer). Categories reuse the deck report's own
  classification — lands, board wipes, ramp, draw, interaction — and a
  named card counts its own copies.
- **Outs** — "what are my outs against an enchantment": the card
  index's full-text search proposes candidates over type line and
  oracle text, the remaining library vetoes everything not still in
  it, and the local pass labels why each survivor answers. A card that
  was drawn is not an out; a sweeper of creatures does not answer an
  enchantment.
- **Mulligan advice** — type the opening hand, get keep-or-mulligan
  with the arithmetic: the hand's land percentile (exact), ramp and
  interaction in hand, early castability. It is **opt-in per game**
  (`mulligan_advice` in the game's settings, off by default) because
  some tables will not want it.
- **Composition, never order** — a library is a multiset. Questions
  that depend on order ("what's my next card?", "when will I draw a
  Wrath?", "top card?") are refused out loud: *library order is never
  modelled*. Aggregate horizons are composition; positions are not.

The surfaces: `GET /api/games/{id}/library` (the derived composition),
`POST /api/games/{id}/odds` (question or structured shape),
`POST /api/games/{id}/outs` (a board object id or free text), and
`POST /api/games/{id}/mulligan` behind the opt-in. An install without
the card index still answers named-card odds from the composition
alone and says what is missing for the rest.

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

## The pod: four players, four clients, one game

A Commander table is more than one tracker, so a game is joinable:
**the host shares a six-character code** (or a `?join=` link), a friend
redeems it while the game is in setup, and they land in the next seat —
their client becomes their chair. What each client sees is scoped in
SQL, never on trust ([ADR 13](../decisions.md#adr-13-hidden-zones-are-authorization-in-sql-not-instruction)):

- **Public to everyone** — the board, the stack, graveyards, exile, the
  command zone, life and every counter, hand *counts*, library *sizes*,
  and the whole public log.
- **Your seat's alone** — your hand's known cards, your library's exact
  composition, your decklist. The identification tiers and the
  shared name cache answer you from your own deck and the global index,
  never another seat's.
- **Nobody's but your own** — your private notes pad, saved as you
  type. Not even the game's host reads a seated player's pad; a local
  seat's pad is the host's, because the host's client runs it.

The host keeps the room: seating, decks, start, rewind, amend and the
per-game settings are host controls; a participant talks, taps, asks the
judge and takes their odds as their own seat. Correction for a
participant is the current-action pane's one-tap accept/fix — the log's
truncate controls are the host's, because the log is the shared game.

The gate that keeps this honest runs in CI: the deterministic
`hidden_zone_leak` check joins every rendered surface — responses,
assembled prompts, the works — against per-seat entitlement, with
marker decks, marker draws and marker notes planted so a missing filter
fails the build instead of a friendship.

## Judges, spectators and the ruling log

The same code opens a third door: **join as judge or spectator**, at any
point in the game's life — seats close when play begins, but a dispute is
exactly when a judge arrives. Both roles watch the public game: board,
stack, event log, turn and priority. Neither sees any seat's hidden
zones — the observer's read runs the same SQL scoping the pod built, so
the CI leak gate covers the judge's screens exactly as it covers a
seat's.

The judge additionally holds the table's tools pointed at the shared
state:

- **The rules judge** — the ⚖ ask, made as the table itself: the prompt
  carries the public fold, never a seat's private zones.
- **The traces** — the ⓘ on any computed value, the why-did-it-die walk,
  a turn's slice. Pure reads over the log the panes already render.
- **The ruling pen** — ⚖ on any log entry records a ruling anchored to
  that ordinal: *what the table decided and why*, straight against the
  event it concerns rather than against memory. The ruling log rides
  under the event log, lands on every attached client live, and
  **survives every rewind** — including the one the ruling itself
  ordered — because a human record is never clobbered by a truncate.

A spectator watches with no pen; a seated player asks the judge rather
than ruling on their own game; the host may rule too, a solo table's
owner being its judge. An observer's client is honestly read-only: the
writer surfaces hide, and the server would refuse them anyway.

The surfaces: `POST /api/games/join` with `role` (`judge` \|
`spectator`), `GET /api/games/{id}/rulings` for the log, `POST
/api/games/{id}/rulings` (`{ord, note}`) for the pen — judge or host
only — and the stream's `ruling` frame for live arrival.

## Replay — the log, played back

State is a fold over an immutable event log, so **the board at any past
ordinal is `fold(events[:n])`** — the prefix property the engine's own
tests prove. Replay is therefore a viewer over data the log already
holds: nothing new is stored, nothing is materialized, and every
position is one cheap read the server folds for you (`GET
/api/games/{id}/replay?at=N`, scoped in SQL like every other read — a
seat scrubs its own stream, the host folds everything, a judge or
spectator scrubs the public game).

**▶ replay** in the turn strip opens the bar once a game has started —
finished games are the natural home, but scrubbing a live game to ask
"what did the board look like on turn 5?" is the same read:

- **The slider** is the whole point: every ordinal is a position. The
  board, stack and strip repaint the folded past; the log dims its
  future rows and marks the row you sit on.
- **Step and turn controls** — one event (◂ ▸), one turn boundary
  (◂ turn / turn ▸), or ▶ to watch it play back event by event; from
  the head, playback starts over at the beginning.
- **The live game never leaves** — the stream keeps running under the
  bar, the writer surfaces stand down while the panes show the past
  (tapping one says so), and **✕ live** re-reads the server's fold and
  puts every pane back on it. A rewind announced while you scrub
  closes the bar for you: the log it was over no longer exists.

## The post-game coach

**🎓 coach** reads the recorded log the way a coach debriefs a game:
the **deterministic facts arrive whole first**, then the model's
interpretation streams after them — and the facts are the product, so
an install with no LLM configured still gets the whole first half,
with a plain note about what is missing.

- **Per seat, private to that seat.** A seated player reads their own
  report; the host reads any seat's (a solo table's chair is every
  chair); a judge or spectator reads the table's public summary. The
  privacy is construction, not filtering: the report folds the
  asker's scoped stream, so another seat's rows were never read —
  the same CI leak gate that covers every other surface covers the
  coach's prompt and reply.
- **Resource usage** — cards left in hand at the end (and the ones the
  seat had seen), casts, lands, draws, damage dealt and taken, life
  lost and gained, and the final-turn picture: unspent mana sources
  at the turn's end, land drops, hand size. Honesty rule and all:
  **floating mana is never modelled** — untapped lands are the read,
  and the panel says so.
- **Missed triggers** — today's trigger registry replayed over the
  log, diffed against the `TRIGGER_FIRED` rows that actually landed:
  *"Rhystic Study (may draw a card) — Bob casts Sol Ring at #412."* A
  registration that arrived after the game explains a gap, and the
  coach says so rather than blaming the player.
- **The interpretation is an LLM read over a deterministic log.** It
  interprets recorded facts — biggest mistakes, best play, the
  turning point — cites ordinals, and never invents state; where the
  log could not know something (unknown hands, unidentified cards),
  it is told to report the gap rather than guess.

The surface is `POST /api/games/{id}/analysis` (`{seat}`), streaming
the judge's SSE framing: `meta` carries the deterministic summary,
`delta` the debrief.

## Where the truth lives

The board you see is the server's fold of the game's append-only event
log — never a client-side guess. The stream's only job is to say "an
ordinal landed"; the panes re-read. See [the table data
model](../table/model.md) for the contract and [Interaction at the
table](../table/interaction.md) for the product bar this surface is the
first half of.
