![Card lookup](../assets/sprites/card.png){: style="float:right; margin-left:1rem" align=right }

# Card & Entity Lookup

Card questions are grounded in real card text, not guesswork — and the two corpora each have a first-class lookup path.

## Magic cards (Scryfall)

The Q&A chat detects card names mentioned in a question — quoted, bracketed, after the word *"named"*, or Title-Case phrases like "Lightning Bolt" — and looks each up via [Scryfall](https://scryfall.com)'s public API **before** answering. No key required.

The model is then instructed to answer from that oracle text only, and to say plainly when it could not look a card up rather than inventing its effects. When a name cannot be resolved, it is cited as unresolved rather than guessed at.

=== "Named lookup"

    `/api/card` resolves a full card name, with fuzzy tolerance for near-misses gated by a credibility check, so a misspelling can never attach the wrong card.

=== "Autocomplete"

    `/api/card/search` powers the palette's autocomplete — it calls Scryfall's name-ordered search directly (one round-trip per keystroke) rather than the fuzzy named lookup, so autocomplete stays responsive without doubling the rate-limit pressure on the public API.

=== "The name dictionary"

    Beyond the text heuristics, the chat carries a dictionary of every unique card name (MTGJSON's `AtomicCards`, one entry per name), indexed on first run. It catches card mentions the heuristics miss: lowercase, unquoted, no Title Case.

### Official rulings

When a card is looked up, its **official rulings** come with it — Gatherer / Oracle rulings, Wizards release notes and Scryfall notes from Scryfall's rulings API, cached and rate-limited the same way. The model is told to treat wotc rulings as authoritative precedent and Scryfall notes as official guidance, and to cite them — card, source, date, decisive phrase — when they decide the question. An answer can cite the ruling that decides the question, not just the rule text.

## D&D entities (Open5e)

The D&D side works the same way through [Open5e](https://open5e.com)'s free API: spells, creatures, magic items, feats and conditions mentioned in a question are resolved to their SRD text before answering, and the model is instructed to answer from that text rather than inventing mechanics.

- **SRD only** — search hits are filtered to the SRD documents (2024 edition preferred, then older), so community homebrew never grounds an answer.
- **Real-world naming** — the 2024 SRD renamed creator-prefixed spells ("Tenser's Floating Disk" → "Floating Disk"), so a leading word is dropped and the search retried; a pure typo falls back to the strongest credible near-match, gated by the same fuzzy check the MTG card lookup uses so a near-miss can never attach the wrong entity.
- **Complete statblocks** — a creature grounds in its full statblock: ability scores, saving throws, skills, senses, languages, damage resistances/vulnerabilities/immunities, condition immunities, traits, and every action category in statblock order — actions, bonus actions, reactions, and legendary actions with their costs. Spells carry the concentration tag alongside casting time, range and duration.
- **The name dictionary** — like the MTG card dictionary, an SRD entity-name dictionary (spells, creatures, magic items, feats, conditions, weapons — built at index time from Open5e's listings) catches lowercase, unquoted mentions the Title-Case heuristics miss: "does hunter's mark stack" grounds in the real spell.

### Local books & Sage Advice

Beyond the SRD, `DND_DOCS_DIR` imports your own documents — a PHB, a monster book, the Sage Advice compendium — into the searchable corpus, cited by book. The Sage Advice compendium's official Q&A rulings are chunked one question per record, so retrieval surfaces them as precedent the way MTG answers cite Gatherer rulings; the model is told to treat them as authoritative interpretation alongside the rules they clarify. Convert PDFs with `scripts/extract-dnd-pdfs.py`; see [Configuration](../deployment/configuration.md).

## Where the lookups live

| Lookup | Route | Grounds |
| ------ | ----- | ------- |
| MTG card (named) | `/api/card` | Scryfall oracle text + rulings |
| MTG card (fragment) | `/api/card/search` | Scryfall name search |
| D&D entity | internal to `/api/ask` | Open5e SRD text |

The live lookups can be retargeted at a mirror: `SCRYFALL_BASE_URL`, `OPEN5E_BASE_URL` — see [Configuration](../deployment/configuration.md).
