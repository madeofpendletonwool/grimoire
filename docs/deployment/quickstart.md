![Deploy](../assets/sprites/chest.png){: style="float:right; margin-left:1rem" align=right }

# Quick Start

One command to run it:

```bash
cp .env.example .env          # fill in ANTHROPIC_API_KEY to enable the chat
docker compose up --build
```

Open <http://localhost:8080>. The first visit asks you to create the **keeper account** — pick a name and passphrase and you are in (see [Accounts & Invites](accounts.md)). Search works immediately; the Q&A chat is enabled once `ANTHROPIC_API_KEY` is set.

## The index builds itself

On first start Grimoire fetches and indexes the corpora — the MTG Comprehensive Rules and the D&D 5e SRD — plus MTGJSON's card-name dictionary and Open5e's SRD entity names. The index is cached in the `/data` volume, so subsequent starts are instant.

Rebuilds are automatic too. If you index local D&D books (`DND_DOCS_DIR`, see [Configuration](configuration.md)), every boot fingerprints that directory and rebuilds on its own when the books changed — add a PDF's extraction, restart the container, done. For anything else (a rules refresh, a forced rebuild) the admin has **Settings → Library → Rebuild the index now**, which runs in the background while the app keeps serving.

The command form still exists as an escape hatch:

```bash
docker compose run --rm grimoire index
```

!!! note "Keep the `/data` volume"

    `grimoire.db` — one SQLite file — holds the rules index, saved chat history, study progress **and** every account. Keep the volume and all of it survives upgrades and reindexes; `grimoire index` only clears the document tables, never a conversation or a login.

## Ports and such

The compose file publishes `${GRIMOIRE_PORT:-8080}:8080`. Change the host port via `GRIMOIRE_PORT` in `.env`.

The container ships with a healthcheck against the open `/healthz` endpoint, so `docker ps` and orchestrators can see readiness.

## The image

The image is a multi-stage build: `golang:1.26-alpine` builds a static binary (no CGO), which lands in `alpine:3.20` with just CA certs, timezone data and a non-root user. Prebuilt images are published on merge to `main`:

```bash
docker pull ghcr.io/madeofpendletonwool/grimoire:latest
```

The binary embeds all front-end assets, so the runtime image is just the binary — there is no web server layer, no node.

## Run locally (no Docker)

```bash
go run ./cmd/grimoire serve      # builds the index on first run, then serves :8080
# or force a rebuild:
go run ./cmd/grimoire index
```

Environment variables are the same as under Docker (`GRIMOIRE_ADDR`, `GRIMOIRE_DB`, `ANTHROPIC_*`, …) — the full list with defaults is in [Configuration](configuration.md).

## Behind a reverse proxy

Grimoire works behind any HTTPS proxy. The session cookie is marked `Secure` automatically when the request arrives over TLS or through a proxy that sets `X-Forwarded-Proto: https`, so forward that header and logins persist correctly:

```nginx
location / {
    proxy_pass http://127.0.0.1:8080;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_set_header Host $host;
    proxy_http_version 1.1;         # SSE: the streamed answers need this
    proxy_buffering off;            # SSE: don't buffer the stream
}
```

!!! tip "Streaming answers"

    Chat answers stream over SSE. Any proxy in front must not buffer event streams — the two directives above are the nginx incantation.
