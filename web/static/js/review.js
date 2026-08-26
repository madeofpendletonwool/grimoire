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

// The generators that stage proposal batches (MAD-359). A batch is a review,
// rendered bigger: header, items grouped by kind, accept-all / dismiss-all,
// per-item override.
const SOURCE_LABEL = {
	skeleton: "Campaign skeleton",
	story_plan: "Story plan",
	scene: "Scene",
	nl_command: "Command",
	session_prep: "Session prep",
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

// openReviewFor opens the queue aimed at one campaign — the skeleton
// generator's hand-off: the batch it staged is the review the DM owes.
export async function openReviewFor(id) {
	if (isNarrow()) $("app").classList.add("rail-hidden");
	$("review-view").hidden = false;
	$("main").classList.add("is-sessioning");
	campaignID = id;
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
	// Open batches render as one grouped card each (a batch is a review,
	// rendered bigger); their items are excluded from the individual list
	// so nothing shows twice. On the decided tabs the items render
	// individually like every other queue item.
	let batchCards = [];
	const inOpenBatch = new Set();
	if (status === "open") {
		try {
			const list = (await api.proposals(campaignID, "open")).batches || [];
			for (const b of list) {
				const full = (await api.proposal(campaignID, b.id)).batch;
				if (!full || !full.items) continue;
				for (const it of full.items) inOpenBatch.add(it.id);
				batchCards.push(batchCard(full));
			}
		} catch (err) {
			renderMeta(err.message, true);
			return;
		}
	}
	const reviews = all.filter((r) => r.status === status && !inOpenBatch.has(r.id));
	const wrap = clear($("review-list"));
	for (const c of batchCards) wrap.append(c);
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

// batchCard renders one proposal batch: the generator's identity and
// premise, its items grouped by kind, and the batch decisions. Per-item
// overrides (modify, dismiss) ride on the item rows and go through the
// batch decision endpoint so dismissed dependencies refuse their
// dependents, exactly as the store promises.
function batchCard(b) {
	const card = el("article", { class: "review-card review-batch-card", attrs: { "data-batch": b.id } });
	card.append(
		el("header", { class: "review-card-head" },
			el("span", { class: "review-kind", attrs: { "data-kind": "proposed_entity" }, text: SOURCE_LABEL[b.source] || b.source }),
			el("span", { class: "review-status", attrs: { "data-status": b.status }, text: `${b.status} · ${b.open_count}/${b.item_count} open` }),
		),
	);
	if (b.prompt) card.append(el("p", { class: "review-summary", text: b.prompt }));

	const groups = new Map();
	for (const it of b.items || []) {
		if (!groups.has(it.kind)) groups.set(it.kind, []);
		groups.get(it.kind).push(it);
	}
	for (const [kind, items] of groups) {
		card.append(el("p", { class: "review-batch-group", text: `${KIND_LABEL[kind] || kind} · ${items.length}` }));
		for (const it of items) card.append(batchItemRow(it));
	}

	card.append(el("div", { class: "review-actions" },
		el("button", { class: "sess-btn review-accept", text: "Accept all", attrs: { type: "button", "data-act": "batch-accept" } }),
		el("button", { class: "sess-btn review-dismiss", text: "Dismiss all", attrs: { type: "button", "data-act": "batch-dismiss" } }),
	));
	return card;
}

// batchItemRow is one proposed object inside a batch. Open items carry the
// per-item override controls; decided ones show what happened to them.
function batchItemRow(it) {
	const row = el("div", { class: "review-batch-item", attrs: { "data-review": it.id } });
	row.dataset.payload = JSON.stringify(it.payload || {});
	row.append(el("span", { class: "review-batch-item-summary", text: it.summary || it.subject }));
	if (it.status === "open") {
		row.append(
			el("button", { class: "sess-btn review-modify-toggle", text: "Modify", attrs: { type: "button", "data-act": "modify-toggle" } }),
			el("button", { class: "sess-btn review-dismiss", text: "Dismiss", attrs: { type: "button", "data-act": "item-dismiss" } }),
			el("div", { class: "review-modify", attrs: { hidden: true } }),
		);
	} else {
		const note = it.decision_note ? ` — ${it.decision_note}` : "";
		row.append(el("span", { class: "review-batch-item-status", text: `${it.status}${note}` }));
	}
	return row;
}

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
	const act = e.target.closest("[data-act]")?.dataset.act;

	// Inside a batch card the decisions are batch decisions: per-item
	// overrides travel through the batch endpoint so a dismissed
	// dependency refuses its dependents instead of failing later.
	const batch = e.target.closest("[data-batch]");
	if (batch) {
		const item = e.target.closest("[data-review]");
		if (item) {
			if (act === "item-dismiss") {
				onBatchItemDecision(batch.dataset.batch, item.dataset.review, "dismiss");
				return;
			}
			if (act === "modify-toggle") {
				toggleModify(item, item.dataset.review);
				return;
			}
			if (act === "modify-apply") {
				onBatchItemModify(batch.dataset.batch, item);
				return;
			}
			return;
		}
		if (act === "batch-accept") {
			onBatchDecide(batch.dataset.batch, "accept");
			return;
		}
		if (act === "batch-dismiss") {
			onBatchDecide(batch.dataset.batch, "dismiss");
			return;
		}
		return;
	}

	const card = e.target.closest("[data-review]");
	if (!card) return;
	const id = card.dataset.review;

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

/* ---------- batch decisions ---------- */

async function onBatchDecide(batchID, decision) {
	if (!campaignID) return;
	try {
		const res = await api.proposalDecide(campaignID, batchID, decision, []);
		renderBatchResult(res);
		await loadQueue();
	} catch (err) {
		renderMeta(err.message, true);
	}
}

// onBatchItemDecision records one per-item override — a dismiss drops that
// item and everything depending on it — and accepts the rest of the batch.
async function onBatchItemDecision(batchID, itemID, decision) {
	if (!campaignID) return;
	try {
		const res = await api.proposalDecide(campaignID, batchID, "accept", [{ item_id: itemID, decision }]);
		renderBatchResult(res);
		await loadQueue();
	} catch (err) {
		renderMeta(err.message, true);
	}
}

async function onBatchItemModify(batchID, item) {
	if (!campaignID) return;
	const input = item.querySelector(".review-modify-input");
	let payload;
	try {
		payload = JSON.parse(input.value);
	} catch (err) {
		renderMeta(`Payload is not valid JSON: ${err.message}`, true);
		return;
	}
	try {
		const res = await api.proposalDecide(campaignID, batchID, "accept", [{ item_id: item.dataset.review, decision: "modify", payload }]);
		renderBatchResult(res);
		await loadQueue();
	} catch (err) {
		renderMeta(err.message, true);
	}
}

// renderBatchResult summarizes what a batch decision did, naming the
// refusals — "depends on X, which was dismissed" — rather than letting
// them cascade silently.
function renderBatchResult(res) {
	const counts = {};
	const refused = [];
	for (const it of res.items || []) {
		counts[it.status] = (counts[it.status] || 0) + 1;
		if (it.reason && it.status !== "accepted" && it.status !== "modified") {
			refused.push(`${it.subject || it.review_id}: ${it.reason}`);
		}
	}
	const parts = Object.entries(counts).map(([k, v]) => `${v} ${k}`);
	let text = `Batch ${res.batch?.status ?? "?"} — ${parts.join(", ")}`;
	if (refused.length) text += ` · refused: ${refused.join("; ")}`;
	renderMeta(text);
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
