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
import { hydrate, sprite } from "./icons.js";
import { initScene, initSettings } from "./scene.js";

import { TOOLS, toolsFor } from "./wm/registry.js";
import * as wm from "./wm/wm.js";
import { initWM, openTool } from "./wm/wm.js";
import { initDrag } from "./wm/drag.js";
import { initKeys, pushModal, popModal, refreshSheet, pretty } from "./wm/keys.js";
import * as ws from "./wm/workspaces.js";

/* ---------- the rail ---------- */

/**
 * Build the tool list for one game. This replaced nine hardcoded buttons in
 * index.html and the html[data-corpus] display:none block in style.css — two
 * hand-maintained lists that both had to be edited for every new tool.
 */
function renderTools(corpus) {
	const nav = clear($("rail-tools"));
	for (const id of toolsFor(corpus)) {
		const def = TOOLS[id];
		nav.append(el("button", {
			class: "rail-action",
			attrs: { type: "button", "data-tool": id, title: def.blurb || def.title },
			on: { click: () => openTool(id) },
		},
			sprite(def.icon),
			el("span", { text: def.title }),
			el("kbd", { class: "rail-accel", text: pretty("mod+g") + " " + def.accel.toUpperCase() }),
		));
	}
	markOpenTools();
}

/** A dot on the tools already on screen, so the rail reflects the workspace. */
function markOpenTools() {
	const open = wm.openTools();
	for (const btn of $("rail-tools").children) {
		btn.classList.toggle("is-open", open.has(btn.dataset.tool));
	}
}

/* ---------- the workspace strip ---------- */

function renderWorkspaces() {
	const strip = clear($("wm-strip"));
	const active = ws.activeSlot();
	for (const entry of ws.list()) {
		// Slots with nothing in them and no preset stay out of the way until
		// something is moved into them.
		if (!entry.tree && entry.slot !== active && !entry.seeded) continue;
		strip.append(el("button", {
			class: `wm-ws${entry.slot === active ? " is-active" : ""}`,
			attrs: { type: "button", role: "tab", "aria-selected": String(entry.slot === active) },
			on: { click: () => ws.switchTo(entry.slot) },
		},
			el("span", { class: "wm-ws-slot", text: String(entry.slot) }),
			el("span", { text: entry.name }),
		));
	}
	markOpenTools();
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
	renderTools(corpus);
	renderWorkspaces();
	refreshSheet(corpus);
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

	$("rail-search").addEventListener("click", () => openPalette());
	$("topbar-search").addEventListener("click", () => openPalette());
}

/* ---------- keyboard ---------- */

/**
 * Every command the keyboard can reach. Declared once here and named in
 * keys.js, so a binding, its cheat-sheet line and the thing it does cannot
 * drift apart.
 */
function commands() {
	return {
		palette: () => openPalette(),
		commands: () => openPalette(),   // the command menu shares the palette shell
		help: () => toggleSheet(),
		dismiss: () => {
			if (!$("wm-sheet-layer").hidden) return toggleSheet(false);
			closePalette();
		},

		open: (id) => openTool(id),
		picker: () => openPalette(),
		close: () => wm.closeWindow(),
		closeAll: () => wm.closeAll(),
		zoom: () => wm.toggleTabsOnFocused(),
		focus: (dir) => wm.moveFocus(dir),
		move: (dir) => wm.moveWindow(dir),
		prevTab: () => wm.cycleTab(-1),
		nextTab: () => wm.cycleTab(1),
		toggleTabs: () => wm.toggleTabsOnFocused(),
		splitRight: () => openPalette(),
		splitDown: () => openPalette(),

		workspace: (n) => ws.switchTo(n),
		resetWorkspace: () => ws.reset(),
	};
}

/* ---------- account, meta ---------- */

async function initAccount() {
	let auth;
	try {
		auth = await api.authState();
	} catch (_) {
		return; // non-fatal: the rest of the app is unaffected
	}
	if (!auth.username) return;

	$("rail-user").textContent = auth.username;
	const button = $("rail-signout");
	button.hidden = false;
	button.addEventListener("click", async () => {
		button.disabled = true;
		try {
			await api.logout();
		} catch (_) { /* the cookie is gone either way; land on the gate */ }
		window.location.assign("/");
	});
}

async function loadMeta() {
	try {
		state.meta = await api.meta();
	} catch (_) {
		return; // non-fatal: the app works without the badge
	}
	const meta = state.meta;
	const counts = (meta.corpora || [])
		.map((c) => `${c.name}: ${c.count.toLocaleString()} entries`)
		.join(" · ");
	$("model-badge").textContent = counts;

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
	await loadPrefs();

	safe("rail", initRail);
	safe("drawer", initDrawer);
	safe("palette", initPalette);
	safe("resolve", initResolve);
	safe("voice", initVoice);
	safe("sheet", initSheet);

	// The window manager, then the layouts it renders.
	safe("wm", () => initWM(state.corpus));
	safe("drag", initDrag);
	safe("keys", () => initKeys(commands(), { corpus: state.corpus }));
	wm.onChange(markOpenTools);

	try {
		await ws.initWorkspaces(state.corpus, renderWorkspaces);
	} catch (err) {
		console.error("workspaces failed to load:", err);
		safe("fallback-chat", () => openTool("chat"));
	}
	safe("tools", () => renderTools(state.corpus));
	safe("workspaces", renderWorkspaces);

	safe("chrome", syncChrome);
	safe("account", initAccount);
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
