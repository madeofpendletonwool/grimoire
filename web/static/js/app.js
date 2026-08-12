// Grimoire — entry point. Wires the shell, then hands off to the chat module.

import { $, isNarrow } from "./dom.js";
import { api } from "./api.js";
import { state, loadCorpusPreference } from "./state.js";
import { initDrawer } from "./drawer.js";
import { initPalette, openPalette } from "./palette.js";
import { initChat, refreshHistory, syncChrome, setCorpus, setFoot } from "./chat.js";

function initRail() {
	const app = $("app");
	const setHidden = (hidden) => app.classList.toggle("rail-hidden", hidden);

	$("rail-collapse").addEventListener("click", () => setHidden(true));
	$("rail-open").addEventListener("click", () => setHidden(false));
	$("rail-scrim").addEventListener("click", () => setHidden(true));

	// On a narrow screen the sidebar is an overlay, so it starts closed.
	setHidden(isNarrow());
	window.addEventListener("resize", () => {
		if (isNarrow()) setHidden(true);
	});

	document.querySelectorAll(".corpus-opt").forEach((btn) => {
		btn.addEventListener("click", () => setCorpus(btn.dataset.corpus));
	});

	$("rail-search").addEventListener("click", () => openPalette());
	$("topbar-search").addEventListener("click", () => openPalette());
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
		setFoot(`Sage: ${meta.chat_model}`);
	}
}

function start() {
	loadCorpusPreference();
	initRail();
	initDrawer();
	initPalette();
	initChat();
	syncChrome();
	loadMeta();
	refreshHistory();
	if (!isNarrow()) $("composer-input").focus();
}

if (document.readyState === "loading") {
	document.addEventListener("DOMContentLoaded", start);
} else {
	start();
}
