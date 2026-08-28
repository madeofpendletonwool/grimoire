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

### Fallback providers (optional)

A standby the chat switches to when the primary fails for a reason a second account wouldn't share — quota or credit exhausted, a rate limit, an expired key, an overloaded or unreachable endpoint. Unset by default.

| Variable                      | Default                    | Notes                                                     |
| ----------------------------- | -------------------------- | --------------------------------------------------------- |
| `ANTHROPIC_FALLBACK_API_KEY`  | _(empty)_                  | Secret. Enables the fallback when set — the only required var. |
| `ANTHROPIC_FALLBACK_BASE_URL` | `https://api.anthropic.com`| Any Anthropic-compatible endpoint.                         |
| `ANTHROPIC_FALLBACK_MODEL`    | `ANTHROPIC_MODEL`          | Defaults to the primary's model, right when the same model is served from a second account. |

Add more rungs by numbering them — `ANTHROPIC_FALLBACK_2_*`, `ANTHROPIC_FALLBACK_3_*`, … — tried in order. The chain stops at the first missing key, so leave no gaps.

Details worth knowing:

- **A malformed request is not retried elsewhere.** Only failures that another provider might survive trigger a handoff; a request the endpoint rejects as invalid would be rejected identically down the chain.
- **Failover happens before any text reaches the reader.** Once a streamed answer has started, an interruption ends it rather than splicing on a second provider's half-answer.
- **The handoff is logged** with the host and model only — never a key.
- **A keyed fallback is enough on its own.** If the primary has no key, the first keyed provider becomes the one that answers, and `/api/status` reports its model as `chat_model`.



## Semantic retrieval (optional, OpenAI-compatible embeddings)

Adds a vector pass alongside FTS5 keyword retrieval — see [Q&A Chat](../features/chat.md#semantic-retrieval-optional) for what it does and when you'd want it. Anthropic has no embeddings API, so this targets the OpenAI `/embeddings` contract that compatible gateways also serve. Off by default; retrieval is unchanged when unset.

| Variable              | Default                     | Notes                                                                       |
| --------------------- | --------------------------- | --------------------------------------------------------------------------- |
| `EMBEDDINGS_BASE_URL` | `https://api.openai.com/v1` | Any OpenAI-compatible endpoint (z.ai: `https://api.z.ai/v1`).                |
| `EMBEDDINGS_API_KEY`  | _(empty)_                   | Secret. Enables semantic retrieval when set with `EMBEDDINGS_MODEL`.        |
| `EMBEDDINGS_MODEL`    | _(empty)_                   | Provider-specific (e.g. `text-embedding-3-small`, `embedding-3`). Required. |

Changing the model later requires a reindex; a dimension mismatch falls back to FTS5 rather than mixing incompatible vectors.

## Session transcription (optional, OpenAI-compatible audio API)

Turns a session recording into a timed `transcript` source — identical in shape to a pasted transcript, so spans, extraction and the canon engine work over it unchanged. Targets the OpenAI `POST /v1/audio/transcriptions` multipart contract that whisper.cpp's server, faster-whisper-server, LocalAI and OpenAI all speak. **Off by default; unset means the audio upload affordance is simply not there** — no button, no degraded path, no warning. See [Sessions](../features/sessions.md) for the flow.

| Variable                  | Default                     | Notes                                                                       |
| ------------------------- | --------------------------- | --------------------------------------------------------------------------- |
| `TRANSCRIBE_BASE_URL`     | `https://api.openai.com/v1` | Any OpenAI-compatible endpoint. With the compose profile service: `http://transcribe:8000/v1`. |
| `TRANSCRIBE_MODEL`        | _(empty)_                   | Required — the model is the switch that enables the hook.                    |
| `TRANSCRIBE_API_KEY`      | _(empty)_                   | Secret, optional — local backends authenticate nobody.                      |
| `TRANSCRIBE_DIR`          | `data/transcribe`           | Where recordings wait while their job runs (beside the database). Never `/tmp` — a reboot must not eat a resumable job's audio. |
| `TRANSCRIBE_KEEP_AUDIO`   | `false`                     | Keep the recording after the transcript lands. Failed jobs keep theirs regardless, for a retry. |
| `TRANSCRIBE_MAX_UPLOAD_MB`| `1024`                      | Upload cap per recording.                                                    |
| `TRANSCRIBE_MAX_DURATION` | `8h`                        | Transcript duration cap, from the returned timings.                         |
| `TRANSCRIBE_CHUNK_MB`     | `24`                        | Chunk target for long recordings (under OpenAI's 25 MB request cap).         |
| `TRANSCRIBE_TIMEOUT`      | `30m`                       | Bound on one chunk request — local CPU whisper is slower than realtime.      |

Worth knowing:

- **Long recordings are the normal case.** A four-hour session is chunked (byte-boundary splits for stream formats — mp3, ogg/opus, flac, aac — and header-synthesized splits for WAV; m4a/mp4/webm go as one request), transcribed sequentially in a background job, and resumable: each chunk's transcript is persisted as it returns, so a restart continues where the ledger left off, and a failed job retries only its unfinished chunks.
- **The upload returns `202` immediately** — no HTTP request is held open for the duration. Poll the job for progress (`chunks_done`/`chunks_total`).
- **The recording is deleted once the transcript source exists**, unless `TRANSCRIBE_KEEP_AUDIO` is on — session recordings of real people are the most sensitive data this app touches. Only the transcript, its sha256 checksum and any segment timings are stored, exactly like an `.srt` upload.

### A local backend with one command

`docker-compose.yml` ships an optional transcription service behind a profile, so the plain `up` stays small:

```sh
docker compose --profile transcribe up -d
```

Then point Grimoire at it in `.env`:

```sh
TRANSCRIBE_BASE_URL=http://transcribe:8000/v1
TRANSCRIBE_MODEL=Systran/faster-whisper-base
```

The bundled image (faster-whisper) is a convenient default; any server speaking the same `/v1/audio/transcriptions` shape works in its place.

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

## Canon engine

The post-session extraction pass — transcripts, DM notes and player journals in, cited candidate facts out for DM review — uses the same `ANTHROPIC_*` endpoint as the chat. Extraction is budget-guarded from day one: a run stops at its soft USD ceiling or candidate cap, and the remainder is deferred and resumable, so a runaway pass over a four-hour transcript cannot become a surprise bill.

| Variable | Default | Notes |
| --- | --- | --- |
| `CANON_BUDGET_USD` | `0` | Soft USD spend ceiling per run. Requires both prices below; with prices unset the budget falls back to the candidate cap and cost is tracked in tokens only. |
| `CANON_MAX_CANDIDATES` | `500` | Hard cap on candidates staged per run. |
| `CANON_BATCH_SIZE` | `8` | Sources processed per run; the rest stay deferred for the next run. |
| `CANON_REQUEST_INTERVAL` | `1s` | Minimum spacing between model calls during a run. Any Go duration. |
| `CANON_PRICE_IN_MTOK` | _(empty)_ | Price per one million input tokens, USD, for the budget estimate. |
| `CANON_PRICE_OUT_MTOK` | _(empty)_ | Price per one million output tokens, USD. |
| `CANON_AGREEMENT_THRESHOLD` | `0.8` | Adversarial pass: agreement score at or above which a verdict counts as agreement; below it the candidate is flagged for review. |
| `CANON_VALIDATE_MODEL` | _(empty)_ | Adversarial pass: the model the validator runs. Same endpoint, key and fallbacks as the chat — only the model name differs. Two passes of the same model is not adversarial validation, so set this to a genuinely different model. |

## Data sources & mirrors

The rules corpora are fetched and indexed at first run; the live lookups run at question time. Each can be retargeted at a mirror.

| Variable              | Default                                        | Notes                                                   |
| --------------------- | ---------------------------------------------- | ------------------------------------------------------- |
| `MTG_RULES_URL`       | auto-discovered from the official rules page   | Source of the MTG rules text. The current file's URL is scraped from the Wizards rules page at index time (Wizards rotates the date-stamped filename on every update); the bundled URL is a fallback, and an explicit value is used as-is. |
| `MTGJSON_URL`         | `https://mtgjson.com/api/v5/AtomicCards.json.gz` | Card database + card-name dictionary (mention detection, deck builder). |
| `DND_REPO`            | the community 5e SRD repo                      | GitHub repo (`owner/name`) the SRD markdown is fetched from. |
| `DND_REF`             | the repo's default branch                      | Branch/tag to fetch the SRD from.                       |
| `DND_DOCS_DIR`        | _(empty)_                                      | Local directory of markdown/text D&D documents (your books, the Sage Advice compendium) indexed alongside the SRD. Convert PDFs with `scripts/extract-dnd-pdfs.py`. The books are gitignored and never leave your machine. |
| `SCRYFALL_BASE_URL`   | `https://api.scryfall.com`                     | Card oracle text + rulings; no key required.            |
| `OPEN5E_BASE_URL`     | `https://api.open5e.com`                       | SRD entities + entity-name dictionary for D&D grounding; no key required. |
| `GRIMOIRE_EDHREC`     | `0`                                            | Enable the deck builder's EDHREC enrichment (per-commander synergy re-ranking + Commander Spellbook combos). Reads EDHREC's unofficial Next.js JSON routes with a disk cache and ~1 req/s spacing; falls back to the local engine if they change. See [Deck Builder](../features/deck-builder.md). |
| `GRIMOIRE_EDHREC_CACHE_DIR` | `data/edhrec-cache`                      | Where the EDHREC enrichment cache lives.                |

### Reindexing

Nobody rebuilds the index by hand:

- **A changed library rebuilds itself.** Every boot fingerprints the `DND_DOCS_DIR` directory (names, sizes, mtimes) and rebuilds automatically when it differs from the fingerprint the index was built against — drop a new book in, restart, done. The app serves the existing index while the rebuild runs.
- **The keeper has a button.** Settings → Library → *Rebuild the index now* (admin only, `POST /api/admin/reindex`) runs the same rebuild in the background and shows its status; useful after a rules release or to force a clean rebuild. `GET /api/admin/reindex` polls it.

`docker compose run --rm grimoire index` still exists, but it is the escape hatch, not the workflow.

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
