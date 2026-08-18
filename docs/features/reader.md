![Reading mode](../assets/sprites/openBook.png){: style="float:right; margin-left:1rem" align=right }

# Reading Mode

The corpus the sage searches is also a shelf of real books. **Read** (in the rail, under Study) opens the reading surface: a contents column on stone, the page itself on parchment, and the whole corpus browsable the way it was written — Magic's *Comprehensive Rules* as nine chapters plus the glossary, each SRD document as its own volume, and every local D&D book as its own too.

## What's on the shelf

| Corpus | Volumes | Shaped from |
| ------ | ------- | ----------- |
| Magic | 1 — *Comprehensive Rules* (chapters → sections → glossary terms) | The comp-rules text file's own structure |
| D&D | 13 SRD volumes (*Spells*, *Classes*, *Monsters A–Z*, *Rules Glossary*, …) | Each markdown document of the 5e SRD |
| D&D | + every local book | PDFs run through [the extractor](lookup.md), or any markdown/text dropped in `DND_DOCS_DIR` |

## The reading experience

- **Raw fidelity, not search mush.** The reading store keeps each section's *raw markdown* — the class tables, the coin tables, the stat blocks arrive as actual tables with their bold and italics, not the flattened rows the full-text index sees.
- **Contents that mirror the book.** Chapters and sections nest by their own heading levels; a spell entry sits under *Spell Descriptions* the same way it does in the document.
- **Page turns.** Prev/next walk the book in order (keyboard: `←`/`→`), skipping past structural stops so "next" always lands on something readable.
- **Citations deep-link into the book.** The drawer's *"Read this in the book"* opens the reader at the page carrying the cited rule — `702.2c` lands on *Keyword Abilities*, a spell record number lands on its spell.
- **Mana as symbols.** Rule numbers in Magic pages are clickable references (they open the drawer), and `{T}: Add {G}` renders as symbols, as everywhere else.

## Under the hood

The parsers already knew the books' shapes — Magic's numbered rules and the SRD's heading trees — so the index build now produces a **reader tree** (`reader_nodes` / `reader_guides` tables in the same SQLite file) alongside the FTS5 records. Node ids share the record numbering scheme, which is what makes citation → page resolution a plain prefix match. An install whose index predates the reader gets its books backfilled by a background rebuild on the next boot — no terminal session required.

| Method | Path | Returns |
| ------ | ---- | ------- |
| `GET` | `/api/reader/guides?corpus=…` | the shelf: `{guides:[{guide,title,kind,nodes}]}` |
| `GET` | `/api/reader/toc?corpus=…&guide=…` | one volume's contents, in book order |
| `GET` | `/api/reader/page?corpus=…&guide=…&number=…` | the page: body, crumbs, prev/next — a bare `number` (a rule or record reference) is resolved onto its page |
