// The tool picker and the command menu.
//
// Every structural command needs a tool to act on — "split right" is not an
// action until something is going in the new pane — and until now three
// bindings (Alt+Enter, and the leader's split pair) stood in for that by
// opening the rules-and-cards palette, which cannot open a tool at all.
//
// One list serves both: pick a tool, or pick a command. It is deliberately not
// the palette. The palette searches the corpus — rules, cards, entities — and
// folding the shell's own commands into it would mean every rule search
// competed with a window command for the same ranking.

import { $, el, clear } from "../dom.js";
import { sprite } from "../icons.js";
import { TOOLS, toolsFor, SEAT_DM } from "./registry.js";

let open = null;   // { layer, input, list, items, index, onClose }

/**
 * Show a menu.
 *
 * `items` are `{ label, hint, icon, run }`. Typing filters, arrows move,
 * Enter runs, Escape closes — the four things a list like this must do
 * without a mouse, since this is the keyboard layer's own affordance.
 */
export function openMenu({ title, items, onClose }) {
	closeMenu();

	const input = el("input", {
		class: "wm-menu-input",
		attrs: { type: "text", placeholder: "Type to filter…", "aria-label": title, autocomplete: "off" },
	});
	const list = el("div", { class: "wm-menu-list", attrs: { role: "listbox" } });

	const layer = el("div", {
		class: "wm-menu-layer",
		attrs: { role: "dialog", "aria-modal": "true", "aria-label": title },
	},
		el("div", { class: "wm-menu-scrim", attrs: { "data-menu-close": "" } }),
		el("div", { class: "wm-menu f-stone" },
			el("header", { class: "wm-menu-head" },
				el("h2", { class: "wm-menu-title", text: title }),
			),
			input,
			list,
		),
	);

	open = { layer, input, list, items, index: 0, onClose };
	document.body.append(layer);

	input.addEventListener("input", () => paint());
	input.addEventListener("keydown", onKeydown);
	layer.addEventListener("click", (e) => {
		if (e.target.closest("[data-menu-close]")) return closeMenu();
		const row = e.target.closest("[data-menu-index]");
		if (row) choose(Number(row.dataset.menuIndex));
	});

	paint();
	input.focus();
	return true;
}

export function isMenuOpen() {
	return !!open;
}

export function closeMenu() {
	if (!open) return false;
	const { layer, onClose } = open;
	open = null;
	layer.remove();
	onClose?.();
	return true;
}

/* ---------- rendering ---------- */

function visible() {
	const q = open.input.value.trim().toLowerCase();
	if (!q) return open.items.map((item, i) => ({ item, i }));
	return open.items
		.map((item, i) => ({ item, i }))
		.filter(({ item }) => `${item.label} ${item.hint || ""}`.toLowerCase().includes(q));
}

function paint() {
	const rows = visible();
	if (open.index >= rows.length) open.index = Math.max(0, rows.length - 1);

	const list = clear(open.list);
	if (rows.length === 0) {
		list.append(el("p", { class: "wm-menu-empty", text: "Nothing matches." }));
		return;
	}
	rows.forEach(({ item, i }, pos) => {
		list.append(el("button", {
			class: `wm-menu-row${pos === open.index ? " is-active" : ""}`,
			attrs: {
				type: "button", role: "option", "data-menu-index": String(i),
				"aria-selected": String(pos === open.index),
			},
		},
			item.icon ? safeIcon(item.icon) : el("span", { class: "ico" }),
			el("span", { class: "wm-menu-label", text: item.label }),
			item.hint ? el("span", { class: "wm-menu-hint", text: item.hint }) : el("span"),
		));
	});
	list.querySelector(".is-active")?.scrollIntoView({ block: "nearest" });
}

// A registry typo costs an icon, not the menu (DESIGN.md invariant 8).
function safeIcon(name) {
	try {
		return sprite(name);
	} catch (err) {
		console.error(err);
		return el("span", { class: "ico" });
	}
}

/* ---------- keys ---------- */

// The menu owns its own keys rather than registering a "modal" layer: it lives
// only while it is open, and a list that answers arrows is not a binding table.
function onKeydown(e) {
	const rows = visible();
	if (e.key === "ArrowDown" || (e.key === "n" && e.ctrlKey)) {
		e.preventDefault();
		e.stopPropagation();
		open.index = rows.length ? (open.index + 1) % rows.length : 0;
		paint();
	} else if (e.key === "ArrowUp" || (e.key === "p" && e.ctrlKey)) {
		e.preventDefault();
		e.stopPropagation();
		open.index = rows.length ? (open.index - 1 + rows.length) % rows.length : 0;
		paint();
	} else if (e.key === "Enter") {
		e.preventDefault();
		e.stopPropagation();
		const row = rows[open.index];
		if (row) choose(row.i);
	} else if (e.key === "Escape") {
		e.preventDefault();
		e.stopPropagation();
		closeMenu();
	}
}

function choose(i) {
	const item = open?.items[i];
	closeMenu();
	try {
		item?.run?.();
	} catch (err) {
		console.error(`menu item ${item?.label} failed:`, err);
	}
}

/* ---------- the lists ---------- */

/** Every tool this game offers this seat, as menu items. Registry-driven,
    like the rail. */
export function toolItems(corpus, seat = SEAT_DM, run) {
	return toolsFor(corpus, seat).map((id) => ({
		label: TOOLS[id].title,
		hint: TOOLS[id].blurb || "",
		icon: TOOLS[id].icon,
		run: () => run(id),
	}));
}

/* ---------- prompt ---------- */

// A rename needs a text field, which the menu above cannot be: it filters a
// fixed list. Rather than a second dialog system, this borrows the menu's
// layer, scrim and frame, so both dismiss the same way and a caller has one
// thing to reason about.
let prompting = null;

/**
 * Ask for one line of text.
 *
 * `onSubmit` receives the trimmed value and is not called for an empty one:
 * every caller renames something that already has a name, and clearing it is
 * never what the user meant — ws.rename would reject it anyway.
 */
export function openPrompt({ title, label, value = "", placeholder = "", submitText = "Save", onSubmit, onClose }) {
	closePrompt();

	const input = el("input", {
		class: "wm-menu-input",
		attrs: { type: "text", value, placeholder, "aria-label": label || title, autocomplete: "off" },
	});

	const submit = () => {
		const next = input.value.trim();
		closePrompt();
		if (next) onSubmit?.(next);
	};

	const layer = el("div", {
		class: "wm-menu-layer",
		attrs: { role: "dialog", "aria-modal": "true", "aria-label": title },
	},
		el("div", { class: "wm-menu-scrim", attrs: { "data-prompt-close": "" } }),
		el("form", { class: "wm-menu f-stone", on: { submit: (e) => { e.preventDefault(); submit(); } } },
			el("header", { class: "wm-menu-head" },
				el("h2", { class: "wm-menu-title", text: title }),
			),
			input,
			el("div", { class: "wm-prompt-actions" },
				el("button", {
					class: "wm-prompt-btn",
					attrs: { type: "button", "data-prompt-close": "" },
					text: "Cancel",
				}),
				el("button", { class: "wm-prompt-btn is-primary", attrs: { type: "submit" }, text: submitText }),
			),
		),
	);

	prompting = { layer, onClose };
	document.body.append(layer);

	layer.addEventListener("click", (e) => {
		if (e.target.closest("[data-prompt-close]")) closePrompt();
	});
	input.addEventListener("keydown", (e) => {
		// Escape is stopped here as well as handled: the shell's dispatcher
		// would otherwise unwind a second layer behind this one in the same
		// keystroke.
		if (e.key !== "Escape") return;
		e.preventDefault();
		e.stopPropagation();
		closePrompt();
	});

	input.focus();
	input.select();
	return true;
}

export const isPromptOpen = () => !!prompting;

export function closePrompt() {
	if (!prompting) return false;
	const { layer, onClose } = prompting;
	prompting = null;
	layer.remove();
	onClose?.();
	return true;
}

/**
 * A yes/no question, as a menu of two.
 *
 * Throwing away a layout someone arranged deserves a confirmation, but not a
 * third dialog primitive — the menu already answers arrows, Enter, Escape and
 * touch, which is the whole requirement.
 *
 * The safe answer is listed first because the menu pre-selects row zero and
 * focuses its filter box: a stray Enter must not destroy the workspace.
 */
export function openConfirm({ title, confirmText, cancelText = "Keep it", onConfirm, onClose }) {
	return openMenu({
		title,
		items: [
			{ label: cancelText, hint: "", run: () => {} },
			{ label: confirmText, hint: "", icon: "close", run: () => onConfirm?.() },
		],
		onClose,
	});
}
