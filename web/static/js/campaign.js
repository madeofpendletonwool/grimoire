// The campaign world (MAD-305): picker, entity browser, fact editor, members
// table and the graph view. A self-contained surface layered over the
// transcript like Study and Encounter. The server resolves the caller's
// scope from their membership row on every request — this module renders
// whatever comes back and never decides visibility itself. Editor surfaces
// are hidden for players, and the server refuses them anyway.

import { $, el, esc, clear, isNarrow } from "./dom.js";
import { api } from "./api.js";
import { reviewFor } from "./review.js";

const KINDS = ["pc", "npc", "faction", "location", "item", "deity", "organization", "creature", "concept"];

let campaigns = [];
let current = null; // { id, my_role }
let entities = [];
let selected = null; // the open entity detail
let kindFilter = "";
let hops = 1;
let supersedeTarget = null; // fact id the fact form will replace, if any
let loadSeq = 0; // guards stale entity-list fetches
let clockInfo = null; // the last GET /clock body (date, weather, strip, due)
let scheduleEntries = []; // the schedule at the caller's scope
let editingScheduleID = null; // the entry whose inline editor is open
let quests = []; // the board: DM machines or player journal entries
let selectedQuestID = null; // the quest whose detail chart is open (DM)
let dungeons = []; // the workshop listing (DM)
let rumors = []; // the mill: whatever the server sent at the caller's scope
let journeys = []; // the roads: the DM's journeys with their status
let selectedJourneyID = null; // the journey whose day table is open (DM)
let selectedDungeonID = null; // the dungeon whose map is open (DM)
let dungeonDetail = null; // the open dungeon's full view

function wire() {
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
	// The calendar (MAD-365): clock reads, schedule writes and inline edits,
	// and advancing the days — all delegated so re-renders need no re-binds.
	$("camp-schedule-form").addEventListener("submit", onCreateScheduleEntry);
	$("camp-advance-form").addEventListener("submit", onAdvanceClock);
	$("camp-schedule").addEventListener("click", onScheduleClick);
	$("camp-schedule").addEventListener("submit", onScheduleInlineSubmit);
	// The simulation tick (MAD-367): preview, then optionally stage the
	// outcomes as a proposal — the clock only moves once the DM accepts
	// that proposal on the ordinary review queue.
	$("camp-simulate-form").addEventListener("submit", onSimulate);
	$("camp-simulate-result").addEventListener("click", onSimulateResultClick);
	// The road (MAD-375): plan a journey at a density, resolve its days
	// as they are played, then resolve the whole road through the review
	// gate — the same hand-off the tick makes.
	$("camp-road-form").addEventListener("submit", onPlanJourney);
	$("camp-journeys").addEventListener("click", onJourneyClick);
	// The quest board (MAD-369): the list, the machine chart and the DM's
	// move control, all delegated so re-renders need no re-binds.
	$("camp-quests").addEventListener("click", onQuestBoardClick);
	$("camp-quest-detail").addEventListener("submit", onQuestMoveSubmit);
	$("camp-quest-detail").addEventListener("click", onQuestDetailClick);
	// The quest designer (MAD-371): the hook form on the quest board. The
	// structure is computed server-side; the batch it stages opens in the
	// review view, the same hand-off the skeleton generator makes.
	$("camp-quest-design-form").addEventListener("submit", onQuestDesign);
	$("camp-place-design-form").addEventListener("submit", onPlaceDesign);
	// The dungeon workshop (MAD-373): the create form, the listing and
	// the map with its drag-and-edit surface, all delegated so
	// re-renders need no re-binds.
	$("camp-dungeon-form").addEventListener("submit", onDungeonDesign);
	$("camp-dungeons").addEventListener("click", onDungeonBoardClick);
	$("camp-dungeon-detail").addEventListener("click", onDungeonDetailClick);
	$("camp-dungeon-detail").addEventListener("submit", onDungeonDetailSubmit);
	$("camp-dungeon-detail").addEventListener("pointerdown", onRoomPointerDown);
	// The rumour mill (MAD-374): hand-authored rumours, generated batches
	// (review-gated like every generator), and the board's delegated
	// actions — heard, hold, debunk, confirm, forget.
	$("camp-rumor-form").addEventListener("submit", onRumorSubmit);
	$("camp-rumor-generate-form").addEventListener("submit", onRumorGenerate);
	$("camp-rumors").addEventListener("click", onRumorBoardClick);
	$("camp-rumors").addEventListener("change", onRumorBoardChange);
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
	// node clicks survive re-renders without re-binds. The faction page's
	// plan controls (activate, move, status, create) ride the same
	// delegation.
	$("camp-sheet-body").addEventListener("click", onSheetClick);
	$("camp-sheet-body").addEventListener("submit", onFactionPlanSubmit);
	$("camp-sheet-body").addEventListener("submit", onPlaceSubmit);
	$("camp-sheet-body").addEventListener("submit", onFleshOutSubmit);
	$("camp-sheet-body").addEventListener("change", onFactionPlanStatusChange);
	$("camp-invite-form").addEventListener("submit", onMintInvite);
	$("camp-members-body").addEventListener("change", onMemberRoleChange);
	$("camp-members-body").addEventListener("click", onMemberRevoke);
	$("camp-invites").addEventListener("click", (e) => {
		const copy = e.target.closest("[data-copy]");
		if (copy) navigator.clipboard?.writeText(copy.dataset.copy);
	});
}

async function openCampaign() {
	$("camp-new-toggle").setAttribute("aria-expanded", "false");
	await loadCampaigns();
}

function closeCampaign() {
	current = null;
	selected = null;
	selectedQuestID = null;
	quests = [];
	selectedJourneyID = null;
	journeys = [];
	journeyDays = [];
	selectedDungeonID = null;
	dungeonDetail = null;
	dungeons = [];
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
	$("camp-quest-design-form").hidden = !isDM();
	$("camp-place-design-form").hidden = !isDM();
	$("camp-dungeon-form").hidden = !isDM();
	$("camp-rumor-form").hidden = !isDM();
	$("camp-rumor-generate-form").hidden = !isDM();
	if (!isDM()) $("camp-cmd-result").hidden = true;
	kindFilter = "";
	renderKindChips();
	editingScheduleID = null;
	selectedQuestID = null;
	await loadEntities();
	loadClock();
	loadSchedule();
	loadQuests();
	loadRumors();
	loadJourneys();
	if (isDM()) loadDungeons(); else clearDungeons();
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
	await reviewFor(campaignID);
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
		reviewFor(current.id);
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
	fillPlaceNear();
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

	// The faction page (MAD-366): dossier, live edges and — for the DM —
	// the plans with their progress. Rendered async into its own section.
	if (selected.kind === "faction") {
		const mount = el("div", { class: "camp-faction", attrs: { id: "camp-faction-body" } });
		mount.append(el("p", { class: "camp-status", text: "Opening the dossier…" }));
		body.append(mount);
		loadFactionDossier(selected.id);
	}

	// The location page (MAD-370): the place block as an editable form and
	// everything else as read-only chips into the entity browser — a view
	// of the graph, not a second place to type things.
	if (selected.kind === "location") {
		const mount = el("div", { class: "camp-location", attrs: { id: "camp-location-body" } });
		mount.append(el("p", { class: "camp-status", text: "Opening the dossier…" }));
		body.append(mount);
		loadLocationDossier(selected.id);
	}

	// What are people saying about them? (MAD-374): the NPC sheet's
	// rumour section — the statements in circulation about this one,
	// loaded at the caller's scope and rendered as quotes.
	if (selected.kind === "npc" || selected.kind === "pc" || selected.kind === "faction") {
		const mount = el("div", { class: "camp-sheet-section", attrs: { "data-rumors-for": selected.id } });
		mount.append(el("h4", { class: "camp-sheet-heading", text: "What people are saying" }));
		mount.append(el("p", { class: "camp-status", text: "Listening…" }));
		body.append(mount);
		loadSheetRumors(selected.id);
	}

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
		// The server formatted the day through the campaign calendar; the
		// bare number is the fallback, never the first choice.
		const when = ev.date || (ev.clock_at != null ? `day ${ev.clock_at}` : "");
		list.append(el("li", { class: "camp-event", text: `${when ? when + " — " : ""}${ev.summary}` }));
	}
	return list;
}

/* ---------- the calendar (MAD-365) ---------- */

// The clock face: today's date, the weather, and a strip of coming days with
// the schedule's due marks. Every date string arrives formatted by the
// server through the campaign's own calendar — this module does zero
// calendar math, so a DM's thirteen-month wheel renders as happily as the
// default Common Reckoning.
async function loadClock() {
	if (!current) return;
	let data;
	try {
		data = await api.campaignClock(current.id, 14, 30);
	} catch (err) {
		clockInfo = null;
		clear($("camp-clock")).append(el("p", { class: "camp-status warn", text: err.message }));
		clear($("camp-clock-strip"));
		return;
	}
	clockInfo = data.clock;
	renderClock();
}

async function loadSchedule() {
	if (!current) return;
	let data;
	try {
		data = await api.campaignSchedule(current.id, 30);
	} catch (err) {
		scheduleEntries = [];
		renderSchedule();
		return;
	}
	scheduleEntries = data.entries || [];
	renderSchedule();
}

function weatherLine(clock) {
	const w = clock.weather;
	if (!w) return clock.season || "";
	const season = clock.season ? ` ${clock.season}` : "";
	return `${w.summary}, ${w.temp_c}°, ${w.wind}${season}${clock.climate ? ` (${clock.climate})` : ""}`;
}

function renderClock() {
	const box = clear($("camp-clock"));
	if (!clockInfo) return;
	box.append(
		el("p", { class: "camp-clock-date", text: clockInfo.date }),
		el("p", { class: "camp-clock-weather", text: weatherLine(clockInfo) }),
	);
	const strip = clear($("camp-clock-strip"));
	const due = clockInfo.due || [];
	for (const d of clockInfo.strip || []) {
		const marks = due.filter((x) => x.day === d.day);
		const cell = el("div", {
			class: "camp-strip-day" + (d.today ? " is-today" : "") + (d.month_start ? " is-month-start" : ""),
			attrs: { role: "listitem", title: d.label + (marks.length ? ` — ${marks.map((m) => m.name).join(", ")}` : "") },
		});
		cell.append(
			el("span", { class: "camp-strip-weekday", text: d.weekday }),
			el("span", { class: "camp-strip-daynum", text: String(d.day_of_month) }),
		);
		if (d.month_start) cell.append(el("span", { class: "camp-strip-month", text: d.month }));
		if (marks.length) cell.append(el("span", { class: "camp-strip-marks", text: "•".repeat(marks.length) }));
		strip.append(cell);
	}
	$("camp-advance-form").hidden = !isDM();
	$("camp-simulate-form").hidden = !isDM();
}

// recurrences renders a recurrence value ("every_n_days:7") as a chip label.
function recurrenceLabel(rec) {
	if (!rec || rec === "none") return "";
	if (rec === "yearly") return "yearly";
	if (rec === "monthly") return "monthly";
	if (rec.startsWith("every_n_days:")) return `every ${rec.split(":")[1]} days`;
	return rec;
}

function renderSchedule() {
	const box = clear($("camp-schedule"));
	box.append(el("h4", { class: "camp-sheet-heading", text: "The schedule" }));
	if ((scheduleEntries || []).length === 0) {
		box.append(el("p", { class: "camp-status", text: "Nothing on the calendar yet." }));
	} else {
		for (const e of scheduleEntries) box.append(scheduleRow(e));
	}
	const form = $("camp-schedule-form");
	form.hidden = !isDM();
	if (isDM()) fillScheduleEntityOptions();
}

function scheduleRow(e) {
	if (editingScheduleID === e.id) return scheduleEditor(e);
	const row = el("div", { class: "camp-sched-entry", attrs: { "data-sid": e.id } });
	const chips = el("span", { class: "camp-fact-chips" });
	const rec = recurrenceLabel(e.recurrence);
	if (rec) chips.append(el("span", { class: "enc-chip", text: rec }));
	if (e.visibility === "secret") chips.append(el("span", { class: "enc-chip", text: "secret" }));
	if (e.status && e.status !== "pending") chips.append(el("span", { class: "enc-chip is-on", text: e.status }));
	row.append(
		el("span", { class: "camp-sched-when", text: e.date || `day ${e.day}` }),
		el("span", { class: "camp-sched-name", text: e.name }),
		chips,
	);
	if (isDM()) {
		row.append(
			el("button", { class: "enc-chip", text: "edit", attrs: { type: "button", "data-edit-sid": e.id } }),
			el("button", { class: "enc-chip", text: "remove", attrs: { type: "button", "data-del-sid": e.id } }),
		);
	}
	return row;
}

// scheduleEditor is the inline patch form: name, day, recurrence, status.
function scheduleEditor(e) {
	const form = el("form", { class: "camp-sched-edit", attrs: { "data-sid": e.id } });
	const rec = e.recurrence || "none";
	const recSelect = el("select", { class: "enc-field camp-sched-edit-rec", attrs: { "aria-label": "Recurrence" } });
	for (const r of ["none", "yearly", "monthly", "every_n_days:7", "every_n_days:10", "every_n_days:30"]) {
		const opt = el("option", { text: r === "none" ? "never" : recurrenceLabel(r) });
		opt.value = r;
		if (r === rec) opt.selected = true;
		recSelect.append(opt);
	}
	const statusSelect = el("select", { class: "enc-field camp-sched-edit-status", attrs: { "aria-label": "Status" } });
	for (const s of ["pending", "fired", "cancelled", "missed"]) {
		const opt = el("option", { text: s });
		opt.value = s;
		if (s === e.status) opt.selected = true;
		statusSelect.append(opt);
	}
	form.append(
		el("input", { class: "enc-field camp-sched-edit-name", attrs: { type: "text", value: e.name, "aria-label": "Name" } }),
		el("input", { class: "enc-field camp-sched-edit-day", attrs: { type: "number", value: String(e.day), "aria-label": "Day" } }),
		recSelect,
		statusSelect,
		el("button", { class: "enc-chip is-on", text: "save", attrs: { type: "submit", "data-save-sid": e.id } }),
		el("button", { class: "enc-chip", text: "cancel", attrs: { type: "button", "data-cancel-edit": e.id } }),
	);
	return form;
}

function fillScheduleEntityOptions() {
	const select = clear($("camp-schedule-entity"));
	select.append(el("option", { text: "—", attrs: { value: "" } }));
	for (const ent of entities) {
		select.append(el("option", { text: `${ent.name} (${ent.kind})`, attrs: { value: ent.id } }));
	}
}

function onScheduleClick(e) {
	const del = e.target.closest("[data-del-sid]");
	if (del) {
		api.campaignScheduleDelete(current.id, del.dataset.delSid)
			.then(() => loadSchedule().then(() => loadClock()))
			.catch((err) => renderMeta(err.message, true));
		return;
	}
	const edit = e.target.closest("[data-edit-sid]");
	if (edit) {
		editingScheduleID = edit.dataset.editSid;
		renderSchedule();
		return;
	}
	const cancel = e.target.closest("[data-cancel-edit]");
	if (cancel) {
		editingScheduleID = null;
		renderSchedule();
	}
}

function onScheduleInlineSubmit(e) {
	e.preventDefault();
	const form = e.target.closest("[data-sid]");
	if (!form) return;
	const sid = form.dataset.sid;
	const patch = {
		name: form.querySelector(".camp-sched-edit-name").value.trim(),
		day: parseInt(form.querySelector(".camp-sched-edit-day").value, 10),
		recurrence: form.querySelector(".camp-sched-edit-rec").value,
		status: form.querySelector(".camp-sched-edit-status").value,
	};
	api.campaignScheduleUpdate(current.id, sid, patch)
		.then(() => {
			editingScheduleID = null;
			return loadSchedule().then(() => loadClock());
		})
		.catch((err) => renderMeta(err.message, true));
}

async function onCreateScheduleEntry(e) {
	e.preventDefault();
	const entry = {
		name: $("camp-schedule-name").value.trim(),
		day: parseInt($("camp-schedule-day").value, 10),
		recurrence: $("camp-schedule-recurrence").value,
		visibility: $("camp-schedule-visibility").value,
	};
	const whose = $("camp-schedule-entity").value;
	if (whose) entry.entity_id = whose;
	try {
		await api.campaignScheduleCreate(current.id, entry);
		$("camp-schedule-name").value = "";
		$("camp-schedule-day").value = "";
		await Promise.all([loadSchedule(), loadClock()]);
	} catch (err) {
		renderMeta(err.message, true);
	}
}

async function onAdvanceClock(e) {
	e.preventDefault();
	const raw = $("camp-advance-days").value.trim();
	if (!raw) return;
	const by = parseInt(raw, 10);
	if (!Number.isFinite(by)) return;
	try {
		const data = await api.campaignClockAdvance(current.id, { by }, $("camp-advance-reason").value, "");
		if (data.clock) renderMeta(`The days move to ${data.clock.date}.`);
		$("camp-advance-days").value = "";
		await Promise.all([loadClock(), loadSchedule()]);
	} catch (err) {
		renderMeta(err.message, true);
	}
}

// The simulation tick (MAD-367): a deterministic preview of one window —
// nothing written to the graph but the tick's own row — with a "stage as
// proposal" hand-off into the review queue. Accepting that proposal is what
// actually moves the clock.
let simulateTickID = null;

async function onSimulate(e) {
	e.preventDefault();
	const days = parseInt($("camp-simulate-days").value, 10) || 14;
	const rawSeed = $("camp-simulate-seed").value.trim();
	const seed = rawSeed ? parseInt(rawSeed, 10) : undefined;
	try {
		const data = await api.campaignSimulate(current.id, days, seed);
		simulateTickID = data.tick?.id || null;
		renderSimulateResult(data);
	} catch (err) {
		renderMeta(err.message, true);
	}
}

function renderSimulateResult(data) {
	const box = clear($("camp-simulate-result"));
	box.hidden = false;
	const r = data.result || {};
	box.append(el("p", { class: "camp-status", text: `Day ${r.from_day} → ${r.to_day} (seed ${r.seed})${data.offline ? " — offline, deterministic summary only" : ""}` }));
	const flavour = data.flavour || {};
	const line = (id, fallback) => flavour[id] || fallback;
	for (const p of r.plans || []) {
		if (!p.moved) continue;
		const id = `plan-${p.plan_id}`;
		const fallback = `moves from ${p.progression.from_state} to ${p.progression.to_state} (gain ${p.progression.gain}).`;
		box.append(el("p", { class: "camp-sim-line", text: `⛭ ${p.faction_name}: ${p.name} — ${line(id, fallback)}` }));
	}
	for (const d of r.due || []) {
		const id = `due-${d.entry_id}-${d.day}`;
		box.append(el("p", { class: "camp-sim-line", text: `☾ day ${d.day}: ${line(id, `${d.name} happens as scheduled.`)}` }));
	}
	for (const m of r.missed || []) {
		box.append(el("p", { class: "camp-sim-line camp-sim-warn", text: `⚠ ${m.name} (day ${m.day}) is still pending, behind the clock.` }));
	}
	for (const a of r.actions || []) {
		const id = `npcact-${a.npc}`;
		box.append(el("p", { class: "camp-sim-line", text: `☺ day ${a.day}: ${line(id, a.summary)}` }));
	}
	for (const c of r.consequences || []) {
		const id = `react-${c.reactor}-${c.plan_id}`;
		box.append(el("p", { class: "camp-sim-line", text: `⚔ day ${c.day}: ${line(id, c.summary)}` }));
	}
	if (!(r.plans?.some((p) => p.moved) || r.due?.length || r.actions?.length || r.consequences?.length)) {
		box.append(el("p", { class: "camp-status", text: "Nothing moves in this window." }));
	} else {
		box.append(el("button", { class: "enc-btn primary", text: "Stage as proposal", attrs: { type: "button", "data-act": "stage-tick" } }));
	}
}

async function onSimulateResultClick(e) {
	const btn = e.target.closest("[data-act='stage-tick']");
	if (!btn || !simulateTickID) return;
	try {
		await api.campaignSimulateStage(current.id, simulateTickID);
		renderMeta("Staged — decide it on the review queue.");
		$("camp-simulate-result").hidden = true;
		simulateTickID = null;
		reviewFor(current.id);
	} catch (err) {
		renderMeta(err.message, true);
	}
}

/* ---------- the road (MAD-375) ---------- */

let journeyDays = []; // the selected journey's day table

// fillRoadOptions populates the from/to selects with the campaign's
// locations — the same entity list the browser holds.
function fillRoadOptions() {
	const locations = entities.filter((e) => e.kind === "location");
	for (const select of [$("camp-road-from"), $("camp-road-to")]) {
		const prev = select.value;
		clear(select);
		select.append(el("option", { text: "—", attrs: { value: "" } }));
		for (const loc of locations) {
			select.append(el("option", { text: loc.name, attrs: { value: loc.id } }));
		}
		if (prev && locations.some((l) => l.id === prev)) select.value = prev;
	}
}

async function onPlanJourney(e) {
	e.preventDefault();
	const plan = {
		from: $("camp-road-from").value,
		to: $("camp-road-to").value,
		density: $("camp-road-density").value,
		pace: $("camp-road-pace").value,
	};
	if (!plan.from || !plan.to) {
		renderMeta("A journey needs a from and a to.", true);
		return;
	}
	const days = parseInt($("camp-road-days").value, 10);
	if (!Number.isNaN(days) && days > 0) plan.days = days;
	const seed = $("camp-road-seed").value.trim();
	if (seed) plan.seed = parseInt(seed, 10);
	try {
		const data = await api.campaignJourneyPlan(current.id, plan);
		renderMeta("The road is planned — resolve days as you play them, then resolve the journey.");
		selectedJourneyID = data.journey?.id || null;
		await loadJourneys();
	} catch (err) {
		renderMeta(err.message, true);
	}
}

async function loadJourneys() {
	if (!current) return;
	let data;
	try {
		data = await api.campaignJourneys(current.id);
	} catch (err) {
		journeys = [];
		renderJourneys();
		return;
	}
	journeys = data.journeys || [];
	if (!selectedJourneyID && journeys.length > 0) selectedJourneyID = journeys[0].id;
	renderJourneys();
	if (selectedJourneyID) loadJourneyDays();
}

async function loadJourneyDays() {
	if (!current || !selectedJourneyID) return;
	let data;
	try {
		data = await api.campaignJourney(current.id, selectedJourneyID);
	} catch (err) {
		journeyDays = [];
		renderJourneys();
		return;
	}
	journeyDays = data.days || [];
	const box = $("camp-journey-days-" + selectedJourneyID);
	if (box) renderDayTable(box, data.journey);
}

// kindChip is one event kind's table glyph.
const kindGlyphs = {
	encounter: "⚔", discovery: "✦", hazard: "⚠", social: "☺", rumour: "❝", landmark: "⌂", uneventful: "·",
};

function renderJourneys() {
	const box = clear($("camp-journeys"));
	$("camp-road-form").hidden = !isDM();
	if (isDM()) fillRoadOptions();
	if (!isDM()) return;
	if ((journeys || []).length === 0) {
		box.append(el("p", { class: "camp-status", text: "No journeys on the roads yet." }));
		return;
	}
	for (const j of journeys) box.append(journeyRow(j));
}

function journeyRow(j) {
	const row = el("div", { class: "camp-journey", attrs: { "data-jid": j.id } });
	const head = el("div", { class: "camp-journey-head" });
	const chips = el("span", { class: "camp-fact-chips" });
	chips.append(el("span", { class: "enc-chip", text: `${j.days}d` }));
	chips.append(el("span", { class: "enc-chip", text: j.density }));
	if (j.status !== "planned") chips.append(el("span", { class: "enc-chip" + (j.status === "done" ? " is-on" : ""), text: j.status }));
	head.append(
		el("span", { class: "camp-journey-route", text: `${j.from_name} → ${j.to_name}` }),
		chips,
	);
	if (isDM() && j.status !== "done" && j.status !== "abandoned") {
		head.append(
			el("button", { class: "enc-chip", text: "abandon", attrs: { type: "button", "data-abandon-journey": j.id } }),
		);
	}
	row.append(head);
	if (selectedJourneyID === j.id || journeys.length === 1) {
		const days = el("div", { class: "camp-journey-days", id: "camp-journey-days-" + j.id });
		renderDayTable(days, j);
		row.append(days);
	}
	return row;
}

// renderDayTable draws the strip: each day a card with its weather and
// its incident, resolved days inked and unresolved ghosted — the same
// grammar the quest board's taken and untaken branches use.
function renderDayTable(box, j) {
	const strip = clear(box);
	if (!j) return;
	strip.append(el("p", { class: "camp-journey-line", text: j.line || `${j.days} days.` }));
	if ((journeyDays || []).length === 0) return;
	const wrap = el("div", { class: "camp-journey-strip f-parchment", attrs: { role: "list", "aria-label": "Day table" } });
	for (const d of journeyDays) {
		const card = el("div", {
			class: "camp-day" + (d.resolved ? " is-inked" : " is-ghost"),
			attrs: {
				role: "listitem",
				title: `${d.date || "day " + d.clock_day} — ${d.weather?.summary || ""}, ${d.weather?.temp_c}°, ${d.weather?.wind || ""}` + (d.encounter_budget ? ` — ${d.encounter_budget}` : ""),
			},
		});
		card.append(
			el("span", { class: "camp-day-when", text: d.date || "day " + d.clock_day }),
			el("span", { class: "camp-day-kind", text: `${kindGlyphs[d.event_kind] || "·"} ${d.event_kind}` }),
			el("span", { class: "camp-day-weather", text: `${d.weather?.summary || ""} ${d.weather?.temp_c}°` }),
		);
		if (d.detail) card.append(el("p", { class: "camp-day-detail", text: d.detail }));
		if (d.entity_name) card.append(el("span", { class: "camp-day-entity", text: d.entity_name }));
		if (isDM() && !d.resolved && j.status !== "done" && j.status !== "abandoned") {
			card.append(el("button", {
				class: "enc-chip", text: "happened",
				attrs: { type: "button", "data-resolve-day": String(d.index) },
			}));
		}
		wrap.append(card);
	}
	strip.append(wrap);
	if (isDM() && j.status !== "done" && j.status !== "abandoned") {
		strip.append(el("button", {
			class: "enc-btn primary", text: "Resolve the journey",
			attrs: { type: "button", "data-resolve-journey": j.id },
		}));
	}
}

async function onJourneyClick(e) {
	const day = e.target.closest("[data-resolve-day]");
	if (day) {
		const jid = day.closest("[data-jid]")?.dataset.jid;
		if (!jid) return;
		const idx = day.dataset.resolveDay;
		const detail = prompt("What actually happened that day (leave empty to keep the roll's account)?");
		if (detail === null) return;
		try {
			await api.campaignJourneyDayResolve(current.id, jid, idx, detail ? { detail } : {});
			await loadJourneyDays();
		} catch (err) {
			renderMeta(err.message, true);
		}
		return;
	}
	const whole = e.target.closest("[data-resolve-journey]");
	if (whole) {
		try {
			await api.campaignJourneyResolve(current.id, whole.dataset.resolveJourney);
			renderMeta("The road is staged — decide it on the review queue.");
			await loadJourneys();
			reviewFor(current.id);
		} catch (err) {
			renderMeta(err.message, true);
		}
		return;
	}
	const abandon = e.target.closest("[data-abandon-journey]");
	if (abandon) {
		try {
			await api.campaignJourneyPatch(current.id, abandon.dataset.abandonJourney, { status: "abandoned" });
			await loadJourneys();
		} catch (err) {
			renderMeta(err.message, true);
		}
	}
}

async function onSheetClick(e) {
	// The location dossier's chips link into the entity browser: one click
	// opens the entity the chip names, exactly like the browser's own rows.
	const open = e.target.closest("[data-open-eid]");
	if (open) {
		selectEntity(open.dataset.openEid);
		return;
	}
	const activate = e.target.closest("[data-plan-activate]");
	if (activate) {
		try {
			await api.campaignFactionPlanUpdate(current.id, activate.dataset.planActivate, { status: "active" });
			await refreshFactionDossier();
		} catch (err) {
			renderMeta(err.message, true);
		}
		return;
	}
	const move = e.target.closest("[data-plan-move]");
	if (move) {
		try {
			await api.campaignFactionPlanTransition(current.id, move.dataset.planMove, move.dataset.moveTo, "moved from the sheet");
			await refreshFactionDossier();
		} catch (err) {
			renderMeta(err.message, true);
		}
		return;
	}
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

/* ---------- the dungeon workshop (MAD-373) ---------- */

// The dungeon surface: knobs in, a seeded room graph out, drawn as the
// map its grid says it is. The whole exchange needs no model — the
// layout is arithmetic on the server — and every edit (dragging a room,
// adding or cutting an edge) writes rows back; an edit never re-rolls
// the dungeon. Dressing asks the model to name the rooms it was handed;
// placing stages the whole dungeon as one proposal batch.

const DUNGEON_CELL = 64; // one grid cell, whole pixels

function clearDungeons() {
	dungeons = [];
	selectedDungeonID = null;
	dungeonDetail = null;
	clear($("camp-dungeons"));
	clear($("camp-dungeon-detail"));
}

async function loadDungeons() {
	const board = $("camp-dungeons");
	if (!current) return;
	let data;
	try {
		data = await api.dungeons(current.id);
	} catch (err) {
		dungeons = [];
		clear(board).append(el("p", { class: "camp-status warn", text: err.message }));
		clear($("camp-dungeon-detail"));
		return;
	}
	dungeons = data.dungeons || [];
	renderDungeonBoard();
	if (selectedDungeonID && dungeons.some((d) => d.id === selectedDungeonID)) {
		await renderDungeonDetail(selectedDungeonID);
	} else {
		selectedDungeonID = null;
		dungeonDetail = null;
		clear($("camp-dungeon-detail"));
	}
}

function renderDungeonBoard() {
	const board = clear($("camp-dungeons"));
	if (dungeons.length === 0) {
		board.append(el("p", { class: "camp-status", text: "No dungeons designed yet." }));
		return;
	}
	for (const d of dungeons) {
		const row = el("div", { class: "camp-quest" + (d.id === selectedDungeonID ? " is-active" : ""), attrs: { "data-did": d.id } });
		const head = el("div", { class: "camp-quest-head" });
		head.append(
			el("span", { class: "camp-quest-name", text: d.name }),
			el("span", { class: "enc-chip" + (d.status === "placed" ? " is-on" : ""), text: d.status }),
			el("span", { class: "enc-chip", text: `${d.size} · lv ${d.level}` }),
		);
		row.append(head);
		board.append(row);
	}
}

function onDungeonBoardClick(e) {
	const row = e.target.closest("[data-did]");
	if (!row) return;
	selectedDungeonID = row.dataset.did === selectedDungeonID ? null : row.dataset.did;
	renderDungeonBoard();
	if (selectedDungeonID) renderDungeonDetail(selectedDungeonID);
	else {
		dungeonDetail = null;
		clear($("camp-dungeon-detail"));
	}
}

// onDungeonDesign creates a dungeon from the knobs: the same params and
// seed always produce the same rooms — re-rolling means a new seed, a
// recorded decision, not a refresh button.
async function onDungeonDesign(e) {
	e.preventDefault();
	const body = {
		name: $("camp-dungeon-name").value.trim(),
		theme: $("camp-dungeon-theme").value.trim(),
		size: $("camp-dungeon-size").value,
		level: parseInt($("camp-dungeon-level").value, 10) || 0,
		expected_sessions: parseInt($("camp-dungeon-sessions").value, 10) || 0,
		combat_density: parseInt($("camp-dungeon-combat").value, 10) || 0,
		puzzle_density: parseInt($("camp-dungeon-puzzle").value, 10) || 0,
		explore_density: parseInt($("camp-dungeon-explore").value, 10) || 0,
		branchiness: parseInt($("camp-dungeon-branch").value, 10) || 0,
	};
	const seed = parseInt($("camp-dungeon-seed").value, 10);
	if (Number.isFinite(seed) && seed !== 0) body.seed = seed;
	renderMeta("Designing the dungeon…");
	let data;
	try {
		data = await api.dungeonCreate(current.id, body);
	} catch (err) {
		renderMeta(err.message, true);
		return;
	}
	$("camp-dungeon-name").value = "";
	renderMeta(`Designed ${data.dungeon.rooms.length} rooms — same seed, same dungeon.`);
	selectedDungeonID = data.dungeon.id;
	await loadDungeons();
}

// renderDungeonDetail opens one dungeon: the map drawn from the stored
// grid (the rendering of the graph, not a second artefact), the room
// list, and the edit controls — dress, place, add and cut edges.
async function renderDungeonDetail(did) {
	const box = $("camp-dungeon-detail");
	clear(box).append(el("p", { class: "camp-status", text: "Opening the map…" }));
	let data;
	try {
		data = await api.dungeonGet(current.id, did);
	} catch (err) {
		clear(box).append(el("p", { class: "camp-status warn", text: err.message }));
		return;
	}
	clear(box);
	dungeonDetail = data.dungeon;
	box.append(renderDungeonMap(dungeonDetail));
	box.append(dungeonEditForms(dungeonDetail));
}

// renderDungeonMap draws the deterministic map: one cell per room at its
// stored grid position, ink lines between connected rooms, the pixel
// font for labels. Hand-rolled SVG the way the neighbourhood map is —
// no library, no physics, whole-pixel geometry. Rooms are draggable for
// the DM: a drop writes the new cell back and nothing else changes.
function renderDungeonMap(d) {
	const rooms = d.rooms || [];
	const edges = d.edges || [];
	const maxX = Math.max(0, ...rooms.map((r) => r.x));
	const maxY = Math.max(0, ...rooms.map((r) => r.y));
	const W = (maxX + 1) * DUNGEON_CELL + DUNGEON_CELL;
	const H = (maxY + 1) * DUNGEON_CELL + DUNGEON_CELL;
	const cx = (x) => DUNGEON_CELL / 2 + x * DUNGEON_CELL;
	const cy = (y) => DUNGEON_CELL / 2 + y * DUNGEON_CELL;
	const svgNS = "http://www.w3.org/2000/svg";
	const svg = document.createElementNS(svgNS, "svg");
	svg.setAttribute("viewBox", `0 0 ${W} ${H}`);
	svg.setAttribute("class", "camp-graph-svg camp-dungeon-svg");
	svg.setAttribute("role", "img");
	svg.setAttribute("aria-label", `Map of ${d.name}`);

	const byKey = new Map(rooms.map((r) => [r.key, r]));
	for (const e of edges) {
		const a = byKey.get(e.from_room), b = byKey.get(e.to_room);
		if (!a || !b) continue;
		const line = document.createElementNS(svgNS, "line");
		line.setAttribute("x1", cx(a.x)); line.setAttribute("y1", cy(a.y));
		line.setAttribute("x2", cx(b.x)); line.setAttribute("y2", cy(b.y));
		line.setAttribute("class", "camp-edge" + (e.kind === "secret_door" ? " is-ghost" : ""));
		if (e.id) line.setAttribute("data-edge", e.id);
		const title = document.createElementNS(svgNS, "title");
		title.textContent = e.kind.replace("_", " ") + (e.one_way ? " (one way)" : "");
		line.append(title);
		svg.append(line);
	}
	for (const r of rooms) {
		const g = document.createElementNS(svgNS, "g");
		g.setAttribute("class", "camp-node" + (r.purpose === "boss" || r.purpose === "entrance" ? " is-center" : ""));
		g.setAttribute("transform", `translate(${cx(r.x)},${cy(r.y)})`);
		g.setAttribute("tabindex", "0");
		g.setAttribute("aria-label", `${r.name || r.purpose} (${r.purpose}, depth ${r.depth})`);
		g.dataset.rid = r.id;
		g.dataset.key = r.key;
		g.dataset.x = String(r.x);
		g.dataset.y = String(r.y);
		const half = 14;
		const rect = document.createElementNS(svgNS, "rect");
		rect.setAttribute("x", -half); rect.setAttribute("y", -half);
		rect.setAttribute("width", half * 2); rect.setAttribute("height", half * 2);
		const label = document.createElementNS(svgNS, "text");
		label.setAttribute("y", half + 12);
		label.setAttribute("class", "camp-node-label");
		const name = r.name || r.purpose;
		label.textContent = name.length > 16 ? name.slice(0, 15) + "…" : name;
		const title = document.createElementNS(svgNS, "title");
		title.textContent = `${r.key} — ${r.purpose} (depth ${r.depth})${r.detail ? `: ${r.detail}` : ""}`;
		g.append(rect, label, title);
		svg.append(g);
	}
	return svg;
}

// dungeonEditForms builds the DM controls under the map: dress, place,
// the edge add form, and the room list with drag hints. Delegated
// handlers run the interactions; the forms carry the dungeon id.
function dungeonEditForms(d) {
	const wrap = el("div", { class: "camp-dungeon-controls" });

	const actions = el("div", { class: "camp-fact-chips" });
	actions.append(el("button", {
		class: "enc-chip", text: "dress the rooms",
		attrs: { type: "button", "data-dress": d.id },
	}));
	actions.append(el("button", {
		class: "enc-chip", text: "place in the world",
		attrs: { type: "button", "data-place": d.id },
	}));
	actions.append(el("button", {
		class: "enc-chip", text: "delete the design",
		attrs: { type: "button", "data-del-dungeon": d.id },
	}));
	wrap.append(actions);

	if (d.secret) {
		wrap.append(el("p", { class: "camp-status", text: `Secret: ${d.secret}` }));
	}
	if (d.key_item) {
		wrap.append(el("p", { class: "camp-status", text: `The locked door needs ${d.key_item}.` }));
	}

	// The edge form: two rooms and a kind. Dragging writes cells; this
	// writes connections — the two edits a map is made of.
	const form = el("form", { class: "camp-sched-edit", attrs: { "data-edge-form": d.id } });
	const from = el("select", { class: "enc-field", attrs: { "aria-label": "From room", required: "" } });
	const to = el("select", { class: "enc-field", attrs: { "aria-label": "To room", required: "" } });
	const kind = el("select", { class: "enc-field", attrs: { "aria-label": "Connection kind" } });
	for (const k of ["door", "locked_door", "secret_door", "stair", "shaft", "passage", "collapse"]) {
		kind.append(el("option", { text: k.replace("_", " "), attrs: { value: k } }));
	}
	for (const r of d.rooms) {
		const label = `${r.name || r.purpose} (${r.key})`;
		from.append(el("option", { text: label, attrs: { value: r.key } }));
		to.append(el("option", { text: label, attrs: { value: r.key } }));
	}
	form.append(from, to, kind, el("button", { class: "enc-btn", text: "Connect", attrs: { type: "submit" } }));
	wrap.append(el("h4", { class: "camp-sheet-heading", text: "Connect two rooms" }), form);

	// The room list: purpose, depth and the encounter link, with a
	// click-to-cut marker for edges.
	const list = el("div", { class: "camp-dungeon-rooms" });
	for (const r of d.rooms) {
		const row = el("div", { class: "camp-fact", attrs: { "data-room": r.id } });
		const chips = el("span", { class: "camp-fact-chips" });
		chips.append(el("span", { class: "enc-chip", text: r.purpose }));
		chips.append(el("span", { class: "enc-chip", text: `depth ${r.depth}` }));
		if (r.encounter_id) chips.append(el("span", { class: "enc-chip is-on", text: "encounter linked" }));
		row.append(
			el("p", { class: "camp-fact-statement prose", text: `${r.name || r.purpose}${r.detail ? ` — ${r.detail}` : ""}` }),
			chips,
		);
		list.append(row);
	}
	wrap.append(el("h4", { class: "camp-sheet-heading", text: "The rooms" }), list);
	return wrap;
}

// onDungeonDetailClick runs the map's chip actions and edge cutting.
async function onDungeonDetailClick(e) {
	if (!current || !dungeonDetail) return;
	const did = dungeonDetail.id;

	if (e.target.closest("[data-dress]")) {
		renderMeta("Dressing the rooms…");
		try {
			const data = await api.dungeonDress(current.id, did);
			dungeonDetail = data.dungeon;
			renderMeta("The rooms are dressed.");
			await renderDungeonDetail(did);
		} catch (err) {
			renderMeta(err.message, true);
		}
		return;
	}
	if (e.target.closest("[data-place]")) {
		renderMeta("Staging the placement…");
		try {
			await api.dungeonPlace(current.id, did);
			renderMeta("The placement is staged — accept it on the review queue.");
			await reviewFor(current.id);
		} catch (err) {
			renderMeta(err.message, true);
		}
		return;
	}
	if (e.target.closest("[data-del-dungeon]")) {
		try {
			await api.dungeonDelete(current.id, did);
			selectedDungeonID = null;
			renderMeta("The design is deleted.");
			await loadDungeons();
		} catch (err) {
			renderMeta(err.message, true);
		}
		return;
	}
	// Cutting an edge: click a map edge (they carry data-edge).
	const edgeEl = e.target.closest("[data-edge]");
	if (edgeEl && edgeEl.dataset.edge) {
		try {
			const data = await api.dungeonEdgeDelete(current.id, did, edgeEl.dataset.edge);
			dungeonDetail = data.dungeon;
			await renderDungeonDetail(did);
		} catch (err) {
			renderMeta(err.message, true);
		}
	}
}

// onDungeonDetailSubmit runs the edge form: two rooms and a kind.
async function onDungeonDetailSubmit(e) {
	if (!current || !dungeonDetail) return;
	const form = e.target.closest("[data-edge-form]");
	if (!form) return;
	e.preventDefault();
	const [from, to, kind] = form.querySelectorAll("select");
	if (from.value === to.value) {
		renderMeta("A room cannot connect to itself.", true);
		return;
	}
	try {
		const data = await api.dungeonEdgeAdd(current.id, dungeonDetail.id, {
			from: from.value, to: to.value, kind: kind.value,
		});
		dungeonDetail = data.dungeon;
		renderMeta("Connected.");
		await renderDungeonDetail(dungeonDetail.id);
	} catch (err) {
		renderMeta(err.message, true);
	}
}

/* ---------- dragging a room writes the cell back ---------- */

// onRoomPointerDown begins a drag on a map room (DM, mouse or touch):
// the room follows the pointer in whole cells and the drop writes the
// new cell back. An edit never re-rolls the dungeon — the PATCH carries
// x and y only.
function onRoomPointerDown(e) {
	if (!isDM() || !dungeonDetail) return;
	const g = e.target.closest("svg .camp-node");
	if (!g || !g.dataset.rid) return;
	e.preventDefault();
	const svg = g.closest("svg");
	const cell = DUNGEON_CELL;
	const toCell = (evt) => {
		const box = svg.getBoundingClientRect();
		const scale = (box.width / svg.viewBox.baseVal.width) || 1;
		const x = Math.floor(((evt.clientX - box.left) / scale) / cell);
		const y = Math.floor(((evt.clientY - box.top) / scale) / cell);
		return { x: Math.max(0, x), y: Math.max(0, y) };
	};
	const home = { x: Number(g.dataset.x), y: Number(g.dataset.y) };
	const place = (at) => g.setAttribute("transform", `translate(${cell / 2 + at.x * cell},${cell / 2 + at.y * cell})`);
	g.setPointerCapture?.(e.pointerId);
	const move = (ev) => place(toCell(ev));
	const up = async (ev) => {
		svg.removeEventListener("pointermove", move);
		svg.removeEventListener("pointerup", up);
		const at = toCell(ev);
		if (at.x === home.x && at.y === home.y) return; // a tap, not a drag
		try {
			const data = await api.dungeonRoomPatch(current.id, dungeonDetail.id, g.dataset.rid, { x: at.x, y: at.y });
			dungeonDetail = data.dungeon;
			renderDungeonDetail(dungeonDetail.id);
		} catch (err) {
			renderMeta(err.message, true);
			renderDungeonDetail(dungeonDetail.id); // put the room back
		}
	};
	svg.addEventListener("pointermove", move);
	svg.addEventListener("pointerup", up);
}

/* ---------- the place designer (MAD-372) ---------- */

// fillPlaceNear offers the campaign's existing places as the near anchor:
// a village generated near Blackwater gets the road and the edge to the
// Blackwater that already exists, never a second one.
function fillPlaceNear() {
	const select = $("camp-place-near");
	if (!select) return;
	const prev = select.value;
	clear(select).append(el("option", { text: "Nowhere in particular", attrs: { value: "" } }));
	for (const ent of entities.filter((e) => e.kind === "location")) {
		select.append(el("option", { text: ent.name, attrs: { value: ent.id } }));
	}
	if ([...select.options].some((o) => o.value === prev)) select.value = prev;
}

// onPlaceDesign runs one place-design exchange: the premise, the
// settlement kind, the scale band, an optional near anchor. The place, its
// people, its sub-locations, its hooks and its secrets come back as one
// proposal batch; nothing lands in the graph until the DM accepts it
// there.
async function onPlaceDesign(e) {
	e.preventDefault();
	const premise = $("camp-place-premise").value.trim();
	if (!premise) return;
	renderMeta("Designing the place…");
	try {
		await api.locationDesign(current.id, {
			premise,
			kind: $("camp-place-kind").value,
			scale: $("camp-place-scale").value,
			near: $("camp-place-near").value,
		});
	} catch (err) {
		renderMeta(err.message, true);
		return;
	}
	$("camp-place-premise").value = "";
	renderMeta("The place is staged — accept it on the review queue.");
	await loadEntities();
	await reviewFor(current.id);
}

// onFleshOutSubmit runs the flesh-out exchange from the location sheet: a
// place that already exists gets its missing block, people and secrets
// proposed around what is already there. Parts re-roll one piece of the
// shape — the people, say, keeping the geography.
async function onFleshOutSubmit(e) {
	const form = e.target.closest("[data-fleshout]");
	if (!form) return;
	e.preventDefault();
	if (!current || !selected) return;
	const body = {
		premise: form.querySelector("input").value.trim(),
	};
	const parts = form.querySelector("select").value;
	if (parts) body.parts = [parts];
	renderMeta("Fleshing the place out…");
	try {
		await api.locationFleshOut(current.id, selected.id, body);
	} catch (err) {
		renderMeta(err.message, true);
		return;
	}
	renderMeta("The proposal is staged — accept it on the review queue.");
	await reviewFor(current.id);
}

/* ---------- the quest board (MAD-369) ---------- */

// onQuestDesign runs one quest-design exchange (MAD-371): the hook, an
// optional shape, fork count and depth. The quest, its cast and its
// revealed secrets come back as a proposal batch; nothing lands in the
// graph until the DM accepts it there.
async function onQuestDesign(e) {
	e.preventDefault();
	const hook = $("camp-quest-hook").value.trim();
	if (!hook) return;
	const kind = $("camp-quest-kind").value;
	const branchPoints = parseInt($("camp-quest-branches").value, 10) || 0;
	const depth = parseInt($("camp-quest-depth").value, 10) || 0;
	renderMeta("Designing the quest…");
	try {
		await api.questDesign(current.id, { hook, kind, branch_points: branchPoints, depth });
	} catch (err) {
		renderMeta(err.message, true);
		return;
	}
	$("camp-quest-hook").value = "";
	renderMeta("The quest is staged — accept it on the review queue.");
	await reviewFor(current.id);
}

// The DM reads every machine; everyone else reads the journal — the public
// quests, their current state and the states already visited. The server
// enforces the leak rules; this module renders whatever comes back.
async function loadQuests() {
	const board = $("camp-quests");
	if (!current) return;
	let data;
	try {
		data = isDM() ? await api.campaignQuests(current.id) : await api.campaignQuestJournal(current.id);
	} catch (err) {
		quests = [];
		clear(board).append(el("p", { class: "camp-status warn", text: err.message }));
		clear($("camp-quest-detail"));
		return;
	}
	quests = data.quests || [];
	if (!isDM()) selectedQuestID = null;
	renderQuestBoard();
	if (isDM() && selectedQuestID) await renderQuestDetail(selectedQuestID);
	else clear($("camp-quest-detail"));
}

function questStateLabel(q, key) {
	const st = (q.state_machine?.states || []).find((s) => s.key === key);
	return st?.label || key;
}

function renderQuestBoard() {
	const board = clear($("camp-quests"));
	if (quests.length === 0) {
		board.append(el("p", { class: "camp-status", text: isDM() ? "No quests recorded yet." : "Nothing offered yet." }));
		return;
	}
	for (const q of quests) {
		const row = el("div", { class: "camp-quest" + (q.id === selectedQuestID ? " is-active" : ""), attrs: { "data-qid": q.id } });
		const head = el("div", { class: "camp-quest-head" });
		head.append(el("span", { class: "camp-quest-name", text: q.name }));
		head.append(el("span", { class: "enc-chip" + (q.status === "active" ? " is-on" : ""), text: q.status || "active" }));
		row.append(head);
		if (isDM()) {
			row.append(el("p", {
				class: "camp-quest-now",
				text: "at " + (questStateLabel(q, q.current_state) || q.current_state),
			}));
			if (q.visibility === "secret") row.append(el("span", { class: "enc-chip", text: "secret" }));
		} else {
			// The journal: the trail of visited states, current one last.
			const trail = el("div", { class: "camp-quest-trail" });
			for (const st of q.visited || []) {
				trail.append(el("span", {
					class: "camp-quest-step" + (st.key === q.current_state?.key ? " is-now" : ""),
					text: st.label || st.key,
				}));
			}
			row.append(trail);
		}
		if (q.summary) row.append(el("p", { class: "camp-quest-summary prose", text: q.summary }));
		board.append(row);
	}
}

function onQuestBoardClick(e) {
	if (!isDM()) return; // the player list is read-only
	const row = e.target.closest("[data-qid]");
	if (!row) return;
	selectedQuestID = row.dataset.qid === selectedQuestID ? null : row.dataset.qid;
	renderQuestBoard();
	if (selectedQuestID) renderQuestDetail(selectedQuestID);
	else clear($("camp-quest-detail"));
}

// renderQuestDetail opens one machine as a deterministic chart plus the
// links into the graph and the DM's move control. The chart is drawn the
// way the neighbourhood map is: hand-rolled SVG, no library, no physics,
// whole-pixel geometry.
async function renderQuestDetail(questID) {
	const box = $("camp-quest-detail");
	clear(box).append(el("p", { class: "camp-status", text: "Opening the machine…" }));
	let data;
	try {
		data = await api.campaignQuestDetail(current.id, questID);
	} catch (err) {
		clear(box).append(el("p", { class: "camp-status warn", text: err.message }));
		return;
	}
	clear(box);
	const q = data.quest;
	box.append(renderQuestMachine(q, data.transitions || []));

	// Who and what the quest is about.
	const links = data.entities || [];
	if (links.length > 0) {
		const chips = el("div", { class: "camp-quest-links" });
		for (const l of links) {
			const name = entities.find((x) => x.id === l.entity_id)?.name || l.entity_id.slice(0, 8);
			chips.append(el("span", { class: "enc-chip", text: `${l.role}: ${name}`, attrs: { "data-unlink": l.entity_id, "data-role": l.role, title: "unlink" } }));
		}
		box.append(chips);
	}

	// The move control: only an active quest moves, and only along real edges.
	if (q.status === "active") {
		const moves = (q.state_machine?.edges || []).filter((ed) => ed.from === q.current_state);
		if (moves.length > 0) {
			const form = el("form", { class: "camp-quest-move", attrs: { "data-move": q.id } });
			const pick = el("select", { class: "enc-field", attrs: { "aria-label": "Move the quest" } });
			for (const m of moves) {
				pick.append(el("option", {
					text: (m.label ? `${m.label} — ` : "") + (questStateLabel(q, m.to) || m.to),
					attrs: { value: m.to },
				}));
			}
			form.append(pick, el("button", { class: "enc-btn", text: "Move", attrs: { type: "submit" } }));
			box.append(form);
		}
		// Branch this quest (MAD-371): two exclusive outcomes off a state
		// the DM picks, proposed as a batch — the mid-campaign fork.
		const branchable = (q.state_machine?.states || []).filter((s) => !s.terminal);
		if (branchable.length > 0) {
			const form = el("form", { class: "camp-quest-move", attrs: { "data-branch": q.id } });
			const pick = el("select", { class: "enc-field", attrs: { "aria-label": "Branch the quest at" } });
			for (const st of branchable) {
				pick.append(el("option", {
					text: (st.label || st.key) + (st.key === q.current_state ? "  (here)" : ""),
					attrs: { value: st.key, selected: st.key === q.current_state ? "" : null },
				}));
			}
			const notes = el("input", {
				class: "enc-field",
				attrs: { type: "text", placeholder: "direction, if any", "aria-label": "Branch notes" },
			});
			form.append(pick, notes, el("button", { class: "enc-btn", text: "Branch", attrs: { type: "submit" } }));
			box.append(form);
		}
		box.append(el("button", {
			class: "enc-chip", text: "abandon the quest",
			attrs: { type: "button", "data-abandon": q.id },
		}));
	}
}

async function onQuestMoveSubmit(e) {
	e.preventDefault();
	const branch = e.target.closest("[data-branch]");
	if (branch) {
		try {
			await api.questBranch(current.id, branch.dataset.branch, {
				state: branch.querySelectorAll("select")[0].value,
				notes: branch.querySelector("input").value.trim(),
			});
			renderMeta("The branch is staged — accept it on the review queue.");
			await reviewFor(current.id);
		} catch (err) {
			renderMeta(err.message, true);
		}
		return;
	}
	const form = e.target.closest("[data-move]");
	if (!form) return;
	try {
		await api.campaignQuestTransition(current.id, form.dataset.move, form.querySelector("select").value);
		await loadQuests();
	} catch (err) {
		renderMeta(err.message, true);
	}
}

async function onQuestDetailClick(e) {
	const abandon = e.target.closest("[data-abandon]");
	if (abandon) {
		try {
			await api.campaignQuestDelete(current.id, abandon.dataset.abandon);
			await loadQuests();
		} catch (err) {
			renderMeta(err.message, true);
		}
		return;
	}
	const unlink = e.target.closest("[data-unlink]");
	if (unlink) {
		try {
			await api.campaignQuestEntityRemove(current.id, selectedQuestID, unlink.dataset.unlink, unlink.dataset.role);
			await renderQuestDetail(selectedQuestID);
		} catch (err) {
			renderMeta(err.message, true);
		}
	}
}

// renderQuestMachine draws one state machine: layers by BFS distance from
// the initial state, states sorted by key inside a layer so the chart is
// stable across renders, the current state in gold, endings ringed, edges
// already taken inked and untaken branches ghosted.
function renderQuestMachine(q, transitions) {
	const machine = q.state_machine || { states: [], edges: [] };
	const states = machine.states || [];
	const edges = machine.edges || [];
	const taken = new Set((transitions || []).map((t) => `${t.from_state}>${t.to_state}`));

	// BFS layers from the initial state; a state nothing reaches still
	// renders, one layer past the deepest reached.
	const dist = new Map([[machine.initial, 0]]);
	const frontier = [machine.initial];
	while (frontier.length > 0) {
		const next = [];
		for (const from of frontier) {
			for (const ed of edges) {
				if (ed.from === from && !dist.has(ed.to)) {
					dist.set(ed.to, dist.get(from) + 1);
					next.push(ed.to);
				}
			}
		}
		frontier.length = 0;
		frontier.push(...next);
	}
	let layers = states.map((s) => dist.has(s.key) ? dist.get(s.key) : 999);
	const maxLayer = Math.max(0, ...layers.map((d) => (d === 999 ? -1 : d)));
	const byLayer = new Map();
	states.forEach((s, i) => {
		const layer = layers[i] === 999 ? maxLayer + 1 : layers[i];
		if (!byLayer.has(layer)) byLayer.set(layer, []);
		byLayer.get(layer).push(s);
	});

	const COL = 190, ROW = 64, PAD = 14;
	const rows = Math.max(...[...byLayer.values()].map((r) => r.length), 1);
	const W = Math.max(360, (byLayer.size) * COL + PAD * 2);
	const H = rows * ROW + PAD * 2 + 8;
	const pos = new Map();
	for (const [layer, group] of [...byLayer.entries()].sort((a, b) => a[0] - b[0])) {
		group.sort((a, b) => a.key.localeCompare(b.key));
		const top = (H - group.length * ROW) / 2;
		group.forEach((s, i) => {
			pos.set(s.key, { x: Math.round(PAD + layer * COL + 70), y: Math.round(top + i * ROW + ROW / 2) });
		});
	}

	const svgNS = "http://www.w3.org/2000/svg";
	const svg = document.createElementNS(svgNS, "svg");
	svg.setAttribute("viewBox", `0 0 ${W} ${H}`);
	svg.setAttribute("class", "camp-graph-svg camp-quest-svg");
	svg.setAttribute("role", "img");
	svg.setAttribute("aria-label", `State machine of ${q.name}`);

	for (const ed of edges) {
		const a = pos.get(ed.from), b = pos.get(ed.to);
		if (!a || !b) continue;
		const line = document.createElementNS(svgNS, "line");
		line.setAttribute("x1", a.x); line.setAttribute("y1", a.y);
		line.setAttribute("x2", b.x); line.setAttribute("y2", b.y);
		line.setAttribute("class", "camp-edge" + (taken.has(`${ed.from}>${ed.to}`) ? " is-taken" : " is-ghost"));
		svg.append(line);
		const label = ed.label || "";
		if (label) {
			const mid = document.createElementNS(svgNS, "text");
			mid.setAttribute("x", Math.round((a.x + b.x) / 2));
			mid.setAttribute("y", Math.round((a.y + b.y) / 2) - 3);
			mid.setAttribute("class", "camp-edge-label");
			mid.textContent = label.length > 16 ? label.slice(0, 15) + "…" : label;
			svg.append(mid);
		}
	}
	for (const s of states) {
		const p = pos.get(s.key);
		const g = document.createElementNS(svgNS, "g");
		g.setAttribute("class", "camp-node camp-qnode" +
			(s.key === q.current_state ? " is-center" : "") +
			(s.terminal ? " is-terminal" : "") +
			(s.key === machine.initial ? " is-initial" : ""));
		g.setAttribute("transform", `translate(${p.x},${p.y})`);
		const name = s.label || s.key;
		const rect = document.createElementNS(svgNS, "rect");
		const w = Math.min(150, Math.max(70, name.length * 7 + 16));
		rect.setAttribute("x", Math.round(-w / 2)); rect.setAttribute("y", -14);
		rect.setAttribute("width", w); rect.setAttribute("height", 28);
		const text = document.createElementNS(svgNS, "text");
		text.setAttribute("y", 4);
		text.setAttribute("class", "camp-node-label");
		text.textContent = name.length > 19 ? name.slice(0, 18) + "…" : name;
		const title = document.createElementNS(svgNS, "title");
		title.textContent = s.key + (s.terminal ? ` (ending: ${s.terminal})` : "") + (s.detail ? ` — ${s.detail}` : "");
		g.append(rect, text, title);
		svg.append(g);
	}
	return svg;
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

/* ---------- the faction page (MAD-366) ---------- */

// The dossier is a read, not a write: territory, leaders, members, allies
// and enemies arrive as live graph edges the server categorized at the
// caller's scope; the plans are DM-only and simply absent from a player's
// payload. This module renders whatever comes back and decides nothing.
let factionDossier = null; // the last dossier body for the selected faction

async function loadFactionDossier(eid) {
	const mount = document.getElementById("camp-faction-body");
	if (!mount || !current || !selected || selected.id !== eid) return;
	let data;
	try {
		data = await api.campaignFaction(current.id, eid);
	} catch (err) {
		clear(mount).append(el("p", { class: "camp-status warn", text: err.message }));
		return;
	}
	if (!selected || selected.id !== eid) return; // the sheet moved on
	factionDossier = data.faction;
	renderFactionDossier(clear(mount));
}

async function refreshFactionDossier() {
	if (selected && selected.kind === "faction") await loadFactionDossier(selected.id);
}

function renderFactionDossier(mount) {
	const f = factionDossier;
	if (!f) return;
	const wrap = el("div", { class: "camp-faction-body" });

	// The interior: the DM reads the whole agent block, a player reads the
	// public face and the reputation — the server already drew that line.
	const face = el("div", { class: "camp-faction-face" });
	if (isDM() && f.agent) {
		const a = f.agent;
		if (a.public_face) face.append(el("p", { class: "camp-fact-statement prose", text: a.public_face }));
		if (a.private_truth) face.append(el("p", { class: "camp-faction-truth prose", text: `Private truth: ${a.private_truth}` }));
		if (a.doctrine) face.append(el("p", { class: "prose", text: a.doctrine }));
		if ((a.goals || []).length) {
			const goals = el("ol", { class: "camp-faction-goals" });
			for (const g of a.goals) goals.append(el("li", { text: g }));
			face.append(el("p", { class: "camp-sheet-heading", text: "Goals, first pursued first" }), goals);
		}
		if (a.reputation) face.append(el("p", { class: "prose", text: `Reputation: ${a.reputation}` }));
		if ((a.internal_conflicts || []).length) {
			face.append(el("p", { class: "prose", text: `Fault lines: ${a.internal_conflicts.join("; ")}` }));
		}
		const scores = el("div", { class: "camp-fact-chips" });
		for (const [label, v] of [["military", a.military], ["economic", a.economic], ["reach", a.reach]]) {
			scores.append(el("span", { class: "enc-chip", text: `${label} ${v}` }));
		}
		face.append(scores);
	} else {
		if (f.public_face) face.append(el("p", { class: "camp-fact-statement prose", text: f.public_face }));
		if (f.reputation) face.append(el("p", { class: "prose", text: `Reputation: ${f.reputation}` }));
	}
	if (face.children.length > 0) wrap.append(section("The face it shows", face));

	// The graph position: live edges, named where the scope can read them.
	const nameOf = (id) => f.roster?.[id]?.name || entities.find((x) => x.id === id)?.name || id.slice(0, 8);
	const ties = el("div", { class: "camp-faction-ties" });
	const groups = [
		["Territory", f.edges?.territory],
		["Leaders", f.edges?.leaders],
		["Members", f.edges?.members],
		["Allies", f.edges?.allies],
		["Enemies", f.edges?.enemies],
		["Pulls the strings of", f.edges?.puppets],
	];
	let anyTie = false;
	for (const [label, ids] of groups) {
		if (!ids || ids.length === 0) continue;
		anyTie = true;
		const chips = el("div", { class: "camp-fact-chips" });
		for (const id of ids) chips.append(el("span", { class: "enc-chip", text: nameOf(id) }));
		ties.append(el("p", { class: "camp-sheet-heading", text: label }), chips);
	}
	if (anyTie) wrap.append(section("Where it stands", ties));

	// The plans: progress bar, step list, and the DM's controls. Absent
	// entirely at a player scope — the server never sends them.
	if (isDM()) {
		const plans = el("div", { class: "camp-plans" });
		for (const p of f.plans || []) plans.append(planCard(p));
		wrap.append(section("Plans", plans));
		const form = planCreateForm();
		wrap.append(form);
	}
	mount.append(wrap);
}

// planCard renders one plan: name, status, the bar, the checklist and the
// legal next moves as buttons — the server only offers states the machine
// declares, so the buttons cannot offer an illegal move.
function planCard(p) {
	const card = el("div", { class: "camp-plan", attrs: { "data-pid": p.id } });
	const head = el("div", { class: "camp-fact-chips" });
	head.append(el("span", { class: "enc-chip is-on", text: p.status }));
	if (p.visibility === "secret") head.append(el("span", { class: "enc-chip", text: "secret" }));
	head.append(el("span", { class: "enc-chip", text: `${p.rate_per_day}/day` }));
	if (p.last_advanced_day != null) head.append(el("span", { class: "enc-chip", text: `advanced day ${p.last_advanced_day}` }));
	card.append(el("p", { class: "camp-sheet-heading", text: p.name }), head);

	const pct = Math.round((p.percent || 0) * 100);
	const bar = el("div", { class: "camp-plan-bar", attrs: { role: "progressbar", "aria-valuenow": String(pct), "aria-valuemin": "0", "aria-valuemax": "100", "aria-label": `${p.name} progress` } });
	bar.append(el("div", { class: "camp-plan-bar-fill", attrs: { style: `width:${pct}%` } }));
	const line = el("div", { class: "camp-plan-state" });
	line.append(el("span", { class: "camp-plan-state-name", text: p.current_state }));
	line.append(el("span", { class: "camp-status", text: `${pct}%` }));
	card.append(bar, line);

	const list = el("ul", { class: "camp-plan-steps" });
	for (const s of p.steps || []) {
		const row = el("li", { class: "camp-plan-step" + (s.done ? " is-done" : "") });
		const mark = el("span", { class: "camp-plan-mark", text: s.done ? "x" : " " });
		const label = s.name || s.state;
		const cost = typeof s.cost === "number" && Number.isFinite(s.cost) ? String(Math.round(s.cost * 100) / 100) : "?";
		row.append(mark, el("span", { class: "camp-plan-step-name", text: label }), el("span", { class: "camp-plan-step-cost", text: `cost ${cost}` }));
		for (const req of s.requires || []) {
			row.append(el("span", {
				class: "enc-chip" + (req.met ? "" : " is-on"),
				text: `${req.requirement?.label || "precondition"} ${req.met ? "holds" : "broken"}`,
				attrs: { title: req.why || "" },
			}));
		}
		list.append(row);
	}
	card.append(list);

	if (isDM()) {
		const controls = el("div", { class: "camp-fact-chips" });
		const status = el("select", { class: "enc-field camp-plan-status", attrs: { "aria-label": `Status of ${p.name}`, "data-plan-status": p.id } });
		for (const s of ["dormant", "active", "stalled", "complete", "abandoned"]) {
			const opt = el("option", { text: s });
			opt.value = s;
			if (s === p.status) opt.selected = true;
			status.append(opt);
		}
		controls.append(status);
		if (p.status === "dormant" || p.status === "stalled") {
			controls.append(el("button", { class: "enc-chip is-on", text: "activate", attrs: { type: "button", "data-plan-activate": p.id } }));
		}
		for (const to of p.next_states || []) {
			controls.append(el("button", { class: "enc-chip", text: `→ ${to}`, attrs: { type: "button", "data-plan-move": p.id, "data-move-to": to } }));
		}
		if (controls.children.length > 0) card.append(controls);
	}
	return card;
}

// planCreateForm builds the DM's plan authoring form: name, rate, the
// initial state, and one line per step — "state cost [name]" — from which
// the linear machine is derived. Branching machines are an API affordance.
function planCreateForm() {
	const form = el("form", { class: "camp-plan-form", attrs: { id: "camp-plan-form" } });
	form.append(el("h4", { class: "camp-sheet-heading", text: "Author a plan" }));
	form.append(
		el("input", { class: "enc-field camp-plan-name", attrs: { type: "text", required: true, placeholder: "The Root Takes Hold", "aria-label": "Plan name" } }),
		el("input", { class: "enc-field camp-plan-rate", attrs: { type: "number", step: "0.5", min: "0", value: "1", "aria-label": "Progress per day" } }),
		el("input", { class: "enc-field camp-plan-initial", attrs: { type: "text", required: true, placeholder: "mustering", "aria-label": "Initial state" } }),
		el("textarea", {
			class: "enc-field camp-plan-steps",
			attrs: { rows: "3", placeholder: "infiltrated 10 Infiltrate the mines\nbloomed 10 Bloom beneath Blackwater", "aria-label": "Steps — one per line: state cost [name]" },
		}),
		el("button", { class: "enc-btn", text: "Set the plan", attrs: { type: "submit" } }),
	);
	return form;
}

async function onFactionPlanSubmit(e) {
	const form = e.target.closest("#camp-plan-form");
	if (!form) return;
	e.preventDefault();
	if (!current || !selected) return;
	const name = form.querySelector(".camp-plan-name").value.trim();
	const initial = form.querySelector(".camp-plan-initial").value.trim();
	const rate = parseFloat(form.querySelector(".camp-plan-rate").value) || 1;
	const steps = [];
	for (const line of form.querySelector(".camp-plan-steps").value.split("\n")) {
		const parts = line.trim().split(/\s+/);
		if (parts.length < 2 || parts[0] === "") continue;
		const state = parts[0];
		const cost = parseFloat(parts[1]);
		if (!Number.isFinite(cost) || cost <= 0) continue;
		steps.push({ state, cost, name: parts.slice(2).join(" ") });
	}
	if (!name || !initial || steps.length === 0) {
		renderMeta("A plan needs a name, an initial state and at least one 'state cost' line.", true);
		return;
	}
	// The machine is the linear chain of the steps: initial -> s1 -> s2...
	const states = [initial, ...steps.map((s) => s.state)];
	const edges = [];
	for (let i = 0; i < states.length - 1; i++) edges.push({ from: states[i], to: states[i + 1] });
	try {
		await api.campaignFactionPlanCreate(current.id, selected.id, {
			name,
			rate_per_day: rate,
			state_machine: { initial, states, edges },
			steps,
		});
		renderMeta("The plan is set.");
		await refreshFactionDossier();
	} catch (err) {
		renderMeta(err.message, true);
	}
}

async function onFactionPlanStatusChange(e) {
	const sel = e.target.closest("[data-plan-status]");
	if (!sel) return;
	try {
		await api.campaignFactionPlanUpdate(current.id, sel.dataset.planStatus, { status: sel.value });
		renderMeta("Plan status set.");
		await refreshFactionDossier();
	} catch (err) {
		renderMeta(err.message, true);
	}
}

/* ---------- the rumour mill (MAD-374) ---------- */

// The mill renders whatever the server sent at the caller's scope: the
// statement, the spread, and who repeats it. The truth column never
// arrives below the DM — the server does not send it — so the DM's "true /
// false / distorted" chip is rendered only when the field came back.
async function loadRumors(params) {
	const board = $("camp-rumors");
	if (!current) return;
	populateRumorSubjects();
	let data;
	try {
		data = await api.campaignRumors(current.id, params);
	} catch (err) {
		clear(board).append(el("p", { class: "camp-status warn", text: err.message }));
		return;
	}
	rumors = data.rumors || [];
	renderRumors();
}

// populateRumorSubjects fills the about selects from the loaded entity
// list — the same list the fact form draws from.
function populateRumorSubjects() {
	const options = entities.map((e) => ({ value: e.id, text: `${e.name} (${e.kind})` }));
	for (const id of ["camp-rumor-about", "camp-rumor-gen-about"]) {
		const select = $(id);
		const prev = select.value;
		clear(select);
		if (id === "camp-rumor-about") select.append(el("option", { text: "nothing in particular", attrs: { value: "" } }));
		for (const o of options) select.append(el("option", { text: o.text, attrs: { value: o.value } }));
		if ([...select.options].some((op) => op.value === prev)) select.value = prev;
	}
}

function renderRumors() {
	const board = clear($("camp-rumors"));
	if (rumors.length === 0) {
		board.append(el("p", { class: "camp-status", text: "Nothing is circulating yet." }));
		return;
	}
	const nameOf = (id) => entities.find((e) => e.id === id)?.name || id.slice(0, 8);
	for (const r of rumors) {
		const row = el("div", { class: "camp-fact", attrs: { "data-rid": r.id } });
		row.append(el("p", { class: "camp-fact-statement prose", text: `“${r.statement}”` }));
		const chips = el("div", { class: "camp-fact-chips" });
		if (r.truth) {
			// DM eyes only: the server never sends truth below the DM.
			const truthChip = el("span", { class: "enc-chip" + (r.truth === "true" ? " is-on" : ""), text: `truth: ${r.truth}` });
			chips.append(truthChip);
		}
		chips.append(el("span", { class: "enc-chip", text: r.spread || "local" }));
		if (r.status && r.status !== "circulating") chips.append(el("span", { class: "enc-chip", text: r.status }));
		if (r.about && r.about.name) {
			chips.append(el("button", {
				class: "enc-chip", text: `about ${r.about.name}`,
				attrs: { type: "button", "data-open-eid": r.about.id },
			}));
		}
		row.append(chips);
		// Who is repeating it, in their own words.
		if ((r.holders || []).length) {
			const holds = el("p", { class: "camp-aliases" });
			holds.textContent = "Said by " + r.holders.map((h) => h.entity === "party" ? "the party" : (nameOf(h.entity) + (h.variant ? ` — “${h.variant}”` : ""))).join("; ");
			row.append(holds);
		}
		if (isDM()) {
			const actions = el("div", { class: "camp-fact-chips" });
			actions.append(el("button", { class: "enc-chip", text: "the party heard it", attrs: { type: "button", "data-heard": r.id, title: "Write the stance the rumour earns for the party" } }));
			actions.append(el("button", { class: "enc-chip", text: "give it a voice", attrs: { type: "button", "data-hold": r.id, title: "Record an NPC repeating it" } }));
			if (r.status !== "debunked") actions.append(el("button", { class: "enc-chip", text: "debunk", attrs: { type: "button", "data-status": r.id, "data-to": "debunked" } }));
			if (r.status !== "confirmed") actions.append(el("button", { class: "enc-chip", text: "confirm", attrs: { type: "button", "data-status": r.id, "data-to": "confirmed" } }));
			actions.append(el("button", { class: "enc-chip", text: "forget", attrs: { type: "button", "data-forget": r.id } }));
			row.append(actions);
		}
		board.append(row);
	}
}

async function onRumorSubmit(e) {
	if (e.target.id !== "camp-rumor-form") return;
	e.preventDefault();
	if (!current || !isDM()) return;
	const statement = $("camp-rumor-statement").value.trim();
	if (!statement) return;
	const body = {
		statement,
		truth: $("camp-rumor-truth").value,
		about: $("camp-rumor-about").value,
		spread: $("camp-rumor-spread").value,
	};
	renderMeta("Setting it circulating…");
	try {
		await api.campaignRumorCreate(current.id, body);
	} catch (err) {
		renderMeta(err.message, true);
		return;
	}
	$("camp-rumor-statement").value = "";
	renderMeta("The rumour is in the mill.");
	await loadRumors();
}

async function onRumorGenerate(e) {
	if (e.target.id !== "camp-rumor-generate-form") return;
	e.preventDefault();
	if (!current || !isDM()) return;
	const about = $("camp-rumor-gen-about").value;
	if (!about) return;
	const n = (id) => parseInt($(id).value, 10) || 0;
	renderMeta("Drawing the rumours — the truth mix and the holders are computed; the model words them…");
	try {
		await api.campaignRumorGenerate(current.id, {
			about,
			true: n("camp-rumor-gen-true"),
			false: n("camp-rumor-gen-false"),
			distorted: n("camp-rumor-gen-distorted"),
		});
	} catch (err) {
		renderMeta(err.message, true);
		return;
	}
	renderMeta("The rumours are staged — decide the batch on the review queue.");
	await reviewFor(current.id);
}

async function onRumorBoardClick(e) {
	if (!current || !isDM()) return;
	const rid = e.target.closest("[data-rid]")?.dataset.rid;
	if (!rid) return;
	if (e.target.closest("[data-heard]")) {
		renderMeta("The party hears the rumour…");
		try {
			const res = await api.campaignRumorHeard(current.id, rid, "party");
			renderMeta(res.heard.outcome === "granted"
				? `Recorded: the party now ${res.heard.stance === "suspects" ? "suspects" : "is wrong about"} the fact behind it.`
				: res.heard.outcome === "knows_already" ? "The party already knows the fact — gossip changes nothing."
				: res.heard.outcome === "carried" ? "The party carries the rumour — nothing in the graph for it to touch yet."
				: "The party already held exactly that.");
		} catch (err) {
			renderMeta(err.message, true);
			return;
		}
		await loadRumors();
		return;
	}
	if (e.target.closest("[data-hold]")) {
		const voice = prompt("Who repeats it? Type an NPC's name:", "");
		if (!voice) return;
		const match = entities.find((x) => x.name.toLowerCase() === voice.trim().toLowerCase());
		if (!match) { renderMeta(`Nobody called ${voice} in the campaign.`, true); return; }
		const variant = prompt(`What exactly does ${match.name} say?`, "") || "";
		try {
			await api.campaignRumorHolderSet(current.id, rid, { entity: match.id, variant });
		} catch (err) {
			renderMeta(err.message, true);
			return;
		}
		await loadRumors();
		return;
	}
	const statusBtn = e.target.closest("[data-status]");
	if (statusBtn) {
		try {
			await api.campaignRumorUpdate(current.id, statusBtn.dataset.status, { status: statusBtn.dataset.to });
		} catch (err) {
			renderMeta(err.message, true);
			return;
		}
		await loadRumors();
		return;
	}
	if (e.target.closest("[data-forget]")) {
		if (!confirm("Take this rumour out of the mill?")) return;
		try {
			await api.campaignRumorDelete(current.id, rid);
		} catch (err) {
			renderMeta(err.message, true);
			return;
		}
		await loadRumors();
	}
}

async function onRumorBoardChange(e) {
	// Reserved for per-row selects; nothing yet.
	void e;
}

// loadSheetRumors fills the sheet's "what people are saying" section for
// one entity — the statements circulating about them, at the caller's
// scope. The server sends quotes only; the mill panel carries the rest.
async function loadSheetRumors(eid) {
	let data;
	try {
		data = await api.campaignRumors(current.id, { about: eid, status: "circulating" });
	} catch (err) {
		const box = document.querySelector(`[data-rumors-for="${eid}"]`);
		if (box) clear(box).append(el("p", { class: "camp-status warn", text: err.message }));
		return;
	}
	const box = document.querySelector(`[data-rumors-for="${eid}"]`);
	if (!box || !selected || selected.id !== eid) return; // the sheet moved on
	clear(box);
	const list = el("div", { class: "camp-facts" });
	for (const r of data.rumors || []) {
		const row = el("div", { class: "camp-fact" });
		row.append(el("p", { class: "camp-fact-statement prose", text: `“${r.statement}”` }));
		if ((r.holders || []).length) {
			const said = el("p", { class: "camp-aliases" });
			said.textContent = "Said by " + r.holders.map((h) => h.entity === "party" ? "the party" : (h.name || h.entity)).join(", ");
			row.append(said);
		}
		list.append(row);
	}
	box.append(list.children.length
		? list
		: el("p", { class: "camp-status", text: "Nothing yet — the mill is quiet about them." }));
}

/* ---------- the location page (MAD-370) ---------- */

// The dossier is a read, not a write: present NPCs, children, items,
// secrets, events and sited quests arrive as live graph rows the server
// assembled at the caller's scope. The only editable thing is the place
// block; the read-aloud description belongs in the entity summary, where
// campaign search reads it. This module renders whatever comes back and
// decides nothing.
let locationDossier = null; // the last dossier body for the selected location

async function loadLocationDossier(eid) {
	const mount = document.getElementById("camp-location-body");
	if (!mount || !current || !selected || selected.id !== eid) return;
	let data;
	try {
		data = await api.campaignLocation(current.id, eid);
	} catch (err) {
		clear(mount).append(el("p", { class: "camp-status warn", text: err.message }));
		return;
	}
	if (!selected || selected.id !== eid) return; // the sheet moved on
	locationDossier = data.location;
	renderLocationDossier(clear(mount));
}

async function refreshLocationDossier() {
	if (selected && selected.kind === "location") await loadLocationDossier(selected.id);
}

// entityChip renders one read-only chip that links into the entity browser.
function entityChip(e, label) {
	return el("button", {
		class: "enc-chip",
		text: label || e.name,
		attrs: { type: "button", "data-open-eid": e.id, title: `${e.kind}${e.status && e.status !== "active" ? " · " + e.status : ""}` },
	});
}

function renderLocationDossier(mount) {
	const d = locationDossier;
	if (!d) return;
	const wrap = el("div", { class: "camp-location-body" });
	const p = d.place || {};

	// The place block: the DM edits it, a player reads its public half.
	if (isDM()) {
		wrap.append(section("The place", placeForm(p)));
	} else {
		const face = el("div", { class: "camp-faction-face" });
		const line = (label, v) => { if (v) face.append(el("p", { class: "prose", text: `${label}: ${v}` })); };
		line("Kind", p.kind);
		line("Scale", p.scale);
		line("Population", p.population);
		line("Government", p.government);
		line("Defences", p.defences);
		line("State", p.state);
		if ((p.services || []).length) face.append(el("p", { class: "prose", text: `Services: ${p.services.join(", ")}` }));
		if ((p.senses || []).length) face.append(el("p", { class: "prose", text: `You notice ${p.senses.join("; ")}` }));
		const chips = el("div", { class: "camp-fact-chips" });
		if (p.climate) chips.append(el("span", { class: "enc-chip", text: p.climate }));
		if (p.danger) chips.append(el("span", { class: "enc-chip is-on", text: `danger ${p.danger}` }));
		if (chips.children.length) face.append(chips);
		if (face.children.length > 0) wrap.append(section("The place", face));
	}

	// Everything else is the graph, chipped read-only.
	const nameOf = (id) => entities.find((x) => x.id === id)?.name || id.slice(0, 8);
	const chipRow = (list, labelFn) => {
		const chips = el("div", { class: "camp-fact-chips" });
		for (const item of list) chips.append(entityChip(item, labelFn ? labelFn(item) : undefined));
		return chips;
	};
	const groups = [
		["Present", d.present],
		["Within", d.children],
		["Items here", d.items],
	];
	let anyGroup = false;
	const ties = el("div", { class: "camp-faction-ties" });
	for (const [label, list] of groups) {
		if (!list || list.length === 0) continue;
		anyGroup = true;
		ties.append(el("p", { class: "camp-sheet-heading", text: label }), chipRow(list));
	}
	// Routes out: the travel block, read where it is there and rendered
	// nowhere when it is not (a player's payload never carried it).
	if ((d.routes || []).length) {
		anyGroup = true;
		const chips = el("div", { class: "camp-fact-chips" });
		for (const r of d.routes) {
			chips.append(el("button", {
				class: "enc-chip",
				text: `${nameOf(r.to)} — ${r.days} day${r.days === 1 ? "" : "s"}${r.terrain ? ` (${r.terrain})` : ""}`,
				attrs: { type: "button", "data-open-eid": r.to },
			}));
		}
		ties.append(el("p", { class: "camp-sheet-heading", text: "Roads out" }), chips);
	}
	if ((d.quests || []).length) {
		anyGroup = true;
		const chips = el("div", { class: "camp-fact-chips" });
		for (const q of d.quests) {
			chips.append(el("button", {
				class: "enc-chip",
				text: `${q.name}${q.status && q.status !== "active" ? ` (${q.status})` : ""}`,
				attrs: { type: "button", "data-open-eid": q.id, title: q.summary || "" },
			}));
		}
		ties.append(el("p", { class: "camp-sheet-heading", text: "Quests sited here" }), chips);
	}
	if (anyGroup) wrap.append(section("What the graph says", ties));

	// Secrets: fact statements, DM-only by construction.
	if ((d.secrets || []).length) {
		wrap.append(section("Secrets", factsList(d.secrets)));
	}

	// History: the events sited here, in play order.
	if ((d.events || []).length) {
		wrap.append(section("History", eventList(d.events)));
	}

	// What are people saying about this? The statements circulating here
	// — the server loads only what the caller's scope may read; the full
	// mill with holders and (for the DM) truth values lives in the panel.
	if ((d.rumours || []).length) {
		const list = el("ul", { class: "camp-events" });
		for (const statement of d.rumours) {
			list.append(el("li", { class: "camp-event", text: `“${statement}”` }));
		}
		wrap.append(section("Rumours", list));
	} else {
		wrap.append(section("Rumours", el("p", { class: "camp-status", text: "Nobody is saying anything yet." })));
	}

	// The flesh-out path (MAD-372): a name and one line becomes a place —
	// the missing block, people and secrets proposed around what is
	// already here, staged as a batch. Parts re-roll one piece.
	if (isDM()) {
		const form = el("form", { class: "camp-plan-form", attrs: { "data-fleshout": d.id } });
		form.append(el("p", { class: "camp-sheet-heading", text: "Flesh this place out" }));
		form.append(el("input", {
			class: "enc-field",
			attrs: { type: "text", placeholder: "what has changed, or leave blank", "aria-label": "Flesh-out premise" },
		}));
		const parts = el("select", { class: "enc-field", attrs: { "aria-label": "Which part" } });
		for (const [value, label] of [
			["", "Everything with room"],
			["place", "The block"],
			["sublocations", "The sub-locations"],
			["npcs", "The people"],
			["hooks", "The hooks"],
			["secrets", "The secrets"],
		]) {
			parts.append(el("option", { text: label, attrs: { value } }));
		}
		form.append(parts, el("button", { class: "enc-btn", text: "Propose", attrs: { type: "submit" } }));
		wrap.append(section("Design", form));
	}

	mount.append(wrap);
}

// placeForm is the DM's block editor. The description is deliberately not
// here: it belongs in the entity summary, where campaign search reads it.
function placeForm(p) {
	const form = el("form", { class: "camp-plan-form", attrs: { id: "camp-place-form" } });
	const field = (cls, label, value, attrs) => {
		const wrap = el("label", { class: "camp-place-field" });
		wrap.append(el("span", { class: "camp-sheet-heading", text: label }));
		wrap.append(el("input", { class: "enc-field " + cls, attrs: { type: "text", value: value || "", ...attrs } }));
		return wrap;
	};
	form.append(
		field("camp-place-kind", "Kind", p.kind, { placeholder: "town, ruin, dungeon…" }),
		field("camp-place-scale", "Scale", p.scale, { placeholder: "large village" }),
		field("camp-place-population", "Population", p.population, { placeholder: "about 900" }),
		field("camp-place-government", "Government", p.government, { placeholder: "a merchant council" }),
		field("camp-place-defences", "Defences", p.defences, { placeholder: "a palisade" }),
		field("camp-place-climate", "Climate", p.climate, { placeholder: "temperate" }),
		field("camp-place-state", "State", p.state, { placeholder: "flooding after the rains" }),
	);
	const danger = field("camp-place-danger", "Danger (0-5)", p.danger ? String(p.danger) : "", { type: "number", min: "0", max: "5", step: "1" });
	form.append(danger);
	const list = (cls, label, values, placeholder) => {
		const wrap = el("label", { class: "camp-place-field" });
		wrap.append(el("span", { class: "camp-sheet-heading", text: label }));
		wrap.append(el("textarea", { class: "enc-field " + cls, attrs: { rows: "2", placeholder }, }));
		wrap.querySelector("textarea").value = (values || []).join("\n");
		return wrap;
	};
	form.append(
		list("camp-place-services", "Services (one per line)", p.services, "inn\nmarket"),
		list("camp-place-senses", "Sensory notes (one per line)", p.senses, "gull noise\ndamp wool"),
		list("camp-place-truth", "Private truth (DM only)", [p.private_truth], "what is really going on"),
		el("button", { class: "enc-btn", text: "Record the place", attrs: { type: "submit" } }),
	);
	return form;
}

async function onPlaceSubmit(e) {
	const form = e.target.closest("#camp-place-form");
	if (!form) return;
	e.preventDefault();
	if (!current || !selected) return;
	const val = (cls) => form.querySelector("." + cls)?.value.trim() || "";
	const lines = (cls) => form.querySelector("." + cls).value.split("\n").map((l) => l.trim()).filter(Boolean);
	const place = {
		kind: val("camp-place-kind"),
		scale: val("camp-place-scale"),
		population: val("camp-place-population"),
		government: val("camp-place-government"),
		defences: val("camp-place-defences"),
		climate: val("camp-place-climate"),
		state: val("camp-place-state"),
		danger: parseInt(val("camp-place-danger"), 10) || 0,
		services: lines("camp-place-services"),
		senses: lines("camp-place-senses"),
		private_truth: val("camp-place-truth"),
	};
	try {
		await api.campaignPlacePut(current.id, selected.id, place);
		renderMeta("The place is recorded.");
		await refreshLocationDossier();
	} catch (err) {
		renderMeta(err.message, true);
	}
}

/* ---------- the window-manager contract ---------- */

// mount() adopts the surface that already exists in index.html rather than
// building one, which is what made migrating nine surfaces tractable: every
// $("camp-entities") lookup inside this module keeps working untouched. The
// cost is one window per tool; see the note on `instances` in wm/registry.js.
let wired = false;

export const tool = {
	mount(host) {
		const view = $("campaign-view");
		host.append(view);
		view.hidden = false;
		if (!wired) {
			wire();
			wired = true;
		}
		openCampaign();
		return { destroy: closeCampaign };
	},
};
