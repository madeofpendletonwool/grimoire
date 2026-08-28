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

import { $, el, clear, isNarrow, debounce } from "./dom.js";
import { api, streamEncounterDesign } from "./api.js";
import { state } from "./state.js";
import { setCorpus } from "./chat.js";
import { renderAnswer } from "./render.js";

let party = [3, 3, 3, 3]; // character levels
let roster = []; // [{ name, cr, xp, count }]
let notes = ""; // the design write-up, Markdown
let band = "Medium"; // target difficulty
let title = ""; // the encounter's name from the designer
let currentID = null; // saved encounter id, null while unsaved
let designing = false;
let evalAbort = null;
let searchAbort = null;
let designAbort = null;
let budgetAbort = null;
const statblocks = new Map(); // name -> creature, cached per session

const BANDS = ["Easy", "Medium", "Hard", "Deadly"];

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
	renderSeeds();
	syncPartyInputs();

	$("enc-party-count").addEventListener("input", onQuickParty);
	$("enc-party-level").addEventListener("input", onQuickParty);

	$("enc-add-party").addEventListener("submit", (e) => {
		e.preventDefault();
		const lvl = parseInt($("enc-level").value, 10);
		if (!Number.isInteger(lvl) || lvl < 1 || lvl > 20) return;
		if (party.length >= 12) return;
		party.push(lvl);
		$("enc-level").value = "1";
		syncPartyInputs();
		refresh();
	});
	$("enc-party").addEventListener("click", (e) => {
		const rm = e.target.closest("[data-party-rm]");
		if (!rm) return;
		party.splice(parseInt(rm.dataset.partyRm, 10), 1);
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

// The two number boxes are the fast path: they describe a party of identical
// levels, which is what most tables are. Editing them rebuilds the party;
// adding a mismatched level under "Mixed levels" blanks the level box so the
// two views never claim different things.
function onQuickParty() {
	const count = clampInt($("enc-party-count").value, 1, 12, 4);
	const level = clampInt($("enc-party-level").value, 1, 20, 3);
	party = Array.from({ length: count }, () => level);
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

/* ---------- The budget readout ---------- */

// "What fits" is worth showing before anything is built: it turns the
// difficulty chips from an abstract label into a number of monsters.
const refreshBudget = debounce(async () => {
	if (budgetAbort) budgetAbort.abort();
	const controller = new AbortController();
	budgetAbort = controller;
	const line = $("enc-budget");
	try {
		const data = await api.encounterBudget(party, band, controller.signal);
		if (controller.signal.aborted) return;
		const b = data.budget || {};
		const shapes = (b.shapes || []).filter((s) => s.count > 1).slice(0, 3)
			.map((s) => `${s.count} × CR ${s.each_cr}`);
		const parts = [`${(b.target_xp || 0).toLocaleString()} adjusted XP to spend`];
		if (b.max_solo_cr) parts.push(`one monster up to CR ${b.max_solo_cr}`);
		if (shapes.length) parts.push(`or ${shapes.join(", or ")}`);
		line.textContent = parts.join(" · ");
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
			feedback,
			current: revising ? roster : [],
			notes: revising ? notes : "",
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
				syncPartyInputs();
				renderSheet();
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
	refreshBudget();
}

const scheduleEvaluate = debounce(evaluate, 250);

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
		xp.textContent = `${v.adjusted_xp.toLocaleString()} adjusted XP (${v.total_xp.toLocaleString()} × ${v.multiplier})`;
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
		statblocks.clear();
		$("enc-name").value = enc.name || "";
		$("enc-delete").hidden = false;
		syncPartyInputs();
		renderSheet();
		renderUnverified([]);
		setStatus("");
		renderVerdict(enc.verdict || {});
		refresh();
	} catch (err) {
		$("encounter-meta").textContent = `Could not load: ${err.message}`;
	}
}

function resetBuilder() {
	roster = [];
	notes = "";
	title = "";
	currentID = null;
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
	renderSheet();
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
		const data = await api.saveEncounter(currentID, name, party, roster, notes);
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
