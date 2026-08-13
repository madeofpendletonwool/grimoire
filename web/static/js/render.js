// Rendering for the content surfaces: answers, rule cards, and card views.

import { el, truncate } from "./dom.js";
import { renderMarkdown, highlight } from "./markdown.js";
import { refs } from "./refs.js";
import { manaNodes, manaInEscaped, setSymbol } from "./mana.js";

/**
 * Render an answer body. `corpus` decides two things: whether rule numbers in
 * the prose become clickable references, and whether {U}-style notation
 * becomes mana pips. Both are Magic's; the D&D SRD numbers nothing and casts
 * nothing in curly braces.
 */
export function renderAnswer(container, text, corpus) {
	const mtg = corpus === "mtg";
	container.innerHTML = renderMarkdown(text, { rules: mtg, mana: mtg });
}

/** Wire rule-reference buttons produced by the markdown pass. */
export function bindRuleRefs(root, corpus) {
	root.querySelectorAll(".rule-ref").forEach((btn) => {
		if (btn.dataset.bound) return;
		btn.dataset.bound = "1";
		btn.addEventListener("click", () => refs.openRule({ number: btn.dataset.rule }, corpus));
	});
}

/** Citation strip under an answer: rules consulted, cards/entities looked up, misses. */
export function renderCitations(sources, cards, entities, unresolved, corpus) {
	const hasAny = (sources && sources.length) || (cards && cards.length) || (entities && entities.length) || (unresolved && unresolved.length);
	if (!hasAny) return null;

	const wrap = el("div", { class: "citations" });

	if (sources && sources.length) {
		wrap.append(el("span", { class: "citations-label", text: "Rules:" }));
		for (const s of sources.slice(0, 10)) {
			wrap.append(el("button", {
				class: "chip",
				text: s.number || s.title || "•",
				attrs: { type: "button", title: (s.title ? s.title + " — " : "") + truncate(s.body, 160) },
				on: { click: () => refs.openRule(s, corpus) },
			}));
		}
	}

	if (cards && cards.length) {
		wrap.append(el("span", { class: "citations-label", text: "Cards:" }));
		for (const c of cards.slice(0, 10)) {
			wrap.append(el("button", {
				class: "chip chip-card",
				text: c.name,
				attrs: { type: "button", title: (c.type_line ? c.type_line + " — " : "") + truncate(c.oracle_text, 160) },
				on: { click: () => refs.openCard(c) },
			}));
		}
	}

	if (entities && entities.length) {
		wrap.append(el("span", { class: "citations-label", text: "References:" }));
		for (const e of entities.slice(0, 10)) {
			wrap.append(el("button", {
				class: "chip chip-entity",
				text: e.name,
				attrs: { type: "button", title: (e.kind ? e.kind + " — " : "") + truncate(e.body, 160) },
				on: { click: () => refs.openEntity(e, corpus) },
			}));
		}
	}

	if (unresolved && unresolved.length) {
		wrap.append(el("span", { class: "citations-label", text: "Not found:" }));
		for (const name of unresolved.slice(0, 6)) {
			wrap.append(el("span", {
				class: "chip chip-miss",
				text: name,
				attrs: { title: "This entity could not be looked up, so the sage was told not to describe it." },
			}));
		}
	}
	return wrap;
}

/**
 * Official rulings under an answer: each ruling's comment text attributed by
 * the card it applies to, its source (wotc/scryfall), and its publish date.
 * Returns null when there are no rulings so callers can skip appending.
 */
export function renderRulings(rulings) {
	if (!rulings || !rulings.length) return null;

	const wrap = el("div", { class: "rulings" });
	wrap.append(el("div", { class: "rulings-label", text: "Official rulings" }));
	for (const r of rulings.slice(0, 20)) {
		const source = sourceLabel(r.source);
		const entry = el("div", { class: "ruling" });
		entry.append(el("p", { class: "ruling-comment", text: r.comment || "" }));
		const attr = el("span", { class: "ruling-attr" });
		if (r.card_name) attr.append(el("span", { class: "ruling-card", text: r.card_name }));
		if (source) {
			if (attr.childNodes.length) attr.append(el("span", { class: "ruling-sep", text: "·" }));
			attr.append(el("span", { class: "ruling-source", text: source }));
		}
		if (r.published_at) {
			if (attr.childNodes.length) attr.append(el("span", { class: "ruling-sep", text: "·" }));
			attr.append(el("span", { class: "ruling-date", text: r.published_at }));
		}
		entry.append(attr);
		wrap.append(entry);
	}
	return wrap;
}

/** Map Scryfall ruling sources to a readable label. */
function sourceLabel(source) {
	switch ((source || "").toLowerCase()) {
		case "wotc": return "WotC";
		case "scryfall": return "Scryfall";
		default: return source || "";
	}
}

/**
 * One rule entry. `sub` styles it as a nested sub-rule within a section.
 * Magic's rules quote mana in curly notation constantly ("{T}: Add {G}"), so
 * on that corpus the symbols are drawn rather than spelled.
 */
export function ruleCard(rule, query, sub, corpus) {
	const node = el("div", { class: "rule-card" + (sub ? " is-sub" : "") });
	const head = el("div");
	if (rule.number) head.append(el("span", { class: "rule-num", text: rule.number }));
	if (rule.title) head.append(el("span", { class: "rule-name", text: rule.title }));
	if (head.childNodes.length) node.append(head);

	let body = highlight(rule.body || "", query);
	if (corpus === "mtg") body = manaInEscaped(body);
	node.append(el("div", { class: "rule-text", html: body }));
	return node;
}

/** Full card view: image, cost, type line, oracle text (or each face). */
export function cardView(card) {
	const node = el("div", { class: "card-view" });

	if (card.image_url) {
		node.append(el("img", {
			attrs: { src: card.image_url, alt: card.name, loading: "lazy" },
		}));
	}

	const head = el("div");
	if (card.mana_cost) {
		head.append(el("span", { class: "c-cost" }, manaNodes(card.mana_cost)));
	}
	head.append(el("span", { class: "c-name", text: card.name }));
	node.append(head);

	const type = el("div", { class: "c-type", text: card.type_line || "" });
	if (card.power && card.toughness) {
		type.append(el("span", { class: "c-pt", text: `${card.power}/${card.toughness}` }));
	} else if (card.loyalty) {
		type.append(el("span", { class: "c-pt", text: card.loyalty }));
	}
	node.append(type);

	if (card.faces && card.faces.length) {
		for (const f of card.faces) {
			const face = el("div", { class: "c-face" });
			const faceHead = el("div", { class: "c-name", text: f.name });
			if (f.mana_cost) {
				faceHead.prepend(el("span", { class: "c-cost" }, manaNodes(f.mana_cost)));
			}
			face.append(faceHead);
			if (f.type_line) face.append(el("div", { class: "c-type", text: f.type_line }));
			face.append(el("div", { class: "c-oracle" }, manaNodes(f.oracle_text || "")));
			node.append(face);
		}
	} else {
		node.append(el("div", { class: "c-oracle" },
			manaNodes(card.oracle_text || "(no oracle text)")));
	}

	// The set's own expansion symbol, with its code as the fallback for the
	// odd promo set Keyrune has no glyph for.
	const foot = el("div", { class: "c-foot" });
	const set = el("span");
	if (card.set) {
		const mark = setSymbol(card.set);
		if (mark) set.append(mark);
		set.append(el("span", { class: "c-set", text: card.set.toUpperCase() }));
	}
	foot.append(set);
	if (card.scryfall_uri) {
		foot.append(el("a", {
			text: "Scryfall ↗",
			attrs: { href: card.scryfall_uri, target: "_blank", rel: "noopener" },
		}));
	}
	node.append(foot);
	return node;
}
