// Study mode: a spaced-repetition flashcard surface over the rules corpus.
// The deck is generated server-side from the FTS5 index (MTG keyword abilities,
// D&D conditions); the client owns the card-by-card reveal-and-grade flow and
// hands each grade to the SM-2 scheduler on the server. It is a self-contained
// surface layered over the transcript, opened from the rail, so it does not
// entangle the Ask/Resolve chat mode machinery.

import { $, el, clear } from "./dom.js";
import { api } from "./api.js";
import { state, activeCorpus, corpusLabel } from "./state.js";

// The four SM-2 grade buttons. Each carries the slug the server expects and the
// short hint shown to the reader. The interval hint is filled in once the
// schedule for the current card is known, so the reader can choose deliberately.
const GRADES = [
	{ slug: "again", label: "Again", hint: "Forgot — review now" },
	{ slug: "hard", label: "Hard", hint: "Correct, but a struggle" },
	{ slug: "good", label: "Good", hint: "Solid recall" },
	{ slug: "easy", label: "Easy", hint: "Instant recall" },
];

let queue = [];
let cursor = 0;
let abortLoad = null;

export function isStudyOpen() {
	return state.studyOpen;
}

export function initStudy() {
	$("rail-study").addEventListener("click", () => openStudy());
	$("study-close").addEventListener("click", closeStudy);
	// Grade + reveal are wired by delegation on the study body so the handlers
	// survive re-renders without re-binding each card.
	$("study-body").addEventListener("click", onStudyBodyClick);
	// A corpus switch from the sidebar reloads the deck for the new corpus when
	// study is open, so MTG <-> D&D swaps without leaving the surface.
	document.querySelectorAll(".corpus-opt").forEach((btn) => {
		btn.addEventListener("click", () => {
			if (state.studyOpen) loadSession(btn.dataset.corpus);
		});
	});
}

/** Open the study surface for the active corpus and load a session. */
export async function openStudy(corpus) {
	state.studyOpen = true;
	const c = corpus || activeCorpus();
	setChrome(c);
	await loadSession(c);
}

/** Drop back to the transcript; a queued fetch is cancelled so it can't paint a
 *  half-loaded session over the chat after the reader left. */
export function closeStudy() {
	if (abortLoad) abortLoad.abort();
	state.studyOpen = false;
	queue = [];
	cursor = 0;
	$("study-view").hidden = true;
	$("transcript").style.display = "";
	$("composer-dock").style.display = "";
}

async function loadSession(corpus) {
	const body = clear($("study-body"));
	body.append(el("p", { class: "study-status", text: "Dealing cards…" }));
	if (abortLoad) abortLoad.abort();
	const controller = new AbortController();
	abortLoad = controller;
	try {
		const data = await api.studyQueue(corpus, 20, controller.signal);
		queue = data.cards || [];
		cursor = 0;
		renderMeta(corpus, data.stats);
		if (queue.length === 0) {
			renderEmpty(body);
			return;
		}
		renderCard();
	} catch (err) {
		if (err.name === "AbortError") return;
		clear(body);
		body.append(el("p", { class: "study-status warn", text: `Could not load study cards: ${err.message}` }));
	}
}

function onStudyBodyClick(e) {
	const reveal = e.target.closest("[data-reveal]");
	if (reveal) {
		revealCard();
		return;
	}
	const grade = e.target.closest("[data-grade]");
	if (grade) {
		gradeCard(grade.dataset.grade);
		return;
	}
	const restart = e.target.closest("[data-restart]");
	if (restart) {
		loadSession(activeCorpus());
	}
}

/* ---------- Rendering ---------- */

function renderMeta(corpus, stats) {
	$("study-title").textContent = `${corpusLabel(corpus)} — study`;
	const s = stats || {};
	const parts = [];
	if (s.total) {
		parts.push(`${s.due || 0} due · ${s.new || 0} new · ${s.learned || 0} learned · ${s.total} total`);
	}
	$("study-meta").textContent = parts.join("   ");
}

function renderEmpty(body) {
	const node = body || clear($("study-body"));
	node.append(
		el("div", { class: "study-empty" },
			el("div", { class: "study-empty-sigil", text: "📇", attrs: { "aria-hidden": "true" } }),
			el("p", { class: "study-empty-title", text: "Nothing due right now" }),
			el("p", { class: "study-empty-sub",
				text: "You're caught up on this deck — come back when the schedule brings cards back into rotation." }),
			el("button", {
				class: "suggestion",
				text: "Study ahead",
				attrs: { type: "button", "data-restart": "1", title: "Pull a session from upcoming cards" },
			}),
		),
	);
}

function currentCard() {
	return queue[cursor] || null;
}

function renderCard() {
	const card = currentCard();
	const body = clear($("study-body"));
	if (!card) {
		renderSessionComplete(body);
		return;
	}

	const progress = el("p", { class: "study-progress", text: `Card ${cursor + 1} of ${queue.length}` });

	const front = el("div", { class: "study-card is-prompt" },
		el("p", { class: "study-card-label", text: card.title || "Recall" }),
		el("div", { class: "prose study-prompt", html: escapeHtml(card.front) }),
		el("button", {
			class: "suggestion study-reveal",
			text: "Reveal answer",
			attrs: { type: "button", "data-reveal": "1" },
		}),
	);

	body.append(progress, front);

	// The answer + grade buttons are hidden until reveal so the reader commits
	// to a recall attempt before seeing the rule text.
	const back = el("div", { class: "study-answer", attrs: { hidden: "" } });
	back.append(el("div", { class: "prose study-back", html: renderBack(card) }));
	if (card.source) {
		back.append(el("p", { class: "study-source", text: card.source }));
	}
	const grades = el("div", { class: "study-grades" });
	for (const g of GRADES) {
		grades.append(el("button", {
			class: `study-grade study-grade-${g.slug}`,
			attrs: { type: "button", "data-grade": g.slug, title: g.hint },
		},
			el("span", { class: "study-grade-label", text: g.label }),
			el("span", { class: "study-grade-hint", text: intervalHint(card, g.slug) }),
		));
	}
	back.append(grades);
	body.append(back);

	$("study-body").scrollTop = 0;
}

function renderSessionComplete(body) {
	body.append(
		el("div", { class: "study-empty" },
			el("div", { class: "study-empty-sigil", text: "✓", attrs: { "aria-hidden": "true" } }),
			el("p", { class: "study-empty-title", text: "Session complete" }),
			el("p", { class: "study-empty-sub", text: "That's the end of this queue. Cards you marked Again are already back in rotation." }),
			el("button", {
				class: "suggestion",
				text: "Start another",
				attrs: { type: "button", "data-restart": "1" },
			}),
		),
	);
}

function revealCard() {
	const answer = $("study-body").querySelector(".study-answer");
	if (!answer || !answer.hidden) return;
	answer.hidden = false;
	$("study-body").querySelector(".study-reveal")?.remove();
	// Focus the default ("Good") grade so keyboard readers can grade without
	// tabbing: space/enter confirms the most common recall quality.
	$("study-body").querySelector(".study-grade-good")?.focus?.();
}

async function gradeCard(slug) {
	const card = currentCard();
	if (!card) return;
	const gradeBtns = $("study-body").querySelectorAll("[data-grade]");
	gradeBtns.forEach((b) => (b.disabled = true));
	try {
		const data = await api.studyGrade(card.key, card.corpus, card.topic, slug);
		const updated = data.card;
		if (updated) queue[cursor] = updated; // keep the freshest schedule for hints
	} catch (err) {
		gradeBtns.forEach((b) => (b.disabled = false));
		flash(`Grade failed: ${err.message}`);
		return;
	}
	cursor++;
	renderCard();
}

/* ---------- Chrome + helpers ---------- */

function setChrome(corpus) {
	$("study-view").hidden = false;
	// Layer the study surface over the transcript rather than unmounting it,
	// so returning to chat restores the open conversation exactly.
	$("transcript").style.display = "none";
	$("composer-dock").style.display = "none";
	renderMeta(corpus, null);
}

function flash(text) {
	const meta = $("study-meta");
	const prev = meta.textContent;
	meta.textContent = text;
	setTimeout(() => { meta.textContent = prev; }, 2500);
}

/** The interval a grade would schedule, shown under each button so the reader
 *  can choose deliberately. Falls back to the qualitative hint when unknown. */
function intervalHint(card, slug) {
	const project = (days) => {
		if (days <= 0) return "now";
		if (days < 1) return `${Math.round(days * 24)}h`;
		if (days === 1) return "1d";
		return `${Math.round(days)}d`;
	};
	const ease = card.ease || 2.5;
	// A rough forward-projection of SM-2: Again resets to a same-session
	// relearn; the passes multiply the current interval by the ease a grade
	// would leave it with. Close enough to help a reader pick deliberately.
	switch (slug) {
		case "again":
			return "<1m";
		case "hard":
			return project(nextInterval(card, Math.max(1.3, ease - 0.14)));
		case "good":
			return project(nextInterval(card, ease));
		case "easy":
			return project(nextInterval(card, ease + 0.1));
		default:
			return "";
	}
}

function nextInterval(card, ease) {
	const reps = (card.reps || 0) + 1;
	if (reps === 1) return 1;
	if (reps === 2) return 6;
	return Math.round((card.interval_days || 6) * ease);
}

function renderBack(card) {
	// The back is plain rule text (one rule per line); render it as pre-wrapped
	// prose so the rule numbers line up.
	return `<div class="study-back-text">${escapeHtml(card.back || "")}</div>`;
}

function escapeHtml(s) {
	return String(s == null ? "" : s)
		.replace(/&/g, "&amp;")
		.replace(/</g, "&lt;")
		.replace(/>/g, "&gt;");
}

function isNarrow() {
	return window.matchMedia("(max-width: 900px)").matches;
}
