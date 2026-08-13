// The conversation: transcript, composer, streaming, and saved history.

import { $, el, clear, isNarrow } from "./dom.js";
import { api, streamAnswer } from "./api.js";
import { state, activeCorpus, corpusLabel, supportsCards, saveCorpusPreference } from "./state.js";
import { renderAnswer, bindRuleRefs, renderCitations, renderRulings } from "./render.js";
import { openRule, openCard, closeDrawer } from "./drawer.js";
import { openPalette } from "./palette.js";

let abortStream = null;

const SUGGESTIONS = {
	mtg: [
		"How does deathtouch interact with trample?",
		"What happens when Lightning Bolt is countered?",
		"Explain the layer system for continuous effects",
		"Can I respond to a mana ability?",
	],
	dnd: [
		"How does grappling work?",
		"What can I do with a bonus action?",
		"Explain advantage and disadvantage",
		"How does concentration on a spell break?",
	],
};

const SLASH = [
	{ cmd: "/card", desc: "Look up a Magic card by name", mtgOnly: true },
	{ cmd: "/rule", desc: "Open a rule by number, e.g. /rule 702.2" },
	{ cmd: "/search", desc: "Search rules and cards" },
];

/* ---------- Setup ---------- */

export function initChat() {
	const form = $("composer");
	const input = $("composer-input");

	form.addEventListener("submit", (e) => {
		e.preventDefault();
		submitComposer();
	});

	input.addEventListener("input", () => {
		autosize(input);
		updateSlashHints();
	});
	input.addEventListener("keydown", onComposerKeydown);

	$("stop-btn").addEventListener("click", stopStreaming);
	$("new-chat").addEventListener("click", () => startNewChat());
	$("topbar-rename").addEventListener("click", renameCurrent);
	$("topbar-delete").addEventListener("click", deleteCurrent);

	renderSuggestions();
}

/* ---------- Conversation lifecycle ---------- */

export function startNewChat() {
	if (state.streaming) stopStreaming();
	state.chat = null;
	closeDrawer();
	clear($("messages"));
	$("welcome").hidden = false;
	syncChrome();
	renderSuggestions();
	highlightHistory();
	focusComposer();
}

export async function openChat(id) {
	if (state.streaming) stopStreaming();
	try {
		const data = await api.getChat(id);
		state.chat = data.chat;
		$("welcome").hidden = true;
		const list = clear($("messages"));
		for (const m of data.messages || []) {
			list.append(m.role === "user" ? userMessage(m.content) : sageMessage(m));
		}
		syncChrome();
		highlightHistory();
		scrollToBottom(true);
		if (isNarrow()) $("app").classList.add("rail-hidden");
		focusComposer();
	} catch (err) {
		setFoot(`That conversation could not be opened: ${err.message}`, true);
	}
}

export async function refreshHistory() {
	try {
		const data = await api.listChats();
		state.chats = data.chats || [];
	} catch (_) {
		state.chats = [];
	}
	renderHistory();
}

function renderHistory() {
	const nav = clear($("history"));
	if (state.chats.length === 0) {
		nav.append(el("p", { class: "history-empty", text: "No saved chats yet." }));
		return;
	}
	let lastGroup = null;
	for (const c of state.chats) {
		const group = dateGroup(new Date(c.updated_at));
		if (group !== lastGroup) {
			nav.append(el("div", { class: "history-group", text: group }));
			lastGroup = group;
		}
		nav.append(el("button", {
			class: "history-item" + (state.chat && state.chat.id === c.id ? " is-active" : ""),
			attrs: { type: "button", "data-chat": c.id, title: c.title || "New chat" },
			on: { click: () => openChat(c.id) },
		},
			el("span", { class: "h-mark", text: c.corpus === "dnd" ? "⚔" : "✦" }),
			el("span", { class: "h-title", text: c.title || "New chat" }),
		));
	}
}

function highlightHistory() {
	$("history").querySelectorAll(".history-item").forEach((node) => {
		node.classList.toggle("is-active", !!state.chat && node.dataset.chat === state.chat.id);
	});
}

function dateGroup(d) {
	const today = new Date();
	const startOfDay = (x) => new Date(x.getFullYear(), x.getMonth(), x.getDate()).getTime();
	const days = Math.round((startOfDay(today) - startOfDay(d)) / 86400000);
	if (days <= 0) return "Today";
	if (days === 1) return "Yesterday";
	if (days < 7) return "This week";
	if (days < 30) return "This month";
	return "Earlier";
}

async function renameCurrent() {
	if (!state.chat) return;
	const next = window.prompt("Rename this conversation", state.chat.title || "");
	if (next == null || !next.trim()) return;
	try {
		const data = await api.renameChat(state.chat.id, next.trim());
		state.chat = data.chat;
		syncChrome();
		await refreshHistory();
	} catch (err) {
		setFoot(`Rename failed: ${err.message}`, true);
	}
}

async function deleteCurrent() {
	if (!state.chat) return;
	if (!window.confirm("Delete this conversation? This cannot be undone.")) return;
	try {
		await api.deleteChat(state.chat.id);
		startNewChat();
		await refreshHistory();
	} catch (err) {
		setFoot(`Delete failed: ${err.message}`, true);
	}
}

/* ---------- Sending ---------- */

function submitComposer() {
	const input = $("composer-input");
	const text = input.value.trim();
	if (!text || state.streaming) return;
	if (handleSlash(text)) {
		input.value = "";
		autosize(input);
		hideSlashHints();
		return;
	}
	input.value = "";
	autosize(input);
	hideSlashHints();
	ask(text);
}

/** Slash commands run locally against the reference surfaces. */
function handleSlash(text) {
	const m = text.match(/^\/(\w+)\s*(.*)$/s);
	if (!m) return false;
	const [, cmd, rest] = m;
	const arg = rest.trim();
	switch (cmd) {
		case "card":
			if (!supportsCards(activeCorpus())) {
				setFoot("Card lookup is Magic-only.", true);
				return true;
			}
			arg ? openCard(arg) : openPalette("");
			return true;
		case "rule":
			arg ? openRule({ number: arg }, activeCorpus()) : openPalette("");
			return true;
		case "search":
			openPalette(arg);
			return true;
		default:
			return false;
	}
}

async function ask(question) {
	// The first question of a session creates the conversation, locking in the
	// corpus chosen in the sidebar.
	if (!state.chat) {
		try {
			const data = await api.createChat(state.corpus);
			state.chat = data.chat;
			await refreshHistory();
		} catch (err) {
			setFoot(`Could not start a conversation: ${err.message}`, true);
			return;
		}
	}

	$("welcome").hidden = true;
	const list = $("messages");
	list.append(userMessage(question));

	const { row, bubble, prose } = pendingSage();
	list.append(row);
	scrollToBottom();

	setStreaming(true);
	let text = "";
	let meta = { sources: [], cards: [], rulings: [], unresolved_cards: [] };
	const corpus = state.chat.corpus;
	const controller = new AbortController();
	abortStream = controller;

	const stick = stickyScroll();

	try {
		await streamAnswer(state.chat.id, question, {
			onMeta: (payload) => {
				meta = payload;
				if (payload.title) {
					state.chat.title = payload.title;
					syncChrome();
					refreshHistory();
				}
			},
			onDelta: (chunk) => {
				text += chunk;
				renderAnswer(prose, text, corpus);
				stick();
			},
			onDone: () => {
				finishSage(row, bubble, prose, text, meta, corpus);
			},
			onError: (message) => {
				if (text) {
					// A partial answer is on screen and stored; note the cut-off
					// rather than discarding what the reader already has.
					finishSage(row, bubble, prose, text, meta, corpus);
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
			// Stopped on purpose: keep whatever text arrived.
			finishSage(row, bubble, prose, text || "(stopped)", meta, corpus);
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
		focusComposer();
	}
}

function stopStreaming() {
	if (abortStream) abortStream.abort();
}

function setStreaming(on) {
	state.streaming = on;
	$("send-btn").hidden = on;
	$("stop-btn").hidden = !on;
	$("composer-input").setAttribute("aria-busy", on ? "true" : "false");
}

/* ---------- Message nodes ---------- */

function userMessage(text) {
	return el("div", { class: "msg msg-user" }, el("div", { class: "bubble", text }));
}

function pendingSage() {
	const prose = el("div", { class: "prose" },
		el("span", { class: "thinking" },
			el("span", { text: "The sage consults the entries" }),
			el("span", { text: "." }), el("span", { text: "." }), el("span", { text: "." })));
	const bubble = el("div", { class: "bubble is-streaming" }, prose);
	const row = el("div", { class: "msg msg-sage" },
		el("div", { class: "who" }, el("span", { text: "✦ The Sage" })),
		bubble);
	return { row, bubble, prose };
}

function sageMessage(m) {
	const corpus = state.chat ? state.chat.corpus : activeCorpus();
	const prose = el("div", { class: "prose" });
	renderAnswer(prose, m.content, corpus);
	const bubble = el("div", { class: "bubble" }, prose);
	const row = el("div", { class: "msg msg-sage" },
		el("div", { class: "who" }, el("span", { text: "✦ The Sage" })),
		bubble);
	bindRuleRefs(prose, corpus);
	const cites = renderCitations(m.sources, m.cards, null, corpus);
	if (cites) bubble.append(cites);
	const rules = renderRulings(m.rulings);
	if (rules) bubble.append(rules);
	return row;
}

function finishSage(row, bubble, prose, text, meta, corpus) {
	renderAnswer(prose, text, corpus);
	bindRuleRefs(prose, corpus);
	bubble.classList.remove("is-streaming");
	const cites = renderCitations(meta.sources, meta.cards, meta.unresolved_cards, corpus);
	if (cites) bubble.append(cites);
	const rules = renderRulings(meta.rulings);
	if (rules) bubble.append(rules);
}

/* ---------- Composer behaviour ---------- */

function onComposerKeydown(e) {
	const hints = $("slash-hints");
	if (!hints.hidden) {
		const options = hints.querySelectorAll("li");
		const current = [...options].findIndex((li) => li.classList.contains("is-active"));
		if (e.key === "ArrowDown" || e.key === "ArrowUp") {
			e.preventDefault();
			const next = e.key === "ArrowDown"
				? (current + 1) % options.length
				: (current - 1 + options.length) % options.length;
			options.forEach((li, i) => li.classList.toggle("is-active", i === next));
			return;
		}
		if (e.key === "Tab" || (e.key === "Enter" && current >= 0)) {
			e.preventDefault();
			applySlash(options[Math.max(current, 0)].dataset.cmd);
			return;
		}
		if (e.key === "Escape") {
			hideSlashHints();
			return;
		}
	}
	// Enter sends; Shift+Enter (or a modifier) writes a newline.
	if (e.key === "Enter" && !e.shiftKey && !e.ctrlKey && !e.metaKey && !e.altKey) {
		e.preventDefault();
		submitComposer();
	}
}

function autosize(input) {
	input.style.height = "auto";
	input.style.height = Math.min(input.scrollHeight, window.innerHeight * 0.4) + "px";
}

function updateSlashHints() {
	const value = $("composer-input").value;
	const m = value.match(/^\/(\w*)$/);
	if (!m) {
		hideSlashHints();
		return;
	}
	const typed = m[1].toLowerCase();
	const cards = supportsCards(activeCorpus());
	const matches = SLASH.filter((s) =>
		(!s.mtgOnly || cards) && s.cmd.slice(1).startsWith(typed));
	if (matches.length === 0) {
		hideSlashHints();
		return;
	}
	const list = clear($("slash-hints"));
	matches.forEach((s, i) => {
		list.append(el("li", {
			class: i === 0 ? "is-active" : "",
			attrs: { "data-cmd": s.cmd, role: "option" },
			on: { click: () => applySlash(s.cmd) },
		},
			el("span", { class: "cmd", text: s.cmd }),
			el("span", { class: "cmd-desc", text: s.desc }),
		));
	});
	list.hidden = false;
}

function applySlash(cmd) {
	const input = $("composer-input");
	input.value = cmd + " ";
	hideSlashHints();
	input.focus();
	autosize(input);
}

function hideSlashHints() {
	$("slash-hints").hidden = true;
}

/* ---------- Chrome ---------- */

/** Keep the title, corpus chip, theme and per-conversation actions in sync. */
export function syncChrome() {
	const corpus = activeCorpus();
	document.documentElement.setAttribute("data-corpus", corpus);
	$("corpus-chip").textContent = corpusLabel(corpus);
	$("conv-title").textContent = state.chat ? (state.chat.title || "New chat") : "New chat";
	$("topbar-rename").hidden = !state.chat;
	$("topbar-delete").hidden = !state.chat;

	document.querySelectorAll(".corpus-opt").forEach((btn) => {
		const on = btn.dataset.corpus === state.corpus;
		btn.classList.toggle("is-active", on);
		btn.setAttribute("aria-checked", on ? "true" : "false");
	});

	$("welcome-sub").textContent = corpus === "dnd"
		? "Ask, and the tome shall answer — D&D 5e SRD."
		: "Ask, and the tome shall answer — Magic: The Gathering.";
}

/** Switching corpus starts a fresh thread: an open one is locked to its own. */
export function setCorpus(corpus) {
	state.corpus = corpus;
	saveCorpusPreference(corpus);
	if (state.chat && state.chat.corpus !== corpus) {
		startNewChat();
	} else {
		syncChrome();
		renderSuggestions();
	}
}

function renderSuggestions() {
	const wrap = clear($("suggestions"));
	for (const text of SUGGESTIONS[state.corpus] || SUGGESTIONS.mtg) {
		wrap.append(el("button", {
			class: "suggestion",
			text,
			attrs: { type: "button" },
			on: {
				click: () => {
					$("composer-input").value = text;
					autosize($("composer-input"));
					submitComposer();
				},
			},
		}));
	}
}

export function setFoot(text, warn) {
	const foot = $("composer-foot");
	foot.textContent = text || "";
	foot.classList.toggle("warn", !!warn);
}

function focusComposer() {
	if (!isNarrow()) $("composer-input").focus();
}

function scrollToBottom(instant) {
	const t = $("transcript");
	t.scrollTo({ top: t.scrollHeight, behavior: instant ? "auto" : "smooth" });
}

/**
 * Follow the stream only while the reader is already at the bottom, so
 * scrolling up to re-read something isn't yanked back down by new tokens.
 */
function stickyScroll() {
	const t = $("transcript");
	let follow = t.scrollHeight - t.scrollTop - t.clientHeight < 120;
	t.addEventListener("scroll", () => {
		follow = t.scrollHeight - t.scrollTop - t.clientHeight < 120;
	}, { passive: true });
	return () => {
		if (follow) t.scrollTop = t.scrollHeight;
	};
}
