// Grimoire's two icon systems, and the one rule for choosing between them.
//
// Shikashi's sprites are 32px pixel art. Pixel art cannot be downscaled — half
// a pixel is mud — so a sprite is only ever drawn at 1x, 2x or 3x, and the
// layout gives it that room. Anything that must be small, or must take the
// corpus accent colour, is a game-icons.net vector instead: it inherits
// `currentColor` and stays crisp at any size.
//
// So: `sprite()` for the things the app is *about* — books, scrolls, orbs,
// potions. `gi()` for the affordances around them — close, send, delete.

import { GAME_ICONS } from "./gameicons.js";

const SVG_NS = "http://www.w3.org/2000/svg";
const COLS = 16;

/**
 * Named cells on the 16-column, 32px sprite sheet.
 *
 * The pack documents its icons as one ordered list, and each category starts
 * on a fresh row — so an index is `row*16 + column` and stays stable as long
 * as the sheet does. `scripts/build-assets.py` copies that sheet verbatim and
 * fails the build if its width ever stops being 16 columns.
 */
export const ICONS = Object.freeze({
	// the tome itself
	spellbook: 71,        // open book, glowing — the wordmark and the welcome
	openBook: 216,
	bookBlue: 208,
	bookRed: 209,
	bookGreen: 210,
	bookGold: 211,

	// what the sage consults
	scroll: 218,          // tied scroll — a cited rule
	scrollOpen: 219,      // unfurled — a rule opened in the drawer
	letter: 217,
	map: 220,
	runestone: 176,

	// Magic and D&D
	orbRed: 288,
	orbBlue: 289,         // mana blue — the Magic corpus
	orbGreen: 290,
	orbGold: 291,
	orbPurple: 292,
	orbDark: 293,
	card: 222,            // playing card — a card lookup
	dice: 221,
	swords: 89,           // crossed swords — the D&D corpus
	staff: 103,           // wizard staff — the sage
	shield: 97,

	// the chamber
	candle: 171,
	lantern: 169,
	torch: 170,
	campfire: 66,
	hourglass: 175,
	mirror: 177,          // scrying glass — the interaction resolver
	magnifier: 168,
	key: 185,
	chest: 187,
	cauldron: 313,
	cauldronLit: 312,

	// spellwork — the thinking state cycles casting into the scrolls
	casting: 336,         // a hand casting magic
	magicScroll1: 337,
	magicScroll2: 338,
	magicScroll3: 339,
	magicScroll4: 340,
	magicScroll5: 341,
	magicScroll6: 342,

	// potions, for the corpus picker's resting state
	potionRed: 144,
	potionBlue: 145,
	potionGreen: 146,
	potionGold: 147,

	sun: 344,
	moon: 346,
});

/** The magic-scroll cycle the thinking indicator runs through. */
export const CASTING_CYCLE = Object.freeze([
	ICONS.casting, ICONS.magicScroll1, ICONS.magicScroll2, ICONS.magicScroll3,
	ICONS.magicScroll4, ICONS.magicScroll5, ICONS.magicScroll6,
]);

/**
 * A pixel sprite. `scale` must be a whole number — 1, 2 or 3 — because
 * anything else resamples the art.
 *
 * Decorative by default: the sprite is `aria-hidden` unless given a `label`,
 * since it almost always sits beside the text it illustrates.
 */
export function sprite(name, { scale = 1, label = "", cls = "" } = {}) {
	const index = ICONS[name];
	if (index == null) throw new Error(`unknown sprite: ${name}`);
	if (!Number.isInteger(scale)) throw new Error(`sprite scale must be whole: ${scale}`);

	const node = document.createElement("i");
	node.className = "ico" + (cls ? " " + cls : "");
	node.style.setProperty("--ix", index % COLS);
	node.style.setProperty("--iy", Math.floor(index / COLS));
	if (scale !== 1) node.style.setProperty("--s", scale);
	if (label) node.setAttribute("aria-label", label);
	else node.setAttribute("aria-hidden", "true");
	return node;
}

/** Point an existing sprite element at a different cell, for animation. */
export function setSprite(node, index) {
	node.style.setProperty("--ix", index % COLS);
	node.style.setProperty("--iy", Math.floor(index / COLS));
}

/**
 * A vector icon, built from path data rather than markup so there is nothing
 * to inject and `fill: currentColor` just works.
 */
export function gi(name, { label = "", cls = "" } = {}) {
	const paths = GAME_ICONS[name];
	if (!paths) throw new Error(`unknown vector icon: ${name}`);

	const svg = document.createElementNS(SVG_NS, "svg");
	svg.setAttribute("class", "gi" + (cls ? " " + cls : ""));
	svg.setAttribute("viewBox", "0 0 512 512");
	if (label) {
		svg.setAttribute("role", "img");
		svg.setAttribute("aria-label", label);
	} else {
		svg.setAttribute("aria-hidden", "true");
	}
	for (const d of paths) {
		const path = document.createElementNS(SVG_NS, "path");
		path.setAttribute("d", d);
		svg.append(path);
	}
	return svg;
}

/**
 * Replace a placeholder's contents with an icon. Templates mark their icon
 * slots with `data-ico="name"` or `data-gi="name"` so the HTML stays readable
 * and the wiring stays in one place.
 */
export function hydrate(root = document) {
	for (const node of root.querySelectorAll("[data-ico]")) {
		const scale = Number(node.dataset.icoScale || 1);
		node.replaceChildren(sprite(node.dataset.ico, { scale }));
	}
	for (const node of root.querySelectorAll("[data-gi]")) {
		node.replaceChildren(gi(node.dataset.gi));
	}
}

/** The mark that stands for a corpus, wherever one needs identifying. */
export function corpusMark(corpus, opts) {
	return sprite(corpus === "dnd" ? "swords" : "orbBlue", opts);
}
