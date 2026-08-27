// The campaign world (MAD-305): picker, entity browser, fact editor, members
// table and the graph view. A self-contained surface layered over the
// transcript like Study and Encounter. The server resolves the caller's
// scope from their membership row on every request — this module renders
// whatever comes back and never decides visibility itself. Editor surfaces
// are hidden for players, and the server refuses them anyway.

import { $, el, esc, clear, isNarrow } from "./dom.js";
import { api } from "./api.js";
import { openReviewFor } from "./review.js";

const KINDS = ["pc", "npc", "faction", "location", "item", "deity", "organization", "creature", "concept"];

let campaigns = [];
let current = null; // { id, my_role }
let entities = [];
let selected = null; // the open entity detail
let kindFilter = "";
let hops = 1;
let supersedeTarget = null; // fact id the fact form will replace, if any
let loadSeq = 0; // guards stale entity-list fetches

export function initCampaign() {
	$("rail-campaign").addEventListener("click", openCampaign);
	$("campaign-close").addEventListener("click", closeCampaign);
	// The entity creator's kind select is static; fill it once.
	const kindSelect = $("camp-entity-kind");
	for (const k of KINDS) kindSelect.append(el("option", { text: k, attrs: { value: k } }));
	syncObjectKind();
	$("camp-select").addEventListener("change", (e) => selectCampaign(e.target.value));
	$("camp-new-toggle").addEventListener("click", toggleNewCampaign);
	$("camp-new-form").addEventListener("submit", onCreateCampaign);
	$("camp-skeleton-form").addEventListener("submit", onSkeletonDesign);
	$("camp-join-form").addEventListener("submit", onJoinCampaign);
	$("camp-cmd-form").addEventListener("submit", onCommand);
	$("camp-cmd-result").addEventListener("click", onCommandResultClick);
	$("camp-search-form").addEventListener("submit", (e) => {
		e.preventDefault();
		runSearch($("camp-search").value.trim());
	});
	$("camp-kinds").addEventListener("click", (e) => {
		const chip = e.target.closest("[data-kind]");
		if (!chip) return;
		kindFilter = chip.dataset.kind;
		for (const c of $("camp-kinds").children) c.classList.toggle("is-on", c === chip);
		loadEntities();
	});
	$("camp-entities").addEventListener("click", (e) => {
		const row = e.target.closest("[data-eid]");
		if (!row) return;
		selectEntity(row.dataset.eid);
	});
	$("camp-new-entity").addEventListener("submit", onCreateEntity);
	$("camp-fact-form").addEventListener("submit", onSubmitFact);
	for (const radio of document.querySelectorAll('input[name="camp-object"]')) {
		radio.addEventListener("change", () => syncObjectKind());
	}
	$("camp-hops").addEventListener("click", (e) => {
		const btn = e.target.closest("[data-hops]");
		if (!btn) return;
		hops = Number(btn.dataset.hops);
		for (const b of $("camp-hops").children) {
			b.classList.toggle("is-active", b === btn);
			b.setAttribute("aria-checked", b === btn ? "true" : "false");
		}
		renderGraph();
	});
	// The sheet is fully delegated: why?, awareness quick-sets, retcon and
	// node clicks survive re-renders without re-binding.
	$("camp-sheet-body").addEventListener("click", onSheetClick);
	$("camp-invite-form").addEventListener("submit", onMintInvite);
	$("camp-members-body").addEventListener("change", onMemberRoleChange);
	$("camp-members-body").addEventListener("click", onMemberRevoke);
	$("camp-invites").addEventListener("click", (e) => {
		const copy = e.target.closest("[data-copy]");
		if (copy) navigator.clipboard?.writeText(copy.dataset.copy);
	});
}

export async function openCampaign() {
	if (isNarrow()) $("app").classList.add("rail-hidden");
	$("campaign-view").hidden = false;
	$("main").classList.add("is-campaiging");
	$("camp-new-toggle").setAttribute("aria-expanded", "false");
	await loadCampaigns();
}

export function closeCampaign() {
	$("campaign-view").hidden = true;
	$("main").classList.remove("is-campaiging");
	current = null;
	selected = null;
	$("rail-campaign").focus();
}

const isDM = () => current && (current.my_role === "dm" || current.my_role === "keeper");

/* ---------- picker ---------- */

async function loadCampaigns() {
	let data;
	try {
		data = await api.campaignList();
	} catch (err) {
		renderMeta(err.message, true);
		return;
	}
	campaigns = data.campaigns || [];
	renderPicker();
	if (campaigns.length === 0) {
		$("camp-body").hidden = true;
		$("camp-empty").hidden = false;
		return;
	}
	$("camp-body").hidden = false;
	$("camp-empty").hidden = true;
	const prev = current && campaigns.some((c) => c.id === current.id) ? current.id : null;
	selectCampaign(prev || campaigns[0].id);
}

function renderPicker() {
	const select = clear($("camp-select"));
	if (campaigns.length === 0) {
		select.append(el("option", { text: "No campaigns yet…", attrs: { value: "" } }));
		return;
	}
	for (const c of campaigns) {
		select.append(el("option", { text: c.name, attrs: { value: c.id } }));
	}
}

async function selectCampaign(id) {
	if (!id) return;
	current = campaigns.find((c) => c.id === id) || null;
	if (!current) return;
	$("camp-select").value = id;
	supersedeTarget = null;
	renderMeta();
	$("camp-new-entity").hidden = !isDM();
	$("camp-fact-form").hidden = !isDM();
	$("camp-members-panel").hidden = !isDM();
	$("camp-cmd-form").hidden = !isDM();
	if (!isDM()) $("camp-cmd-result").hidden = true;
	kindFilter = "";
	renderKindChips();
	await loadEntities();
	if (isDM()) {
		loadMembers();
		loadInvites();
	} else {
		clear($("camp-members-body"));
		clear($("camp-invites"));
	}
	selectEntity(null);
}

function renderMeta(message, warn) {
	const meta = $("campaign-meta");
	meta.textContent = message || (current ? roleLabel(current.my_role) : "");
	meta.classList.toggle("warn", !!warn);
}

function roleLabel(role) {
	if (role === "dm" || role === "keeper") return "You run this table.";
	if (role === "observer") return "You are watching.";
	if (role === "player") return "You sit at this table.";
	return "";
}

function toggleNewCampaign() {
	// The New button reveals the empty state's create form even when other
	// campaigns exist; creating from there returns to the picker.
	$("camp-empty").hidden = !$("camp-empty").hidden;
	$("camp-new-toggle").setAttribute("aria-expanded", String(!$("camp-empty").hidden));
	if (!$("camp-empty").hidden) $("camp-new-name").focus();
}

async function onCreateCampaign(e) {
	e.preventDefault();
	try {
		const data = await api.campaignCreate($("camp-new-name").value.trim(), $("camp-new-system").value.trim(), $("camp-new-premise").value);
		$("camp-new-name").value = "";
		$("camp-new-system").value = "";
		$("camp-new-premise").value = "";
		$("camp-new-toggle").setAttribute("aria-expanded", "false");
		await loadCampaigns();
		if (data.campaign) selectCampaign(data.campaign.id);
	} catch (err) {
		renderMeta(err.message, true);
	}
}

async function onJoinCampaign(e) {
	e.preventDefault();
	try {
		await api.campaignJoin($("camp-join-code").value.trim());
		$("camp-join-code").value = "";
		$("camp-new-toggle").setAttribute("aria-expanded", "false");
		await loadCampaigns();
	} catch (err) {
		renderMeta(err.message, true);
	}
}

/* ---------- start from a premise ---------- */

// onSkeletonDesign is the empty state's front door (MAD-361): found the
// campaign, then hand the premise to the skeleton generator. The acts and
// session plans land in the spine right away; everything canon — factions,
// NPCs, the central secret, the hooks, the web — waits in the review queue,
// and that is where this hands off to.
async function onSkeletonDesign(e) {
	e.preventDefault();
	const name = $("camp-skeleton-name").value.trim();
	const premise = $("camp-skeleton-premise").value.trim();
	if (!name || !premise) return;
	const start = parseInt($("camp-skeleton-start").value, 10) || 1;
	const end = parseInt($("camp-skeleton-end").value, 10) || 12;
	const acts = parseInt($("camp-skeleton-acts").value, 10) || 0;
	renderMeta("Computing the skeleton…");
	let campaignID = null;
	try {
		const created = await api.campaignCreate(name, "", premise);
		campaignID = created.campaign?.id;
		if (!campaignID) throw new Error("the campaign did not come back");
		await api.skeleton(campaignID, {
			premise,
			level_start: start,
			level_end: end,
			act_count: acts,
		});
	} catch (err) {
		renderMeta(err.message, true);
		if (campaignID) await loadCampaigns();
		return;
	}
	$("camp-skeleton-name").value = "";
	$("camp-skeleton-premise").value = "";
	$("camp-new-toggle").setAttribute("aria-expanded", "false");
	closeCampaign();
	await openReviewFor(campaignID);
}

/* ---------- the command line (MAD-363) ---------- */

// onCommand runs one plain-English command through the campaign's command
// engine. Graph mutations come back staged behind the review gate; a
// clarifying question comes back with its candidates; a refusal says so
// plainly. This surface never decides which is which — the server does.
async function onCommand(e) {
	e.preventDefault();
	if (!current) return;
	const input = $("camp-cmd-input");
	const text = input.value.trim();
	if (!text) return;
	const box = $("camp-cmd-result");
	box.hidden = false;
	clear(box).append(el("p", { class: "camp-status", text: "Running the command…" }));
	input.value = "";
	let data;
	try {
		data = await api.campaignCommand(current.id, text);
	} catch (err) {
		clear(box).append(el("p", { class: "camp-status warn", text: err.message }));
		return;
	}
	renderCommandResult(box, data.command);
	// A merge or a cast write changed the world the browser is showing.
	if (data.command && data.command.kind === "written") loadEntities();
}

// renderCommandResult draws one command outcome: the message, the staged
// batch's items with a door into the review queue, or the question's
// candidates as one-click names.
function renderCommandResult(box, cmd) {
	if (!cmd) return;
	box.hidden = false;
	const inner = clear(box);
	inner.append(el("p", { class: "camp-cmd-message", text: cmd.message || "(no response)" }));
	if (cmd.kind === "batch" && cmd.batch) {
		const items = el("div", { class: "camp-cmd-items" });
		for (const it of cmd.batch.items || []) {
			items.append(el("span", { class: "enc-chip", text: it.subject || it.kind }));
		}
		inner.append(items);
		inner.append(el("button", {
			class: "enc-btn",
			text: "Open the review queue",
			attrs: { type: "button", "data-open-review": cmd.batch.id },
		}));
	}
	if (cmd.kind === "question" && cmd.question && (cmd.question.candidates || []).length) {
		const cands = el("div", { class: "camp-cmd-items" });
		for (const c of cmd.question.candidates) {
			const label = c.kind ? `${c.name} (${c.kind})` : c.name;
			const chip = el("button", { class: "enc-chip", text: label, attrs: { type: "button", "data-name": c.name } });
			if (c.note) chip.title = c.note;
			cands.append(chip);
		}
		inner.append(cands);
	}
}

// onCommandResultClick handles the affordances a result carries: the door
// into the review queue, and the candidate chips that drop a name into the
// command line for the DM's next command.
async function onCommandResultClick(e) {
	const open = e.target.closest("[data-open-review]");
	if (open) {
		openReviewFor(current.id);
		return;
	}
	const name = e.target.closest("[data-name]");
	if (name) {
		const input = $("camp-cmd-input");
		input.value = (input.value ? input.value.replace(/\s+$/, "") + " " : "") + name.dataset.name;
		input.focus();
	}
}

/* ---------- entity browser ---------- */

function renderKindChips() {
	const wrap = clear($("camp-kinds"));
	const all = el("button", { class: "enc-chip is-on", text: "all", attrs: { type: "button", "data-kind": "" } });
	wrap.append(all);
	for (const k of KINDS) {
		wrap.append(el("button", { class: "enc-chip", text: k, attrs: { type: "button", "data-kind": k } }));
	}
}

async function loadEntities() {
	if (!current) return;
	const seq = ++loadSeq;
	let data;
	try {
		data = await api.campaignEntities(current.id, kindFilter);
	} catch (err) {
		if (seq === loadSeq) renderEntityList(el("p", { class: "study-status warn", text: err.message }));
		return;
	}
	if (seq !== loadSeq) return;
	entities = data.entities || [];
	const list = clear($("camp-entities"));
	if (entities.length === 0) {
		list.append(el("p", { class: "camp-status", text: "Nothing here yet." }));
		return;
	}
	for (const ent of entities) {
		const row = el("button", {
			class: "camp-entity" + (selected && selected.id === ent.id ? " is-active" : ""),
			attrs: { type: "button", "data-eid": ent.id, role: "option", "aria-selected": String(!!selected && selected.id === ent.id) },
		});
		row.append(
			el("span", { class: "camp-entity-kind", text: ent.kind }),
			el("span", { class: "camp-entity-name", text: ent.name }),
		);
		list.append(row);
	}
}

async function onCreateEntity(e) {
	e.preventDefault();
	try {
		await api.campaignEntityCreate(current.id,
			$("camp-entity-kind").value, $("camp-entity-name").value.trim(), $("camp-entity-summary").value.trim());
		$("camp-entity-name").value = "";
		$("camp-entity-summary").value = "";
		await loadEntities();
	} catch (err) {
		renderMeta(err.message, true);
	}
}

async function runSearch(q) {
	const hits = clear($("camp-hits"));
	if (!q) {
		$("camp-hits").hidden = true;
		return;
	}
	let data;
	try {
		data = await api.campaignSearch(current.id, q);
	} catch (err) {
		hits.append(el("p", { class: "camp-status warn", text: err.message }));
		$("camp-hits").hidden = false;
		return;
	}
	$("camp-hits").hidden = false;
	const results = data.hits || [];
	if (results.length === 0) {
		hits.append(el("p", { class: "camp-status", text: "Nothing in the campaign answers to that." }));
		return;
	}
	for (const hit of results) {
		const label = hit.kind === "entity" ? (entities.find((e) => e.id === hit.ref_id)?.name || hit.ref_id) : hit.kind;
		hits.append(el("p", { class: "camp-hit", text: `${label} — ${hit.snippet}` }));
	}
}

/* ---------- the sheet ---------- */

async function selectEntity(id) {
	const body = clear($("camp-sheet-body"));
	$("camp-sheet-status").hidden = false;
	if (!id) {
		$("camp-sheet-status").textContent = "Choose an entity to open its sheet.";
		selected = null;
		renderGraph();
		return;
	}
	$("camp-sheet-status").textContent = "Opening the sheet…";
	let data;
	try {
		data = await api.campaignEntity(current.id, id);
	} catch (err) {
		$("camp-sheet-status").textContent = err.message;
		return;
	}
	$("camp-sheet-status").hidden = true;
	selected = data.entity;
	renderSheet();
	fillFactFormSubjects();
	await renderGraph();
	if (isDM()) $("camp-fact-subject").value = selected.id;
	for (const row of $("camp-entities").children) {
		const active = row.dataset.eid === selected.id;
		row.classList.toggle("is-active", active);
		row.setAttribute("aria-selected", String(active));
	}
}

function renderSheet() {
	const body = clear($("camp-sheet-body"));
	if (!selected) return;
	const head = el("header", { class: "camp-sheet-head" });
	head.append(
		el("h3", { class: "camp-sheet-title", text: selected.name }),
		el("span", { class: "enc-chip is-on", text: selected.kind }),
	);
	if (selected.status && selected.status !== "active") {
		head.append(el("span", { class: "enc-chip", text: selected.status }));
	}
	body.append(head);
	if (selected.summary) body.append(el("p", { class: "camp-sheet-summary prose", text: selected.summary }));

	// Also-known-as: the aliases table, small.
	if ((selected.names || []).length > 1) {
		const names = selected.names.filter((n) => n.kind !== "canonical").map((n) => n.name).join(" · ");
		if (names) body.append(el("p", { class: "camp-aliases", text: `Also called ${names}` }));
	}

	body.append(section("Facts", factsList(selected.facts || [])));
	body.append(section("Ties", relList(selected.relationships || [])));
	body.append(section("Appearances", eventList(selected.events || [])));

	if (isDM()) {
		const form = $("camp-fact-form");
		form.hidden = false;
		form.scrollIntoView({ block: "nearest" });
	}
}

function section(heading, list) {
	const wrap = el("section", { class: "camp-sheet-section" });
	wrap.append(el("h4", { class: "camp-sheet-heading", text: heading }));
	if (list.children.length === 0) {
		wrap.append(el("p", { class: "camp-status", text: "None recorded." }));
	} else {
		wrap.append(list);
	}
	return wrap;
}

function factsList(facts) {
	const list = el("div", { class: "camp-facts" });
	for (const f of facts) {
		const row = el("div", { class: "camp-fact", attrs: { "data-fid": f.id } });
		row.append(el("p", { class: "camp-fact-statement prose", text: f.statement }));
		const chips = el("div", { class: "camp-fact-chips" });
		chips.append(el("span", { class: "enc-chip" + (f.confidence === "canon" ? " is-on" : ""), text: f.confidence }));
		if (f.visibility === "secret") chips.append(el("span", { class: "enc-chip", text: "secret" }));
		if (isDM()) {
			chips.append(el("button", { class: "enc-chip", text: "party knows", attrs: { type: "button", "data-aware": "knows" } }));
			chips.append(el("button", { class: "enc-chip", text: "party suspects", attrs: { type: "button", "data-aware": "suspects" } }));
			chips.append(el("button", { class: "enc-chip", text: "why?", attrs: { type: "button", "data-why": f.id } }));
			chips.append(el("button", { class: "enc-chip", text: "retcon", attrs: { type: "button", "data-retcon": f.id, "data-statement": f.statement } }));
		}
		row.append(chips);
		const why = el("div", { class: "camp-why", attrs: { "data-why-box": f.id } });
		why.hidden = true;
		row.append(why);
		list.append(row);
	}
	return list;
}

function relList(rels) {
	const list = el("ul", { class: "camp-rels" });
	const nameOf = (id) => entities.find((e) => e.id === id)?.name || (selected && selected.id === id ? selected.name : id.slice(0, 8));
	for (const r of rels) {
		list.append(el("li", { class: "camp-rel", html: `<b>${esc(nameOf(r.from))}</b> ${esc(r.rel_type)} <b>${esc(nameOf(r.to))}</b>` }));
	}
	return list;
}

function eventList(events) {
	const list = el("ul", { class: "camp-events" });
	for (const ev of events) {
		const day = ev.clock_at != null ? `day ${ev.clock_at}` : "";
		list.append(el("li", { class: "camp-event", text: `${day ? day + " — " : ""}${ev.summary}` }));
	}
	return list;
}

async function onSheetClick(e) {
	const aware = e.target.closest("[data-aware]");
	if (aware) {
		const fid = aware.closest("[data-fid]").dataset.fid;
		try {
			await api.campaignAwarenessSet(current.id, "party", fid, aware.dataset.aware);
			renderMeta("Recorded what the party holds.");
		} catch (err) {
			renderMeta(err.message, true);
		}
		return;
	}
	const why = e.target.closest("[data-why]");
	if (why) {
		await toggleWhy(why.dataset.why);
		return;
	}
	const retcon = e.target.closest("[data-retcon]");
	if (retcon) {
		beginSupersede(retcon.dataset.retcon, retcon.dataset.statement);
		return;
	}
}

async function toggleWhy(fid) {
	const box = $("camp-sheet-body").querySelector(`[data-why-box="${fid}"]`);
	if (!box) return;
	if (!box.hidden) {
		box.hidden = true;
		return;
	}
	box.hidden = false;
	clear(box).append(el("p", { class: "camp-status", text: "Checking the record…" }));
	let data;
	try {
		data = await api.campaignFact(current.id, fid);
	} catch (err) {
		clear(box).append(el("p", { class: "camp-status warn", text: err.message }));
		return;
	}
	const fact = data.fact;
	const inner = clear(box);
	const prov = fact.provenance || [];
	if (prov.length === 0) {
		inner.append(el("p", { class: "camp-status", text: "No provenance rows." }));
	} else {
		for (const p of prov) {
			inner.append(el("p", { class: "camp-why-line", text: `${p.method}${p.quote ? `: “${p.quote}”` : ""}` }));
		}
	}
	const aware = fact.awareness || [];
	for (const a of aware) {
		inner.append(el("p", { class: "camp-why-line", text: `${a.knower} ${a.stance} this (${Math.round(a.confidence * 100)}%)` }));
	}
}

function beginSupersede(fid, statement) {
	supersedeTarget = fid;
	$("camp-fact-form").hidden = false;
	const banner = $("camp-fact-form").querySelector(".camp-supersede-banner");
	if (banner) banner.remove();
	$("camp-fact-form").prepend(el("p", {
		class: "camp-supersede-banner",
		text: `Replacing: “${statement}” — write the new truth below.`,
	}));
	$("camp-fact-statement").focus();
	$("camp-fact-form").scrollIntoView({ behavior: "smooth", block: "nearest" });
}

function fillFactFormSubjects() {
	const subject = clear($("camp-fact-subject"));
	const object = clear($("camp-fact-entity"));
	const pool = entities.length > 0 ? entities : (selected ? [selected] : []);
	for (const ent of pool) {
		subject.append(el("option", { text: `${ent.name} (${ent.kind})`, attrs: { value: ent.id } }));
		object.append(el("option", { text: ent.name, attrs: { value: ent.id } }));
	}
	if (selected) subject.value = selected.id;
}

function syncObjectKind() {
	const literal = document.querySelector('input[name="camp-object"]:checked').value === "literal";
	$("camp-fact-literal-row").hidden = !literal;
	$("camp-fact-entity-row").hidden = literal;
}

async function onSubmitFact(e) {
	e.preventDefault();
	const literal = document.querySelector('input[name="camp-object"]:checked').value === "literal";
	const fact = {
		subject: $("camp-fact-subject").value,
		predicate: $("camp-fact-predicate").value.trim(),
		statement: $("camp-fact-statement").value.trim(),
		confidence: $("camp-fact-confidence").value,
		visibility: $("camp-fact-visibility").value,
	};
	if (literal) fact.object_literal = $("camp-fact-literal").value.trim();
	else fact.object_entity = $("camp-fact-entity").value;
	try {
		if (supersedeTarget) {
			await api.campaignFactSupersede(current.id, supersedeTarget, fact);
			supersedeTarget = null;
			const banner = $("camp-fact-form").querySelector(".camp-supersede-banner");
			if (banner) banner.remove();
		} else {
			await api.campaignFactCreate(current.id, fact);
		}
		$("camp-fact-predicate").value = "";
		$("camp-fact-statement").value = "";
		$("camp-fact-literal").value = "";
		await selectEntity(selected.id);
	} catch (err) {
		renderMeta(err.message, true);
	}
}

/* ---------- the map (graph view v0) ---------- */

// The neighbourhood of the selected entity, drawn as a map on parchment:
// the center in gold, one ring per hop, ink lines between. No physics, no
// animation, no library — a deterministic chart that reads at a glance and
// respects the design system (whole-pixel geometry, pixel font for labels).
async function renderGraph() {
	const wrap = clear($("camp-graph"));
	if (!current || !selected) {
		wrap.append(el("p", { class: "camp-status", text: "Open an entity to chart its neighbourhood." }));
		return;
	}
	let data;
	try {
		data = await api.campaignGraph(current.id, selected.id, hops);
	} catch (err) {
		wrap.append(el("p", { class: "camp-status warn", text: err.message }));
		return;
	}
	const nodes = data.nodes || [];
	const edges = data.edges || [];
	const pos = layout(nodes);
	const W = 560, H = 440, cx = W / 2, cy = H / 2;
	const svgNS = "http://www.w3.org/2000/svg";
	const svg = document.createElementNS(svgNS, "svg");
	svg.setAttribute("viewBox", `0 0 ${W} ${H}`);
	svg.setAttribute("class", "camp-graph-svg");
	svg.setAttribute("role", "img");
	svg.setAttribute("aria-label", `Neighbourhood of ${selected.name}, ${hops} hop${hops > 1 ? "s" : ""}`);

	const byId = new Map(nodes.map((n) => [n.id, n]));
	for (const edge of edges) {
		const a = pos.get(edge.from), b = pos.get(edge.to);
		if (!a || !b) continue;
		const line = document.createElementNS(svgNS, "line");
		line.setAttribute("x1", cx + a.x); line.setAttribute("y1", cy + a.y);
		line.setAttribute("x2", cx + b.x); line.setAttribute("y2", cy + b.y);
		line.setAttribute("class", "camp-edge");
		svg.append(line);
		const mid = document.createElementNS(svgNS, "text");
		mid.setAttribute("x", cx + (a.x + b.x) / 2);
		mid.setAttribute("y", cy + (a.y + b.y) / 2 - 3);
		mid.setAttribute("class", "camp-edge-label");
		mid.textContent = edge.rel_type;
		svg.append(mid);
	}
	const half = (n) => n.hops === 0 ? 7 : 5;
	for (const n of nodes) {
		const p = pos.get(n.id);
		const g = document.createElementNS(svgNS, "g");
		g.setAttribute("class", "camp-node" + (n.hops === 0 ? " is-center" : ""));
		g.setAttribute("transform", `translate(${cx + p.x},${cy + p.y})`);
		g.setAttribute("tabindex", "0");
		g.setAttribute("role", "button");
		g.setAttribute("aria-label", `${n.name} (${n.kind})`);
		g.dataset.eid = n.id;
		const rect = document.createElementNS(svgNS, "rect");
		const s = half(n) * 2;
		rect.setAttribute("x", -half(n)); rect.setAttribute("y", -half(n));
		rect.setAttribute("width", s); rect.setAttribute("height", s);
		const label = document.createElementNS(svgNS, "text");
		label.setAttribute("y", half(n) + 11);
		label.setAttribute("class", "camp-node-label");
		label.textContent = n.name.length > 18 ? n.name.slice(0, 17) + "…" : n.name;
		const title = document.createElementNS(svgNS, "title");
		title.textContent = `${n.name} — ${n.kind}${n.summary ? `: ${n.summary}` : ""}`;
		g.append(rect, label, title);
		if (n.hops > 0) {
			g.addEventListener("click", () => selectEntity(n.id));
			g.addEventListener("keydown", (ev) => {
				if (ev.key === "Enter" || ev.key === " ") {
					ev.preventDefault();
					selectEntity(n.id);
				}
			});
		}
		svg.append(g);
	}
	wrap.append(svg);
	if (nodes.length === 1) {
		wrap.append(el("p", { class: "camp-status", text: "No ties recorded yet — the map grows as the world does." }));
	}
}

// Concentric rings: the center at the origin, hop 1 at radius 120, hop 2 at
// 190, nodes sorted by name so the chart is stable across renders.
function layout(nodes) {
	const rings = [[], [], []];
	for (const n of nodes) rings[Math.min(n.hops, 2)].push(n);
	const radius = [0, 120, 190];
	const pos = new Map();
	for (let r = 0; r < 3; r++) {
		const ring = rings[r].sort((a, b) => a.name.localeCompare(b.name));
		ring.forEach((n, i) => {
			const angle = ring.length === 1 && r === 0 ? -Math.PI / 2 : (i / ring.length) * Math.PI * 2 - Math.PI / 2;
			pos.set(n.id, { x: Math.round(Math.cos(angle) * radius[r]), y: Math.round(Math.sin(angle) * radius[r]) });
		});
	}
	return pos;
}

/* ---------- the table (members + invites, DM only) ---------- */

async function loadMembers() {
	let data;
	try {
		data = await api.campaignMembers(current.id);
	} catch (err) {
		clear($("camp-members-body")).append(el("p", { class: "camp-status warn", text: err.message }));
		return;
	}
	const body = clear($("camp-members-body"));
	for (const m of data.members || []) {
		const row = el("div", { class: "camp-member", attrs: { "data-uid": m.user_id } });
		const role = el("select", { class: "enc-field camp-role", attrs: { "aria-label": `Role for ${m.username}` } });
		for (const r of ["dm", "player", "observer"]) {
			const opt = el("option", { text: r, attrs: { value: r } });
			if (m.role === r) opt.selected = true;
			role.append(opt);
		}
		row.append(
			el("span", { class: "camp-member-name", text: m.username || m.user_id.slice(0, 8) }),
			role,
			el("button", { class: "enc-chip", text: "revoke", attrs: { type: "button", "data-revoke": m.user_id } }),
		);
		body.append(row);
	}
}

async function onMemberRoleChange(e) {
	const row = e.target.closest("[data-uid]");
	if (!row) return;
	try {
		await api.campaignMemberUpdate(current.id, row.dataset.uid, { role: e.target.value });
		renderMeta("Seat updated.");
	} catch (err) {
		renderMeta(err.message, true);
		await loadMembers();
	}
}

async function onMemberRevoke(e) {
	const btn = e.target.closest("[data-revoke]");
	if (!btn) return;
	try {
		await api.campaignMemberRemove(current.id, btn.dataset.revoke);
		await loadMembers();
	} catch (err) {
		renderMeta(err.message, true);
	}
}

async function onMintInvite(ev) {
	ev.preventDefault();
	let data;
	try {
		data = await api.campaignInviteCreate(current.id, $("camp-invite-role").value);
	} catch (err) {
		renderMeta(err.message, true);
		return;
	}
	renderInvite(data.invite);
	await loadInvites();
}

function renderInvite(invite) {
	if (!invite || !invite.url) return;
	const box = clear($("camp-invites"));
	box.prepend(el("p", { class: "camp-invite-link" },
		el("a", { attrs: { href: invite.url }, text: invite.url }),
		el("button", { class: "enc-chip", text: "copy", attrs: { type: "button", "data-copy": invite.url } }),
		el("span", { class: "camp-status", text: " — shown once; copy it now." })));
}

async function loadInvites() {
	let data;
	try {
		data = await api.campaignInvites(current.id);
	} catch (err) {
		return; // non-fatal beside the minted link
	}
	const box = $("camp-invites");
	for (const inv of data.invites || []) {
		box.append(el("p", { class: "camp-invite-row", text: `${inv.campaign_role} — ${inv.status}` }));
	}
}
