# Deck Builder

The deck builder (the deck-box button in the sidebar) helps you build a Commander deck from an idea — *"Kaalia, aggressive angels and dragons, mid budget"* — and to analyze a list you paste in.

## The flow

1. **Describe the idea.** Free text, optional color constraint (`Mardu`, `BRW`). The server proposes commanders from the local card database — commander-legal, color-legal, ranked by EDHREC popularity, narrowed by your terms — with a one-line model-written "why" for each.
2. **Draft.** Pick a commander and the model drafts the 99 from a candidate list the server prepared and verified: identity-legal cards with their real oracle text and ranks. Every card the model names is checked against the card database before you see it; a name that isn't a real card is flagged, never displayed as one.
3. **Revise.** "More interaction", "cheaper curve", "cut the salt" — the model re-picks, and the report re-computes.
4. **Analyze.** Paste any decklist and get color-identity checks, curve, land/ramp/draw/interaction counts, salt warnings, and a model-written critique — grounded in the deterministic analysis. If the list has no `Commander:` line, name the commander in the field under the box.
5. **Talk about it.** Ask follow-up questions about whatever deck is on screen — what to cut, why the curve is off, whether a card is earning its slot. The model gets the real list, the computed analysis, and the real oracle text of any card you name, and is told to say so rather than guess when a card's text wasn't provided.

Decks save per account, export as text, and reload with their report recomputed.

## What a pasted list may look like

One card per line. Counts are written `2x Name`, `2 Name`, `Name x2` or `Name x 2`, and a line with no count is one copy.

Exports from Archidekt, Moxfield, Arena and MTGO paste in as they are: the set code and collector number a site appends (`1 Command Tower [40K]`, `1 Sol Ring (LTC) 123 *F*`) are stripped before lookup, `Commander (1)` / `Creatures (30)` / `Sideboard` headings are read as headings rather than counted as cards, and `SB:` marks a sideboard line. Names resolve past punctuation and accents — `Kodamas Reach` finds *Kodama's Reach*, `Lim-Duls Vault` finds *Lim-Dûl's Vault* — and a double-faced card can be written as either its full name or its front face.

## Where the data comes from

- **MTGJSON AtomicCards** — the card table (names, costs, oracle text, color identity, EDHREC rank/salt, commander eligibility) is captured during `grimoire index` from the same download that feeds the chat's card-name dictionary. This is the floor: the synergy engine, analyzer, and mana math all run offline off this table.
- **EDHREC enrichment (opt-in).** Set `GRIMOIRE_EDHREC=1` and the builder additionally re-ranks candidates with EDHREC's per-commander synergy data and serves Commander Spellbook combos (the `/api/deck/combos` endpoint). EDHREC publishes no official API, so this reads the same Next.js JSON routes a browser does — cached on disk (`GRIMOIRE_EDHREC_CACHE_DIR`, default `data/edhrec-cache`) and spaced ~1 request/second. If the routes change, the builder silently falls back to the local engine.

## The discipline

Same as the sage: the model never names a card you haven't seen verified. Commanders are looked up before drafting starts; picks are validated after the draft lands; a pasted list resolves exact-first, then by punctuation and accent, then by edit distance — and a name that clears none of those bars is reported as unresolved rather than replaced by the nearest text-search hit. A wrong match is worse than an honest miss, because the report is computed from it.

Suggestions are also gated on the card being legal in the format as printed, so the Alchemy rebalance of a card is never proposed as a commander you could sleeve.
