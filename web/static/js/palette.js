// Command palette (Ctrl/⌘+K): one search box over both rules and cards.
//
// This replaces the old "Search the Rules" and "Card Lookup" pages. Results
// open in the reference drawer, so consulting a rule never takes you out of
// the conversation.

import { $, clear, el, debounce, truncate } from "./dom.js";
import { api } from "./api.js";
import { activeCorpus, corpusLabel, supportsCards } from "./state.js";
import { openRule, openCard } from "./drawer.js";
import { gi } from "./icons.js";

let items = [];        // flat list of {kind, label, text, payload}
let activeIndex = -1;
let inflight = null;
let lastFocus = null;

export function initPalette() {
	const input = $("palette-input");

	$("palette").addEventListener("click", (e) => {
		if (e.target.closest("[data-palette-close]")) closePalette();
	});
	input.addEventListener("input", () => runSearch(input.value.trim()));
	input.addEventListener("keydown", onKeydown);

	document.addEventListener("keydown", (e) => {
		if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
			e.preventDefault();
			$("palette").hidden ? openPalette() : closePalette();
		} else if (e.key === "Escape" && !$("palette").hidden) {
			closePalette();
		}
	});

	// The shortcut hint should match the platform it is shown on.
	if (navigator.platform && /mac/i.test(navigator.platform)) {
		$("palette-kbd").textContent = "⌘ K";
	}
}

export function openPalette(prefill = "") {
	lastFocus = document.activeElement;
	$("palette").hidden = false;
	$("palette-scope").textContent = corpusLabel(activeCorpus());
	const input = $("palette-input");
	input.value = prefill;
	input.focus();
	input.select();
	if (prefill) runSearch(prefill);
	else showMessage("Type to search rules" + (supportsCards(activeCorpus()) ? " and cards" : "") + "…");
}

export function closePalette() {
	$("palette").hidden = true;
	if (inflight) {
		inflight.abort();
		inflight = null;
	}
	items = [];
	activeIndex = -1;
	if (lastFocus && lastFocus.isConnected) lastFocus.focus();
	lastFocus = null;
}

const runSearch = debounce(async (q) => {
	if (!q) {
		showMessage("Type to search rules" + (supportsCards(activeCorpus()) ? " and cards" : "") + "…");
		return;
	}
	if (inflight) inflight.abort();
	const controller = new AbortController();
	inflight = controller;

	const corpus = activeCorpus();
	const wantCards = supportsCards(corpus);

	// Rules and cards are independent lookups; a Scryfall hiccup should not
	// take the rule results down with it.
	const [rules, cards] = await Promise.all([
		api.search(corpus, q, 12, controller.signal).then((d) => d.results || []).catch(() => []),
		wantCards
			? api.cardSearch(q, 6, controller.signal).then((d) => d.matches || []).catch(() => [])
			: Promise.resolve([]),
	]);
	if (controller.signal.aborted) return;
	inflight = null;

	items = [
		...cards.map((c) => ({ kind: "card", label: c.mana_cost || "card", text: c.name, sub: c.type_line, payload: c })),
		...rules.map((r) => ({ kind: "rule", label: r.number || "rule", text: r.title || truncate(r.body, 90), payload: r })),
	];
	activeIndex = items.length ? 0 : -1;
	renderResults(cards.length, rules.length);
}, 180);

function renderResults(cardCount, ruleCount) {
	const list = clear($("palette-results"));
	if (items.length === 0) {
		showMessage("Nothing found. Try different words, or a rule number like 205.1a.");
		return;
	}

	let i = 0;
	if (cardCount > 0) {
		list.append(el("div", { class: "palette-group", text: "Cards" }));
		for (; i < cardCount; i++) list.append(itemNode(items[i], i));
	}
	if (ruleCount > 0) {
		list.append(el("div", { class: "palette-group", text: "Rules" }));
		for (; i < items.length; i++) list.append(itemNode(items[i], i));
	}
	updateActive();
}

function itemNode(item, index) {
	return el("button", {
		class: "palette-item",
		attrs: { type: "button", role: "option", "data-index": index },
		on: { click: () => choose(index) },
	},
		el("span", { class: "p-key", text: item.label }),
		el("span", { class: "p-text", text: item.sub ? `${item.text} — ${item.sub}` : item.text }),
	);
}

function showMessage(text) {
	items = [];
	activeIndex = -1;
	clear($("palette-results")).append(el("div", { class: "palette-empty" },
		gi("no-results", { cls: "gi-xl" }),
		el("span", { text }),
	));
}

function onKeydown(e) {
	if (items.length === 0) return;
	if (e.key === "ArrowDown") {
		e.preventDefault();
		activeIndex = (activeIndex + 1) % items.length;
		updateActive();
	} else if (e.key === "ArrowUp") {
		e.preventDefault();
		activeIndex = (activeIndex - 1 + items.length) % items.length;
		updateActive();
	} else if (e.key === "Enter" && activeIndex >= 0) {
		e.preventDefault();
		choose(activeIndex);
	}
}

function updateActive() {
	$("palette-results").querySelectorAll(".palette-item").forEach((node) => {
		const on = Number(node.dataset.index) === activeIndex;
		node.classList.toggle("is-active", on);
		if (on) node.scrollIntoView({ block: "nearest" });
	});
}

function choose(index) {
	const item = items[index];
	if (!item) return;
	closePalette();
	if (item.kind === "card") openCard(item.payload);
	else openRule(item.payload, activeCorpus());
}
