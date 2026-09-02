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
import { TOOLS, toolsFor } from "./registry.js";

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

/** Every tool this game ships, as menu items. Registry-driven, like the rail. */
export function toolItems(corpus, run) {
	return toolsFor(corpus).map((id) => ({
		label: TOOLS[id].title,
		hint: TOOLS[id].blurb || "",
		icon: TOOLS[id].icon,
		run: () => run(id),
	}));
}
