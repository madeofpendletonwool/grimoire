# 📜 Grimoire

A self-hosted, nerdily-themed **rules reference** for **Magic: The Gathering** and **D&D 5e (SRD)** — full-text search plus an optional LLM-backed Q&A "sage" chat, toggle between the two corpora with a click. Ships as a single Go binary in a tiny Docker container.

![status](https://img.shields.io/badge/status-ready-green) ![go](https://img.shields.io/badge/go-1.26-00ADD8) ![docker](https://img.shields.io/badge/docker-multistage-2496ED)

---

## Features

- **🔎 Full-text search** across both rule sets (SQLite + FTS5). Search by word, phrase, or a direct rule number like `205.1a`. Results for a mechanic (e.g. "deathtouch") cluster into the full rule section — the parent plus every sub-rule — so you see how it actually works.
- **🃏 Card lookup** — pull the real oracle text of any Magic card by name via Scryfall (no API key required). The chat consults it automatically when a card is mentioned, so it never invents card effects. The Card Lookup tab autocompletes as you type — start typing a name and matching cards pop up below the input to click on, so you never have to type out a long card name in full.
- **💬 Ask the Sage** — an optional RAG chat that grounds answers in the retrieved rules and cards, and cites the entries it used. Cited rules and cards are clickable and open in an in-app pop-up (the full mechanic section for a rule, the full card for a card) instead of sending you out to Scryfall.
- **✦ / ⚔ Corpus toggle** — flip between MTG and D&D; the whole UI re-themes (mana-blue vs. dragon-red).
- **📜 Spellbook UI** — parchment, gold, small-caps. No build step, no JS framework.
- **🐳 One command to run** — `docker compose up`. The index builds itself on first start.
- **No hard dependencies** — pure-Go SQLite (FTS5), no CGO, single static binary.

## Quick start (Docker)

```bash
cp .env.example .env          # fill in ANTHROPIC_API_KEY to enable the chat
docker compose up --build
```

Open <http://localhost:8080>. Search works immediately; the Q&A chat is enabled once `ANTHROPIC_API_KEY` is set.

The search index is built on first run (it fetches the MTG Comprehensive Rules and the D&D 5e SRD) and cached in a volume, so subsequent starts are instant. To rebuild it:

```bash
docker compose run --rm grimoire index
```

## Q&A chat — LLM provider

The chat speaks the **Anthropic Messages API** and is fully configurable, so it works against Anthropic directly or any Anthropic-compatible endpoint. It is configured for **z.ai (GLM)** out of the box:

| Variable            | Default                          | Notes                                            |
| ------------------- | -------------------------------- | ------------------------------------------------ |
| `ANTHROPIC_BASE_URL`| `https://api.z.ai/api/anthropic` | z.ai's Anthropic endpoint; use `https://api.anthropic.com` for Anthropic |
| `ANTHROPIC_API_KEY` | _(empty)_                        | Secret. Enables the chat when set.               |
| `ANTHROPIC_MODEL`   | `glm-4.6`                        | Any model the endpoint serves (e.g. `claude-3-5-sonnet-20241022`). |

The key is read from the environment only — it is never baked into the image, the compose file, source, or logs. Without a key, search works fully and the chat shows a "configure a key" notice.

## Run locally (no Docker)

```bash
go run ./cmd/grimoire serve      # builds the index on first run, then serves :8080
# or force a rebuild:
go run ./cmd/grimoire index
```

Environment variables are the same as above (`GRIMOIRE_ADDR`, `GRIMOIRE_DB`, `ANTHROPIC_*`, …).

## HTTP API

| Method | Path           | Body / Params                                   | Returns                              |
| ------ | -------------- | ----------------------------------------------- | ------------------------------------ |
| `GET`  | `/api/search`  | `q`, `corpus=mtg\|dnd`, `limit`                 | `{ results: [{number,title,body,source}] }` |
| `GET`  | `/api/section` | `number`, `corpus=mtg\|dnd`                     | `{ parent:{…}, children:[…] }` (full mechanic section) |
| `GET`  | `/api/card`    | `q` (MTG card name)                             | `{ card: {...} }` or `{ card: null, matches: [...] }` |
| `GET`  | `/api/card/search` | `q`, `limit` (MTG card name fragment)     | `{ matches: [{name,mana_cost,type_line,…}] }` |
| `POST` | `/api/ask`     | `{corpus, question}`                            | `{configured, answer, sources:[…], cards:[…]}`  |
| `GET`  | `/api/meta`    | —                                               | `{corpora:[…], chat_configured, chat_model}` |
| `GET`  | `/healthz`     | —                                               | `{status, indexed}`                  |

Example:

```bash
curl 'http://localhost:8080/api/search?corpus=mtg&q=ward'
curl 'http://localhost:8080/api/card?q=Lightning%20Bolt'
curl 'http://localhost:8080/api/card/search?q=bolt&limit=8'
curl -X POST localhost:8080/api/ask -H 'content-type: application/json' \
  -d '{"corpus":"mtg","question":"What does Lightning Bolt do?"}'
```

## Card lookup (Scryfall)

MTG card questions are grounded in real card text, not guesswork. The Q&A chat
detects card names mentioned in a question (quoted, bracketed, after the word
"named", or Title-Case phrases like "Lightning Bolt") and looks each up via
[Scryfall](https://scryfall.com)'s public API — no key required — before
answering. The model is then instructed to answer from that oracle text only,
and to say plainly when it could not look a card up rather than inventing its
effects. The `/api/card` endpoint exposes the same lookup for the UI's "Card
Lookup" tab. The `/api/card/search` endpoint powers the tab's autocomplete — it
calls Scryfall's name-ordered search directly (one round-trip per keystroke)
rather than the fuzzy named lookup, so autocomplete stays responsive without
doubling the rate-limit pressure on the public API.

## Data sources

- **MTG** — the official [Comprehensive Rules](https://magic.wizards.com/en/rules) (parsed locally into numbered rules + glossary).
- **D&D** — the community [5e SRD in Markdown](https://github.com/downfallx/dnd-5e-srd-markdown).

Both are fetched and indexed at first run. Overrides: `MTG_RULES_URL`, `DND_REPO`, `DND_REF`.

## Architecture

```
cmd/grimoire      CLI entrypoint: `serve` (default) | `index`
internal/
  cards/          Scryfall card lookup (named + search), cached + rate-limited
  data/           fetch + parse MTG (.txt) and D&D (markdown) into records
  index/          SQLite + FTS5 store; full-text + rule-number search
  llm/            Anthropic Messages client (configurable base URL) + RAG
  server/         HTTP handlers + JSON API
web/              embedded templates + static assets (the spellbook UI)
```

The binary embeds all front-end assets, so the runtime image is just the binary + CA certs.

## Development

```bash
go build ./...
go vet ./...
go test ./...
```

## License

Source under the MIT license. Rule text remains the property of Wizards of the Coast; this project indexes it for personal reference only.
