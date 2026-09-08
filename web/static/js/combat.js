// The combat tracker window (MAD-487, the surface of MAD-422): the DM's
// screen. The engine is fully landed — every route this module calls is
// the state machine's own; nothing here recomputes a rule. Start a fight
// from the party and a saved encounter (or a statblock search), advance
// turns with the prompts surfaced, and apply the mid-play writes from
// each combatant's row.
//
// The design constraint from MAD-318, verbatim: at the table, latency
// and legibility beat features. Readable at arm's length in a dim room.
// The densest stream of buttons in the app lives here — the damage box
// is a one-tap affair, not a form.
//
// One stream keeps it live: the board SSE pushes snapshot frames after
// every committed combat write (the store pings the campaign topic), so
// the tracker refreshes on those frames and after each of its own writes.
// Board and table screen stay consistent — they read the same state.

import { $, el, clear, debounce } from "./dom.js";
import { api } from "./api.js";
import {
	CONDITIONS, DAMAGE_TYPES, promptRows, concentrationRows,
	addLine, stepLine, startPayload, lineupCount, turnLabel,
} from "./combatvm.js";

let campaigns = [];
let campaignID = null;
let wired = false;
let mounted = false;
let streamCtl = null;

// The active battle's read: {combat, order, log?} or null.
let battle = null;
// The journal the last fetch carried (the ended view keeps showing it).
let journal = [];

/* ---------- the lineup ---------- */

let partyMembers = [];   // [{entity_id, name, block}] from the campaign party
let pickedPCs = new Set();
let monsters = [];       // [{name, count}]
let companions = [];     // [{name, count, companionName}]
let encounterID = "";    // the saved encounter the lineup came from, if any
let encountersById = new Map(); // id -> the encounter view, monsters inline
let searchAbort = null;
let compAbort = null;

/* ---------- the stream ---------- */

// The board's stream carries the campaign's snapshots; every committed
// combat write pings it. Frames in → quiet refresh of the same battle.
function startStream() {
	stopStream();
	if (!campaignID) return;
	streamCtl = { campaign: campaignID };
	const ctl = streamCtl;
	const connect = () => {
		if (!streamCtl || streamCtl.campaign !== campaignID) return;
		const es = new EventSource(`/api/campaigns/${encodeURIComponent(campaignID)}/board/stream`);
		ctl.es = es;
		for (const kind of ["open", "board"]) {
			es.addEventListener(kind, (ev) => {
				let payload;
				try { payload = JSON.parse(ev.data); } catch (_) { return; }
				if (!mounted || !battle) return;
				// Same battle → refresh it; a frame whose combat is gone or
				// different → the fight ended or moved on without this
				// window's hand.
				const frame = payload.board && payload.board.combat;
				if (!frame || frame.combat_id === battle.combat.id) refreshActive(false, true);
			});
		}
		es.onerror = () => {
			es.close();
			if (!streamCtl || streamCtl.campaign !== campaignID) return;
			ctl.failures = (ctl.failures || 0) + 1;
			if (ctl.failures > 8) return;
			setTimeout(connect, 2500);
		};
	};
	connect();
}

function stopStream() {
	if (streamCtl && streamCtl.es) streamCtl.es.close();
	streamCtl = null;
}

/* ---------- campaigns and boot ---------- */

async function loadCampaigns() {
	let data;
	try {
		data = await api.campaignList();
	} catch (err) {
		$("cbt-meta").textContent = err.message;
		return;
	}
	campaigns = (data.campaigns || []).filter((c) => c.my_role === "dm" || c.my_role === "keeper");
	const sel = clear($("cbt-campaign"));
	if (!campaigns.length) {
		sel.append(el("option", { text: "No campaigns yet…", attrs: { value: "" } }));
		$("cbt-body").hidden = true;
		$("cbt-empty").hidden = false;
		return;
	}
	$("cbt-empty").hidden = true;
	$("cbt-body").hidden = false;
	for (const c of campaigns) sel.append(el("option", { text: c.name, attrs: { value: c.id } }));
	const stored = localStorage.getItem("grimoire-combat-campaign");
	if (campaigns.some((c) => c.id === stored)) campaignID = stored;
	else if (!campaigns.some((c) => c.id === campaignID)) campaignID = campaigns[0].id;
	sel.value = campaignID;
	await loadAll();
}

async function loadAll() {
	stopStream();
	startStream();
	await refreshActive(true);
}

// refreshActive re-reads the campaign's live battle. `primeSetup` also
// primes the lineup when there is none — the opened tool is usable
// either way. A stream-triggered refresh leaves an ended battle's view
// standing: the ended summary and journal are the DM's to read, and the
// next frame is not a reason to take them away.
async function refreshActive(primeSetup = false, fromStream = false) {
	if (!campaignID) return;
	let data;
	try {
		data = await api.combatActive(campaignID);
	} catch (err) {
		$("cbt-meta").textContent = err.message;
		return;
	}
	if (!data.combat) {
		const wasActive = battle && battle.combat.status === "active";
		if (fromStream && !wasActive) return;
		battle = null;
		journal = [];
		if (primeSetup || !$("cbt-battle").hidden) await primeSetup();
		return;
	}
	battle = { combat: data.combat, order: data.order || [] };
	renderBattle();
	loadJournal();
}

/* ---------- the lineup builder ---------- */

async function primeSetup() {
	$("cbt-setup").hidden = false;
	$("cbt-battle").hidden = true;
	await Promise.all([loadParty(), loadEncounters(), loadSessions()]);
	renderLineup();
}

async function loadParty() {
	partyMembers = [];
	pickedPCs.clear();
	try {
		const data = await api.campaignParty(campaignID);
		partyMembers = (data.party && data.party.members) || [];
	} catch (_) {
		partyMembers = []; // not this campaign's DM — the start button will say so
	}
	// Everyone with declared numbers marches; the rest wait unchecked.
	for (const m of partyMembers) {
		if (m.block && (m.block.level || m.block.max_hp || m.block.ac)) pickedPCs.add(m.entity_id);
	}
}

async function loadEncounters() {
	const sel = $("cbt-encounter");
	const current = sel.value;
	clear(sel);
	encountersById = new Map();
	sel.append(el("option", { attrs: { value: "" }, text: "No saved encounter — field it by hand" }));
	try {
		const data = await api.campaignEncounters(campaignID);
		for (const enc of data.encounters || []) {
			encountersById.set(enc.id, enc);
			sel.append(el("option", { attrs: { value: enc.id }, text: enc.name }));
		}
	} catch (_) { /* the dropdown just offers none */ }
	sel.value = encountersById.has(current) ? current : "";
	encounterID = sel.value;
}

async function loadSessions() {
	const sel = $("cbt-session");
	const current = sel.value;
	clear(sel);
	sel.append(el("option", { attrs: { value: "" }, text: "No session — the journal stays combat-local" }));
	sel.append(el("option", { attrs: { value: "__new__" }, text: "＋ new session — start it live" }));
	try {
		const data = await api.listSessions(campaignID);
		for (const s of (data.sessions || []).slice().reverse()) {
			sel.append(el("option", {
				attrs: { value: s.id },
				text: `${s.ordinal}. ${s.name} — ${s.status}`,
			}));
		}
	} catch (_) { /* unlinked combat is fine */ }
	sel.value = [...sel.options].some((o) => o.value === current) ? current : "";
}

function onPickEncounter() {
	encounterID = $("cbt-encounter").value;
	monsters = [];
	const enc = encountersById.get(encounterID);
	if (enc) {
		for (const m of enc.monsters || []) addLine(monsters, m.name, m.count);
		if (enc.name && !$("cbt-name").value.trim()) $("cbt-name").value = enc.name;
	}
	renderLineup();
}

function renderLineup() {
	renderParty();
	renderMonsters();
	renderCompanions();
	const n = lineupCount([...pickedPCs], monsters, companions);
	$("cbt-start").disabled = n === 0;
	$("cbt-start").textContent = n === 0
		? "Field someone to start"
		: `Roll initiative — ${n} combatant${n === 1 ? "" : "s"}`;
}

function renderParty() {
	const host = clear($("cbt-party"));
	if (!partyMembers.length) {
		host.append(el("p", { class: "cbt-hint", text: "No pcs declared yet — add character sheets from the campaign world." }));
		return;
	}
	for (const m of partyMembers) {
		const hasSheet = m.block && (m.block.level || m.block.max_hp || m.block.ac);
		const sub = hasSheet
			? [m.block.level ? `L${m.block.level}` : "", m.block.class || ""].filter(Boolean).join(" · ")
			: "no structured sheet";
		host.append(el("label", { class: "cbt-pick" + (pickedPCs.has(m.entity_id) ? " is-on" : "") },
			el("input", {
				attrs: { type: "checkbox", "data-pc": m.entity_id, ...(pickedPCs.has(m.entity_id) ? { checked: true } : {}) },
			}),
			el("span", { class: "cbt-pick-name", text: m.name }),
			el("span", { class: "cbt-pick-sub" + (hasSheet ? "" : " is-warn"), text: sub })));
	}
}

function renderMonsters() {
	const host = clear($("cbt-monsters"));
	if (!monsters.length) {
		host.append(el("p", { class: "cbt-hint", text: "Nothing on the other side yet — pick a saved encounter or search the bestiary." }));
		return;
	}
	monsters.forEach((m, i) => {
		host.append(el("div", { class: "cbt-line" },
			el("span", { class: "cbt-line-count", text: `${m.count}×` }),
			el("span", { class: "cbt-line-name", text: m.name }),
			el("span", { class: "cbt-line-ctl" },
				el("button", { class: "cbt-step", attrs: { type: "button", "data-mon": String(i), "data-delta": "-1", "aria-label": `One fewer ${m.name}` }, text: "−" }),
				el("button", { class: "cbt-step", attrs: { type: "button", "data-mon": String(i), "data-delta": "1", "aria-label": `One more ${m.name}` }, text: "+" }),
				el("button", { class: "cbt-step rm", attrs: { type: "button", "data-mon-rm": String(i), "aria-label": `Remove ${m.name}` }, text: "✕" }))));
	});
}

function renderCompanions() {
	const host = clear($("cbt-companions"));
	if (!companions.length) {
		host.append(el("p", { class: "cbt-hint", text: "The wolf, the sidekick — statblock beside the party, under its own name." }));
		return;
	}
	companions.forEach((c, i) => {
		host.append(el("div", { class: "cbt-line" },
			el("span", { class: "cbt-line-count", text: `${c.count}×` }),
			el("span", { class: "cbt-line-name", text: c.companionName ? `${c.companionName} (${c.name})` : c.name }),
			el("span", { class: "cbt-line-ctl" },
				el("button", { class: "cbt-step", attrs: { type: "button", "data-comp": String(i), "data-delta": "-1" }, text: "−" }),
				el("button", { class: "cbt-step", attrs: { type: "button", "data-comp": String(i), "data-delta": "1" }, text: "+" }),
				el("button", { class: "cbt-step rm", attrs: { type: "button", "data-comp-rm": String(i) }, text: "✕" }))));
	});
}

function setupNote(text) {
	$("cbt-setup-note").textContent = text || "";
}

/* ---------- the statblock pickers ---------- */

const runSearch = debounce(async (inputId, resultsId, abortName, picker) => {
	const q = $(inputId).value.trim();
	const box = clear($(resultsId));
	if (q.length < 2) return;
	if (searchAbort && abortName === "mon") searchAbort.abort();
	if (compAbort && abortName === "comp") compAbort.abort();
	const controller = new AbortController();
	if (abortName === "mon") searchAbort = controller; else compAbort = controller;
	try {
		const data = await api.encounterMonsters(q, controller.signal);
		if (controller.signal.aborted) return;
		const hits = data.monsters || [];
		if (!hits.length) {
			box.append(el("p", { class: "cbt-hint", text: `No statblocks match “${q}”.` }));
			return;
		}
		for (const m of hits) {
			box.append(el("button", {
				class: "cbt-hit",
				attrs: { type: "button", "data-add": picker, "data-name": m.name, title: `Add ${m.name} (CR ${m.cr})` },
			},
				el("span", { class: "cbt-hit-name", text: m.name }),
				el("span", { class: "cbt-hit-meta", text: `CR ${m.cr}${m.type ? " · " + m.type : ""}` })));
		}
	} catch (err) {
		if (err.name !== "AbortError") box.append(el("p", { class: "cbt-hint is-warn", text: `Search failed: ${err.message}` }));
	}
}, 300);

/* ---------- starting ---------- */

async function onStart(e) {
	e.preventDefault();
	if (!campaignID) return;
	setupNote("Rolling initiative…");
	let sessionID = $("cbt-session").value;
	try {
		if (sessionID === "__new__") {
			const created = await api.createSession(campaignID, "");
			sessionID = created.session.id;
			await api.updateSession(campaignID, sessionID, { status: "live" });
		} else if (sessionID) {
			// A planned session goes live with the fight; a live or done
			// one links as-is.
			const list = await api.listSessions(campaignID);
			const picked = (list.sessions || []).find((s) => s.id === sessionID);
			if (picked && picked.status === "planned") {
				await api.updateSession(campaignID, sessionID, { status: "live" });
			}
		}
		const body = startPayload({
			name: $("cbt-name").value,
			encounterID,
			sessionID,
			pcs: [...pickedPCs],
			monsters,
			companions,
		});
		const data = await api.combatStart(campaignID, body);
		battle = { combat: data.combat, order: data.order || [] };
		showPrompts((data.warnings || []).map((w) => ({ kind: "warn", text: w })));
		setupNote("");
		$("cbt-name").value = "";
		monsters = [];
		companions = [];
		renderBattle();
		loadJournal();
	} catch (err) {
		setupNote(err.message);
	}
}

/* ---------- the battle view ---------- */

let lastFocus = null; // the combatant row whose amount box had focus

function renderBattle() {
	const view = $("cbt-battle");
	view.hidden = false;
	$("cbt-setup").hidden = true;
	if (!battle) return;
	const c = battle.combat;
	const ended = c.status !== "active";
	$("cbt-round").textContent = ended ? "The fight is over" : `Round ${c.round}`;
	$("cbt-turn").textContent = ended
		? (c.end_reason ? c.end_reason : turnLabel(c, battle.order))
		: `— ${turnLabel(c, battle.order)}`;
	$("cbt-next").hidden = ended;
	$("cbt-new").hidden = !ended;
	// Alive counts derive from the order on screen — the response after a
	// mid-play write carries the combatant, not the counts.
	const alive = { party: 0, foe: 0 };
	for (const ct of battle.order || []) if (!ct.dead) alive[ct.side] = (alive[ct.side] || 0) + 1;
	$("cbt-alive").textContent = ended
		? `${alive.party ?? 0} of the party standing · ${alive.foe ?? 0} of the foe`
		: `${alive.party ?? 0} of the party up · ${alive.foe ?? 0} of the foe standing`;
	renderOrder();
}

function renderOrder() {
	const host = clear($("cbt-order"));
	for (const ct of battle.order || []) host.append(combatantRow(ct));
	if (!host.childElementCount) host.append(el("p", { class: "cbt-hint", text: "No one in the order." }));
	restoreFocus();
}

function restoreFocus() {
	if (!lastFocus) return;
	const row = document.querySelector(`[data-row="${lastFocus}"]`);
	const box = row && row.querySelector(".cbt-amount");
	if (box) {
		box.focus();
		box.select();
	}
	lastFocus = null;
}

function combatantRow(ct) {
	const snap = ct.statblock || {};
	const statblockBacked = ct.kind !== "pc";
	const row = el("article", {
		class: "cbt-card" +
			(ct.is_turn ? " is-turn" : "") +
			(ct.side === "foe" ? " is-foe" : "") +
			(ct.downed ? " is-down" : "") +
			(ct.dead ? " is-dead" : ""),
		attrs: { "data-row": ct.id },
	});

	// The head: initiative, name, the numbers that don't move.
	const head = el("div", { class: "cbt-card-head" });
	head.append(el("b", { class: "cbt-init", text: String(ct.initiative), title: ct.init_formula || "initiative" }));
	const named = el("span", { class: "cbt-name" }, el("b", { text: ct.name }));
	if (ct.is_turn) named.append(el("i", { class: "cbt-turn-mark", text: "▶ acting" }));
	head.append(named);
	const sub = [`AC ${ct.ac}`];
	if (snap.label) sub.push(snap.label);
	if (snap.cr) sub.push(`CR ${snap.cr}`);
	if (snap.statblock) sub.push(snap.statblock);
	head.append(el("span", { class: "cbt-sub", text: sub.join(" · ") }));
	row.append(head);

	// The vitals: the biggest number on the card. Temp hp rides beside it.
	const vitals = el("div", { class: "cbt-vitals" });
	const max = ct.effective_max_hp || ct.max_hp || 0;
	const frac = max > 0 ? Math.max(0, Math.min(1, ct.hp / max)) : 0;
	const bar = el("span", { class: "cbt-bar" });
	bar.append(el("i", { class: "cbt-bar-fill", attrs: { style: `width:${Math.round(frac * 100)}%` } }));
	vitals.append(bar);
	vitals.append(el("b", {
		class: "cbt-hp" + (ct.dead ? " is-zero" : ct.hp === 0 || ct.downed ? " is-zero" : (max > 0 && ct.hp * 2 <= max ? " is-low" : "")),
		text: String(ct.hp),
	}));
	vitals.append(el("span", { class: "cbt-max", text: `/ ${max}${ct.temp_hp ? ` (+${ct.temp_hp} temp)` : ""}` }));
	if (ct.downed && !ct.dead) {
		vitals.append(el("span", { class: "cbt-state is-down", text: ct.stable ? "stable" : "dying" }));
	}
	if (ct.dead) vitals.append(el("span", { class: "cbt-state is-dead", text: "dead" }));
	row.append(vitals);

	// Death-save pips: three and three, the sheet's own grammar.
	if (ct.downed && !ct.dead && !ct.stable && ct.kind === "pc") {
		const saves = el("span", { class: "cbt-saves", title: "death saves — successes then failures" });
		for (let i = 0; i < 3; i++) saves.append(el("i", { class: "cbt-pip ok" + (i < ct.death_successes ? " is-full" : "") }));
		saves.append(el("i", { class: "cbt-pip-sep" }));
		for (let i = 0; i < 3; i++) saves.append(el("i", { class: "cbt-pip bad" + (i < ct.death_failures ? " is-full" : "") }));
		vitals.append(saves);
	}

	// Chips: conditions with their end button, the reveal state.
	const chips = el("div", { class: "cbt-chips" });
	for (const cond of ct.conditions || []) {
		const chip = el("span", { class: "cbt-chip is-cond", title: `${cond.rounds} round${cond.rounds === 1 ? "" : "s"} left` },
			el("span", { text: cond.name }));
		chip.append(el("button", {
			class: "cbt-chip-x",
			attrs: { type: "button", "data-cond-end": cond.id, "data-ctid": ct.id, "aria-label": `End ${cond.name}` },
			text: "×",
		}));
		chips.append(chip);
	}
	if (ct.reaction_spent) chips.append(el("span", { class: "cbt-chip is-spent", text: "reaction spent" }));
	if (ct.side === "foe" && ct.reveal) {
		chips.append(el("span", { class: "cbt-chip is-reveal", text: ct.reveal === "hp" ? "room sees hp" : "room sees a word" }));
	}
	if (chips.childElementCount) row.append(chips);

	// The one-tap row: amount, type, and the verbs. Enter applies damage.
	if (!ct.dead) {
		const act = el("form", { class: "cbt-act", attrs: { "data-act": ct.id }, on: { submit: onApplyDamage } });
		act.append(el("input", {
			class: "cbt-amount",
			attrs: { type: "number", inputmode: "numeric", placeholder: "hp", "aria-label": `Amount for ${ct.name}`, "data-amount": ct.id },
		}));
		act.append(el("input", {
			class: "cbt-type",
			attrs: { type: "text", list: "cbt-damage-types", placeholder: "type", "aria-label": "Damage type", "data-type": ct.id },
		}));
		act.append(el("button", { class: "cbt-verb hit", attrs: { type: "submit" }, text: "hit" }));
		act.append(el("button", {
			class: "cbt-verb heal", attrs: { type: "button", "data-do": "heal", "data-ctid": ct.id }, text: "heal",
		}));
		act.append(el("button", {
			class: "cbt-verb temp", attrs: { type: "button", "data-do": "temp", "data-ctid": ct.id }, text: "temp",
		}));
		row.append(act);

		// Death saves: downed pcs only — four faces of the d20.
		if (ct.downed && ct.kind === "pc") {
			const saves = el("div", { class: "cbt-deathrow" });
			for (const [label, result] of [["✓", "success"], ["✗", "fail"], ["20", "crit-success"], ["1", "crit-fail"]]) {
				saves.append(el("button", {
					class: "cbt-verb save" + (result.startsWith("crit") ? " crit" : ""),
					attrs: { type: "button", "data-save": result, "data-ctid": ct.id, title: `Death save: ${result}` },
					text: label,
				}));
			}
			row.append(saves);
		}

		// The row of less-common verbs: reaction, conditions, legendary,
		// the reveal. Statblock-backed rows only where the engine says so.
		const more = el("div", { class: "cbt-more" });
		more.append(el("button", {
			class: "cbt-mini" + (ct.reaction_spent ? " is-on" : ""),
			attrs: { type: "button", "data-do": "reaction", "data-ctid": ct.id },
			text: ct.reaction_spent ? "reaction back" : "spend reaction",
		}));
	if (statblockBacked) {
		more.append(el("select", {
			class: "cbt-cond-pick",
			attrs: { "aria-label": `Condition for ${ct.name}`, "data-cond": ct.id },
			on: { change: onConditionPick },
		},
			el("option", { attrs: { value: "" }, text: "condition…" }),
			...CONDITIONS.map((name) => el("option", { attrs: { value: name }, text: name }))));
		more.append(el("input", {
			class: "cbt-rounds",
			attrs: { type: "number", min: "1", max: "20", placeholder: "rds", "aria-label": "Rounds", "data-rounds": ct.id },
		}));
	}
		if (snap.legendary_max > 0) {
			more.append(el("button", {
				class: "cbt-mini",
				attrs: { type: "button", "data-do": "legendary", "data-ctid": ct.id },
				text: `legendary ${ct.legendary_used}/${snap.legendary_max}`,
			}));
		}
		if (ct.side === "foe") {
			more.append(el("button", {
				class: "cbt-mini" + (ct.reveal ? " is-on" : ""),
				attrs: { type: "button", "data-do": "reveal", "data-ctid": ct.id, "data-reveal": ct.reveal || "" },
				text: revealLabel(ct.reveal),
			}));
		}
		row.append(more);
	}

	// The peek: the frozen snapshot, in the statblock's own spelling.
	const lines = [];
	if (snap.resist && snap.resist.length) lines.push(`Resists ${snap.resist.join(", ")}`);
	if (snap.immune && snap.immune.length) lines.push(`Immune to ${snap.immune.join(", ")}`);
	if (snap.vulnerable && snap.vulnerable.length) lines.push(`Vulnerable to ${snap.vulnerable.join(", ")}`);
	if (snap.recharge && snap.recharge.length) lines.push(`Recharge: ${snap.recharge.map((r) => `${r.name} (${r.usage})`).join(", ")}`);
	if (snap.lair) lines.push("Lair actions — fires at initiative count 20, losing ties");
	if (lines.length) {
		const details = el("details", { class: "cbt-peek" }, el("summary", { text: "statblock" }));
		for (const line of lines) details.append(el("p", { text: line }));
		row.append(details);
	}
	return row;
}

function revealLabel(mode) {
	if (mode === "hp") return "room sees hp";
	if (mode === "word") return "room sees a word";
	return "hidden from the room";
}

function nextReveal(mode) {
	if (mode === "") return "hp";
	if (mode === "hp") return "word";
	return "";
}

/* ---------- prompts, journal, notes ---------- */

function showPrompts(rows) {
	const host = clear($("cbt-prompts"));
	for (const r of rows) host.append(el("p", { class: "cbt-prompt is-" + (r.kind || "note"), text: r.text }));
	$("cbt-prompts").hidden = !rows.length;
}

async function loadJournal() {
	if (!battle) return;
	try {
		const data = await api.combatGet(campaignID, battle.combat.id);
		journal = data.log || [];
	} catch (_) {
		return; // the journal is a tab, not a gate
	}
	renderJournal();
}

function renderJournal() {
	const host = clear($("cbt-journal"));
	const rows = journal.slice().reverse();
	for (const entry of rows) {
		const at = entry.created_at ? new Date(entry.created_at).toLocaleTimeString() : "";
		host.append(el("article", { class: "cbt-entry is-" + entry.kind },
			el("span", { class: "cbt-entry-kind", text: entry.kind.replace(/_/g, " ") }),
			el("span", { class: "cbt-entry-note", text: entry.note || "" }),
			el("time", { class: "cbt-entry-time", text: at })));
	}
	if (!host.childElementCount) host.append(el("p", { class: "cbt-hint", text: "Nothing logged yet." }));
}

function note(text) {
	$("cbt-note").textContent = text || "";
}

/* ---------- the turn engine ---------- */

async function onNext() {
	if (!battle) return;
	try {
		const data = await api.combatNext(campaignID, battle.combat.id);
		battle = { combat: data.combat, order: data.order || [] };
		renderBattle();
		showPrompts(promptRows(data));
		loadJournal();
	} catch (err) {
		note(err.message);
	}
}

async function onEnd() {
	if (!battle) return;
	const reason = window.prompt("How did it end? (the journal keeps the words)", "");
	if (reason === null) return;
	try {
		const data = await api.combatEnd(campaignID, battle.combat.id, reason);
		battle = { combat: data.combat, order: battle.order };
		showPrompts([{ kind: "ended", text: data.summary || "The fight ends." }]);
		renderBattle();
		loadJournal();
	} catch (err) {
		note(err.message);
	}
}

/* ---------- the mid-play writes ---------- */

// The write path: apply, repaint from the response, and surface what the
// engine asks (concentration rides the damage response). One helper, one
// error surface — every button below funnels through it.
async function apply(fn) {
	if (!battle) return;
	try {
		note("");
		const out = await fn();
		// Fold the returned combatant into the order — server truth, not
		// a local patch.
		const idx = battle.order.findIndex((c) => c.id === out.combatant.id);
		if (idx >= 0) battle.order[idx] = { ...battle.order[idx], ...out.combatant, is_turn: battle.order[idx].is_turn };
		renderBattle();
		const prompts = concentrationRows(out.concentration_checks || []);
		if (prompts.length) showPrompts(prompts);
		if (out.summary) note(out.summary);
		loadJournal();
	} catch (err) {
		note(err.message);
	}
}

function amountFor(ctid) {
	const box = document.querySelector(`[data-amount="${ctid}"]`);
	const n = box ? parseInt(box.value, 10) : NaN;
	if (Number.isInteger(n) && n !== 0) {
		box.value = "";
		return Math.abs(n);
	}
	return null;
}

function typeFor(ctid) {
	const box = document.querySelector(`[data-type="${ctid}"]`);
	return box ? box.value.trim().toLowerCase() : "";
}

// Damage lands from the acting combatant by default — the journal's
// attribution (MAD-428's campaign stats) reads it back flat.
function actingSource() {
	const c = battle && battle.combat;
	if (!c || c.turn_index < 0) return "";
	const acting = battle.order[c.turn_index];
	return acting ? acting.id : "";
}

function onApplyDamage(e) {
	e.preventDefault();
	const form = e.target.closest("[data-act]");
	if (!form) return;
	const ctid = form.dataset.act;
	const amount = amountFor(ctid);
	if (amount == null) {
		note("Say how much first.");
		return;
	}
	lastFocus = ctid;
	apply(() => api.combatDamage(campaignID, battle.combat.id, ctid, amount, typeFor(ctid), "", false, actingSource()));
}

async function onOrderClick(e) {
	const hit = e.target.closest("[data-add]");
	if (hit) {
		const name = hit.dataset.name;
		if (hit.dataset.add === "comp") {
			const line = companions.find((c) => c.name === name && !c.companionName);
			if (line) line.count++;
			else {
				const named = $("cbt-comp-name").value.trim();
				companions.push({ name, count: 1, companionName: named });
				$("cbt-comp-name").value = "";
			}
		} else {
			addLine(monsters, name, 1);
		}
		renderLineup();
		return;
	}
	const mon = e.target.closest("[data-mon]");
	if (mon) { stepLine(monsters, parseInt(mon.dataset.mon, 10), parseInt(mon.dataset.delta, 10)); renderLineup(); return; }
	const monRm = e.target.closest("[data-mon-rm]");
	if (monRm) { monsters.splice(parseInt(monRm.dataset.monRm, 10), 1); renderLineup(); return; }
	const comp = e.target.closest("[data-comp]");
	if (comp) { stepLine(companions, parseInt(comp.dataset.comp, 10), parseInt(comp.dataset.delta, 10)); renderLineup(); return; }
	const compRm = e.target.closest("[data-comp-rm]");
	if (compRm) { companions.splice(parseInt(compRm.dataset.compRm, 10), 1); renderLineup(); return; }
}

function onPartyClick(e) {
	const box = e.target.closest("[data-pc]");
	if (!box) return;
	if (box.checked) pickedPCs.add(box.dataset.pc);
	else pickedPCs.delete(box.dataset.pc);
	const label = box.closest(".cbt-pick");
	if (label) label.classList.toggle("is-on", box.checked);
	renderLineup();
}

async function onBattleClick(e) {
	const btn = e.target.closest("[data-do]");
	if (btn) {
		const ctid = btn.dataset.ctid;
		const doWhat = btn.dataset.do;
		if (doWhat === "heal" || doWhat === "temp") {
			const amount = amountFor(ctid);
			if (amount == null) { note("Say how much first."); return; }
			lastFocus = ctid;
			await apply(() => doWhat === "heal"
				? api.combatHeal(campaignID, battle.combat.id, ctid, amount)
				: api.combatTempHP(campaignID, battle.combat.id, ctid, amount));
			return;
		}
		if (doWhat === "reaction") {
			await apply(() => api.combatReaction(campaignID, battle.combat.id, ctid, null));
			return;
		}
		if (doWhat === "legendary") {
			const ability = window.prompt("Which legendary action?");
			if (!ability || !ability.trim()) return;
			await apply(() => api.combatLegendary(campaignID, battle.combat.id, ctid, ability.trim(), 1));
			return;
		}
		if (doWhat === "reveal") {
			const mode = nextReveal(btn.dataset.reveal || "");
			try {
				await api.combatReveal(campaignID, battle.combat.id, ctid, mode);
				await refreshActive();
			} catch (err) {
				note(err.message);
			}
			return;
		}
	}
	const save = e.target.closest("[data-save]");
	if (save) {
		await apply(() => api.combatDeathSave(campaignID, battle.combat.id, save.dataset.ctid, save.dataset.save));
		return;
	}
	const condEnd = e.target.closest("[data-cond-end]");
	if (condEnd) {
		await apply(() => api.combatConditionEnd(campaignID, battle.combat.id, condEnd.dataset.ctid, condEnd.dataset.condEnd));
	}
}

// onConditionPick applies the picked condition — the select resets so the
// same condition can be re-applied without a repaint in between.
async function onConditionPick(e) {
	const pick = e.target;
	const name = pick.value;
	if (!name) return;
	const roundsBox = document.querySelector(`[data-rounds="${pick.dataset.cond}"]`);
	const rounds = roundsBox ? Math.max(1, parseInt(roundsBox.value, 10) || 1) : 1;
	pick.value = "";
	if (roundsBox) roundsBox.value = "";
	await apply(() => api.combatCondition(campaignID, battle.combat.id, pick.dataset.cond, name, rounds));
}

/* ---------- tabs ---------- */

function showTab(which) {
	const isJournal = which === "journal";
	$("cbt-battle-main").hidden = isJournal;
	$("cbt-journal").hidden = !isJournal;
	$("cbt-tab-battle").classList.toggle("is-on", !isJournal);
	$("cbt-tab-journal").classList.toggle("is-on", isJournal);
	if (isJournal) loadJournal();
}

/* ---------- wiring ---------- */

function wire() {
	// The one-tap box's type suggestions: the thirteen, offered not
	// enforced — the engine matches the snapshot's own lists either way.
	const dl = clear($("cbt-damage-types"));
	for (const t of DAMAGE_TYPES) dl.append(el("option", { attrs: { value: t } }));
	$("cbt-campaign").addEventListener("change", async () => {
		const id = $("cbt-campaign").value;
		if (!id || id === campaignID) return;
		campaignID = id;
		localStorage.setItem("grimoire-combat-campaign", id);
		battle = null;
		journal = [];
		showPrompts([]);
		await loadAll();
	});
	$("cbt-encounter").addEventListener("change", onPickEncounter);
	$("cbt-party").addEventListener("change", onPartyClick);
	$("cbt-setup-lines").addEventListener("click", onOrderClick);
	$("cbt-search").addEventListener("submit", (e) => {
		e.preventDefault();
		runSearch("cbt-search-input", "cbt-results", "mon", "mon");
	});
	$("cbt-search-input").addEventListener("input", () => runSearch("cbt-search-input", "cbt-results", "mon", "mon"));
	$("cbt-comp-search").addEventListener("submit", (e) => {
		e.preventDefault();
		runSearch("cbt-comp-search-input", "cbt-comp-results", "comp", "comp");
	});
	$("cbt-comp-search-input").addEventListener("input", () => runSearch("cbt-comp-search-input", "cbt-comp-results", "comp", "comp"));
	$("cbt-start-form").addEventListener("submit", onStart);
	$("cbt-next").addEventListener("click", onNext);
	$("cbt-end").addEventListener("click", onEnd);
	$("cbt-new").addEventListener("click", primeSetup);
	$("cbt-order").addEventListener("click", onBattleClick);
	$("cbt-tab-battle").addEventListener("click", () => showTab("battle"));
	$("cbt-tab-journal").addEventListener("click", () => showTab("journal"));
}

/* ---------- the window-manager contract ---------- */

export const tool = {
	mount(host) {
		const view = $("combat-view");
		host.append(view);
		view.hidden = false;
		mounted = true;
		if (!wired) {
			wire();
			wired = true;
		}
		if (!campaigns.length) loadCampaigns();
		else if (!streamCtl) loadAll();
		return {
			destroy() {
				mounted = false;
				stopStream(); // the board keeps its own stream for presence
			},
		};
	},
};
