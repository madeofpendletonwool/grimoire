# Getting Started

The stack is plain Go with no framework and no front-end build step. Clone, build, run.

```bash
go build ./...
go vet ./...
go test ./...
```

For local serving:

```bash
go run ./cmd/grimoire serve      # builds the index on first run, then serves :8080
```

## The front end is embedded

`web/embed.go` embeds everything under `web/static/` and `web/templates/` into the binary, so **a CSS or JS edit needs a rebuild to show up** — there is no dev server watching files. The runtime image is just the binary + CA certs.

There is no bundler: `web/static/js/` is plain ES modules — chat, palette, reference drawer, study, resolve, voice, scene, icons. Nothing loads from a third party.

## Working on the interface

Read the [Design System](../DESIGN.md) first. It covers the token system, the two icon systems and when to use each, the nine-slice contract, how the four themes are derived from the backdrop art, and the invariants that keep the pixel art crisp and the text readable.

## Regenerating the art

The shipped assets in `web/static/assets/` and `web/static/fonts/` are checked in, so a normal build needs none of this. The scripts exist for when the art or the fonts change. They are pure-stdlib Python 3 — no pip install.

```bash
python3 scripts/build-assets.py     # sprites, nine-slice frames, scenes, themes.css, favicons
python3 scripts/fetch-gameicons.py  # web/static/js/gameicons.js
python3 scripts/fetch-fonts.py      # web/static/fonts/ + fonts.css
python3 scripts/export-readme-assets.py  # docs/assets/ — README + docs-site art
```

`build-assets.py` reads the upstream art packs from `icon-packs/`, which is **gitignored on purpose** — one of the licences permits adapting the material but not republishing the pack, so the repository carries only the derived output. Get the packs from the links in [ATTRIBUTIONS.md](https://github.com/madeofpendletonwool/grimoire/blob/main/ATTRIBUTIONS.md) before running it. Recolours in that script are exhaustive: an unmapped colour fails the build rather than shipping half-recoloured, so an upstream change surfaces instead of going quiet.

`export-readme-assets.py` needs no packs: it derives everything from the committed art (README sprites, vector glyphs, the composited scene strip, and the frames/fonts/favicons this documentation site is skinned with).

## CI

GitHub Actions runs on every push and PR to `main`:

- **CI** — gofmt check, `go vet`, `go build`, `go test -race`
- **Publish image** — builds the container and pushes it to GHCR (`:latest` + `:sha`)
- **Docs** — builds this MkDocs site and publishes it to GitHub Pages

## This documentation site

The docs are [Material for MkDocs](https://squidfunk.github.io/mkdocs-material/), skinned to look like the app itself: `docs/stylesheets/grimoire.css` transplants the app's tokens, fonts and nine-slice frames, and `docs/assets/` carries the derived art. Preview locally with:

```bash
pip install -r docs/requirements.txt
mkdocs serve          # http://localhost:8000
mkdocs build --strict # what CI runs
```

Pages are markdown under `docs/`, wired up in `mkdocs.yml`. On push to `main`, `.github/workflows/docs.yml` rebuilds and publishes the site automatically.
