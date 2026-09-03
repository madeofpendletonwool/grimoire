// The D&D encounter builder. The old surface asked the DM to already know
// which monsters they wanted; this one asks for a table, a difficulty and —
// optionally — a mood, and hands back a written encounter: a name, a roster
// of real SRD statblocks, terrain, tactics, a twist and scaling advice. The
// DM's vision drives it (the idea box is the brief, and "Revise" argues with
// the result), but nothing is required to get started.
//
// The server owns everything that must be right: the DMG budget, the XP math,
// and the shortlist of creatures the model is allowed to use. The verdict on
// screen is always the server's, recomputed from the party and roster.
//
// A DM who runs a campaign can prefill the party boxes from its declared
// party block (MAD-378) — the campaign is a prefill and a save scope, never
// a requirement; a table with no campaign sees exactly the old surface.

import { $, el, clear, isNarrow, debounce } from "./dom.js";
import { api, streamEncounterDesign } from "./api.js";
import { state } from "./state.js";
import { setCorpus } from "./chat.js";
import { renderAnswer } from "./render.js";

let party = [3, 3, 3, 3]; // character levels
let roster = []; // [{ name, cr, xp, count }]
let notes = ""; // the design write-up, Markdown
let band = "Medium"; // target difficulty
let objective = { kind: "defeat", rounds: 0, focus: "" }; // what the fight is about
let terrain = null; // the battlefield generated with the objective, { features, hazards }
let title = ""; // the encounter's name from the designer
let currentID = null; // saved encounter id, null while unsaved
let campaignID = ""; // the campaign the builder is designing against, "" for none
let partyFromCampaign = false; // the boxes still hold the campaign's declared party
let designing = false;
let evalAbort = null;
let tacticsAbort = null;
let searchAbort = null;
let designAbort = null;
let budgetAbort = null;
const statblocks = new Map(); // name -> creature, cached per session

const BANDS = ["Easy", "Medium", "Hard", "Deadly"];

// The objective kinds, in chip order. Defeat is first and default: a DM who
// wants today's behaviour changes nothing. The rounds value is each kind's
// default round budget, revealed as a parameter when its chip is picked.
const OBJECTIVES = [
	{ kind: "defeat", label: "Defeat" },
	{ kind: "survive", label: "Survive", rounds: 6, roundsLabel: "Hold for how many rounds?" },
	{ kind: "reach", label: "Reach", rounds: 5, roundsLabel: "Rounds before the pursuit closes" },
	{ kind: "protect", label: "Protect", rounds: 5, roundsLabel: "Rounds the ward can take", focus: "What must still be standing?" },
	{ kind: "stop", label: "Stop", rounds: 4, roundsLabel: "Rounds until the clock completes", focus: "What are they stopping?" },
	{ kind: "retrieve", label: "Retrieve", rounds: 5, roundsLabel: "Rounds to get clear once taken", focus: "What is being taken?" },
	{ kind: "escape", label: "Escape", rounds: 5, roundsLabel: "Rounds before the way closes" },
];

// Starters for the DM who genuinely has nothing. They are deliberately vague
// — the point of the designer is that a mood is enough.
const SEEDS = [
	"something creepy in the swamp",
	"a boss fight for the finale",
	"bandits with an actual plan",
	"undead stirring in a crypt",
	"an ambush in a dark tunnel",
	"something circling overhead",
	"a cult mid-ritual",
	"guardians of a sealed door",
];

function wire() {

	renderBands();
	renderObjectives();
	renderSeeds();
	syncPartyInputs();

	$("enc-party-count").addEventListener("input", onQuickParty);
	$("enc-party-level").addEventListener("input", onQuickParty);

	$("enc-objectives").addEventListener("click", (e) => {
		const pick = e.target.closest("[data-objective]");
		if (!pick) return;
		pickObjective(pick.dataset.objective);
	});
	$("enc-obj-rounds").addEventListener("input", () => {
		if (objective.kind === "defeat") return;
		objective.rounds = clampInt($("enc-obj-rounds").value, 1, 20, OBJECTIVES.find((o) => o.kind === objective.kind)?.rounds || 5);
		refreshBudget();
	});
	$("enc-obj-focus").addEventListener("input", () => {
		objective.focus = $("enc-obj-focus").value.trim();
		refreshBudget();
	});

	$("enc-campaign").addEventListener("change", onPickCampaign);
	$("enc-party-from").addEventListener("click", (e) => {
		if (!e.target.closest("[data-party-from-edit]")) return;
		takeBackParty();
	});

	$("enc-add-party").addEventListener("submit", (e) => {
		e.preventDefault();
		const lvl = parseInt($("enc-level").value, 10);
		if (!Number.isInteger(lvl) || lvl < 1 || lvl > 20) return;
		if (party.length >= 12) return;
		party.push(lvl);
		$("enc-level").value = "1";
		stopTrackingCampaignParty();
		syncPartyInputs();
		refresh();
	});
	$("enc-party").addEventListener("click", (e) => {
		const rm = e.target.closest("[data-party-rm]");
		if (!rm) return;
		party.splice(parseInt(rm.dataset.partyRm, 10), 1);
		stopTrackingCampaignParty();
		syncPartyInputs();
		refresh();
	});

	$("enc-bands").addEventListener("click", (e) => {
		const pick = e.target.closest("[data-band]");
		if (!pick) return;
		band = pick.dataset.band;
		renderBands();
		refreshBudget();
	});
	$("enc-seeds").addEventListener("click", (e) => {
		const seed = e.target.closest("[data-seed]");
		if (!seed) return;
		$("enc-idea").value = seed.dataset.seed;
		$("enc-idea").focus();
	});

	$("enc-design-form").addEventListener("submit", (e) => {
		e.preventDefault();
		runDesign(false);
	});
	$("enc-surprise").addEventListener("click", () => {
		$("enc-idea").value = "";
		runDesign(false);
	});
	$("enc-refine-form").addEventListener("submit", (e) => {
		e.preventDefault();
		runDesign(true);
	});

	$("enc-search").addEventListener("submit", (e) => {
		e.preventDefault();
		runSearch($("enc-search-input").value);
	});
	$("enc-search-input").addEventListener("input", debounce(() => {
		runSearch($("enc-search-input").value);
	}, 350));
	$("enc-results").addEventListener("click", (e) => {
		const add = e.target.closest("[data-add]");
		if (!add) return;
		addMonster(JSON.parse(add.dataset.add));
	});

	$("enc-roster").addEventListener("click", onRosterClick);

	$("enc-save").addEventListener("submit", onSave);
	$("enc-new").addEventListener("click", resetBuilder);
	$("enc-copy").addEventListener("click", onCopy);
	$("enc-delete").addEventListener("click", onDelete);
	$("enc-saved").addEventListener("change", onPickSaved);
	$("enc-ask").addEventListener("click", askSage);
}

/** Open the builder surface. */
function openEncounter() {
	loadSavedList();
	loadCampaigns();
	refresh();
	$("enc-idea").focus();
}

/** Drop back to the transcript. */
function closeEncounter() {
	for (const c of [evalAbort, searchAbort, designAbort, budgetAbort]) if (c) c.abort();
}

/* ---------- The brief ---------- */

function renderBands() {
	const wrap = clear($("enc-bands"));
	for (const b of BANDS) {
		wrap.append(el("button", {
			class: "enc-chip enc-band-chip" + (b === band ? " is-on" : ""),
			attrs: { type: "button", "data-band": b, "aria-pressed": b === band },
			text: b,
		}));
	}
}

function renderSeeds() {
	const wrap = clear($("enc-seeds"));
	for (const s of SEEDS) {
		wrap.append(el("button", {
			class: "enc-seed",
			attrs: { type: "button", "data-seed": s, title: `Use “${s}” as the brief` },
			text: s,
		}));
	}
}

/* ---------- The objective ---------- */

// The chip row beside the difficulty chips. Defeat stays selected by default,
// so a DM who wants today's behaviour changes nothing; picking another kind
// reveals its one or two parameters and re-aims the budget readout.
function renderObjectives() {
	const wrap = clear($("enc-objectives"));
	for (const o of OBJECTIVES) {
		const on = o.kind === (objective.kind || "defeat");
		wrap.append(el("button", {
			class: "enc-chip enc-band-chip" + (on ? " is-on" : ""),
			attrs: { type: "button", "data-objective": o.kind, "aria-pressed": on },
			text: o.label,
		}));
	}
	syncObjectiveParams();
}

function pickObjective(kind) {
	if (objective.kind === kind) return;
	const meta = OBJECTIVES.find((o) => o.kind === kind) || OBJECTIVES[0];
	objective = { kind: meta.kind, rounds: meta.rounds || 0, focus: "" };
	terrain = null;
	renderObjectives();
	refreshBudget();
}

// The one or two parameters a kind carries — rounds, and what the fight is
// over. Defeat has none, so the row stays hidden.
function syncObjectiveParams() {
	const meta = OBJECTIVES.find((o) => o.kind === (objective.kind || "defeat")) || OBJECTIVES[0];
	const params = $("enc-objective-params");
	params.hidden = meta.kind === "defeat";
	if (meta.kind === "defeat") return;
	$("enc-obj-rounds-label").textContent = meta.roundsLabel || "Rounds";
	$("enc-obj-rounds").value = String(objective.rounds || meta.rounds || 5);
	const focusRow = $("enc-obj-focus-row");
	focusRow.hidden = !meta.focus;
	if (meta.focus) {
		$("enc-obj-focus-label").textContent = meta.focus;
		$("enc-obj-focus").value = objective.focus || "";
	}
}

// What the client sends: the declared kind, its clock when it has one, and
// the thing the fight is over when the kind wants one. The server fills
// every default it does not receive.
function objectivePayload() {
	if (!objective.kind || objective.kind === "defeat") return { kind: "defeat" };
	const meta = OBJECTIVES.find((o) => o.kind === objective.kind) || OBJECTIVES[0];
	const out = { kind: objective.kind };
	if (meta.rounds) out.rounds = objective.rounds || meta.rounds;
	if (meta.focus) out.focus = objective.focus || "";
	return out;
}

// The two number boxes are the fast path: they describe a party of identical
// levels, which is what most tables are. Editing them rebuilds the party;
// adding a mismatched level under "Mixed levels" blanks the level box so the
// two views never claim different things.
function onQuickParty() {
	const count = clampInt($("enc-party-count").value, 1, 12, 4);
	const level = clampInt($("enc-party-level").value, 1, 20, 3);
	party = Array.from({ length: count }, () => level);
	stopTrackingCampaignParty();
	renderParty();
	renderPartySum();
	refresh();
}

function syncPartyInputs() {
	$("enc-party-count").value = String(party.length || 1);
	const uniform = party.length > 0 && party.every((l) => l === party[0]);
	$("enc-party-level").value = uniform ? String(party[0]) : "";
	renderParty();
	renderPartySum();
}

function renderPartySum() {
	const n = party.length;
	const avg = n ? (party.reduce((a, b) => a + b, 0) / n) : 0;
	$("enc-party-sum").textContent = n
		? `${n} PC${n === 1 ? "" : "s"} · avg level ${avg % 1 ? avg.toFixed(1) : avg}`
		: "no party yet";
}

function renderParty() {
	const wrap = clear($("enc-party"));
	if (party.length === 0) {
		wrap.append(el("p", { class: "enc-empty", text: "No party yet — add character levels." }));
		return;
	}
	party.forEach((lvl, i) => {
		wrap.append(el("span", { class: "enc-chip", attrs: { "data-party-rm": String(i), title: "Remove character" } },
			el("span", { class: "enc-chip-lvl", text: `L${lvl}` })));
	});
}

function clampInt(raw, min, max, fallback) {
	const n = parseInt(raw, 10);
	if (!Number.isInteger(n)) return fallback;
	return Math.min(max, Math.max(min, n));
}

/* ---------- The campaign, when there is one ---------- */

// The party boxes are the DM's; a campaign is only ever a prefill (MAD-378).
// Picking one fills the boxes from its declared party block and says where
// the numbers came from; the first manual edit takes them back. Without a
// campaign — or for a DM who has none — the surface is exactly what it
// always was.

async function loadCampaigns() {
	const wrap = $("enc-campaign").closest(".enc-campaign-more");
	try {
		const data = await api.campaignList();
		const mine = (data.campaigns || []).filter((c) => c.my_role === "dm" || c.my_role === "keeper");
		if (mine.length === 0) {
			wrap.hidden = true;
			return;
		}
		const sel = $("enc-campaign");
		const current = sel.value;
		clear(sel);
		sel.append(el("option", {
			attrs: { value: "" },
			text: "No campaign — my own table",
		}));
		for (const c of mine) {
			sel.append(el("option", { attrs: { value: c.id }, text: c.name }));
		}
		sel.value = mine.some((c) => c.id === current) ? current : "";
		wrap.hidden = false;
	} catch (_) {
		// No campaign list, no campaign features — the builder carries on.
		wrap.hidden = true;
	}
}

async function onPickCampaign() {
	campaignID = $("enc-campaign").value;
	stopTrackingCampaignParty();
	if (!campaignID) {
		refresh();
		return;
	}
	try {
		const data = await api.campaignParty(campaignID);
		adoptCampaignParty(data.party || {});
	} catch (_) {
		// Not this campaign's DM, or it went away — the boxes stay the DM's
		// own and the campaign only scopes the save.
	}
	refresh();
}

// adoptCampaignParty takes a party view (levels, label, problems) and, when
// the campaign declares any levels, makes them the table.
function adoptCampaignParty(view) {
	const levels = view.levels || [];
	if (levels.length === 0) {
		stopTrackingCampaignParty();
		return;
	}
	party = levels.slice();
	partyFromCampaign = true;
	syncPartyInputs();
	renderPartyFromLine(view.label || "", view.problems || []);
}

// The provenance line: where the numbers came from, what could not be read,
// and the affordance that takes the table back.
function renderPartyFromLine(label, problems) {
	const line = $("enc-party-from");
	clear(line);
	line.append(document.createTextNode(label || "from your campaign"));
	if (problems.length) {
		line.append(el("span", {
			class: "enc-party-from-warn",
			text: ` · ${problems.length} unreadable key${problems.length === 1 ? "" : "s"}`,
			attrs: { title: problems.map((p) => `${p.name}: ${p.field} — ${p.detail}`).join("\n") },
		}));
	}
	line.append(el("button", {
		class: "enc-party-from-edit",
		attrs: { type: "button", "data-party-from-edit": "" },
		text: "edit",
	}));
	line.hidden = false;
}

// stopTrackingCampaignParty drops the provenance without touching the boxes:
// the numbers are the DM's own now, whatever they started as.
function stopTrackingCampaignParty() {
	partyFromCampaign = false;
	$("enc-party-from").hidden = true;
}

// The edit affordance on the line itself: claim the numbers and go straight
// to the level box, which is the thing a DM editing a uniform table edits.
function takeBackParty() {
	stopTrackingCampaignParty();
	$("enc-party-level").focus();
	refresh();
}

/* ---------- The budget readout ---------- */

// "What fits" is worth showing before anything is built: it turns the
// difficulty chips from an abstract label into a number of monsters. The
// objective rides along, so the readout is the objective-aware aim and the
// "how this ends" block is real before a single monster is placed.
const refreshBudget = debounce(async () => {
	if (budgetAbort) budgetAbort.abort();
	const controller = new AbortController();
	budgetAbort = controller;
	const line = $("enc-budget");
	try {
		const data = await api.encounterBudget(party, band, campaignID, objectivePayload(), controller.signal);
		if (controller.signal.aborted) return;
		const b = data.budget || {};
		const shapes = (b.shapes || []).filter((s) => s.count > 1).slice(0, 3)
			.map((s) => `${s.count} × CR ${s.each_cr}`);
		const parts = [`${(b.target_xp || 0).toLocaleString()} adjusted XP to spend`];
		if (b.max_solo_cr) parts.push(`one monster up to CR ${b.max_solo_cr}`);
		if (shapes.length) parts.push(`or ${shapes.join(", or ")}`);
		line.textContent = parts.join(" · ");
		renderEnding(data.ending, data.terrain || null);
	} catch (err) {
		if (err.name !== "AbortError") line.textContent = "";
	}
}, 250);

/* ---------- Designing ---------- */

async function runDesign(revising) {
	if (designing) return;
	if (party.length === 0) {
		setStatus("Set the table first — how many players, and what level?");
		return;
	}
	const idea = $("enc-idea").value.trim();
	const feedback = revising ? $("enc-feedback").value.trim() : "";
	if (revising && !feedback) {
		setStatus("Say what should change, then press Revise.");
		return;
	}

	if (designAbort) designAbort.abort();
	const controller = new AbortController();
	designAbort = controller;
	designing = true;
	setDesignBusy(true);
	setStatus(revising ? "Reworking the encounter…" : "Consulting the bestiary…");
	if (!revising) {
		title = "";
		$("enc-sheet-title").hidden = true;
		clear($("enc-roster"));
		$("enc-unverified").hidden = true;
	}

	let streamed = "";
	const box = $("enc-notes");
	box.classList.remove("prose");
	box.textContent = "";
	try {
		await streamEncounterDesign({
			idea,
			party,
			difficulty: band,
			objective: objectivePayload(),
			feedback,
			current: revising ? roster : [],
			notes: revising ? notes : "",
			campaign_id: campaignID,
		}, {
			onMeta: (meta) => {
				const n = meta.candidates || 0;
				setStatus(revising
					? `Reworking against ${n} candidate statblocks…`
					: `Choosing from ${n} statblocks that fit the budget…`);
			},
			onDelta: (text) => {
				streamed += text;
				// Plain text while it streams: re-parsing Markdown on every
				// token would rebuild the sheet hundreds of times.
				box.textContent = streamed;
			},
			onDone: (payload) => {
				title = payload.name || title;
				roster = payload.monsters || [];
				notes = payload.notes || streamed;
				if (Array.isArray(payload.party) && payload.party.length) party = payload.party;
				// The objective comes back normalised: the server filled the
				// defaults and its terrain with it.
				objective = payload.objective || { kind: "defeat" };
				terrain = payload.terrain || null;
				syncPartyInputs();
				renderObjectives();
				renderSheet();
				renderEnding(payload.ending, terrain);
				renderTactics(payload.tactics || null);
				renderVerdict(payload.verdict || {});
				renderUnverified(payload.unverified || []);
				if (title && !$("enc-name").value.trim()) $("enc-name").value = title;
				$("enc-refine-btn").disabled = false;
				setStatus(payload.truncated ? "The design was cut short — revise or try again." : "");
			},
			onError: (msg) => setStatus(`Design failed: ${msg}`),
		}, controller.signal);
	} catch (err) {
		if (err.name !== "AbortError") setStatus(`Design failed: ${err.message}`);
	} finally {
		designing = false;
		setDesignBusy(false);
	}
}

function setDesignBusy(busy) {
	$("enc-design-btn").disabled = busy;
	$("enc-surprise").disabled = busy;
	$("enc-refine-btn").disabled = busy || roster.length === 0;
	$("enc-sheet").classList.toggle("is-working", busy);
}

function setStatus(text) {
	$("enc-status").textContent = text || "";
	$("enc-status").hidden = !text;
}

/* ---------- The sheet ---------- */

function renderSheet() {
	const heading = $("enc-sheet-title");
	heading.textContent = title || "";
	heading.hidden = !title;
	renderRoster();
	const box = $("enc-notes");
	if (notes) {
		box.classList.add("prose");
		// The roster is rendered as controls above; leaving the model's own
		// Roster section in the prose would print it twice.
		renderAnswer(box, stripRosterSection(notes), "dnd");
	} else {
		box.classList.remove("prose");
		box.textContent = "";
	}
}

// stripRosterSection drops the machine-parsed block from the displayed prose,
// keeping every other section intact.
function stripRosterSection(md) {
	const lines = md.split("\n");
	const out = [];
	let skipping = false;
	for (const line of lines) {
		const heading = /^#{1,6}\s+(.*)$/.exec(line);
		if (heading) {
			skipping = heading[1].trim().replace(/[*_:\s]/g, "").toLowerCase() === "roster";
			if (skipping) continue;
		}
		if (!skipping) out.push(line);
	}
	return out.join("\n").trim();
}

function renderUnverified(names) {
	const flag = $("enc-unverified");
	if (!names.length) {
		flag.hidden = true;
		return;
	}
	flag.hidden = false;
	flag.textContent = `Not in the SRD, so left out: ${names.join(", ")}.`;
}

// "How this ends": success, failure, and the clock if there is one, plus the
// terrain the fight is generated with. Every word of it is server arithmetic
// or a server-declared effect — the block is a readout, not the model's.
function renderEnding(ending, terr) {
	const node = $("enc-ending");
	if (!ending || ending.kind === "defeat") {
		node.hidden = true;
		return;
	}
	clear(node);
	node.append(el("h4", { class: "enc-ending-title", text: `How this ends — ${ending.label}` }));
	const line = (label, text) => node.append(el("p", { class: "enc-ending-line" },
		el("strong", { text: `${label} ` }), document.createTextNode(text)));
	if (ending.clock) line("Clock:", ending.clock);
	line("Success:", ending.success);
	line("Failure:", ending.failure);
	if (terr && (terr.features?.length || terr.hazards?.length)) {
		const list = el("ul", { class: "enc-ending-terrain" });
		for (const f of terr.features || []) {
			list.append(el("li", {},
				el("strong", { text: featureLabel(f.kind) }),
				document.createTextNode(` — ${f.effect}${f.area ? ` (${f.area})` : ""}`)));
		}
		for (const h of terr.hazards || []) {
			list.append(el("li", {},
				el("strong", { text: `Hazard: ${h.name}` }),
				document.createTextNode(
					` — DC ${h.dc} ${h.save_ability.toUpperCase()} save, ${h.damage} ${h.damage_type} on a failure` +
					`${h.trigger ? `; ${h.trigger}` : ""}${h.area ? ` (${h.area})` : ""}`)));
		}
		node.append(list);
	}
	node.hidden = false;
}

// The server's vocabulary, spelled for people.
function featureLabel(kind) {
	const labels = {
		cover: "Cover", elevation: "Elevation", difficult_ground: "Difficult ground",
		chokepoint: "Chokepoint", concealment: "Concealment", water: "Water", darkness: "Darkness",
	};
	return labels[kind] || kind;
}

/* ---------- The tactical read ---------- */

// What these monsters will do, and to whom — the server's arithmetic, in
// the same spirit as the difficulty gauge: every figure on screen carries
// its derivation, shown on click. The model's tactics prose rides under the
// block only when the gate let it through; a rejected prose is labelled
// with the figures that did not trace.
function renderTactics(block) {
	const node = clear($("enc-tactics"));
	if (!block || !(block.threat || []).length) {
		node.hidden = true;
		return;
	}
	node.hidden = false;
	node.append(el("h4", { class: "enc-ending-title", text: "What they will do" }));

	for (const c of block.caveats || []) {
		node.append(el("p", { class: "enc-tactics-caveat", text: c }));
	}

	const focus = (block.focus || []).filter((f) => f.target || f.reason);
	if (focus.length) {
		node.append(section("Who they want", focus.map((f) => {
			const line = el("p", { class: "enc-tactics-line" },
				el("strong", { text: f.monster }));
			if (f.target) {
				line.append(document.createTextNode(` → ${f.target}`));
				if (f.chance) line.append(figureNode(f.chance, f.mode === "save" ? " fail" : " hit"));
				if (f.per_round) line.append(figureNode(f.per_round, " /rd"));
				if (f.reason) line.append(el("span", { class: "enc-tactics-why", text: ` — ${f.reason}` }));
			} else if (f.reason) {
				line.append(el("span", { class: "enc-tactics-why", text: ` — ${f.reason}` }));
			}
			for (const h of f.holdouts || []) {
				line.append(el("span", { class: "enc-tactics-why", text: ` (${h})` }));
			}
			return line;
		})));
	}

	const at = block.attrition || {};
	const drops = [...(at.party || []).map((d) => ({ pc: d.pc, rounds: d.rounds, aimed: d.aimed_at })),
		...(at.monsters || []).map((d) => ({ pc: d.monster, rounds: d.rounds, aimed: d.answer, monster: d.monster, hopeless: d.hopeless }))];
	if (drops.length) {
		node.append(section("Rounds to drop", drops.map((d) => {
			const line = el("p", { class: "enc-tactics-line" },
				d.monster ? el("strong", { text: d.monster }) : document.createTextNode(d.pc));
			line.append(document.createTextNode(": "));
			if (d.rounds) {
				line.append(figureNode(d.rounds, " rounds"));
				if (d.aimed) line.append(document.createTextNode(" under "));
				if (d.aimed) line.append(figureNode(d.aimed, " /rd"));
			} else {
				line.append(el("span", { class: "enc-tactics-warn", text: "nothing the party brings touches it" }));
			}
			return line;
		})));
	}

	if ((block.counterplay || []).length) {
		node.append(section("Counterplay", block.counterplay.map((c) => el("p", { class: "enc-tactics-line" },
			el("strong", { text: [c.monster, c.pc].filter(Boolean).join(" × ") }),
			document.createTextNode(` — ${c.detail}`)))));
	}

	if ((block.spotlight || []).length) {
		node.append(section("The spotlight check", block.spotlight.map((s) => {
			const line = el("p", { class: "enc-tactics-line" }, el("strong", { text: s.pc }));
			line.append(document.createTextNode(": "));
			if (s.threat_share) {
				line.append(document.createTextNode("faces "));
				line.append(figureNode(s.threat_share, " of the threat"));
			}
			if (s.answer_share) {
				if (s.threat_share) line.append(document.createTextNode(", "));
				line.append(document.createTextNode("supplies "));
				line.append(figureNode(s.answer_share, " of the answer"));
			}
			if (s.benched) {
				line.append(el("span", { class: "enc-tactics-warn", text: ` benched — ${s.reason}` }));
			}
			return line;
		})));
	}

	const mv = block.movement;
	if (mv) {
		const line = el("p", { class: "enc-tactics-line" }, el("strong", { text: `Movement matters: ${mv.label}` }));
		line.append(document.createTextNode(" "));
		line.append(figureNode(mv.score, " /100"));
		if ((mv.drivers || []).length) {
			line.append(el("span", { class: "enc-tactics-why", text: ` — ${mv.drivers.join(", ")}` }));
		}
		node.append(section("The ground", [line]));
	}

	const prose = block.prose;
	if (prose && prose.prose) {
		if (prose.prose_status === "rejected") {
			const names = (prose.violations || []).map((v) => v.token).join(", ");
			node.append(el("p", {
				class: "enc-tactics-warn",
				text: `The model's tactics prose was rejected: ${names || "a figure"} traces to no server arithmetic. The read above is the derived one.`,
			}));
		} else {
			node.append(el("p", { class: "enc-tactics-prose", text: prose.prose }));
		}
	}

	function section(title, lines) {
		const sec = el("div", { class: "enc-tactics-sec" });
		sec.append(el("h5", { class: "enc-tactics-h", text: title }));
		for (const line of lines) sec.append(line);
		return sec;
	}
}

// figureNode is one number plus its trace: click to show the arithmetic
// that produced it, the way the difficulty gauge shows its multiplier.
function figureNode(f, suffix) {
	const wrap = el("span", { class: "enc-fig", attrs: { role: "button", tabindex: "0", title: "Show the arithmetic" } },
		el("strong", { text: fmtFig(f.Value) }),
		suffix || f.Unit ? el("span", { class: "enc-fig-unit", text: suffix || (f.Unit ? ` ${f.Unit}` : "") }) : null);
	const how = el("span", { class: "enc-fig-how", text: f.How });
	wrap.append(how);
	wrap.addEventListener("click", () => wrap.classList.toggle("is-open"));
	return wrap;
}

function fmtFig(v) {
	const n = Math.round(v * 10) / 10;
	return Number.isInteger(n) ? n.toLocaleString() : n.toLocaleString(undefined, { maximumFractionDigits: 1 });
}

function renderRoster() {
	const wrap = clear($("enc-roster"));
	if (roster.length === 0) {
		wrap.append(el("p", {
			class: "enc-empty",
			text: "Nothing fielded yet — describe what you want (or don't) and build one.",
		}));
		return;
	}
	roster.forEach((m, i) => {
		const open = statblocks.has(m.name) && statblocks.get(m.name).open;
		wrap.append(el("div", { class: "enc-row" + (open ? " is-open" : "") },
			el("div", { class: "enc-row-head" },
				el("button", {
					class: "enc-row-name",
					attrs: { type: "button", "data-block": m.name, "aria-expanded": open, title: `Statblock for ${m.name}` },
				},
				el("span", { class: "enc-row-count", text: `${m.count}×` }),
				el("span", { class: "enc-row-label", text: m.name })),
				el("span", { class: "enc-row-meta", text: `CR ${m.cr} · ${(m.count * m.xp).toLocaleString()} XP` }),
				el("span", { class: "enc-row-ctl" },
					el("button", { class: "enc-step", attrs: { type: "button", "data-step": String(i), "data-delta": "-1", "aria-label": `One fewer ${m.name}` }, text: "−" }),
					el("button", { class: "enc-step", attrs: { type: "button", "data-step": String(i), "data-delta": "1", "aria-label": `One more ${m.name}` }, text: "+" }),
					el("button", { class: "enc-step rm", attrs: { type: "button", "data-rm": String(i), "aria-label": `Remove ${m.name}` }, text: "✕" }))),
			open ? statblockNode(statblocks.get(m.name).creature) : null));
	});
}

function onRosterClick(e) {
	const block = e.target.closest("[data-block]");
	if (block) {
		toggleStatblock(block.dataset.block);
		return;
	}
	const step = e.target.closest("[data-step]");
	if (step) {
		adjustCount(parseInt(step.dataset.step, 10), parseInt(step.dataset.delta, 10));
		return;
	}
	const rm = e.target.closest("[data-rm]");
	if (rm) {
		roster.splice(parseInt(rm.dataset.rm, 10), 1);
		refresh();
	}
}

async function toggleStatblock(name) {
	const cached = statblocks.get(name);
	if (cached) {
		cached.open = !cached.open;
		renderRoster();
		return;
	}
	statblocks.set(name, { creature: null, open: true });
	renderRoster();
	try {
		const data = await api.encounterStatblock(name);
		statblocks.set(name, { creature: data.creature || null, open: true });
	} catch (_) {
		statblocks.set(name, { creature: null, open: true });
	}
	renderRoster();
}

// statblockNode renders the SRD entry the way a statblock reads: the line of
// defences, then the traits and actions with their real text.
function statblockNode(c) {
	if (!c) return el("div", { class: "enc-block", text: "Fetching the statblock…" });
	const line = [c.size, c.type, c.alignment].filter(Boolean).join(" · ");
	const speeds = c.speeds
		? Object.entries(c.speeds).map(([k, v]) => `${k} ${v} ft.`).join(", ")
		: "";
	const node = el("div", { class: "enc-block" },
		line ? el("p", { class: "enc-block-line", text: line }) : null,
		el("p", { class: "enc-block-stats", text: statLine(c) }),
		speeds ? el("p", { class: "enc-block-stats", text: `Speed ${speeds}` }) : null,
		c.senses ? el("p", { class: "enc-block-stats", text: `Senses ${c.senses}` }) : null,
		defenceLine(c),
		c.tags && c.tags.length ? el("p", { class: "enc-block-tags", text: c.tags.join(" · ") }) : null);
	for (const t of c.traits || []) {
		node.append(el("p", { class: "enc-block-entry" },
			el("strong", { text: t.name + ". " }), document.createTextNode(t.desc || "")));
	}
	for (const a of c.actions || []) {
		const kind = a.kind && a.kind !== "ACTION" ? ` [${a.kind.toLowerCase().replace(/_/g, " ")}]` : "";
		node.append(el("p", { class: "enc-block-entry" },
			el("strong", { text: a.name + kind + ". " }), document.createTextNode(a.desc || "")));
	}
	return node;
}

function statLine(c) {
	const bits = [];
	if (c.ac) bits.push(`AC ${c.ac}`);
	if (c.hp) bits.push(`HP ${c.hp}${c.hit_dice ? ` (${c.hit_dice})` : ""}`);
	bits.push(`CR ${c.cr} · ${(c.xp || 0).toLocaleString()} XP`);
	return bits.join(" · ");
}

function defenceLine(c) {
	const bits = [];
	if (c.resist) bits.push(`Resistant to ${c.resist}`);
	if (c.immune) bits.push(`Immune to ${c.immune}`);
	if (c.vulnerable) bits.push(`Vulnerable to ${c.vulnerable}`);
	if (c.cond_immune) bits.push(`Condition immunities: ${c.cond_immune}`);
	if (!bits.length) return null;
	return el("p", { class: "enc-block-stats", text: bits.join(" · ") });
}

/* ---------- Manual search ---------- */

async function runSearch(q) {
	q = (q || "").trim();
	const box = clear($("enc-results"));
	if (q.length < 2) return;
	box.append(el("p", { class: "enc-empty", text: "Consulting the bestiary…" }));
	if (searchAbort) searchAbort.abort();
	const controller = new AbortController();
	searchAbort = controller;
	try {
		const data = await api.encounterMonsters(q, controller.signal);
		if (controller.signal.aborted) return;
		renderResults(box, q, data.monsters || [], data.warning || "");
	} catch (err) {
		if (err.name === "AbortError") return;
		clear(box);
		box.append(el("p", { class: "enc-empty warn", text: `Search failed: ${err.message}` }));
	}
}

function renderResults(box, q, monsters, warning) {
	clear(box);
	if (warning) {
		box.append(el("p", { class: "enc-empty warn", text: warning }));
		return;
	}
	if (monsters.length === 0) {
		box.append(el("p", { class: "enc-empty", text: `No SRD monsters match “${q}”.` }));
		return;
	}
	for (const m of monsters) {
		// el() sets attributes with setAttribute, which escapes for us — running
		// esc() here too would store "&quot;name&quot;" and JSON.parse would
		// throw on the click, which is what used to break adding by hand.
		box.append(el("button", {
			class: "enc-hit",
			attrs: { type: "button", "data-add": JSON.stringify(m), title: `Add ${m.name} (CR ${m.cr})` },
		},
		el("span", { class: "enc-hit-name", text: m.name }),
		el("span", { class: "enc-hit-meta", text: `CR ${m.cr} · ${m.xp} XP${m.type ? " · " + m.type : ""}` })));
	}
}

function addMonster(hit) {
	const found = roster.find((m) => m.name === hit.name);
	if (found) {
		found.count++;
	} else {
		roster.push({ name: hit.name, cr: hit.cr, xp: hit.xp, count: 1 });
	}
	refresh();
}

function adjustCount(i, delta) {
	const m = roster[i];
	if (!m) return;
	m.count += delta;
	if (m.count < 1) roster.splice(i, 1);
	refresh();
}

/* ---------- Verdict ---------- */

function refresh() {
	renderParty();
	renderPartySum();
	renderRoster();
	$("enc-refine-btn").disabled = designing || roster.length === 0;
	scheduleEvaluate();
	scheduleTactics();
	refreshBudget();
}

const scheduleEvaluate = debounce(evaluate, 250);
const scheduleTactics = debounce(fetchTactics, 350);

// The tactical read (MAD-381) follows the roster the way the verdict does:
// recomputed by the server on every edit, so what is on screen is the
// current arithmetic, never a stale promise from the last design. With a
// campaign picked, its party block fills the read in.
async function fetchTactics() {
	if (roster.length === 0) {
		renderTactics(null);
		return;
	}
	if (tacticsAbort) tacticsAbort.abort();
	const controller = new AbortController();
	tacticsAbort = controller;
	try {
		const data = await api.encounterTactics(party, roster, campaignID, objectivePayload(), terrain, controller.signal);
		if (controller.signal.aborted) return;
		renderTactics(data.tactics || null);
	} catch (err) {
		if (err.name !== "AbortError") {
			// A failed read keeps the last analysis on screen; the block
			// stays honest rather than flashing empty.
		}
	}
}

async function evaluate() {
	if (evalAbort) evalAbort.abort();
	const controller = new AbortController();
	evalAbort = controller;
	try {
		const data = await api.encounterEvaluate(party, roster, controller.signal);
		if (controller.signal.aborted) return;
		renderVerdict(data.verdict || {});
	} catch (err) {
		if (err.name === "AbortError") return;
		// Validation rejects and outages both leave the meter quiet rather
		// than red: the builder keeps editing.
		renderVerdict({});
	}
}

const BAND_CLASS = { Trivial: "trivial", Easy: "easy", Medium: "medium", Hard: "hard", Deadly: "deadly" };

function renderVerdict(v) {
	const bandEl = $("enc-band");
	bandEl.textContent = v.difficulty || (party.length ? "—" : "Add a party");
	bandEl.className = "enc-band " + (BAND_CLASS[v.difficulty] || "none");
	if (v.difficulty && v.difficulty !== band && roster.length > 0) {
		bandEl.title = `You asked for ${band}`;
	} else {
		bandEl.removeAttribute("title");
	}

	const xp = $("enc-xp");
	if (v.adjusted_xp != null && roster.length > 0) {
		// A survive encounter shows the wave arithmetic: the total across
		// waves is what the party has to survive, and each wave was priced
		// at its own multiplier.
		if (v.waves && v.waves.length > 1) {
			const per = v.waves.map((w) => `${w.total_xp.toLocaleString()} × ${w.multiplier}`).join(" + ");
			xp.textContent = `${v.adjusted_xp.toLocaleString()} adjusted XP across ${v.waves.length} waves (${per})`;
		} else {
			xp.textContent = `${v.adjusted_xp.toLocaleString()} adjusted XP (${v.total_xp.toLocaleString()} × ${v.multiplier})`;
		}
	} else {
		xp.textContent = "";
	}

	// The meter runs to half again the Deadly threshold, so a Deadly
	// encounter reads as "well past the line" rather than pinning at full and
	// telling the DM nothing. The band thresholds sit on it as tick marks.
	const meter = $("enc-meter");
	const fill = $("enc-meter-fill");
	const deadly = v.thresholds && v.thresholds.Deadly;
	if (deadly) {
		const scale = deadly * 1.5;
		const pct = Math.min(100, Math.round(((v.adjusted_xp || 0) / scale) * 100));
		fill.style.width = pct + "%";
		fill.className = "enc-fill " + (BAND_CLASS[v.difficulty] || "none");
		const ticks = clear($("enc-ticks"));
		for (const name of BANDS) {
			const t = v.thresholds[name];
			if (t == null) continue;
			ticks.append(el("span", {
				class: "enc-tick" + (v.difficulty === name ? " hit" : ""),
				attrs: { style: `left:${Math.min(100, (t / scale) * 100)}%` },
			}));
		}
		meter.hidden = false;
	} else {
		fill.style.width = "0%";
		meter.hidden = true;
	}

	const th = clear($("enc-thresholds"));
	if (v.thresholds && party.length > 0) {
		for (const name of BANDS) {
			const t = v.thresholds[name];
			if (t == null) continue;
			const margin = v.margins && v.margins[name] != null ? v.margins[name] : null;
			const note = margin == null ? "" : margin > 0
				? `${margin.toLocaleString()} under`
				: margin < 0 ? `${(-margin).toLocaleString()} over` : "exactly at";
			th.append(el("span", {
				class: "enc-th" + (v.difficulty === name ? " hit" : "") + (name === band ? " asked" : ""),
				text: `${name} ${t.toLocaleString()}` + (note ? ` · ${note}` : ""),
			}));
		}
	}
}

/* ---------- Save / load ---------- */

async function loadSavedList() {
	try {
		const data = await api.listEncounters();
		const sel = $("enc-saved");
		const current = sel.value;
		clear(sel);
		sel.append(el("option", { attrs: { value: "" }, text: "Saved encounters…" }));
		for (const enc of data.encounters || []) {
			sel.append(el("option", { attrs: { value: enc.id }, text: enc.name }));
		}
		sel.value = current;
	} catch (_) { /* the dropdown just stays empty */ }
}

async function onPickSaved() {
	const id = $("enc-saved").value;
	if (!id) return;
	try {
		const data = await api.getEncounter(id);
		const enc = data.encounter;
		if (!enc) return;
		currentID = enc.id;
		party = enc.party && enc.party.length ? enc.party : party;
		roster = enc.monsters || [];
		notes = enc.notes || "";
		title = enc.name || "";
		objective = enc.objective || { kind: "defeat" };
		terrain = enc.terrain || null;
		statblocks.clear();
		// A loaded encounter knows the campaign it belongs to; the picker
		// follows it when that campaign is one of the DM's own.
		if (enc.campaign_id) {
			const sel = $("enc-campaign");
			if ([...sel.options].some((o) => o.value === enc.campaign_id)) {
				campaignID = enc.campaign_id;
				sel.value = campaignID;
			}
		}
		stopTrackingCampaignParty();
		$("enc-name").value = enc.name || "";
		$("enc-delete").hidden = false;
		syncPartyInputs();
		renderObjectives();
		renderSheet();
		renderEnding(enc.ending, terrain);
		renderUnverified([]);
		setStatus("");
		renderVerdict(enc.verdict || {});
		refresh(); // re-derives the tactics read for the loaded roster
	} catch (err) {
		$("encounter-meta").textContent = `Could not load: ${err.message}`;
	}
}

function resetBuilder() {
	roster = [];
	notes = "";
	title = "";
	currentID = null;
	objective = { kind: "defeat", rounds: 0, focus: "" };
	terrain = null;
	statblocks.clear();
	$("enc-name").value = "";
	$("enc-idea").value = "";
	$("enc-feedback").value = "";
	$("enc-delete").hidden = true;
	$("enc-saved").value = "";
	$("enc-refine-btn").disabled = true;
	$("encounter-meta").textContent = "";
	renderUnverified([]);
	setStatus("");
	renderObjectives();
	renderSheet();
	renderEnding(null, null);
	renderTactics(null);
	refresh();
}

async function onSave(e) {
	e.preventDefault();
	const name = $("enc-name").value.trim() || title;
	if (!name) {
		$("encounter-meta").textContent = "Name the encounter before saving.";
		return;
	}
	if (party.length === 0 && roster.length === 0) {
		$("encounter-meta").textContent = "Nothing to save — build or add something first.";
		return;
	}
	try {
		// An update keeps whatever scope the encounter already has; a new
		// save with a campaign picked is the campaign's — the one record a
		// planned fight has, visible to the continuity engine. The objective
		// and its terrain travel with the roster: they are what the fight is
		// about, and the round tracker reads them.
		const data = currentID
			? await api.saveEncounter(currentID, name, party, roster, notes, objectivePayload(), terrain)
			: campaignID
				? await api.saveCampaignEncounter(campaignID, name, party, roster, notes, objectivePayload(), terrain)
				: await api.saveEncounter(null, name, party, roster, notes, objectivePayload(), terrain);
		const enc = data.encounter;
		currentID = enc.id;
		$("enc-name").value = enc.name;
		$("enc-delete").hidden = false;
		$("encounter-meta").textContent = `Saved “${enc.name}” · ${enc.verdict.difficulty || "no verdict yet"}`;
		await loadSavedList();
		$("enc-saved").value = currentID;
	} catch (err) {
		$("encounter-meta").textContent = `Save failed: ${err.message}`;
	}
}

// Copy hands the whole encounter over as plain text, because prep ends up in
// someone's own notes app more often than it stays here.
async function onCopy() {
	if (roster.length === 0 && !notes) {
		$("encounter-meta").textContent = "Nothing to copy yet.";
		return;
	}
	const lines = [];
	if (title) lines.push(`# ${title}`, "");
	lines.push(`Party: ${party.length} characters, levels ${party.join(", ")}`, "");
	if (roster.length) {
		lines.push("## Roster");
		for (const m of roster) lines.push(`${m.count} × ${m.name} (CR ${m.cr}, ${m.xp} XP each)`);
		lines.push("");
	}
	if (objective.kind && objective.kind !== "defeat") {
		const meta = OBJECTIVES.find((o) => o.kind === objective.kind);
		lines.push(`## How this ends — ${meta ? meta.label : objective.kind}`);
		if (objective.rounds) lines.push(`Clock: ${objective.rounds} rounds`);
		if (terrain) {
			for (const f of terrain.features || []) lines.push(`${featureLabel(f.kind)}: ${f.effect}`);
			for (const h of terrain.hazards || []) lines.push(`Hazard — ${h.name}: DC ${h.dc} ${h.save_ability.toUpperCase()}, ${h.damage} ${h.damage_type}`);
		}
		lines.push("");
	}
	if (notes) lines.push(stripRosterSection(notes));
	try {
		await navigator.clipboard.writeText(lines.join("\n"));
		$("encounter-meta").textContent = "Copied.";
	} catch (_) {
		$("encounter-meta").textContent = "Could not reach the clipboard.";
	}
}

async function onDelete() {
	if (!currentID) return;
	try {
		await api.deleteEncounter(currentID);
		currentID = null;
		$("enc-name").value = "";
		$("enc-delete").hidden = true;
		$("encounter-meta").textContent = "Deleted.";
		await loadSavedList();
	} catch (err) {
		$("encounter-meta").textContent = `Delete failed: ${err.message}`;
	}
}

/* ---------- Ask the sage ---------- */

// A deep link into the normal ask flow: switch to the D&D corpus, drop back
// to the transcript, and preload the composer with the encounter as a
// question. The DMG's encounter-building rules are already indexed, so the
// grounded pipeline does the rest.
function askSage() {
	if (party.length === 0 || roster.length === 0) {
		$("encounter-meta").textContent = "Build an encounter before consulting the sage.";
		return;
	}
	const levels = "[" + party.join(", ") + "]";
	const monsters = roster.map((m) => `${m.count}× ${m.name} (CR ${m.cr})`).join(", ");
	const question =
		`Party of levels ${levels} vs ${monsters} — is this encounter deadly, ` +
		`and what adjustments does the DMG suggest?`;
	if (state.corpus !== "dnd") setCorpus("dnd");
	closeEncounter();
	const input = $("composer-input");
	input.value = question;
	input.dispatchEvent(new Event("input", { bubbles: true }));
	input.focus();
}

/* ---------- the window-manager contract ---------- */

// mount() adopts the surface that already exists in index.html rather than
// building one, which is what made migrating nine surfaces tractable: every
// $("enc-party") lookup inside this module keeps working untouched. The
// cost is one window per tool; see the note on `instances` in wm/registry.js.
let wired = false;

export const tool = {
	mount(host) {
		const view = $("encounter-view");
		host.append(view);
		view.hidden = false;
		if (!wired) {
			wire();
			wired = true;
		}
		openEncounter();
		return { destroy: closeEncounter };
	},
};
