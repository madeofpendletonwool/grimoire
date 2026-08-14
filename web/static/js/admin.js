// The admin's invitation manager. Lives as a section in the settings popup, and
// only renders for the install's admin (the first keeper). Self-service account
// creation stays off; this is the one way to let friends in — mint a single-use
// invite link, hand it over, and watch it spend.

import { el, clear } from "./dom.js";
import { api } from "./api.js";
import { addSettingsSection, rebuildSettings } from "./scene.js";

let section = null; // the mounted section node, swapped on every popup rebuild
let invites = []; // most recent list, so a rebuild re-renders without a refetch

/** initAdmin hides itself unless the signed-in user is the admin. */
export async function initAdmin() {
	let state;
	try {
		state = await api.authState();
	} catch (_) {
		return; // non-fatal: the rest of the app is unaffected
	}
	if (!state || !state.is_admin) return;

	addSettingsSection(buildSection);
	try {
		const data = await api.listInvites();
		invites = data.invites || [];
	} catch (_) {
		invites = [];
	}
	rebuildSettings();
}

function buildSection() {
	section = el("div", { class: "set-group set-invites" });
	render();
	return section;
}

function render() {
	if (!section) return;
	clear(section);
	section.append(el("p", { class: "set-label", text: "Invitations" }));
	section.append(newInviteRow());
	section.append(el("p", { class: "set-hint", text: "Each link lets one friend make an account, then it spends." }));
	for (const inv of invites) {
		section.append(inviteRow(inv));
	}
}

function newInviteRow() {
	const button = el("button", {
		class: "set-opt invite-new",
		attrs: { type: "button" },
		text: "Mint a new invite link",
	});
	button.addEventListener("click", async () => {
		button.disabled = true;
		try {
			const data = await api.createInvite("");
			invites = [data.invite, ...invites];
			render();
			flashNewLink();
		} catch (err) {
			button.disabled = false;
			alert(err.message || "Could not create the invite.");
		}
	});
	return el("div", { class: "invite-new-row" }, button);
}

// flashNewLink points the freshly minted link out so the admin notices the one
// code they will ever see for it.
function flashNewLink() {
	if (!section) return;
	const link = section.querySelector(".invite-link.fresh");
	if (!link) return;
	link.scrollIntoView({ block: "nearest" });
	const copy = link.querySelector(".invite-copy");
	if (copy) copy.focus();
}

function inviteRow(inv) {
	const status = inv.status || "pending";
	const row = el("div", { class: "invite-row" });

	const head = el("div", { class: "invite-head" },
		el("span", { class: `invite-status is-${status}`, text: status }),
		el("span", { class: "invite-note", text: inv.note || "" }),
	);

	// The raw link is present only on a freshly created invite (the list never
	// carries the code). Show it with a copy affordance and a one-time warning.
	if (inv.code && inv.url) {
		const link = el("div", { class: "invite-link fresh" },
			el("input", {
				class: "invite-url",
				attrs: { type: "text", value: inv.url, readonly: "", "aria-label": "Invite link" },
				on: { focus: (e) => e.target.select() },
			}),
			copyButton(inv.url),
		);
		row.append(head, link, el("p", { class: "invite-once", text: "Copy this now — it is not shown again." }));
		return row;
	}

	const meta = el("p", { class: "invite-meta", text: describe(inv) });
	row.append(head, meta);

	// A pending invite can still be closed; spent/expired ones only get trimmed.
	if (status === "pending") {
		row.append(revokeButton(inv));
	}
	return row;
}

function copyButton(url) {
	const btn = el("button", { class: "invite-copy", attrs: { type: "button" }, text: "Copy" });
	btn.addEventListener("click", async () => {
		try {
			await navigator.clipboard.writeText(url);
		} catch (_) {
			// clipboard may be unavailable (non-secure context); fall back to
			// selecting the adjacent field so a manual copy works.
			const field = btn.parentElement?.querySelector(".invite-url");
			if (field) field.focus();
		}
		btn.textContent = "Copied";
		setTimeout(() => { btn.textContent = "Copy"; }, 1500);
	});
	return btn;
}

function revokeButton(inv) {
	const btn = el("button", { class: "invite-revoke", attrs: { type: "button" }, text: "Revoke" });
	btn.addEventListener("click", async () => {
		btn.disabled = true;
		try {
			await api.revokeInvite(inv.id);
			invites = invites.filter((i) => i.id !== inv.id);
			render();
		} catch (err) {
			btn.disabled = false;
			alert(err.message || "Could not revoke the invite.");
		}
	});
	return btn;
}

function describe(inv) {
	const parts = [];
	if (inv.created_at) parts.push(`made ${when(inv.created_at)}`);
	if (inv.expires_at && statusOf(inv) === "pending") parts.push(`expires ${when(inv.expires_at)}`);
	if (inv.used_at) parts.push(`used ${when(inv.used_at)}`);
	return parts.join(" · ");
}

function statusOf(inv) {
	return inv.status || "pending";
}

// when renders an ISO timestamp as a short, locale-friendly date. The API sends
// RFC3339; toLocaleDateString with no time is enough for an audit list.
function when(iso) {
	const d = new Date(iso);
	if (isNaN(d.getTime())) return "";
	return d.toLocaleDateString(undefined, { month: "short", day: "numeric" });
}
