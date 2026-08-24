// The canon review queue (MAD-310): the DM's human gate, rendered as a
// post-session ritual. Every item shows the proposed change, the verbatim
// quoted span with its surrounding context, who said it, the adversarial
// rationale and agreement score, and — for open items — Accept / Modify /
// Dismiss. Accept is the only path that writes canon, and the server refuses
// every non-DM caller before this module even renders.

import { $, el, esc, clear, isNarrow } from "./dom.js";
import { api } from "./api.js";

let campaigns = [];
let campaignID = null;
let status = "open";

const KIND_LABEL = {
	proposed_fact: "Fact",
	proposed_event: "Event",
	proposed_discovery: "Discovery",
	proposed_relationship: "Relationship",
	proposed_entity: "Entity",
	low_agreement: "Low agreement",
	contradiction: "Contradiction",
	engine_flag: "Engine flag",
};

export function initReview() {
	$("rail-review").addEventListener("click", openReview);
	$("review-close").addEventListener("click", closeReview);
	$("review-campaign").addEventListener("change", (e) => {
		campaignID = e.target.value || null;
		loadQueue();
	});
	$("review-build").addEventListener("click", onBuild);
	$("review-export").addEventListener("click", onExport);
	$("review-accept-agree").addEventListener("click", onAcceptAgree);
	$("review-filters").addEventListener("click", (e) => {
		const chip = e.target.closest("[data-status]");
		if (!chip) return;
		status = chip.dataset.status;
		for (const c of $("review-filters").children) {
			c.classList.toggle("is-active", c === chip);
			c.setAttribute("aria-selected", c === chip ? "true" : "false");
		}
		loadQueue();
	});
	$("review-list").addEventListener("click", onListClick);
}

export async function openReview() {
	if (isNarrow()) $("app").classList.add("rail-hidden");
	$("review-view").hidden = false;
	$("main").classList.add("is-sessioning");
	await loadCampaigns();
}

export function closeReview() {
	$("review-view").hidden = true;
	$("main").classList.remove("is-sessioning");
	$("rail-review").focus();
}

async function loadCampaigns() {
	let data;
	try {
		data = await api.campaignList();
	} catch (err) {
		renderMeta(err.message, true);
		return;
	}
	campaigns = data.campaigns || [];
	const select = clear($("review-campaign"));
	if (!campaigns.length) {
		select.append(el("option", { text: "No campaigns yet…", attrs: { value: "" } }));
		clear($("review-list")).append(el("p", { class: "sess-empty", text: "Record a campaign and sessions first; the queue builds from their sources." }));
		return;
	}
	for (const c of campaigns) select.append(el("option", { text: c.name, attrs: { value: c.id } }));
	if (!campaigns.some((c) => c.id === campaignID)) campaignID = campaigns[0].id;
	select.value = campaignID;
	await loadQueue();
}

async function loadQueue() {
	if (!campaignID) return;
	let data;
	try {
		// Always fetch the whole queue and filter here: the meta line needs
		// the true open/total counts regardless of the active tab.
		data = await api.reviews(campaignID, "");
	} catch (err) {
		renderMeta(err.message, true);
		return;
	}
	const all = data.reviews || [];
	const reviews = all.filter((r) => r.status === status);
	const wrap = clear($("review-list"));
	for (const r of reviews) wrap.append(reviewCard(r));
	const open = all.filter((r) => r.status === "open").length;
	renderMeta(all.length ? `${open} open · ${all.length} total` : "Queue is clear.");
}

function renderMeta(text, isError) {
	$("review-meta").textContent = text;
	$("review-meta").classList.toggle("is-error", !!isError);
}

async function onBuild() {
	if (!campaignID) return;
	try {
		await api.reviewBuild(campaignID);
		await loadQueue();
	} catch (err) {
		renderMeta(err.message, true);
	}
}

function onExport() {
	if (!campaignID) return;
	window.location.assign(api.reviewExportURL(campaignID, ""));
}

// The one batch affordance: accept every open proposal the adversarial pass
// agreed with at or above the threshold in the input. Low-agreement items,
// contradictions and engine flags still need an individual decision.
async function onAcceptAgree() {
	if (!campaignID) return;
	const threshold = parseFloat($("review-threshold").value);
	if (Number.isNaN(threshold) || threshold < 0 || threshold > 1) {
		renderMeta("Threshold must be a number between 0 and 1.", true);
		return;
	}
	try {
		const res = await api.reviewAcceptAgree(campaignID, threshold);
		const failed = (res.failed || []).length;
		renderMeta(`Batch accepted ${res.accepted}${failed ? ` · ${failed} failed (still open)` : ""}`);
		await loadQueue();
	} catch (err) {
		renderMeta(err.message, true);
	}
}

/* ---------- rendering ---------- */

function reviewCard(r) {
	const card = el("article", { class: "review-card", attrs: { "data-review": r.id } });
	card.dataset.payload = JSON.stringify(r.payload || {});

	card.append(
		el("header", { class: "review-card-head" },
			el("span", { class: "review-kind", attrs: { "data-kind": r.kind }, text: KIND_LABEL[r.kind] || r.kind }),
			el("span", { class: "review-status", attrs: { "data-status": r.status }, text: r.status }),
		),
	);

	if (r.summary) {
		card.append(el("p", { class: "review-summary", text: r.summary }));
	}

	// The verbatim span with its surrounding context, and who said it.
	if (r.quote) {
		const who = [r.source_kind, r.source_author && `by ${r.source_author}`, r.source_title].filter(Boolean).join(" · ");
		card.append(el("blockquote", { class: "review-quote" },
			el("span", { class: "review-context", text: r.context_before || "" }),
			el("mark", { class: "review-span", text: r.quote }),
			el("span", { class: "review-context", text: r.context_after || "" }),
		));
		card.append(el("p", { class: "review-source", text: `“${r.quote}” — ${who} (bytes ${r.span_start}–${r.span_end})` }));
	}

	// The adversarial pass's voice.
	if (r.rationale || r.verdict) {
		card.append(el("p", { class: "review-rationale" },
			el("span", { class: "review-verdict", text: `${r.verdict || "checker"} · agreement ${(r.agreement ?? 0).toFixed(2)} · confidence ${(r.confidence ?? 0).toFixed(2)}` }),
			r.rationale ? el("span", { text: ` — ${r.rationale}` }) : null,
		));
	}

	if (r.status === "open") {
		card.append(reviewActions(r));
	} else {
		const who = r.decided_by || "the DM";
		card.append(el("p", { class: "review-decision", text: `${r.status} by ${who}${r.decision_note ? ` — ${r.decision_note}` : ""}` }));
	}
	return card;
}

function reviewActions(r) {
	const wrap = el("div", { class: "review-actions" });
	const modify = el("div", { class: "review-modify", attrs: { hidden: true } });
	wrap.append(
		el("button", { class: "sess-btn review-accept", text: "Accept as canon", attrs: { type: "button", "data-act": "accept" } }),
		el("button", { class: "sess-btn review-modify-toggle", text: "Modify", attrs: { type: "button", "data-act": "modify-toggle" } }),
		el("button", { class: "sess-btn review-dismiss", text: "Dismiss", attrs: { type: "button", "data-act": "dismiss" } }),
		modify,
	);
	// The modify editor, built lazily on first toggle so the card stays light.
	const noteInput = el("input", { class: "sess-field review-note", attrs: { type: "text", placeholder: "Optional note" } });
	wrap.append(noteInput);
	return wrap;
}

function onListClick(e) {
	const card = e.target.closest("[data-review]");
	if (!card) return;
	const id = card.dataset.review;
	const act = e.target.closest("[data-act]")?.dataset.act;

	if (act === "accept" || act === "dismiss") {
		onDecide(id, act, card);
		return;
	}
	if (act === "modify-toggle") {
		toggleModify(card, id);
		return;
	}
	if (act === "modify-apply") {
		onModify(id, card);
	}
}

async function onDecide(id, decision, card) {
	if (!campaignID) return;
	const note = card.querySelector(".review-note")?.value.trim() || "";
	try {
		await api.reviewDecide(campaignID, id, decision, note);
		await loadQueue();
	} catch (err) {
		renderMeta(err.message, true);
	}
}

function toggleModify(card, id) {
	let box = card.querySelector(".review-modify");
	if (!box.dataset.loaded) {
		// Prefill with the payload as it stands, pretty-printed.
		const current = JSON.stringify(currentPayload(card), null, 2);
		box.dataset.loaded = "1";
		box.append(
			el("textarea", { class: "sess-source-input review-modify-input", text: current, attrs: { rows: 6 } }),
			el("button", { class: "sess-btn review-modify-apply", text: "Apply modify", attrs: { type: "button", "data-act": "modify-apply" } }),
		);
	}
	box.hidden = !box.hidden;
}

// currentPayload reconstructs the payload JSON from the card's rendered
// summary — the server also accepts a full payload on modify, and the real
// source of truth is the item's stored payload. Re-fetch is cleaner than
// guessing, so modify re-reads from the card's data attribute if present.
function currentPayload(card) {
	try {
		return JSON.parse(card.dataset.payload || "{}");
	} catch (_) {
		return {};
	}
}

async function onModify(id, card) {
	if (!campaignID) return;
	const input = card.querySelector(".review-modify-input");
	let payload;
	try {
		payload = JSON.parse(input.value);
	} catch (err) {
		renderMeta(`Payload is not valid JSON: ${err.message}`, true);
		return;
	}
	const note = card.querySelector(".review-note")?.value.trim() || "";
	try {
		await api.reviewDecide(campaignID, id, "modify", note, payload);
		await loadQueue();
	} catch (err) {
		renderMeta(err.message, true);
	}
}
