# Interaction at the table

The product contract for how Grimoire behaves during a game. The engine side
of this is [the data model](model.md); this page is the half a player
actually touches, and it is written before the UI exists for the same reason
the model was: so "we'll sort out the correction flow later" is not available
as an option.

## The bar

From the plan, and it is the whole design brief:

> **Correction must be cheaper than manual entry.**

If Grimoire requires *cast card → open phone → find card → add card → update
state → close phone*, it has already lost — people would rather use a die for
life and pocket change for counters. So the bar is not "can it track a game."
It is: telling Grimoire what happened must cost less than writing it down,
and being told wrong must cost almost nothing.

Stated as testable product constraints, pinned here and repeated as
acceptance criteria in the issues that build them (MAD-328):

- Correcting a misidentified card from its log entry costs **at most two
  taps**.
- Undoing the last event costs **one tap**.
- No correction path opens a modal that **blocks the log**.
- Any clarification Grimoire asks is answerable in **one tap or one spoken
  word**.

These are criteria a reviewer can count. "Easy to adjust" that cannot be
counted is a vibe, and vibes do not survive contact with a four-player
Commander game.

## The confirmation ladder

Every proposed action carries a **disposition**, assigned from the
resolution confidence, and the disposition decides the UI:

| Disposition | When | What happens |
|---|---|---|
| `auto` | High confidence, cheap to undo | Applied immediately. Appears in the log with a one-tap undo. Life totals, tap/untap, draws, land drops, damage. |
| `confirm` | Applied, but worth a look | Applied **optimistically** and highlighted in the current-action pane with accept / fix. Card identification below the certainty bar. |
| `ask` | Genuinely ambiguous | **Not** applied. One question, with tappable answers. |

The ladder is a confidence valve, not a speed setting. `auto` exists because
"Collin takes 3" is never ambiguous and a confirmation tap for every one of
them is the manual-entry tax this design exists to avoid. `ask` exists
because a silent wrong write is the one failure this design cannot tolerate —
and because the answer to a good question is cheap (one tap), while the fix
for a bad write is only *cheap* (two taps) if everything else held.

Nothing is ever applied twice: a rejected action produces zero events, and an
`ask` that is never answered simply expires into the unresolved tray. The log
only ever contains what actually happened.

## The one-tap rule

The rule that makes `ask` tolerable at a real table:

> **Every clarification must be answerable in one tap or one spoken word, and
> must never block the log.**

If a question cannot be reduced to a small set of tappable answers, it does
not get asked. It goes to the **unresolved tray** (`mtg_pending`, open) and
play continues. A modal that stops the game is worse than a wrong board
state, because a wrong board state costs one tap to fix and a stopped game
cannot be un-stopped.

Two clarifications, concretely:

- *"Bob — pay {1} for Rhystic Study?"* → **✓ pays / ✗ doesn't.** Good: one
  tap, both answers meaningful, the game is not waiting on it.
- *"What exactly did Bob cast?"* → not asked. Parked, and the log keeps
  moving; the answer arrives when a player types or says it.

## Tracking levels

The engine models the unknown honestly — `unknown` is a real value, never
collapsed into "empty" — and the UI says so rather than guessing:

| Zone | Level | What the player sees |
|---|---|---|
| Battlefield / stack / graveyard / exile / command | **tracked** | Everything: objects, counters, modifiers, attachments, tapped state. |
| Life, poison, commander damage, energy, every named counter | **tracked** | Live values, every change sourced in the log. |
| Hand | **count only** | A number. Contents appear only when revealed — and a reveal is public. |
| Library | **composition, never order** | Composition when a deck is attached. **Order questions are refused, out loud** — "library order is never modelled" — rather than answered plausibly. |

Once a tracker pretends to know something it cannot know, it is untrustworthy
— and once it is untrustworthy, nobody opens it again. Refusing is a feature.

## Correction

Because state is a pure fold over an append-only log
([ADR 9](../decisions.md#adr-9-the-event-log-is-the-only-writer-state-is-a-fold)),
correction is not a feature bolted on; it is the natural operation:

- **Undo** — rewind by one. The always-visible single tap.
- **Amend** — rewind to an entry, substitute the corrected action, re-apply.
  The common case is a misidentified card ("no, *Smothering Tithe*, not
  Rhystic"), reachable from the log entry itself, prefilled from the entry's
  recorded cause. Two taps: *not it* → the fix.
- **Rewind to ordinal** — truncate at *N* and re-fold. Every entry in the log
  is a rewind point.
- **Direct board edit** — tap any value on the board; the edit is an ordinary
  Action (`SET_COUNTERS`, `CHANGE_LIFE` with `to`), so a hand correction and
  an engine action share one path and one audit trail.

Multi-client safety: the writer announces the rewind on the ordinal stream
and every attached client re-folds. No client is ever left folding a log that
no longer exists.

## Voice

**Push-to-talk, not ambient listening** — a deliberate v1 choice. Cross-talk
at a four-player table is unsolved, and holding a button solves speaker
attribution for free: **the seat holding the button is the seat that acted**.
The interim transcript shows in the current-action pane so a misheard word is
visible *before* it becomes an action, and the resolved phrase lands on the
ladder like any other input. Ambient listening, diarisation, and cameras are
later experiments; nothing in the plan depends on them.

## What the engine refuses

The refusals are the product's honesty, and they are load-bearing:

- **Never a silent wrong write.** Ambiguity becomes a question or a parked
  item, never a guess.
- **Never a blocking modal.** Questions ride alongside the log; the log never
  waits.
- **Never library order.** Not modelled, not implied, answered with a refusal
  when asked.
- **Never card behaviour from oracle text.** Declared or confirmed deltas
  only — [ADR 11](../decisions.md#adr-11-card-behaviour-is-declared-never-simulated).
- **Never a hidden-zone leak.** What a seat may not see is filtered in SQL,
  not requested in a prompt — [ADR 13](../decisions.md#adr-13-hidden-zones-are-authorization-in-sql-not-instruction).
