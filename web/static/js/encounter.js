// The D&D encounter builder: a party editor, an SRD monster search, a live
// difficulty verdict, and save/load — layered over the transcript the way the
// study surface is. The verdict is always the server's: the meter calls the
// evaluate endpoint, and saved encounters come back with the difficulty
// recomputed from the stored party and roster, so no client-side XP math can
// drift from the DMG tables.

import { $, el, clear, esc, isNarrow, debounce } from "./dom.js";
import { api } from "./api.js";
import { state } from "./state.js";
import { setCorpus } from "./chat.js";

let party = []; // array of levels, e.g. [3, 3, 3, 2]
let roster = []; // [{ name, cr, xp, count }]
let currentID = null; // saved encounter id, null while unsaved
let evalAbort = null;
let searchAbort = null;

export function initEncounter() {
	$("rail-encounter").addEventListener("click", openEncounter);
	$("encounter-close").addEventListener("click", closeEncounter);

	$("enc-add-party").addEventListener("submit", (e) => {
		e.preventDefault();
		const lvl = parseInt($("enc-level").value, 10);
		if (!Number.isInteger(lvl) || lvl < 1 || lvl > 20) return;
		party.push(lvl);
		$("enc-level").value = "1";
		refresh();
	});
	$("enc-party").addEventListener("click", (e) => {
		const rm = e.target.closest("[data-party-rm]");
		if (!rm) return;
		party.splice(parseInt(rm.dataset.partyRm, 10), 1);
		refresh();
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
		const hit = JSON.parse(add.dataset.add);
		addMonster(hit);
	});

	$("enc-roster").addEventListener("click", (e) => {
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
	});

	$("enc-save").addEventListener("submit", onSave);
	$("enc-new").addEventListener("click", () => {
		party = [];
		roster = [];
		currentID = null;
		$("enc-name").value = "";
		$("enc-delete").hidden = true;
		$("enc-saved").value = "";
		refresh();
	});
	$("enc-delete").addEventListener("click", onDelete);
	$("enc-saved").addEventListener("change", onPickSaved);
	$("enc-ask").addEventListener("click", askSage);
}

/** Open the builder surface. */
export function openEncounter() {
	state.encounterOpen = true;
	if (isNarrow()) $("app").classList.add("rail-hidden");
	$("encounter-view").hidden = false;
	$("main").classList.add("is-encountering");
	loadSavedList();
	refresh();
	$("enc-search-input").focus();
}

/** Drop back to the transcript. */
export function closeEncounter() {
	state.encounterOpen = false;
	if (evalAbort) evalAbort.abort();
	if (searchAbort) searchAbort.abort();
	$("encounter-view").hidden = true;
	$("main").classList.remove("is-encountering");
	$("rail-encounter").focus();
}

/* ---------- Party ---------- */

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
	wrap.append(el("span", { class: "enc-party-sum", text: `${party.length} PC${party.length === 1 ? "" : "s"}` }));
}

/* ---------- Search + roster ---------- */

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
		box.append(el("button", {
			class: "enc-hit",
			attrs: { type: "button", "data-add": esc(JSON.stringify(m)), title: `Add ${m.name} (CR ${m.cr})` },
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

function renderRoster() {
	const wrap = clear($("enc-roster"));
	if (roster.length === 0) {
		wrap.append(el("p", { class: "enc-empty", text: "Nothing fielded — search the bestiary and add monsters." }));
		return;
	}
	roster.forEach((m, i) => {
		wrap.append(el("div", { class: "enc-row" },
			el("span", { class: "enc-row-name", text: `${m.count}× ${m.name}` }),
			el("span", { class: "enc-row-meta", text: `CR ${m.cr} · ${m.count * m.xp} XP` }),
			el("span", { class: "enc-row-ctl" },
				el("button", { class: "enc-step", attrs: { type: "button", "data-step": String(i), "data-delta": "-1" }, text: "−" }),
				el("button", { class: "enc-step", attrs: { type: "button", "data-step": String(i), "data-delta": "1" }, text: "+" }),
				el("button", { class: "enc-step rm", attrs: { type: "button", "data-rm": String(i) }, text: "✕" }))));
	});
}

/* ---------- Verdict ---------- */

function refresh() {
	renderParty();
	renderRoster();
	scheduleEvaluate();
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
const BAND_ORDER = ["Easy", "Medium", "Hard", "Deadly"];

function renderVerdict(v) {
	const band = $("enc-band");
	band.textContent = v.difficulty || (party.length ? "—" : "Add a party");
	band.className = "enc-band " + (BAND_CLASS[v.difficulty] || "none");

	const xp = $("enc-xp");
	if (v.adjusted_xp != null && roster.length > 0) {
		xp.textContent = `${v.adjusted_xp.toLocaleString()} adjusted XP (${v.total_xp.toLocaleString()} × ${v.multiplier})`;
	} else {
		xp.textContent = "";
	}

	// The meter fills toward the Deadly threshold, the last band the DMG
	// defines; above it, the bar simply reads full.
	const fill = $("enc-meter-fill");
	if (v.thresholds && v.thresholds.Deadly) {
		const pct = Math.min(100, Math.round((v.adjusted_xp / v.thresholds.Deadly) * 100));
		fill.style.width = pct + "%";
		fill.className = "enc-fill " + (BAND_CLASS[v.difficulty] || "none");
		$("enc-meter").hidden = false;
	} else {
		fill.style.width = "0%";
		$("enc-meter").hidden = true;
	}

	const th = clear($("enc-thresholds"));
	if (v.thresholds && party.length > 0) {
		for (const bandName of BAND_ORDER) {
			const t = v.thresholds[bandName];
			if (t == null) continue;
			const margin = v.margins && v.margins[bandName] != null ? v.margins[bandName] : null;
			const note = margin == null ? "" : margin > 0
				? `${margin.toLocaleString()} under`
				: margin < 0 ? `${(-margin).toLocaleString()} over` : "exactly at";
			th.append(el("span", {
				class: "enc-th" + (v.difficulty === bandName ? " hit" : ""),
				text: `${bandName} ${t.toLocaleString()}` + (note ? ` · ${note}` : ""),
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
		party = enc.party || [];
		roster = enc.monsters || [];
		$("enc-name").value = enc.name || "";
		$("enc-delete").hidden = false;
		renderVerdict(enc.verdict || {});
		refresh();
	} catch (err) {
		$("encounter-meta").textContent = `Could not load: ${err.message}`;
	}
}

async function onSave(e) {
	e.preventDefault();
	const name = $("enc-name").value.trim();
	if (!name) {
		$("encounter-meta").textContent = "Name the encounter before saving.";
		return;
	}
	if (party.length === 0 && roster.length === 0) {
		$("encounter-meta").textContent = "Nothing to save — add a party or monsters first.";
		return;
	}
	try {
		const data = await api.saveEncounter(currentID, name, party, roster);
		const enc = data.encounter;
		currentID = enc.id;
		$("enc-delete").hidden = false;
		$("encounter-meta").textContent = `Saved “${enc.name}” · ${enc.verdict.difficulty || "no verdict yet"}`;
		await loadSavedList();
		$("enc-saved").value = currentID;
	} catch (err) {
		$("encounter-meta").textContent = `Save failed: ${err.message}`;
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
		$("encounter-meta").textContent = "Add a party and monsters before consulting the sage.";
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
