// Rendering for the content surfaces: answers, rule cards, and card views.

import { el, truncate } from "./dom.js";
import { renderMarkdown, highlight } from "./markdown.js";
import { refs } from "./refs.js";

/**
 * Render an answer body. `corpus` decides whether rule numbers in the prose
 * become clickable references (MTG numbers them; the D&D SRD does not).
 */
export function renderAnswer(container, text, corpus) {
	container.innerHTML = renderMarkdown(text, { rules: corpus === "mtg" });
}

/** Wire rule-reference buttons produced by the markdown pass. */
export function bindRuleRefs(root, corpus) {
	root.querySelectorAll(".rule-ref").forEach((btn) => {
		if (btn.dataset.bound) return;
		btn.dataset.bound = "1";
		btn.addEventListener("click", () => refs.openRule({ number: btn.dataset.rule }, corpus));
	});
}

/** Citation strip under an answer: rules consulted, cards looked up, misses. */
export function renderCitations(sources, cards, unresolved, corpus) {
	const hasAny = (sources && sources.length) || (cards && cards.length) || (unresolved && unresolved.length);
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

	if (unresolved && unresolved.length) {
		wrap.append(el("span", { class: "citations-label", text: "Not found:" }));
		for (const name of unresolved.slice(0, 6)) {
			wrap.append(el("span", {
				class: "chip chip-miss",
				text: name,
				attrs: { title: "This card could not be looked up, so the sage was told not to describe it." },
			}));
		}
	}
	return wrap;
}

/** One rule entry. `sub` styles it as a nested sub-rule within a section. */
export function ruleCard(rule, query, sub) {
	const node = el("div", { class: "rule-card" + (sub ? " is-sub" : "") });
	const head = el("div");
	if (rule.number) head.append(el("span", { class: "rule-num", text: rule.number }));
	if (rule.title) head.append(el("span", { class: "rule-name", text: rule.title }));
	if (head.childNodes.length) node.append(head);
	node.append(el("div", { class: "rule-text", html: highlight(rule.body || "", query) }));
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
	if (card.mana_cost) head.append(el("span", { class: "c-cost", text: card.mana_cost }));
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
			face.append(el("div", { class: "c-name", text: f.name }));
			if (f.type_line) face.append(el("div", { class: "c-type", text: f.type_line }));
			face.append(el("div", { class: "c-oracle", text: f.oracle_text || "" }));
			node.append(face);
		}
	} else {
		node.append(el("div", { class: "c-oracle", text: card.oracle_text || "(no oracle text)" }));
	}

	const foot = el("div", { class: "c-foot" });
	foot.append(el("span", { text: card.set ? card.set.toUpperCase() : "" }));
	if (card.scryfall_uri) {
		foot.append(el("a", {
			text: "Scryfall ↗",
			attrs: { href: card.scryfall_uri, target: "_blank", rel: "noopener" },
		}));
	}
	node.append(foot);
	return node;
}
