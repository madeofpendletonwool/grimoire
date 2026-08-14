![Search](../assets/sprites/magnifier.png){: style="float:right; margin-left:1rem" align=right }

# Search & Command Palette

One search box sits over everything: <kbd>⌘</kbd><kbd>K</kbd> (macOS) or <kbd>Ctrl</kbd><kbd>K</kbd> opens the command palette from anywhere in the app.

Results open in a **reference drawer** beside the conversation, so consulting a rule never takes you out of the chat. The drawer is the book's right-hand page — spine and all.

## The command palette

The palette searches **both rule sets and Scryfall at once**:

- MTG Comprehensive Rules — by rule text or number (`702.2`, `205.1a`)
- D&D 5e SRD entries
- Magic cards via Scryfall name search, autocompleting as you type — you never have to spell out "Asmoranomardicadaistinaculdacar" in full

Selecting a rule or card opens it in the drawer; selecting a search term jumps to full-text results.

## Slash commands in the composer

The composer takes slash commands directly:

| Command | Example | Opens |
| ------- | ------- | ----- |
| `/card` | `/card Lightning Bolt` | The card in the reference drawer |
| `/rule` | `/rule 702.2` | The rule (expanded to its section) in the drawer |
| `/search` | `/search hexproof board wipe` | Full-text results in the drawer |

## Full-text search

Search runs across both rule sets on SQLite + FTS5:

- **By word or phrase** — quoted phrases match exactly.
- **By rule number** — `205.1a` jumps straight to the rule.
- **Section expansion** — opening a numbered rule expands it to the full mechanic section: the parent rule plus every sub-rule, so you see how it actually works rather than one clause in isolation.

## The reference drawer

Whatever you open — rule, section, card — renders on the drawer's parchment page:

- **Rules** carry their number, title and full text, with sub-rules indented beneath the parent.
- **Cards** show the real oracle text, mana cost, type line and power/toughness, with Scryfall imagery and a link out.
- **Search terms** show the matched entries with the matching words highlighted.

!!! tip "Two corpora, two accents"

    Magic and D&D each keep their own accent color through the whole interface — mana-blue for Magic, dragon-red for D&D. Pick one per conversation; search follows the open conversation's corpus.
