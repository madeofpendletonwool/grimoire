// Magic's symbols, rendered as symbols.
//
// Scryfall's oracle text and the Comprehensive Rules both write mana in curly
// notation — {2}{U}{U}, {T}, {W/P}. Printed literally that is noise; a reader
// checking a rule about "{T}: add {G}" has to translate it in their head. The
// Mana font turns each one into the pip it actually is.
//
// This is Magic-only. The D&D SRD has no such notation, and a stray {X} in
// rules prose should stay a stray {X}, so every caller gates on the corpus.

import { esc } from "./dom.js";

// A symbol is at most a few characters of the alphabet Scryfall uses. The
// pattern is deliberately tight: whatever it captures becomes part of a class
// name, so it must not be able to carry anything else.
const SYMBOL = /\{([0-9A-Za-z]{1,2}(?:\/[0-9A-Za-z])?|½|∞)\}/g;

// Symbols whose Mana class name is not simply the lowercased contents.
const ALIAS = Object.freeze({
	t: "tap",
	q: "untap",
	pw: "planeswalker",
	"½": "half",
	"∞": "infinity",
});

/**
 * The Mana class for one symbol's inner text, or null if we do not know it.
 * Unknown symbols are left as written rather than rendered as a blank pip.
 */
function manaClass(inner) {
	const key = inner.toLowerCase();
	if (ALIAS[key]) return "ms-" + ALIAS[key];
	// Hybrid and Phyrexian drop the slash: {W/U} is ms-wu, {2/W} is ms-2w.
	const flat = key.replace("/", "");
	if (!/^[a-z0-9]{1,3}$/.test(flat)) return null;
	return "ms-" + flat;
}

/**
 * Replace mana notation in a string that has *already been HTML-escaped*.
 * Returns an HTML string. Used by the Markdown renderer, which escapes first
 * and adds markup after.
 */
export function manaInEscaped(html) {
	return String(html == null ? "" : html).replace(SYMBOL, (whole, inner) => {
		const cls = manaClass(inner);
		if (!cls) return whole;
		return `<i class="ms ${cls} ms-cost ms-shadow" role="img" aria-label="${esc(inner)}"></i>`;
	});
}

/**
 * Turn raw, unescaped text into nodes: text runs as text, symbols as pips.
 * Used wherever we would otherwise have set textContent — nothing is parsed
 * as HTML, so card text from Scryfall can never become markup.
 */
export function manaNodes(text) {
	const frag = document.createDocumentFragment();
	const src = String(text == null ? "" : text);
	let last = 0;

	for (const m of src.matchAll(SYMBOL)) {
		const cls = manaClass(m[1]);
		if (!cls) continue;
		if (m.index > last) frag.append(src.slice(last, m.index));
		const pip = document.createElement("i");
		pip.className = `ms ${cls} ms-cost ms-shadow`;
		pip.setAttribute("role", "img");
		pip.setAttribute("aria-label", m[1]);
		frag.append(pip);
		last = m.index + m[0].length;
	}
	if (last < src.length) frag.append(src.slice(last));
	return frag;
}

/** True when the text contains at least one symbol we can render. */
export function hasMana(text) {
	SYMBOL.lastIndex = 0;
	for (const m of String(text || "").matchAll(SYMBOL)) {
		if (manaClass(m[1])) return true;
	}
	return false;
}

/**
 * The set's own expansion symbol, via Keyrune. Falls back to the set code in
 * text when the font has no glyph for it — Keyrune covers real sets, not the
 * odd promo code Scryfall sometimes returns.
 */
export function setSymbol(code) {
	const slug = String(code || "").toLowerCase();
	if (!/^[a-z0-9]{2,6}$/.test(slug)) return null;
	const node = document.createElement("i");
	node.className = `ss ss-${slug}`;
	node.setAttribute("role", "img");
	node.setAttribute("aria-label", slug.toUpperCase());
	return node;
}
