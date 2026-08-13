"""Minimal pure-Python PNG read/write (8-bit), so asset builds need no deps.

Only what `build-assets.py` needs: decode any 8-bit colour type into flat RGBA
bytes, re-encode as RGBA, and the three operations we do to pixel art —
nearest-neighbour integer scaling, region copy, and palette swap. Every
operation here is lossless and integer-only; pixel art must never be resampled.
"""

import struct
import zlib

MAGIC = b"\x89PNG\r\n\x1a\n"
_CHANNELS = {0: 1, 2: 3, 3: 1, 4: 2, 6: 4}


class Image:
    """An RGBA8 raster. `px` is a bytearray of w*h*4 bytes."""

    def __init__(self, w, h, px=None):
        self.w = w
        self.h = h
        self.px = px if px is not None else bytearray(w * h * 4)

    def at(self, x, y):
        o = (y * self.w + x) * 4
        return tuple(self.px[o:o + 4])

    def crop(self, x, y, w, h):
        out = Image(w, h)
        for row in range(h):
            so = ((y + row) * self.w + x) * 4
            do = (row * w) * 4
            out.px[do:do + w * 4] = self.px[so:so + w * 4]
        return out

    def paste(self, src, x, y):
        for row in range(src.h):
            so = (row * src.w) * 4
            do = ((y + row) * self.w + x) * 4
            self.px[do:do + src.w * 4] = src.px[so:so + src.w * 4]

    def scale(self, factor):
        """Nearest-neighbour integer upscale. Only integers keep pixels square."""
        if factor == 1:
            return self
        w, h = self.w * factor, self.h * factor
        out = Image(w, h)
        for y in range(self.h):
            row = bytearray()
            for x in range(self.w):
                o = (y * self.w + x) * 4
                row += self.px[o:o + 4] * factor
            for k in range(factor):
                do = ((y * factor + k) * w) * 4
                out.px[do:do + w * 4] = row
        return out

    def recolour(self, mapping):
        """Swap exact colours. `mapping` is {"#rrggbb": "#rrggbb"}.

        Every opaque colour in the source must be listed, so a sprite that
        gains a new shade upstream fails the build instead of silently
        shipping half-recoloured.
        """
        table = {}
        for src, dst in mapping.items():
            table[_rgb(src)] = _rgb(dst)
        out = Image(self.w, self.h, bytearray(self.px))
        seen = set()
        for i in range(0, len(out.px), 4):
            if out.px[i + 3] < 8:
                continue
            key = (out.px[i], out.px[i + 1], out.px[i + 2])
            seen.add(key)
            if key in table:
                out.px[i], out.px[i + 1], out.px[i + 2] = table[key]
        missing = seen - set(table)
        if missing:
            raise ValueError(
                "recolour map is missing: "
                + ", ".join(sorted("#%02x%02x%02x" % c for c in missing))
            )
        return out

    def tint(self, rgb):
        """Replace RGB everywhere, keeping alpha — turns a mask into a glyph."""
        r, g, b = _rgb(rgb)
        out = Image(self.w, self.h, bytearray(self.px))
        for i in range(0, len(out.px), 4):
            if out.px[i + 3]:
                out.px[i], out.px[i + 1], out.px[i + 2] = r, g, b
        return out


def _rgb(s):
    s = s.lstrip("#")
    return (int(s[0:2], 16), int(s[2:4], 16), int(s[4:6], 16))


def read(path):
    data = open(path, "rb").read()
    if data[:8] != MAGIC:
        raise ValueError(f"{path}: not a PNG")
    i = 8
    idat = b""
    palette = trns = None
    w = h = depth = colour = None
    while i < len(data):
        length = struct.unpack(">I", data[i:i + 4])[0]
        kind = data[i + 4:i + 8]
        chunk = data[i + 8:i + 8 + length]
        i += 12 + length
        if kind == b"IHDR":
            w, h, depth, colour = struct.unpack(">IIBB", chunk[:10])
        elif kind == b"PLTE":
            palette = chunk
        elif kind == b"tRNS":
            trns = chunk
        elif kind == b"IDAT":
            idat += chunk
        elif kind == b"IEND":
            break
    if depth != 8:
        raise ValueError(f"{path}: unsupported bit depth {depth}")

    raw = zlib.decompress(idat)
    channels = _CHANNELS[colour]
    stride = w * channels
    rows = bytearray()
    prev = bytearray(stride)
    pos = 0
    for _ in range(h):
        filt = raw[pos]
        pos += 1
        line = bytearray(raw[pos:pos + stride])
        pos += stride
        if filt:
            _unfilter(line, prev, filt, channels, stride)
        rows += line
        prev = line

    img = Image(w, h)
    for y in range(h):
        line = rows[y * stride:(y + 1) * stride]
        for x in range(w):
            if colour == 3:
                idx = line[x]
                r, g, b = palette[idx * 3:idx * 3 + 3]
                a = trns[idx] if trns and idx < len(trns) else 255
            elif colour == 6:
                r, g, b, a = line[x * 4:x * 4 + 4]
            elif colour == 2:
                r, g, b = line[x * 3:x * 3 + 3]
                a = 255
            elif colour == 4:
                r, a = line[x * 2:x * 2 + 2]
                g = b = r
            else:
                r = g = b = line[x]
                a = 255
            o = (y * w + x) * 4
            img.px[o:o + 4] = bytes((r, g, b, a))
    return img


def _unfilter(line, prev, filt, bpp, stride):
    for x in range(stride):
        a = line[x - bpp] if x >= bpp else 0
        b = prev[x]
        c = prev[x - bpp] if x >= bpp else 0
        if filt == 1:
            line[x] = (line[x] + a) & 0xFF
        elif filt == 2:
            line[x] = (line[x] + b) & 0xFF
        elif filt == 3:
            line[x] = (line[x] + (a + b) // 2) & 0xFF
        else:
            p = a + b - c
            pa, pb, pc = abs(p - a), abs(p - b), abs(p - c)
            pred = a if (pa <= pb and pa <= pc) else (b if pb <= pc else c)
            line[x] = (line[x] + pred) & 0xFF


def write(path, img):
    raw = bytearray()
    for y in range(img.h):
        raw.append(0)  # filter: none — these are tiny, flat, already-small images
        raw += img.px[y * img.w * 4:(y + 1) * img.w * 4]

    def chunk(kind, body):
        head = struct.pack(">I", len(body)) + kind
        return head + body + struct.pack(">I", zlib.crc32(kind + body) & 0xFFFFFFFF)

    ihdr = struct.pack(">IIBBBBB", img.w, img.h, 8, 6, 0, 0, 0)
    out = (MAGIC
           + chunk(b"IHDR", ihdr)
           + chunk(b"IDAT", zlib.compress(bytes(raw), 9))
           + chunk(b"IEND", b""))
    open(path, "wb").write(out)
