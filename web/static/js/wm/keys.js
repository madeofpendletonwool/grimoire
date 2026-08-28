// The keyboard layer: one dispatcher, a leader chord, and a cheat sheet.
//
// Resolution lives in keymap.js (pure, tested). This file owns the parts that
// need a document: listening, deciding when typing should swallow a key,
// tracking which layers are active, and drawing the two pieces of UI that make
// a keyboard-driven shell learnable — the which-key hint bar and the sheet.
//
// Commands are injected rather than imported, so the keyboard layer has no
// opinion about the window manager and no import cycle with it.

import { $, el, clear } from "../dom.js";
import { createKeymap, describe, normalize, parseSpec, LAYERS } from "./keymap.js";
import { TOOLS, TOOL_IDS, toolsFor } from "./registry.js";

const LEADER = "mod+g";
const HINT_DELAY = 400;   // ms before the leader shows its menu

const state = {
	km: null,
	pending: null,        // the leader, while a chord is half-typed
	hintTimer: null,
	modalDepth: 0,        // how many floating dialogs are stacked
	mac: false,
	corpus: "mtg",
};

/** A modal claims the top layer while it is open, so Escape reaches it first. */
export function pushModal() {
	state.modalDepth++;
}
export function popModal() {
	state.modalDepth = Math.max(0, state.modalDepth - 1);
}

const activeLayers = () => (state.modalDepth > 0 ? LAYERS : ["window", "global"]);

/**
 * Wire the keyboard.
 *
 * `cmd` maps a name to a function. Everything below is declared against those
 * names, so rebinding later is a data change and the cheat sheet is generated
 * rather than written down twice and allowed to drift.
 */
export function initKeys(cmd, { corpus = "mtg" } = {}) {
	state.mac = /mac|iphone|ipad/i.test(navigator.platform || navigator.userAgent || "");
	state.corpus = corpus;
	const km = createKeymap();
	state.km = km;

	const g = (spec, name, label, group, opts) => km.add("global", spec, cmd[name], { label, group, ...opts });

	/* --- finding things --- */
	g("mod+k", "palette", "Search rules and cards", "Find");
	g("mod+shift+p", "commands", "Command menu", "Find");
	g("?", "help", "Keyboard shortcuts", "Find", { typing: false });

	/* --- workspaces ---
	   All nine bind; only the first is listed. Nine near-identical rows push
	   everything else off the cheat sheet, and once you know Alt+1 you know
	   the other eight. */
	for (let i = 1; i <= 9; i++) {
		km.add("global", `alt+${i}`, () => cmd.workspace(i), {
			label: i === 1 ? "Switch workspace (1-9)" : `Workspace ${i}`,
			group: "Workspaces", listed: i === 1,
		});
	}

	/* --- focus and layout ---
	   Alt rather than Ctrl throughout: Ctrl+1..9 switches browser tabs and
	   Ctrl+W closes them, and neither is ours to take. */
	for (const dir of ["left", "right", "up", "down"]) {
		km.add("global", `alt+arrow${dir}`, () => cmd.focus(dir),
			{ label: `Focus ${dir}`, group: "Windows" });
		km.add("global", `alt+shift+arrow${dir}`, () => cmd.move(dir),
			{ label: `Move window ${dir}`, group: "Windows" });
	}
	g("alt+w", "close", "Close window", "Windows");
	g("alt+f", "zoom", "Zoom window", "Windows");
	g("alt+[", "prevTab", "Previous tab", "Windows");
	g("alt+]", "nextTab", "Next tab", "Windows");
	km.add("global", "alt+enter", cmd.picker, { label: "Open a tool", group: "Windows", listed: false });

	/* --- the leader ---
	   Everything reachable directly is also reachable here, plus the things
	   no safe direct binding was left for. */
	km.add("global", `${LEADER} v`, cmd.splitRight, { label: "Split right", group: "Leader" });
	km.add("global", `${LEADER} s`, cmd.splitDown, { label: "Split down", group: "Leader" });
	km.add("global", `${LEADER} t`, cmd.toggleTabs, { label: "Tab / untab", group: "Leader" });
	km.add("global", `${LEADER} w`, cmd.close, { label: "Close window", group: "Leader" });
	km.add("global", `${LEADER} x`, cmd.closeAll, { label: "Close every window", group: "Leader" });
	km.add("global", `${LEADER} r`, cmd.resetWorkspace, { label: "Reset to preset", group: "Leader" });

	// One accelerator per tool, straight from the registry — a new tool gets
	// its shortcut, its menu entry and its cheat-sheet line for free.
	for (const id of TOOL_IDS) {
		const def = TOOLS[id];
		km.add("global", `${LEADER} ${def.accel}`, () => cmd.open(id), {
			label: def.title, group: "Open a tool", tool: id,
		});
	}

	/* --- always available, even mid-typing --- */
	km.add("global", "escape", cmd.dismiss, { label: "Dismiss", group: "Find", typing: true });

	document.addEventListener("keydown", onKeydown, true);
	buildSheet();
}

export function setKeyCorpus(corpus) {
	state.corpus = corpus;
}

/* ---------- dispatch ---------- */

/**
 * Typing must win. A shell that steals "w" while the DM is writing a session
 * note is worse than one with no shortcuts, so plain keys are ignored inside
 * inputs — but modified ones (Ctrl+K, Alt+1) and Escape still fire, because
 * those are why they are modified.
 */
function isTyping(target) {
	if (!target) return false;
	const tag = target.tagName;
	return tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT" || target.isContentEditable;
}

function onKeydown(e) {
	if (e.isComposing || e.keyCode === 229) return;   // mid-IME composition

	const descriptor = describe(e, state.mac);
	const bare = !e.ctrlKey && !e.metaKey && !e.altKey;

	if (state.pending) {
		const got = state.km.resolve(descriptor, activeLayers(), state.pending);
		clearPending();
		if (got?.kind === "action") {
			e.preventDefault();
			run(got);
		}
		// A cancelled chord still swallows the key: the leader was a promise
		// that the next keystroke belonged to the shell.
		e.preventDefault();
		return;
	}

	const got = state.km.resolve(descriptor, activeLayers());
	if (!got) return;

	if (got.kind === "prefix") {
		if (isTyping(e.target) && bare) return;
		e.preventDefault();
		setPending(descriptor);
		return;
	}

	if (isTyping(e.target) && bare && !got.meta.typing) return;
	e.preventDefault();
	run(got);
}

function run(got) {
	try {
		got.handler?.();
	} catch (err) {
		console.error(`shortcut ${got.meta?.spec} failed:`, err);
	}
}

/* ---------- the which-key hint bar ---------- */

function setPending(leader) {
	state.pending = leader;
	document.body.classList.add("wm-chording");
	state.hintTimer = setTimeout(() => showHint(leader), HINT_DELAY);
}

function clearPending() {
	state.pending = null;
	clearTimeout(state.hintTimer);
	document.body.classList.remove("wm-chording");
	$("wm-hint")?.remove();
}

/**
 * Show what the leader offers, after a beat. The delay matters: someone who
 * knows the chord never sees it, and someone who does not gets taught without
 * having to go looking.
 */
function showHint(leader) {
	$("wm-hint")?.remove();
	const items = state.km.continuations(leader, activeLayers())
		.filter((m) => !m.tool || toolsFor(state.corpus).includes(m.tool));

	const bar = el("div", { class: "wm-hint f-stone", attrs: { id: "wm-hint", role: "status", "aria-live": "polite" } },
		el("span", { class: "wm-hint-lead", text: pretty(leader) + " …" }),
		...items.map((m) => el("span", { class: "wm-hint-item" },
			el("kbd", { text: pretty(parseSpec(m.spec)[1]) }),
			el("span", { text: m.label }),
		)),
	);
	document.body.append(bar);
}

/* ---------- the cheat sheet ---------- */

/** Render a spec the way a keyboard is labelled, not the way it is stored. */
export function pretty(spec) {
	const mod = state.mac ? "⌘" : "Ctrl";
	const names = {
		arrowleft: "←", arrowright: "→", arrowup: "↑", arrowdown: "↓",
		escape: "Esc", enter: "↵", " ": "Space",
	};
	return normalize(spec).split("+").map((p) => {
		if (p === "mod") return mod;
		if (p === "alt") return state.mac ? "⌥" : "Alt";
		if (p === "shift") return state.mac ? "⇧" : "Shift";
		return names[p] || (p.length === 1 ? p.toUpperCase() : p);
	}).join(state.mac ? "" : "+");
}

function buildSheet() {
	const sheet = $("wm-sheet");
	if (!sheet) return;
	const body = clear(sheet.querySelector(".wm-sheet-body") || sheet);

	const groups = new Map();
	for (const meta of state.km.list()) {
		if (meta.listed === false) continue;
		if (meta.tool && !toolsFor(state.corpus).includes(meta.tool)) continue;
		if (!groups.has(meta.group)) groups.set(meta.group, []);
		groups.get(meta.group).push(meta);
	}

	for (const [name, items] of groups) {
		body.append(el("section", { class: "wm-sheet-group" },
			el("h3", { class: "wm-sheet-heading", text: name }),
			el("dl", { class: "wm-sheet-list" },
				...items.flatMap((m) => [
					el("dt", {}, ...parseSpec(m.spec).map((step) => el("kbd", { text: pretty(step) }))),
					el("dd", { text: m.label }),
				]),
			),
		));
	}
}

/** Rebuild the sheet when the game changes — the tool list changes with it. */
export function refreshSheet(corpus) {
	state.corpus = corpus;
	buildSheet();
}
