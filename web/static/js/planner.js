// The story planner: the narrative spine in the UI — acts as columns,
// scenes as cards, cast and secrets as chips. Vanilla JS matching
// campaign.js and sessions.js; the server owns everything that must be
// right (vocabularies, quest-edge checks, scope), so this module is honest
// plumbing: build the spine through the API, render what comes back, and
// keep the DM's hands on the table.
//
// DM-only by construction: the endpoints behind it refuse every other
// scope, and a player opening this view sees the planner's empty state.

import { $, el, clear, isNarrow } from "./dom.js";
import { api } from "./api.js";
import { state } from "./state.js";

let campaignID = null;   // the campaign whose spine is showing
let campaigns = [];
let spine = null;        // the last loaded /story payload
let picker = null;       // the scene editor's entity picker data

const KINDS = {
	social: "social",
	exploration: "exploration",
	combat: "combat",
	revelation: "revelation",
	downtime: "downtime",
	travel: "travel",
};

const ROLES = { focus: "focus", present: "present", offstage: "offstage", mentioned: "mentioned" };
const DISPOSITIONS = { in_play: "in play", revealed_if: "revealed if…", withheld: "withheld" };

export function initPlanner() {
	$("rail-planner").addEventListener("click", openPlanner);
	$("planner-close").addEventListener("click", closePlanner);
	$("planner-campaign").addEventListener("change", onPickCampaign);
	$("planner-new-campaign").addEventListener("click", onNewCampaign);
	$("planner-shape").addEventListener("change", onPickShape);
	$("planner-add-act").addEventListener("click", onQuickAct);
	$("planner-spine").addEventListener("click", onSpineClick);
}

/** Open the planner. */
export async function openPlanner() {
	if (isNarrow()) $("app").classList.add("rail-hidden");
	$("planner-view").hidden = false;
	$("main").classList.add("is-sessioning");
	await loadCampaigns();
}

/** Drop back to the transcript. */
export function closePlanner() {
	$("planner-view").hidden = true;
	$("main").classList.remove("is-sessioning");
	$("rail-planner").focus();
}

/* ---------- campaigns ---------- */

async function loadCampaigns() {
	try {
		const body = await api.listCampaigns();
		campaigns = body.campaigns || [];
	} catch (err) {
		renderError(err);
		return;
	}
	if (!campaigns.length) {
		campaignID = null;
		renderPicker();
		renderEmpty("No campaigns yet — press + to create one, then plan its spine.");
		return;
	}
	if (!campaigns.some((c) => c.id === campaignID)) campaignID = campaigns[0].id;
	renderPicker();
	await loadSpine();
}

function renderPicker() {
	const sel = clear($("planner-campaign"));
	for (const c of campaigns) {
		sel.append(el("option", { text: c.name, attrs: { value: c.id } }));
	}
	sel.value = campaignID || "";
	sel.disabled = !campaigns.length;
}

function renderError(err) {
	clear($("planner-board"));
	$("planner-meta").textContent = `Could not load the spine — ${err.message}`;
}

function renderEmpty(text) {
	clear($("planner-board"));
	$("planner-meta").textContent = "";
	const empty = $("planner-empty");
	empty.textContent = text;
	empty.hidden = false;
}

function onPickCampaign() {
	campaignID = $("planner-campaign").value || null;
	if (campaignID) loadSpine();
}

function onNewCampaign() {
	const name = window.prompt("Campaign name?");
	if (!name || !name.trim()) return;
	api.createCampaign(name.trim(), "dnd5e")
		.then(() => loadCampaigns())
		.catch((err) => window.alert(err.message));
}

/* ---------- the board ---------- */

async function loadSpine() {
	if (!campaignID) return;
	$("planner-empty").hidden = true;
	let body;
	try {
		body = await api.story(campaignID);
	} catch (err) {
		if (err.message && /may do that|not available/.test(err.message)) {
			renderEmpty("The spine is the DM's material — this view needs the DM's seat.");
			return;
		}
		renderError(err);
		return;
	}
	spine = body;
	renderBoard();
	loadPickerData();
}

async function loadPickerData() {
	try {
		const [ents, quests, sessions] = await Promise.all([
			api.campaignEntities(campaignID),
			api.campaignQuests(campaignID),
			api.listSessions(campaignID),
		]);
		picker = {
			entities: ents.entities || [],
			quests: quests.quests || [],
			sessions: sessions.sessions || [],
		};
	} catch (_) {
		picker = { entities: [], quests: [], sessions: [] };
	}
	renderBoard();
}

function renderBoard() {
	const board = clear($("planner-board"));
	const acts = (spine && spine.acts) || [];
	const plans = (spine && spine.plans) || [];
	if (!acts.length) {
		board.append(el("p", { class: "planner-empty-hint", text: "No acts yet — pick a shape below or add the first act." }));
	}
	const wrap = el("div", { class: "planner-cols" });
	for (const act of acts) wrap.append(actColumn(act));
	board.append(wrap);

	const scenes = acts.flatMap((a) => a.scenes || []);
	$("planner-meta").textContent =
		`${acts.length} act${acts.length === 1 ? "" : "s"} · ${scenes.length} scene${scenes.length === 1 ? "" : "s"} · ${plans.length} session plan${plans.length === 1 ? "" : "s"}`;
}

function actColumn(act) {
	const col = el("section", {
		class: "planner-act",
		attrs: { "data-act": act.id },
	},
		el("header", { class: "planner-act-head" },
			el("h3", { class: "planner-act-name", text: act.name }),
			el("span", { class: "planner-act-band", text: `levels ${act.level_start}–${act.level_end}` }),
			el("span", { class: `planner-act-status chip-${act.status}`, text: act.status }),
			el("button", {
				class: "planner-icon-btn", text: "+ scene",
				attrs: { type: "button", title: "Add a scene to this act" },
				on: { click: () => onAddScene(act.id) },
			}),
		),
		act.premise ? el("p", { class: "planner-act-premise", text: act.premise }) : null,
	);
	const list = el("div", { class: "planner-scenes" });
	for (const sc of act.scenes || []) list.append(sceneCard(act, sc));
	if (!(act.scenes || []).length) {
		list.append(el("p", { class: "planner-scene-empty", text: "No scenes yet." }));
	}
	col.append(list);
	return col;
}

function sceneCard(act, sc) {
	const card = el("article", {
		class: "planner-scene",
		attrs: { "data-scene": sc.id, tabindex: "0" },
	},
		el("header", { class: "planner-scene-head" },
			el("span", { class: `planner-scene-kind kind-${sc.kind}`, text: KINDS[sc.kind] || sc.kind }),
			el("span", { class: "planner-scene-name", text: sc.name }),
		),
		sc.purpose ? el("p", { class: "planner-scene-purpose", text: sc.purpose }) : null,
	);

	const chips = el("div", { class: "planner-chips" });
	for (const c of sc.cast || []) {
		chips.append(el("span", {
			class: "planner-chip chip-cast",
			attrs: { title: c.role },
			text: entityLabel(c.entity_id) ? `${entityLabel(c.entity_id)} · ${c.role}` : c.entity_id.slice(0, 8),
		}));
	}
	for (const s of sc.secrets || []) {
		chips.append(el("span", {
			class: "planner-chip chip-secret",
			text: `secret ${DISPOSITIONS[s.disposition] || s.disposition}`,
		}));
	}
	for (const o of sc.outcomes || []) {
		const parts = [o.label];
		if (o.quest_transition) parts.push(`${o.quest_transition.from} → ${o.quest_transition.to}`);
		chips.append(el("span", { class: "planner-chip chip-outcome", text: parts.join(" · ") }));
	}
	if ((sc.cast || []).length || (sc.secrets || []).length || (sc.outcomes || []).length) {
		card.append(chips);
	}
	return card;
}

function entityLabel(id) {
	const e = picker && picker.entities.find((x) => x.id === id);
	return e ? e.name : "";
}

/* ---------- board interactions: one click-per-action, prompt-driven ---------- */

async function onSpineClick(e) {
	const actBtn = e.target.closest("[data-act] .planner-icon-btn");
	if (actBtn) return; // handled by its own listener
	const card = e.target.closest("[data-scene]");
	if (card) onOpenScene(card.dataset.scene);
}

async function onAddScene(actID) {
	if (!campaignID) return;
	const name = window.prompt("Scene name?");
	if (!name || !name.trim()) return;
	const kind = window.prompt(`Kind (${Object.keys(KINDS).join(", ")})?`, "social");
	if (!kind || !KINDS[kind]) return;
	const purpose = window.prompt("Purpose (optional)?") || "";
	try {
		await api.sceneCreate(campaignID, { act_id: actID, kind, name: name.trim(), purpose });
		await loadSpine();
	} catch (err) {
		window.alert(err.message);
	}
}

async function onOpenScene(sceneID) {
	if (!campaignID) return;
	let body;
	try {
		body = await api.sceneGet(campaignID, sceneID);
	} catch (err) {
		window.alert(err.message);
		return;
	}
	const sc = body.scene;
	const doing = window.prompt(
		`${sc.name} (${sc.kind})\n\nCast: ${(sc.cast || []).map((c) => `${entityLabel(c.entity_id)} (${c.role})`).join(", ") || "none"}\n` +
		`Secrets: ${(sc.secrets || []).map((s) => `${s.fact_id.slice(0, 8)} ${DISPOSITIONS[s.disposition]}`).join(", ") || "none"}\n` +
		`Outcomes: ${(sc.outcomes || []).map((o) => o.label).join(", ") || "none"}\n\n` +
		`add cast · remove cast · add secret · add outcome · status · delete — which?`);
	if (!doing) return;
	try {
		switch (doing.trim().toLowerCase()) {
			case "add cast": {
				if (!picker) await loadPickerData();
				const who = window.prompt(`Which entity? (${picker.entities.map((x) => x.name).join(", ")})`);
				const ent = picker.entities.find((x) => x.name.toLowerCase() === (who || "").trim().toLowerCase());
				if (!ent) return window.alert("No such entity.");
				const role = window.prompt(`Role (${Object.keys(ROLES).join(", ")})?`, "present");
				if (!ROLES[role]) return window.alert("No such role.");
				await api.sceneCastAdd(campaignID, sceneID, { entity_id: ent.id, role });
				break;
			}
			case "remove cast": {
				if (!picker) await loadPickerData();
				const who = window.prompt("Which entity's row goes?");
				const ent = picker.entities.find((x) => x.name.toLowerCase() === (who || "").trim().toLowerCase());
				if (!ent) return window.alert("No such entity.");
				await api.sceneCastRemove(campaignID, sceneID, ent.id);
				break;
			}
			case "add secret": {
				const factID = window.prompt("Fact id of the secret (copy from the campaign's facts)?");
				if (!factID || !factID.trim()) return;
				const disposition = window.prompt(`Disposition (${Object.keys(DISPOSITIONS).join(", ")})?`, "in_play");
				if (!DISPOSITIONS[disposition]) return window.alert("No such disposition.");
				await api.sceneSecretAdd(campaignID, sceneID, { fact_id: factID.trim(), disposition });
				break;
			}
			case "add outcome": {
				if (!picker) await loadPickerData();
				const label = window.prompt("Label (A–D by convention)?");
				if (!label || !label.trim()) return;
				const summary = window.prompt("Summary (optional)?") || "";
				let transition = null;
				if (picker.quests.length && window.prompt("Name a quest transition? (y/N)", "n") === "y") {
					const q = picker.quests[0];
					const from = window.prompt(`From state? (${q.state_machine.states.join(", ")})`);
					const to = window.prompt(`To state?`);
					transition = { quest: q.id, from, to };
				}
				await api.sceneOutcomeAdd(campaignID, sceneID, {
					label: label.trim(), summary, quest_transition: transition,
				});
				break;
			}
			case "status": {
				const status = window.prompt("New status (planned, active, done)?");
				if (!["planned", "active", "done"].includes(status)) return window.alert("No such status.");
				await api.sceneUpdate(campaignID, sceneID, { status });
				break;
			}
			case "delete": {
				await api.sceneDelete(campaignID, sceneID);
				break;
			}
			default:
				return;
		}
		await loadSpine();
	} catch (err) {
		window.alert(err.message);
	}
}

/* ---------- the shape helpers: deterministic, no model ---------- */

let shapesCache = null;

async function ensureShapes() {
	if (shapesCache) return shapesCache;
	try {
		const body = await api.storyShapes();
		shapesCache = body.shapes || [];
	} catch (_) {
		shapesCache = [];
	}
	const sel = clear($("planner-shape"));
	sel.append(el("option", { text: "Shape…", attrs: { value: "" } }));
	for (const sh of shapesCache) {
		sel.append(el("option", { text: `${sh.label} — ${sh.acts.map((a) => a.label).join(" / ")}`, attrs: { value: sh.key } }));
	}
	return shapesCache;
}

async function onPickShape() {
	await ensureShapes();
	const key = $("planner-shape").value;
	if (!key || !campaignID) return;
	const shape = shapesCache.find((s) => s.key === key);
	if (!shape) return;
	if (!window.confirm(`Lay out "${shape.label}" as new acts (levels 1–12)?`)) return;

	// The pace endpoint prices the band across the shape's act count.
	const from = Number(window.prompt("From level?", "1"));
	const to = Number(window.prompt("To level?", "12"));
	let perAct = null;
	try {
		const body = await api.storyPace(from, to, shape.acts.length);
		perAct = body.pace.per_act || [];
	} catch (_) { /* a failed pace read falls back to un-priced acts */ }

	try {
		for (let i = 0; i < shape.acts.length; i++) {
			const band = perAct[i] || { level_start: from, level_end: to, sessions: 0 };
			await api.actCreate(campaignID, {
				name: shape.acts[i].label,
				premise: shape.acts[i].purpose,
				level_start: band.level_start,
				level_end: band.level_end,
			});
		}
		$("planner-shape").value = "";
		await loadSpine();
	} catch (err) {
		window.alert(err.message);
	}
}

async function onQuickAct() {
	if (!campaignID) return;
	const name = window.prompt("Act name?");
	if (!name || !name.trim()) return;
	const lo = Number(window.prompt("Level start?", "1"));
	const hi = Number(window.prompt("Level end?", "4"));
	try {
		await api.actCreate(campaignID, { name: name.trim(), level_start: lo, level_end: hi });
		await loadSpine();
	} catch (err) {
		window.alert(err.message);
	}
}
