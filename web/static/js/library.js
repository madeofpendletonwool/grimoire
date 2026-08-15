// The admin's library control. Lives as a section in the settings popup beside
// the invitations, and only renders for the install's admin (the first
// keeper). Rebuilding re-fetches the rules corpora and re-reads the local
// D&D books, so adding a book is a button, not a terminal. A boot already
// rebuilds on its own when the books change; this is for "do it now anyway".

import { el, clear } from "./dom.js";
import { api } from "./api.js";
import { addSettingsSection, rebuildSettings } from "./scene.js";

let section = null; // the mounted section node, swapped on every popup rebuild
let poll = null; // status poller while a rebuild runs

/** initLibrary hides itself unless the signed-in user is the admin. */
export async function initLibrary() {
	let state;
	try {
		state = await api.authState();
	} catch (_) {
		return; // non-fatal: the rest of the app is unaffected
	}
	if (!state || !state.is_admin) return;

	addSettingsSection(buildSection);
	// An in-flight rebuild started by a previous page load should still show
	// as running here, so the status is checked whenever the popup is built.
	rebuildSettings();
}

function buildSection() {
	section = el("div", { class: "set-group set-library" });
	render({ running: false });
	// Best-effort initial status: a rebuild kicked off earlier keeps polling.
	api.reindexStatus().then((st) => {
		render(st);
		if (st && st.running) startPolling();
	}).catch(() => {});
	return section;
}

function render(status) {
	if (!section) return;
	clear(section);
	section.append(el("p", { class: "set-label", text: "Library" }));

	const running = !!(status && status.running);
	const button = el("button", {
		class: "set-opt",
		attrs: {
			type: "button",
			disabled: running ? "" : undefined,
			title: running ? "A rebuild is already running" : "Re-fetch the rules and re-read the local D&D books",
		},
		text: running ? "Rebuilding… (takes a minute or two)" : "Rebuild the index now",
	});
	button.addEventListener("click", startRebuild);
	section.append(button);

	section.append(el("p", {
		class: "set-hint",
		text: "The index rebuilds itself when the local books change; this covers rule updates and anything else.",
	}));

	if (status && status.error) {
		section.append(el("p", { class: "set-hint warn", text: `Last rebuild failed: ${status.error}` }));
	} else if (status && status.finished_at && !running) {
		section.append(el("p", { class: "set-hint", text: `Last rebuilt ${when(status.finished_at)}.` }));
	}
}

async function startRebuild() {
	try {
		const st = await api.reindexStart();
		render(st);
		if (st && st.running) startPolling();
	} catch (err) {
		alert((err && err.message) || "Could not start the rebuild.");
		render({ running: false });
	}
}

// startPolling refreshes the status every few seconds until the rebuild
// finishes, so the section settles on its own without a page reload.
function startPolling() {
	stopPolling();
	poll = setInterval(async () => {
		try {
			const st = await api.reindexStatus();
			render(st);
			if (!st || !st.running) stopPolling();
		} catch (_) {
			stopPolling();
		}
	}, 4000);
}

function stopPolling() {
	if (poll) clearInterval(poll);
	poll = null;
}

// when renders an RFC3339 timestamp as a short, locale-friendly stamp.
function when(iso) {
	const d = new Date(iso);
	if (isNaN(d.getTime())) return "";
	return d.toLocaleString(undefined, { month: "short", day: "numeric", hour: "numeric", minute: "numeric" });
}
