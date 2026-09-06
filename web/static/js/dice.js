// The dice window (MAD-420, stage 3 of MAD-417): the roll bar, the quick
// chips, the live shared feed — and the stage, where a roll is an event
// the whole table watches land. A roll is created the moment the server
// answers, but it is *revealed* on a timeline: the dice tumble, settle on
// their natural values, the modifiers slide in, and the total stamps —
// the anticipation is the feature, not decoration.
//
// One stream serves everyone: the same SSE connection feeds this window's
// list and the app-wide roll curtain (every public roll drops in front of
// whoever is watching the campaign, dice window open or not). Visibility
// is the server's call, as always — a player's stream simply never
// carries a secret roll.

import { openTool } from "./wm/wm.js";
import { $, el, clear } from "./dom.js";
import { api } from "./api.js";

let campaigns = [];
let campaignID = null;
let standing = { dm: true, character_id: "" };
let cursor = 0;
let quickCharacter = ""; // the sheet the quick-roll chips belong to (DM picks)
let mode = "";           // "", advantage, disadvantage
let secret = false;

let wired = false;
let mounted = false;
let seen = new Set();
let stageTimers = [];
let streamCtl = null;    // the shared stream (window + curtain)

const prefersReduced = () => window.matchMedia("(prefers-reduced-motion: reduce)").matches;

/* ---------- the shared stream ---------- */

// startStream owns the one EventSource for the chosen campaign. Roll
// events fan out to the window's feed and the curtain; the window's
// callback is registered while mounted, the curtain's always.
function startStream() {
	stopStream();
	if (!campaignID) return;
	streamCtl = { campaign: campaignID };
	const ctl = streamCtl;

	const connect = () => {
		if (!streamCtl || streamCtl.campaign !== campaignID) return;
		const es = new EventSource(`/api/campaigns/${encodeURIComponent(campaignID)}/rolls/stream?after=${cursor}`);
		ctl.es = es;
		es.addEventListener("open", () => {
			ctl.failures = 0; // the stream is healthy again
		});
		es.addEventListener("roll", (ev) => {
			let payload;
			try { payload = JSON.parse(ev.data); } catch (_) { return; }
			const roll = payload.roll;
			if (!roll || seen.has(roll.id)) return;
			seen.add(roll.id);
			cursor = Math.max(cursor, roll.seq);
			if (mounted) appendFeedRow(roll);
			announceCurtain(roll);
		});
		es.onerror = () => {
			// EventSource would reconnect against the stale cursor's URL;
			// close it and reconnect fresh so the gap is fetched, then
			// backfill through the REST window in case the gap is old.
			// A stream that keeps failing (logged out, revoked) stops
			// trying rather than looping forever — the window's next open
			// starts a fresh one.
			es.close();
			if (!streamCtl || streamCtl.campaign !== campaignID) return;
			ctl.failures = (ctl.failures || 0) + 1;
			if (ctl.failures > 8) return;
			setTimeout(async () => {
				connect();
				try {
					const data = await api.diceFeed(campaignID, cursor, 50);
					for (const roll of data.rolls || []) {
						if (seen.has(roll.id)) continue;
						seen.add(roll.id);
						cursor = Math.max(cursor, roll.seq);
						if (mounted) appendFeedRow(roll);
						announceCurtain(roll);
					}
				} catch (_) { /* the reconnect will bring it */ }
			}, 2500);
		};
	};
	connect();
}

function stopStream() {
	if (streamCtl && streamCtl.es) streamCtl.es.close();
	streamCtl = null;
}

/* ---------- the stage ---------- */

// dieEl builds one die: a gold-rimmed stone face in the die's own
// polygon, the value settable for tumble and settle. Every surface of the
// dice feature renders dice through this one shape, so the stage, the
// curtain and the feed all speak the same visual language.
function dieEl(sides, value, opts = {}) {
	const die = el("span", { class: `die die-${sides}` + (opts.dropped ? " is-dropped" : "") });
	const face = el("span", { class: "die-face" });
	face.append(el("b", { class: "die-val", text: value == null ? "" : String(value) }));
	die.append(face);
	return die;
}

function stageDiceEls(roll) {
	const out = [];
	for (const term of roll.dice || []) {
		if (term.kind !== "dice") continue;
		for (const d of term.dice || []) {
			out.push({ sides: term.sides, value: d.value, kept: d.kept });
		}
	}
	return out;
}

// miniDice flattens engine terms into one row of die elements — the shape
// every surface (stage, curtain, feed, cards, session log) renders.
export function miniDice(terms) {
	const row = el("span", { class: "dice-row-dice" });
	for (const term of terms || []) {
		if (term.kind !== "dice") continue;
		for (const d of term.dice || []) {
			row.append(dieEl(term.sides, d.value, { dropped: !d.kept }));
		}
	}
	return row;
}

// playStage runs the reveal: tumble (values cycling), settle (natural
// values stamp, dropped dice tip over), modifiers, total. Reduced motion
// renders the finished tableau at once — same information, no theater.
function playStage(roll) {
	const stage = $("dice-stage");
	for (const t of stageTimers) clearTimeout(t);
	stageTimers = [];
	clear(stage);
	stage.classList.remove("is-nat20", "is-nat1");

	const dice = stageDiceEls(roll);
	const reduced = prefersReduced();

	const head = el("p", { class: "dice-stage-who" });
	head.append(stageWho(roll));
	stage.append(head);

	const row = el("div", { class: "dice-stage-dice" });
	stage.append(row);
	const total = el("b", { class: "dice-total", text: String(roll.total) });
	const mods = el("span", { class: "dice-stage-mods" });
	stage.append(el("div", { class: "dice-stage-result" }, mods, total));

	if (!dice.length) { // a flat formula — no dice to throw
		total.classList.add("is-stamping");
		return;
	}

	const els = dice.map((d) => {
		const elDie = dieEl(d.sides, reduced ? d.value : null, { dropped: !d.kept });
		row.append(elDie);
		return elDie;
	});

	if (reduced) {
		if (roll.natural_20) stage.classList.add("is-nat20");
		if (roll.natural_1) stage.classList.add("is-nat1");
		return;
	}

	// Tumble: every die spins on its own clock while its value cycles.
	const cycles = els.map((elDie, i) => {
		const sides = dice[i].sides;
		const val = elDie.querySelector(".die-val");
		elDie.classList.add("is-tumbling");
		return setInterval(() => {
			val.textContent = String(1 + Math.floor(Math.random() * sides));
		}, 70 + (i % 3) * 18);
	});

	// Settle: staggered left to right, the natural value lands.
	dice.forEach((d, i) => {
		const at = 820 + i * 130;
		stageTimers.push(setTimeout(() => {
			clearInterval(cycles[i]);
			const elDie = els[i];
			elDie.classList.remove("is-tumbling");
			elDie.classList.add("is-settled");
			elDie.querySelector(".die-val").textContent = String(d.value);
		}, at));
	});

	const settleDone = 820 + dice.length * 130 + 240;
	if (roll.modifier) {
		stageTimers.push(setTimeout(() => {
			mods.append(el("span", { class: "dice-mod", text: (roll.modifier > 0 ? "+" : "") + roll.modifier }));
		}, settleDone));
	}
	stageTimers.push(setTimeout(() => {
		total.classList.add("is-stamping");
		if (roll.natural_20) stage.classList.add("is-nat20");
		if (roll.natural_1) stage.classList.add("is-nat1");
	}, settleDone + 120));
}

// stageWho is the stage's one-line provenance: who, what, for what.
function stageWho(roll) {
	const wrap = el("span", {});
	const who = roll.character_name || roll.actor_name || "the table";
	wrap.append(el("b", { text: who }), el("span", { class: "dice-stage-formula", text: ` ${roll.notation}` }));
	if (roll.context && roll.context !== "other") {
		wrap.append(el("span", { class: "dice-stage-kind", text: ` ${roll.context}` }));
	}
	if (roll.detail) {
		wrap.append(el("span", { class: "dice-stage-detail", text: ` — ${roll.detail}` }));
	}
	return wrap;
}

/* ---------- the curtain ---------- */

let curtainTimer = null;

// announceCurtain drops the party-wide moment: a compact plate from the
// top of the app — the dice, the total, whose it was. Clicking it opens
// this window; it leaves on its own after a few seconds.
function announceCurtain(roll) {
	const curtain = $("roll-curtain");
	if (!curtain) return;
	if (curtainTimer) clearTimeout(curtainTimer);
	clear(curtain);
	curtain.hidden = false;
	curtain.className = "roll-curtain" + (roll.natural_20 ? " is-nat20" : roll.natural_1 ? " is-nat1" : "");

	const dice = el("span", { class: "roll-curtain-dice" });
	for (const d of stageDiceEls(roll).slice(0, 10)) dice.append(dieEl(d.sides, d.value, { dropped: !d.kept }));
	const who = el("b", { class: "roll-curtain-who", text: roll.character_name || roll.actor_name || "the table" });
	const label = el("span", { class: "roll-curtain-label" });
	label.append(who, el("span", { text: ` ${roll.formula.replace(/\s+/g, "")}` }));
	if (roll.detail) label.append(el("span", { class: "roll-curtain-detail", text: ` — ${roll.detail}` }));
	const total = el("b", { class: "roll-curtain-total", text: String(roll.total) });

	curtain.append(dice, label, total);
	curtain.title = "Open the dice window";
	curtain.onclick = () => {
		hideCurtain();
		openTool("dice");
	};
	curtainTimer = setTimeout(hideCurtain, 4200);
}

function hideCurtain() {
	const curtain = $("roll-curtain");
	if (!curtain) return;
	if (curtainTimer) clearTimeout(curtainTimer);
	curtainTimer = null;
	curtain.hidden = true;
	clear(curtain);
}

/* ---------- the feed ---------- */

function feedRow(roll) {
	const dice = el("span", { class: "dice-row-dice" });
	for (const d of stageDiceEls(roll).slice(0, 12)) dice.append(dieEl(d.sides, d.value, { dropped: !d.kept }));
	const line = el("span", { class: "dice-row-line" });
	line.append(el("b", { class: "dice-row-who", text: roll.character_name || roll.actor_name || "the table" }));
	line.append(el("span", { class: "dice-row-formula", text: ` ${roll.formula.replace(/\s+/g, "")}` }));
	if (roll.mode) line.append(el("i", { class: "dice-row-mode", text: ` ${roll.mode === "advantage" ? "adv" : "dis"}` }));
	if (roll.detail) line.append(el("span", { class: "dice-row-detail", text: ` — ${roll.detail}` }));

	const right = el("span", { class: "dice-row-side" });
	right.append(el("b", { class: "dice-row-total", text: String(roll.total) }));
	if (roll.natural_20) right.append(el("i", { class: "dice-flare is-20", text: "nat 20" }));
	if (roll.natural_1) right.append(el("i", { class: "dice-flare is-1", text: "nat 1" }));

	const row = el("article", {
		class: "dice-row" + (roll.visibility === "secret" ? " is-secret" : "") + (roll.seq >= cursor ? " is-new" : ""),
		attrs: { "data-roll": roll.id },
	},
		dice, line, right,
	);
	if (roll.visibility === "secret") {
		row.append(el("i", { class: "dice-row-lock", attrs: { title: "Secret — only the DM's window shows this" }, text: "secret" }));
	}
	const at = roll.created_at ? new Date(roll.created_at).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" }) : "";
	row.append(el("time", { class: "dice-row-time", text: at }));
	return row;
}

function appendFeedRow(roll) {
	const feed = $("dice-feed");
	if (!feed) return;
	const empty = feed.querySelector(".dice-feed-empty");
	if (empty) empty.remove();
	feed.prepend(feedRow(roll));
	while (feed.children.length > 120) feed.lastChild.remove();
}

function renderFeed(rolls) {
	const feed = clear($("dice-feed"));
	if (!rolls.length) {
		feed.append(el("p", { class: "camp-status dice-feed-empty", text: "No rolls yet — the first one is yours." }));
		return;
	}
	for (const roll of rolls) feed.append(feedRow(roll));
}

/* ---------- rolling ---------- */

async function submitRoll(e) {
	e.preventDefault();
	const input = $("dice-formula");
	const formula = input.value.trim();
	if (!formula) {
		input.focus();
		return;
	}
	input.value = "";
	try {
		const data = await api.diceRoll(campaignID, {
			formula,
			mode,
			visibility: secret ? "secret" : "public",
			context: $("dice-context").value,
			detail: $("dice-detail").value.trim(),
			character_id: standing.dm && quickCharacter ? quickCharacter : "",
		});
		$("dice-detail").value = "";
		const roll = data.roll;
		if (!seen.has(roll.id)) {
			seen.add(roll.id);
			cursor = Math.max(cursor, roll.seq);
			appendFeedRow(roll);
		}
		playStage(roll);
		announceCurtain(roll);
	} catch (err) {
		renderMeta(err.message, true);
		$("dice-formula").value = formula;
	}
}

// performRoll is the composer's door: `/r 3d6+2` posts through the same
// surface and gets the same curtain. Returns the roll for inline
// rendering by whoever called.
export async function performRoll(cid, formula, opts = {}) {
	const data = await api.diceRoll(cid, { formula, ...opts });
	if (campaignID === cid && !seen.has(data.roll.id)) {
		seen.add(data.roll.id);
		cursor = Math.max(cursor, data.roll.seq);
		appendFeedRow(data.roll);
	}
	return data.roll;
}

// parseRollCommand recognizes the composer's slash spelling: /r or /roll
// followed by a formula. Everything else is not ours.
export function parseRollCommand(text) {
	const m = /^\s*\/(r|roll)\s+([^\s]+)(?:\s|$)/i.exec(text || "");
	if (!m) return null;
	return { formula: m[2], rest: text.slice(m[0].length).trim() };
}

/* ---------- quick rolls ---------- */

async function loadQuickRolls() {
	const box = $("dice-quickrolls");
	if (!box) return;
	clear(box);
	box.hidden = true;

	if (!standing.dm) {
		// A player's chips are their bound character's — the server says
		// whose sheet this caller may read, and it is exactly one.
		if (!standing.character_id) return;
		await appendQuickRolls(box, standing.character_id, false);
		return;
	}

	// The DM's chips: pick a pc from the party, roll as them.
	const data = await api.campaignEntities(campaignID, "pc");
	const pcs = data.entities || [];
	if (!pcs.length) return;
	box.hidden = false;
	const pick = el("select", { class: "dice-pc-picker", attrs: { "aria-label": "Roll as" } });
	pick.append(el("option", { text: "roll as…", attrs: { value: "" } }));
	for (const pc of pcs) pick.append(el("option", { text: pc.name, attrs: { value: pc.id } }));
	pick.value = quickCharacter;
	pick.addEventListener("change", async () => {
		quickCharacter = pick.value;
		const chips = box.querySelector(".dice-chips");
		if (chips) chips.remove();
		if (quickCharacter) await appendQuickRolls(box, quickCharacter, true);
	});
	box.append(el("span", { class: "dice-quickrolls-label", text: "quick rolls" }), pick);
	if (quickCharacter) await appendQuickRolls(box, quickCharacter, true);
}

async function appendQuickRolls(box, characterID, asDM) {
	try {
		const data = await api.characterSheet(campaignID, characterID);
		const chips = el("div", { class: "dice-chips" });
		for (const q of data.quick_rolls || []) {
			chips.append(el("button", {
				class: "chip dice-chip",
				text: q.label,
				attrs: { type: "button", "data-formula": q.formula, title: q.formula },
			}));
		}
		if (!chips.children.length) return;
		box.append(chips);
		box.hidden = false;
	} catch (_) { /* a sheet nobody may read simply offers no chips */ }
}

// rollCard is the inline rendering for surfaces that borrowed the roller
// — the composer's /r, later the tracker's buttons: the dice, the total,
// one line of provenance. It plays the same settle animation in miniature.
export function rollCard(roll) {
	const dice = miniDice(roll.dice || []);
	const line = el("span", { class: "dice-row-line" });
	line.append(el("b", { class: "dice-row-who", text: roll.character_name || roll.actor_name || "the table" }));
	line.append(el("span", { class: "dice-row-formula", text: ` ${roll.formula.replace(/\s+/g, "")}` }));
	if (roll.detail) line.append(el("span", { class: "dice-row-detail", text: ` — ${roll.detail}` }));
	const total = el("b", { class: "dice-row-total", text: String(roll.total) });
	if (!prefersReduced()) total.classList.add("is-stamping");
	const card = el("article", {
		class: "dice-row dice-card" + (roll.visibility === "secret" ? " is-secret" : ""),
	},
		dice, line, el("span", { class: "dice-row-side" },
			total,
			roll.natural_20 ? el("i", { class: "dice-flare is-20", text: "nat 20" }) : null,
			roll.natural_1 ? el("i", { class: "dice-flare is-1", text: "nat 1" }) : null,
		),
	);
	return card;
}

/* ---------- the window ---------- */

function wire() {
	$("dice-campaign").addEventListener("change", onPickCampaign);
	$("dice-bar").addEventListener("submit", submitRoll);
	$("dice-denoms").addEventListener("click", (e) => {
		const btn = e.target.closest("[data-die]");
		if (!btn) return;
		const input = $("dice-formula");
		input.value = (input.value.trim() + " " + btn.dataset.die).trim();
		input.focus();
	});
	$("dice-quickrolls").addEventListener("click", (e) => {
		const chip = e.target.closest("[data-formula]");
		if (!chip) return;
		$("dice-formula").value = chip.dataset.formula;
		$("dice-formula").focus();
	});
	$("dice-adv").addEventListener("click", () => toggleMode("advantage"));
	$("dice-dis").addEventListener("click", () => toggleMode("disadvantage"));
	$("dice-secret").addEventListener("click", () => {
		secret = !secret;
		$("dice-secret").setAttribute("aria-pressed", String(secret));
		$("dice-secret").classList.toggle("is-on", secret);
	});
	$("dice-formula").addEventListener("keydown", (e) => {
		if (e.key === "Enter") {
			e.preventDefault();
			$("dice-bar").requestSubmit();
		}
	});
}

function toggleMode(next) {
	mode = mode === next ? "" : next;
	$("dice-adv").setAttribute("aria-pressed", String(mode === "advantage"));
	$("dice-dis").setAttribute("aria-pressed", String(mode === "disadvantage"));
	$("dice-adv").classList.toggle("is-on", mode === "advantage");
	$("dice-dis").classList.toggle("is-on", mode === "disadvantage");
}

async function onPickCampaign() {
	const id = $("dice-campaign").value;
	if (!id || id === campaignID) return;
	campaignID = id;
	localStorage.setItem("grimoire-dice-campaign", id);
	await loadDice();
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
	const sel = clear($("dice-campaign"));
	if (!campaigns.length) {
		sel.append(el("option", { text: "No campaigns yet…", attrs: { value: "" } }));
		$("dice-body").hidden = true;
		$("dice-empty").hidden = false;
		return;
	}
	$("dice-body").hidden = false;
	$("dice-empty").hidden = true;
	for (const c of campaigns) sel.append(el("option", { text: c.name, attrs: { value: c.id } }));
	const stored = localStorage.getItem("grimoire-dice-campaign");
	if (campaigns.some((c) => c.id === stored)) campaignID = stored;
	else if (!campaigns.some((c) => c.id === campaignID)) campaignID = campaigns[0].id;
	sel.value = campaignID;
	await loadDice();
}

async function loadDice() {
	if (!campaignID) return;
	let data;
	try {
		data = await api.diceFeed(campaignID, 0, 50);
	} catch (err) {
		renderMeta(err.message, true);
		return;
	}
	seen = new Set();
	for (const roll of data.rolls || []) seen.add(roll.id);
	cursor = data.latest || 0;
	standing = data.standing || { dm: true };
	$("dice-secret").hidden = !standing.dm;
	if (!standing.dm) {
		secret = false;
		$("dice-secret").setAttribute("aria-pressed", "false");
		$("dice-secret").classList.remove("is-on");
	}
	renderFeed(data.rolls || []);
	renderMeta(standing.dm
		? "DM — the whole feed, secret rolls included. A secret roll never reaches a player's window."
		: "Player — the table's shared rolls, live.");
	await loadQuickRolls();
	startStream();
}

function renderMeta(message, warn) {
	const meta = $("dice-meta");
	meta.textContent = message || "";
	meta.classList.toggle("warn", !!warn);
}

/* ---------- boot: the curtain follows the campaign without this window ---------- */

// initDice runs at app boot (app.js): it picks the campaign the dice
// window last used (or the first) and opens the shared stream so the
// curtain can drop even with this window closed — the party's moments do
// not depend on anyone having the right window open.
export async function initDice() {
	try {
		const data = await api.campaignList();
		const list = data.campaigns || [];
		if (!list.length) return;
		const stored = localStorage.getItem("grimoire-dice-campaign");
		campaignID = list.some((c) => c.id === stored) ? stored : list[0].id;
		startStream();
	} catch (_) { /* logged out or offline — the window will sort itself out */ }
}

/* ---------- the window-manager contract ---------- */

export const tool = {
	mount(host) {
		const view = $("dice-view");
		host.append(view);
		view.hidden = false;
		mounted = true;
		if (!wired) {
			wire();
			wired = true;
		}
		if (!campaigns.length) loadCampaigns();
		else {
			const sel = $("dice-campaign");
			if (sel.value !== campaignID) sel.value = campaignID;
			if (!streamCtl) loadDice();
		}
		return {
			destroy() {
				mounted = false;
				// The stream outlives the window: the curtain still drops.
			},
		};
	},
};
