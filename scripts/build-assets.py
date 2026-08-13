#!/usr/bin/env python3
"""Derive Grimoire's shipped pixel assets from the upstream art packs.

The packs themselves live in `icon-packs/` and are *not* committed (see
.gitignore) — Crusenho's licence permits adapting the material but not
republishing the pack. What ships is what this script produces: the icon sheet
we use, nine-slice frames recoloured into Grimoire's own palette, the scene
layers, and the favicons. Running it is only necessary when the art changes.

    python3 scripts/build-assets.py

Everything is nearest-neighbour and integer-scaled; pixel art is never
resampled. Recolour maps are exhaustive — an unmapped colour is a build error,
so an upstream tweak surfaces instead of shipping half-recoloured.
"""

import os
import shutil
import subprocess
import sys
import tempfile
import zipfile

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import pngkit  # noqa: E402

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
PACKS = os.path.join(ROOT, "icon-packs")
OUT = os.path.join(ROOT, "web", "static", "assets")

# Grimoire's chrome palette — the same values style.css exposes as tokens.
STONE = {
    "900": "#14100a", "800": "#1b1610", "700": "#241d15",
    "600": "#2f261b", "500": "#3d3225", "line": "#4a3c2b",
}
GOLD = {"bright": "#ffd071", "mid": "#c9a227", "dim": "#8a7024", "pale": "#ffe9a8"}


# ---------------------------------------------------------------- unpacking

def unpack(work):
    """Extract every pack into `work`, returning the roots we read from."""
    def need(name):
        p = os.path.join(PACKS, name)
        if not os.path.exists(p):
            sys.exit(f"missing pack: {p}\n"
                     f"Place the source art packs in icon-packs/ and re-run.")
        return p

    icons = os.path.join(work, "icons")
    with zipfile.ZipFile(need("Shikashi's Fantasy Icons Pack v2.zip")) as z:
        z.extractall(icons)

    ui = os.path.join(work, "ui")
    os.makedirs(ui, exist_ok=True)
    # bsdtar ships with macOS and most Linux distros and reads 7z.
    subprocess.run(["bsdtar", "-xf", need("Complete_UI_Book_Styles_Pack_Free.7z"),
                    "-C", ui], check=True)

    scenes = {}
    for key, name in SCENES.items():
        d = os.path.join(work, "scene-" + key)
        with zipfile.ZipFile(need(name)) as z:
            z.extractall(d)
        scenes[key] = d

    return {
        "icons": os.path.join(
            icons, "Shikashi's Fantasy Icons Pack v2",
            "#1 - Transparent Icons.png"),
        "ui": os.path.join(
            ui, "Complete_UI_Book_Styles_Pack_Free_v1.0",
            "01_TravelBookLite", "Sprites"),
        "scenes": scenes,
    }


SCENES = {
    "cave": "Parallax_Backgrounds_Cave.zip",
    "deadforest": "Parallax_Backgrounds_DeadForest.zip",
    "plains": "Parallax_Backgrounds_Plains.zip",
    "snowy": "Parallax_Backgrounds_SnowyMountains.zip",
}


# ------------------------------------------------------------------- icons

# Shikashi's sheet is an exact 16-column, 32px grid, and the pack's documented
# icon order is row-major across it — the per-category counts in the pack
# README (11, 5, 7, 16, 9, 28, 26, 16, 64, 31, 15, 11, 6, 39) line up with the
# populated cells row for row. So `index = row*16 + col` is a stable address,
# and js/icons.js names the ones we use against these same indices.
SHEET_COLS = 16
CELL = 32
SPELLBOOK = 71


def build_icons(src):
    sheet = pngkit.read(src)
    if sheet.w != SHEET_COLS * CELL:
        sys.exit(f"icon sheet is {sheet.w}px wide; expected "
                 f"{SHEET_COLS * CELL}px (16 columns of {CELL}px)")
    os.makedirs(os.path.join(OUT, "sprites"), exist_ok=True)
    pngkit.write(os.path.join(OUT, "sprites", "icons32.png"), sheet)

    # Favicons: the spellbook, at 1x for the tab and 6x for a home screen.
    x, y = (SPELLBOOK % SHEET_COLS) * CELL, (SPELLBOOK // SHEET_COLS) * CELL
    mark = sheet.crop(x, y, CELL, CELL)
    pngkit.write(os.path.join(OUT, "favicon-32.png"), mark)
    pngkit.write(os.path.join(OUT, "icon-192.png"), mark.scale(6))
    return sheet


# ------------------------------------------------------------------ frames
#
# TravelBook Lite gives us two native palettes: a parchment panel (Popup) and a
# dark leather one (Slot). Grimoire needs a third — the stone chrome — plus
# gold buttons, so those are recoloured from the same shapes. Each entry is
# (source sprite, output name, recolour map or None, scale).

def build_frames(ui):
    def sprite(name):
        return pngkit.read(os.path.join(ui, f"UI_TravelBook_{name}.png"))

    os.makedirs(os.path.join(OUT, "ui"), exist_ok=True)

    def emit(name, img):
        pngkit.write(os.path.join(OUT, "ui", name + ".png"), img)

    # Parchment panel — content surfaces. Ships as-is; style.css's parchment
    # tokens are taken from these exact pixels so fill and frame agree.
    emit("panel-parchment", sprite("Popup01a"))

    # Stone panel — app chrome. Slot's leather browns become Grimoire's stone.
    slot_to_stone = {
        "#45292a": STONE["700"],   # fill
        "#634041": STONE["line"],  # outer edge
        "#825f60": STONE["500"],   # bevel highlight
        "#381e1f": STONE["800"],   # inner top shade
    }
    emit("panel-stone", sprite("Slot01a").recolour(slot_to_stone))
    emit("panel-stone-active", sprite("Slot01b").recolour({
        "#4d2a26": STONE["600"],
        "#6d403b": STONE["500"],
        "#8e5e58": "#6e5a3d",
        "#3f1f1c": STONE["700"],
    }))

    # Inset field — the composer and resolver inputs, sunk into the stone.
    emit("field", sprite("Frame01a").recolour({
        "#c6b09b": STONE["900"],   # fill
        "#8a7868": "#0d0a06",      # outer edge
        "#ffe3c6": STONE["line"],  # inner highlight
    }))
    emit("field-focus", sprite("Frame01a").recolour({
        "#c6b09b": STONE["900"],
        "#8a7868": GOLD["dim"],
        "#ffe3c6": GOLD["mid"],
    }))

    # Chips and suggestions sit on parchment, so they keep the pack's beige.
    emit("chip", sprite("Frame01a"))
    emit("chip-active", sprite("FrameSelect01a"))

    # Gold button, and its pressed state.
    emit("button", sprite("Frame01a").recolour({
        "#c6b09b": GOLD["bright"],
        "#8a7868": GOLD["dim"],
        "#ffe3c6": GOLD["pale"],
    }))
    emit("button-active", sprite("FrameSelect01a").recolour({
        "#e5b67f": GOLD["mid"],
        "#dea974": GOLD["dim"],
    }))

    # Corner brackets for focus rings — transparent middle, no `fill`.
    emit("select", sprite("Select01a").recolour({
        "#341f20": GOLD["dim"],
        "#d0c4c0": GOLD["bright"],
    }))

    # Progress bar frame and its fill.
    emit("bar", sprite("Bar01a").recolour({
        "#45292a": STONE["900"],
        "#634041": STONE["line"],
        "#825f60": STONE["500"],
    }))
    emit("bar-fill", sprite("Fill01a").recolour({"#e5b67f": GOLD["mid"]}))

    # The book itself: cover frames the sign-in gate, page frames the drawer.
    # The cover doubles in size so its corner filigree survives at UI scale.
    emit("book-cover", sprite("BookCover01a").scale(2))
    emit("page", sprite("BookPageRight01a"))

    # Opt-in pointer. Doubled, because a 14px cursor disappears on a
    # high-density display.
    emit("cursor", sprite("Cursor01c").scale(2))
    emit("cursor-press", sprite("Cursor01d").scale(2))


# ------------------------------------------------------------------ scenes

def build_scenes(scenes):
    for key, src in scenes.items():
        dst = os.path.join(OUT, "scenes", key)
        os.makedirs(dst, exist_ok=True)
        layers = sorted(f for f in os.listdir(src)
                        if f.endswith(".png") and f[:-4].isdigit())
        if not layers:
            sys.exit(f"scene {key}: no numbered layers in {src}")
        for f in layers:
            shutil.copyfile(os.path.join(src, f), os.path.join(dst, f))
        print(f"  scene {key}: {len(layers)} layers")


# --------------------------------------------------------------------- main

def main():
    with tempfile.TemporaryDirectory() as work:
        src = unpack(work)
        print("icons:")
        sheet = build_icons(src["icons"])
        print(f"  icons32.png {sheet.w}x{sheet.h} "
              f"({sheet.h // CELL} rows of {SHEET_COLS})")
        print("frames:")
        build_frames(src["ui"])
        print(f"  {len(os.listdir(os.path.join(OUT, 'ui')))} nine-slices")
        print("scenes:")
        build_scenes(src["scenes"])
    print(f"\nwrote {OUT}")


if __name__ == "__main__":
    main()
