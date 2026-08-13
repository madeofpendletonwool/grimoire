// Resolve mode: a structured MTG interaction resolver. The reader states a
// board and a proposed spell/ability sequence, and the sage walks the resulting
// interactions step by step (stack, APNAP triggers, layers, replacement
// effects) with each step cited. It is separate from the free Q&A chat: a
// resolve is stateless and not saved, so the transcript is cleared on entry.

import { $, el, clear, isNarrow } from "./dom.js";
import { streamResolve } from "./api.js";
import { state, activeCorpus, supportsResolve } from "./state.js";
import { renderAnswer, bindRuleRefs, renderCitations } from "./render.js";
import { closeDrawer } from "./drawer.js";

// The canned puzzles mirror internal/resolver/puzzles.go so the UI can offer
// them as one-click examples. Each carries the line-oriented board/sequence the
// parser on the server understands.
const PUZZLES = [
	{
		name: "Blood Artist + Zulaport vs. a board wipe",
		board: "You: Blood Artist\nYou: Zulaport Cutthroat\nYou: Doomed Traveler",
		sequence: "1. Opp casts Wrath of God",
		note: "It is the opponent's turn.",
	},
	{
		name: "Fleshbag Marauder across two players (APNAP)",
		board: "You: Blood Artist\nOpp: Grim Haruspex",
		sequence: "1. You cast Fleshbag Marauder\n2. Its ETB trigger resolves (each player sacrifices a creature)",
		note: "It is your turn (you are the active player).",
	},
	{
		name: "Rest in Peace stops dies triggers",
		board: "You: Rest in Peace\nOpp: Blood Artist",
		sequence: "1. Opp casts Wrath of God",
		note: "Rest in Peace: if a card or token would be put into a graveyard from anywhere, exile it instead.",
	},
];

let abortStream = null;

export function isResolveMode() {
	return state.mode === "resolve";
}

export function initResolve() {
	const resolver = $("resolver");
	resolver.addEventListener("submit", (e) => {
		e.preventDefault();
		submitResolver();
	});
	$("resolve-stop").addEventListener("click", stopResolveStreaming);

	for (const id of ["resolver-board", "resolver-sequence"]) {
		const ta = $(id);
		ta.addEventListener("input", () => autosize(ta));
	}

	document.querySelectorAll(".mode-opt").forEach((btn) => {
		btn.addEventListener("click", () => setMode(btn.dataset.mode));
	});
}

/** Switch between "ask" and "resolve". Resolve is MTG-only; a non-MTG corpus forces ask. */
export function setMode(mode) {
	if (mode === "resolve" && !supportsResolve(activeCorpus())) {
		mode = "ask";
	}
	if (state.mode === mode) return;
	stopResolveStreaming();
	state.mode = mode;
	if (mode === "resolve") {
		state.chat = null; // resolve is stateless; no open conversation
	}
	clear($("messages"));
	$("welcome").hidden = false;
	closeDrawer();
	syncModeChrome();
	renderWelcome();
	if (mode === "resolve") {
		focusResolver();
	} else if (!isNarrow()) {
		$("composer-input").focus();
	}
}

/** Stop the in-flight resolve stream, keeping whatever trace arrived. */
export function stopResolveStreaming() {
	if (abortStream) abortStream.abort();
}

/**
 * Apply mode-dependent chrome. Called from chat.syncChrome so the toggle and
 * composer stay in step with corpus + mode changes made anywhere in the app.
 */
export function syncModeChrome() {
	const mtg = supportsResolve(activeCorpus());
	const resolve = state.mode === "resolve" && mtg;

	// Resolve is unavailable outside Magic; silently revert to ask.
	if (!mtg && state.mode === "resolve") {
		state.mode = "ask";
	}

	$("mode-toggle").hidden = !mtg;
	document.querySelectorAll(".mode-opt").forEach((btn) => {
		const on = btn.dataset.mode === state.mode;
		btn.classList.toggle("is-active", on);
		btn.setAttribute("aria-checked", on ? "true" : "false");
	});

	$("composer").hidden = resolve;
	$("resolver").hidden = !resolve;
	$("resolver-caveat").hidden = !resolve;
	// Rename/delete belong to saved conversations; resolve is stateless.
	$("topbar-rename").hidden = resolve || !state.chat;
	$("topbar-delete").hidden = resolve || !state.chat;
}

/** Render the welcome suggestions for the active mode. */
export function renderWelcome() {
	const sub = $("welcome-sub");
	const wrap = clear($("suggestions"));
	if (state.mode === "resolve") {
		sub.textContent = "State a board and a sequence — the sage walks the interactions, step by step.";
		PUZZLES.forEach((p) => {
			wrap.append(el("button", {
				class: "suggestion",
				text: p.name,
				attrs: { type: "button", title: "Load this example puzzle" },
				on: {
					click: () => {
						$("resolver-board").value = p.board;
						$("resolver-sequence").value = p.sequence;
						$("resolver-note").value = p.note || "";
						autosize($("resolver-board"));
						autosize($("resolver-sequence"));
						focusResolver();
					},
				},
			}));
		});
	}
}

/* ---------- Resolve flow ---------- */

function submitResolver() {
	if (state.streaming) return;
	const board = $("resolver-board").value;
	const sequence = $("resolver-sequence").value;
	const note = $("resolver-note").value;
	if (!board.trim() && !sequence.trim()) {
		setFoot("State a board or a sequence to resolve.", true);
		return;
	}
	resolve(board, sequence, note);
}

async function resolve(board, sequence, note) {
	$("welcome").hidden = true;
	const list = $("messages");
	list.append(resolveRecap(board, sequence, note));

	const { row, bubble, prose } = pendingSage();
	list.append(row);
	scrollToBottom();

	setStreaming(true);
	let text = "";
	let meta = { sources: [], cards: [], unresolved_cards: [] };
	const controller = new AbortController();
	abortStream = controller;
	const stick = stickyScroll();

	try {
		await streamResolve(board, sequence, note, {
			onMeta: (payload) => { meta = payload; },
			onDelta: (chunk) => {
				text += chunk;
				renderAnswer(prose, text, "mtg");
				stick();
			},
			onDone: () => finishTrace(row, bubble, prose, text, meta),
			onError: (message) => {
				if (text) {
					finishTrace(row, bubble, prose, text, meta);
					row.append(el("p", { class: "drawer-note", text: message }));
				} else {
					row.classList.add("is-error");
					prose.innerHTML = "";
					prose.append(el("p", { text: message }));
					bubble.classList.remove("is-streaming");
				}
			},
		}, controller.signal);
	} catch (err) {
		if (err.name === "AbortError") {
			finishTrace(row, bubble, prose, text || "(stopped)", meta);
		} else {
			row.classList.add("is-error");
			prose.innerHTML = "";
			prose.append(el("p", { text: `The sage could not be reached: ${err.message}` }));
			bubble.classList.remove("is-streaming");
		}
	} finally {
		setStreaming(false);
		abortStream = null;
		scrollToBottom();
		focusResolver();
	}
}

function finishTrace(row, bubble, prose, text, meta) {
	renderAnswer(prose, text, "mtg");
	bindRuleRefs(prose, "mtg");
	bubble.classList.remove("is-streaming");
	const cites = renderCitations(meta.sources, meta.cards, null, meta.unresolved_cards, "mtg");
	if (cites) bubble.append(cites);
}

function setStreaming(on) {
	state.streaming = on;
	$("resolve-btn").hidden = on;
	$("resolve-stop").hidden = !on;
	["resolver-board", "resolver-sequence", "resolver-note"].forEach((id) => {
		$(id).setAttribute("aria-busy", on ? "true" : "false");
	});
}

/* ---------- Message nodes ---------- */

// A compact recap of what the reader asked to resolve, shown as the user turn.
function resolveRecap(board, sequence, note) {
	const recap = el("div", { class: "resolver-recap" });
	recap.append(recapSection("Board", board));
	recap.append(recapSection("Sequence", sequence));
	if (note.trim()) recap.append(recapSection("Note", note));
	return el("div", { class: "msg msg-user" }, el("div", { class: "bubble" }, recap));
}

function recapSection(label, text) {
	if (!text.trim()) return el("span");
	return el("div", { class: "recap-sec" },
		el("span", { class: "recap-k", text: label }),
		el("div", { class: "recap-v", text: text }));
}

function pendingSage() {
	const prose = el("div", { class: "prose" },
		el("span", { class: "thinking" },
			el("span", { text: "The sage reads the stack" }),
			el("span", { text: "." }), el("span", { text: "." }), el("span", { text: "." })));
	const bubble = el("div", { class: "bubble is-streaming" }, prose);
	const row = el("div", { class: "msg msg-sage" },
		el("div", { class: "who" }, el("span", { text: "🔮 The Resolver" })),
		bubble);
	return { row, bubble, prose };
}

/* ---------- Helpers (resolve owns its own, decoupled from chat.js) ---------- */

function setFoot(text, warn) {
	const foot = $("composer-foot");
	foot.textContent = text || "";
	foot.classList.toggle("warn", !!warn);
}

function autosize(input) {
	input.style.height = "auto";
	input.style.height = Math.min(input.scrollHeight, window.innerHeight * 0.4) + "px";
}

function focusResolver() {
	if (!isNarrow()) $("resolver-board").focus();
}

function scrollToBottom(instant) {
	const t = $("transcript");
	t.scrollTo({ top: t.scrollHeight, behavior: instant ? "auto" : "smooth" });
}

function stickyScroll() {
	const t = $("transcript");
	let follow = t.scrollHeight - t.scrollTop - t.clientHeight < 120;
	t.addEventListener("scroll", () => {
		follow = t.scrollHeight - t.scrollTop - t.clientHeight < 120;
	}, { passive: true });
	return () => { if (follow) t.scrollTop = t.scrollHeight; };
}
