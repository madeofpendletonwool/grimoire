![Chat](../assets/sprites/staff.png){: style="float:right; margin-left:1rem" align=right }

# Q&A Chat

The app is a conversation. Ask a question, get an answer streamed token by token, grounded in retrieved rules, real card text, official rulings and real SRD entries, with every source it consulted cited beneath.

Follow-up questions carry the earlier turns, so *"what if it were tapped instead?"* resolves against what was already said.

## Grounding, not generation

Every answer is preceded by a retrieval pass:

1. **Keyword retrieval** over the corpus (SQLite FTS5) — the backbone. Exact rule numbers and keyword matches always work.
2. **Card lookup** — any Magic card named in the question is resolved to its Scryfall oracle text before answering; any D&D spell, creature, item, feat or condition is resolved to its SRD text via Open5e. See [Card & Entity Lookup](lookup.md).
3. **Official rulings** — a looked-up card brings its Gatherer/Oracle rulings, Wizards release notes and Scryfall notes along as precedent.
4. Optional **semantic retrieval** — an embeddings pass that merges in rules that are semantically close even when lexically distinct (see below).

The model is then instructed to answer from that text only, and to say plainly when it could not look something up rather than inventing it.

## Sources, cited

Beneath every answer sits its citations: rule chips that open the full rule in the [reference drawer](search.md#the-reference-drawer), card chips with the consulted oracle text, and official rulings quoted with source and date. A card the sage could not resolve is struck through rather than silently dropped.

## Conversations are saved

Threads persist in SQLite with auto-generated titles, and the sidebar groups them by date. Rename, delete, resume across reloads and devices. A conversation's rule set is locked when it is created, because the grounding differs per corpus — Magic and D&D each keep their own accent, mana-blue and dragon-red.

!!! note "Voice input"

    A mic button beside the composer transcribes speech via the browser's Web Speech API (Chrome/Edge over HTTPS or localhost). Nothing is sent until the transcript is reviewed and sent like any other message.

## The LLM provider

The chat speaks the **Anthropic Messages API** and is fully configurable, so it works against Anthropic directly or any Anthropic-compatible endpoint. It is configured for **z.ai (GLM)** out of the box:

| Variable             | Default                          | Notes                                                                             |
| -------------------- | -------------------------------- | --------------------------------------------------------------------------------- |
| `ANTHROPIC_BASE_URL` | `https://api.z.ai/api/anthropic` | z.ai's Anthropic endpoint; use `https://api.anthropic.com` for Anthropic directly |
| `ANTHROPIC_API_KEY`  | _(empty)_                        | Secret. Enables the chat when set.                                                |
| `ANTHROPIC_MODEL`    | `glm-4.6`                        | Any model the endpoint serves (e.g. `claude-3-5-sonnet-20241022`).                 |

The key is read from the environment only — it is never baked into the image, the compose file, source, or logs. Without a key, search works fully and the chat shows a "configure a key" notice. All configuration variables are covered in [Configuration](../deployment/configuration.md).

## Semantic retrieval (optional)

Keyword retrieval misses questions whose words don't overlap the rule they need — *"does my hexproof guy die to a board wipe?"* shares no terms with the destroy/damage rules. Set an **OpenAI-compatible embeddings** endpoint and retrieval adds a vector pass alongside FTS5, merging in semantically close rules.

FTS5 stays the backbone, so exact rule-number lookup (`702.2`) and keyword matches still work; embeddings only add recall. Off by default — retrieval is unchanged when unset.

| Variable              | Default                     | Notes                                                                                  |
| --------------------- | --------------------------- | -------------------------------------------------------------------------------------- |
| `EMBEDDINGS_BASE_URL` | `https://api.openai.com/v1` | Any OpenAI-compatible endpoint (z.ai: `https://api.z.ai/v1`).                          |
| `EMBEDDINGS_API_KEY`  | _(empty)_                   | Secret. Enables semantic retrieval when set with `EMBEDDINGS_MODEL`.                   |
| `EMBEDDINGS_MODEL`    | _(empty)_                   | Provider-specific (e.g. `text-embedding-3-small`, `embedding-3`). Required.            |

Vectors are stored in the same SQLite file as little-endian float32 BLOBs and scored with a linear-scan cosine — fine for an index of thousands of rules. They are (re)built during `grimoire index`, and also on the next `serve` if an index already exists without them. Changing the model later requires a reindex; a dimension mismatch falls back to FTS5 rather than mixing incompatible vectors.

## Answer caching

Answers are cached: a repeat (or grounding-equivalent) question returns instantly without a second model call. The cache key folds in the retrieved source set, so a rules **reindex** invalidates affected entries on its own — a stale answer can't survive a grounding change.

Send `?nocache` on `/api/ask` or `/api/chats/{id}/messages` to force a fresh answer. The lifetime is tunable via `GRIMOIRE_ANSWER_CACHE_TTL` (default `168h`).
