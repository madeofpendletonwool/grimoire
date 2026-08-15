![Study](../assets/sprites/hourglass.png){: style="float:right; margin-left:1rem" align=right }

# Study Mode

Study mode (the flashcard button in the sidebar) turns the corpus into a spaced-repetition flashcard deck — for new players and judges-in-training.

The corpus is already a question bank:

- **MTG keyword abilities** — chapter 702, one card per keyword: Deathtouch, Ward, …
- **D&D SRD conditions** — Blinded, Charmed, Exhaustion, …
- **D&D SRD spells** — Fireball, Hunter's Mark, Misty Step, … one card per spell entry, the front asking what it does

so the cards are generated from the existing FTS5 index rather than authored by hand. No LLM is required. Where a corpus offers more than one deck (D&D today), tabs above the cards switch between them; each deck keeps its own schedule.

## A session

A session deals the cards due now plus a few new ones. Each card has:

- a **front** — the keyword name or condition to recall
- a **back** — the full rule text

Grade each card **Again / Hard / Good / Easy** and an **SM-2** scheduler reschedules it: an Again brings it back within the session, a Good/Easy spaces it out by days that grow with each correct recall and the card's personal ease factor.

## Where progress lives

Schedules live in a per-user `reviews` table in the same SQLite file as the rest of the app, so progress survives reloads and reindexes — the concept keys are stable rule numbers (MTG) or stable section ids (D&D). Study progress is per account; see [Accounts](../deployment/accounts.md).
