// The Sessions surface: the campaign session layer in the UI.
//
// The server owns everything that must be right — the verbatim sources, the
// byte-offset spans, the prior-ruling FTS — and this module is mostly honest
// plumbing: paste or upload lands through the same API, the log renders what
// the server stored, and the one piece of local cleverness (showing a
// selection's byte offsets) mirrors exactly what the server will resolve
// later, so what the DM sees selecting text is what a cited span will be.

import { $, el, clear, isNarrow } from "./dom.js";
import { api } from "./api.js";
import { state } from "./state.js";

let campaignID = null;   // the campaign whose sessions are showing
let campaigns = [];      // the picker's data
let sessionID = null;    // the open session, null on the empty state
let session = null;      // its full view

const SOURCE_KINDS = {
	transcript: "transcript",
	dm_notes: "DM notes",
	player_journal: "player journal",
	chat_log: "chat log",
	live_mark: "live mark",
};

const EVENT_KINDS = {
	ruling: "ruling",
	qa: "Q&A",
	note: "note",
	discovery: "discovery",
	encounter: "encounter",
};

export function initSessions() {
	$("rail-sessions").addEventListener("click", openSessions);
	$("sessions-close").addEventListener("click", closeSessions);
	$("sess-campaign").addEventListener("change", onPickCampaign);
	$("sess-new-campaign").addEventListener("click", onNewCampaign);
	$("sess-new-form").addEventListener("submit", onCreateSession);
	$("sess-list").addEventListener("click", onSessionListClick);
	$("sess-source-form").addEventListener("submit", onPasteSource);
	$("sess-source-file").addEventListener("change", onUploadSource);
	$("sess-event-form").addEventListener("submit", onLogEvent);
	$("sess-quick-discovery").addEventListener("click", onQuickDiscovery);
	$("sess-export").addEventListener("click", onExport);
	$("sess-sources").addEventListener("click", onSourceClick);
	$("sess-sources").addEventListener("mouseup", onSourceSelection);
	$("sess-sources").addEventListener("keyup", onSourceSelection);
}

/** Open the sessions surface. */
export async function openSessions() {
	state.sessionsOpen = true;
	if (isNarrow()) $("app").classList.add("rail-hidden");
	$("sessions-view").hidden = false;
	$("main").classList.add("is-sessioning");
	await loadCampaigns();
}

/** Drop back to the transcript. */
export function closeSessions() {
	state.sessionsOpen = false;
	$("sessions-view").hidden = true;
	$("main").classList.remove("is-sessioning");
	$("rail-sessions").focus();
}

/* ---------- campaigns ---------- */

async function loadCampaigns() {
	try {
		const body = await api.listCampaigns();
		campaigns = body.campaigns || [];
	} catch (err) {
		renderCampaignError(err);
		return;
	}
	if (!campaigns.length) {
		campaignID = null;
		renderPicker();
		renderNoCampaigns();
		return;
	}
	if (!campaigns.some((c) => c.id === campaignID)) campaignID = campaigns[0].id;
	renderPicker();
	await loadSessions();
}

function renderPicker() {
	const sel = clear($("sess-campaign"));
	for (const c of campaigns) {
		sel.append(el("option", { text: c.name, attrs: { value: c.id } }));
	}
	sel.value = campaignID || "";
	sel.disabled = !campaigns.length;
}

function renderCampaignError(err) {
	clear($("sess-list"));
	$("sessions-meta").textContent = "";
	$("sess-empty").textContent = `Could not load campaigns — ${err.message}`;
	$("sess-empty").hidden = false;
}

function renderNoCampaigns() {
	clear($("sess-list"));
	hideDetail();
	$("sess-empty").textContent = "No campaigns yet — press + to create one, then record your first session.";
	$("sess-empty").hidden = false;
	$("sessions-meta").textContent = "";
}

function onPickCampaign() {
	campaignID = $("sess-campaign").value || null;
	sessionID = null;
	if (campaignID) loadSessions();
}

function onNewCampaign() {
	const name = window.prompt("Campaign name?");
	if (!name || !name.trim()) return;
	api.createCampaign(name.trim(), "dnd5e")
		.then(() => loadCampaigns())
		.catch((err) => window.alert(err.message));
}

/* ---------- sessions list ---------- */

async function loadSessions() {
	if (!campaignID) return;
	let body;
	try {
		body = await api.listSessions(campaignID);
	} catch (err) {
		renderCampaignError(err);
		return;
	}
	const list = body.sessions || [];
	const wrap = clear($("sess-list"));
	for (const s of list) {
		wrap.append(sessionRow(s));
	}
	if (!list.length) {
		wrap.append(el("p", { class: "sess-empty", text: "No sessions recorded yet." }));
	}
	const active = list.find((s) => s.id === sessionID);
	if (active) {
		renderDetail(active);
	} else {
		sessionID = null;
		hideDetail();
	}
	$("sessions-meta").textContent = `${list.length} session${list.length === 1 ? "" : "s"}`;
}

function sessionRow(s) {
	const when = s.started_at ? new Date(s.started_at).toLocaleDateString() : "";
	return el("button", {
		class: "sess-row" + (s.id === sessionID ? " is-active" : ""),
		attrs: { type: "button", "data-session": s.id, role: "listitem" },
	},
		el("span", { class: "sess-row-ordinal", text: String(s.ordinal).padStart(2, "0") }),
		el("span", { class: "sess-row-main" },
			el("span", { class: "sess-row-name", text: s.name }),
			el("span", { class: "sess-row-sub", text: `${s.status}${when ? " · " + when : ""}` }),
		),
	);
}

async function onSessionListClick(e) {
	const row = e.target.closest("[data-session]");
	if (!row) return;
	sessionID = row.dataset.session;
	await loadSessions();
}

async function onCreateSession(e) {
	e.preventDefault();
	if (!campaignID) return;
	const name = $("sess-new-name").value.trim();
	try {
		const body = await api.createSession(campaignID, name);
		$("sess-new-name").value = "";
		sessionID = body.session.id;
		await loadSessions();
	} catch (err) {
		window.alert(err.message);
	}
}

/* ---------- session detail ---------- */

function hideDetail() {
	session = null;
	$("sess-empty").hidden = false;
	for (const id of ["sess-session-head", "sess-sources-panel", "sess-log-panel", "sess-actions"]) {
		$(id).hidden = true;
	}
}

function renderDetail(s) {
	session = s;
	$("sess-empty").hidden = true;
	$("sess-sources-panel").hidden = false;
	$("sess-log-panel").hidden = false;
	$("sess-actions").hidden = false;

	const head = clear($("sess-session-head"));
	head.hidden = false;
	const statusButtons = el("span", { class: "sess-status-btns" });
	const next = { planned: ["live", "Start"], live: ["done", "End session"] }[s.status];
	if (next) {
		statusButtons.append(el("button", {
			class: "sess-btn",
			text: next[1],
			attrs: { type: "button" },
			on: { click: () => onAdvanceStatus(next[0]) },
		}));
	}
	head.append(
		el("h3", { class: "sess-session-title", text: `${s.ordinal}. ${s.name}` }),
		el("p", { class: "sess-session-sub", text: describeStatus(s) }),
		statusButtons,
	);
	loadSources();
	loadEvents();
}

function describeStatus(s) {
	const started = s.started_at ? new Date(s.started_at).toLocaleString() : "";
	const ended = s.ended_at ? new Date(s.ended_at).toLocaleString() : "";
	if (s.status === "planned") return "Planned — not yet played.";
	if (s.status === "live") return `Live${started ? " · started " + started : ""}.`;
	return `Done${started ? " · " + started : ""}${ended ? " → " + new Date(s.ended_at).toLocaleTimeString() : ""}.`;
}

async function onAdvanceStatus(status) {
	if (!campaignID || !sessionID) return;
	try {
		await api.updateSession(campaignID, sessionID, { status });
		await loadSessions();
	} catch (err) {
		window.alert(err.message);
	}
}

/* ---------- sources ---------- */

async function loadSources() {
	let body;
	try {
		body = await api.listSources(campaignID, sessionID);
	} catch (err) {
		$("sess-source-hint").textContent = err.message;
		return;
	}
	const wrap = clear($("sess-sources"));
	const sources = body.sources || [];
	for (const src of sources) {
		wrap.append(sourceCard(src));
	}
	if (!sources.length) {
		wrap.append(el("p", { class: "sess-empty", text: "No sources yet — paste a transcript or upload .txt/.md/.srt/.vtt." }));
	}
	$("sess-source-hint").textContent = "Stored verbatim and immutable — spans cite these bytes.";
}

function sourceCard(src) {
	const kind = SOURCE_KINDS[src.kind] || src.kind;
	const meta = [
		src.author || "unknown author",
		`${src.byte_size.toLocaleString()} bytes`,
		src.timed ? "timed" : "",
	].filter(Boolean).join(" · ");
	return el("details", { class: "sess-source", attrs: { "data-source": src.id } },
		el("summary", {},
			el("span", { class: "sess-source-kind", text: kind }),
			el("span", { class: "sess-source-title", text: src.title || "untitled" }),
			el("span", { class: "sess-source-meta", text: meta }),
		),
		el("div", { class: "sess-source-body" }),
		el("span", { class: "sess-span-chip", attrs: { hidden: true } }),
	);
}

async function onSourceClick(e) {
	const card = e.target.closest("details[data-source]");
	if (!card || !card.open) return;
	const bodyEl = card.querySelector(".sess-source-body");
	if (bodyEl.dataset.loaded) return;
	bodyEl.dataset.loaded = "1";
	try {
		const body = await api.getSource(campaignID, sessionID, card.dataset.source);
		const src = body.source;
		bodyEl.append(el("pre", { class: "sess-source-content", text: src.content }));
		if (src.timed && body.timing && body.timing.length) {
			bodyEl.append(el("p", {
				class: "sess-source-timing",
				text: `${body.timing.length} cues · ${(body.timing[0].start_ms / 60000).toFixed(1)}–${(body.timing[body.timing.length - 1].end_ms / 60000).toFixed(1)} min`,
			}));
		}
		card.dataset.content = src.content;
	} catch (err) {
		bodyEl.append(el("p", { class: "sess-empty", text: err.message }));
	}
}

// onSourceSelection shows the addressable span of whatever the DM selected in
// an open source: the byte offsets a downstream fact would cite, computed the
// way the server computes them.
function onSourceSelection() {
	const sel = window.getSelection();
	const card = sel && sel.anchorNode ? sel.anchorNode.parentElement?.closest("details[data-source]") : null;
	const chip = card && card.querySelector(".sess-span-chip");
	if (!card || !chip || !card.dataset.content) return;
	const text = sel.toString();
	if (!text) {
		chip.hidden = true;
		return;
	}
	const content = card.dataset.content;
	const start = content.indexOf(text);
	if (start < 0) {
		chip.hidden = true;
		return;
	}
	chip.hidden = false;
	chip.textContent = `span ${start}–${start + text.length} · ${text.length} bytes`;
}

async function onPasteSource(e) {
	e.preventDefault();
	const content = $("sess-source-content").value;
	if (!content.trim() || !campaignID || !sessionID) return;
	try {
		await api.addSource(campaignID, sessionID, {
			kind: $("sess-source-kind").value,
			author: $("sess-source-author").value.trim(),
			content,
		});
		$("sess-source-content").value = "";
		await loadSources();
	} catch (err) {
		$("sess-source-hint").textContent = err.message;
	}
}

async function onUploadSource() {
	const file = $("sess-source-file").files[0];
	$("sess-source-file").value = "";
	if (!file || !campaignID || !sessionID) return;
	try {
		await api.uploadSource(campaignID, sessionID, file, $("sess-source-kind").value);
		await loadSources();
	} catch (err) {
		$("sess-source-hint").textContent = err.message;
	}
}

/* ---------- the log ---------- */

async function loadEvents() {
	let body;
	try {
		body = await api.listEvents(campaignID, sessionID);
	} catch (err) {
		clear($("sess-log")).append(el("p", { class: "sess-empty", text: err.message }));
		return;
	}
	const wrap = clear($("sess-log"));
	const events = body.events || [];
	for (const ev of events) wrap.append(eventRow(ev));
	if (!events.length) {
		wrap.append(el("p", { class: "sess-empty", text: "Nothing logged yet." }));
	}
}

function eventRow(ev) {
	const extras = ev.payload && Object.keys(ev.payload).length
		? el("ul", { class: "sess-event-payload" },
			...Object.entries(ev.payload).map(([k, v]) => el("li", { text: `${k}: ${typeof v === "object" ? JSON.stringify(v) : v}` })))
		: null;
	const at = ev.created_at ? new Date(ev.created_at).toLocaleTimeString() : "";
	return el("article", { class: "sess-event" },
		el("header", { class: "sess-event-head" },
			el("span", { class: "sess-event-kind", attrs: { "data-kind": ev.kind }, text: EVENT_KINDS[ev.kind] || ev.kind }),
			el("span", { class: "sess-event-summary", text: ev.summary || "" }),
			el("time", { class: "sess-event-time", text: at }),
		),
		ev.detail ? el("p", { class: "sess-event-detail", text: ev.detail }) : null,
		extras,
	);
}

async function onLogEvent(e) {
	e.preventDefault();
	if (!campaignID || !sessionID) return;
	const kind = $("sess-event-kind").value;
	const summary = $("sess-event-summary").value.trim();
	const detail = $("sess-event-detail").value.trim();
	try {
		const body = await api.addEvent(campaignID, sessionID, { kind, summary, detail });
		$("sess-event-summary").value = "";
		$("sess-event-detail").value = "";
		renderPrior(body.prior_matches);
		await loadEvents();
	} catch (err) {
		window.alert(err.message);
	}
}

// onQuickDiscovery is the in-play button: one prompt, one discovery event,
// hands kept on the table.
async function onQuickDiscovery() {
	if (!campaignID || !sessionID) return;
	const what = window.prompt("What did they discover?");
	if (!what || !what.trim()) return;
	try {
		await api.addEvent(campaignID, sessionID, { kind: "discovery", summary: what.trim() });
		await loadEvents();
	} catch (err) {
		window.alert(err.message);
	}
}

// renderPrior shows the MAD-286 surfacing: the campaign's own earlier
// rulings that match what was just asked. "You ruled the other way on this
// three sessions ago."
function renderPrior(matches) {
	const wrap = $("sess-prior");
	clear(wrap);
	if (!matches || !matches.length) {
		wrap.hidden = true;
		return;
	}
	wrap.hidden = false;
	wrap.append(el("p", { class: "sess-prior-head", text: "Prior rulings on this:" }));
	for (const m of matches.slice(0, 5)) {
		wrap.append(el("div", {
			class: "sess-prior-match",
			attrs: { title: `Session ${m.session_ordinal} · ${m.at || ""}` },
		},
			el("span", { class: "sess-prior-ordinal", text: `S${m.session_ordinal}` }),
			el("span", { class: "sess-prior-text" },
				el("span", { class: "sess-prior-q", text: m.summary }),
				m.detail ? el("span", { class: "sess-prior-a", text: ` — ${m.detail}` }) : null,
			),
		));
	}
}

function onExport() {
	if (!campaignID || !sessionID) return;
	window.location.assign(api.sessionExportURL(campaignID, sessionID));
}
