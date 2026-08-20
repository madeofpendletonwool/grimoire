// Sharing an answer out: the share control under each sage answer, the
// copied-link toast, and the Settings list of the links this keeper keeps.
//
// A share is a snapshot taken at share time, so the button needs nothing but
// the chat and message ids — the page it produces stands on its own even
// after the conversation is gone.

import { el, clear, truncate } from "./dom.js";
import { api } from "./api.js";
import { gi } from "./icons.js";
import { addSettingsSection, rebuildSettings } from "./scene.js";

/** A share affordance for one assistant message. Returns null when there is
 *  nothing to share from (no saved message id — a stream that never
 *  finished). */
export function shareButton(chatID, messageID) {
	if (!chatID || !messageID) return null;
	const btn = el("button", {
		class: "msg-action",
		attrs: { type: "button", "aria-label": "Share this answer", title: "Share this answer" },
	}, gi("share"), el("span", { text: "Share" }));
	btn.addEventListener("click", async () => {
		btn.disabled = true;
		try {
			const data = await api.shareMessage(chatID, messageID);
			const url = new URL(data.url, window.location.href).href;
			await copyToClipboard(url);
			toast("Link copied");
		} catch (err) {
			toast(err.message || "Could not create the link", true);
		} finally {
			btn.disabled = false;
		}
	});
	return btn;
}

async function copyToClipboard(text) {
	try {
		await navigator.clipboard.writeText(text);
	} catch (_) {
		// clipboard may be unavailable (non-secure context); a text prompt is
		// clunky but always works.
		window.prompt("Copy this link:", text);
	}
}

let toastTimer = null;

/** A small fixed notice — "Link copied" and its failures. One at a time. */
function toast(text, warn) {
	let node = document.querySelector(".toast");
	if (!node) {
		node = el("div", { class: "toast", attrs: { role: "status" } });
		document.body.append(node);
	}
	node.textContent = text;
	node.classList.toggle("warn", !!warn);
	node.classList.add("is-shown");
	clearTimeout(toastTimer);
	toastTimer = setTimeout(() => node.classList.remove("is-shown"), 2200);
}

/* ---------- the Settings list ---------- */

let section = null; // the mounted section node, swapped on every popup rebuild
let shares = []; // most recent list, so a rebuild re-renders without a refetch

/** initShares adds the shared-links section to Settings for whoever can use
 *  it. Like the admin section, it hides itself when the feature is off (the
 *  server answers 503 when no share store is wired). */
export async function initShares() {
	try {
		const data = await api.listShares();
		shares = data.shares || [];
	} catch (_) {
		return; // shared links not available — no section at all
	}
	addSettingsSection(buildSection);
	rebuildSettings();
}

function buildSection() {
	section = el("div", { class: "set-group set-shares" });
	render();
	return section;
}

function render() {
	if (!section) return;
	clear(section);
	section.append(el("p", { class: "set-label", text: "Shared links" }));
	if (shares.length === 0) {
		section.append(el("p", { class: "set-hint", text: "Answers you share get public, read-only links — listed here until revoked." }));
		return;
	}
	for (const sh of shares) {
		section.append(shareRow(sh));
	}
}

function shareRow(sh) {
	const revoked = !!sh.revoked_at;
	const row = el("div", { class: "share-row" + (revoked ? " is-revoked" : "") });

	row.append(el("span", {
		class: "share-status" + (revoked ? " is-revoked" : ""),
		text: revoked ? "revoked" : "live",
	}));
	row.append(el("a", {
		class: "share-q",
		text: truncate(sh.question || "an answer", 46) || "an answer",
		attrs: { href: sh.url, target: "_blank", rel: "noopener", title: sh.question || "" },
	}));

	row.append(el("span", { class: "share-date", text: when(sh.created_at) }));

	if (!revoked) {
		const revoke = el("button", { class: "invite-revoke", attrs: { type: "button" }, text: "Revoke" });
		revoke.addEventListener("click", async () => {
			revoke.disabled = true;
			try {
				await api.revokeShare(sh.token);
				sh.revoked_at = new Date().toISOString();
				render();
			} catch (err) {
				revoke.disabled = false;
				toast(err.message || "Could not revoke the link", true);
			}
		});
		row.append(revoke);
	}
	return row;
}

// when renders an ISO timestamp as a short, locale-friendly date.
function when(iso) {
	const d = new Date(iso);
	if (isNaN(d.getTime())) return "";
	return d.toLocaleDateString(undefined, { month: "short", day: "numeric" });
}
