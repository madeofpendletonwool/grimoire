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
	// The quest board (MAD-369): the list, the machine chart and the DM's
	// move control, all delegated so re-renders need no re-binds.
	$("camp-quests").addEventListener("click", onQuestBoardClick);
	$("camp-quest-detail").addEventListener("submit", onQuestMoveSubmit);
	$("camp-quest-detail").addEventListener("click", onQuestDetailClick);
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
	editingScheduleID = null;
	selectedQuestID = null;
	await loadEntities();
	loadClock();
	loadSchedule();
	loadQuests();
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

/* ---------- the quest board (MAD-369) ---------- */

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
		box.append(el("button", {
			class: "enc-chip", text: "abandon the quest",
			attrs: { type: "button", "data-abandon": q.id },
		}));
	}
}

async function onQuestMoveSubmit(e) {
	e.preventDefault();
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

	// Rumours circulate once MAD-374 lands; the shape ships now.
	wrap.append(section("Rumours", el("p", { class: "camp-status", text: "None circulating yet." })));

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
