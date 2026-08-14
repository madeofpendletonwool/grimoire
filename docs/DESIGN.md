# Grimoire's design system

How the interface is built, and what you have to preserve when you add to it.

Grimoire's UI is made of real pixel art — nine-sliced frames, a 32px sprite
sheet, four parallax backdrops — assembled with plain CSS and ES modules. There
is no build step and no framework. That makes it easy to change and easy to
break: most of the rules below exist because the art only stays crisp and the
text only stays readable under specific conditions.

**If you read nothing else, read [Invariants](#invariants).**

---

## The idea

> Candlelit stone for the chrome; parchment for the content. Pixel art carries
> the room, the frames and the icons. The words stay at full resolution.

The app is a reading tool. Someone is working through a 400-word rules answer,
so the prose is set in a real serif at real resolution — a pixel font would tax
the one thing the app exists to do. Everything *around* the prose is pixel art.

Two consequences worth internalising:

- **The tome never changes.** Parchment, ink, gold and the corpus accent are
  identical in every theme and every state. The room around the book changes;
  the book does not. This is why themes are safe.
- **Give the art room rather than shrinking it.** 32px pixel art cannot be
  scaled to 16px without turning to mush. When a sprite does not fit, the
  layout grows or the icon becomes a vector — the sprite is never squeezed.

---

## Where things live

| File | Owns | Edit? |
|---|---|---|
| `web/static/style.css` | Tokens, layout, typography, component internals | yes |
| `web/static/pixel.css` | Sprites, **which surface is framed in which sprite**, the scene | yes |
| `web/static/themes.css` | Per-theme stone ramps + frame URLs | **generated** |
| `web/static/fonts.css` | `@font-face` for the five families | **generated** |
| `web/static/mana.css`, `keyrune.css` | Upstream symbol classes | **vendored** |
| `web/static/js/icons.js` | Both icon systems; `sprite()`, `gi()`, `hydrate()` | yes |
| `web/static/js/gameicons.js` | SVG path data | **generated** |
| `web/static/js/scene.js` | Backdrop, parallax, theme switch, settings popup | yes |
| `web/static/js/mana.js` | `{U}`-notation → Mana font pips | yes |
| `web/static/js/render.js` | Answers, rule cards, card views | yes |

**style.css vs pixel.css.** style.css says what a component *is* — its layout,
type and spacing. pixel.css says what it is *framed in*. They are split because
a framed surface must not carry its own background or radius, and keeping that
knowledge in one file makes the rule enforceable by reading rather than by
memory.

Generated files carry a `do not edit` header. Regenerate with the scripts in
[Regenerating assets](#regenerating-assets); editing them by hand means the
next build silently reverts you.

---

## Invariants

Break these and the design degrades in ways that are easy to miss in review.

**1. Pixel art scales by whole numbers only.**
Sprites at `--s: 1 | 2 | 3`. Frames where `border-width` is an integer multiple
of `border-image-slice`. A fractional scale resamples the pixels and the whole
thing goes soft. Current pairs, all clean:

| Frame | slice | border-width | scale |
|---|---|---|---|
| parchment | `5 fill` | `10px` | 2× |
| stone / field / button | `3 fill` | `6px` | 2× |
| chip / rule-ref | `3 fill` | `3px` | 1× |
| select | `6` | `12px` | 2× |
| page | `6 2 8 2 fill` | `12px 4px 16px 4px` | 2× |
| cover | `28 fill` | `28px` | 1× (sprite is pre-doubled 2×) |
| bar | `1 3 1 3 fill` | `2px 6px` | 2× |

**2. A framed surface carries no `background-color` and no `border-radius`.**
The sprite supplies both. A stray radius clips the sprite's own corners off.
Every framed element is listed in the shared reset at the top of pixel.css's
nine-slice section — add yours there too.

**3. The parchment tokens *are* the sprite's pixels.**
`--parchment: #ffd7a8` is the literal colour inside `panel-parchment.png`. A
nine-sliced surface and a plain CSS one sit side by side constantly; the moment
these diverge there is a visible seam. Change one, change the other.

**4. Themes change hue, never the lightness ladder.**
`STONE_L` in `build-assets.py` is fixed. A theme borrows a scene's hue and
applies it to those exact lightness steps, so contrast between any two steps is
identical in every theme. Never hand-write a theme colour.

**5. Parchment, ink, gold and the corpus accent are theme-independent.**
Blue-for-Magic and red-for-D&D carry meaning; a backdrop must not restate it.

**6. Nothing loads from a third party.**
The binary has to render on a machine with no route to the internet. Fonts,
icons and art are all under `/static/`. No CDN, no Google Fonts link.

**7. Every animation must survive `prefers-reduced-motion`.**
A global rule in style.css neutralises all of them, and `scene.js` skips the
parallax listener entirely. If you add motion that is not a CSS animation, gate
it yourself.

**8. Sprites are decorative; the control carries the name.**
`sprite()` and `gi()` emit `aria-hidden="true"` unless given a `label`. The
button around them keeps its `aria-label`. Never let an icon be the only
accessible name.

**9. Text contrast has a floor.**
`--ink` on `--parchment` is ~10:1, `--ink-soft` ~5.5:1. The scene veil exists
to guarantee a floor over *any* backdrop, including the bright Plains. If you
lighten the veil, re-check the welcome copy over the brightest theme.

---

## Choosing an icon

Two systems, one rule:

> **Sprite when there is room. Vector when it must be small or must take the
> accent colour.**

```js
import { sprite, gi, ICONS } from "./icons.js";

sprite("spellbook")                    // 32px pixel art
sprite("spellbook", { scale: 3 })      // 96px, for the welcome mark
gi("search")                           // vector, sizes in em, inherits color
gi("delete", { cls: "gi-lg" })
```

- **`sprite(name)`** — Shikashi's 32px sheet. Named cells are in `ICONS` in
  icons.js. Use for the nouns the app is *about*: spellbook, scroll, orb,
  staff, candle, potion. Minimum 32px. Full-colour, so it does not tint.
- **`gi(name)`** — game-icons.net vectors. Use for affordances: close, send,
  menu, delete, search. Monochrome, inherits `currentColor`, crisp at any size.

Templates mark slots declaratively and `hydrate()` fills them at boot:

```html
<span data-ico="spellbook" data-ico-scale="3"></span>
<button data-gi="close" aria-label="Close reference"></button>
```

Adding a sprite: find its cell index (`row * 16 + column` on the sheet) and add
a named entry to `ICONS`. Adding a vector: add it to the `ICONS` map in
`scripts/fetch-gameicons.py`, re-run, and record it in ATTRIBUTIONS.md — the
icons are CC BY 3.0 and attribution is per-icon.

---

## Adding a new surface

Worked example — a framed panel on stone:

1. **Layout and type in style.css.** No background, no border, no radius.
   ```css
   .my-panel {
       padding: var(--sp-3);
       font-family: var(--font-prose);
       color: var(--parchment);
   }
   ```
2. **Frame it in pixel.css.** Add the selector to the shared reset list *and*
   to the frame rule it adopts:
   ```css
   .nine, …, .my-panel { border-style: solid; border-radius: 0; … }

   .f-stone, .palette-box, .settings-popup, .my-panel {
       border-width: 6px;
       border-image-source: var(--frame-stone);
       border-image-slice: 3 fill;
   }
   ```
   Use `var(--frame-*)`, never a direct file path — that is what makes it
   follow the theme.
3. **Check it against a theme and against bare stone**, not just the default.

**Which frame?** `--frame-stone` for chrome panels · `panel-parchment` for
content the user reads · `--frame-field` for text input · `button` for primary
actions · `chip` for inline pills · `page` for the drawer · `cover` for the
gate.

### Typography

Four voices, all self-hosted:

| Token | Font | For |
|---|---|---|
| `--font-prose` | EB Garamond | Answers, rules, card text |
| `--font-display` | Cinzel | Wordmark, headings, buttons — uppercase, tracked |
| `--font-pixel` | Departure Mono | Keycaps, section labels, badges, rule numbers |
| `--font-ui` | system sans | Chrome text |

Pixel font for **labels only**, never for prose.

### Magic symbols

Rule text and card text contain `{T}`, `{2}{U}{U}`, `{W/P}`. Render them:

```js
import { manaNodes, manaInEscaped } from "./mana.js";

node.append(manaNodes(card.oracle_text));   // raw text → nodes (safe)
html = manaInEscaped(alreadyEscapedHtml);   // inside the Markdown pass
```

**Always gate on `corpus === "mtg"`.** The D&D SRD has no such notation.

---

## Themes

Four, each derived from its own backdrop art. Picking one in Settings sets
`data-scene` on `<html>`; themes.css swaps the stone ramp and the frame sprites.

| Theme | Hue | Chrome |
|---|---|---|
| `cave` | 242° | Violet-black |
| `deadforest` | 34° | Warm brown (matches the original palette) |
| `snowy` | 222° | Blue-grey |
| `plains` | 219° | Deep blue |
| `none` | — | Falls back to `:root`, no backdrop |

**Adding a theme:** drop a layered parallax pack into `icon-packs/`, add it to
`SCENES` in `build-assets.py` *and* in `scene.js`, and re-run the build. Hue,
stone ramp, frames and sky colour are all derived — you write no colours.

### Two things about the backdrop art

- **Layers are numbered front-to-back.** `0.png` is the foreground; the highest
  index is the sky. `scene.js` appends them in reverse so index 0 paints on
  top. Stacking them the other way buries every scene under its own sky.
- **The scene is `position: fixed; z-index: 0`.** Content must live inside a
  positioned wrapper — `.app` and `.gate` both set `position: relative;
  z-index: 1`. Static content added outside them will render *behind* the
  backdrop.

---

## Regenerating assets

Shipped assets are committed; a normal build needs none of this. All three
scripts are pure-stdlib Python 3.

```bash
python3 scripts/build-assets.py     # sprites, frames, scenes, themes.css, favicons
python3 scripts/fetch-gameicons.py  # js/gameicons.js
python3 scripts/fetch-fonts.py      # fonts/ + fonts.css
```

`build-assets.py` reads from `icon-packs/`, which is **gitignored on purpose**:
one upstream licence permits adapting the material but not republishing the
pack, so the repo carries only derived output. Get the packs from the links
in [ATTRIBUTIONS.md](https://github.com/madeofpendletonwool/grimoire/blob/main/ATTRIBUTIONS.md) first.

Its recolour maps are **exhaustive** — an unmapped colour aborts the build
rather than shipping a half-recoloured sprite. If it fails after an art update,
that is the check working; add the new colour to the map.

---

## Verifying a change

The front end is embedded in the binary, so **every CSS/JS edit needs a
rebuild** — there is no watcher.

```bash
go build ./... && go test ./...
```

Then look at it. Things worth checking that automated tests will not catch:

- At least one theme *and* bare stone, not just the default.
- Both corpora — the accent flips blue↔red.
- Narrow (≤560px): the topbar drops labels; nothing may overflow. Measure
  `document.documentElement.scrollWidth` against `innerWidth` rather than
  trusting a screenshot — a headless window can clip and look like overflow.
- Zoom to 200%: no blurred sprites, no resampled corners.
- `prefers-reduced-motion` on: everything stops, layout unchanged.
