#!/usr/bin/env python3
"""Rebuild the field nine-slice sprites for every theme.

The shipped field.png / field-focus.png carry their bevel at inconsistent
depths per side: the bright line exists only on the left and bottom edges,
while the top and right render as [fill, dark, fill] — bands indistinguishable
from the fill inside and the chamber behind, so those two sides of every
framed input (the composer, encounter/deck/session fields) read as missing.

This regenerates each sprite as a symmetric groove using tones already in the
sprite: outermost 1px = the dark lip, next 1px = the bright lip (gold on the
focus variant), interior = the fill. The four transparent corner pixels of the
originals are kept. Run from the repo root; rewrites the eight PNGs in place.
"""

from pathlib import Path

from PIL import Image

THEMES = ["cave", "deadforest", "plains", "snowy"]
ASSETS = Path("web/static/assets/ui")


def rebuild(path: Path, focus: bool) -> None:
    im = Image.open(path).convert("RGBA")
    w, h = im.size
    px = im.load()
    mid_y, mid_x = h // 2, w // 2

    dark = px[0, mid_y]        # outer lip
    bright = px[1, mid_y]      # inner lip (gold on focus)
    fill = px[mid_x, mid_y]    # the sunk surface

    out = Image.new("RGBA", (w, h))
    op = out.load()
    for y in range(h):
        for x in range(w):
            ring = min(x, y, w - 1 - x, h - 1 - y)
            op[x, y] = dark if ring == 0 else bright if ring == 1 else fill
    # Keep the original's transparent corner notches.
    for cx, cy in ((0, 0), (w - 1, 0), (0, h - 1), (w - 1, h - 1)):
        op[cx, cy] = (*op[cx, cy][:3], px[cx, cy][3])

    out.save(path)
    print(f"rebuilt {path}  dark={dark[:3]} bright={bright[:3]} fill={fill[:3]}")


def main() -> None:
    for theme in THEMES:
        for name, focus in (("field.png", False), ("field-focus.png", True)):
            rebuild(ASSETS / theme / name, focus)


if __name__ == "__main__":
    main()
