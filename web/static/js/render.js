// Rendering for the content surfaces: answers, rule cards, and card views.

import { el, truncate } from "./dom.js";
import { renderMarkdown, highlight } from "./markdown.js";
import { refs } from "./refs.js";
import { manaNodes, manaInEscaped, setSymbol } from "./mana.js";
import { verifyCitations } from "./api.js";

/**
 * Render an answer body. `corpus` decides two things: whether rule numbers in
 * the prose become clickable references, and whether {U}-style notation
 * becomes mana pips. Both are Magic's; the D&D SRD numbers nothing and casts
 * nothing in curly braces.
 */
export function renderAnswer(container, text, corpus) {
	const mtg = corpus === "mtg";
	container.innerHTML = renderMarkdown(splitFollowUps(text).body, { rules: mtg, mana: mtg });
}

// The sage closes an answer with a machine-readable line of the questions a
// player would ask next. It is an instruction to this UI, not prose, so it is
// stripped before rendering — including while it is still arriving, or the
// reader watches "FOLLOW-U" type itself out at the end of every answer.
const FOLLOWUPS_RE = /^FOLLOW-UPS:[ \t]*(.*)$/im;
const FOLLOWUPS_PARTIAL_RE = /\n(?:F(?:O(?:L(?:L(?:O(?:W(?:-(?:U(?:P(?:S(?::.*)?)?)?)?)?)?)?)?)?)?)$/i;

/**
 * Separate an answer's prose from the follow-up questions it ends with.
 * Returns the body to render and up to three questions for the branch chips.
 */
export function splitFollowUps(text) {
	const src = String(text == null ? "" : text);
	const m = src.match(FOLLOWUPS_RE);
	if (m) {
		return {
			body: src.slice(0, m.index).trimEnd(),
			followUps: m[1].split("|").map((q) => q.trim()).filter(Boolean).slice(0, 3),
		};
	}
	// Mid-stream: the marker has begun but not finished arriving.
	return { body: src.replace(FOLLOWUPS_PARTIAL_RE, ""), followUps: [] };
}

/**
 * The questions a reader is most likely to ask next, as one-click branches.
 * The sage already writes these caveats into its answers ("this only works
 * while the spell is still on the stack"); each one is a question, and asking
 * it should not require retyping the whole scenario.
 */
export function renderFollowUps(questions, onPick) {
	if (!questions || !questions.length) return null;
	const wrap = el("div", { class: "followups" });
	wrap.append(el("span", { class: "citations-label", text: "Ask next:" }));
	for (const q of questions) {
		wrap.append(el("button", {
			class: "chip chip-followup",
			text: q,
			attrs: { type: "button" },
			on: { click: () => onPick(q) },
		}));
	}
	return wrap;
}

/** Wire rule-reference buttons produced by the markdown pass. */
export function bindRuleRefs(root, corpus) {
	root.querySelectorAll(".rule-ref").forEach((btn) => {
		if (btn.dataset.bound) return;
		btn.dataset.bound = "1";
		btn.addEventListener("click", () => refs.openRule({ number: btn.dataset.rule }, corpus));
	});
}

/**
 * Resolve every rule number in a rendered answer against the index and mark
 * what came back. A number that exists gets the rule's own title and text on
 * its tooltip — the check a reader would otherwise have to click to make — and
 * a number that does not exist is marked as an invention rather than being
 * offered as a citation.
 *
 * It runs on rendered output rather than during the answer, so a conversation
 * reopened later is verified exactly as strictly as a live one. A failed
 * request leaves every reference as it was: unverified is not the same claim
 * as wrong, and the UI must never say the second when it means the first.
 *
 * Returns the checks so callers can reuse the rule text (the copy action
 * quotes it).
 */
export async function verifyRuleRefs(root, corpus) {
	if (corpus !== "mtg") return [];
	const refsIn = [...root.querySelectorAll(".rule-ref")];
	const numbers = [...new Set(refsIn.map((b) => b.dataset.rule))];
	if (numbers.length === 0) return [];

	let checks;
	try {
		checks = (await verifyCitations(corpus, numbers)).checks || [];
	} catch (_) {
		return [];
	}
	const byNumber = new Map(checks.map((c) => [c.number, c]));
	for (const btn of refsIn) {
		const check = byNumber.get(btn.dataset.rule);
		if (!check) continue;
		btn.dataset.cite = check.status;
		if (check.status === "unknown") {
			btn.title = "No rule with this number exists in the index — the sage may have misremembered it.";
		} else {
			btn.title = (check.title ? check.title + " — " : "") + (check.body || "");
		}
	}
	return checks;
}

/**
 * A copy action for a whole answer.
 *
 * Rule numbers and citation chips are buttons, and browsers leave buttons out
 * of a text selection, so copying an answer the ordinary way silently drops
 * every rule number in it — "referenced in rule :" is what lands in the paste,
 * which is worse than useless in the argument the copy was meant to settle.
 * This copies the answer as the sage wrote it, numbers intact, and appends the
 * text of every rule it cited: the flyouts a reader could have clicked, spelled
 * out for somewhere they cannot.
 */
export function copyAnswerButton(getText, getChecks) {
	const btn = el("button", {
		class: "msg-action",
		text: "Copy ruling",
		attrs: { type: "button", "aria-label": "Copy this answer with the rules it cites" },
	});
	btn.addEventListener("click", async () => {
		try {
			await navigator.clipboard.writeText(rulingText(getText(), getChecks()));
			btn.textContent = "Copied";
		} catch (_) {
			btn.textContent = "Copy failed";
		}
		setTimeout(() => { btn.textContent = "Copy ruling"; }, 1600);
	});
	return btn;
}

/** The plain-text form of an answer: the prose, then the rules it cited. */
export function rulingText(answer, checks) {
	const cited = (checks || []).filter((c) => c.status === "indexed" && c.body);
	if (cited.length === 0) return answer;
	const lines = [answer, "", "— Rules cited —"];
	for (const c of cited) {
		lines.push("", c.number + (c.title ? " — " + c.title : ""), c.body);
	}
	return lines.join("\n");
}

/** Citation strip under an answer: rules consulted, cards/entities looked up, misses. */
export function renderCitations(sources, cards, entities, unresolved, corpus) {
	const hasAny = (sources && sources.length) || (cards && cards.length) || (entities && entities.length) || (unresolved && unresolved.length);
	if (!hasAny) return null;

	const wrap = el("div", { class: "citations" });

	// A source with a URL lives off-site — a Scryfall search the sage ran —
	// and there is no drawer page for it; the chip is the link to the full
	// result list, which is the whole point of citing a search.
	const rules = (sources || []).filter((s) => !s.url);
	const searches = (sources || []).filter((s) => s.url);

	if (rules.length) {
		wrap.append(el("span", { class: "citations-label", text: "Rules:" }));
		for (const s of rules.slice(0, 10)) {
			wrap.append(el("button", {
				class: "chip",
				text: s.number || s.title || "•",
				attrs: { type: "button", title: (s.title ? s.title + " — " : "") + truncate(s.body, 160) },
				on: { click: () => refs.openRule(s, corpus) },
			}));
		}
	}

	if (searches.length) {
		wrap.append(el("span", { class: "citations-label", text: "Searches:" }));
		for (const s of searches.slice(0, 6)) {
			wrap.append(el("a", {
				class: "chip chip-search",
				text: s.title || s.url,
				attrs: { href: s.url, target: "_blank", rel: "noopener noreferrer", title: (s.body ? s.body + " — " : "") + "open the full list on Scryfall" },
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
