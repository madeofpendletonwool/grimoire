// The screen strip (MAD-485, stage 2 of MAD-318): the thin column that
// turns the window layout into play mode. The scene, the clock, the
// notes, and the Ask Grimoire mount — everything the tracker and the
// board do not already carry, and nothing they do. One read serves it
// (the live context), one existing write path feeds it (session events,
// kind note), and the layout it belongs in is one Alt+2 away.
//
// The design constraint from MAD-318, verbatim: every panel readable at
// arm's length in a dim room. Big clock, big scene name, big chips —
// high contrast, few words, no dashboards.
//
// DM-only by construction: the live-context read refuses every other
// scope, and a player opening this tool sees the screen's empty state.

import { $, el, clear, debounce } from "./dom.js";
import { api } from "./api.js";
import { openEntity } from "./drawer.js";
import {
	currentScene, sceneMeta, castChips, elapsedLabel, noteLines,
	actingCombatant, capturePayload, entityRefs,
} from "./screenvm.js";

let campaigns = [];
let campaignID = null;
let wired = false;
let mounted = false;

// The last live-context read: {session, scenes} or null.
let live = null;
// The campaign's entities, for names the chips and setting line spell.
let entities = [];
// Sessions for the go-live picker, fetched only while none is live.
let pickable = null;
// Notes against the live session, newest first.
let notes = [];
// A scene the DM tapped to read, overriding the seated one.
let pickedSceneID = "";

// The capture form's own context, read fresh when it opens: the battle
// (and its acting combatant) at the moment of capture.
let capBattle = null;

let tickTimer = null;
let pollTimer = null;

const POLL_MS = 20000;

/* ---------- campaigns and boot ---------- */

async function loadCampaigns() {
	let data;
	try {
		data = await api.listCampaigns();
	} catch (err) {
		$("scr-meta").textContent = err.message;
		return;
	}
	campaigns = (data.campaigns || []).filter((c) => c.my_role === "dm" || c.my_role === "keeper");
	const sel = clear($("scr-campaign"));
	if (!campaigns.length) {
		sel.append(el("option", { text: "No campaigns yet…", attrs: { value: "" } }));
		$("scr-empty-title").textContent = "No campaign yet";
		$("scr-empty-text").textContent = "The screen belongs to a table. Found a campaign first, or take a seat with an invite.";
		$("scr-body").hidden = true;
		$("scr-empty").hidden = false;
		return;
	}
	$("scr-empty").hidden = true;
	$("scr-body").hidden = false;
	for (const c of campaigns) sel.append(el("option", { text: c.name, attrs: { value: c.id } }));
	const stored = localStorage.getItem("grimoire-screen-campaign");
	if (campaigns.some((c) => c.id === stored)) campaignID = stored;
	else if (!campaigns.some((c) => c.id === campaignID)) campaignID = campaigns[0].id;
	sel.value = campaignID;
	await loadAll();
}

async function loadAll() {
	pickedSceneID = "";
	entities = [];
	pickable = null;
	notes = [];
	capBattle = null;
	closeCapture();
	await loadLive();
}

/* ---------- the live context ---------- */

async function loadLive() {
	if (!campaignID) return;
	let body;
	try {
		body = await api.campaignLive(campaignID);
	} catch (err) {
		if (err.message && /may do that|not available/.test(err.message)) {
			$("scr-empty-title").textContent = "The DM's seat";
			$("scr-empty-text").textContent = "The screen is the DM's material — this view needs the DM's seat.";
			$("scr-meta").textContent = "";
			$("scr-body").hidden = true;
			$("scr-empty").hidden = false;
			return;
		}
		$("scr-meta").textContent = err.message;
		return;
	}
	live = body.live || { session: null, scenes: [] };
	$("scr-meta").textContent = "";
	$("scr-body").hidden = false;

	// Names for the chips and the setting line; the live read carries ids.
	if (!entities.length) {
		try {
			const ents = await api.campaignEntities(campaignID);
			entities = ents.entities || [];
		} catch (_) { /* chips fall back to short ids */ }
	}
	// The go-live picker's stock, refreshed whenever the table waits —
	// a session that ended elsewhere must not still be pickable.
	if (!live.session) {
		try {
			const list = await api.listSessions(campaignID);
			pickable = (list.sessions || []).filter((ses) => ses.status !== "done");
		} catch (_) {
			pickable = [];
		}
	}
	// The parking lot rides the live session's own log.
	if (live.session) {
		try {
			const log = await api.listEvents(campaignID, live.session.id);
			notes = noteLines(log.events || []);
		} catch (_) { /* the pad still writes; the list catches up */ }
	} else {
		notes = [];
	}
	render();
}

/* ---------- painting ---------- */

function entityName(id) {
	const e = entities.find((x) => x.id === id);
	return e ? e.name : "";
}

function render() {
	if (!mounted || !live) return;
	renderClock();
	renderScene();
	renderNotes();
	tick();
}

function renderClock() {
	const clock = $("scr-clock");
	const golive = $("scr-golive");
	const ses = live.session;
	if (ses) {
		clock.hidden = false;
		golive.hidden = true;
		$("scr-session").textContent = ses.name || `Session ${ses.ordinal}`;
		return;
	}
	clock.hidden = true;
	golive.hidden = false;
	const sel = clear($("scr-golive-session"));
	const stock = pickable || [];
	if (!stock.length) {
		sel.append(el("option", { text: "No session to run — create one in Sessions", attrs: { value: "" } }));
		return;
	}
	for (const s of stock) {
		sel.append(el("option", {
			text: `${s.ordinal}. ${s.name} (${s.status})`,
			attrs: { value: s.id },
		}));
	}
}

function renderScene() {
	const card = $("scr-scene-card");
	const empty = $("scr-scene-empty");
	const scene = currentScene(live.scenes || [], live.session && live.session.id, pickedSceneID);
	if (!scene) {
		card.hidden = true;
		empty.hidden = false;
		clear($("scr-scene-tabs"));
		clear($("scr-cast")).append(el("p", { class: "scr-hint", text: "Whoever the scene brings." }));
		return;
	}
	card.hidden = false;
	empty.hidden = true;
	clear(card).append(
		el("header", { class: "scr-scene-head" },
			el("span", { class: `scr-scene-kind kind-${scene.kind}`, text: scene.kind }),
			el("b", { class: "scr-scene-name", text: scene.name }),
		),
		scene.purpose ? el("p", { class: "scr-scene-purpose", text: scene.purpose }) : null,
		el("p", { class: "scr-scene-meta", text: sceneMeta(scene, entityName(scene.setting_entity)) }),
	);

	// Other live scenes stay one tap away — the DM reads, the Planner writes.
	const others = (live.scenes || []).filter((sc) => sc.id !== scene.id);
	const tabs = clear($("scr-scene-tabs"));
	if (others.length) {
		tabs.append(el("span", { class: "scr-scene-tabs-label", text: "also active" }));
		for (const other of others) {
			tabs.append(el("button", {
				class: "scr-scene-tab", text: other.name,
				attrs: { type: "button", title: "Show this scene" },
				on: { click: () => { pickedSceneID = other.id; renderScene(); } },
			}));
		}
	}

	const host = clear($("scr-cast"));
	const chips = castChips(scene, entities);
	for (const c of chips) host.append(chipEl(c));
	if (!chips.length) host.append(el("p", { class: "scr-hint", text: "No one is cast in this scene." }));
}

function chipEl(c) {
	const chip = el("button", {
		class: "scr-chip" + (c.role === "focus" ? " is-focus" : ""),
		attrs: { type: "button", title: `${c.role}${c.kind ? ` · ${c.kind}` : ""} — open in the drawer` },
	},
		el("b", { class: "scr-chip-name", text: c.name }),
		el("i", { class: "scr-chip-role", text: c.role }),
	);
	chip.addEventListener("click", () => peekEntity(c.entity_id, c.name));
	return chip;
}

async function peekEntity(entityID, fallbackName) {
	if (!campaignID) return;
	try {
		const body = await api.campaignEntity(campaignID, entityID);
		const e = body.entity || {};
		const lines = [e.summary, ...((body.facts || []).map((f) => f.statement))].filter(Boolean);
		openEntity({
			kind: e.kind || "entity",
			name: e.name || fallbackName,
			body: lines.join("\n\n") || "Nothing recorded yet.",
		}, "dnd");
	} catch (err) {
		window.alert(err.message);
	}
}

function renderNotes() {
	const list = clear($("scr-note-list"));
	const input = $("scr-note-input");
	const liveNow = !!(live && live.session);
	input.disabled = !liveNow;
	$("scr-note-park").disabled = !liveNow;
	$("scr-note-note").textContent = liveNow ? "" : "Notes park against the live session — go live first.";
	for (const n of notes) {
		list.append(el("li", { class: "scr-note" },
			el("span", { class: "scr-note-text", text: n.text }),
			n.at ? el("time", { class: "scr-note-time", text: new Date(n.at).toLocaleTimeString() }) : null,
		));
	}
	if (!notes.length && liveNow) {
		list.append(el("li", { class: "scr-note is-empty", text: "Nothing parked yet." }));
	}
	renderCapture();
}

/* ---------- in-play capture (MAD-483) ---------- */

// The capture form: one tap, one line, done — the context rides in the
// payload without the DM typing it. A ruling shows the table's own prior
// rulings while the question is still being typed; a discovery can
// propose a fact into the review queue on the way out.
function renderCapture() {
	const liveNow = !!(live && live.session);
	for (const id of ["scr-disc-btn", "scr-rule-btn"]) $(id).disabled = !liveNow;
	if (!liveNow) closeCapture();
	const hint = $("scr-capture-hint");
	hint.hidden = liveNow;
	if (!liveNow) hint.textContent = "Capture logs against the live session — go live first.";
}

async function openCapture(kind) {
	if (!campaignID || !live || !live.session) return;
	$("scr-capture-hint").hidden = true;
	const form = $("scr-cap-form");
	form.hidden = false;
	$("scr-cap-kind").textContent = kind === "ruling" ? "ruling" : "discovery";
	$("scr-cap-summary").value = "";
	$("scr-cap-detail").value = "";
	$("scr-cap-note").textContent = "";
	$("scr-cap-prior").hidden = true;
	clear($("scr-cap-prior"));
	$("scr-cap-propose-label").hidden = kind !== "discovery";
	$("scr-cap-propose").checked = false;
	$("scr-cap-fact").hidden = true;
	// The fight is read fresh at open: the acting combatant is the capture
	// context, and turns move faster than the strip's poll. Names for the
	// links get one more chance if the boot read never landed.
	capBattle = null;
	try {
		capBattle = await api.combatActive(campaignID);
	} catch (_) { /* no battle, no context block */ }
	if (!entities.length) {
		try {
			const ents = await api.campaignEntities(campaignID);
			entities = ents.entities || [];
		} catch (_) { /* links fall back to short ids */ }
	}
	renderCaptureLinks();
	$("scr-cap-summary").focus();
}

function closeCapture() {
	const form = $("scr-cap-form");
	if (form.hidden) return;
	form.hidden = true;
	capBattle = null;
	clear($("scr-cap-links"));
	$("scr-cap-prior").hidden = true;
	clear($("scr-cap-prior"));
	$("scr-cap-note").textContent = "";
}

// The context line: the scene, the fight's acting combatant, and the
// entity refs the event will link — spelled so the DM sees what rides.
function renderCaptureLinks() {
	const host = clear($("scr-cap-links"));
	const scene = currentScene(live.scenes || [], live.session && live.session.id, pickedSceneID);
	const acting = capBattle ? actingCombatant(capBattle.order || []) : null;
	const chips = [];
	if (scene) {
		chips.push(el("span", { class: "scr-cap-link", title: "The live scene", text: `⌖ ${scene.name}` }));
	}
	if (capBattle && capBattle.combat && acting) {
		chips.push(el("span", {
			class: "scr-cap-link is-combat", title: "The acting combatant", text: `⚔ ${acting.name}`,
		}));
	}
	for (const ref of entityRefs(scene, acting, entities)) {
		chips.push(el("span", {
			class: "scr-cap-link" + (ref.source === "focus" ? " is-focus" : ""),
			title: `Entity link · ${ref.source}`,
			text: ref.name,
		}));
	}
	host.append(...chips);
	fillSubjectSelect(scene, acting);
}

// The subject picker for "also propose as fact": the linked refs first,
// then every campaign entity, so an unlinked subject is a choice, not a
// dead end.
function fillSubjectSelect(scene, acting) {
	const sel = clear($("scr-cap-subject"));
	const refs = entityRefs(scene, acting, entities);
	const linked = new Set(refs.map((r) => r.id));
	for (const r of refs) {
		sel.append(el("option", { text: r.name, attrs: { value: r.id } }));
	}
	for (const e of entities) {
		if (!linked.has(e.id)) sel.append(el("option", { text: e.name, attrs: { value: e.id } }));
	}
	if (!sel.options.length) {
		sel.append(el("option", { text: "No entities yet", attrs: { value: "" } }));
	}
}

// The prior-ruling matcher, live: what the DM is typing, matched against
// the campaign's own rulings before anything is logged.
const matchPrior = debounce(async () => {
	if (!campaignID || !live || !live.session) return;
	const q = $("scr-cap-summary").value.trim();
	const wrap = $("scr-cap-prior");
	if ($("scr-cap-kind").textContent !== "ruling" || q.length < 3) {
		wrap.hidden = true;
		clear(wrap);
		return;
	}
	let body;
	try {
		body = await api.rulingMatches(campaignID, live.session.id, q);
	} catch (_) {
		return; // the matcher is advisory; a failed read stays out of the way
	}
	// A slow reply that outlived its form (closed, or switched to a
	// discovery) renders nothing.
	if ($("scr-cap-form").hidden || $("scr-cap-kind").textContent !== "ruling") return;
	const matches = body.matches || [];
	clear(wrap);
	if (!matches.length) {
		wrap.hidden = true;
		return;
	}
	wrap.hidden = false;
	wrap.append(el("p", { class: "scr-cap-prior-head", text: "Prior rulings on this:" }));
	for (const m of matches.slice(0, 5)) {
		wrap.append(el("div", {
			class: "scr-cap-prior-match",
			attrs: { title: `Session ${m.session_ordinal} · ${m.at || ""}` },
		},
			el("span", { class: "scr-cap-prior-ordinal", text: `S${m.session_ordinal}` }),
			el("span", { class: "scr-cap-prior-text" },
				el("span", { class: "scr-cap-prior-q", text: m.summary }),
				m.detail ? el("span", { class: "scr-cap-prior-a", text: ` — ${m.detail}` }) : null,
			),
		));
	}
}, 350);

async function onCaptureSubmit(e) {
	e.preventDefault();
	if (!campaignID || !live || !live.session) return;
	const kind = $("scr-cap-kind").textContent === "ruling" ? "ruling" : "discovery";
	const summary = $("scr-cap-summary").value.trim();
	const detail = $("scr-cap-detail").value.trim();
	const note = $("scr-cap-note");
	if (!summary) {
		note.textContent = "One line first — what happened.";
		return;
	}
	const scene = currentScene(live.scenes || [], live.session.id, pickedSceneID);
	const acting = capBattle ? actingCombatant(capBattle.order || []) : null;
	const payload = capturePayload(scene, capBattle && capBattle.combat, acting, entities);
	const propose = kind === "discovery" && $("scr-cap-propose").checked;
	note.textContent = propose ? "Logging… proposing…" : "Logging…";
	let ev;
	try {
		const body = await api.addEvent(campaignID, live.session.id, { kind, summary, detail, payload });
		ev = body.event;
	} catch (err) {
		note.textContent = err.message;
		return;
	}
	if (propose) {
		const subject = $("scr-cap-subject").value;
		const statement = $("scr-cap-statement").value.trim() || summary;
		if (!ev || !ev.id) {
			note.textContent = "Logged — but the event id never arrived; the proposal was not sent.";
			return;
		}
		if (!subject) {
			note.textContent = "Logged — but the proposal needs a subject entity.";
			return;
		}
		try {
			const res = await api.proposeEventFact(campaignID, live.session.id, ev.id, {
				statement,
				subject,
				predicate: $("scr-cap-predicate").value.trim() || "discovered",
				visibility: $("scr-cap-visibility").value,
			});
			const skipped = res.batch && res.batch.skipped ? res.batch.skipped.length : 0;
			note.textContent = skipped
				? "Logged — this fact proposal already sits in the review queue."
				: "Logged — proposed as a fact; Review decides.";
		} catch (err) {
			note.textContent = `Logged — the proposal failed: ${err.message}`;
			return;
		}
	} else {
		note.textContent = "Logged.";
	}
	// The event is the immutable record; leave the confirmation a beat,
	// then fold the form away for the next capture.
	setTimeout(() => { closeCapture(); }, 900);
}

function onProposeToggle() {
	$("scr-cap-fact").hidden = !$("scr-cap-propose").checked;
	if ($("scr-cap-propose").checked && !$("scr-cap-statement").value) {
		$("scr-cap-statement").value = $("scr-cap-summary").value.trim();
	}
}

/** The ticking half of the clock — one label, repainted each second. */
function tick() {
	if (!mounted || !live || !live.session) return;
	$("scr-time").textContent = elapsedLabel(live.session.started_at);
}

/* ---------- the writes ---------- */

async function onGoLive(e) {
	e.preventDefault();
	const sid = $("scr-golive-session").value;
	if (!sid || !campaignID) return;
	try {
		await api.updateSession(campaignID, sid, { status: "live" });
		pickable = null;
		await loadLive();
	} catch (err) {
		$("scr-golive-note").textContent = err.message;
	}
}

async function onParkNote(e) {
	e.preventDefault();
	const input = $("scr-note-input");
	const what = input.value.trim();
	const note = $("scr-note-note");
	if (!what || !campaignID || !live || !live.session) return;
	try {
		await api.addEvent(campaignID, live.session.id, { kind: "note", summary: what });
		input.value = "";
		note.textContent = "";
		const log = await api.listEvents(campaignID, live.session.id);
		notes = noteLines(log.events || []);
		renderNotes();
	} catch (err) {
		note.textContent = err.message;
	}
}

/* ---------- wiring ---------- */

function wire() {
	$("scr-campaign").addEventListener("change", async (e) => {
		if (!e.target.value) return;
		campaignID = e.target.value;
		localStorage.setItem("grimoire-screen-campaign", campaignID);
		await loadAll();
	});
	$("scr-golive").addEventListener("submit", onGoLive);
	$("scr-notes").addEventListener("submit", onParkNote);
	$("scr-disc-btn").addEventListener("click", () => openCapture("discovery"));
	$("scr-rule-btn").addEventListener("click", () => openCapture("ruling"));
	$("scr-cap-cancel").addEventListener("click", closeCapture);
	$("scr-cap-form").addEventListener("submit", onCaptureSubmit);
	$("scr-cap-propose").addEventListener("change", onProposeToggle);
	$("scr-cap-summary").addEventListener("input", matchPrior);
}

/* ---------- the window-manager contract ---------- */

export const tool = {
	mount(host) {
		const view = $("screen-view");
		host.append(view);
		view.hidden = false;
		mounted = true;
		if (!wired) {
			wire();
			wired = true;
		}
		if (!campaigns.length) loadCampaigns();
		else if (!live) loadLive();
		else render();
		// The clock is local; the context breathes on a slow beat —
		// scenes change at planning speed, not combat speed.
		tickTimer = setInterval(tick, 1000);
		pollTimer = setInterval(() => { loadLive(); }, POLL_MS);
		return {
			destroy() {
				mounted = false;
				if (tickTimer) clearInterval(tickTimer);
				if (pollTimer) clearInterval(pollTimer);
				tickTimer = null;
				pollTimer = null;
			},
		};
	},
};
