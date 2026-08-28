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

import colorsys
import collections
import math
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
THEMES_CSS = os.path.join(ROOT, "web", "static", "themes.css")

# The lightness ladder Grimoire's chrome walks, and the warm hue it walks it
# at by default. Themes keep every step on this ladder and change only the
# hue, so a retinted chrome is exactly as legible as the original — the
# contrast between any two steps is a property of the ladder, not the colour.
STONE_L = {
    "950": 0.035, "900": 0.059, "800": 0.084, "700": 0.112,
    "600": 0.145, "500": 0.192, "line": 0.229, "400": 0.270,
}
BASE_HUE, BASE_SAT = 33 / 360, 0.27

GOLD = {"bright": "#ffd071", "mid": "#c9a227", "dim": "#8a7024", "pale": "#ffe9a8"}

# Chrome should take a scene's colour, not its intensity. A backdrop of
# saturated sky would otherwise drag the whole interface with it.
MAX_THEME_SAT = 0.30


def ramp(hue, sat):
    """The stone ladder at a given hue. Returns {step: "#rrggbb"}."""
    out = {}
    for step, lightness in STONE_L.items():
        r, g, b = colorsys.hls_to_rgb(hue, lightness, sat)
        out[step] = "#%02x%02x%02x" % (round(r * 255), round(g * 255), round(b * 255))
    return out


STONE = ramp(BASE_HUE, BASE_SAT)


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

    # Pixel RPG UI Pack: one sheet, from which we take the plate the window
    # manager's tabs and title bars are cut from. bsdtar reads rar as well as
    # 7z, so no second extractor.
    rpg = os.path.join(work, "rpg")
    os.makedirs(rpg, exist_ok=True)
    subprocess.run(["bsdtar", "-xf", need("Pixel RPG UI Pack.rar"), "-C", rpg], check=True)

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
        "rpg": os.path.join(rpg, "Ui.png"),
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

def stone_frames(sprite, stone, outdir):
    """The frames that carry a theme's colour, emitted into `outdir`.

    These five are the chrome: the panels behind the palette and the settings
    popup, the inset the composer sits in, and the progress bar. Everything
    else — parchment, gold, the book itself — is shared, because the room
    changes between themes and the tome does not.
    """
    os.makedirs(outdir, exist_ok=True)

    def emit(name, img):
        pngkit.write(os.path.join(outdir, name + ".png"), img)

    emit("panel-stone", sprite("Slot01a").recolour({
        "#45292a": stone["700"],   # fill
        "#634041": stone["line"],  # outer edge
        "#825f60": stone["500"],   # bevel highlight
        "#381e1f": stone["800"],   # inner top shade
    }))
    emit("panel-stone-active", sprite("Slot01b").recolour({
        "#4d2a26": stone["600"],
        "#6d403b": stone["500"],
        "#8e5e58": stone["400"],
        "#3f1f1c": stone["700"],
    }))
    emit("field", sprite("Frame01a").recolour({
        "#c6b09b": stone["900"],   # fill
        "#8a7868": stone["950"],   # outer edge
        "#ffe3c6": stone["line"],  # inner highlight
    }))
    # Focus keeps gold, which never changes — but its fill is themed, so the
    # sprite still has to be generated per theme.
    emit("field-focus", sprite("Frame01a").recolour({
        "#c6b09b": stone["900"],
        "#8a7868": GOLD["dim"],
        "#ffe3c6": GOLD["mid"],
    }))
    emit("bar", sprite("Bar01a").recolour({
        "#45292a": stone["900"],
        "#634041": stone["line"],
        "#825f60": stone["500"],
    }))


# The Pixel RPG UI Pack's plate, at (747, 372) on its sheet: 46x12, and
# exactly two colours arranged in concentric rings — border, a dark inset
# line, then fill. That ring structure is what makes it sliceable at 3 on all
# four sides (`border-image-slice: 3 fill` against `border-width: 6px`, the 2x
# every other frame here uses), and it is the same shape make-field-sprites.py
# had to rebuild the field frames into by hand. This one arrives that way.
PLATE = (747, 372, 46, 12)
PLATE_EDGE, PLATE_LINE = "#6d4e54", "#201727"


def plate_frames(plate, stone, outdir):
    """Tabs and title bars, recoloured onto a theme's stone ladder.

    A tabbed container and a window's title bar are the two places the old
    chrome had nothing to say: tabs borrowed the chip sprite meant for inline
    pills, and title bars used the 2px progress bar. Both now get a plate that
    was drawn as a plate.
    """
    os.makedirs(outdir, exist_ok=True)

    def emit(name, mapping):
        pngkit.write(os.path.join(outdir, name + ".png"), plate.recolour(mapping))

    # An unselected tab recedes into the chrome behind it.
    emit("tab", {PLATE_EDGE: stone["600"], PLATE_LINE: stone["900"]})
    # The selected one takes gold on its inset line — the same "this is the
    # live one" gold that field-focus uses, so the two agree.
    emit("tab-active", {PLATE_EDGE: stone["500"], PLATE_LINE: GOLD["dim"]})
    # A title bar sits on the window's own fill (stone 700), so it steps one
    # rung up to separate from it without becoming a second surface.
    emit("titleplate", {PLATE_EDGE: stone["600"], PLATE_LINE: stone["800"]})


def build_frames(ui, rpg, themes):
    def sprite(name):
        return pngkit.read(os.path.join(ui, f"UI_TravelBook_{name}.png"))

    plate = pngkit.read(rpg).crop(*PLATE)

    os.makedirs(os.path.join(OUT, "ui"), exist_ok=True)

    def emit(name, img):
        pngkit.write(os.path.join(OUT, "ui", name + ".png"), img)

    # Parchment panel — content surfaces. Ships as-is; style.css's parchment
    # tokens are taken from these exact pixels so fill and frame agree.
    emit("panel-parchment", sprite("Popup01a"))

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
    emit("bar-fill", sprite("Fill01a").recolour({"#e5b67f": GOLD["mid"]}))

    # The book itself: cover frames the sign-in gate, page frames the drawer.
    # The cover doubles in size so its corner filigree survives at UI scale.
    emit("book-cover", sprite("BookCover01a").scale(2))
    emit("page", sprite("BookPageRight01a"))

    # Opt-in pointer. Doubled, because a 14px cursor disappears on a
    # high-density display.
    emit("cursor", sprite("Cursor01c").scale(2))
    emit("cursor-press", sprite("Cursor01d").scale(2))

    # The default (no backdrop) chrome, then one set per theme.
    stone_frames(sprite, STONE, os.path.join(OUT, "ui"))
    plate_frames(plate, STONE, os.path.join(OUT, "ui"))
    for key, theme in themes.items():
        stone_frames(sprite, theme["stone"], os.path.join(OUT, "ui", key))
        plate_frames(plate, theme["stone"], os.path.join(OUT, "ui", key))


# ------------------------------------------------------------------ scenes

def build_scenes(scenes):
    """Copy the layers and read each scene's colour back out of its own art."""
    themes = {}
    for key, src in scenes.items():
        dst = os.path.join(OUT, "scenes", key)
        os.makedirs(dst, exist_ok=True)
        layers = sorted(f for f in os.listdir(src)
                        if f.endswith(".png") and f[:-4].isdigit())
        if not layers:
            sys.exit(f"scene {key}: no numbered layers in {src}")
        for f in layers:
            shutil.copyfile(os.path.join(src, f), os.path.join(dst, f))

        hue, sat = scene_hue(dst, layers)
        themes[key] = {
            "layers": len(layers),
            "hue": hue,
            "sat": sat,
            "stone": ramp(hue, sat),
            "sky": top_colour(os.path.join(dst, layers[0])),
        }
        print(f"  scene {key:11s} {len(layers)} layers  "
              f"hue {hue * 360:5.1f}°  sat {sat:.2f}  sky {themes[key]['sky']}")
    return themes


def scene_hue(d, layers):
    """A scene's representative hue and saturation.

    Hue is circular, so it is averaged as a unit vector rather than as a
    number — otherwise a scene straddling 0° averages to cyan. Each pixel
    votes in proportion to its own saturation, so the large flat greys that
    dominate these images by area do not drown out the colour that gives the
    scene its character.
    """
    x = y = weight = 0.0
    sats = []
    for name in layers:
        img = pngkit.read(os.path.join(d, name))
        counts = collections.Counter()
        for i in range(0, len(img.px), 4):
            if img.px[i + 3] < 128:
                continue
            counts[(img.px[i], img.px[i + 1], img.px[i + 2])] += 1
        for (r, g, b), n in counts.items():
            h, l, s = colorsys.rgb_to_hls(r / 255, g / 255, b / 255)
            if s < 0.06 or l < 0.04 or l > 0.95:
                continue
            w = n * s
            x += math.cos(h * 2 * math.pi) * w
            y += math.sin(h * 2 * math.pi) * w
            weight += w
            sats.append((s, n))
    if not weight:
        return BASE_HUE, BASE_SAT
    total = sum(n for _, n in sats)
    hue = (math.atan2(y, x) / (2 * math.pi)) % 1.0
    sat = min(sum(s * n for s, n in sats) / total, MAX_THEME_SAT)
    return hue, sat


def top_colour(path):
    """The dominant colour along the top edge of the backmost layer.

    The art is 216px tall and the viewport is not, so above the scene sits a
    band of nothing. Filling it with this colour continues the sky (or the
    cave ceiling) instead of cutting it off with a hard line.
    """
    img = pngkit.read(path)
    counts = collections.Counter()
    for yy in range(min(8, img.h)):
        for xx in range(img.w):
            r, g, b, a = img.at(xx, yy)
            if a >= 128:
                counts[(r, g, b)] += 1
    if not counts:
        return "#000000"
    r, g, b = counts.most_common(1)[0][0]
    return "#%02x%02x%02x" % (r, g, b)


THEMES_HEAD = """/* Per-backdrop themes. Generated by scripts/build-assets.py — do not edit.

   Each scene's hue is measured from its own art and applied to the chrome's
   existing lightness ladder, so a retinted interface is exactly as legible as
   the default one: only the hue moves, never the contrast between steps.

   What changes is the room — the stone panels, the composer's inset, the
   backdrop's own sky. What does not change is the tome: parchment, ink and
   gold are identical in every theme, because that is the surface the reading
   happens on and it is the one thing that should feel constant. The corpus
   accent stays too; blue-for-Magic and red-for-D&D mean something, and a
   backdrop should not restate it. */
"""


def write_themes(themes):
    out = [THEMES_HEAD]
    for key, theme in themes.items():
        stone = theme["stone"]
        steps = "\n".join(f"\t--stone-{k}: {v};" for k, v in stone.items())
        out.append(f"""
html[data-scene="{key}"] {{
{steps}
\t--scene-sky: {theme['sky']};
\t--frame-stone: url("/static/assets/ui/{key}/panel-stone.png");
\t--frame-stone-active: url("/static/assets/ui/{key}/panel-stone-active.png");
\t--frame-field: url("/static/assets/ui/{key}/field.png");
\t--frame-field-focus: url("/static/assets/ui/{key}/field-focus.png");
\t--frame-bar: url("/static/assets/ui/{key}/bar.png");
\t--frame-tab: url("/static/assets/ui/{key}/tab.png");
\t--frame-tab-active: url("/static/assets/ui/{key}/tab-active.png");
\t--frame-titleplate: url("/static/assets/ui/{key}/titleplate.png");
}}""")
    open(THEMES_CSS, "w").write("\n".join(out) + "\n")
    print(f"  themes.css: {len(themes)} themes")


# --------------------------------------------------------------------- main

def main():
    with tempfile.TemporaryDirectory() as work:
        src = unpack(work)
        print("icons:")
        sheet = build_icons(src["icons"])
        print(f"  icons32.png {sheet.w}x{sheet.h} "
              f"({sheet.h // CELL} rows of {SHEET_COLS})")
        # Scenes first: each one's colour decides its theme's frames.
        print("scenes:")
        themes = build_scenes(src["scenes"])
        print("frames:")
        build_frames(src["ui"], src["rpg"], themes)
        shared = len([f for f in os.listdir(os.path.join(OUT, "ui"))
                      if f.endswith(".png")])
        print(f"  {shared} shared, {len(themes)} themed sets")
        print("themes:")
        write_themes(themes)
    print(f"\nwrote {OUT}")


if __name__ == "__main__":
    main()
