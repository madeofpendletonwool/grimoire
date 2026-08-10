# 📜 Grimoire

A self-hosted, nerdily-themed **rules reference** for **Magic: The Gathering** and **D&D 5e (SRD)** — full-text search plus an optional LLM-backed Q&A "sage" chat, toggle between the two corpora with a click. Ships as a single Go binary in a tiny Docker container.

![status](https://img.shields.io/badge/status-ready-green) ![go](https://img.shields.io/badge/go-1.26-00ADD8) ![docker](https://img.shields.io/badge/docker-multistage-2496ED)

---

## Features

- **🔎 Full-text search** across both rule sets (SQLite + FTS5). Search by word, phrase, or a direct rule number like `205.1a`.
- **💬 Ask the Sage** — an optional RAG chat that grounds answers in the retrieved rules and cites the entries it used.
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
| `POST` | `/api/ask`     | `{corpus, question}`                            | `{configured, answer, sources:[…]}`  |
| `GET`  | `/api/meta`    | —                                               | `{corpora:[…], chat_configured, chat_model}` |
| `GET`  | `/healthz`     | —                                               | `{status, indexed}`                  |

Example:

```bash
curl 'http://localhost:8080/api/search?corpus=mtg&q=ward'
curl -X POST localhost:8080/api/ask -H 'content-type: application/json' \
  -d '{"corpus":"mtg","question":"What does ward do?"}'
```

## Data sources

- **MTG** — the official [Comprehensive Rules](https://magic.wizards.com/en/rules) (parsed locally into numbered rules + glossary).
- **D&D** — the community [5e SRD in Markdown](https://github.com/downfallx/dnd-5e-srd-markdown).

Both are fetched and indexed at first run. Overrides: `MTG_RULES_URL`, `DND_REPO`, `DND_REF`.

## Architecture

```
cmd/grimoire      CLI entrypoint: `serve` (default) | `index`
internal/
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
