// Grimoire — entry point. Wires the shell, then hands off to the chat module.

import { $, isNarrow } from "./dom.js";
import { api } from "./api.js";
import { state, loadCorpusPreference } from "./state.js";
import { initDrawer } from "./drawer.js";
import { initPalette, openPalette } from "./palette.js";
import { initChat, refreshHistory, syncChrome, setCorpus, setFoot } from "./chat.js";
import { initResolve } from "./resolve.js";
import { initVoice } from "./voice.js";
import { initStudy } from "./study.js";

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

// initAccount reveals the sign-out control once we know who is signed in. It
// stays hidden when the server runs without accounts, so there is nothing to
// sign out of and nothing to show.
async function initAccount() {
	let state;
	try {
		state = await api.authState();
	} catch (_) {
		return; // non-fatal: the rest of the app is unaffected
	}
	if (!state.username) return;

	$("rail-user").textContent = state.username;
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
		setFoot(`Sage: ${meta.chat_model}`);
	}
}

function start() {
	loadCorpusPreference();
	initRail();
	initDrawer();
	initPalette();
	initChat();
	initResolve();
	initVoice();
	initStudy();
	syncChrome();
	initAccount();
	loadMeta();
	refreshHistory();
	if (!isNarrow()) $("composer-input").focus();
}

if (document.readyState === "loading") {
	document.addEventListener("DOMContentLoaded", start);
} else {
	start();
}
