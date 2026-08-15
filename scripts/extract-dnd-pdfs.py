#!/usr/bin/env python3
"""Extract D&D book PDFs into the markdown Grimoire's D&D corpus ingests.

Grimoire's D&D corpus is markdown (the fetched SRD), so local books enter the
same way: this script converts each PDF into a markdown file whose headings
carry the document structure, and `DND_DOCS_DIR=<dir>` indexes the result
alongside the SRD (see README "Local books").

    pip install pymupdf
    python3 scripts/extract-dnd-pdfs.py <pdf-or-dir>... --out <dir>

The extractor is layout-aware where it matters for rulebooks:

- Two-column pages are read column by column (left half top-to-bottom, then
  right half), so prose paragraphs stay whole instead of interleaving.
- Headings are detected from font size relative to each book's body size and
  become markdown ATX headings; Grimoire's chunker turns each heading section
  into citation-anchored records.
- Repeated running heads / footers / page numbers are dropped by frequency.
- Line wraps from the print layout are healed, both the visible hyphen
  ("ref-\nerence" -> "reference") and the soft hyphen (U+00AD) that books
  typeset from a word processor leave behind.
- Word spacing survives either encoding: a book whose OCR emits each space as
  its own text span is joined with those spaces intact, rather than arriving as
  one unsearchable run-on word.
- A "Sage Advice" PDF (official Q&A compendium) is special-cased: the
  compendium sets each question in bold italic and keeps that style running
  into the start of the answer, so a bold-italic run is cut at its question
  mark — the question becomes a heading, the remainder opens the answer. Every
  Q&A is then its own record and the sage can cite rulings as precedent.
- Scanned PDFs (no text layer) are reported and skipped; run OCR on them first
  if you want them in.

The output is plain text with headings — OCR letter substitution in the
underlying text ("leveI" for "level", "Vou" for "You") is left as-is;
full-text search tolerates it, and attempts to "fix" it risk corrupting real
words.
"""

import argparse
import re
import sys
from collections import Counter
from pathlib import Path
try:
    import pymupdf
except ImportError:  # pragma: no cover
    sys.exit("pymupdf is required: pip install pymupdf")


# ---------------------------------------------------------------- page walks


def doc_text_stats(doc):
    """Return (chars_per_page, body_size) for the document, sampling pages."""
    sizes = Counter()
    pages_chars = 0
    sampled = 0
    for pno in range(len(doc)):
        page = doc[pno]
        if pno >= 40 and pno < len(doc) - 5 and pno % 5:  # sample sparsely mid-doc
            continue
        for block in page.get_text("dict")["blocks"]:
            if block.get("type") != 0:
                continue
            for line in block["lines"]:
                for span in line["spans"]:
                    n = len(span["text"].strip())
                    if n:
                        sizes[round(span["size"], 1)] += n
                        pages_chars += n
        sampled += 1
    body = sizes.most_common(1)[0][0] if sizes else 0
    return pages_chars / max(sampled, 1), body


def repeated_headers(doc, max_pages=400):
    """Block texts that recur on many pages: running heads, footers, folios."""
    seen = Counter()
    for pno in range(0, min(len(doc), max_pages)):
        page = doc[pno]
        for block in page.get_text("dict")["blocks"]:
            if block.get("type") != 0:
                continue
            text = block_text(block["lines"]).strip()
            if 0 < len(text) < 120:
                seen[normalize_repeats(text)] += 1
    pages = min(len(doc), max_pages)
    return {t for t, n in seen.items() if pages >= 10 and n >= pages * 0.3}


def normalize_repeats(text):
    return re.sub(r"\d+", "#", text)[:120]


def block_text(lines):
    out = []
    for line in lines:
        out.append("".join(span["text"] for span in line["spans"]))
    return "\n".join(out)


# ---------------------------------------------------------------- extraction


class Line:
    """One styled text line in a column, in reading order."""

    __slots__ = ("col", "y", "y1", "size", "bold_italic", "text")

    def __init__(self, col, y, y1, size, bold_italic, text):
        self.col, self.y, self.y1 = col, y, y1
        self.size, self.bold_italic, self.text = size, bold_italic, text


def page_lines(page, drop, sage=False):
    """Yield styled fragments for one page in reading order: left column top
    to bottom, then right. Each PDF line is first split into consecutive
    same-style span groups, because OCR'd books routinely set a heading's last
    line and the paragraph after it (or an italic phrase mid-sentence) on one
    rendered line — the style boundary, not the line, is the real break.
    Fragments of one line share its y bounds and keep their x order (sort is
    stable), and each fragment carries its dominant span size and whether all
    of it is bold-italic.
    """
    mid = page.rect.width / 2
    lines = []
    for block in page.get_text("dict")["blocks"]:
        if block.get("type") != 0:
            continue
        block_is_drop = normalize_repeats(block_text(block["lines"]).strip()) in drop
        for line in block["lines"]:
            raw = line["spans"]
            if not raw:
                continue
            y0, y1 = line["bbox"][1], line["bbox"][3]
            x0, x1 = line["bbox"][0], line["bbox"][2]
            col = 0 if (x0 + x1) / 2 < mid else 1
            whole = "".join(span["text"] for span in raw).strip()
            if not whole:
                continue
            if block_is_drop or normalize_repeats(whole) in drop:
                continue
            if re.fullmatch(r"\d{1,4}", whole):  # bare page number
                continue
            # Split the line into consecutive same-size span groups. Size, not
            # weight, is the boundary that matters: a heading differs from the
            # paragraph after it by size, while an italic phrase inside a
            # sentence (a spell name inside a Sage Advice question) is the same
            # size and must not break the sentence into fragments. Whether a
            # group reads as bold-italic is then a property of the group — the
            # 80%-of-characters test below — rather than of every span in it.
            groups = []  # [ (size, {text, n, bi_n}) ]
            for span in raw:
                text = span["text"]
                if not text.strip():
                    # Whitespace-only spans are separators, not content: OCR'd
                    # books (the PHB) emit every inter-word space as its own
                    # span, and dropping them glues the whole book into
                    # unsearchable run-on words.
                    if groups:
                        groups[-1][1]["text"].append(" ")
                    continue
                font = span["font"].lower()
                bi = "bold" in font and "italic" in font
                size = round(span["size"])
                if groups and groups[-1][0] == size:
                    g = groups[-1][1]
                    g["text"].append(text)
                    g["n"] += len(text.strip())
                    if bi:
                        g["bi_n"] += len(text.strip())
                else:
                    groups.append((size, {"text": [text], "n": len(text.strip()), "bi_n": len(text.strip()) if bi else 0}))
            for size, g in groups:
                text = "".join(g["text"]).strip()
                if not text:
                    continue
                # "Mostly bold-italic" identifies display type in a rulebook.
                # In a Q&A compendium it is the wrong test: a question's last
                # line shares its line with the first words of the answer, so
                # the bold-italic share there is whatever the wrap happened to
                # leave — sometimes 0.9, sometimes 0.4. Losing the question
                # entirely on that coin flip is worse than over-including, and
                # split_question cuts the answer back off at the question mark,
                # so any bold-italic at all marks the line.
                if sage:
                    bold_italic = g["bi_n"] > 0
                else:
                    bold_italic = g["n"] > 0 and g["bi_n"] >= g["n"] * 0.8
                lines.append(Line(col, y0, y1, float(size), bold_italic, text))
    lines.sort(key=lambda l: (l.col, l.y))
    return lines


def runs(lines):
    """Group consecutive same-styled, vertically adjacent lines into runs —
    the unit a heading or paragraph is made of. Style size is quantized to a
    whole point: OCR'd PDFs measure consecutive lines of one style as 9.2 and
    9.4, and without quantizing a wrapped heading splits into pieces."""
    out = []
    cur = None
    for line in lines:
        style = (line.col, round(line.size), line.bold_italic)
        # Consecutive lines of tightly-leaded print overlap: a reported line box
        # includes ascender and descender room the next line starts inside, so
        # the gap between them is routinely negative (-4pt on 11pt leading).
        # Requiring a non-negative gap therefore refused to group any wrapped
        # line — which is why questions and headings arrived in fragments. Lines
        # are sorted top-down, so an overlap can never exceed one line height.
        height = max(line.y1 - line.y, 4)
        gap = line.y - cur["y1"] if cur is not None else 0
        adjacent = (
            cur is not None
            and cur["style"] == style
            and -height <= gap < height * 1.9
        )
        if adjacent:
            cur["text"].append(line.text)
            cur["y1"] = line.y1
        else:
            cur = {"style": style, "text": [line.text], "y": line.y, "y1": line.y1, "size": line.size, "bold_italic": line.bold_italic}
            out.append(cur)
    return out


def heading_level(size, body, levels):
    """Map a font size to a markdown heading level using the doc's size map.
    The comparison tolerates the ±0.5pt jitter OCR'd PDFs measure for one
    physical style."""
    for lvl, cutoff in levels:
        if size >= cutoff - 0.45:
            return lvl
    return 0


def looks_like_heading(text):
    if not text or len(text) > 90 or len(text) < 4:
        return False
    if text.endswith((",", ";", ":")):
        return False
    if sum(c.isalpha() for c in text) < 3:
        return False
    words = text.split()
    if not words or len(words) > 12:
        return False
    if len(words) >= 7:
        # A long "heading" must read like one: mostly words that start with a
        # capital or digit. This is what keeps letter-spaced body text
        # (Tasha's renders whole paragraphs at heading size) out of the
        # heading stream while real titles ("Known and Prepared Spells")
        # stay in.
        caps = sum(
            1 for w in words if w[:1].isupper() or w[:1].isdigit()
        )
        if caps / len(words) < 0.6:
            return False
    return True


def split_question(text):
    """Split a Sage Advice bold-italic run into its question and the opening
    words of its answer. The compendium sets each question in bold italic and
    then keeps that style running into the start of the answer ("...for the
    extra damage? The Great Weapon Fighting feature..."), so the question mark,
    not the style, is where one ends and the other begins.

    Deliberately not gated on looks_like_heading: a Sage Advice question is a
    whole sentence, and the word cap that keeps letterspaced body text out of
    the heading stream is exactly what used to reject these — leaving only the
    accidental short tails of wrapped questions ("in 4 hours?") as headings.

    A run carrying no question yields ("", ""), and the caller treats it as
    ordinary prose.
    """
    text = text.strip().lstrip("\t ").strip()
    i = text.find("?")
    if i < 0:
        return "", ""
    return text[: i + 1].strip(), text[i + 1 :].strip()


def heal_hyphens(text):
    """Join print-layout line wraps: 'ref-\\nerences' -> 'references'.

    A soft hyphen (U+00AD) is the same wrap marker in invisible form, and books
    typeset from a word processor are full of them. Left in, they split words
    no query will ever spell that way ('Hand\\xadbook', 'Di\\xadvine'), so they
    are removed along with the whitespace the wrap left behind.
    """
    text = re.sub(r"­[ \t]*\n?[ \t]*", "", text)
    text = re.sub(r"(\w)-\n(\w)", r"\1\2", text)
    return re.sub(r"[ \t]*\n[ \t]*", " ", text)


def collect_heading_sizes(doc, body, max_pages=300):
    """Heading style sizes for the level map. Sizes are quantized to whole
    points first — OCR'd PDFs measure one physical style as 10.3/10.4/10.5/10.6
    — and a style must recur (>=3 uses) to qualify. Returns up to five sizes:
    the four largest plus the most-used of the rest, so the rare giant
    part-titles and the ubiquitous run-in section heads both keep a level."""
    counts = Counter()
    for pno in range(0, min(len(doc), max_pages)):
        for run in runs(page_lines(doc[pno], frozenset())):
            if run["size"] > body + 1.2 and looks_like_heading(heal_hyphens("\n".join(run["text"]))):
                counts[round(run["size"])] += 1
    real = {s: n for s, n in counts.items() if n >= 3}
    if not real:
        return []
    by_size = sorted(real, reverse=True)
    if len(by_size) <= 5:
        return by_size
    head, tail = by_size[:4], by_size[4:]
    most_used = max(tail, key=lambda s: real[s])
    return head + [most_used]


def extract_pdf(path, out_dir, force_sage=False):
    doc = pymupdf.open(path)
    chars_per_page, body = doc_text_stats(doc)
    if chars_per_page < 60:
        print(f"  SKIP {path.name}: no text layer ({chars_per_page:.0f} chars/page) — scan? Run OCR first.")
        doc.close()
        return False

    sage = force_sage or "sage" in path.name.lower()
    drop = repeated_headers(doc)

    # Heading size buckets map, descending, to '##'.. — the doc title comes
    # from the filename.
    print(f"  ...  {path.name}: scanning heading styles")
    ordered = sorted(collect_heading_sizes(doc, body), reverse=True)[:5]
    levels = [(i + 2, s) for i, s in enumerate(ordered)]

    md = [f"# {title_from_filename(path, sage)}", ""]
    sections = 0
    pages = len(doc)
    # The previous md entry is prose that an unfinished sentence can continue —
    # but never a heading: a heading without a period is normal ("Spell
    # Level"), and gluing the paragraph after it into the heading line is how
    # letterspaced books once produced 26-word headings.
    def unfinished():
        return (
            len(md) > 1
            and md[-1]
            and not md[-1].startswith("#")
            and not re.search(r"[.!?…:;]$", md[-1])
        )

    for pno in range(pages):
        for run in runs(page_lines(doc[pno], drop, sage)):
            # Joined with newlines, not spaces: heal_hyphens needs to see where
            # each printed line ended to tell a wrap ("mod-\nifiers") from a
            # real compound ("hand-crossbow"). It collapses them to spaces
            # itself once the wraps are healed.
            text = heal_hyphens("\n".join(run["text"])).strip()
            if not text:
                continue
            if sage and run["bold_italic"]:
                question, answer = split_question(text)
                if question:
                    md.append("")
                    md.append(f"#### {question}")
                    sections += 1
                    if answer:
                        md.append("")
                        md.append(answer)
                    continue
            lvl = heading_level(run["size"], body, levels)
            if lvl and looks_like_heading(text):
                md.append("")
                md.append(f"{'#' * lvl} {text}")
                sections += 1
                continue
            if unfinished():
                md[-1] += " " + text
            else:
                md.append("")
                md.append(text)
    doc.close()

    if sage:
        md = merge_question_headings(md)
    else:
        md = merge_split_headings(md)

    out = out_dir / (slugify(path.stem) + ".md")
    out.write_text("\n".join(md).strip() + "\n", encoding="utf-8")
    print(f"  OK   {path.name} -> {out.name} ({sections} headings, {pages} pages)")
    return True


def merge_split_headings(md):
    """Join consecutive headings of the same level whose text was fragmented
    by letterspaced display type ("T" / "' S" / "A LL" / "O PT"): a heading
    that does not end in terminal punctuation continues into the next heading
    of the same level."""
    out = []
    for line in md:
        if (
            out
            and line.startswith("#")
            and out[-1].startswith("#")
            and not re.search(r"[.!?]$", out[-1])
        ):
            lvl_new = len(line) - len(line.lstrip("#"))
            lvl_prev = len(out[-1]) - len(out[-1].lstrip("#"))
            if lvl_new == lvl_prev:
                out[-1] = out[-1].rstrip() + " " + line[lvl_new:].strip()
                continue
        out.append(line)
    return out


def merge_question_headings(md):
    """Join a question heading that wrapped across several #### entries back
    into one, in both directions: a #### that does not end in terminal
    punctuation continues into the next entry, and a #### that opens with a
    lowercase word is a tail of the previous one. Each merge stops at the
    question's closing punctuation, a non-question paragraph, or four lines."""
    merged = _merge_down(md)
    merged = _merge_up(merged)
    return merged


def _merge_down(md):
    out = []
    i = 0
    while i < len(md):
        line = md[i]
        if line.startswith("#### ") and not re.search(r"[.!?]$", line):
            acc = line
            j = i + 1
            while j < len(md) and j - i < 4:
                nxt = md[j]
                if not nxt:
                    j += 1
                    continue
                if not nxt.startswith("#### "):
                    # A body paragraph may carry the question's tail.
                    acc = acc.rstrip() + " " + nxt
                    j += 1
                    break
                acc = acc.rstrip() + " " + nxt[len("#### "):]
                j += 1
                if re.search(r"[.!?]$", acc):
                    break
            out.append(re.sub(r"\s+", " ", acc).strip())
            i = j
            continue
        out.append(line)
        i += 1
    return out


def _merge_up(md):
    out = []
    for line in md:
        if line.startswith("#### ") and line[len("#### "):][:1].islower():
            # The tail belongs to the last heading, skipping blank separators.
            k = len(out) - 1
            while k >= 0 and not out[k]:
                k -= 1
            if k >= 0 and out[k].startswith("#### "):
                out[k] = re.sub(r"\s+", " ", out[k].rstrip() + " " + line[len("#### "):]).strip()
                continue
        out.append(line)
    return out


def title_from_filename(path, sage=False):
    """The book's display name, written as the document's H1. Grimoire reads it
    back as the citation source, so it is the name a reader sees under an
    answer. Book file names are already properly cased ("Xanathar's Guide to
    Everything"); title-casing them only produced "Xanathar'S Guide To
    Everything", so the stem is kept as-is apart from separator cleanup."""
    if sage:
        # The prompt tells the model to treat these excerpts as official
        # precedent by name, so the source has to read as the real document
        # rather than as whatever the PDF happened to be called.
        return "Sage Advice Compendium"
    name = path.stem.replace("_", " ").strip()
    if name.islower():  # a genuinely lowercase file name still deserves a cap
        name = name.title()
    return name


def slugify(name):
    s = re.sub(r"[^a-z0-9]+", "-", name.lower()).strip("-")
    return s or "book"


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("inputs", nargs="+", type=Path, help="PDF files or directories")
    ap.add_argument("--out", required=True, type=Path, help="output directory for markdown")
    ap.add_argument("--sage", action="store_true", help="treat every input as a Sage-Advice-style Q&A document")
    args = ap.parse_args()

    args.out.mkdir(parents=True, exist_ok=True)
    pdfs = []
    for i in args.inputs:
        if i.is_dir():
            pdfs.extend(sorted(i.glob("*.pdf")))
        elif i.suffix.lower() == ".pdf":
            pdfs.append(i)
    if not pdfs:
        sys.exit("no PDFs found in the given inputs")

    print(f"extracting {len(pdfs)} PDFs into {args.out} ...")
    ok = 0
    for p in pdfs:
        try:
            if extract_pdf(p, args.out, force_sage=args.sage):
                ok += 1
        except Exception as e:  # keep going; one bad book must not sink the rest
            print(f"  FAIL {p.name}: {e}")
    print(f"done: {ok}/{len(pdfs)} extracted")
    print(f"index them with: DND_DOCS_DIR={args.out} (see README — Local books)")


if __name__ == "__main__":
    main()
