// The Handouts window (MAD-490, stage 4 of MAD-319): what the DM hands
// the party. Letters as readable cards, maps as images — the party-scope
// reading material, one surface for both seats.
//
// The DM's desk is the compose form, the draft pile and the lifecycle
// buttons; a member's window is the same cards without any of it — the
// rows a player receives are already published-only, decided in the
// server's SQL before the list ever arrives. Nothing here hides
// anything: a draft never reaches this code at a player's seat, so the
// player's Handouts cannot leak what the player's Grimoire cannot.
//
// The pure half — ordering, grouping, which actions apply — lives in
// handoutsvm.js and is tested under node (ADR 14); this module paints
// what it decides.

import { $, el, clear } from "./dom.js";
import { api } from "./api.js";
import { renderMarkdown } from "./markdown.js";
import { handoutActions, groupHandouts, STATUS_LABELS } from "./handoutsvm.js";

let campaigns = [];
let campaignID = null;
let isDM = false;
let wired = false;
let mounted = false;
let pendingImage = null; // the file the compose form holds, uploaded with the draft

/* ---------- painting ---------- */

function paintMeta() {
	const meta = $("handouts-meta");
	if (!campaignID) {
		meta.textContent = "";
		return;
	}
	meta.textContent = isDM
		? "DM — drafts stay yours until you hand them over."
		: "What your DM has handed the party.";
}

function paint(handouts) {
	const host = clear($("handouts-cards"));
	const groups = groupHandouts(handouts);
	for (const group of groups) {
		host.append(sectionEl(group));
	}
	if (!host.childElementCount) {
		host.append(el("p", {
			class: "handouts-hint",
			text: isDM
				? "Nothing drafted yet — write the first letter below."
				: "Nothing has been handed to the party yet.",
		}));
	}
}

function sectionEl(group) {
	const section = el("section", { class: "handouts-section" });
	section.append(el("h3", {
		class: "handouts-kind",
		text: group.kind === "map" ? "Maps" : "Letters & notes",
	}));
	const grid = el("div", { class: "handouts-grid" });
	for (const h of group.rows) grid.append(cardEl(h));
	section.append(grid);
	return section;
}

function cardEl(h) {
	const card = el("article", { class: "handout-card" + (h.status !== "published" ? " is-unpublished" : "") });

	const head = el("div", { class: "handout-head" });
	head.append(el("b", { class: "handout-title", text: h.title }));
	if (isDM) {
		head.append(el("span", { class: "handout-status is-" + h.status, text: STATUS_LABELS[h.status] || h.status }));
	}
	card.append(head);

	if (h.image_url) {
		const img = el("img", {
			class: "handout-image",
			attrs: { src: h.image_url, alt: h.title, loading: "lazy" },
		});
		card.append(img);
	}
	if (h.body) {
		const body = el("div", { class: "handout-body" });
		body.innerHTML = renderMarkdown(h.body, { rules: false, mana: false });
		card.append(body);
	}
	if (h.published_at) {
		card.append(el("p", {
			class: "handout-stamp",
			text: "handed to the party " + new Date(h.published_at).toLocaleDateString(),
		}));
	}

	const actions = handoutActions(h, isDM);
	if (actions.length) {
		const row = el("div", { class: "handout-actions" });
		for (const action of actions) {
			row.append(el("button", {
				class: "enc-btn handout-act" + (action === "retire" ? " danger" : ""),
				type: "button",
				text: actionLabel(action),
				attrs: { "data-act": action, "data-id": h.id },
			}));
		}
		if (h.status !== "published") {
			row.append(el("button", {
				class: "enc-btn handout-act danger", type: "button", text: "delete",
				attrs: { "data-act": "delete", "data-id": h.id },
			}));
		}
		card.append(row);
	}
	return card;
}

function actionLabel(action) {
	if (action === "publish") return "hand it to the party";
	if (action === "unpublish") return "take it back to draft";
	return "retire";
}

/* ---------- wiring ---------- */

function wire() {
	$("handouts-campaign").addEventListener("change", async () => {
		const id = $("handouts-campaign").value;
		if (!id || id === campaignID) return;
		campaignID = id;
		localStorage.setItem("grimoire-handouts-campaign", id);
		await load();
	});

	$("handout-compose").addEventListener("submit", async (e) => {
		e.preventDefault();
		const title = $("handout-title-input").value;
		if (!title.trim()) {
			$("handout-note").textContent = "a handout needs a title";
			return;
		}
		const file = pendingImage;
		if ($("handout-kind-input").value === "map" && !file && !$("handout-body-input").value.trim()) {
			$("handout-note").textContent = "a map needs its image (or at least notes) — attach one below";
			return;
		}
		let data;
		try {
			data = await api.handoutCreate(campaignID, {
				kind: $("handout-kind-input").value,
				title,
				body: $("handout-body-input").value,
			});
		} catch (err) {
			$("handout-note").textContent = err.message;
			return;
		}
		const handout = data.handout;
		if (file) {
			try {
				const after = await api.handoutImage(campaignID, handout.id, file);
				handout.image_url = after.handout.image_url;
			} catch (err) {
				$("handout-note").textContent = `draft saved, but the image did not land: ${err.message}`;
			}
			pendingImage = null;
		}
		$("handout-compose").reset();
		$("handout-file-name").textContent = "";
		$("handout-note").textContent = "drafted — publish it when the party earns it";
		setTimeout(() => ($("handout-note").textContent = ""), 4000);
		await load();
	});

	$("handout-file").addEventListener("change", () => {
		pendingImage = $("handout-file").files[0] || null;
		$("handout-file-name").textContent = pendingImage ? pendingImage.name : "";
	});

	$("handouts-cards").addEventListener("click", async (e) => {
		const btn = e.target.closest(".handout-act");
		if (!btn) return;
		const id = btn.dataset.id;
		const act = btn.dataset.act;
		btn.disabled = true;
		try {
			if (act === "delete") await api.handoutDelete(campaignID, id);
			else await api.handoutStatus(campaignID, id, act);
			await load();
		} catch (err) {
			$("handout-note").textContent = err.message;
			btn.disabled = false;
		}
	});
}

async function loadCampaigns() {
	let data;
	try {
		data = await api.campaignList();
	} catch (err) {
		$("handouts-meta").textContent = err.message;
		return;
	}
	campaigns = data.campaigns || [];
	const sel = clear($("handouts-campaign"));
	if (!campaigns.length) {
		sel.append(el("option", { text: "No campaigns yet…", attrs: { value: "" } }));
		$("handouts-body").hidden = true;
		$("handouts-empty").hidden = false;
		return;
	}
	$("handouts-body").hidden = false;
	$("handouts-empty").hidden = true;
	for (const c of campaigns) sel.append(el("option", { text: c.name, attrs: { value: c.id } }));
	const stored = localStorage.getItem("grimoire-handouts-campaign");
	if (campaigns.some((c) => c.id === stored)) campaignID = stored;
	else if (!campaigns.some((c) => c.id === campaignID)) campaignID = campaigns[0].id;
	sel.value = campaignID;
	await load();
}

async function load() {
	if (!campaignID) return;
	const selected = campaigns.find((c) => c.id === campaignID);
	isDM = !!(selected && (selected.my_role === "dm" || selected.my_role === "keeper"));
	$("handout-compose").hidden = !isDM;
	paintMeta();
	let data;
	try {
		data = await api.handoutList(campaignID);
	} catch (err) {
		$("handouts-meta").textContent = err.message;
		return;
	}
	paint(data.handouts || []);
}

/* ---------- the window-manager contract ---------- */

export const tool = {
	mount(host) {
		const view = $("handouts-view");
		host.append(view);
		view.hidden = false;
		mounted = true;
		if (!wired) {
			wire();
			wired = true;
		}
		if (!campaigns.length) loadCampaigns();
		else {
			const sel = $("handouts-campaign");
			if (sel.value !== campaignID) sel.value = campaignID;
			load();
		}
		return {
			destroy() {
				mounted = false;
			},
		};
	},
};
