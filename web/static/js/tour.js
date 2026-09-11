// The shell tour: five steps over the chrome, and nothing else.
//
// Scope is the whole design here. The Guide teaches the *work* — found a
// campaign, decide what is canon, run a session — and it does that from a
// window, beside the tool in question. What a window cannot teach is the
// chrome it is sitting in: that Alt+2 is a different room, that Ctrl+G opens
// anything, that the ⋯ on a titlebar splits it. Those have no empty state to
// talk from and no home in any tool, because they *are* the shell. So they
// get an overlay, and they are the only things that do.
//
// Two decisions worth knowing before editing:
//
// The hole is four rectangles, not one big `box-shadow` spread. A shadow
// covers its own target, so the workspace tab this is pointing at could not
// be clicked while the tour explained it. Four scrim rects leave the target
// genuinely uncovered and hit-testable.
//
// Escape is not ours to catch. keys.js binds the document in the capture
// phase at boot (keys.js:166), so a listener added later cannot win the key
// however it stops propagation. close() joins the shell's own dismiss chain
// instead, at the top — this scrims everything, so nothing is above it.

import { $, el, isNarrow } from "./dom.js";
import { pushModal, popModal, pretty } from "./wm/keys.js";

// Each anchor is a thunk, never a captured node: the strip is rebuilt on
// every workspace change and swaps shape across the narrow threshold, so a
// node held from open() would be detached by the time it was measured.
const STEPS = [
	{
		id: "workspaces",
		title: "Workspaces",
		body: () => `Each tab is a saved arrangement of windows — prep in one, the table in another. ${pretty("alt+1")} to ${pretty("alt+9")} switch between them, and the ⋯ renames or resets one. Your layout is saved as you arrange it.`,
		anchor: () => (isNarrow() ? document.querySelector(".wm-ws-picker") : $("wm-strip")),
	},
	{
		id: "tools",
		title: "Every tool lives here",
		body: () => `${pretty("mod+g")} then a letter opens any tool — then U comes back to the Guide. Nothing is buried in a menu; this list is the whole app.`,
		anchor: () => (isNarrow() ? $("topbar-commands") : $("rail-tools-btn")),
	},
	{
		id: "window",
		title: "The window menu",
		body: "Split a window beside another, move it, zoom it, or gather several into tabs. Windows are yours to arrange — the app does not close one on your behalf.",
		anchor: () => document.querySelector(".wm-window.is-focused .wm-controls") || document.querySelector(".wm-controls"),
	},
	{
		id: "search",
		title: "Search the rules",
		body: () => `${pretty("mod+k")} searches the rule text and the cards directly — the fastest way to settle a question mid-session without asking anyone.`,
		anchor: () => $("topbar-search"),
	},
	{
		id: "keys",
		title: "Everything else",
		body: "The full keyboard map lives here, and it knows which seat you are in — it lists only what you can actually open.",
		anchor: () => $("wm-help"),
	},
];

/** The steps whose chrome exists right now. A step with no target is not a
    step: at narrow widths the rail is collapsed and some of this is simply
    not on screen, and pointing at (0,0) would be worse than saying nothing. */
export function tourSteps() {
	return STEPS.filter((step) => {
		try {
			return !!step.anchor();
		} catch (_) {
			return false;
		}
	});
}

let layer = null;
let steps = [];
let at = 0;
let lastFocus = null;
let onResize = null;

export const isTourOpen = () => !!layer;

/* ---------- geometry ---------- */

const PAD = 6;   // the brackets frame the chrome rather than sitting on it

function place() {
	if (!layer) return;
	const target = steps[at]?.anchor();
	if (!target) return close();

	const r = target.getBoundingClientRect();
	const box = {
		top: Math.max(0, r.top - PAD),
		left: Math.max(0, r.left - PAD),
		width: r.width + PAD * 2,
		height: r.height + PAD * 2,
	};

	const ring = layer.querySelector(".tour-ring");
	Object.assign(ring.style, {
		top: `${box.top}px`, left: `${box.left}px`,
		width: `${box.width}px`, height: `${box.height}px`,
	});

	// Four scrim rects around the hole, so the target itself stays live.
	const cuts = layer.querySelectorAll(".tour-cut");
	const W = window.innerWidth;
	const H = window.innerHeight;
	const rects = [
		{ top: 0, left: 0, width: W, height: box.top },
		{ top: box.top + box.height, left: 0, width: W, height: Math.max(0, H - box.top - box.height) },
		{ top: box.top, left: 0, width: box.left, height: box.height },
		{ top: box.top, left: box.left + box.width, width: Math.max(0, W - box.left - box.width), height: box.height },
	];
	rects.forEach((rect, i) => Object.assign(cuts[i].style, {
		top: `${rect.top}px`, left: `${rect.left}px`,
		width: `${rect.width}px`, height: `${rect.height}px`,
	}));

	// The card sits below the target, or above it when there is no room.
	const card = layer.querySelector(".tour-card");
	card.style.removeProperty("bottom");
	const below = box.top + box.height + 12;
	const cardH = card.offsetHeight || 200;
	const top = below + cardH > H ? Math.max(12, box.top - cardH - 12) : below;
	card.style.top = `${top}px`;
	const left = Math.min(Math.max(12, box.left), Math.max(12, W - card.offsetWidth - 12));
	card.style.left = `${left}px`;
}

/* ---------- render ---------- */

function paint() {
	const step = steps[at];
	layer.querySelector(".tour-step").textContent = `${at + 1} of ${steps.length}`;
	layer.querySelector(".tour-title").textContent = step.title;
	layer.querySelector(".tour-body").textContent =
		typeof step.body === "function" ? step.body() : step.body;
	layer.querySelector(".tour-back").disabled = at === 0;
	layer.querySelector(".tour-next").textContent = at === steps.length - 1 ? "Done" : "Next";
	place();
}

function go(delta) {
	const next = at + delta;
	if (next < 0) return;
	if (next >= steps.length) return close();
	at = next;
	paint();
}

/* ---------- open / close ---------- */

export function openTour() {
	if (layer) return true;
	steps = tourSteps();
	if (!steps.length) return false;
	at = 0;
	lastFocus = document.activeElement;

	layer = el("div", {
		class: "tour-layer",
		attrs: { role: "dialog", "aria-modal": "true", "aria-labelledby": "tour-title" },
	});
	for (let i = 0; i < 4; i++) layer.append(el("div", { class: "tour-cut" }));
	layer.append(el("div", { class: "tour-ring f-select", attrs: { "aria-hidden": "true" } }));

	const card = el("div", { class: "tour-card f-stone" },
		el("p", { class: "tour-step" }),
		el("h2", { class: "tour-title", attrs: { id: "tour-title" } }),
		el("p", { class: "tour-body" }),
	);
	card.append(el("div", { class: "tour-actions" },
		el("button", {
			class: "enc-btn tour-skip", attrs: { type: "button" },
			on: { click: () => close() },
		}, el("span", { text: "Skip" })),
		el("button", {
			class: "enc-btn tour-back", attrs: { type: "button" },
			on: { click: () => go(-1) },
		}, el("span", { text: "Back" })),
		el("button", {
			class: "enc-btn primary tour-next", attrs: { type: "button" },
			on: { click: () => go(1) },
		}, el("span", { text: "Next" })),
	));
	layer.append(card);

	// A trap scoped to one component. There is no shell-wide focus trap to
	// reuse — pushModal only bumps a layer counter — and three buttons do not
	// justify inventing one.
	layer.addEventListener("keydown", (e) => {
		if (e.key === "Tab") {
			const btns = [...layer.querySelectorAll("button:not([disabled])")];
			if (!btns.length) return;
			const i = btns.indexOf(document.activeElement);
			e.preventDefault();
			btns[(i + (e.shiftKey ? -1 : 1) + btns.length) % btns.length].focus();
		} else if (e.key === "ArrowRight") {
			e.preventDefault();
			go(1);
		} else if (e.key === "ArrowLeft") {
			e.preventDefault();
			go(-1);
		}
	});

	document.body.append(layer);
	pushModal();
	paint();
	// paint() measures, and place() closes the tour if the chrome it was
	// about to frame has gone — a workspace re-render between filtering the
	// steps and drawing the first one is enough. Closing leaves no layer to
	// focus, so check before reaching through it.
	if (!layer) return false;
	layer.querySelector(".tour-next").focus();

	onResize = () => place();
	window.addEventListener("resize", onResize);
	window.addEventListener("scroll", onResize, true);
	return true;
}

/**
 * Close the tour. Returns false when none was open, so it can head the
 * shell's dismiss chain the way closePrompt and closeMenu do.
 */
export function closeTour() {
	if (!layer) return false;
	window.removeEventListener("resize", onResize);
	window.removeEventListener("scroll", onResize, true);
	onResize = null;
	layer.remove();
	layer = null;
	popModal();
	steps = [];
	// Put focus back where it was. The window manager tracks focus inside
	// #wm-root (wm.js:88), so dropping it here would leave the next Alt+arrow
	// moving the wrong window.
	if (lastFocus?.isConnected) lastFocus.focus();
	lastFocus = null;
	return true;
}

const close = () => closeTour();
