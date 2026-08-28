// The MTG deck builder: describe an idea, pick from proposed commanders,
// draft the 99 with the model over a stream, revise with feedback, analyze a
// pasted list, and save/load decks. Layered over the transcript the way the
// study and encounter surfaces are. Every card shown came back
// server-verified; names the model invented are flagged, never displayed as
// real.

import { $, el, clear, isNarrow } from "./dom.js";
import { api, streamDeckBuild, streamDeckChat } from "./api.js";
import { state } from "./state.js";
import { renderAnswer, bindRuleRefs } from "./render.js";

let idea = "";
let colors = "";
let commander = null; // { name, mana_cost, type_line, oracle_text, ... }
let entries = []; // current deck: [{ name, count, board, note }]
let analysis = null;
let commentary = "";
let unverified = [];
let currentID = null; // saved deck id, null while unsaved
let draftAbort = null;
let chatTurns = []; // the conversation about this deck: [{ role, content }]
let chatAbort = null;

function wire() {

	$("deck-idea-form").addEventListener("submit", onPropose);
	$("deck-commanders").addEventListener("click", (e) => {
		const pick = e.target.closest("[data-draft]");
		if (!pick) return;
		startDraft(pick.dataset.draft);
	});
	$("deck-feedback-form").addEventListener("submit", onRevise);
	$("deck-analyze-form").addEventListener("submit", onAnalyze);
	$("deck-save").addEventListener("submit", onSave);
	$("deck-copy").addEventListener("click", onCopy);
	$("deck-delete").addEventListener("click", onDelete);
	$("deck-saved").addEventListener("change", onPickSaved);
	$("deck-chat-form").addEventListener("submit", onChatAsk);
	$("deck-chat-clear").addEventListener("click", resetChat);
	renderChat();
}

/** Open the builder surface. */
function openDeck() {
	loadSavedList();
	$("deck-idea").focus();
}

/** Drop back to the transcript. */
function closeDeck() {
	if (draftAbort) draftAbort.abort();
	if (chatAbort) chatAbort.abort();
}

/* ---------- Propose ---------- */

async function onPropose(e) {
	e.preventDefault();
	idea = $("deck-idea").value.trim();
	colors = $("deck-colors").value.trim();
	if (!idea && !colors) {
		$("deck-meta").textContent = "Describe the deck you want first.";
		return;
	}
	const box = clear($("deck-commanders"));
	box.append(el("p", { class: "deck-empty", text: "Consulting the card database…" }));
	try {
		const data = await api.deckPropose(idea, colors);
		renderCommanders(box, data.commanders || []);
	} catch (err) {
		clear(box);
		box.append(el("p", { class: "deck-empty warn", text: `Search failed: ${err.message}` }));
	}
}

function renderCommanders(box, cmdrs) {
	clear(box);
	if (cmdrs.length === 0) {
		box.append(el("p", { class: "deck-empty", text: "No commanders matched — try broader terms." }));
		return;
	}
	box.append(el("p", { class: "deck-hint", text: "Pick a commander to draft with:" }));
	for (const c of cmdrs.slice(0, 8)) {
		const bits = [];
		if (c.mana_cost) bits.push(c.mana_cost);
		if (c.type_line) bits.push(c.type_line.split(" — ")[0]);
		if (c.edhrec_rank) bits.push(`EDHREC #${c.edhrec_rank.toLocaleString()}`);
		// el() sets attributes with setAttribute, which escapes for us — running
		// esc() here too would hand back "Shelob&#39;s ..." as the commander.
		box.append(el("button", {
			class: "deck-hit",
			attrs: { type: "button", "data-draft": c.name, title: `Draft with ${c.name}` },
		},
		el("span", { class: "deck-hit-name", text: c.name }),
		el("span", { class: "deck-hit-meta", text: bits.join(" · ") }),
		c.why ? el("span", { class: "deck-hit-why", text: c.why }) : null));
	}
}

/* ---------- Draft + revise ---------- */

function startDraft(name) {
	commander = { name };
	entries = [];
	unverified = [];
	analysis = null;
	commentary = "";
	resetChat();
	$("deck-revise-btn").disabled = false;
	runBuild(false);
}

async function onRevise(e) {
	e.preventDefault();
	if (!commander) return;
	runBuild(true);
}

async function runBuild(revision) {
	if (draftAbort) draftAbort.abort();
	const controller = new AbortController();
	draftAbort = controller;
	const feedback = revision ? $("deck-feedback").value.trim() : "";
	const status = $("deck-draft-status");
	status.textContent = revision ? "Revising the draft…" : `Drafting around ${commander.name}…`;
	if (!revision) {
		clear($("deck-list"));
		$("deck-commentary").hidden = true;
		clear($("deck-report"));
	}

	let streamed = "";
	try {
		await streamDeckBuild(idea, commander.name, feedback, revision ? entries : [], {
			onMeta: (meta) => {
				if (meta.edhrec) status.textContent += " (EDHREC synergy data)";
			},
			onDelta: (text) => {
				streamed += text;
				$("deck-commentary").hidden = false;
				// Plain text while it streams — re-rendering Markdown on every
				// token would rebuild the block hundreds of times.
				$("deck-commentary").textContent = streamed;
				$("deck-commentary").classList.remove("prose");
			},
			onDone: (payload) => {
				entries = payload.deck || [];
				analysis = payload.analysis || null;
				unverified = payload.unverified || [];
				commentary = payload.commentary || streamed;
				const box = $("deck-commentary");
				box.classList.add("prose");
				renderAnswer(box, commentary, "mtg");
				bindRuleRefs(box, "mtg");
				renderDeck();
				renderReport();
				status.textContent = `${commander.name} · ${totalMain()} cards`;
			},
			onError: (msg) => {
				status.textContent = `Draft failed: ${msg}`;
			},
		}, controller.signal);
	} catch (err) {
		if (err.name !== "AbortError") status.textContent = `Draft failed: ${err.message}`;
	}
}

function totalMain() {
	return entries.filter((e) => e.board !== "sideboard" && e.board !== "commander")
		.reduce((n, e) => n + e.count, 0);
}

/* ---------- Analyze ---------- */

async function onAnalyze(e) {
	e.preventDefault();
	const list = $("deck-analyze-input").value.trim();
	if (!list) return;
	const status = $("deck-draft-status");
	status.textContent = "Analyzing the list…";
	try {
		const data = await api.deckAnalyze(list, $("deck-analyze-commander").value.trim());
		commander = data.commander ? { name: data.commander.name } : commander;
		entries = data.deck || [];
		resetChat();
		analysis = data.analysis || null;
		unverified = data.unresolved || [];
		commentary = data.critique || "";
		renderDeck();
		renderReport();
		status.textContent = analysis ? `${commander ? commander.name + " · " : ""}${analysis.total_main} cards analyzed` : "Analyzed.";
	} catch (err) {
		status.textContent = `Analysis failed: ${err.message}`;
	}
}

/* ---------- Talk about the deck ----------
   The analyzer says what the list *is*; this is where the player argues with
   it. The deck on screen travels with every question, so a draft that was
   never saved is as discussable as a saved one. */

async function onChatAsk(e) {
	e.preventDefault();
	const input = $("deck-chat-input");
	const question = input.value.trim();
	if (!question) return;
	if (entries.length === 0 && !commander) {
		$("deck-chat-status").textContent = "Draft, load or analyze a deck first.";
		return;
	}
	if (chatAbort) chatAbort.abort();
	const controller = new AbortController();
	chatAbort = controller;

	input.value = "";
	const history = chatTurns.slice();
	chatTurns.push({ role: "user", content: question });
	const answer = { role: "assistant", content: "" };
	chatTurns.push(answer);
	renderChat();
	$("deck-chat-status").textContent = "Thinking…";

	const cards = entries.map((c) => ({ name: c.name, count: c.count, board: c.board }));
	try {
		await streamDeckChat(commander ? commander.name : "", cards, question, history, {
			onDelta: (text) => {
				answer.content += text;
				renderChat();
			},
			onDone: () => {
				$("deck-chat-status").textContent = "";
				renderChat();
			},
			onError: (msg) => {
				$("deck-chat-status").textContent = msg;
				// Drop an answer that never arrived, so the next question is
				// not sent with an empty assistant turn in its history.
				if (!answer.content) chatTurns = chatTurns.filter((t) => t !== answer);
				renderChat();
			},
		}, controller.signal);
	} catch (err) {
		if (err.name !== "AbortError") $("deck-chat-status").textContent = `Ask failed: ${err.message}`;
	}
}

function resetChat() {
	if (chatAbort) chatAbort.abort();
	chatTurns = [];
	$("deck-chat-status").textContent = "";
	renderChat();
}

/** A block of model-written Markdown, rendered the way a sage answer is —
 *  headings, lists, emphasis and mana pips, not literal asterisks. */
function prose(className, text) {
	const box = el("div", { class: `${className} prose` });
	renderAnswer(box, text, "mtg");
	bindRuleRefs(box, "mtg");
	return box;
}

function renderChat() {
	const log = clear($("deck-chat-log"));
	if (chatTurns.length === 0) {
		log.append(el("p", {
			class: "deck-empty",
			text: "Ask about the deck — what to cut, why the curve is off, whether a card is pulling its weight.",
		}));
		return;
	}
	for (const t of chatTurns) {
		const sage = t.role === "assistant";
		log.append(el("div", { class: `deck-chat-turn is-${sage ? "sage" : "player"}` },
			el("span", { class: "deck-chat-who", text: sage ? "Sage" : "You" }),
			sage && t.content
				? prose("deck-chat-text", t.content)
				: el("div", { class: "deck-chat-text", text: t.content || "…" })));
	}
	log.scrollTop = log.scrollHeight;
}

/* ---------- Rendering ---------- */

function renderDeck() {
	const wrap = clear($("deck-list"));
	if (entries.length === 0) {
		wrap.append(el("p", { class: "deck-empty", text: "No draft yet — describe an idea and pick a commander." }));
		return;
	}
	const cmdr = entries.filter((e) => e.board === "commander");
	const main = entries.filter((e) => e.board !== "commander" && e.board !== "sideboard");
	const side = entries.filter((e) => e.board === "sideboard");

	if (cmdr.length || commander) {
		wrap.append(el("p", { class: "deck-section", text: "Commander" }));
		for (const e of cmdr.length ? cmdr : [{ name: commander.name, count: 1 }]) {
			wrap.append(deckRow(e));
		}
	}
	wrap.append(el("p", { class: "deck-section", text: `Maindeck (${main.reduce((n, e) => n + e.count, 0)})` }));
	for (const e of main) wrap.append(deckRow(e));
	if (side.length) {
		wrap.append(el("p", { class: "deck-section", text: `Sideboard (${side.reduce((n, e) => n + e.count, 0)})` }));
		for (const e of side) wrap.append(deckRow(e));
	}
	if (unverified.length > 0) {
		wrap.append(el("p", { class: "deck-flag warn", text: `Couldn't verify (excluded): ${unverifiedNames().join(", ")}` }));
	}
}

/** The unverified list arrives as names from a draft and as whole entries from
 *  an analysis. Read both as names — printing one shape with the other's
 *  renderer is how the panel ended up listing "[object Object]". */
function unverifiedNames() {
	return unverified.map((u) => (typeof u === "string" ? u : u && u.name) || "").filter(Boolean);
}

function deckRow(e) {
	const note = e.note ? el("span", { class: "deck-row-note", text: e.note }) : null;
	return el("div", { class: "deck-row" },
		el("span", { class: "deck-row-count", text: String(e.count) }),
		el("span", { class: "deck-row-name", text: e.name }),
		note);
}

function renderReport() {
	const wrap = clear($("deck-report"));
	if (!analysis) return;

	const chips = [];
	if (analysis.identity) chips.push(`Identity ${analysis.identity}`);
	chips.push(`${analysis.total_main} maindeck`);
	chips.push(`${analysis.lands} lands`);
	chips.push(`avg MV ${Number(analysis.avg_mv || 0).toFixed(1)}`);
	if (analysis.game_changers) chips.push(`${analysis.game_changers} game-changers`);
	const chipRow = el("div", { class: "deck-chips" });
	for (const c of chips) chipRow.append(el("span", { class: "deck-chip", text: c }));
	wrap.append(chipRow);

	const roles = el("div", { class: "deck-chips" });
	roles.append(el("span", { class: "deck-chip", text: `Ramp ${analysis.ratios.ramp}` }));
	roles.append(el("span", { class: "deck-chip", text: `Draw ${analysis.ratios.draw}` }));
	roles.append(el("span", { class: "deck-chip", text: `Interaction ${analysis.ratios.interaction}` }));
	wrap.append(roles);

	if (analysis.identity_violations && analysis.identity_violations.length > 0) {
		const names = analysis.identity_violations.map((v) => `${v.name} (${v.identity})`).join(", ");
		wrap.append(el("p", { class: "deck-flag warn", text: `Color identity violations: ${names}` }));
	}
	if (analysis.saltiest && analysis.saltiest.length > 0) {
		const salt = analysis.saltiest.map((s) => `${s.name} (${Number(s.salt).toFixed(1)})`).join(", ");
		wrap.append(el("p", { class: "deck-flag salt", text: `Saltiest: ${salt}` }));
	}
	for (const w of analysis.warnings || []) {
		wrap.append(el("p", { class: "deck-flag", text: w }));
	}
	if (commentary) {
		// The model writes Markdown. Dropping it into textContent is what put
		// literal "## What Works" and "**Bottom line**" on screen.
		wrap.append(prose("deck-critique", commentary));
	}

	// Curve bar chart, pure frontend from the analysis bucket counts.
	renderCurve(analysis.curve || {});
}

function renderCurve(curve) {
	const chart = $("deck-curve");
	clear(chart);
	const max = Math.max(1, ...Object.values(curve));
	const bars = el("div", { class: "deck-curve-bars" });
	for (let mv = 0; mv <= 10; mv++) {
		const n = curve[String(mv)] || curve[mv] || 0;
		bars.append(el("div", {
			class: "deck-curve-col",
			attrs: { title: `MV ${mv}: ${n} cards` },
		},
		el("div", { class: "deck-curve-bar", attrs: { style: `height:${Math.round((n / max) * 100)}%` } }),
		el("span", { class: "deck-curve-n", text: n ? String(n) : "" }),
		el("span", { class: "deck-curve-lbl", text: mv === 10 ? "10+" : String(mv) })));
	}
	chart.hidden = false;
	chart.append(el("p", { class: "deck-section", text: "Mana curve" }), bars);
}

/* ---------- Save / load ---------- */

async function loadSavedList() {
	try {
		const data = await api.listDecks();
		const sel = $("deck-saved");
		const current = sel.value;
		clear(sel);
		sel.append(el("option", { attrs: { value: "" }, text: "Saved decks…" }));
		for (const d of data.decks || []) {
			sel.append(el("option", { attrs: { value: d.id }, text: d.name }));
		}
		sel.value = current;
	} catch (_) { /* dropdown stays empty */ }
}

async function onPickSaved() {
	const id = $("deck-saved").value;
	if (!id) return;
	try {
		const data = await api.getDeck(id);
		const d = data.deck;
		if (!d) return;
		currentID = d.id;
		commander = d.commander ? { name: d.commander } : null;
		entries = d.cards || [];
		resetChat();
		analysis = null;
		unverified = [];
		commentary = d.notes || "";
		$("deck-name").value = d.name || "";
		$("deck-delete").hidden = false;
		$("deck-revise-btn").disabled = !commander;
		if (commander) $("deck-idea").value = $("deck-idea").value || d.name;
		renderDeck();
		renderReport();
		$("deck-draft-status").textContent = `${commander ? commander.name + " · " : ""}${totalMain()} cards`;
	} catch (err) {
		$("deck-meta").textContent = `Could not load: ${err.message}`;
	}
}

async function onSave(e) {
	e.preventDefault();
	const name = $("deck-name").value.trim();
	if (!name) {
		$("deck-meta").textContent = "Name the deck before saving.";
		return;
	}
	if (entries.length === 0) {
		$("deck-meta").textContent = "Nothing to save — draft or analyze a deck first.";
		return;
	}
	const cards = entries.map((e) => ({ name: e.name, count: e.count, board: e.board }));
	try {
		const data = await api.saveDeck(currentID, name, commander ? commander.name : "", cards, commentary);
		const d = data.deck;
		currentID = d.id;
		$("deck-delete").hidden = false;
		$("deck-meta").textContent = `Saved “${d.name}”.`;
		await loadSavedList();
		$("deck-saved").value = currentID;
	} catch (err) {
		$("deck-meta").textContent = `Save failed: ${err.message}`;
	}
}

async function onDelete() {
	if (!currentID) return;
	try {
		await api.deleteDeck(currentID);
		currentID = null;
		entries = [];
		analysis = null;
		$("deck-name").value = "";
		$("deck-delete").hidden = true;
		$("deck-meta").textContent = "Deleted.";
		renderDeck();
		renderReport();
		await loadSavedList();
	} catch (err) {
		$("deck-meta").textContent = `Delete failed: ${err.message}`;
	}
}

function onCopy() {
	const lines = [];
	for (const e of entries) {
		if (e.board === "commander") lines.push(`Commander: ${e.name}`);
	}
	for (const e of entries) {
		if (e.board !== "commander" && e.board !== "sideboard") lines.push(`${e.count} ${e.name}`);
	}
	for (const e of entries) {
		if (e.board === "sideboard") lines.push(`${e.count} ${e.name}`);
	}
	const text = lines.join("\n");
	if (!text) return;
	navigator.clipboard?.writeText(text).then(
		() => { $("deck-meta").textContent = "Decklist copied."; },
		() => { $("deck-meta").textContent = "Copy failed — select the list manually."; });
}

/* ---------- the window-manager contract ---------- */

// mount() adopts the surface that already exists in index.html rather than
// building one, which is what made migrating nine surfaces tractable: every
// $("deck-idea") lookup inside this module keeps working untouched. The
// cost is one window per tool; see the note on `instances` in wm/registry.js.
let wired = false;

export const tool = {
	mount(host) {
		const view = $("deck-view");
		host.append(view);
		view.hidden = false;
		if (!wired) {
			wire();
			wired = true;
		}
		openDeck();
		return { destroy: closeDeck };
	},
};
