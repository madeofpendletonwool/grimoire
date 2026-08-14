# 📜 Grimoire

A self-hosted, nerdily-themed **rules reference** for **Magic: The Gathering** and **D&D 5e (SRD)** — a chat-first sage that answers grounded in the real rules and real card text, with full-text search and card lookup a keystroke away. Ships as a single Go binary in a tiny Docker container.

![status](https://img.shields.io/badge/status-ready-green) ![go](https://img.shields.io/badge/go-1.26-00ADD8) ![docker](https://img.shields.io/badge/docker-multistage-2496ED)

---

## Features

- **💬 Chat first** — the app is a conversation. Ask a question, get an answer streamed token by token, grounded in retrieved rules and real card text, with every rule and card it consulted cited beneath. Follow-up questions carry the earlier turns, so "what if it were tapped instead?" resolves against what was already said.
- **📚 Conversations are saved** — threads persist in SQLite with auto-generated titles, and the sidebar groups them by date. Rename, delete, resume across reloads and devices. A conversation's rule set is locked when it is created, because the grounding differs per corpus.
- **⌘K Command palette** — one search box over both rule sets and Scryfall. Results open in a reference drawer beside the conversation, so consulting a rule never takes you out of the chat. Slash commands work in the composer too: `/card Lightning Bolt`, `/rule 702.2`, `/search`.
- **🔎 Full-text search** across both rule sets (SQLite + FTS5). Search by word, phrase, or a direct rule number like `205.1a`. Opening a numbered rule expands it to the full mechanic section — the parent plus every sub-rule — so you see how it actually works.
- **🔐 Yours alone** — the whole app sits behind a login. The first visit to a fresh install creates the keeper account, who becomes the admin (no seeded password to forget to change). The admin then invites friends via single-use links; self-service creation stays off, and conversations are scoped per account. Passwords are argon2id; sessions are server-side and revocable.
- **🃏 Card lookup** — pull the real oracle text of any Magic card by name via Scryfall (no API key required). The chat consults it automatically when a card is mentioned, so it never invents card effects; when it cannot resolve a name it says so instead of guessing. The palette autocompletes as you type, so you never have to spell out "Asmoranomardicadaistinaculdacar" in full.
- **🔮 Interaction resolver** — a separate Resolve mode (Magic only) for the questions that are hardest to google. State a board and a proposed spell/ability sequence, and the sage walks the resulting interactions step by step — the stack, APNAP trigger ordering, continuous-effect layers, and replacement effects — citing the rule at each step. Grounded in real card text and the full interaction chapters (117, 603, 613, 616). It is an assistant, not a Comprehensive-Rules oracle, so the UI says so plainly.
- **📇 Study mode** — a spaced-repetition deck over the corpus, for new players and judges-in-training. MTG keyword abilities (chapter 702) and D&D conditions turn into flashcards; grade each card Again / Hard / Good / Easy and an SM-2 scheduler brings it back on the right day. Progress persists per account across reloads.
- **✦ / ⚔ Two corpora** — Magic and D&D, each with its own accent (mana-blue vs. dragon-red). Pick one per conversation.
- **📜 An actual tome, not a theme** — the interface is built from real pixel art rather than emoji and CSS gradients. Answers, rules and cards sit on a nine-sliced parchment page; the reference drawer is the book's right-hand page, spine and all; the sign-in screen is the book shut, and opens into it. Sprites carry the nouns the app is about — spellbook, scroll, mana orb, wizard's staff, candle. Long rules text stays in real high-resolution serif, because a pixel font would tax the one thing this app exists to do. See [ATTRIBUTIONS.md](ATTRIBUTIONS.md).
- **🏔 Four themes, taken from the art** — pick a chamber in Settings and the whole interface follows it: a violet cavern, a brown dead forest, blue-grey peaks, or open plains. Each theme's colour is *measured* from that scene's own pixels at build time and applied to the chrome's existing lightness ladder, so a retinted interface is exactly as legible as the default. The backdrop is a real parallax stack that drifts with the pointer — the sky holds still, the foreground travels. The parchment never changes: the room changes, the book does not.
- **{T}: Add {G}** — Magic's mana, tap and set symbols render as symbols, in card text, rule text and the sage's prose, via the Mana and Keyrune fonts. No build step, no JS framework, no bundler — just ES modules, and every font self-hosted so it works offline.
- **🐳 One command to run** — `docker compose up`. The index builds itself on first start.
- **No hard dependencies** — pure-Go SQLite (FTS5), no CGO, single static binary.

## Quick start (Docker)

```bash
cp .env.example .env          # fill in ANTHROPIC_API_KEY to enable the chat
docker compose up --build
```

Open <http://localhost:8080>. The first visit asks you to create the keeper account — pick a name and passphrase and you are in. Search works immediately; the Q&A chat is enabled once `ANTHROPIC_API_KEY` is set.

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

### Semantic retrieval (optional)

The chat grounds answers in rules it retrieves by keyword (FTS5). That misses questions whose words don't overlap the rule they need — "does my hexproof guy die to a board wipe?" shares no terms with the destroy/damage rules. Set an **OpenAI-compatible embeddings** endpoint and retrieval adds a vector pass alongside FTS5, merging in rules that are semantically close even when lexically distinct. FTS5 stays the backbone, so exact rule-number lookup (`702.2`) and keyword matches still work; embeddings only add recall. Off by default — retrieval is unchanged when unset.

Anthropic has no embeddings API, so this targets the OpenAI `/embeddings` contract that compatible gateways also serve. Vectors are stored in the same SQLite file as little-endian float32 BLOBs and scored with a linear-scan cosine (fine for an index of thousands of rules). Vectors are (re)built during `grimoire index`, and also on the next `serve` if an index already exists without them. Changing the model later requires a reindex; a dimension mismatch falls back to FTS5 rather than mixing incompatible vectors.

| Variable              | Default                          | Notes                                            |
| --------------------- | -------------------------------- | ------------------------------------------------ |
| `EMBEDDINGS_BASE_URL` | `https://api.openai.com/v1`      | Any OpenAI-compatible endpoint (z.ai: `https://api.z.ai/v1`). |
| `EMBEDDINGS_API_KEY`  | _(empty)_                        | Secret. Enables semantic retrieval when set with `EMBEDDINGS_MODEL`. |
| `EMBEDDINGS_MODEL`    | _(empty)_                        | Provider-specific (e.g. `text-embedding-3-small`, `embedding-3`). Required. |

Answers are cached: a repeat (or grounding-equivalent) question returns instantly without a second model call. The key folds in the retrieved source set, so a rules **reindex** invalidates affected entries on its own — a stale answer can't survive a grounding change. Send `?nocache` on `/api/ask` or `/api/chats/{id}/messages` to force a fresh answer.

| Variable                   | Default | Notes                                                                  |
| -------------------------- | ------- | ---------------------------------------------------------------------- |
| `GRIMOIRE_ANSWER_CACHE_TTL`| `168h`  | How long a cached answer stays fresh. Any Go duration (`24h`, `168h`…). |

## Accounts

Grimoire is single-household software, so it has a login but no open sign-up.
On a fresh install the app has no accounts and the login page offers to create
the first one: whoever reaches the app claims it, and that first account is the
**admin** (the keeper). Account creation then closes, and later arrivals only
see a login form.

The admin invites friends in from **Settings → Invitations**: each click mints a
single-use invite link (`/?invite=…`) that lets exactly one person make an
account, then spends. The admin can see which links are pending, used, or
expired, and revoke a pending one. Self-service creation stays off by default —
an invite is the only way in past the first keeper.

| Variable                     | Default | Notes                                                                     |
| ---------------------------- | ------- | ------------------------------------------------------------------------- |
| `GRIMOIRE_SESSION_TTL`       | `720h`  | How long a session lasts. Any Go duration (`24h`, `168h`, …).              |
| `GRIMOIRE_INVITE_TTL`        | `168h`  | How long a freshly minted invite link stays usable. `0` = never expire.   |
| `GRIMOIRE_OPEN_REGISTRATION` | `false` | Leave self-service account creation open after the first keeper (the original escape hatch; invites are the recommended path). |

- Passwords are hashed with **argon2id** (64 MiB, 3 passes) using a per-password
  random salt; the parameters travel inside every stored hash, so raising the
  cost later leaves existing passwords verifiable.
- Sessions are **server-side**: the cookie holds an opaque 256-bit token, the
  database holds only its SHA-256 digest, and signing out deletes the row — so a
  stolen cookie stops working the moment you log out. The cookie is `HttpOnly`
  and `SameSite=Lax`, and is marked `Secure` automatically when the request
  arrives over TLS or through a proxy that sets `X-Forwarded-Proto: https`.
- Invite codes are treated the same way as session tokens: the link carries the
  raw secret, the database stores only its SHA-256 digest, and the raw code is
  returned to the admin **once**, at creation — the list never shows it.
- Accounts live in the same SQLite file as everything else, so they survive
  `grimoire index` and ride along with the `/data` volume.
- **Upgrading from a version without accounts?** Conversations recorded under the
  old anonymous owner are handed to the first account created, so no history is
  lost behind the new login. Upgrading from a version before admins/invites? The
  oldest existing account is marked admin on the way up, matching "first created
  user is the admin."

## Run locally (no Docker)

```bash
go run ./cmd/grimoire serve      # builds the index on first run, then serves :8080
# or force a rebuild:
go run ./cmd/grimoire index
```

Environment variables are the same as above (`GRIMOIRE_ADDR`, `GRIMOIRE_DB`, `ANTHROPIC_*`, …).

## Interaction resolver (Magic)

Resolve mode (the 🔮 toggle in the top bar, Magic only) answers the questions a
rules lookup can't: *given this board and this sequence of plays, what exactly
happens, and in what order?* It is built for Commander, where a single board
wipe can stack a dozen dies-triggers across two players.

You state the board and the sequence in a compact, line-oriented form:

```
Board                         Sequence
You: Blood Artist             1. Opp casts Wrath of God
You: Zulaport Cutthroat
You: Doomed Traveler
```

Each permanent is `[controller]: <card> [# tapped | +1/+1 counters: N]`; each
sequence step is `[N.] <action>` (the actor can live in the prose:
"Opp casts…"). The resolver looks up every named card on Scryfall, grounds in
the full interaction chapters — priority and the stack (117), triggered
abilities and APNAP (603), interaction of continuous effects / layers (613),
and replacement and prevention effects (616) — plus any keyword abilities and
state-based actions the specific cards pull in, then streams a numbered, cited
walkthrough.

It is **LLM-assisted, not a Comprehensive-Rules engine**: it reasons over the
provided text and cites the rule at each step, but the UI is honest that it is
an assistant rather than an oracle — verify anything match-deciding against the
rule it cites. A resolve is stateless and not saved; the Q&A chat is where
saved conversations live.

## Study mode

Study mode (the 📇 button in the sidebar) turns the corpus into a
spaced-repetition flashcard deck. The corpus is already a question bank — MTG
keyword abilities (chapter 702, one card per keyword: Deathtouch, Ward, …) and
D&D SRD conditions — so the cards are generated from the existing FTS5 index
rather than authored by hand. No LLM is required.

A session deals the cards due now plus a few new ones; each card has a front
(the keyword name / condition to recall) and a back (the full rule text). Grade
each card **Again / Hard / Good / Easy** and an **SM-2** scheduler reschedules
it: an Again brings it back within the session, a Good/Easy spaces it out by
days that grow with each correct recall and the card's personal ease factor.
Schedules live in a per-user `reviews` table in the same SQLite file as the rest
of the app, so progress survives reloads and reindexes (the concept keys are
stable rule numbers).

## HTTP API

| Method | Path           | Body / Params                                   | Returns                              |
| ------ | -------------- | ----------------------------------------------- | ------------------------------------ |
| `GET`  | `/api/search`  | `q`, `corpus=mtg\|dnd`, `limit`                 | `{ results: [{number,title,body,source}] }` |
| `GET`  | `/api/section` | `number`, `corpus=mtg\|dnd`                     | `{ parent:{…}, children:[…] }` (full mechanic section) |
| `GET`  | `/api/card`    | `q` (MTG card name)                             | `{ card: {...} }` or `{ card: null, matches: [...] }` |
| `GET`  | `/api/card/search` | `q`, `limit` (MTG card name fragment)     | `{ matches: [{name,mana_cost,type_line,…}] }` |
| `POST` | `/api/ask`     | `{corpus, question}`                            | `{configured, answer, sources:[…], cards:[…]}` — stateless one-shot |
| `POST` | `/api/resolve` | `{board, sequence, note}` (Magic)              | SSE: `meta`, `delta`, `done`, `error` — step-by-step interaction trace |
| `GET`  | `/api/chats`   | —                                               | `{chats:[…]}`                        |
| `POST` | `/api/chats`   | `{corpus}`                                      | `{chat:{…}}`                         |
| `GET`  | `/api/chats/{id}` | —                                            | `{chat:{…}, messages:[…]}`           |
| `PATCH`| `/api/chats/{id}` | `{title}`                                    | `{chat:{…}}`                         |
| `DELETE`| `/api/chats/{id}` | —                                           | `204`                                |
| `POST` | `/api/chats/{id}/messages` | `{question}`                         | SSE: `meta`, `delta`, `done`, `error` |
| `GET`  | `/api/study/queue` | `corpus`, `topic` (optional), `limit`        | `{cards:[{key,front,back,…}], stats:{total,new,due,learned}}` |
| `POST` | `/api/study/grade` | `{key, corpus, grade}`                       | `{card:{…, reps, interval_days, ease, due_at}}` |
| `GET`  | `/api/meta`    | —                                               | `{corpora:[…], chat_configured, chat_model}` |
| `GET`  | `/api/auth/state` | —                                            | `{authenticated, username, is_admin, setup_required, registration_open}` |
| `POST` | `/api/auth/setup` | `{username, password}`                       | `{user:{username}}` + session cookie; `403` once closed; first caller becomes the admin |
| `POST` | `/api/auth/register` | `{username, password, invite}`            | `{user:{username}}` + session cookie; invite-gated signup (open path); `410` once the invite is spent/expired |
| `POST` | `/api/auth/login` | `{username, password}`                       | `{user:{username}}` + session cookie; `401` on a bad pair |
| `POST` | `/api/auth/logout` | —                                           | `204`, session revoked                |
| `POST` | `/api/invites`   | `{note?}` (admin)                            | `{invite:{id,code,url,status,note,created_at,expires_at?}}` — `code`/`url` shown once, at creation |
| `GET`  | `/api/invites`   | — (admin)                                    | `{invites:[{id,status,note,created_at,expires_at?,used_at?}]}` — never the raw `code` |
| `DELETE`| `/api/invites/{id}` | — (admin)                                 | `204`; revokes a pending link (or trims a spent one) |
| `GET`  | `/healthz`     | —                                               | `{status, indexed}`                  |

Every `/api/*` endpoint requires a session except the auth handshake routes
above (`state`, `setup`, `register`, `login`) and the admin-only invite routes
(which require a session *and* the admin role). `/healthz` is deliberately open
so the container healthcheck works. Unauthenticated API calls get `401` with a
JSON body; an unauthenticated browser hitting `/` gets the login screen.

`/api/ask` remains for scripted one-off questions. The UI uses the `/api/chats`
endpoints, which save the thread and stream the answer.

Example — log in once, keep the cookie, then call the API:

```bash
curl -c jar -X POST localhost:8080/api/auth/login -H 'content-type: application/json' \
  -d '{"username":"keeper","password":"your-passphrase"}'

curl -b jar 'http://localhost:8080/api/search?corpus=mtg&q=ward'
curl -b jar 'http://localhost:8080/api/card?q=Lightning%20Bolt'
curl -b jar 'http://localhost:8080/api/card/search?q=bolt&limit=8'
curl -b jar -X POST localhost:8080/api/ask -H 'content-type: application/json' \
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
- **MTG card names** — [MTGJSON](https://mtgjson.com)'s `AtomicCards` (one entry per unique card name). Indexed on first run into a dictionary the chat uses to detect card mentions the text heuristics miss (lowercase, unquoted, no Title Case).
- **D&D** — the community [5e SRD in Markdown](https://github.com/downfallx/dnd-5e-srd-markdown).

Both are fetched and indexed at first run. Overrides: `MTG_RULES_URL`, `MTGJSON_URL`, `DND_REPO`, `DND_REF`.

## Architecture

```
cmd/grimoire      CLI entrypoint: `serve` (default) | `index`
internal/
  auth/           accounts + server-side sessions (argon2id, same SQLite file)
  cards/          Scryfall card lookup (named + search), cached + rate-limited
  chat/           saved conversations + messages (same SQLite file as the index)
  data/           fetch + parse MTG (.txt) and D&D (markdown) into records
  index/          SQLite + FTS5 store; full-text + rule-number search
  llm/            Anthropic Messages client (configurable base URL), RAG + streaming
  server/         HTTP handlers, JSON API, SSE chat endpoint, session gate
  study/          spaced-repetition reviews (SM-2) over the corpus (same SQLite file)
web/
  templates/      the app shell, plus the login / first-run gate
  static/js/      ES modules: app, chat, palette, drawer, render, markdown, api, auth
```

The binary embeds all front-end assets, so the runtime image is just the binary + CA certs.

Chat history and accounts live in the **same SQLite file** as the rules index.
That is safe: `grimoire index` only clears the document tables, so rebuilding the
index never drops a conversation or signs anyone out. Keep the `/data` volume and
both survive upgrades.

## Development

```bash
go build ./...
go vet ./...
go test ./...
```

The front end is embedded in the binary, so a CSS or JS edit needs a rebuild to
show up — there is no dev server watching files.

Working on the interface? Read **[docs/DESIGN.md](docs/DESIGN.md)** first. It
covers the token system, the two icon systems and when to use each, the
nine-slice contract, how the four themes are derived from the backdrop art, and
the invariants that keep the pixel art crisp and the text readable.
[CLAUDE.md](CLAUDE.md) is the short orientation for the repo as a whole.

### Regenerating the art

The shipped assets in `web/static/assets/` and `web/static/fonts/` are checked
in, so a normal build needs none of this. The scripts exist for when the art or
the fonts change. They are pure-stdlib Python 3 — no pip install.

```bash
python3 scripts/build-assets.py     # sprites, nine-slice frames, scenes, favicons
python3 scripts/fetch-gameicons.py  # web/static/js/gameicons.js
python3 scripts/fetch-fonts.py      # web/static/fonts/ + fonts.css
```

`build-assets.py` reads the upstream art packs from `icon-packs/`, which is
**gitignored on purpose** — one of the licences permits adapting the material
but not republishing the pack, so the repository carries only the derived
output. Get the packs from the links in [ATTRIBUTIONS.md](ATTRIBUTIONS.md)
before running it. Recolours in that script are exhaustive: an unmapped colour
fails the build rather than shipping half-recoloured, so an upstream change
surfaces instead of going quiet.

## Credits

The interface is built from other people's art — pixel UI by Crusenho, icons by
Shikashi, backdrops by Admurin, vectors from game-icons.net, and Andrew Gioia's
Mana and Keyrune fonts. Full credits, licences and what was adapted are in
[ATTRIBUTIONS.md](ATTRIBUTIONS.md). The app links the same list from its
Settings popup.

## License

Source under the MIT license. Rule text remains the property of Wizards of the Coast; this project indexes it for personal reference only. The bundled art and fonts are under their own licences — see [ATTRIBUTIONS.md](ATTRIBUTIONS.md).
