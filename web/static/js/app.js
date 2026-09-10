// Grimoire — entry point. Wires the shell, then hands the screen to the
// window manager.
//
// The old shell opened one surface at a time and hid the rest with a class on
// <main>; three of them shared that class, so closing one stripped what the
// others needed and they stacked down the page. Everything below the topbar is
// now a workspace of tiled windows, and which tools exist — in the rail, in the
// command menu, on the keyboard, in a saved layout — comes from one registry.

import { $, el, clear, isNarrow } from "./dom.js";
import { api } from "./api.js";
import { state, loadCorpusPreference, saveCorpusPreference } from "./state.js";
import { initDrawer } from "./drawer.js";
import { initPalette, openPalette, closePalette } from "./palette.js";
import { refreshHistory, syncChrome, setCorpus, setFoot, startNewChat } from "./chat.js";
import { initResolve } from "./resolve.js";
import { initVoice } from "./voice.js";
import { initAdmin } from "./admin.js";
import { initLibrary } from "./library.js";
import { initShares } from "./shares.js";
import { hydrate, gi } from "./icons.js";
import { initScene, initSettings, addSettingsSection, rebuildSettings } from "./scene.js";
import { initDice } from "./dice.js";
import { initBoard } from "./board.js";

import { TOOLS, inSeat } from "./wm/registry.js";
import { loadSeat, seatRole } from "./seat.js";
import * as wm from "./wm/wm.js";
import { initWM, openTool } from "./wm/wm.js";
import { initDrag } from "./wm/drag.js";
import { initKeys, pushModal, popModal, refreshSheet } from "./wm/keys.js";
import { openMenu, closeMenu, closePrompt, openPrompt, openConfirm, toolItems } from "./wm/menu.js";
import * as ws from "./wm/workspaces.js";

/* ---------- the workspace strip ---------- */

/**
 * The strip is the app's navigation now that the rail has stopped listing
 * every tool, so it has to carry the things that were previously keyboard-only
 * or missing outright: making a workspace, naming one, and throwing one away.
 *
 * On a narrow screen a row of tabs plus a "+" is more chrome than a phone can
 * spare, so it collapses to the active workspace's name and opens the same
 * actions as a menu.
 */
function renderWorkspaces() {
	const strip = clear($("wm-strip"));
	const active = ws.activeSlot();
	const entries = ws.list().filter((entry) => ws.showsInStrip(entry, active));

	if (isNarrow()) {
		const current = entries.find((e) => e.slot === active);
		strip.append(el("button", {
			class: "wm-ws is-active wm-ws-picker",
			attrs: { type: "button", "aria-label": `Workspace: ${current?.name || active}. Switch or edit` },
			on: { click: () => openWorkspaceSwitcher() },
		},
			el("span", { class: "wm-ws-slot", text: String(active) }),
			el("span", { text: current?.name || `Workspace ${active}` }),
			el("span", { class: "wm-ws-caret", attrs: { "aria-hidden": "true" }, text: "\u25be" }),
		));
		wm.setEmptyActions({ reset: ws.hasPreset(active) ? () => ws.reset() : null });
		return;
	}

	for (const entry of entries) {
		const on = entry.slot === active;
		const tab = el("div", { class: `wm-ws-wrap${on ? " is-active" : ""}` },
			el("button", {
				class: `wm-ws${on ? " is-active" : ""}`,
				attrs: { type: "button", role: "tab", "aria-selected": String(on) },
				on: {
					click: () => ws.switchTo(entry.slot),
					// A rename is one gesture for anyone who expects tabs to
					// behave like tabs; the ⋯ menu is the one that has to work
					// on a touchscreen, and does.
					dblclick: () => renameWorkspace(entry.slot),
				},
			},
				el("span", { class: "wm-ws-slot", text: String(entry.slot) }),
				el("span", { text: entry.name }),
			),
		);
		// Only the workspace you are looking at offers its actions: every one
		// of them acts on the active layout anyway, and nine ⋯ buttons in a
		// row is noise.
		if (on) {
			tab.append(el("button", {
				class: "wm-ws-more",
				attrs: { type: "button", "aria-label": `Options for ${entry.name}`, title: "Workspace options" },
				on: { click: () => openWorkspaceMenu(entry.slot) },
			}, el("span", { class: "wm-ws-more-dots", attrs: { "aria-hidden": "true" }, text: "\u22ef" })));
		}
		strip.append(tab);
	}

	// Re-offered per workspace rather than wired once at boot: only a seeded
	// slot has a preset to go back to, and offering "reset" on a slot the user
	// made would delete its row and drop it out of the strip.
	wm.setEmptyActions({ reset: ws.hasPreset(active) ? () => ws.reset() : null });

	strip.append(el("button", {
		class: "wm-ws wm-ws-new",
		attrs: { type: "button", "aria-label": "New workspace", title: "New workspace" },
		on: { click: () => newWorkspace() },
	}, el("span", { attrs: { "aria-hidden": "true" }, text: "+" })));
}

/* ---------- workspace actions ---------- */

/**
 * Make a workspace holding one tool.
 *
 * Picking the tool first is the point: "I just want Chat open" is a real and
 * common want, and it is one click, one pick, and a workspace that persists —
 * rather than a mode, or an empty slot you then have to furnish.
 */
function newWorkspace() {
	if (!ws.freeSlot()) {
		return notice("All nine workspaces are in use", "Close one from its ⋯ menu to free a slot.");
	}
	pickTool("New workspace — pick a tool", (id) => {
		ws.create(TOOLS[id]?.title || "Workspace", id);
		renderWorkspaces();
	});
}

function renameWorkspace(slot) {
	const entry = ws.list().find((e) => e.slot === slot);
	openPrompt({
		title: `Rename workspace ${slot}`,
		label: "Workspace name",
		value: entry?.name || "",
		placeholder: "At the table",
		onSubmit: (name) => {
			ws.rename(slot, name);
			renderWorkspaces();
		},
		onClose: popModal,
	});
	pushModal();
}

/** Everything you can do to one workspace, on one button a thumb can hit. */
function openWorkspaceMenu(slot) {
	const entry = ws.list().find((e) => e.slot === slot);
	const name = entry?.name || `Workspace ${slot}`;
	const items = [
		{ label: "Rename…", hint: "Workspace", run: () => renameWorkspace(slot) },
		{ label: "Open a tool…", hint: "Workspace", run: () => pickTool("Open a tool", (id) => openTool(id)) },
		{ label: "Close every window", hint: "Workspace", run: () => wm.closeAll() },
	];
	// A seeded slot resets; a slot the user made has no preset to go back to,
	// so the honest offer there is to throw it away.
	if (ws.hasPreset(slot)) {
		items.push({
			label: "Reset to preset", hint: "Workspace",
			run: () => confirmDestroy(`Reset “${name}” to its preset?`, "Reset it", () => {
				ws.reset(slot);
				renderWorkspaces();
			}),
		});
	} else {
		items.push({
			label: "Close workspace", hint: "Workspace",
			run: () => confirmDestroy(`Close “${name}”?`, "Close it", () => {
				ws.remove(slot);
				renderWorkspaces();
			}),
		});
	}
	openMenu({ title: name, items, onClose: popModal });
	pushModal();
}

/** The narrow-screen strip: switch, or reach the same actions. */
function openWorkspaceSwitcher() {
	const active = ws.activeSlot();
	const items = ws.list()
		.filter((entry) => ws.showsInStrip(entry, active))
		.map((entry) => ({
			label: entry.name,
			hint: entry.slot === active ? "Current" : `Workspace ${entry.slot}`,
			run: () => {
				ws.switchTo(entry.slot);
				renderWorkspaces();
			},
		}));
	items.push({ label: "New workspace…", hint: "Workspace", run: () => newWorkspace() });
	items.push({ label: `Options for “${ws.list().find((e) => e.slot === active)?.name || active}”…`, hint: "Workspace", run: () => openWorkspaceMenu(active) });
	openMenu({ title: "Workspaces", items, onClose: popModal });
	pushModal();
}

function confirmDestroy(title, confirmText, onConfirm) {
	openConfirm({ title, confirmText, onConfirm, onClose: popModal });
	pushModal();
}

/** A dead end with an exit — the menu is the only dialog that reads well on
    a phone, so a message uses it rather than a fourth kind of box. */
function notice(title, detail) {
	openMenu({ title, items: [{ label: detail, hint: "", run: () => {} }], onClose: popModal });
	pushModal();
}

/* ---------- the cheat sheet ---------- */

function toggleSheet(show) {
	const layer = $("wm-sheet-layer");
	const wasOpen = !layer.hidden;
	const open = show ?? wasOpen === false;
	if (open === !wasOpen) open ? pushModal() : popModal();
	layer.hidden = !open;
}

function initSheet() {
	$("wm-sheet-layer").addEventListener("click", (e) => {
		if (e.target.closest("[data-sheet-close]")) toggleSheet(false);
	});
	$("wm-help").addEventListener("click", () => toggleSheet(true));
}

/* ---------- corpus ---------- */

/**
 * Switching games swaps the whole workspace set. Nothing is closed on the
 * user's behalf: the other game's windows stay exactly as they were, so
 * switching back is one keystroke rather than a rebuild.
 */
async function pickCorpus(corpus) {
	if (corpus === state.corpus) return;
	setCorpus(corpus);
	saveCorpusPreference(corpus);
	api.uiSavePrefs({ corpus }).catch(() => { /* localStorage still has it */ });

	await ws.switchCorpus(corpus);
	renderWorkspaces();
	refreshSheet(corpus, seatRole());
}

function initRail() {
	const app = $("app");
	const setHidden = (hidden) => app.classList.toggle("rail-hidden", hidden);

	$("rail-collapse").addEventListener("click", () => setHidden(true));
	$("rail-open").addEventListener("click", () => setHidden(false));
	$("rail-scrim").addEventListener("click", () => setHidden(true));

	setHidden(isNarrow());
	window.addEventListener("resize", () => {
		if (isNarrow()) setHidden(true);
	});

	document.querySelectorAll(".corpus-opt").forEach((btn) => {
		btn.addEventListener("click", () => pickCorpus(btn.dataset.corpus));
	});

	// New chat sits in the rail, outside every window, so it is wired here
	// rather than in the chat module's own one-shot wiring.
	$("new-chat").addEventListener("click", () => startNewChat());

	// The one pointer route to every tool, now that the rail does not list
	// them. It opens the same registry-built picker the keyboard does.
	$("rail-tools-btn").addEventListener("click", () => pickTool("Open a tool", (id) => openTool(id)));
	if (/mac|iphone|ipad/i.test(navigator.platform || navigator.userAgent || "")) {
		$("tools-kbd").textContent = "\u2318 G";
	}

	$("rail-search").addEventListener("click", () => openPalette());
	$("topbar-search").addEventListener("click", () => openPalette());
}

/* ---------- the tool picker and the command menu ---------- */

/**
 * Ask which tool, then do something with it. Split right, split down and
 * "open a tool" all need a tool before they mean anything; all three used to
 * open the rules palette instead, which cannot open one.
 */
function pickTool(title, run) {
	openMenu({
		title,
		items: toolItems(state.corpus, seatRole(), run),
		onClose: popModal,
	});
	pushModal();
}

/**
 * What you can do to one window.
 *
 * Split, move, zoom and tab/untab existed only as chords — Alt+F and the
 * leader's Shift pairs — which means they did not exist at all on a phone or
 * for anyone who has not read the cheat sheet. The titlebar's ⋯ calls this.
 */
function openWindowMenu() {
	const items = [
		{ label: "Zoom", hint: "Fill the workspace", run: () => wm.toggleZoom() },
		{ label: "Split right…", hint: "Window", run: () => pickTool("Split right", (id) => wm.splitFocused("row", id)) },
		{ label: "Split down…", hint: "Window", run: () => pickTool("Split down", (id) => wm.splitFocused("col", id)) },
		{ label: "Tab / untab", hint: "Window", run: () => wm.toggleTabsOnFocused() },
		{ label: "Move left", hint: "Move", run: () => wm.moveWindow("left") },
		{ label: "Move right", hint: "Move", run: () => wm.moveWindow("right") },
		{ label: "Move up", hint: "Move", run: () => wm.moveWindow("up") },
		{ label: "Move down", hint: "Move", run: () => wm.moveWindow("down") },
		{ label: "Close window", hint: "Window", run: () => wm.closeWindow() },
	];
	openMenu({ title: "Window", items, onClose: popModal });
	pushModal();
}

/** Everything the shell can do, in one list. */
function openCommandMenu(cmd) {
	const items = [
		...toolItems(state.corpus, seatRole(), (id) => openTool(id)).map((it) => ({ ...it, hint: "Open" })),
		{ label: "Split right", hint: "Window", run: cmd.splitRight },
		{ label: "Split down", hint: "Window", run: cmd.splitDown },
		{ label: "Tab / untab", hint: "Window", run: cmd.toggleTabs },
		{ label: "Zoom window", hint: "Window", run: cmd.zoom },
		{ label: "Close window", hint: "Window", run: cmd.close },
		{ label: "Close every window", hint: "Window", run: cmd.closeAll },
		{ label: "New workspace…", hint: "Workspace", run: cmd.newWorkspace },
		{ label: "Rename this workspace…", hint: "Workspace", run: cmd.renameWorkspace },
		{ label: "Reset workspace to preset", hint: "Workspace", run: cmd.resetWorkspace },
		...ws.list()
			.filter((entry) => ws.showsInStrip(entry, ws.activeSlot()))
			.map((entry) => ({
				label: entry.name, hint: `Workspace ${entry.slot}`, run: () => ws.switchTo(entry.slot),
			})),
		{ label: "Keyboard shortcuts", hint: "Help", run: () => toggleSheet(true) },
	];
	openMenu({ title: "Commands", items, onClose: popModal });
	pushModal();
}

/* ---------- keyboard ---------- */

/**
 * Every command the keyboard can reach. Declared once here and named in
 * keys.js, so a binding, its cheat-sheet line and the thing it does cannot
 * drift apart.
 */
function commands() {
	const cmd = {
		palette: () => openPalette(),
		help: () => toggleSheet(),
		// Escape unwinds one layer at a time, topmost first.
		dismiss: () => {
			if (closePrompt()) return;
			if (closeMenu()) return;
			if (!$("wm-sheet-layer").hidden) return toggleSheet(false);
			closePalette();
		},

		// The shell only opens what it offers (ADR 22): a role-gated tool is
		// absent from a member's keyboard, not merely failing. Saved layouts
		// mount directly through the window manager and are untouched.
		open: (id) => {
			if (!inSeat(id, seatRole())) return null;
			return openTool(id);
		},
		close: () => wm.closeWindow(),
		closeAll: () => wm.closeAll(),
		zoom: () => wm.toggleZoom(),
		focus: (dir) => wm.moveFocus(dir),
		move: (dir) => wm.moveWindow(dir),
		prevTab: () => wm.cycleTab(-1),
		nextTab: () => wm.cycleTab(1),
		toggleTabs: () => wm.toggleTabsOnFocused(),

		picker: () => pickTool("Open a tool", (id) => openTool(id)),
		splitRight: () => pickTool("Split right", (id) => wm.splitFocused("row", id)),
		splitDown: () => pickTool("Split down", (id) => wm.splitFocused("col", id)),

		workspace: (n) => ws.switchTo(n),
		resetWorkspace: () => ws.reset(),
		newWorkspace: () => newWorkspace(),
		renameWorkspace: () => renameWorkspace(ws.activeSlot()),
	};
	cmd.commands = () => openCommandMenu(cmd);
	return cmd;
}

/* ---------- account, meta ---------- */

// Who is signed in, for the settings popup's account section. The section is
// rebuilt on every theme change, so it reads this rather than closing over a
// value that was only correct the first time.
let account = null;

/**
 * Sign-out and the corpus counts, in the settings popup.
 *
 * They used to be two more rows and a paragraph at the bottom of the rail —
 * a column that does not scroll and was already overflowing. They describe
 * this account and this install, which is what the popup is for.
 */
function accountSection() {
	if (!account?.username && !state.meta) return null;
	const box = el("div", { class: "set-group set-account" });

	if (account?.username) {
		box.append(el("p", { class: "set-label", text: "Signed in" }));
		box.append(el("p", { class: "set-account-user", text: account.username }));
		box.append(el("button", {
			class: "set-signout",
			attrs: { type: "button" },
			on: {
				click: async (e) => {
					e.currentTarget.disabled = true;
					try {
						await api.logout();
					} catch (_) { /* the cookie is gone either way; land on the gate */ }
					window.location.assign("/");
				},
			},
		}, gi("signout"), el("span", { text: "Sign out" })));
	}

	const counts = (state.meta?.corpora || [])
		.map((c) => `${c.name}: ${c.count.toLocaleString()} entries`)
		.join(" · ");
	if (counts) box.append(el("p", { class: "set-build", text: counts }));

	return box;
}

async function initAccount() {
	let auth;
	try {
		auth = await api.authState();
	} catch (_) {
		return; // non-fatal: the rest of the app is unaffected
	}
	if (!auth.username) return;

	account = auth;
	// scene.js owns this button's contents; a failed settings init must not
	// take the account section down with it.
	const slot = $("rail-user");
	if (slot) slot.textContent = auth.username;
	rebuildSettings();
}

async function loadMeta() {
	try {
		state.meta = await api.meta();
	} catch (_) {
		return; // non-fatal: the app works without the counts
	}
	const meta = state.meta;
	rebuildSettings();

	if (meta.chat_configured === false) {
		setFoot("The sage is asleep — set ANTHROPIC_API_KEY on the server to enable chat. Rule search still works.", true);
	} else if (meta.chat_model) {
		const standby = (meta.chat_fallbacks || []).join(", then ");
		setFoot(standby ? `Sage: ${meta.chat_model} (standby: ${standby})` : `Sage: ${meta.chat_model}`);
	}
}

/**
 * Interface preferences moved server-side with the layouts, so the chosen game
 * follows the account rather than the browser. localStorage stays as the
 * offline answer and as the value we start from while the request is in
 * flight — the shell must not wait on the network to draw.
 */
async function loadPrefs() {
	try {
		const { prefs } = await api.uiPrefs();
		if (prefs?.corpus && prefs.corpus !== state.corpus) {
			state.corpus = prefs.corpus;
			saveCorpusPreference(prefs.corpus);
		}
	} catch (_) { /* the local preference is already applied */ }
}

/* ---------- boot ---------- */

// One module's missing element must not take the whole shell down: a thrown
// init used to silently kill every init after it (dead buttons, no history,
// empty badge). Each runs isolated; failures land in the console instead.
function safe(name, init) {
	try {
		init();
	} catch (err) {
		console.error(`init ${name} failed:`, err);
	}
}

async function start() {
	// Icons first: the template marks its slots with data-ico / data-gi, and
	// everything below assumes those slots already hold real art.
	hydrate();
	safe("scene", initScene);
	safe("settings", initSettings);
	safe("corpus-preference", loadCorpusPreference);

	// The seat runs alongside the prefs fetch: both must land before the
	// keyboard and the workspace layer shape themselves, and neither waits
	// on the other.
	const prefs = loadPrefs();
	const seat = loadSeat();

	safe("rail", initRail);
	safe("drawer", initDrawer);
	safe("palette", initPalette);
	safe("resolve", initResolve);
	safe("voice", initVoice);
	safe("sheet", initSheet);

	await prefs;
	await seat;

	// The window manager, then the layouts it renders.
	safe("wm", () => initWM(state.corpus));
	safe("drag", initDrag);
	const cmd = commands();
	safe("keys", () => initKeys(cmd, { corpus: state.corpus, seat: seatRole() }));
	// Ctrl+Shift+P was the command menu's only door. A shell whose commands
	// are reachable one way, by chord, is a shell that does not exist on a
	// phone.
	safe("commands-button", () => {
		$("topbar-commands").addEventListener("click", () => openCommandMenu(cmd));
	});

	// The window manager owns no navigation of its own: an empty workspace and
	// a window's ⋯ both ask the shell, which is the only thing that knows what
	// this seat may open.
	safe("wm-hooks", () => {
		wm.setWindowMenu(() => openWindowMenu());
		// `reset` is set per workspace by renderWorkspaces; only the opener is
		// constant.
		wm.setEmptyActions({ open: () => pickTool("Open a tool", (id) => openTool(id)) });
	});
	// The strip shows which slots hold something, so it follows the layout.
	wm.onChange(renderWorkspaces);
	safe("account-section", () => addSettingsSection(accountSection));

	try {
		await ws.initWorkspaces(state.corpus, seatRole(), renderWorkspaces);
	} catch (err) {
		console.error("workspaces failed to load:", err);
		safe("fallback-chat", () => openTool("chat"));
	}
	safe("workspaces", renderWorkspaces);

	// Crossing the narrow threshold swaps the strip between tabs and a single
	// picker, so it is the one resize the shell redraws for.
	let wasNarrow = isNarrow();
	window.addEventListener("resize", () => {
		if (isNarrow() === wasNarrow) return;
		wasNarrow = isNarrow();
		renderWorkspaces();
	});

	safe("chrome", syncChrome);
	safe("account", initAccount);
	safe("dice", () => initDice());
	safe("board", () => initBoard());
	safe("admin", initAdmin);
	safe("library", initLibrary);
	safe("shares", initShares);
	safe("meta", loadMeta);
	safe("history", refreshHistory);
}

if (document.readyState === "loading") {
	document.addEventListener("DOMContentLoaded", start);
} else {
	start();
}
