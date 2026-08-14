![The tome](../assets/sprites/bookGold.png){: style="float:right; margin-left:1rem" align=right }

# The Tome & Themes

The chrome is assembled from licensed game art rather than emoji and CSS gradients: a nine-sliced pixel UI kit carries the frames, a 32px sprite sheet the icons, layered parallax packs the scenes.

Answers, rules and cards sit on a nine-sliced parchment page; the reference drawer is the book's right-hand page, spine and all; the sign-in screen is the book shut, and opens into it.

<div class="grid cards" markdown>

-   ![Pixel frames](../assets/sprites/bookGold.png){: loading=lazy }

    **Nine-sliced frames**

    Every framed surface — parchment panels, stone chrome, buttons, chips, fields — is a sprite drawn at a whole-number scale. A framed surface carries no CSS background or radius; the sprite supplies both.

-   ![Sprites](../assets/sprites/spellbook.png){: loading=lazy }

    **Two icon systems**

    Pixel sprites for the nouns the app is *about* — spellbook, scroll, mana orb, wizard's staff, candle. Monochrome vectors for the small affordances — close, send, search — that must stay crisp at any size or take the accent color.

-   ![Scenes](../assets/sprites/moon.png){: loading=lazy }

    **A real parallax backdrop**

    The chamber behind the app is a layered parallax stack that drifts with the pointer — the sky holds still, the foreground travels.

-   ![Symbols](../assets/sprites/runestone.png){: loading=lazy }

    **Magic's own symbols**

    Mana, tap and set symbols render as symbols — `{T}: Add {G}` in card text, rule text and the sage's prose — via the Mana and Keyrune fonts.

</div>

## Words stay at full resolution

The app is a reading tool: someone is working through a 400-word rules answer, so the prose is set in a real serif (EB Garamond) at real resolution. The pixel font (Departure Mono) is for labels only — keycaps, section labels, badges, rule numbers — because a pixel font would tax the one thing the app exists to do.

## Four themes, measured from the art

Pick a chamber in Settings and the whole interface follows it: a violet cavern, a brown dead forest, blue-grey peaks, or open plains.

| Theme | Hue | Chrome |
| ----- | --- | ------ |
| `cave` | 242° | Violet-black |
| `deadforest` | 34° | Warm brown (matches the original palette) |
| `snowy` | 222° | Blue-grey |
| `plains` | 219° | Deep blue |
| `none` | — | Falls back to the default, no backdrop |

Each theme's color is *measured* from that scene's own pixels at build time and applied to the chrome's existing lightness ladder, so a retinted interface is exactly as legible as the default. **The parchment never changes: the room changes, the book does not.**

## No build step, no framework

No bundler, no JS framework — just ES modules and plain CSS, and every font self-hosted so it works offline. The binary embeds all front-end assets, so the runtime image is just the binary + CA certs.

## Under the hood

The whole system — tokens, frames, the icon rule, theme derivation, and the invariants that keep pixel art crisp and text readable — is written down in the [Design System](../DESIGN.md) doc. Credits and licences for the art and fonts are in [ATTRIBUTIONS.md](https://github.com/madeofpendletonwool/grimoire/blob/main/ATTRIBUTIONS.md) — the interface is built from other people's art (Crusenho, Shikashi, Admurin, game-icons.net, Andrew Gioia), and the app links the same list from its Settings popup.

!!! note "These docs wear the same chrome"

    The site you are reading is skinned with the app's own derived assets — the parchment page, the stone chrome, the gold, the sprites and the fonts — exported from the same committed art. The stylesheet lives in `docs/stylesheets/grimoire.css`.
