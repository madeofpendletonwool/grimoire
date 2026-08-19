// The reference drawer: rules and cards open here, beside the conversation,
// instead of on a separate page or behind a modal that hides the chat.

import { $, clear, el, isNarrow } from "./dom.js";
import { api } from "./api.js";
import { ruleCard, cardView } from "./render.js";
import { registerRefHandlers } from "./refs.js";
import { openReader } from "./reader.js";
import { activeCorpus } from "./state.js";

let lastFocus = null;

export function initDrawer() {
	$("drawer-close").addEventListener("click", closeDrawer);
	document.addEventListener("keydown", (e) => {
		if (e.key === "Escape" && !$("drawer").hidden && !isPaletteOpen()) closeDrawer();
	});
	registerRefHandlers({ openRule, openCard, openEntity });
}

const isPaletteOpen = () => !$("palette").hidden;

function showDrawer(title) {
	const drawer = $("drawer");
	if (drawer.hidden) lastFocus = document.activeElement;
	$("drawer-title").textContent = title;
	drawer.hidden = false;
	$("app").classList.add("drawer-open");
	return clear($("drawer-body"));
}

export function closeDrawer() {
	$("drawer").hidden = true;
	$("app").classList.remove("drawer-open");
	clear($("drawer-body"));
	if (lastFocus && lastFocus.isConnected) lastFocus.focus();
	lastFocus = null;
}

/**
 * Open a rule. A numbered MTG rule expands to its whole mechanic section
 * (parent plus every sub-rule) so the reader sees how the rule actually works,
 * not just the fragment that was cited.
 */
export async function openRule(rule, corpus) {
	const c = corpus || activeCorpus();
	const number = rule.number || "";
	const title = number ? `${number}${rule.title ? " · " + rule.title : ""}` : (rule.title || "Rule");
	const body = showDrawer(title);

	if (rule.body) body.append(ruleCard(rule, "", false, c));

	// A numbered entry can be opened as a page of its book — the reader
	// resolves the citation onto the section that carries it.
	if (number) body.append(readLink(c, number));

	const root = sectionKeyOf(number);
	if (!root) return;

	const note = el("p", { class: "drawer-note", text: "Loading the full section…" });
	body.append(note);
	try {
		const data = await api.section(c, root);
		const entries = [];
		if (data.parent) entries.push(data.parent);
		if (data.children) entries.push(...data.children);
		if (entries.length === 0) {
			note.remove();
			return;
		}
		clear(body);
		if (data.parent && data.parent.body && data.parent.body.length <= 60) {
			$("drawer-title").textContent = `${data.parent.number} · ${data.parent.body}`;
		}
		entries.forEach((e, i) => body.append(ruleCard(e, "", i > 0, c)));
		if (number) body.append(readLink(c, number));
	} catch (_) {
		note.textContent = "The full section could not be loaded.";
	}
}

/** The trailing affordance that hands a citation to the reading surface. */
function readLink(corpus, number) {
	return el("button", {
		class: "drawer-read",
		text: "Read this in the book",
		attrs: { type: "button", title: "Open the reading surface at this rule" },
		on: {
			click: () => {
				closeDrawer();
				openReader(corpus, { number });
			},
		},
	});
}

/** Open a card. Accepts a resolved card object or a bare name to look up. */
export async function openCard(card) {
	if (typeof card === "string") {
		const body = showDrawer(card);
		body.append(el("p", { class: "drawer-note", text: "Consulting Scryfall…" }));
		try {
			const data = await api.card(card);
			const found = data.card || (data.matches && data.matches[0]);
			if (!found) {
				clear(body).append(el("p", { class: "drawer-note", text: `No card found for “${card}”.` }));
				return;
			}
			$("drawer-title").textContent = found.name;
			clear(body).append(cardView(found));
		} catch (err) {
			clear(body).append(el("p", { class: "drawer-note", text: "The card could not be reached." }));
		}
		return;
	}
	showDrawer(card.name || "Card").append(cardView(card));
}

/**
 * Open a resolved entity — a D&D spell, monster or item the sage looked up.
 * The answer already ships the entity's full text, so there is nothing to
 * fetch. It renders through ruleCard because that is exactly the shape: a
 * short marker, a name, and the body underneath.
 */
export function openEntity(entity, corpus) {
	const c = corpus || activeCorpus();
	showDrawer(entity.name || "Reference")
		.append(ruleCard({ number: entity.kind, title: entity.name, body: entity.body }, "", false, c));
}

/** "702.2a" -> "702.2"; null for unnumbered entries, which have no section. */
function sectionKeyOf(number) {
	const m = String(number || "").match(/^(\d{1,3}(?:\.\d+)+)([a-z]+)?$/);
	return m ? m[1] : null;
}
