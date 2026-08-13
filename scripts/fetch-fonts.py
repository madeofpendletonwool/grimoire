#!/usr/bin/env python3
"""Vendor Grimoire's webfonts into web/static/fonts/.

The app ships as one self-contained binary and has to work on a box with no
internet, so nothing may load from a CDN at runtime. This pulls the woff2
files once and writes web/static/fonts.css with the @font-face rules that
point at them.

    python3 scripts/fetch-fonts.py

Four families, four jobs:
  EB Garamond    rules prose and answers — an old-style serif with real italics
  Cinzel         the wordmark and headings — carved Roman capitals
  Departure Mono the pixel voice: keycaps, section labels, badges
  Mana/Keyrune   Magic's mana, tap and set symbols as actual glyphs

Only the `latin` subsets are taken; Grimoire's own chrome is English and rule
text is ASCII. Card names from Scryfall fall back to the system serif.
"""

import io
import os
import re
import sys
import urllib.request
import zipfile

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
FONTS = os.path.join(ROOT, "web", "static", "fonts")
CSS = os.path.join(ROOT, "web", "static", "fonts.css")

UA = ("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 "
      "(KHTML, like Gecko) Chrome/120.0 Safari/537.36")

# Google Fonts serves a different file per subset; we keep `latin` only.
GOOGLE = {
    "eb-garamond": ("EB+Garamond:ital,wght@0,400;0,600;0,700;1,400", [
        ("normal", 400), ("normal", 600), ("normal", 700), ("italic", 400),
    ]),
    "cinzel": ("Cinzel:wght@600;700", [("normal", 600), ("normal", 700)]),
}

DEPARTURE = ("https://github.com/rektdeckard/departure-mono/releases/latest"
             "/download/DepartureMono-1.500.zip")
SYMBOL_FONTS = {
    "mana": "https://cdn.jsdelivr.net/npm/mana-font@latest/fonts/mana.woff2",
    "keyrune": "https://cdn.jsdelivr.net/npm/keyrune@latest/fonts/keyrune.woff2",
}
SYMBOL_CSS = {
    "mana": "https://cdn.jsdelivr.net/npm/mana-font@latest/css/mana.min.css",
    "keyrune": "https://cdn.jsdelivr.net/npm/keyrune@latest/css/keyrune.min.css",
}

FACE = re.compile(r"/\* (\S+) \*/\s*@font-face \{(.*?)\}", re.S)
FIELD = {k: re.compile(rf"{k}:\s*([^;]+);") for k in
         ("font-style", "font-weight", "src")}
URL = re.compile(r"url\((https://[^)]+\.woff2)\)")


def get(url, binary=True):
    req = urllib.request.Request(url, headers={"User-Agent": UA})
    with urllib.request.urlopen(req, timeout=30) as r:
        data = r.read()
    return data if binary else data.decode("utf-8")


def google(slug, spec, wanted):
    """Download the latin woff2 for each (style, weight) we asked for."""
    css = get(f"https://fonts.googleapis.com/css2?family={spec}&display=swap",
              binary=False)
    found = {}
    for subset, block in FACE.findall(css):
        if subset != "latin":
            continue
        style = FIELD["font-style"].search(block).group(1).strip()
        weight = int(FIELD["font-weight"].search(block).group(1).strip())
        url = URL.search(FIELD["src"].search(block).group(1)).group(1)
        found[(style, weight)] = url

    written = []
    for style, weight in wanted:
        url = found.get((style, weight))
        if not url:
            sys.exit(f"{slug}: no latin face for {style} {weight}")
        name = f"{slug}-{weight}{'-italic' if style == 'italic' else ''}.woff2"
        blob = get(url)
        open(os.path.join(FONTS, name), "wb").write(blob)
        written.append((name, style, weight, len(blob)))
        print(f"  {name:32s} {len(blob) // 1024:3d}KB")
    return written


def departure():
    blob = get(DEPARTURE)
    with zipfile.ZipFile(io.BytesIO(blob)) as z:
        hit = next((n for n in z.namelist()
                    if n.endswith(".woff2") and "__MACOSX" not in n), None)
        if not hit:
            sys.exit("departure-mono: no woff2 in release zip")
        data = z.read(hit)
    open(os.path.join(FONTS, "departure-mono.woff2"), "wb").write(data)
    print(f"  {'departure-mono.woff2':32s} {len(data) // 1024:3d}KB  ({hit})")


def symbols():
    """Mana and Keyrune: the woff2, plus their class CSS repointed at it."""
    for name, url in SYMBOL_FONTS.items():
        blob = get(url)
        open(os.path.join(FONTS, f"{name}.woff2"), "wb").write(blob)
        print(f"  {name + '.woff2':32s} {len(blob) // 1024:3d}KB")

    for name, url in SYMBOL_CSS.items():
        css = get(url, binary=False).lstrip("﻿")
        # Upstream @font-face points at its own relative paths and at formats
        # we do not ship. Drop it; fonts.css declares the family itself.
        css = re.sub(r"@font-face\s*\{.*?\}", "", css, flags=re.S).strip()
        out = os.path.join(ROOT, "web", "static", f"{name}.css")
        header = (f"/* {name}: glyph classes from the upstream package, with "
                  f"its @font-face stripped.\n   The family is declared in "
                  f"fonts.css against our self-hosted woff2. */\n")
        open(out, "w").write(header + css + "\n")
        print(f"  {name + '.css':32s} {len(css) // 1024:3d}KB")


FONTS_CSS_HEAD = """/* Self-hosted webfonts. Generated by scripts/fetch-fonts.py.

   Nothing here may reference a CDN: Grimoire is a single binary that has to
   render correctly on a machine with no route to the internet. `swap` so a
   cold cache shows the fallback serif rather than nothing. */
"""


def write_css(garamond, cinzel):
    lines = [FONTS_CSS_HEAD]

    def face(family, file, style, weight):
        lines.append(f"""
@font-face {{
\tfont-family: "{family}";
\tfont-style: {style};
\tfont-weight: {weight};
\tfont-display: swap;
\tsrc: url("/static/fonts/{file}") format("woff2");
}}""")

    for name, style, weight, _ in garamond:
        face("EB Garamond", name, style, weight)
    for name, style, weight, _ in cinzel:
        face("Cinzel", name, style, weight)
    face("Departure Mono", "departure-mono.woff2", "normal", 400)
    face("Mana", "mana.woff2", "normal", 400)
    face("Keyrune", "keyrune.woff2", "normal", 400)
    open(CSS, "w").write("\n".join(lines) + "\n")
    print(f"\nwrote {CSS}")


def main():
    os.makedirs(FONTS, exist_ok=True)
    print("google fonts:")
    written = {slug: google(slug, spec, wanted)
               for slug, (spec, wanted) in GOOGLE.items()}
    print("departure mono:")
    departure()
    print("magic symbols:")
    symbols()
    write_css(written["eb-garamond"], written["cinzel"])
    total = sum(os.path.getsize(os.path.join(FONTS, f))
                for f in os.listdir(FONTS))
    print(f"{len(os.listdir(FONTS))} files, {total // 1024}KB in {FONTS}")


if __name__ == "__main__":
    main()
