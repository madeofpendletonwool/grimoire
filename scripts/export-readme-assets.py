#!/usr/bin/env python3
"""Export README art from the committed game assets.

The README shows the app's own iconography instead of emoji: pixel sprites
cropped from the committed 32px sheet, vector glyphs rendered from the
committed game-icons path data in the app's gold, and one parallax scene
composited to a single strip. Everything is derived from files already in the
repository — no upstream art packs are needed, so this runs anywhere.

Outputs to docs/assets/ (all committed):
  sprites/<name>.png       1x cells (32px), as the app draws them
  sprites/<name>@2x.png    pre-doubled cells, for sizes the app uses 2x at
  sprites/<name>@3x.png    pre-tripled cells (the welcome-sigil scale)
  icons/<name>.svg         vector glyphs, filled with the app's --gold
  scene-<name>.png         every parallax layer stacked and pre-doubled
  ui/, fonts/, favicon-32.png, icon-192.png
                          straight copies for the MkDocs site, which is
                          skinned with the app's own chrome (frames,
                          webfonts, favicon) — see stylesheets/grimoire.css

The named cells and glyph paths are parsed from web/static/js/icons.js and
web/static/js/gameicons.js, so this script can never drift from what the app
actually renders.
"""

import json
import os
import re
import shutil
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import pngkit

ROOT = os.path.normpath(os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
SHEET = os.path.join(ROOT, "web", "static", "assets", "sprites", "icons32.png")
SCENES = os.path.join(ROOT, "web", "static", "assets", "scenes")
ICONS_JS = os.path.join(ROOT, "web", "static", "js", "icons.js")
GAMEICONS_JS = os.path.join(ROOT, "web", "static", "js", "gameicons.js")
OUT = os.path.join(ROOT, "docs", "assets")

SHEET_COLS = 16
CELL = 32
# The gold the app's frames and buttons use (style.css --gold), readable on
# both light and dark README themes.
GOLD = "#c9a227"

# Sprites the README shows, and the scales to emit for each. 1x everywhere a
# bullet sits beside text (as in the app's own sidebar); the spellbook also
# gets the 2x and 3x scales the gate and welcome screens use.
SPRITES = {
    "spellbook": (1, 2, 3),  # the wordmark, the gate, the welcome
    "staff": (1,),           # chat first — the sage
    "openBook": (1,),        # conversations are saved
    "magnifier": (1,),       # full-text search
    "key": (1,),             # accounts
    "card": (1,),            # card lookup
    "swords": (1,),          # the D&D corpus
    "mirror": (1,),          # the interaction resolver
    "hourglass": (1,),       # study mode
    "scrollOpen": (1,),      # reading mode
    "orbBlue": (1,),         # the Magic corpus
    "bookGold": (1,),        # the tome
    "moon": (1,),            # themes
    "runestone": (1,),       # symbols
    "chest": (1,),           # one command to run
    "shield": (1,),          # no hard dependencies
}

# Vector glyphs the README shows — the affordances the app itself renders as
# vectors rather than sprites.
VECTORS = ["search", "mic"]

# Parallax scenes to composite into single strips.
SCENE_STRIPS = ["cave"]

# Derived output the MkDocs site reuses verbatim (nothing is rebuilt for it —
# the docs are skinned with exactly the pixels the app ships). Frames are the
# default-theme chrome; the fonts are the prose/display/pixel voices the CSS
# declares @font-face for.
SITE_UI = [
    "panel-parchment.png", "panel-stone.png", "panel-stone-active.png",
    "button.png", "button-active.png", "chip.png", "chip-active.png",
    "field.png", "field-focus.png", "page.png", "select.png",
    "bar.png", "bar-fill.png", "book-cover.png",
]
SITE_FONTS = [
    "eb-garamond-400.woff2", "eb-garamond-400-italic.woff2",
    "eb-garamond-600.woff2", "eb-garamond-700.woff2",
    "cinzel-600.woff2", "cinzel-700.woff2", "departure-mono.woff2",
]
SITE_ROOT_FILES = ["favicon-32.png", "icon-192.png"]


def named_cells():
    """name -> sheet index, parsed from the ICONS map the app renders with."""
    src = open(ICONS_JS, encoding="utf-8").read()
    block = re.search(r"export const ICONS = Object\.freeze\(\{(.*?)\n\}\);", src, re.S)
    if not block:
        sys.exit("could not find the ICONS map in js/icons.js")
    cells = {}
    for name, index in re.findall(r"(\w+):\s*(\d+),", block.group(1)):
        cells[name] = int(index)
    return cells


def vector_paths():
    """name -> [path data], parsed from GAME_ICONS."""
    src = open(GAMEICONS_JS, encoding="utf-8").read()
    block = re.search(r"export const GAME_ICONS = Object\.freeze\(\{(.*?)\n\}\);", src, re.S)
    if not block:
        sys.exit("could not find the GAME_ICONS map in js/gameicons.js")
    glyphs = {}
    for entry in re.finditer(r'"(\w+)":\s*\[(.*?)\]', block.group(1), re.S):
        glyphs[entry.group(1)] = re.findall(r'"([^"]+)"', entry.group(2))
    return glyphs


def export_sprites(cells):
    sheet = pngkit.read(SHEET)
    if sheet.w != SHEET_COLS * CELL:
        sys.exit(f"icon sheet is {sheet.w}px wide; expected {SHEET_COLS * CELL}px")
    outdir = os.path.join(OUT, "sprites")
    os.makedirs(outdir, exist_ok=True)
    for name, scales in SPRITES.items():
        index = cells.get(name)
        if index is None:
            sys.exit(f"unknown sprite {name!r} — add it to ICONS in js/icons.js")
        x, y = (index % SHEET_COLS) * CELL, (index // SHEET_COLS) * CELL
        cell = sheet.crop(x, y, CELL, CELL)
        for scale in scales:
            suffix = "" if scale == 1 else f"@{scale}x"
            pngkit.write(os.path.join(outdir, f"{name}{suffix}.png"), cell.scale(scale))


def export_vectors(glyphs):
    outdir = os.path.join(OUT, "icons")
    os.makedirs(outdir, exist_ok=True)
    for name in VECTORS:
        paths = glyphs.get(name)
        if not paths:
            sys.exit(f"unknown vector {name!r} — add it to scripts/fetch-gameicons.py")
        body = "".join(f'<path fill="{GOLD}" d="{d}"/>' for d in paths)
        svg = (f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 512 512" '
               f'width="32" height="32">{body}</svg>')
        with open(os.path.join(outdir, f"{name}.svg"), "w", encoding="utf-8") as fh:
            fh.write(svg)


def export_scenes():
    for key in SCENE_STRIPS:
        d = os.path.join(SCENES, key)
        layers = sorted((f for f in os.listdir(d) if f.endswith(".png")),
                        key=lambda f: -int(f[:-4]))  # highest index (sky) first
        if not layers:
            sys.exit(f"no scene layers in {d}")
        w, h = (lambda im: (im.w, im.h))(pngkit.read(os.path.join(d, layers[0])))
        strip = pngkit.Image(w, h)
        for fname in layers:
            strip.paste(pngkit.read(os.path.join(d, fname)), 0, 0)
        pngkit.write(os.path.join(OUT, f"scene-{key}.png"), strip.scale(2))


def export_site():
    """Copy the app's own chrome into docs/assets for the MkDocs site."""
    static = lambda *p: os.path.join(ROOT, "web", "static", *p)
    for names, src in ((SITE_UI, static("assets", "ui")),
                       (SITE_FONTS, static("fonts")),
                       (SITE_ROOT_FILES, static("assets"))):
        parts = os.path.relpath(src, static()).split(os.sep)
        if parts[0] == "assets":  # web/static/assets/ui -> docs/assets/ui
            parts = parts[1:]
        outdir = os.path.join(OUT, *parts)
        os.makedirs(outdir, exist_ok=True)
        for name in names:
            shutil.copyfile(os.path.join(src, name), os.path.join(outdir, name))


def main():
    export_sprites(named_cells())
    export_vectors(vector_paths())
    export_scenes()
    export_site()
    print(json.dumps({"sprites": sorted(SPRITES), "vectors": VECTORS,
                      "scenes": SCENE_STRIPS, "out": os.path.relpath(OUT, ROOT)}))


if __name__ == "__main__":
    main()
