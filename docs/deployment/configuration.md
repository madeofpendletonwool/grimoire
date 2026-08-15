# Configuration

Grimoire is configured entirely through environment variables. Under Docker, put them in `.env` next to `docker-compose.yml` (see `.env.example`); bare metal, just export them.

Every key set in `.env` is a secret — `.env` is gitignored on purpose, and secrets are never baked into the image, the compose file, source, or logs.

## Server

| Variable       | Default            | Notes                                                                        |
| -------------- | ------------------ | ---------------------------------------------------------------------------- |
| `GRIMOIRE_ADDR`| `:8080`            | Listen address.                                                              |
| `GRIMOIRE_DB`  | `data/grimoire.db` (bare) / `/data/grimoire.db` (image) | Path to the single SQLite file holding the index, chats, study progress and accounts. |
| `GRIMOIRE_PORT`| `8080`             | Compose only: the **host** port published in `docker-compose.yml`.           |

## Q&A chat (Anthropic Messages API)

The chat speaks the Anthropic Messages API and is fully configurable, so it works against Anthropic directly or any Anthropic-compatible endpoint. It is configured for **z.ai (GLM)** out of the box.

| Variable             | Default                          | Notes                                                                             |
| -------------------- | -------------------------------- | --------------------------------------------------------------------------------- |
| `ANTHROPIC_BASE_URL` | `https://api.z.ai/api/anthropic` | z.ai's Anthropic endpoint; use `https://api.anthropic.com` for Anthropic directly |
| `ANTHROPIC_API_KEY`  | _(empty)_                        | Secret. Enables the chat when set.                                                |
| `ANTHROPIC_MODEL`    | `glm-4.6`                        | Any model the endpoint serves (e.g. `claude-3-5-sonnet-20241022`).                 |

Without a key, search works fully and the chat shows a "configure a key" notice.

## Semantic retrieval (optional, OpenAI-compatible embeddings)

Adds a vector pass alongside FTS5 keyword retrieval — see [Q&A Chat](../features/chat.md#semantic-retrieval-optional) for what it does and when you'd want it. Anthropic has no embeddings API, so this targets the OpenAI `/embeddings` contract that compatible gateways also serve. Off by default; retrieval is unchanged when unset.

| Variable              | Default                     | Notes                                                                       |
| --------------------- | --------------------------- | --------------------------------------------------------------------------- |
| `EMBEDDINGS_BASE_URL` | `https://api.openai.com/v1` | Any OpenAI-compatible endpoint (z.ai: `https://api.z.ai/v1`).                |
| `EMBEDDINGS_API_KEY`  | _(empty)_                   | Secret. Enables semantic retrieval when set with `EMBEDDINGS_MODEL`.        |
| `EMBEDDINGS_MODEL`    | _(empty)_                   | Provider-specific (e.g. `text-embedding-3-small`, `embedding-3`). Required. |

Changing the model later requires a reindex; a dimension mismatch falls back to FTS5 rather than mixing incompatible vectors.

## Answer cache

| Variable                    | Default | Notes                                                                            |
| --------------------------- | ------- | -------------------------------------------------------------------------------- |
| `GRIMOIRE_ANSWER_CACHE_TTL` | `168h`  | How long a cached answer stays fresh. Any Go duration (`24h`, `168h`…).          |

The cache key folds in the retrieved source set, so a rules reindex invalidates affected entries on its own. `?nocache` on the ask endpoints forces a fresh answer.

## Accounts & invites

| Variable                     | Default | Notes                                                                        |
| ---------------------------- | ------- | ---------------------------------------------------------------------------- |
| `GRIMOIRE_SESSION_TTL`       | `720h`  | How long a session lasts. Any Go duration (`24h`, `168h`, …).                |
| `GRIMOIRE_INVITE_TTL`        | `168h`  | How long a freshly minted invite link stays usable. `0` = never expire.      |
| `GRIMOIRE_OPEN_REGISTRATION` | `false` | Leave self-service account creation open after the first keeper (the original escape hatch; invites are the recommended path). |

See [Accounts & Invites](accounts.md) for the model: first visitor claims the keeper account, everyone else arrives via single-use invite links.

## Data sources & mirrors

The rules corpora are fetched and indexed at first run; the live lookups run at question time. Each can be retargeted at a mirror.

| Variable              | Default                                        | Notes                                                   |
| --------------------- | ---------------------------------------------- | ------------------------------------------------------- |
| `MTG_RULES_URL`       | the official Comprehensive Rules               | Source of the MTG rules text.                           |
| `MTGJSON_URL`         | `https://mtgjson.com/api/v5/AtomicCards.json.gz` | Card-name dictionary used for mention detection.      |
| `DND_REPO`            | the community 5e SRD repo                      | GitHub repo (`owner/name`) the SRD markdown is fetched from. |
| `DND_REF`             | the repo's default branch                      | Branch/tag to fetch the SRD from.                       |
| `DND_DOCS_DIR`        | _(empty)_                                      | Local directory of markdown/text D&D documents (your books, the Sage Advice compendium) indexed alongside the SRD. Convert PDFs with `scripts/extract-dnd-pdfs.py`. Changing it needs a reindex. |
| `SCRYFALL_BASE_URL`   | `https://api.scryfall.com`                     | Card oracle text + rulings; no key required.            |
| `OPEN5E_BASE_URL`     | `https://api.open5e.com`                       | SRD entities + entity-name dictionary for D&D grounding; no key required. |

## A worked `.env`

```bash
# chat
ANTHROPIC_BASE_URL=https://api.z.ai/api/anthropic
ANTHROPIC_API_KEY=sk-your-key-here
ANTHROPIC_MODEL=glm-4.6

# optional: semantic retrieval on z.ai
EMBEDDINGS_BASE_URL=https://api.z.ai/v1
EMBEDDINGS_API_KEY=sk-your-key-here
EMBEDDINGS_MODEL=embedding-3

# server
GRIMOIRE_PORT=8080
```

Everything else keeps its default. See `.env.example` in the repo root for the full annotated template.
