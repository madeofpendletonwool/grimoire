// The play surface's view model: the DOM-free half of play.js, here so
// node --test can run it (ADR 14's rule for jstest/ — no DOM, no fetch,
// only the derivations the panes paint from).
//
// Nothing here recomputes game state — the server's fold is the truth and
// play.js re-reads it after every ordinal that lands. What this module
// owns is spelling: log lines a table can read at a glance, the board's
// display values, and the current-action pane's notion of "the last
// thing that was applied", which is also the undo target.

/* ---------- spelling the game ---------- */

/** The steps the engine walks, in CR order, with table-facing names. */
const STEP_NAMES = {
	"beginning|untap": "untap",
	"beginning|upkeep": "upkeep",
	"beginning|draw": "draw",
	"precombat_main|main": "precombat main",
	"combat|beginning_of_combat": "beginning of combat",
	"combat|declare_attackers": "declare attackers",
	"combat|declare_blockers": "declare blockers",
	"combat|combat_damage": "combat damage",
	"combat|end_of_combat": "end of combat",
	"postcombat_main|main": "postcombat main",
	"end|end": "end step",
	"end|cleanup": "cleanup",
};

/** The one-line position every pane's header agrees on. */
export function stepLabel(phase, step) {
	return STEP_NAMES[`${phase}|${step}`] || step || phase || "";
}

/** A seat's display name — the seated name, else "Seat N". */
export function seatName(state, seat) {
	const p = state?.seats?.[seat];
	return (p && p.name) || `Seat ${seat}`;
}

/** seat → name for the whole table. */
export function seatNames(state) {
	const out = {};
	for (const [seat, p] of Object.entries(state?.seats || {})) {
		out[seat] = p.name || `Seat ${seat}`;
	}
	return out;
}

/** The turn strip's headline: game · turn · whose turn · step · priority. */
export function turnLine(game, state) {
	if (!state) return "";
	if (state.status === "setup") return "setting up";
	if (state.status === "finished") return "finished";
	const parts = [`T${state.turn || 1}`, `${seatName(state, state.turn_seat)}'s turn`, stepLabel(state.phase, state.step)];
	if (state.priority_seat) parts.push(`priority: ${seatName(state, state.priority_seat)}`);
	const line = parts.join(" · ");
	return game?.name ? `${game.name} · ${line}` : line;
}

/** A count-only value the engine may honestly not know: "?" is a value. */
export function formatCount(count) {
	if (!count || !count.known) return "?";
	return String(count.n);
}

/** An object's display name: its card, its token spec, or an honest blank. */
export function objectName(o) {
	if (!o) return "";
	if (o.identity?.card) return o.identity.card;
	if (o.identity?.token?.name) return o.identity.token.name;
	return "unknown card";
}

/**
 * Computed power/toughness — the JS read of the same layer walk the
 * engine's Characteristics performs for P/T: pt_set, then pt_modify in
 * timestamp order, then the two counter kinds, then the swap. Unknown
 * stays unknown (a bonus on a stat nobody declared is not a stat), which
 * is why {known:false} is a real answer and not a failure.
 */
export function computedPT(o) {
	const base = o?.base || {};
	let power = numOrNull(base.power);
	let toughness = numOrNull(base.toughness);
	const mods = o?.modifiers || [];
	for (const layer of ["pt_set", "pt_modify"]) {
		for (const mod of mods) {
			if ((mod.layer || "") !== layer) continue;
			const d = mod.delta || {};
			if (layer === "pt_set") {
				if (d.set_power != null) power = d.set_power;
				if (d.set_toughness != null) toughness = d.set_toughness;
			} else {
				if (d.power != null && power != null) power += d.power;
				if (d.toughness != null && toughness != null) toughness += d.toughness;
			}
		}
	}
	const counters = o?.counters || {};
	if (counters["+1/+1"]) {
		if (power != null) power += counters["+1/+1"];
		if (toughness != null) toughness += counters["+1/+1"];
	}
	if (counters["-1/-1"]) {
		if (power != null) power -= counters["-1/-1"];
		if (toughness != null) toughness -= counters["-1/-1"];
	}
	for (const mod of mods) {
		if (mod.layer === "pt_switch" && mod.delta?.swap) {
			[power, toughness] = [toughness, power];
			break;
		}
	}
	return { power, toughness, known: power != null || toughness != null };
}

/** The P/T a chip renders — "7/7", "3/1*", or nothing at all. */
export function ptLine(o) {
	const pt = computedPT(o);
	if (!pt.known) return "";
	return `${pt.power == null ? "?" : pt.power}/${pt.toughness == null ? "?" : pt.toughness}`;
}

/**
 * Computed type line — base types plus the type layer, case-insensitively
 * de-duplicated. What the combat drafts key on ("is this a Creature?"),
 * computed rather than stored, the same discipline as P/T.
 */
export function computedTypes(o) {
	const types = [...((o?.base?.types) || [])];
	for (const mod of o?.modifiers || []) {
		if ((mod.layer || "") !== "type") continue;
		const d = mod.delta || {};
		for (const t of d.remove_types || []) {
			const i = types.findIndex((x) => x.toLowerCase() === String(t).toLowerCase());
			if (i >= 0) types.splice(i, 1);
		}
		for (const t of d.add_types || []) {
			if (!types.some((x) => x.toLowerCase() === String(t).toLowerCase())) types.push(t);
		}
	}
	return types;
}

/** Whether the object's computed type line carries the type. */
export function isType(o, type) {
	const want = String(type).toLowerCase();
	return computedTypes(o).some((t) => t.toLowerCase() === want);
}

/** Counters as display chips: named, arbitrary, zero hidden. */
export function counterChips(counters) {
	return Object.entries(counters || {})
		.filter(([, n]) => n !== 0)
		.sort((a, b) => a[0].localeCompare(b[0]))
		.map(([name, n]) => ({ name, n }));
}

/** Commander tax for a seat's commander, derived from the fold's casts. */
export function commanderTax(state, seat) {
	const p = state?.seats?.[seat];
	if (!p?.commander) return 0;
	return 2 * (state.commander_casts?.[p.commander] || 0);
}

/** Zone tallies the seat strip shows: the count-only pair plus the object counts. */
export function zoneTally(state, seat) {
	const p = state?.seats?.[seat] || {};
	let graveyard = 0, exile = 0, command = 0;
	for (const o of Object.values(state?.objects || {})) {
		if (o.owner !== seat) continue;
		if (o.zone === "graveyard") graveyard++;
		else if (o.zone === "exile") exile++;
		else if (o.zone === "command") command++;
	}
	return { hand: p.hand, library: p.library, graveyard, exile, command };
}

/** The seat the composer acts as by default: who holds priority, else whose turn. */
export function defaultActingSeat(state) {
	if (!state || state.status !== "active") return 0;
	return state.priority_seat || state.turn_seat;
}

/** May someone pass priority right now (untap and cleanup: nobody may). */
export function canPass(state) {
	return state?.status === "active" && !!state.priority_seat;
}

/* ---------- the log's voice ---------- */

const withSource = (line, ev) =>
	ev.source_card ? `${line} (${ev.source_card})` : line;

/**
 * One log line per event — the sentence a table would say. `state` is the
 * freshest fold (it may postdate the event after a rewind; names are what
 * the board shows now, which is the honest spelling for a live log).
 */
export function describeEvent(ev, state) {
	const names = seatNames(state);
	const who = (seat) => names[seat] || (seat ? `Seat ${seat}` : "");
	const objName = (id) => objectName(state?.objects?.[id]) || `object ${id}`;
	const actor = ev.actor_seat ? `${who(ev.actor_seat)} ` : "";
	const target = ev.target_seat ? who(ev.target_seat) : ev.target_object ? objName(ev.target_object) : "";

	switch (ev.kind) {
		case "GAME_STARTED":
			return `the game begins — ${Object.keys(names).length} seats, ${ev.format || "commander"}`;
		case "GAME_ENDED":
			return `the game ends${ev.reason ? ` — ${ev.reason}` : ""}`;
		case "TURN_STARTED":
			return `Turn ${ev.turn} — ${who(ev.turn_seat)}`;
		case "STEP_ENTERED":
			return `→ ${stepLabel(ev.phase, ev.step)}`;
		case "TURN_ENDED":
			return `turn ${ev.turn} ends`;
		case "PLAYER_LEFT":
			return `${who(ev.target_seat)} leaves the game${ev.cause && ev.cause !== "concession" ? ` (${ev.cause})` : ""}`;
		case "OBJECT_CEASED":
			return `${objName(ev.object)} ceases to exist`;
		case "PRIORITY_PASSED":
			return `${who(ev.actor_seat)} passes`;
		case "STACK_PUSHED":
			if (ev.mode === "cast") return `${objName(ev.object) || ev.card} goes on the stack`;
			if (ev.mode === "activated") return `${who(ev.controller)} activates ${ev.ability}`;
			return `${ev.card || objName(ev.source_obj)} triggers`;
		case "STACK_RESOLVED":
			return `${ev.card || objName(ev.object) || (ev.mode === "cast" ? "the spell" : "the ability")} resolves` +
				(ev.to_zone ? ` → ${ev.to_zone}` : "");
		case "OBJECT_CREATED": {
			const token = ev.identity?.token;
			if (token) {
				const pt = token.power != null && token.toughness != null ? `${token.power}/${token.toughness} ` : "";
				return `${actor}creates a ${pt}${token.name || "token"}`;
			}
			if (ev.to_zone === "graveyard" && ev.from === "library") return `${who(ev.owner)} mills ${ev.identity?.card || "a card"}`;
			return `${ev.identity?.card || "a card"} enters ${ev.to_zone || "play"}${ev.from ? ` from ${ev.from}` : ""}`;
		}
		case "ZONE_CHANGED":
			return zoneChangeLine(ev, { actor, objName, who });
		case "LAND_PLAYED":
			return `${actor}plays ${ev.card || objName(ev.object)}`;
		case "CREATURE_ETB":
			return `${ev.card || objName(ev.object)} enters`;
		case "DIED":
			return `${objName(ev.object)} dies${ev.cause ? ` (${ev.cause})` : ""}`;
		case "CAST":
			return `${actor}casts ${ev.card}${ev.from && ev.from !== "hand" ? ` from ${ev.from}` : ""}`;
		case "ATTACKERS_DECLARED":
			return `${actor}attacks — ${attackSummary(ev.attackers, names, state)}`;
		case "BLOCKERS_DECLARED":
			return `blocks — ${(ev.blockers || [])
				.map((b) => `${objName(b.blocker)}→${(b.attackers || []).map(objName).join(", ")}`)
				.join(" · ") || "none"}`;
		case "COMBAT_RESOLVED":
			return "combat damage is dealt";
		case "DAMAGE_DEALT":
			return withSource(`${target} takes ${ev.amount} damage${ev.combat ? " (combat)" : ""}`, ev);
		case "DAMAGE_MARKED":
			return `${objName(ev.object)} is marked ${ev.amount} damage`;
		case "LIFE_CHANGED":
			if (ev.to != null) return withSource(`${who(ev.target_seat)}'s life becomes ${ev.to}`, ev);
			if (ev.delta < 0) return withSource(`${who(ev.target_seat)} loses ${-ev.delta} life`, ev);
			return withSource(`${who(ev.target_seat)} gains ${ev.delta} life`, ev);
		case "COUNTER_CHANGED": {
			const on = ev.object ? objName(ev.object) : who(ev.target_seat);
			if (ev.to != null) return `${on}: ${ev.name} → ${ev.to}`;
			return `${on} ${ev.delta > 0 ? "gets" : "loses"} ${Math.abs(ev.delta)} ${ev.name} counter${Math.abs(ev.delta) === 1 ? "" : "s"}`;
		}
		case "FLAG_CHANGED":
			if (!ev.value) return `${who(ev.target_seat)} loses the ${ev.flag}`;
			return `${who(ev.target_seat)} ${ev.flag === "monarch" ? "becomes the monarch" : ev.flag === "initiative" ? "takes the initiative" : `gains ${ev.flag}`}`;
		case "TAP_CHANGED":
			return `${objName(ev.object)} ${ev.tapped ? "taps" : "untaps"}`;
		case "PHASE_CHANGED":
			return `${objName(ev.object)} phases ${ev.phased ? "out" : "in"}`;
		case "ATTACHED":
			return `${objName(ev.object)} attaches to ${objName(ev.target_object)}`;
		case "UNATTACHED":
			return `${objName(ev.object)} unattaches`;
		case "MODIFIER_ADDED":
			return `${objName(ev.object)}: ${modifierLabel(ev.modifier)}`;
		case "MODIFIER_REMOVED":
			return `${objName(ev.object)}: effect ends`;
		case "TRIGGER_FIRED":
			return `${ev.card || objName(ev.source_obj)} triggers`;
		case "CARD_DRAWN":
			return `${who(ev.target_seat)} draws ${ev.count}`;
		case "CARD_KNOWN":
			return `${who(ev.target_seat)} knows ${(ev.cards || []).join(", ")}`;
		case "CARD_REVEALED":
			return `${who(ev.target_seat)} reveals ${(ev.cards || []).join(", ")}`;
		case "EFFECT_DECLARED":
			return `effect — ${ev.effect}`;
		case "ZONE_COUNT_SET":
			return `${who(ev.target_seat)}'s ${ev.zone} set to ${ev.to}`;
		default:
			return ev.kind;
	}
}

function zoneChangeLine(ev, { actor, objName, who }) {
	const name = objName(ev.object);
	switch (ev.cause) {
		case "sacrifice": return `${actor}sacrifices ${name}`;
		case "destroy": return `${actor}destroys ${name}`;
		case "bounce": return `${actor}returns ${name} to hand`;
		case "exile": return `${actor}exiles ${name}`;
		case "draw": return `${who(ev.owner)} draws ${name}`;
		case "resolve": return `${name} resolves`;
		default:
			if (ev.to_zone === "battlefield") return `${actor}returns ${name} to the battlefield (${ev.cause || "move"})`;
			return `${name} moves to ${ev.to_zone}${ev.cause ? ` (${ev.cause})` : ""}`;
	}
}

function attackSummary(attackers, names, state) {
	return (attackers || [])
		.map((at) => {
			const who = at.target_seat ? (names[at.target_seat] || `Seat ${at.target_seat}`) : objectName(state?.objects?.[at.target_object]);
			return `${objectName(state?.objects?.[at.object])} → ${who || "?"}`;
		})
		.join(" · ") || "nothing";
}

/** A modifier's source name, the way the trace will spell it in 5b. */
export function modifierLabel(mod) {
	if (!mod) return "effect";
	if (mod.source_card) return `${mod.source_card}: ${mod.layer}`;
	return `${mod.layer}`;
}

/* ---------- the current action ---------- */

/**
 * Two rows share a batch when they sit on contiguous ords and carry the
 * same writer stamp — the uuid the store mints per Submit. Rows from
 * before the stamp fall back to cause equality, the best a repeating
 * cause allows.
 */
function sameBatch(a, b) {
	if (a.ord + 1 !== b.ord) return false;
	if (!!a.batch !== !!b.batch) return false;
	if (a.batch) return a.batch === b.batch;
	return !!a.cause && a.cause === b.cause;
}

const sortedLog = (events) => (events || []).filter((e) => e.ord).slice().sort((a, b) => a.ord - b.ord);

/**
 * The batch of events one Submit produced around an ordinal: the unit
 * amend rewrites and undo removes. `action` is the recorded cause
 * parsed — the prefill for the correction, straight from the log entry.
 */
export function actionBatchAt(events, ord) {
	const log = sortedLog(events);
	const idx = log.findIndex((e) => e.ord === ord);
	if (idx < 0) return null;
	let lo = idx, hi = idx;
	while (lo > 0 && sameBatch(log[lo - 1], log[lo])) lo--;
	while (hi + 1 < log.length && sameBatch(log[hi], log[hi + 1])) hi++;
	const cause = log[idx].cause || "";
	let action = null;
	if (cause) {
		try { action = JSON.parse(cause); } catch (_) { /* a cause we cannot parse is not ours to amend */ }
	}
	return {
		from: log[lo].ord,
		to: log[hi].ord,
		cause,
		action,
		events: log.slice(lo, hi + 1),
		undoTo: log[lo].ord - 1,
	};
}

/**
 * The last applied action and its event rows: the contiguous tail of the
 * log sharing one writer stamp. One Submit is one batch — the action's
 * own rows, the state-based sweep it triggered and the triggers it
 * flushed all ride the same stamp — so this tail is exactly "what the
 * engine last applied", and the ordinal before it is where undo rewinds
 * to.
 */
export function lastActionBatch(events) {
	const log = sortedLog(events);
	if (!log.length) return null;
	return actionBatchAt(log, log[log.length - 1].ord);
}

/**
 * The pane's ladder emphasis (MAD-331): an optimistic application — the
 * confirmation ladder's confirm rung, stamped on the action's cause —
 * stays marked "worth a look" until acknowledged, long after the
 * fresh-paint glow is gone. Auto needs no look; ask never applied.
 */
export function confirmHighlight(batch, acknowledged) {
	return !!batch
		&& batch.to > (acknowledged || 0)
		&& batch?.action?.disposition === "confirm";
}

/** The current-action pane's headline: the sentence for a submitted action. */
export function actionSummary(action, state) {
	if (!action) return "";
	const names = seatNames(state);
	const who = (s) => `${names[s] || `Seat ${s}`} `;
	const objName = (id) => objectName(state?.objects?.[id]) || `object ${id}`;
	switch (action.kind) {
		case "CAST": return `${who(action.seat)}casts ${action.card || "a card"}${action.from_zone && action.from_zone !== "hand" ? ` from ${action.from_zone}` : ""}`;
		case "PLAY_LAND": return `${who(action.seat)}plays ${action.card || "a land"}`;
		case "DRAW": return `${who(action.seat)}draws ${action.count || 1}`;
		case "MILL": return `${who(action.seat)}mills ${action.count}`;
		case "PASS_PRIORITY": return `${who(action.seat)}passes`;
		case "ADVANCE": return `${who(action.seat)}advances`;
		case "CREATE_TOKEN": {
			const t = action.token || {};
			const pt = t.power != null && t.toughness != null ? `${t.power}/${t.toughness} ` : "";
			return `${who(action.seat)}makes ${action.count || 1}× ${pt}${t.name || "token"}`;
		}
		case "CHANGE_LIFE": {
			const seat = action.target_seat || action.seat;
			if (action.to != null) return `${names[seat] || `Seat ${seat}`}'s life becomes ${action.to}`;
			return `${names[seat] || `Seat ${seat}`} ${action.delta < 0 ? "loses" : "gains"} ${Math.abs(action.delta)} life`;
		}
		case "DEAL_DAMAGE": {
			const target = action.target_seat ? (names[action.target_seat] || `Seat ${action.target_seat}`) : objName(action.target_object);
			return `${who(action.seat)}deals ${action.amount} to ${target}`;
		}
		case "ADJUST_COUNTERS":
		case "SET_COUNTERS": {
			const on = action.on_object ? objName(action.on_object) : (names[action.target_seat || action.seat] || `Seat ${action.target_seat || action.seat}`);
			return `${on}: ${action.counter_name} ${action.to != null ? `→ ${action.to}` : (action.delta > 0 ? "+" : "") + action.delta}`;
		}
		case "TAP": return action.all ? `${who(action.seat)}taps out` : `${who(action.seat)}taps ${objName(action.object)}`;
		case "UNTAP": return action.all ? `${who(action.seat)}untaps everything` : `${who(action.seat)}untaps ${objName(action.object)}`;
		case "MOVE_ZONE": return `${objName(action.object)} → ${action.to_zone}${action.cause ? ` (${action.cause})` : ""}`;
		case "DECLARE_ATTACKERS": return `${who(action.seat)}declares ${(action.attackers || []).length} attacker(s)`;
		case "DECLARE_BLOCKERS": return `${who(action.seat)}declares ${(action.blockers || []).length} blocker(s)`;
		case "RESOLVE_COMBAT": return `${who(action.seat)}resolves combat damage`;
		case "SET_FLAG": return `${names[action.target_seat || action.seat] || ""} ${action.value ? "" : "loses "}${action.flag}`.trim();
		case "START_GAME": return "the game begins";
		case "END_GAME": return `the game ends${action.reason ? ` — ${action.reason}` : ""}`;
		case "CONCEDE": return `${who(action.seat)}concedes`;
		case "SET_ZONE_COUNT": return `${names[action.target_seat || action.seat] || ""}'s ${action.zone} → ${action.to}`;
		case "REVEAL": return `${who(action.seat)}reveals ${(action.cards || []).join(", ")}`;
		default: return action.kind;
	}
}

/* ---------- card lookup → declared base ---------- */

/**
 * A cardView from /api/card becomes the BaseChars a CAST or PLAY_LAND
 * declares: the type line's words, the colors the mana cost spells, and
 * the numbers that are numbers. "*" and friends stay unknown — declared,
 * never invented (ADR 11).
 */
export function baseCharsFromCard(card) {
	if (!card) return {};
	const types = [];
	for (const half of String(card.type_line || "").split("—")) {
		for (const word of half.trim().split(/\s+/)) {
			if (word) types.push(word);
		}
	}
	const colors = [];
	for (const pip of String(card.mana_cost || "").matchAll(/\{([^}]+)\}/g)) {
		for (const ch of pip[1]) {
			if ("WUBRG".includes(ch) && !colors.includes(ch)) colors.push(ch);
		}
	}
	return {
		name: card.name || "",
		types,
		colors,
		power: numOrNull(card.power),
		toughness: numOrNull(card.toughness),
		loyalty: numOrNull(card.loyalty),
	};
}

function numOrNull(v) {
	if (v == null || v === "") return null;
	const n = Number.parseInt(String(v), 10);
	return Number.isFinite(n) ? n : null;
}
