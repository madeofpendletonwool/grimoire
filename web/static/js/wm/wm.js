// The window manager: renders a layout tree, owns focus, mounts tools.
//
// tree.js decides *what the layout is*; this file decides *what the screen
// looks like*. The split is load-bearing — the tree is testable because it
// never touches the document, and it only stays that way if the rendering
// lives here (ADR 14).
//
// One rule shapes everything below: **a window element is created once and
// reused forever**. A structural change rebuilds the scaffolding — splits,
// gutters, tab strips — but the <section class="wm-window"> for a leaf is
// moved into the new scaffolding, never recreated. Recreating it would tear
// down the tool inside, which is a live conversation, an open editor, or a
// stream in flight.

import { $, el, clear, isNarrow } from "../dom.js";
import { sprite, gi } from "../icons.js";
import * as T from "./tree.js";
import { toolDef, knownTool } from "./registry.js";

/* ---------- state ---------- */

const wm = {
	root: null,        // the tree, or null for an empty workspace
	focus: null,       // id of the focused leaf
	corpus: "mtg",
	mounted: new Map(), // leaf id -> { el, tool, handle, bodyEl }
	listeners: new Set(),
	rendering: false,
};

/** Subscribe to structural change. persist.js saves from here, debounced. */
export function onChange(fn) {
	wm.listeners.add(fn);
	return () => wm.listeners.delete(fn);
}

function changed() {
	for (const fn of wm.listeners) {
		try {
			fn(snapshot());
		} catch (err) {
			console.error("wm listener failed:", err);
		}
	}
}

/** What persistence stores: the tree, nothing about the DOM. */
export const snapshot = () => ({ tree: T.serialize(wm.root), focus: focusedTool() });

export const currentTree = () => wm.root;
export const focusedID = () => wm.focus;
export const focusedTool = () => {
	const leaf = wm.focus && T.find(wm.root, wm.focus);
	return leaf ? leaf.tool : null;
};
export const openTools = () => T.toolsIn(wm.root);
/** Is this tool already on screen? Navigation helpers need to know whether
    opening it will mount it (and run its own load) or merely focus it. */
export const isOpen = (tool) => !!T.findByTool(wm.root, tool);

/* ---------- lifecycle ---------- */

export function initWM(corpus) {
	wm.corpus = corpus;
	const host = $("wm-root");
	host.addEventListener("click", onHostClick);
	// Focus follows the click into a tool's own controls too, so typing in a
	// window's input makes that window the keyboard target.
	host.addEventListener("focusin", (e) => {
		const win = e.target.closest?.(".wm-window");
		if (win && win.dataset.node !== wm.focus) setFocus(win.dataset.node, { render: true });
	});
	window.addEventListener("resize", onViewportChange);
}

/**
 * Replace the whole layout — switching workspace, or loading a saved one.
 * Tools that survive the swap keep their mounted state; the rest are torn down.
 */
export function setLayout(tree, { focusTool = null } = {}) {
	wm.root = tree;
	reconcileMounts();

	const wanted = focusTool && T.findByTool(wm.root, focusTool);
	wm.focus = wanted ? wanted.id : (T.firstLeaf(wm.root)?.id ?? null);
	render();
	changed();
}

export function setCorpusScope(corpus) {
	wm.corpus = corpus;
}

/* ---------- opening and closing ---------- */

/**
 * Open a tool. Singleton tools focus their existing window instead of opening
 * a second one — see the note on `instances` in registry.js.
 */
export function openTool(id, { how = "row-after" } = {}) {
	if (!knownTool(id)) return null;

	const existing = T.findByTool(wm.root, id);
	if (existing) {
		revealLeaf(existing.id);
		return existing.id;
	}

	const leaf = T.leaf(id);
	wm.root = wm.root ? T.insert(wm.root, wm.focus ?? T.firstLeaf(wm.root).id, leaf, how) : leaf;
	wm.focus = leaf.id;
	reconcileMounts();
	render();
	changed();
	return leaf.id;
}

export function closeWindow(id = wm.focus) {
	if (!id || !T.find(wm.root, id)) return;

	// Pick the successor before the tree forgets where the window was, so
	// focus lands somewhere adjacent rather than jumping to the first leaf.
	// A sibling in the same container comes first: closing one of three tabs
	// should leave you on the next tab, not throw you out of the container —
	// and focusDir treats a tabbed container as opaque, by design.
	const next = siblingOf(id)
		?? T.focusDir(wm.root, id, "right")
		?? T.focusDir(wm.root, id, "left")
		?? T.focusDir(wm.root, id, "down")
		?? T.focusDir(wm.root, id, "up");

	wm.root = T.remove(wm.root, id);
	wm.focus = (next && T.find(wm.root, next)) ? next : (T.firstLeaf(wm.root)?.id ?? null);
	reconcileMounts();
	render();
	changed();
}

/** The neighbour inside the same container: the next one along, or the
    previous when the window being closed is the last. */
function siblingOf(id) {
	const parent = T.parentOf(wm.root, id);
	if (!parent) return null;
	const i = parent.kids.findIndex((k) => k.id === id);
	const neighbour = parent.kids[i + 1] ?? parent.kids[i - 1];
	return neighbour ? T.firstLeaf(neighbour)?.id ?? null : null;
}

/** Close every window — "reset this workspace". */
export function closeAll() {
	wm.root = null;
	wm.focus = null;
	reconcileMounts();
	render();
	changed();
}

/* ---------- structural commands ---------- */

export function splitFocused(dir, tool) {
	if (!wm.focus) return openTool(tool);
	const id = openTool(tool, { how: `${dir}-after` });
	return id;
}

export function toggleTabsOnFocused() {
	const parent = wm.focus && T.parentOf(wm.root, wm.focus);
	if (!parent) return;
	wm.root = T.toggleContainer(wm.root, parent.id);
	render();
	changed();
}

export function moveFocus(dir) {
	const next = T.focusDir(wm.root, wm.focus, dir);
	if (next) revealLeaf(next);
}

export function moveWindow(dir) {
	if (!wm.focus) return;
	const next = T.moveDir(wm.root, wm.focus, dir);
	if (next === wm.root) return;
	wm.root = next;
	render();
	changed();
}

export function cycleTab(step) {
	const parent = wm.focus && T.parentOf(wm.root, wm.focus);
	if (!parent || parent.t !== "tabs") return;
	const i = parent.kids.findIndex((k) => k.id === wm.focus);
	const next = (i + step + parent.kids.length) % parent.kids.length;
	wm.root = T.setActive(wm.root, parent.id, next);
	wm.focus = T.firstLeaf(parent.kids[next])?.id ?? wm.focus;
	render();
	changed();
}

export function resizeSplit(splitID, gutter, delta) {
	const next = T.resize(wm.root, splitID, gutter, delta);
	if (next === wm.root) return;
	wm.root = next;
	applyFractions();   // cheap: no rebuild, so a drag stays smooth
}

/** Persist after a drag ends, rather than on every pointer move. */
export const commitResize = () => changed();

/* ---------- focus ---------- */

export function setFocus(id, { render: doRender = true } = {}) {
	if (!id || id === wm.focus || !T.find(wm.root, id)) return;
	wm.focus = id;
	if (doRender) paintFocus();
}

/**
 * Focus a leaf and make sure it is actually visible — a leaf inside a tabbed
 * container has to become the active tab first, or focus would land on
 * something the user cannot see.
 */
function revealLeaf(id) {
	const path = T.pathTo(wm.root, id);
	if (!path) return;
	let structural = false;
	for (let i = 0; i < path.length - 1; i++) {
		const node = path[i];
		if (node.t !== "tabs") continue;
		const want = node.kids.indexOf(path[i + 1]);
		if (want >= 0 && want !== node.active) {
			wm.root = T.setActive(wm.root, node.id, want);
			structural = true;
		}
	}
	wm.focus = id;
	if (structural) {
		render();
		changed();
	} else {
		paintFocus();
	}
	focusBody(id);
}

function focusBody(id) {
	const rec = wm.mounted.get(id);
	if (!rec) return;
	// Prefer the tool's own first control; fall back to the window itself so
	// keyboard commands still have a target.
	const target = rec.bodyEl.querySelector("input, textarea, [tabindex]:not([tabindex='-1'])") || rec.el;
	try {
		target.focus({ preventScroll: true });
	} catch (_) { /* nothing focusable is fine */ }
}

/* ---------- mounting ---------- */

/**
 * Bring mounted tools in line with the tree: mount what appeared, destroy what
 * left. Destroying is what finally makes closing a surface abort its in-flight
 * requests — closeSessions, closePlanner, closeReview and closeCampaign never
 * did, so a close mid-load left a fetch running against a dead view.
 */
function reconcileMounts() {
	const live = new Set(T.leaves(wm.root).map((l) => l.id));

	for (const [id, rec] of wm.mounted) {
		if (live.has(id)) continue;
		try {
			rec.handle?.destroy?.();
		} catch (err) {
			console.error(`tool ${rec.tool} destroy failed:`, err);
		}
		restash(rec);
		rec.el.remove();
		wm.mounted.delete(id);
	}

	for (const leaf of T.leaves(wm.root)) {
		if (!wm.mounted.has(leaf.id)) createWindow(leaf);
	}
}

/**
 * Return a tool's markup to the stash before its window is destroyed.
 *
 * A tool mounts by *moving* its <section> out of #wm-stash, so removing the
 * window element would take that markup with it — and with it every id the
 * module addresses, permanently. Putting it back also means a reopened tool
 * finds its board still loaded and its fields still filled.
 */
function restash(rec) {
	if (!rec.adopted?.isConnected) return;
	rec.adopted.hidden = true;
	$("wm-stash").append(rec.adopted);
}

function createWindow(leaf) {
	const def = toolDef(leaf.tool);
	const body = el("div", { class: "wm-body" });

	const node = el("section", {
		class: "wm-window f-stone",
		attrs: { "data-node": leaf.id, "data-tool": leaf.tool, tabindex: "-1", "aria-label": def?.title || leaf.tool },
	}, titlebar(leaf, def), body);

	const rec = { el: node, bodyEl: body, tool: leaf.tool, handle: null, adopted: null };
	wm.mounted.set(leaf.id, rec);

	// Tools load on first open rather than all at boot, which is why the
	// registry stores a dynamic import rather than a static one.
	if (!def) {
		body.append(el("p", { class: "wm-error", text: `No tool named “${leaf.tool}”.` }));
		return rec;
	}
	body.append(el("p", { class: "wm-loading", text: "Opening…" }));

	def.load().then((mod) => {
		if (wm.mounted.get(leaf.id) !== rec) return; // closed while loading
		clear(body);
		if (!mod.tool?.mount) {
			body.append(el("p", { class: "wm-error", text: `${def.title} has not been migrated to the window manager yet.` }));
			return;
		}
		rec.handle = mod.tool.mount(body, { id: leaf.id, corpus: wm.corpus, retitle: (t) => retitle(leaf.id, t) });
		// Whatever the tool moved in is what has to go back out on close.
		rec.adopted = body.firstElementChild || null;
		retitle(leaf.id, rec.handle?.title?.());
	}).catch((err) => {
		console.error(`tool ${leaf.tool} failed to load:`, err);
		if (wm.mounted.get(leaf.id) !== rec) return;
		clear(body).append(el("p", { class: "wm-error", text: `${def.title} could not be opened.` }));
	});

	return rec;
}

function titlebar(leaf, def) {
	const controls = el("div", { class: "wm-controls" },
		iconBtn("tab", "Tab this window with its neighbour", "menu"),
		iconBtn("close", "Close window", "close"),
	);
	return el("header", { class: "wm-titlebar f-titleplate" },
		def ? safeSprite(def.icon) : el("span"),
		el("h2", { class: "wm-title", text: def?.title || leaf.tool }),
		controls,
	);
}

// sprite() and gi() throw on an unknown name. A registry typo must cost an
// icon, not the whole render — the surrounding control keeps its aria-label,
// so an empty slot stays operable (DESIGN.md invariant 8).
function safeSprite(name, opts) {
	try {
		return sprite(name, opts);
	} catch (err) {
		console.error(err);
		return el("span", { class: "ico" });
	}
}

function safeGi(name) {
	try {
		return gi(name);
	} catch (err) {
		console.error(err);
		return el("span", { class: "gi" });
	}
}

function iconBtn(action, label, icon) {
	return el("button", {
		class: "icon-btn wm-ctl",
		attrs: { type: "button", "data-wm": action, "aria-label": label, title: label },
	}, safeGi(icon));
}

/** A tool renames its own window — "Planner — Ashford", a chat's title. */
export function retitle(id, text) {
	if (!text) return;
	const rec = wm.mounted.get(id);
	if (!rec) return;
	const h = rec.el.querySelector(".wm-title");
	if (h) h.textContent = text;
	rec.el.setAttribute("aria-label", text);
}

/* ---------- rendering ---------- */

/**
 * Rebuild the scaffolding and move the (already-mounted) windows into it.
 *
 * Called only on structural change — never on resize, which mutates flex
 * values in place, and never on focus, which toggles a class. Reinserting an
 * element resets its scroll position, so the offsets are saved and restored
 * around the swap.
 */
function render() {
	const host = $("wm-root");
	const scrolls = new Map();
	for (const [id, rec] of wm.mounted) scrolls.set(id, rec.bodyEl.scrollTop);
	const hadFocus = document.activeElement;

	wm.rendering = true;
	clear(host);
	if (!wm.root) {
		host.append(emptyState());
	} else if (isNarrow()) {
		host.append(renderNarrow());
	} else {
		host.append(build(wm.root));
	}
	wm.rendering = false;

	for (const [id, top] of scrolls) {
		const rec = wm.mounted.get(id);
		if (rec && top) rec.bodyEl.scrollTop = top;
	}
	if (hadFocus?.isConnected) {
		try { hadFocus.focus({ preventScroll: true }); } catch (_) { /* gone */ }
	}
	paintFocus();
}

function build(node) {
	if (node.t === "leaf") return wm.mounted.get(node.id)?.el ?? el("div");

	if (node.t === "tabs") {
		const strip = el("div", { class: "wm-tabstrip", attrs: { role: "tablist" } },
			...node.kids.map((kid, i) => tabButton(kid, i === node.active, node.id, i)));
		// Every tab is built, not just the visible one, and the rest are
		// hidden with CSS. A tab rendered into a detached tree would break
		// every getElementById inside it — which is how the tools address
		// their own markup — and switching tabs would remount and reset them.
		const panels = node.kids.map((kid, i) => el("div", {
			class: `wm-tabpanel${i === node.active ? " is-active" : ""}`,
		}, build(kid)));
		return el("div", { class: "wm-tabs", attrs: { "data-node": node.id } },
			strip, el("div", { class: "wm-tabbody" }, ...panels));
	}

	const kids = [];
	node.kids.forEach((kid, i) => {
		if (i > 0) {
			kids.push(el("div", {
				class: "wm-gutter f-bar",
				attrs: {
					"data-split": node.id, "data-gutter": String(i - 1), role: "separator",
					"aria-orientation": node.dir === "row" ? "vertical" : "horizontal",
					tabindex: "-1",
				},
			}));
		}
		kids.push(el("div", {
			class: "wm-pane",
			attrs: { "data-pane": kid.id, style: `flex: ${node.fr[i]} 1 0` },
		}, build(kid)));
	});
	return el("div", { class: `wm-split is-${node.dir}`, attrs: { "data-node": node.id } }, ...kids);
}

function tabButton(kid, active, containerID, index) {
	const leaf = T.firstLeaf(kid);
	const def = leaf && toolDef(leaf.tool);
	return el("button", {
		class: `wm-tab f-tab${active ? " is-active" : ""}`,
		attrs: {
			type: "button", role: "tab", "aria-selected": String(active),
			"data-tabs": containerID, "data-index": String(index),
		},
	},
		def ? safeSprite(def.icon) : el("span"),
		el("span", { class: "wm-tab-label", text: def?.title || leaf?.tool || "Window" }),
	);
}

/**
 * Narrow screens get one window at a time with a tab strip as the switcher.
 * Splits are ignored rather than shrunk — two 160px columns is not a layout,
 * and this keeps the tree intact so rotating back restores it.
 */
function renderNarrow() {
	const all = T.leaves(wm.root);
	const activeIdx = Math.max(0, all.findIndex((l) => l.id === wm.focus));
	const strip = el("div", { class: "wm-tabstrip", attrs: { role: "tablist" } },
		...all.map((leaf, i) => tabButton(leaf, i === activeIdx, "narrow", i)));
	const panels = all.map((leaf, i) => el("div", {
		class: `wm-tabpanel${i === activeIdx ? " is-active" : ""}`,
	}, wm.mounted.get(leaf.id)?.el ?? el("div")));
	return el("div", { class: "wm-tabs is-narrow" },
		strip, el("div", { class: "wm-tabbody" }, ...panels));
}

function emptyState() {
	return el("div", { class: "wm-empty" },
		safeSprite("spellbook", { scale: 3 }),
		el("p", { class: "wm-empty-title", text: "No tools open" }),
		el("p", { class: "wm-empty-sub", text: "Pick one from the rail, or press Ctrl+G then a letter." }),
	);
}

/** Focus is a class, not a rebuild — the frame swaps to panel-stone-active. */
function paintFocus() {
	for (const [id, rec] of wm.mounted) {
		const on = id === wm.focus;
		rec.el.classList.toggle("is-focused", on);
		// The focus indicator is the frame itself: panel-stone-active, drawn
		// per theme by build-assets.py and until now unused by anything.
		rec.el.classList.toggle("f-stone-active", on);
	}
}

/** Resize writes flex values straight onto the panes, so dragging is smooth. */
function applyFractions() {
	const host = $("wm-root");
	T.walk(wm.root, (node) => {
		if (node.t !== "split") return;
		const container = host.querySelector(`.wm-split[data-node="${node.id}"]`);
		if (!container) return;
		node.kids.forEach((kid, i) => {
			const pane = container.querySelector(`:scope > .wm-pane[data-pane="${kid.id}"]`);
			if (pane) pane.style.flex = `${node.fr[i]} 1 0`;
		});
	});
}

/* ---------- events ---------- */

function onHostClick(e) {
	const ctl = e.target.closest("[data-wm]");
	if (ctl) {
		const win = ctl.closest(".wm-window");
		const id = win?.dataset.node;
		if (ctl.dataset.wm === "close") closeWindow(id);
		else if (ctl.dataset.wm === "tab") { setFocus(id); toggleTabsOnFocused(); }
		return;
	}

	const tab = e.target.closest(".wm-tab");
	if (tab) {
		const container = tab.dataset.tabs;
		const index = Number(tab.dataset.index);
		if (container === "narrow") {
			const leaf = T.leaves(wm.root)[index];
			if (leaf) revealLeaf(leaf.id);
		} else {
			wm.root = T.setActive(wm.root, container, index);
			const node = T.find(wm.root, container);
			wm.focus = T.firstLeaf(node.kids[index])?.id ?? wm.focus;
			render();
			changed();
		}
		return;
	}

	const win = e.target.closest(".wm-window");
	if (win) setFocus(win.dataset.node);
}

// Crossing the narrow threshold changes the rendering mode entirely, so it is
// the one resize that rebuilds. Within a mode, nothing happens.
let wasNarrow = null;
function onViewportChange() {
	const now = isNarrow();
	if (now === wasNarrow) return;
	wasNarrow = now;
	render();
}
