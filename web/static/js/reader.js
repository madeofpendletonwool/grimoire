// Reading mode: the book-shaped surface. The same corpora the sage searches
// are browsable as actual books — Magic's Comprehensive Rules as chapters and
// sections, the SRD's own documents as guides, and each local D&D book (PDFs
// run through the extractor) as its own volume. A citation in the chat can
// deep-link here ("read it in the book"), and rule numbers inside a page open
// the drawer the same way they do in answers.
//
// Like study mode, this is a self-contained surface layered over the
// transcript, so leaving it restores the open conversation exactly.

import { $, el, clear, isNarrow } from "./dom.js";
import { openTool, isOpen, focusedTool } from "./wm/wm.js";
import { api } from "./api.js";
import { state, activeCorpus, corpusLabel } from "./state.js";
import { renderMarkdown } from "./markdown.js";
import { bindRuleRefs } from "./render.js";
import { gi } from "./icons.js";

let corpus = null; // the corpus whose guides are showing
let guide = ""; // selected guide id
let toc = []; // the selected guide's stops
let number = ""; // the stop currently on the page
let abort = null; // cancels a page fetch superseded by another click

function wire() {

	// Guide tabs and TOC entries are delegated so re-renders keep working.
	$("reader-guides").addEventListener("click", (e) => {
		const tab = e.target.closest("[data-guide]");
		if (!tab || tab.dataset.guide === guide) return;
		loadGuide(corpus, tab.dataset.guide);
	});
	$("reader-toc").addEventListener("click", (e) => {
		const item = e.target.closest("[data-number]");
		if (!item || item.dataset.number === number) return;
		loadPage(corpus, guide, item.dataset.number);
	});
	$("reader-page").addEventListener("click", (e) => {
		const nav = e.target.closest("[data-nav]");
		if (nav && nav.dataset.nav) loadPage(corpus, guide, nav.dataset.nav);
	});

	// A corpus switch reloads the shelf for the new corpus when reading is
	// open, the same move study mode makes.
	document.querySelectorAll(".corpus-opt").forEach((btn) => {
		btn.addEventListener("click", () => {
			if (isOpen("reader")) openReader(btn.dataset.corpus);
		});
	});

	// Page-turn keys, unless the reader is typing somewhere.
	document.addEventListener("keydown", (e) => {
		if (focusedTool() !== "reader" || e.altKey || e.ctrlKey || e.metaKey) return;
		const tag = (document.activeElement && document.activeElement.tagName) || "";
		if (tag === "INPUT" || tag === "TEXTAREA") return;
		const page = $("reader-page");
		if (e.key === "ArrowLeft") {
			const prev = page.querySelector("[data-nav-prev]");
			if (prev && prev.dataset.navPrev) { loadPage(corpus, guide, prev.dataset.navPrev); }
		} else if (e.key === "ArrowRight") {
			const next = page.querySelector("[data-nav-next]");
			if (next && next.dataset.navNext) { loadPage(corpus, guide, next.dataset.navNext); }
		}
	});

	// On a narrow screen the contents column collapses behind a toggle.
	$("reader-contents").addEventListener("click", () => {
		$("reader-view").classList.toggle("toc-open");
	});
}

/**
 * Open the reading surface. Accepts an optional target — { guide } to land on
 * a specific book, { number } to land on a stop the server resolves (a rule
 * number from a citation) — and otherwise resumes wherever the reader last
 * was in the corpus.
 */
async function openReader(c, target) {
	corpus = c || activeCorpus();
	$("reader-title").textContent = `${corpusLabel(corpus)} — read`;
	try {
		const data = await api.readerGuides(corpus);
		const guides = data.guides || [];
		if (!guides.length) {
			renderShelf(guides);
			renderEmptyShelf();
			return;
		}
		// A bare number (a citation's rule or record number) resolves through
		// the server onto the guide + stop that actually carries it.
		if (target && target.number && !target.guide) {
			try {
				const p = await api.readerPage(corpus, null, target.number);
				target = { guide: p.guide, number: p.number };
			} catch (_) {
				target = null;
			}
		}
		renderShelf(guides);
		let pick = guide;
		if (target && target.guide && guides.some((g) => g.guide === target.guide)) {
			pick = target.guide;
		} else if (!guides.some((g) => g.guide === pick)) {
			pick = guides[0].guide;
		}
		if (pick !== guide || !toc.length || target) {
			await loadGuide(corpus, pick, target);
		}
	} catch (err) {
		renderEmptyShelf(`The shelf could not be reached: ${err.message}`);
	}
}

/** Drop back to the transcript; a queued fetch is cancelled with it. */
function closeReader() {
	if (abort) abort.abort();
	$("reader-view").classList.remove("toc-open");
}

/** Load one guide: its contents, and either the target stop or the cover. */
async function loadGuide(c, g, target) {
	guide = g;
	toc = [];
	try {
		const data = await api.readerTOC(c, g);
		if (g !== guide) return; // another guide was picked mid-fetch
		toc = data.toc || [];
	} catch (err) {
		clear($("reader-page")).append(el("p", { class: "reader-note warn", text: `Could not load the contents: ${err.message}` }));
		return;
	}
	renderTOC();
	const first = firstReadable();
	let stop = target && target.number && toc.some((t) => t.number === target.number) ? target.number : first;
	if (!stop) {
		renderEmptyGuide();
		return;
	}
	loadPage(c, g, stop);
}

/** The first stop that carries text — the book's natural opening. */
function firstReadable() {
	for (const t of toc) if (t.has_body) return t.number;
	return toc.length ? toc[0].number : "";
}

async function loadPage(c, g, n) {
	if (abort) abort.abort();
	const controller = new AbortController();
	abort = controller;
	const page = $("reader-page");
	page.setAttribute("aria-busy", "true");
	try {
		const p = await api.readerPage(c, g, n, controller.signal);
		if (g !== guide) return; // the reader moved on mid-fetch
		number = p.number;
		renderPage(p);
	} catch (err) {
		if (err.name === "AbortError") return;
		clear(page).append(el("p", { class: "reader-note warn", text: `The page could not be opened: ${err.message}` }));
	} finally {
		page.removeAttribute("aria-busy");
	}
}

/* ---------- Rendering ---------- */

/** The guide tabs, one per book; a single book renders nothing (lone-tab noise). */
function renderShelf(guides) {
	const wrap = clear($("reader-guides"));
	if (guides.length < 2) {
		wrap.hidden = true;
		if (guides.length === 1) guide = guides[0].guide;
		return;
	}
	wrap.hidden = false;
	for (const g of guides) {
		wrap.append(el("button", {
			class: "reader-guide" + (g.guide === guide ? " is-active" : ""),
			text: g.title,
			attrs: {
				type: "button",
				"data-guide": g.guide,
				role: "tab",
				"aria-selected": g.guide === guide ? "true" : "false",
				title: g.kind === "book" ? "A local book" : (g.kind === "srd" ? "From the 5e SRD" : "The corpus's own rulebook"),
			},
		}));
	}
}

/** The contents column. Stops indent by level; the active stop is marked. */
function renderTOC() {
	const wrap = clear($("reader-toc"));
	if (!toc.length) {
		wrap.append(el("p", { class: "reader-note", text: "No contents." }));
		return;
	}
	for (const t of toc) {
		wrap.append(el("button", {
			class: "reader-toc-item" +
				(t.level === 1 ? " is-chapter" : "") +
				(t.number === number ? " is-active" : ""),
			text: t.title,
			attrs: {
				type: "button",
				"data-number": t.number,
				style: `--toc-level: ${t.level};`,
				"aria-current": t.number === number ? "true" : undefined,
			},
		}));
	}
}

/** One page: crumbs, the heading, the body, and the page-turn footer. */
function renderPage(p) {
	const view = clear($("reader-page"));

	const crumbs = el("nav", { class: "reader-crumbs", attrs: { "aria-label": "Ancestors" } });
	(p.crumbs || []).forEach((c, i) => {
		if (i) crumbs.append(el("span", { class: "reader-crumb-sep", text: "›" }));
		crumbs.append(el("button", {
			class: "reader-crumb",
			text: c.title,
			attrs: { type: "button", "data-number": c.number, title: "Open in the contents" },
			on: { click: () => loadPage(corpus, guide, c.number) },
		}));
	});

	const head = el("h3", { class: "reader-heading", text: p.title });
	view.append(crumbs, head);

	const body = el("div", { class: "prose reader-prose" });
	const mtg = p.corpus === "mtg";
	body.innerHTML = renderMarkdown(p.body || "", { rules: mtg, mana: mtg });
	// Rule numbers inside Magic pages open the drawer, as they do in answers.
	bindRuleRefs(body, p.corpus);
	view.append(body);

	if (p.source) {
		view.append(el("p", { class: "reader-source", text: p.source }));
	}

	const foot = el("div", { class: "reader-turn" });
	foot.append(turnBtn(p.prev, "reader-turn-prev", "Previous", "←", "data-nav-prev"));
	foot.append(turnBtn(p.next, "reader-turn-next", "Next", "→", "data-nav-next"));
	view.append(foot);

	// Mark the contents entry, and bring it into view when the column scrolls.
	renderTOC();
	$("reader-toc").querySelector(".is-active")?.scrollIntoView({ block: "nearest" });
	view.scrollTop = 0;
	view.focus({ preventScroll: true });
}

function turnBtn(stop, cls, label, arrow, navAttr) {
	const btn = el("button", {
		class: `reader-turn-btn ${cls}`,
		attrs: { type: "button", title: stop ? `${label}: ${stop.title}` : label, [navAttr]: stop ? stop.number : "" },
	}, el("span", { class: "reader-turn-arrow", text: arrow }), el("span", { text: stop ? stop.title : label }));
	if (!stop) btn.disabled = true;
	return btn;
}

function renderEmptyShelf(detail) {
	renderShelf([]);
	clear($("reader-toc")).append(el("p", { class: "reader-note", text: "" }));
	clear($("reader-page")).append(
		el("div", { class: "reader-empty" },
			gi("no-results", { cls: "gi-xl" }),
			el("p", { class: "reader-empty-title", text: "No books on the shelf" }),
			el("p", { class: "reader-empty-sub",
				text: detail || "This corpus hasn't been shaped into a book yet — rebuild the index and the rules arrive as a readable volume." }),
		),
	);
}

function renderEmptyGuide() {
	clear($("reader-page")).append(
		el("div", { class: "reader-empty" },
			gi("no-results", { cls: "gi-xl" }),
			el("p", { class: "reader-empty-title", text: "Nothing readable here" }),
			el("p", { class: "reader-empty-sub", text: "This volume's contents came through empty." }),
		),
	);
}

/* ---------- the window-manager contract ---------- */

// mount() adopts the surface that already exists in index.html rather than
// building one, which is what made migrating nine surfaces tractable: every
// $("reader-toc") lookup inside this module keeps working untouched. The
// cost is one window per tool; see the note on `instances` in wm/registry.js.
let wired = false;

export const tool = {
	mount(host) {
		const view = $("reader-view");
		host.append(view);
		view.hidden = false;
		if (!wired) {
			wire();
			wired = true;
		}
		flushRead();
		return { destroy: closeReader };
	},
};

/**
 * Open the reading surface at a citation — the drawer's "Read this in the
 * book". The target is parked rather than passed, because opening the tool may
 * mount it, and mount() runs the load itself; handing it a pending target
 * means the page is fetched once whether the window was already up or not.
 */
let pendingRead = null;

export function readAt(corpus, target) {
	pendingRead = { corpus, target };
	const already = isOpen("reader");
	openTool("reader");
	if (already) flushRead();
}

function flushRead() {
	const p = pendingRead;
	pendingRead = null;
	return openReader(p?.corpus, p?.target);
}
