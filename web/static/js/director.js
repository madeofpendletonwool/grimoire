// The encounter director panel (MAD-484, stage 3 of MAD-318): the
// advisory surface beside the tracker. The engine is fully landed
// (MAD-427) — given the live battle it suggests what these monsters
// would plausibly do next and why, every suggestion carrying the state
// that justifies it. This window is that advice on the DM's screen and
// nothing more.
//
// Advisory only, by construction: the one combat call this module wires
// is the existing director route, a POST that writes nothing. No
// damage, no next-turn, no reveal — no write wrapper is imported here,
// so no code path in this panel can change battle state. The DM reads,
// the DM decides, the tracker does.
//
// The design constraint from MAD-318, verbatim: advisory content must
// not crowd the controls the DM is mid-tap on. The panel rides behind
// the board's tab in the play layout — beside the tracker, one tap
// from it, never over it. What it shows is reading material: the
// action line in display type, every citation in full, and the honest
// tail (dropped, caveats, model) rendered, not tucked away.

import { $, el, clear } from "./dom.js";
import { api } from "./api.js";
import { suggestionRows, transparency, combatLine } from "./directorvm.js";

let campaigns = [];
let campaignID = null;
let wired = false;
let mounted = false;

// The last advisory pass: the director route's body, kept for repaints.
let advice = null;
// One pass at a time — the model can take a while, and the route is
// the panel's whole budget.
let asking = false;

/* ---------- campaigns and boot ---------- */

async function loadCampaigns() {
	let data;
	try {
		data = await api.listCampaigns();
	} catch (err) {
		$("dir-meta").textContent = err.message;
		return;
	}
	campaigns = (data.campaigns || []).filter((c) => c.my_role === "dm" || c.my_role === "keeper");
	const sel = clear($("dir-campaign"));
	if (!campaigns.length) {
		sel.append(el("option", { text: "No campaigns yet…", attrs: { value: "" } }));
		$("dir-empty-title").textContent = "No campaign yet";
		$("dir-empty-text").textContent = "The director advises at a table. Found a campaign first, or take a seat with an invite.";
		$("dir-body").hidden = true;
		$("dir-empty").hidden = false;
		return;
	}
	$("dir-empty").hidden = true;
	$("dir-body").hidden = false;
	for (const c of campaigns) sel.append(el("option", { text: c.name, attrs: { value: c.id } }));
	const stored = localStorage.getItem("grimoire-director-campaign");
	if (campaigns.some((c) => c.id === stored)) campaignID = stored;
	else if (!campaigns.some((c) => c.id === campaignID)) campaignID = campaigns[0].id;
	sel.value = campaignID;
	renderAdvice();
}

/* ---------- the one call ---------- */

async function onAsk(e) {
	e.preventDefault();
	if (!campaignID || asking) return;
	const question = $("dir-question").value.trim();
	setAsking(true, question);
	try {
		const body = await api.combatDirector(campaignID, question);
		advice = body;
		$("dir-note").textContent = "";
		renderAdvice();
	} catch (err) {
		// A failed pass leaves the last advice standing — the note says
		// what went wrong; the reading material stays the reading
		// material.
		$("dir-note").textContent = err.message;
	} finally {
		setAsking(false);
	}
}

function setAsking(on, question = "") {
	asking = on;
	const btn = $("dir-suggest");
	btn.disabled = on;
	btn.textContent = on ? "Reading…" : "Suggest";
	if (on) {
		$("dir-note").textContent = question
			? `Reading the battle — “${question}”…`
			: "Reading the battle…";
	}
}

/* ---------- painting ---------- */

function renderAdvice() {
	if (!mounted) return;
	const host = clear($("dir-advice"));
	$("dir-rest").hidden = !!advice;
	if (!advice) return;

	const t = transparency(advice);
	host.append(el("p", { class: "dir-battle", text: combatLine(advice.combat) }));
	if (advice.question) host.append(el("p", { class: "dir-echo", text: `You asked: ${advice.question}` }));

	const rows = suggestionRows(advice);
	for (const r of rows) host.append(suggestionCard(r));
	if (!rows.length) {
		// The gate caught everything, or the model offered nothing —
		// either way the panel says so plainly.
		host.append(el("p", {
			class: "dir-none",
			text: t.dropped > 0
				? "Nothing survived the gate this pass — every suggestion lacked a citable basis."
				: "The director offered nothing this pass. Ask again, or focus the question.",
		}));
	}
	host.append(stripEl(t));
}

function suggestionCard(r) {
	const card = el("article", { class: "dir-card" });
	card.append(el("b", { class: "dir-actor", text: r.actor || "someone" }));
	if (r.action) card.append(el("p", { class: "dir-action", text: r.action }));
	if (r.reasoning) card.append(el("p", { class: "dir-why", text: r.reasoning }));
	// The basis in full: every citation shown, statblock text and live
	// state told apart by their badges — never collapsed away.
	const list = el("ul");
	for (const c of r.citations) {
		list.append(el("li", { class: `dir-cite is-${c.kind}` },
			el("b", { class: "dir-cite-id", text: c.id ? `[${c.id}]` : "•" }),
			el("span", { class: "dir-cite-body" },
				c.source ? el("span", { class: "dir-cite-src", text: `${c.source} — ` }) : null,
				el("span", { class: "dir-cite-text", text: c.text }))));
	}
	card.append(el("div", { class: "dir-cites" },
		el("h4", { class: "dir-cites-label", text: "the state that justifies it" }),
		list));
	return card;
}

/** The honest tail: what the gate caught, what the grounding could not
    read, and which model did the talking. Rendered, not hidden. */
function stripEl(t) {
	const strip = el("footer", { class: "dir-strip" });
	if (t.model) strip.append(el("span", { class: "dir-model", text: `advised by ${t.model}` }));
	if (t.droppedNote) strip.append(el("span", { class: "dir-dropped", text: t.droppedNote }));
	if (t.caveats.length) {
		const list = el("ul", { class: "dir-caveats" });
		for (const c of t.caveats) list.append(el("li", { class: "dir-caveat", text: c }));
		strip.append(list);
	}
	if (!strip.childElementCount) strip.hidden = true;
	return strip;
}

/* ---------- wiring ---------- */

function wire() {
	$("dir-campaign").addEventListener("change", async (e) => {
		if (!e.target.value) return;
		campaignID = e.target.value;
		localStorage.setItem("grimoire-director-campaign", campaignID);
		advice = null;
		$("dir-note").textContent = "";
		renderAdvice();
	});
	$("dir-ask").addEventListener("submit", onAsk);
}

/* ---------- the window-manager contract ---------- */

export const tool = {
	mount(host) {
		const view = $("director-view");
		host.append(view);
		view.hidden = false;
		mounted = true;
		if (!wired) {
			wire();
			wired = true;
		}
		if (!campaigns.length) loadCampaigns();
		else renderAdvice();
		return {
			destroy() {
				mounted = false;
			},
		};
	},
};
