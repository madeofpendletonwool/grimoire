// The play window (MAD-327, stage 3 of MAD-321): the manual Magic
// tracker — board state, event log and current action, all visible at
// once, live over the game's ordinal stream. It is deliberately AI-free:
// the tap surface *is* the correction UI the intent pipeline will need,
// it de-risks the engine before a model touches it, and it is the
// fallback for the moment voice mishears.
//
// The server owns truth. Every value the panes paint comes from the
// folded state GET /api/games/{id} returns; the stream's job is to say
// "an ordinal landed" (and, on rewind, "the log you hold no longer
// exists"), and the panes re-read. A manual edit is just another Action
// through the same POST the engine validates — there is no second write
// path, which is the whole easy-adjust contract.

import { $, el, clear } from "./dom.js";
import { api } from "./api.js";
import {
	seatName, turnLine, formatCount, objectName, ptLine,
	counterChips, commanderTax, zoneTally, defaultActingSeat, canPass,
	describeEvent, lastActionBatch, actionBatchAt, actionSummary, baseCharsFromCard, isType,
} from "./playvm.js";

let games = [];
let gameID = null;
let game = null;
let state = null;
let events = [];          // the log window, ascending by ord
let cursor = 0;           // the last ordinal this client has seen
let seen = new Set();     // event ids — dedupe between POST answers and the stream
let actingOverride = 0;   // the manually picked acting seat; 0 follows priority
let acknowledged = 0;     // the ord the current-action pane was ✓ed at
let editDraft = null;     // {at, from} — ✎ not it, awaiting the corrected submit

// The context strip's subject — the one thing the board pane is "about"
// between taps. It survives re-paints, which happen on every ordinal.
// null hides the strip.
let ctx = null;           // {kind:"object", id} | {kind:"seat", seat, zone}
                           // | {kind:"counter", seat, name} | {kind:"attack"} | {kind:"block"}
let attackDraft = new Set(); // object ids tapped in as attackers
let blockPick = 0;        // the attacker blockers are being assigned to
let blockDraft = new Map();  // attacker id → blocker ids
let cardCache = new Map();   // card name → cardView, this client's own universe
let wired = false;
let mounted = false;
let streamCtl = null;
let refreshTimer = null;
let searchTimer = null;

const LOG_WINDOW = 400;

/* ---------- data ---------- */

async function loadGames(pickID = "") {
	try {
		const data = await api.gameList();
		games = data.games || [];
	} catch (err) {
		renderMeta(err.message, true);
		return;
	}
	const sel = clear($("play-game"));
	if (!games.length) {
		sel.append(el("option", { text: "No games yet…", attrs: { value: "" } }));
		showSetup(null);
		return;
	}
	for (const g of games) {
		const label = `${g.name || "Untitled"} · ${g.status}${g.format && g.format !== "commander" ? ` (${g.format})` : ""}`;
		sel.append(el("option", { text: label, attrs: { value: g.id } }));
	}
	const stored = pickID || localStorage.getItem("grimoire-play-game") || "";
	gameID = games.some((g) => g.id === stored) ? stored : games[0].id;
	sel.value = gameID;
	await openGame(gameID);
}

async function openGame(id) {
	if (!id) return;
	cancelEdit();
	closeContext();
	attackDraft = new Set();
	blockDraft = new Map();
	blockPick = 0;
	actingOverride = 0;
	acknowledged = 0;
	gameID = id;
	localStorage.setItem("grimoire-play-game", id);
	try {
		await reloadGame();
	} catch (err) {
		renderMeta(err.message, true);
		return;
	}
	startStream();
	render();
}

/** Re-read the game row, the folded state and the log window — the full
    honest picture, run at open, after a rewind, and on demand. */
async function reloadGame() {
	const data = await api.gameGet(gameID);
	game = data.game;
	state = data.state || null;
	await reloadLog();
}

/** The log pane's window: the newest LOG_WINDOW rows. */
async function reloadLog() {
	let latest = 0;
	if (state?.last_ord) latest = state.last_ord;
	else if (game?.latest_ord) latest = game.latest_ord;
	const after = Math.max(0, latest - LOG_WINDOW);
	const data = await api.gameEvents(gameID, after, LOG_WINDOW);
	events = data.events || [];
	seen = new Set(events.map((e) => e.id));
	cursor = data.latest || (events.length ? events[events.length - 1].ord : 0);
}

/** The stream said something landed; re-read the fold (debounced — a
    combat action lands a dozen rows at once and they are one paint). */
function scheduleRefresh() {
	clearTimeout(refreshTimer);
	refreshTimer = setTimeout(async () => {
		try {
			const data = await api.gameGet(gameID);
			game = data.game;
			state = data.state || null;
			// The fold may include rows the window has not delivered yet;
			// widen honestly rather than dedupe by luck.
			if (state?.last_ord && state.last_ord > cursor) {
				const win = await api.gameEvents(gameID, cursor, LOG_WINDOW);
				for (const ev of win.events || []) {
					if (!seen.has(ev.id)) {
						seen.add(ev.id);
						events.push(ev);
					}
				}
				cursor = Math.max(cursor, win.latest || 0);
			}
			render();
		} catch (_) { /* the next wake retries; the panes keep the last good paint */ }
	}, 90);
}

/* ---------- the ordinal stream ---------- */

function startStream() {
	stopStream();
	if (!gameID) return;
	streamCtl = { game: gameID };
	const ctl = streamCtl;

	const connect = () => {
		if (!streamCtl || streamCtl.game !== gameID) return;
		const es = new EventSource(`/api/games/${encodeURIComponent(gameID)}/stream?after=${cursor}`);
		ctl.es = es;
		es.addEventListener("open", () => { ctl.failures = 0; });
		es.addEventListener("event", (ev) => {
			let payload;
			try { payload = JSON.parse(ev.data); } catch (_) { return; }
			const row = payload.event;
			if (!row || seen.has(row.id)) return;
			seen.add(row.id);
			events.push(row);
			cursor = Math.max(cursor, row.ord);
			scheduleRefresh();
		});
		es.addEventListener("rewind", () => {
			// The log this client holds no longer exists. Drop everything
			// and re-read — folding stale rows would be worse than a blank.
			events = [];
			seen = new Set();
			cursor = 0;
			cancelEdit();
			closeContext();
			reloadGame().then(render).catch(() => { /* the next wake retries */ });
		});
		es.onerror = () => {
			es.close();
			if (!streamCtl || streamCtl.game !== gameID) return;
			ctl.failures = (ctl.failures || 0) + 1;
			if (ctl.failures > 8) return;
			setTimeout(async () => {
				connect();
				try {
					const win = await api.gameEvents(gameID, cursor, LOG_WINDOW);
					let fresh = false;
					for (const ev of win.events || []) {
						if (seen.has(ev.id)) continue;
						seen.add(ev.id);
						events.push(ev);
						fresh = true;
					}
					cursor = Math.max(cursor, win.latest || 0);
					if (fresh) scheduleRefresh();
				} catch (_) { /* the reconnect brought it */ }
			}, 2500);
		};
	};
	connect();
}

function stopStream() {
	if (streamCtl?.es) streamCtl.es.close();
	streamCtl = null;
	clearTimeout(refreshTimer);
}

/* ---------- the writer ---------- */

/** Submit an action; the answer paints immediately (the engine already
    validated it before a row was written) and the stream wakes every
    other client. A rejection answers 400 and writes nothing; the pane
    says why. */
async function submit(action) {
	const seat = actingSeat();
	if (!action.seat) action.seat = seat;
	if (!action.source) action.source = "tap";
	try {
		const data = await api.gameAction(gameID, action);
		absorb(data);
		render();
	} catch (err) {
		currentMeta(err.message, true);
	}
}

/** Fold a writer answer into the local picture: its events (deduped
    against the stream) and its state (already folded server-side). */
function absorb(data) {
	if (data?.state) state = data.state;
	for (const ev of data?.events || []) {
		if (!seen.has(ev.id)) {
			seen.add(ev.id);
			events.push(ev);
		}
		cursor = Math.max(cursor, ev.ord);
	}
}

/** Rewind truncates the log past an ordinal; the refolded state comes
    back in the answer and the stream announces the rewind to everyone. */
async function rewindTo(ord) {
	try {
		const data = await api.gameRewind(gameID, ord);
		state = data.state || state;
		await reloadLog();
		cancelEdit();
		render();
	} catch (err) {
		currentMeta(err.message, true);
	}
}

/** Rotate priority to a seat: each pass is a real PASS_PRIORITY by the
    current holder, in CR rotation — the only honest way priority moves.
    "Give priority →" and the stack's resolve use it. */
async function rotatePriorityTo(target) {
	const guard = (state?.order || []).length + 2;
	for (let i = 0; i < guard; i++) {
		if (!state?.priority_seat || state.priority_seat === target) break;
		try {
			absorb(await api.gameAction(gameID, { kind: "PASS_PRIORITY", seat: state.priority_seat, source: "tap" }));
		} catch (err) {
			currentMeta(err.message, true);
			break;
		}
	}
	render();
}

/** Resolve the stack top: everyone passes in rotation until the engine
    resolves it (CR 117.3b/117.4 — the engine does the resolving). */
async function resolveTop() {
	const before = state?.stack?.length || 0;
	const guard = (state?.order || []).length + 2;
	for (let i = 0; i < guard && (state?.stack?.length || 0) > 0; i++) {
		try {
			absorb(await api.gameAction(gameID, { kind: "PASS_PRIORITY", seat: state.priority_seat, source: "tap" }));
		} catch (err) {
			currentMeta(err.message, true);
			break;
		}
	}
	if (before > 0 && (state?.stack?.length || 0) === before) {
		currentMeta("the stack did not resolve — does anyone hold priority?", true);
	}
	render();
}

/* ---------- acting seat ---------- */

function actingSeat() {
	if (actingOverride && state?.seats?.[actingOverride]) return actingOverride;
	return defaultActingSeat(state) || (state?.order ? state.order[0] : 0);
}

/* ---------- rendering ---------- */

function render() {
	if (!mounted) return;
	renderSetup();
	renderStrip();
	renderBoard();
	renderLog();
	renderCurrent();
	renderComposerTargets();
	renderMeta();
}

function renderMeta(message, warn) {
	const meta = $("play-meta");
	if (message != null) {
		meta.textContent = message;
		meta.classList.toggle("warn", !!warn);
		return;
	}
	if (!game) {
		meta.textContent = "";
		return;
	}
	if (state?.status === "active") meta.textContent = "Tap a value to change it — every edit is an action on the log.";
	else if (state?.status === "finished") meta.textContent = "Finished. Rewind any log entry to reopen it.";
	else meta.textContent = "Seat the table, then start the game.";
}

function showSetup(g) {
	$("play-setup").hidden = !g || g.status !== "setup";
	$("play-body").hidden = !$("play-setup").hidden;
}

function renderSetup() {
	if (!game) {
		showSetup(null);
		return;
	}
	showSetup(game);
	if (game.status !== "setup") return;
	const list = clear($("play-seat-list"));
	const seats = game.seats || [];
	for (const sc of seats) {
		list.append(el("li", { class: "play-seat-row" },
			el("b", { text: `${sc.seat}. ${sc.name || `Seat ${sc.seat}`}` }),
			sc.commander ? el("span", { class: "play-seat-cmdr", text: ` — ${sc.commander}` }) : null,
			sc.starting_life ? el("span", { class: "play-seat-life", text: ` · ${sc.starting_life} life` }) : null,
		));
	}
	if (!seats.length) {
		list.append(el("li", { class: "play-seat-row camp-status", text: "No one is seated yet." }));
	}
	$("play-start").disabled = seats.length < 2;
}

function renderStrip() {
	const active = state?.status === "active";
	$("play-strip").hidden = !active;
	if (!active) return;

	$("play-turnline").textContent = turnLine(game, state);

	const sel = clear($("play-acting"));
	for (const seat of state.order || []) {
		sel.append(el("option", {
			text: seatName(state, seat) + (seat === state.turn_seat ? " (turn)" : ""),
			attrs: { value: String(seat) },
		}));
	}
	const acting = actingSeat();
	if ([...sel.options].some((o) => Number(o.value) === acting)) sel.value = String(acting);

	$("play-pass").disabled = !canPass(state) || state.priority_seat !== acting;
	$("play-give").hidden = !state.priority_seat || state.priority_seat === acting;
	$("play-advance").disabled = (state.stack?.length || 0) > 0;
	$("play-resolve-combat").hidden = !(state.phase === "combat" && state.step === "combat_damage"
		&& (state.attackers?.length || 0) > 0 && !state.combat_resolved);
}

function renderBoard() {
	const host = clear($("play-seats"));
	if (state?.status === "active" || state?.status === "finished") {
		for (const seat of state.order || []) host.append(seatCard(seat));
	} else {
		host.append(el("p", { class: "camp-status", text: "The board arrives when the game starts." }));
	}
	renderStack();
	renderContext();
}

/** One seat's strip: identity, life, counters, zones, permanents. */
function seatCard(seat) {
	const p = state.seats[seat] || {};
	const isTurn = state.turn_seat === seat && state.status === "active";
	const dead = !p.alive;

	const card = el("article", {
		// A parchment page: the seat's values are content the table reads,
		// so the frame is the book's, not the room's.
		class: "play-seat f-parchment" + (isTurn ? " is-turn" : "") + (dead ? " is-dead" : ""),
	});

	const head = el("header", { class: "play-seat-head" });
	head.append(el("b", { class: "play-seat-name", text: seatName(state, seat) }));
	if (p.commander) head.append(el("span", { class: "play-seat-cmdr", text: p.commander }));
	for (const [flag, value] of Object.entries(p.flags || {})) {
		if (value) head.append(el("i", { class: "play-flag", text: flag }));
	}
	if (dead) head.append(el("i", { class: "play-flag is-dead", text: "out" }));

	// Life: the number the table squints at, one tap from its stepper.
	head.append(el("button", {
		class: "play-life",
		attrs: { type: "button", "data-life": String(seat), title: "Life — tap to change" },
		text: String(p.life ?? "—"),
	}));

	card.append(head);

	// Player counters and commander damage — every generic value tappable.
	const nums = el("div", { class: "play-seat-nums" });
	for (const c of counterChips(p.counters)) {
		nums.append(el("button", {
			class: "play-chip play-count",
			attrs: { type: "button", "data-counter-seat": String(seat), "data-counter": c.name, title: `${c.name}: tap to adjust` },
			text: `${c.name} ${c.n}`,
		}));
	}
	for (const [cmdr, n] of Object.entries(p.commander_damage || {})) {
		if (n > 0) {
			nums.append(el("span", {
				class: "play-chip play-cmdr-dmg" + (n >= 21 ? " is-lethal" : ""),
				attrs: { title: `commander damage from ${cmdr} (21 ends it)` },
				text: `${cmdr.split(",")[0]} ${n}`,
			}));
		}
	}
	if (nums.children.length) card.append(nums);

	// Zones: hand count-only, library composition-honest ("?" is a value).
	const tally = zoneTally(state, seat);
	const zones = el("div", { class: "play-seat-zones" });
	zones.append(el("button", {
		class: "play-zone", attrs: { type: "button", "data-zone": "hand", "data-seat": String(seat) },
		title: "Hand — tap to draw or set the count",
	}, el("span", { text: "hand " }), el("b", { text: formatCount(p.hand) })));
	zones.append(el("button", {
		class: "play-zone", attrs: { type: "button", "data-zone": "library", "data-seat": String(seat) },
		title: "Library — tap to mill or set the count",
	}, el("span", { text: "lib " }), el("b", { text: formatCount(p.library) })));
	for (const key of ["graveyard", "exile", "command"]) {
		if (!tally[key]) continue;
		zones.append(el("span", { class: "play-zone" },
			el("span", { text: `${key.slice(0, 3)} ` }), el("b", { text: String(tally[key]) })));
	}
	card.append(zones);

	// The battlefield: identical tokens collapse into one chip ("3× 1/1
	// Soldier"), real cards stand alone with their computed truth.
	const field = el("div", { class: "play-field" });
	const groups = new Map();
	const commanders = [];
	for (const o of seatObjects(seat)) {
		if (o.zone === "command") {
			if (o.identity?.card) commanders.push(o);
			continue;
		}
		const key = o.identity?.token
			? `t:${o.identity.token.name}:${o.identity.token.power}/${o.identity.token.toughness}`
			: `c:${o.id}`;
		if (!groups.has(key)) groups.set(key, { objs: [], token: o.identity?.token || null });
		groups.get(key).objs.push(o);
	}
	// The command zone's commanders stay castable in one tap, tax included.
	for (const o of commanders) {
		field.append(el("button", {
			class: "play-chip play-cast-cmdr",
			attrs: { type: "button", "data-cast-cmdr": String(o.id), title: `Cast from the command zone — tax ${commanderTax(state, seat)}` },
			text: `⌘ ${o.identity.card} (+${commanderTax(state, seat)})`,
		}));
	}
	for (const g of groups.values()) {
		field.append(g.token ? tokenChip(g) : objectChip(g.objs[0]));
	}
	if (!field.children.length) {
		field.append(el("span", { class: "play-field-empty", text: dead ? "" : "nothing on the battlefield" }));
	}
	card.append(field);
	return card;
}

/** A seat's tracked objects in id (arrival) order. */
function seatObjects(seat) {
	return Object.values(state?.objects || {})
		.filter((o) => o.controller === seat && (o.zone === "battlefield" || o.zone === "command") && !o.phased)
		.sort((a, b) => a.id - b.id);
}

function tokenChip(g) {
	const t = g.token;
	const pt = t?.power != null && t?.toughness != null ? ` ${t.power}/${t.toughness}` : "";
	return el("button", {
		class: "play-chip play-obj is-token",
		attrs: { type: "button", "data-object": String(g.objs[0].id), title: t?.name || "token" },
	},
		el("b", { text: `${g.objs.length}× ` }),
		el("span", { text: `${t?.name || "token"}${pt}` }),
	);
}

function objectChip(o) {
	const chip = el("button", {
		class: "play-chip play-obj" +
			(o.tapped ? " is-tapped" : "") +
			(attackDraft.has(o.id) ? " is-attacking" : "") +
			(blockDraft.get(blockPick)?.includes(o.id) ? " is-blocking" : "") +
			(ctx?.kind === "object" && ctx.id === o.id ? " is-selected" : ""),
		attrs: { type: "button", "data-object": String(o.id), title: objectName(o) },
	});
	const pt = ptLine(o);
	chip.append(el("span", { class: "play-obj-name", text: objectName(o) }));
	if (pt) chip.append(el("b", { class: "play-obj-pt", text: pt }));
	const c = o.counters || {};
	const plus = (c["+1/+1"] || 0) - (c["-1/-1"] || 0);
	if (plus) chip.append(el("i", { class: "play-obj-count", text: `${plus > 0 ? "+" : ""}${plus}` }));
	for (const kc of counterChips(c)) {
		if (kc.name === "+1/+1" || kc.name === "-1/-1") continue;
		chip.append(el("i", { class: "play-obj-count", text: `${kc.name} ${kc.n}` }));
	}
	if (o.damage) chip.append(el("i", { class: "play-obj-count is-damage", text: `${o.damage} dmg` }));
	if (o.attached_to) chip.append(el("i", { class: "play-obj-attach", text: "→" }));
	if ((o.attachments || []).length) chip.append(el("i", { class: "play-obj-attach", text: `+${o.attachments.length}` }));
	return chip;
}

function renderStack() {
	const wrap = $("play-stackwrap");
	const stack = state?.stack || [];
	const queue = state?.trigger_queue || [];
	wrap.hidden = !stack.length && !queue.length;
	if (wrap.hidden) return;
	const list = clear($("play-stack"));
	// Top of the stack first — that is what resolves next.
	for (let i = stack.length - 1; i >= 0; i--) {
		const item = stack[i];
		const who = seatName(state, item.controller);
		const what = item.mode === "cast"
			? (item.card || objectName(state.objects?.[item.object]) || "a spell")
			: (item.ability || item.card || "an ability");
		const row = el("li", { class: "play-stack-item" + (i === stack.length - 1 ? " is-top" : "") },
			el("span", { text: what }),
			el("i", { class: "play-stack-who", text: who }));
		if (i === stack.length - 1) {
			row.append(el("button", {
				class: "enc-btn", attrs: { type: "button", "data-resolve": "1", title: "Everyone passes; the top resolves" },
				text: "resolve",
			}));
		}
		list.append(row);
	}
	if (queue.length) {
		list.append(el("li", { class: "play-stack-item is-trigger" },
			el("span", { text: `${queue.length} trigger${queue.length === 1 ? "" : "s"} waiting` })));
	}
}

/* ---------- the log pane ---------- */

function renderLog() {
	const host = clear($("play-log"));
	const rows = events.slice().sort((a, b) => b.ord - a.ord);
	const logEl = $("play-log");
	const nearHead = logEl.scrollHeight - logEl.scrollTop - logEl.clientHeight < 160;
	for (const ev of rows) {
		const row = el("li", {
			class: "play-row" + (ev.visibility === "seat" ? " is-private" : ""),
			attrs: { "data-ord": String(ev.ord) },
		},
			el("b", { class: "play-row-ord", text: String(ev.ord) }),
			el("span", { class: "play-row-text", text: describeEvent(ev, state || {}) }),
		);
		if (ev.source && ev.source !== "tap" && ev.source !== "system") {
			row.append(el("i", { class: "play-row-src", text: ev.source }));
		}
		// Every entry carries both halves of the correction contract:
		// ✎ rewrites it (amend, two taps — the second is the composer's
		// submit), ⟲ rewinds to it. Neither covers the log.
		row.append(el("button", {
			class: "play-row-fix",
			attrs: { type: "button", "data-amend": String(ev.ord), title: "Not it — correct this entry" },
			text: "✎",
		}));
		row.append(el("button", {
			class: "play-row-rewind",
			attrs: { type: "button", "data-rewind": String(ev.ord), title: `Rewind to #${ev.ord} — everything after it is undone` },
			text: "⟲",
		}));
		host.append(row);
	}
	if (!rows.length) {
		host.append(el("li", { class: "play-row is-empty", text: "Nothing has happened yet — the first action is yours." }));
	}
	if (nearHead) logEl.scrollTop = 0; // newest-first: stick to the head unless reading history
}

/* ---------- the current action pane ---------- */

function currentMeta(message, warn) {
	const meta = $("play-current-meta");
	meta.textContent = message || "";
	meta.classList.toggle("warn", !!warn);
}

function renderCurrent() {
	const pane = $("play-current");
	const active = state?.status === "active" || state?.status === "finished";
	pane.hidden = !active;
	if (!active) return;
	const batch = lastActionBatch(events);
	const text = $("play-current-text");
	const ok = $("play-ok");
	const edit = $("play-edit");
	if (!batch) {
		text.textContent = "—";
		ok.disabled = edit.disabled = true;
		pane.classList.remove("is-fresh");
		return;
	}
	text.textContent = actionSummary(batch.action, state || {}) || describeEvent(batch.events[0], state || {});
	ok.disabled = edit.disabled = false;
	pane.classList.toggle("is-fresh", batch.to > acknowledged);
}

function cancelEdit() {
	editDraft = null;
	const banner = $("play-edit-banner");
	banner.hidden = true;
	clear(banner);
}

/** ✎ not it: prefill the composer from the applied action's own cause.
     Submitting rewrites it — amend, two taps, the log telling both
     halves. */
function startEdit() {
	const batch = lastActionBatch(events);
	if (batch) startEditAt(batch.to);
}

/** ✎ not it, on any log entry: the same amend, reachable from the entry
     itself. Everything after the entry is undone with it — amend is a
     rewind with a correction riding along. */
function startEditAt(ord) {
	const batch = actionBatchAt(events, ord);
	if (!batch?.action) return;
	editDraft = { at: batch.from, from: batch.from };
	const banner = $("play-edit-banner");
	clear(banner);
	banner.append(document.createTextNode(`editing #${batch.from} — submitting rewrites it, everything after is undone `));
	banner.append(el("button", {
		class: "enc-btn", attrs: { type: "button" }, text: "cancel",
		on: { click: cancelEdit },
	}));
	banner.hidden = false;
	prefillComposer(batch.action);
}

/** Load an action's cause into the composer's fields — amend prefills
     from the log entry itself, so the common fix is "type the right card
     and submit". */
function prefillComposer(action) {
	if (action.card) $("play-card").value = action.card;
	if (action.from_zone) $("play-from").value = action.from_zone;
	if (action.kind === "CREATE_TOKEN" && action.token) {
		$("play-token-name").value = action.token.name || "";
		$("play-token-p").value = action.token.power ?? "";
		$("play-token-t").value = action.token.toughness ?? "";
		$("play-token-count").value = action.count || 1;
		$("play-more").open = true;
	}
	if (action.kind === "DEAL_DAMAGE" && action.amount) {
		$("play-damage-amount").value = action.amount;
		if (action.source_card) $("play-damage-source").value = action.source_card;
		$("play-more").open = true;
	}
	if ((action.kind === "ADJUST_COUNTERS" || action.kind === "SET_COUNTERS") && action.counter_name) {
		$("play-counter-name").value = action.counter_name;
		if (action.delta) $("play-counter-delta").value = action.delta;
		$("play-more").open = true;
	}
	$("play-card").focus();
}

/** Submit the corrected action: one call that truncates at the edited
     entry's batch and applies the correction in a single server
     transaction — no client ever sees the rewound intermediate, and a
     rejected correction leaves the log untouched (the draft stays, so
     the fix can be adjusted and resubmitted). */
async function submitEdited(action) {
	const draft = editDraft;
	if (!draft) {
		submit(action);
		return;
	}
	try {
		await api.gameAmend(gameID, draft.at, { ...action, source: "tap" });
	} catch (err) {
		currentMeta(err.message, true);
		return;
	}
	cancelEdit();
	// Our own stream announces the rewind too; re-read either way so the
	// composer's next paint stands on the post-edit truth.
	await reloadGame().catch(() => {});
	render();
}

/* ---------- the context strip ---------- */

function closeContext() {
	ctx = null;
}

/** The strip above the board: whatever the last tap selected — a
    permanent's actions, a seat's life stepper, a zone's tallies, a
    combat draft. It never covers the log; it is a row in the board pane.
    The board re-paints whole on every ordinal, so the strip rebuilds too
    — and what was typed into it survives, because losing a half-entered
    number to an unrelated event would be the manual-entry tax this
    surface exists to avoid. */
function renderContext() {
	const host = $("play-context");
	if (!state || state.status !== "active" || !ctx) {
		host.hidden = true;
		clear(host);
		return;
	}
	// Drafts follow the step; leaving the step closes them.
	if (ctx.kind === "attack" && !(state.phase === "combat" && state.step === "declare_attackers")) {
		attackDraft = new Set();
		ctx = null;
		host.hidden = true;
		clear(host);
		return;
	}
	if (ctx.kind === "block" && !(state.phase === "combat" && state.step === "declare_blockers")) {
		blockDraft = new Map();
		ctx = null;
		host.hidden = true;
		clear(host);
		return;
	}
	const typed = {};
	for (const field of host.querySelectorAll("input,select")) {
		if (field.id) typed[field.id] = field.value;
	}
	host.hidden = false;
	clear(host);
	switch (ctx.kind) {
		case "object": renderObjectContext(host); break;
		case "seat": renderSeatContext(host); break;
		case "counter": renderCounterContext(host); break;
		case "attack": renderAttackDraft(host); break;
		case "block": renderBlockDraft(host); break;
	}
	for (const field of host.querySelectorAll("input,select")) {
		if (field.id && typed[field.id] != null && field.value !== typed[field.id]) {
			field.value = typed[field.id];
		}
	}
}

function renderObjectContext(host) {
	const o = state.objects?.[ctx.id];
	if (!o || o.zone !== "battlefield") {
		ctx = null;
		host.hidden = true;
		return;
	}
	host.append(el("b", { class: "play-ctx-name", text: objectName(o) }));
	if (ptLine(o)) host.append(el("span", { class: "play-ctx-pt", text: ptLine(o) }));
	host.append(el("i", { class: "play-ctx-zone", text: o.zone }));

	host.append(el("button", {
		class: "enc-btn", attrs: { type: "button", "data-act": o.tapped ? "untap" : "tap" },
		text: o.tapped ? "untap" : "tap",
	}));
	host.append(el("button", {
		class: "enc-btn", attrs: { type: "button", "data-act": "phase" }, text: o.phased ? "phase in" : "phase out",
	}));
	host.append(counterBtn("+1/+1", 1), counterBtn("+1/+1", -1));
	host.append(el("button", {
		class: "enc-btn", attrs: { type: "button", "data-act": "zone", "data-to": "graveyard", "data-cause": "sacrifice" }, text: "sacrifice",
	}));
	host.append(el("button", {
		class: "enc-btn", attrs: { type: "button", "data-act": "zone", "data-to": "graveyard", "data-cause": "destroy" }, text: "destroy",
	}));
	host.append(el("button", {
		class: "enc-btn", attrs: { type: "button", "data-act": "zone", "data-to": "hand", "data-cause": "bounce" }, text: "bounce",
	}));
	host.append(el("button", {
		class: "enc-btn", attrs: { type: "button", "data-act": "zone", "data-to": "exile", "data-cause": "exile" }, text: "exile",
	}));

	// Damage to this object, with the amount spoken at the table.
	const dmg = el("span", { class: "play-ctx-group" });
	dmg.append(el("input", {
		class: "enc-field play-num", attrs: { type: "number", min: "1", id: "play-ctx-dmg-n", "aria-label": "Damage to this object", placeholder: "dmg" },
	}));
	dmg.append(el("button", { class: "enc-btn", attrs: { type: "button", "data-act": "damage" }, text: "deal" }));
	host.append(dmg);

	// A named counter of the set's own invention.
	const counter = el("span", { class: "play-ctx-group" });
	counter.append(el("input", {
		class: "enc-field", attrs: { type: "text", id: "play-ctx-counter-name", placeholder: "counter", "aria-label": "Counter name" },
	}));
	counter.append(el("input", {
		class: "enc-field play-num", attrs: { type: "number", placeholder: "±n", id: "play-ctx-counter-delta", "aria-label": "Counter delta" },
	}));
	counter.append(el("button", { class: "enc-btn", attrs: { type: "button", "data-act": "counter" }, text: "adjust" }));
	host.append(counter);

	host.append(el("button", { class: "enc-btn", attrs: { type: "button", "data-act": "close" }, text: "✕" }));
}

function counterBtn(name, delta) {
	return el("button", {
		class: "enc-btn",
		attrs: { type: "button", "data-act": "counter", "data-counter": name, "data-delta": String(delta) },
		text: `${delta > 0 ? "+" : ""}${delta} ${name}`,
	});
}

function renderSeatContext(host) {
	const p = state.seats[ctx.seat] || {};
	host.append(el("b", { class: "play-ctx-name", text: `${seatName(state, ctx.seat)} — ${ctx.zone}` }));

	if (ctx.zone === "life") {
		for (const d of [-5, -1, 1, 5]) {
			host.append(el("button", {
				class: "enc-btn",
				attrs: { type: "button", "data-act": "life", "data-delta": String(d) },
				text: `${d > 0 ? "+" : ""}${d}`,
			}));
		}
		const set = el("span", { class: "play-ctx-group" });
		set.append(el("input", {
			class: "enc-field play-num", attrs: { type: "number", id: "play-ctx-life-set", "aria-label": "Set life to", placeholder: "set" },
		}));
		set.append(el("button", { class: "enc-btn", attrs: { type: "button", "data-act": "life-set" }, text: "set" }));
		host.append(set);
	} else if (ctx.zone === "hand") {
		host.append(el("button", { class: "enc-btn", attrs: { type: "button", "data-act": "draw" }, text: "draw 1" }));
		const n = el("span", { class: "play-ctx-group" });
		n.append(el("input", {
			class: "enc-field play-num", attrs: { type: "number", min: "1", id: "play-ctx-draw-n", "aria-label": "Draw count", placeholder: "n" },
		}));
		n.append(el("button", { class: "enc-btn", attrs: { type: "button", "data-act": "draw-n" }, text: "draw n" }));
		host.append(n);
		const c = el("span", { class: "play-ctx-group" });
		c.append(el("input", {
			class: "enc-field play-num", attrs: { type: "number", min: "0", id: "play-ctx-hand-set", "aria-label": "Set hand count", placeholder: "set" },
		}));
		c.append(el("button", { class: "enc-btn", attrs: { type: "button", "data-act": "hand-set" }, text: "set" }));
		host.append(c);
	} else if (ctx.zone === "library") {
		const n = el("span", { class: "play-ctx-group" });
		n.append(el("input", {
			class: "enc-field play-num", attrs: { type: "number", min: "1", id: "play-ctx-mill-n", "aria-label": "Mill count", placeholder: "n" },
		}));
		n.append(el("button", { class: "enc-btn", attrs: { type: "button", "data-act": "mill-n" }, text: "mill n" }));
		host.append(n);
		const c = el("span", { class: "play-ctx-group" });
		c.append(el("input", {
			class: "enc-field play-num", attrs: { type: "number", min: "0", id: "play-ctx-lib-set", "aria-label": "Set library count", placeholder: "set" },
		}));
		c.append(el("button", { class: "enc-btn", attrs: { type: "button", "data-act": "lib-set" }, text: "set" }));
		host.append(c);
	}

	// Status flags and the way out.
	host.append(el("button", {
		class: "enc-btn", attrs: { type: "button", "data-act": "flag-monarch" },
		text: p.flags?.monarch ? "monarch ✓" : "make monarch",
	}));
	host.append(el("button", {
		class: "enc-btn", attrs: { type: "button", "data-act": "flag-initiative" },
		text: p.flags?.initiative ? "initiative ✓" : "take initiative",
	}));
	if (p.alive) {
		host.append(el("button", {
			class: "enc-btn danger", attrs: { type: "button", "data-act": "concede" }, text: "concede",
		}));
	}
	host.append(el("button", { class: "enc-btn", attrs: { type: "button", "data-act": "close" }, text: "✕" }));
}

/** A player-counter stepper, opened from a counter chip. */
function renderCounterContext(host) {
	const p = state.seats[ctx.seat] || {};
	const now = p.counters?.[ctx.name] ?? 0;
	host.append(el("b", { class: "play-ctx-name", text: `${seatName(state, ctx.seat)} — ${ctx.name} ${now}` }));
	for (const d of [-5, -1, 1, 5]) {
		host.append(el("button", {
			class: "enc-btn",
			attrs: { type: "button", "data-act": "counter", "data-counter": ctx.name, "data-delta": String(d) },
			text: `${d > 0 ? "+" : ""}${d}`,
		}));
	}
	host.append(el("button", { class: "enc-btn", attrs: { type: "button", "data-act": "close" }, text: "✕" }));
}

/** While the step is declare_attackers: the tapped-in attackers and the
    target they share. */
function renderAttackDraft(host) {
	host.append(el("b", { class: "play-ctx-name", text: `attacking with ${attackDraft.size}` }));
	const pick = el("select", { class: "enc-field", attrs: { id: "play-attack-target", "aria-label": "Attack target" } });
	for (const seat of state.order || []) {
		if (seat === state.turn_seat) continue;
		pick.append(el("option", { text: seatName(state, seat), attrs: { value: `seat:${seat}` } }));
	}
	for (const o of Object.values(state.objects || {})) {
		if (o.zone === "battlefield" && !o.phased && isType(o, "Planeswalker") && o.controller !== state.turn_seat) {
			pick.append(el("option", {
				text: `${objectName(o)} (${seatName(state, o.controller)})`,
				attrs: { value: `obj:${o.id}` },
			}));
		}
	}
	host.append(pick);
	host.append(el("button", {
		class: "enc-btn primary", attrs: { type: "button", "data-act": "declare-attackers" }, text: "declare",
	}));
	host.append(el("button", { class: "enc-btn", attrs: { type: "button", "data-act": "attack-clear" }, text: "clear" }));
	host.append(el("button", { class: "enc-btn", attrs: { type: "button", "data-act": "close" }, text: "✕" }));
}

/** While the step is declare_blockers: one attacker at a time, its
    blockers tapped in below. */
function renderBlockDraft(host) {
	const attackers = state.attackers || [];
	if (!attackers.length) {
		host.append(el("span", { class: "camp-status", text: "No attackers to block." }));
		return;
	}
	if (!blockPick || !attackers.some((a) => a.object === blockPick)) blockPick = attackers[0].object;

	host.append(el("b", { class: "play-ctx-name", text: "block" }));
	const tabs = el("span", { class: "play-ctx-tabs" });
	for (const at of attackers) {
		tabs.append(el("button", {
			class: "play-chip" + (at.object === blockPick ? " is-selected" : ""),
			attrs: { type: "button", "data-block-pick": String(at.object) },
			text: objectName(state.objects?.[at.object]),
		}));
	}
	host.append(tabs);
	const assigned = blockDraft.get(blockPick) || [];
	host.append(el("span", { class: "play-ctx-note", text: assigned.length ? `${assigned.length} blocking` : "tap blockers below" }));
	host.append(el("button", {
		class: "enc-btn primary", attrs: { type: "button", "data-act": "declare-blockers" }, text: "declare",
	}));
	host.append(el("button", { class: "enc-btn", attrs: { type: "button", "data-act": "close" }, text: "✕" }));
}

/* ---------- wiring ---------- */

function wire() {
	$("play-game").addEventListener("change", (e) => { openGame(e.target.value); });
	$("play-new").addEventListener("click", createGame);
	$("play-seat-form").addEventListener("submit", seatFormSubmit);
	$("play-start").addEventListener("click", startGame);

	$("play-advance").addEventListener("click", () => submit({ kind: "ADVANCE" }));
	$("play-pass").addEventListener("click", () => submit({ kind: "PASS_PRIORITY" }));
	$("play-give").addEventListener("click", () => rotatePriorityTo(actingSeat()));
	$("play-draw").addEventListener("click", () => submit({ kind: "DRAW", count: 1 }));
	$("play-untap-all").addEventListener("click", () => submit({ kind: "UNTAP", all: true }));
	$("play-resolve-combat").addEventListener("click", () => submit({ kind: "RESOLVE_COMBAT" }));
	$("play-acting").addEventListener("change", (e) => {
		actingOverride = Number(e.target.value) || 0;
		render();
	});

	// Board taps: one delegated listener, because the board re-paints
	// whole on every ordinal and per-chip handlers would not survive it.
	$("play-board").addEventListener("click", onBoardTap);

	$("play-undo").addEventListener("click", () => {
		const batch = lastActionBatch(events);
		if (batch) rewindTo(batch.undoTo);
	});
	$("play-ok").addEventListener("click", () => {
		const batch = lastActionBatch(events);
		if (batch) acknowledged = batch.to;
		render();
	});
	$("play-edit").addEventListener("click", startEdit);

	$("play-log").addEventListener("click", (e) => {
		const amend = e.target.closest("[data-amend]");
		if (amend) {
			startEditAt(Number(amend.dataset.amend));
			return;
		}
		const btn = e.target.closest("[data-rewind]");
		if (btn) rewindTo(Number(btn.dataset.rewind));
	});

	$("play-cast").addEventListener("click", () => castCard(false));
	$("play-land").addEventListener("click", () => castCard(true));
	$("play-token-make").addEventListener("click", makeToken);
	$("play-damage-deal").addEventListener("click", dealDamage);
	$("play-counter-apply").addEventListener("click", applyCounter);

	$("play-card").addEventListener("input", cardSearchDebounced);
	$("play-card").addEventListener("keydown", (e) => {
		if (e.key === "Enter") {
			e.preventDefault();
			hideCardList();
		}
	});
	document.addEventListener("click", (e) => {
		if (!e.target.closest("#play-card") && !e.target.closest("#play-card-list")) hideCardList();
	});
}

async function createGame() {
	const name = `Commander ${new Date().toLocaleDateString([], { month: "short", day: "numeric" })}`;
	try {
		const data = await api.gameCreate({ name });
		await loadGames(data.game.id);
	} catch (err) {
		renderMeta(err.message, true);
	}
}

async function seatFormSubmit(e) {
	e.preventDefault();
	const name = $("play-seat-name").value.trim();
	if (!name) {
		$("play-seat-name").focus();
		return;
	}
	const seated = game?.seats || [];
	const position = seated.length ? Math.max(...seated.map((s) => s.seat)) + 1 : 1;
	try {
		const data = await api.gameSeat(gameID, {
			position,
			name,
			commander: $("play-seat-commander").value.trim(),
		});
		game = data.game;
		$("play-seat-name").value = "";
		$("play-seat-commander").value = "";
		render();
	} catch (err) {
		renderMeta(err.message, true);
	}
}

async function startGame() {
	try {
		await api.gameStart(gameID);
		await reloadGame();
		render();
	} catch (err) {
		renderMeta(err.message, true);
	}
}

/** The board's one tap handler: chips, zones, life, the context strip. */
function onBoardTap(e) {
	const btn = e.target.closest("button");
	if (!btn || !state) return;
	const ds = btn.dataset;

	if (ds.resolve) { resolveTop(); return; }
	if (ds.act) { contextAction(ds); return; }
	if (ds.blockPick) { blockPick = Number(ds.blockPick); render(); return; }

	// A counter chip on a seat opens that counter's stepper.
	if (ds.counterSeat) {
		ctx = { kind: "counter", seat: Number(ds.counterSeat), name: ds.counter };
		render();
		return;
	}

	// Life and zones open their context strips.
	if (ds.life) {
		ctx = { kind: "seat", seat: Number(ds.life), zone: "life" };
		render();
		return;
	}
	if (ds.zone && ds.seat) {
		ctx = { kind: "seat", seat: Number(ds.seat), zone: ds.zone };
		render();
		return;
	}

	// The commander's one-tap cast, tax included. The card's declared
	// base rides the cast — the commander object mints from config with
	// no characteristics, and the fold learns them here.
	if (ds.castCmdr) {
		const o = state.objects?.[Number(ds.castCmdr)];
		if (o?.identity?.card) castFromZone(o.identity.card, "command");
		return;
	}

	if (!ds.object) return;
	const id = Number(ds.object);
	const o = state.objects?.[id];
	if (!o) return;

	// During declare_attackers, the active seat's creatures tap in as
	// attackers rather than opening their sheet.
	if (state.phase === "combat" && state.step === "declare_attackers"
		&& o.controller === state.turn_seat && o.zone === "battlefield" && isType(o, "Creature")) {
		if (attackDraft.has(id)) attackDraft.delete(id);
		else attackDraft.add(id);
		ctx = { kind: "attack" };
		render();
		return;
	}
	// During declare_blockers, a defending seat's creatures join the
	// picked attacker's block.
	if (state.phase === "combat" && state.step === "declare_blockers"
		&& o.zone === "battlefield" && o.controller !== state.turn_seat && isType(o, "Creature")
		&& !state.attackers?.some((a) => a.object === id)) {
		const list = blockDraft.get(blockPick) || [];
		const at = list.indexOf(id);
		if (at >= 0) list.splice(at, 1);
		else list.push(id);
		blockDraft.set(blockPick, list);
		ctx = { kind: "block" };
		render();
		return;
	}

	ctx = { kind: "object", id };
	render();
}

/** The context strip's own buttons. */
function contextAction(ds) {
	if (!state) return;
	const seatFromCtx = ctx?.kind === "seat" || ctx?.kind === "counter" ? ctx.seat : 0;
	const o = ctx?.kind === "object" ? state.objects?.[ctx.id] : null;
	switch (ds.act) {
		case "close":
			closeContext();
			render();
			return;
		case "counter": {
			// Object counters from the object strip, seat counters from
			// their steppers — one path, one ADJUST_COUNTERS action.
			const name = ds.counter || $("#play-ctx-counter-name")?.value.trim() || "";
			const delta = Number(ds.delta ?? ($("#play-ctx-counter-delta")?.value || 0));
			if (!name || !delta) return;
			if (o) submit({ kind: "ADJUST_COUNTERS", on_object: o.id, counter_name: name, delta });
			else if (seatFromCtx) submit({ kind: "ADJUST_COUNTERS", target_seat: seatFromCtx, counter_name: name, delta });
			return;
		}
		case "tap":
			if (o) submit({ kind: "TAP", object: o.id });
			return;
		case "untap":
			if (o) submit({ kind: "UNTAP", object: o.id });
			return;
		case "phase":
			if (o) submit({ kind: "SET_PHASED", object: o.id, phased: !o.phased });
			return;
		case "zone":
			if (o) {
				submit({ kind: "MOVE_ZONE", object: o.id, to_zone: ds.to, cause: ds.cause });
				closeContext();
			}
			return;
		case "damage": {
			const n = Number($("#play-ctx-dmg-n")?.value || 0);
			if (o && n > 0) submit({ kind: "DEAL_DAMAGE", amount: n, target_object: o.id });
			return;
		}
		case "life":
			submit({ kind: "CHANGE_LIFE", target_seat: seatFromCtx, delta: Number(ds.delta) });
			return;
		case "life-set": {
			const to = Number($("#play-ctx-life-set")?.value);
			if (Number.isFinite(to)) submit({ kind: "CHANGE_LIFE", target_seat: seatFromCtx, to });
			return;
		}
		case "draw":
			submit({ kind: "DRAW", seat: seatFromCtx, count: 1 });
			return;
		case "draw-n": {
			const n = Number($("#play-ctx-draw-n")?.value || 0);
			if (n > 0) submit({ kind: "DRAW", seat: seatFromCtx, count: n });
			return;
		}
		case "hand-set": {
			const to = Number($("#play-ctx-hand-set")?.value);
			if (Number.isFinite(to)) submit({ kind: "SET_ZONE_COUNT", seat: seatFromCtx, target_seat: seatFromCtx, zone: "hand", to });
			return;
		}
		case "mill-n": {
			const n = Number($("#play-ctx-mill-n")?.value || 0);
			if (n > 0) submit({ kind: "MILL", seat: seatFromCtx, target_seat: seatFromCtx, count: n });
			return;
		}
		case "lib-set": {
			const to = Number($("#play-ctx-lib-set")?.value);
			if (Number.isFinite(to)) submit({ kind: "SET_ZONE_COUNT", seat: seatFromCtx, target_seat: seatFromCtx, zone: "library", to });
			return;
		}
		case "flag-monarch":
			submit({ kind: "SET_FLAG", seat: seatFromCtx, target_seat: seatFromCtx, flag: "monarch", value: "yes" });
			return;
		case "flag-initiative":
			submit({ kind: "SET_FLAG", seat: seatFromCtx, target_seat: seatFromCtx, flag: "initiative", value: "yes" });
			return;
		case "concede":
			submit({ kind: "CONCEDE", seat: seatFromCtx });
			closeContext();
			return;
		case "declare-attackers": {
			const pick = $("#play-attack-target")?.value || "";
			if (!attackDraft.size || !pick) return;
			const attackers = [...attackDraft].map((id) => pick.startsWith("seat:")
				? { object: id, target_seat: Number(pick.slice(5)) }
				: { object: id, target_object: Number(pick.slice(4)) });
			attackDraft = new Set();
			submit({ kind: "DECLARE_ATTACKERS", seat: state.turn_seat, attackers });
			closeContext();
			return;
		}
		case "attack-clear":
			attackDraft = new Set();
			render();
			return;
		case "declare-blockers": {
			// The draft is attacker → blockers; the action wants each
			// blocker's attackers (one creature can block several).
			const byBlocker = new Map();
			for (const [attacker, list] of blockDraft.entries()) {
				for (const b of list) {
					if (!byBlocker.has(b)) byBlocker.set(b, []);
					byBlocker.get(b).push(attacker);
				}
			}
			const blockers = [...byBlocker.entries()].map(([blocker, attackers]) => ({ blocker, attackers }));
			blockDraft = new Map();
			submit({ kind: "DECLARE_BLOCKERS", blockers });
			closeContext();
			return;
		}
	}
}

/* ---------- the composer ---------- */

/** Cast (or play) a named card from a zone: resolve the card once for
    its declared base characteristics, then submit. An unresolvable name
    casts with unknowns — the honest state, never a guess. */
async function castFromZone(card, from) {
	const action = { kind: "CAST", card, from_zone: from };
	try {
		const resolved = await resolveCard(card);
		if (resolved) {
			const base = baseCharsFromCard(resolved);
			if (base.types.length || base.power != null || base.loyalty != null) action.base = base;
		}
	} catch (_) { /* offline or unknown: the cast still lands, characteristics unknown */ }
	if (editDraft) await submitEdited(action);
	else submit(action);
}

async function castCard(asLand) {
	const q = $("play-card").value.trim();
	if (!q) {
		$("play-card").focus();
		return;
	}
	const from = $("play-from").value;
	if (asLand) {
		// Playing a land from hand or library creates its object on
		// arrival; the declared base is the same lookup's.
		const action = { kind: "PLAY_LAND", card: q, from_zone: from === "hand" ? "hand" : from };
		try {
			const resolved = await resolveCard(q);
			if (resolved) action.base = baseCharsFromCard(resolved);
		} catch (_) { /* unknown stays unknown */ }
		if (editDraft) await submitEdited(action);
		else submit(action);
		return;
	}
	await castFromZone(q, from);
}

function makeToken() {
	const name = $("play-token-name").value.trim();
	if (!name) {
		$("play-token-name").focus();
		return;
	}
	const p = $("play-token-p").value;
	const t = $("play-token-t").value;
	const spec = { name, types: (p === "" && t === "") ? ["Artifact"] : ["Creature"] };
	if (p !== "") spec.power = Number(p);
	if (t !== "") spec.toughness = Number(t);
	const count = Math.max(1, Number($("play-token-count").value || 1));
	const action = { kind: "CREATE_TOKEN", token: spec, count };
	if (editDraft) submitEdited(action);
	else submit(action);
}

function dealDamage() {
	const val = $("play-damage-target").value || "";
	const amount = Number($("play-damage-amount").value || 0);
	if (!val || amount < 1) {
		currentMeta("pick a target and an amount", true);
		return;
	}
	const action = { kind: "DEAL_DAMAGE", amount };
	const source = $("play-damage-source").value.trim();
	if (source) action.source_card = source;
	if (val.startsWith("seat:")) action.target_seat = Number(val.slice(5));
	else action.source_obj = Number(val.slice(4));
	if (editDraft) submitEdited(action);
	else submit(action);
}

function applyCounter() {
	const val = $("play-counter-target").value || "";
	const name = $("play-counter-name").value.trim();
	const delta = Number($("play-counter-delta").value || 0);
	if (!val || !name || !delta) {
		currentMeta("pick a target, name the counter, give a delta", true);
		return;
	}
	const action = { kind: "ADJUST_COUNTERS", counter_name: name, delta };
	if (val.startsWith("seat:")) action.target_seat = Number(val.slice(5));
	else action.on_object = Number(val.slice(4));
	if (editDraft) submitEdited(action);
	else submit(action);
}

/** Resolve a typed card name once: exact cache, then the lookup
    surface, then the search's best row. */
async function resolveCard(q) {
	const key = q.toLowerCase();
	if (cardCache.has(key)) return cardCache.get(key);
	const data = await api.card(q);
	let card = data.card || null;
	if (!card && data.matches?.length) card = data.matches[0];
	if (card) cardCache.set(key, card);
	return card;
}

/* ---------- card autocomplete ---------- */

function cardSearchDebounced() {
	clearTimeout(searchTimer);
	searchTimer = setTimeout(cardSearch, 180);
}

async function cardSearch() {
	const q = $("play-card").value.trim();
	const box = $("play-card-list");
	if (q.length < 2) {
		box.hidden = true;
		return;
	}
	let matches = [];
	try {
		const data = await api.cardSearch(q, 6);
		matches = data.matches || [];
	} catch (_) {
		return;
	}
	clear(box);
	if (!matches.length) {
		box.hidden = true;
		return;
	}
	for (const m of matches) {
		box.append(el("button", {
			class: "play-card-opt",
			attrs: { type: "button", role: "option", "data-card": m.name, title: m.type_line || m.name },
			on: {
				click: () => {
					$("play-card").value = m.name;
					cardCache.set(m.name.toLowerCase(), m);
					hideCardList();
				},
			},
		},
			el("b", { text: m.name }),
			m.type_line ? el("span", { class: "play-card-type", text: ` ${m.type_line.split(" — ")[0]}` }) : null,
			m.mana_cost ? el("i", { class: "play-card-cost", text: ` ${m.mana_cost}` }) : null,
		));
	}
	box.hidden = false;
}

function hideCardList() {
	$("play-card-list").hidden = true;
}

/** The composer's seat/object target selects follow the board. */
function renderComposerTargets() {
	if (!state || (state.status !== "active" && state.status !== "finished")) return;
	for (const selID of ["play-damage-target", "play-counter-target"]) {
		const sel = $(selID);
		if (!sel) continue;
		const prev = sel.value;
		clear(sel);
		sel.append(el("option", { text: "target…", attrs: { value: "" } }));
		for (const seat of state.order || []) {
			sel.append(el("option", { text: seatName(state, seat), attrs: { value: `seat:${seat}` } }));
		}
		for (const o of Object.values(state.objects || {})) {
			if (o.zone !== "battlefield" || o.phased) continue;
			sel.append(el("option", { text: objectName(o), attrs: { value: `obj:${o.id}` } }));
		}
		if ([...sel.options].some((o) => o.value === prev)) sel.value = prev;
	}
}

/* ---------- the window-manager contract ---------- */

export const tool = {
	mount(host) {
		const view = $("play-view");
		host.append(view);
		view.hidden = false;
		mounted = true;
		if (!wired) {
			wire();
			wired = true;
		}
		if (!games.length) loadGames();
		else if (gameID && !streamCtl) openGame(gameID);
		return {
			destroy() {
				mounted = false;
				// The stream stops with the window: nothing else consumes
				// it yet, unlike the dice curtain.
				stopStream();
			},
		};
	},
};
