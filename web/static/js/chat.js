// The conversation: transcript, composer, streaming, and saved history.

import { openTool } from "./wm/wm.js";
import { $, el, clear, isNarrow } from "./dom.js";
import { api, streamAnswer } from "./api.js";
import { state, activeCorpus, corpusLabel, supportsCards, saveCorpusPreference } from "./state.js";
import { renderAnswer, bindRuleRefs, renderCitations, renderRulings, verifyRuleRefs, copyAnswerButton, splitFollowUps, renderFollowUps } from "./render.js";
import { openRule, openCard, closeDrawer } from "./drawer.js";
import { openPalette } from "./palette.js";
import { syncModeChrome, renderWelcome, isResolveMode, walkFromQuestion } from "./resolve.js";
import { shareButton } from "./shares.js";
import { sprite, gi } from "./icons.js";

let abortStream = null;

// The open conversation as plain turns. The resolver handoff transcribes a
// scenario that is usually built across several messages ("...and the
// equipment gives it hexproof"), so it needs the turns, not just the last one.
let transcript = [];

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

function wire() {
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
	$("topbar-rename").addEventListener("click", renameCurrent);
	$("topbar-delete").addEventListener("click", deleteCurrent);

	renderSuggestions();
}

/* ---------- Conversation lifecycle ---------- */

export function startNewChat() {
	// Reachable from the rail while the chat window is closed, so open it
	// first: otherwise the conversation loads into markup sitting in the
	// stash, and the click does nothing visible.
	openTool("chat");
	if (state.streaming) stopStreaming();
	state.mode = "ask"; // "New chat" is a saved conversation; leave resolve mode.
	state.chat = null;
	transcript = [];
	closeDrawer();
	clear($("messages"));
	$("welcome").hidden = false;
	syncChrome();
	renderSuggestions();
	highlightHistory();
	focusComposer();
}

export async function openChat(id) {
	openTool("chat");   // the sidebar history reaches here with no window up
	if (state.streaming) stopStreaming();
	try {
		const data = await api.getChat(id);
		state.chat = data.chat;
		transcript = [];
		$("welcome").hidden = true;
		const list = clear($("messages"));
		let lastQuestion = "";
		for (const m of data.messages || []) {
			if (m.role === "user") lastQuestion = m.content;
			list.append(m.role === "user" ? userMessage(m.content) : sageMessage(m, lastQuestion));
			transcript.push({ role: m.role, content: m.content });
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
		nav.append(el("p", { class: "history-empty" },
			gi("no-history", { cls: "gi-xl" }),
			el("span", { text: "No saved chats yet." })));
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
			el("span", { class: "h-mark" }, gi(c.corpus === "dnd" ? "dnd" : "mtg")),
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
	transcript.push({ role: "user", content: question });

	const { row, bubble, prose } = pendingSage();
	list.append(row);
	scrollToBottom();

	setStreaming(true);
	let text = "";
	let meta = { sources: [], cards: [], entities: [], rulings: [], unresolved_cards: [] };
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
			// The sage went back to the index mid-answer. Say what it is
			// consulting, so the pause reads as work rather than a stall.
			onLookup: (tool, arg) => {
				showLookup(bubble, tool, arg);
				stick();
			},
			// Rules fetched during the answer are citations too; the meta
			// frame went out before the first token and could not carry them.
			onSources: (extra) => {
				meta = { ...meta, sources: [...(meta.sources || []), ...extra] };
			},
			onDone: (payload) => {
				finishSage(row, bubble, prose, text, meta, corpus, question, payload.message_id);
			},
			onError: (message) => {
				if (text) {
					// A partial answer is on screen and stored; note the cut-off
					// rather than discarding what the reader already has.
					finishSage(row, bubble, prose, text, meta, corpus, question);
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
			finishSage(row, bubble, prose, text || "(stopped)", meta, corpus, question);
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
			sprite("casting", { cls: "ico-casting" }),
			el("span", { text: "The sage consults the entries…" })));
	const bubble = el("div", { class: "bubble is-streaming" }, prose);
	const row = el("div", { class: "msg msg-sage" }, sageWho(), bubble);
	return { row, bubble, prose };
}

/** The byline over an answer: the sage's staff, then the name. */
function sageWho() {
	return el("div", { class: "who" }, sprite("staff"), el("span", { text: "The Sage" }));
}

function sageMessage(m, question) {
	const corpus = state.chat ? state.chat.corpus : activeCorpus();
	const prose = el("div", { class: "prose" });
	renderAnswer(prose, m.content, corpus);
	const bubble = el("div", { class: "bubble" }, prose);
	const row = el("div", { class: "msg msg-sage" }, sageWho(), bubble);
	bindRuleRefs(prose, corpus);
	const cites = renderCitations(m.sources, m.cards, m.entities, null, corpus);
	if (cites) bubble.append(cites);
	const rules = renderRulings(m.rulings);
	if (rules) bubble.append(rules);
	const { body, followUps } = splitFollowUps(m.content);
	const branches = renderFollowUps(followUps, askFollowUp);
	if (branches) bubble.append(branches);
	attachActions(row, prose, body, corpus, m.id, question || "");
	return row;
}

// finishSage seals a streamed answer. messageID is the stored assistant
// message id from the done event — zero when the stream never completed, in
// which case there is nothing durable to share.
function finishSage(row, bubble, prose, text, meta, corpus, question, messageID) {
	clearLookup(bubble);
	const { body, followUps } = splitFollowUps(text);
	renderAnswer(prose, text, corpus);
	bindRuleRefs(prose, corpus);
	bubble.classList.remove("is-streaming");
	const cites = renderCitations(meta.sources, meta.cards, meta.entities, meta.unresolved_cards, corpus);
	if (cites) bubble.append(cites);
	const rules = renderRulings(meta.rulings);
	if (rules) bubble.append(rules);
	const branches = renderFollowUps(followUps, askFollowUp);
	if (branches) bubble.append(branches);
	transcript.push({ role: "assistant", content: body });
	attachActions(row, prose, body, corpus, messageID, question);
}

/**
 * The actions under an answer: copy, and a share link once the turn is stored.
 *
 * The copy action waits on verification because it quotes the rules it cites,
 * and those come back from the same check that marks the references — one
 * request serving both.
 */
function attachActions(row, prose, body, corpus, messageID, question) {
	const actions = el("div", { class: "msg-actions" });
	let checks = [];
	actions.append(copyAnswerButton(() => body, () => checks));
	if (describesInteraction(corpus, question)) {
		actions.append(el("button", {
			class: "msg-action",
			text: "Walk the stack",
			attrs: { type: "button", "aria-label": "Walk this interaction step by step in the resolver" },
			on: { click: () => walkFromQuestion(question, turnsBefore(question)) },
		}));
	}
	if (state.chat && messageID) {
		const share = shareButton(state.chat.id, messageID);
		if (share) actions.append(share);
	}
	row.append(actions);
	verifyRuleRefs(prose, corpus).then((result) => { checks = result; });
}

// The turns that came before one question, so the resolver transcribes the
// board as it stood when that question was asked rather than as the rest of
// the conversation later left it.
function turnsBefore(question) {
	for (let i = transcript.length - 1; i >= 0; i--) {
		if (transcript[i].role === "user" && transcript[i].content === question) {
			return transcript.slice(0, i);
		}
	}
	return transcript.slice();
}

// The resolver only has something to say about a question that describes
// things happening in an order. "What does hexproof mean" has no stack to
// walk; "can I equip in response to their instant" is a board and a sequence
// written as a sentence, and that is the case the handoff exists for.
const INTERACTION_RE = /\b(respond|response|stack|trigger(s|ed|ing)?|resolv(e|es|ed|ing)|counter(s|ed)?|target(s|ed|ing)?|attack(s|ing)?|block(s|ing)?|priority|sacrific|before|after|first|then|while)\b/i;

function describesInteraction(corpus, question) {
	return corpus === "mtg" && !!question && INTERACTION_RE.test(question);
}

/** Ask one of the sage's suggested follow-ups, as though it were typed. */
function askFollowUp(question) {
	const input = $("composer-input");
	input.value = question;
	autosize(input);
	submitComposer();
}

/** A note under the streaming answer naming the rule being fetched. */
function showLookup(bubble, tool, arg) {
	let note = bubble.querySelector(".lookup-note");
	if (!note) {
		note = el("div", { class: "lookup-note" });
		bubble.append(note);
	}
	const what = tool === "lookup_rule" ? `rule ${arg}` : `"${arg}"`;
	note.textContent = `Consulting ${what}…`;
}

function clearLookup(bubble) {
	bubble.querySelector(".lookup-note")?.remove();
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
	clear($("corpus-chip")).append(
		gi(corpus === "dnd" ? "dnd" : "mtg"),
		el("span", { text: corpusLabel(corpus) }),
	);
	$("conv-title").textContent = state.chat ? (state.chat.title || "New chat") : "New chat";
	$("topbar-rename").hidden = !state.chat;
	$("topbar-delete").hidden = !state.chat;

	document.querySelectorAll(".corpus-opt").forEach((btn) => {
		const on = btn.dataset.corpus === state.corpus;
		btn.classList.toggle("is-active", on);
		btn.setAttribute("aria-checked", on ? "true" : "false");
	});

	// Mode chrome (Ask/Resolve toggle + composer swap) follows corpus + mode.
	syncModeChrome();

	if (!isResolveMode()) {
		$("welcome-sub").textContent = corpus === "dnd"
			? "Ask, and the tome shall answer — D&D 5e SRD."
			: "Ask, and the tome shall answer — Magic: The Gathering.";
	}
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
	if (isResolveMode()) {
		renderWelcome(); // resolver owns its own puzzle suggestions
		return;
	}
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

/* ---------- the window-manager contract ---------- */

// Chat is a tool like the rest now, rather than the page every other surface
// covered up. Its transcript and composer travel together into the window;
// the conversation's own header (title, corpus chip, Ask/Resolve) went with
// them, because that header describes a conversation, not the application.
let wired = false;

export const tool = {
	mount(host) {
		const view = $("chat-view");
		host.append(view);
		view.hidden = false;
		if (!wired) {
			wire();
			wired = true;
		}
		syncChrome();
		if (!isNarrow()) focusComposer();
		// Streaming is deliberately not aborted on close: a window closed
		// mid-answer should still have the answer waiting when it is reopened,
		// which is the opposite of the surfaces that abort a search.
		return { destroy() {} };
	},
};
