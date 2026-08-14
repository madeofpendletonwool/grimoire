# HTTP API

Everything the UI speaks is a plain JSON API over the same origin, with server-sent events for streamed answers. Point scripts at it, or wrap your own front end around it.

## Authentication

Every `/api/*` endpoint requires a session except the auth handshake routes (`state`, `setup`, `register`, `login`) and the admin-only invite routes (which require a session *and* the admin role). `/healthz` is deliberately open so the container healthcheck works.

Unauthenticated API calls get `401` with a JSON body; an unauthenticated browser hitting `/` gets the login screen. See [Accounts & Invites](accounts.md) for the model.

## Endpoints

| Method | Path | Body / Params | Returns |
| ------ | ---- | ------------- | ------- |
| `GET` | `/api/search` | `q`, `corpus=mtg\|dnd`, `limit` | `{ results: [{number,title,body,source}] }` |
| `GET` | `/api/section` | `number`, `corpus=mtg\|dnd` | `{ parent:{…}, children:[…] }` (full mechanic section) |
| `GET` | `/api/card` | `q` (MTG card name) | `{ card: {...} }` or `{ card: null, matches: [...] }` |
| `GET` | `/api/card/search` | `q`, `limit` (MTG card name fragment) | `{ matches: [{name,mana_cost,type_line,…}] }` |
| `POST` | `/api/ask` | `{corpus, question}`, `?nocache` | `{configured, cached, answer, sources:[…], cards:[…], entities:[…], rulings:[…], unresolved_cards:[…]}` — stateless one-shot |
| `POST` | `/api/resolve` | `{board, sequence, note}` (Magic) | SSE: `meta`, `delta`, `done`, `error` — step-by-step interaction trace |
| `GET` | `/api/chats` | — | `{chats:[…]}` |
| `POST` | `/api/chats` | `{corpus}` | `{chat:{…}}` |
| `GET` | `/api/chats/{id}` | — | `{chat:{…}, messages:[…]}` |
| `PATCH` | `/api/chats/{id}` | `{title}` | `{chat:{…}}` |
| `DELETE` | `/api/chats/{id}` | — | `204` |
| `POST` | `/api/chats/{id}/messages` | `{question}`, `?nocache` | SSE: `meta`, `delta`, `done`, `error` |
| `GET` | `/api/study/queue` | `corpus`, `topic` (optional), `limit` | `{cards:[{key,front,back,…}], stats:{total,new,due,learned}}` |
| `POST` | `/api/study/grade` | `{key, corpus, grade}` | `{card:{…, reps, interval_days, ease, due_at}}` |
| `GET` | `/api/meta` | — | `{corpora:[…], chat_configured, chat_model}` |
| `GET` | `/api/auth/state` | — | `{authenticated, username, is_admin, setup_required, registration_open}` |
| `POST` | `/api/auth/setup` | `{username, password}` | `{user:{username}}` + session cookie; `403` once closed; first caller becomes the admin |
| `POST` | `/api/auth/register` | `{username, password, invite}` | `{user:{username}}` + session cookie; invite-gated signup (open path); `410` once the invite is spent/expired |
| `POST` | `/api/auth/login` | `{username, password}` | `{user:{username}}` + session cookie; `401` on a bad pair |
| `POST` | `/api/auth/logout` | — | `204`, session revoked |
| `POST` | `/api/invites` | `{note?}` (admin) | `{invite:{id,code,url,status,note,created_at,expires_at?}}` — `code`/`url` shown once, at creation |
| `GET` | `/api/invites` | — (admin) | `{invites:[{id,status,note,created_at,expires_at?,used_at?}]}` — never the raw `code` |
| `DELETE` | `/api/invites/{id}` | — (admin) | `204`; revokes a pending link (or trims a spent one) |
| `GET` | `/healthz` | — | `{status, indexed}` |

`/api/ask` remains for scripted one-off questions. The UI uses the `/api/chats` endpoints, which save the thread and stream the answer.

## Streaming (SSE)

The chat and resolve endpoints stream over server-sent events, emitting events in this order:

```
event: meta    → grounding metadata (sources, cards, entities, rulings)
event: delta   → a token of the answer (many)
event: done    → the completed message
event: error   → terminal failure
```

## A worked session

Log in once, keep the cookie, then call the API:

```bash
curl -c jar -X POST localhost:8080/api/auth/login \
  -H 'content-type: application/json' \
  -d '{"username":"keeper","password":"your-passphrase"}'

# search
curl -b jar 'http://localhost:8080/api/search?corpus=mtg&q=ward'

# card lookup + autocomplete
curl -b jar 'http://localhost:8080/api/card?q=Lightning%20Bolt'
curl -b jar 'http://localhost:8080/api/card/search?q=bolt&limit=8'

# a grounded one-shot answer
curl -b jar -X POST localhost:8080/api/ask \
  -H 'content-type: application/json' \
  -d '{"corpus":"mtg","question":"What does Lightning Bolt do?"}'
```

To watch a streamed answer from a terminal:

```bash
curl -b jar -N -X POST localhost:8080/api/chats/1/messages \
  -H 'content-type: application/json' \
  -d '{"question":"Does hexproof stop a board wipe?"}'
```
