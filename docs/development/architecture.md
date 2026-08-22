# Architecture

One Go binary, one SQLite file, no frameworks — for the UI or anything else.

```
cmd/grimoire      CLI entrypoint: `serve` (default) | `index`
internal/
  auth/           accounts + server-side sessions (argon2id, same SQLite file)
  cache/          grounded-answer cache: repeat questions skip the model (same SQLite file)
  cards/          Scryfall card lookup (named + search), cached + rate-limited
  chat/           saved conversations + messages (same SQLite file as the index)
  data/           fetch + parse MTG (.txt) and D&D (markdown) into records; corpus registry + entity-resolver interface
  deck/           Commander deck builder: parsing, synergy engine, analysis, saved decks
  embeddings/     OpenAI-compatible embeddings client for semantic retrieval
  encounter/      D&D encounter builder: DMG difficulty maths, mirrored SRD bestiary, design planning
  entities/       per-corpus entity resolution into grounding text (MTG cards, D&D via Open5e)
  index/          SQLite + FTS5 store; full-text + rule-number search
  llm/            Anthropic Messages client (configurable base URL), RAG + streaming
  resolver/       Magic interaction resolver: board + sequence → step-by-step prompt
  rulings/        official MTG card rulings from Scryfall, cached + rate-limited
  server/         HTTP handlers, JSON API, SSE chat endpoint, session gate
  study/          spaced-repetition reviews (SM-2) over the corpus (same SQLite file)
web/
  templates/      the app shell, plus the login / first-run gate
  static/js/      ES modules: chat, palette, reference drawer, study, resolve, deck, encounter, voice, scene, icons
```

The binary embeds all front-end assets, so the runtime image is just the binary + CA certs.

## One SQLite file

Chat history and accounts live in the **same SQLite file** as the rules index. That is safe: `grimoire index` only clears the document tables, so rebuilding the index never drops a conversation or signs anyone out. Keep the `/data` volume and both survive upgrades.

Also riding along in that file: the grounded-answer cache, the per-user study schedules, saved decks and encounters, and the mirrored SRD bestiary the encounter builder chooses from. Pure-Go SQLite with FTS5 — no CGO, a single static binary.

## The retrieval flow

An `/api/ask` (or chat message) walks this path:

```
question
  ├─ FTS5 keyword retrieval (always)
  ├─ embeddings vector pass (optional, when EMBEDDINGS_* is set)
  ├─ card mention detection → Scryfall oracle text + rulings (MTG)
  └─ entity mention detection → Open5e SRD text (D&D)
        ↓
  prompt: answer from this text only, cite your sources
        ↓
  Anthropic Messages API (streamed) → answer + citations
        ↓
  cached against the source set (reindex invalidates)
```

## Data sources

- **MTG** — the official [Comprehensive Rules](https://magic.wizards.com/en/rules), parsed locally into numbered rules + glossary.
- **MTG card names** — [MTGJSON](https://mtgjson.com)'s `AtomicCards` (one entry per unique card), indexed on first run into a mention-detection dictionary.
- **MTG cards + rulings** — [Scryfall](https://scryfall.com)'s public API (oracle text, fuzzy name match, official rulings). No key required.
- **D&D** — the community [5e SRD in Markdown](https://github.com/downfallx/dnd-5e-srd-markdown).
- **D&D entities** — [Open5e](https://open5e.com)'s REST API (SRD spells, creatures, items, feats) for grounding D&D questions in real statblocks and spell text. No key required.

The rules corpora are fetched and indexed at first run (`MTG_RULES_URL`, `MTGJSON_URL`, `DND_REPO`, `DND_REF`); the live lookups can be retargeted at a mirror (`SCRYFALL_BASE_URL`, `OPEN5E_BASE_URL`).

## The front end

No build step and no framework: `web/templates` is a server-rendered shell, `web/static/js/` is plain ES modules, and the whole design system — tokens, nine-slice frames, the two icon systems, theme derivation — is documented in the [Design System](../DESIGN.md).
