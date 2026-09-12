// The play surface's view model (playvm.js) is DOM-free by design — these
// are the derivations the board, log and current-action panes paint from,
// kept honest so the tracker never spells a game state the engine would
// not recognize. The P/T walk mirrors engine.Characteristics for the
// display half; the batch logic is what undo and amend stand on.

import test from "node:test";
import assert from "node:assert/strict";

import {
	stepLabel, seatName, turnLine, formatCount, objectName, ptLine, computedPT,
	computedTypes, isType, counterChips, commanderTax, zoneTally,
	defaultActingSeat, canPass, describeEvent, lastActionBatch, actionSummary,
	baseCharsFromCard,
} from "../web/static/js/playvm.js";

/* ---------- fixtures ---------- */

const names = { 1: "Collin", 2: "Bob" };

const state = {
	status: "active",
	turn: 7,
	turn_seat: 1,
	phase: "precombat_main",
	step: "main",
	priority_seat: 1,
	order: [1, 2],
	seats: {
		1: { seat: 1, name: "Collin", life: 37, alive: true, commander: "Atraxa, Praetors' Voice",
			hand: { known: true, n: 7 }, library: { known: false, n: 0 },
			counters: { poison: 0, energy: 4 }, commander_damage: { "Krenko, Mob Boss": 3 } },
		2: { seat: 2, name: "Bob", life: 28, alive: true, commander: "Krenko, Mob Boss",
			hand: { known: true, n: 5 }, library: { known: true, n: 92 },
			counters: { poison: 2 } },
	},
	objects: {
		11: { id: 11, zone: "battlefield", controller: 1, owner: 1, tapped: true,
			identity: { card: "Sol Ring" }, base: { name: "Sol Ring", types: ["Artifact"] } },
		12: { id: 12, zone: "battlefield", controller: 2, owner: 2,
			identity: { card: "Atraxa, Praetors' Voice" },
			base: { name: "Atraxa", types: ["Creature"], power: 4, toughness: 4 },
			counters: { "+1/+1": 2 },
			modifiers: [
				{ layer: "pt_modify", duration: "until_end_of_turn", source_card: "Giant Growth",
					delta: { power: 3, toughness: 3 } },
			],
			damage: 1 },
		13: { id: 13, zone: "graveyard", controller: 2, owner: 2,
			identity: { card: "Counterspell" }, base: { types: ["Instant"] } },
	},
	commander_casts: { "Atraxa, Praetors' Voice": 1 },
};

/* ---------- spelling ---------- */

test("stepLabel spells the CR structure the way a table says it", () => {
	assert.equal(stepLabel("beginning", "untap"), "untap");
	assert.equal(stepLabel("precombat_main", "main"), "precombat main");
	assert.equal(stepLabel("postcombat_main", "main"), "postcombat main");
	assert.equal(stepLabel("combat", "combat_damage"), "combat damage");
	assert.equal(stepLabel("end", "end"), "end step");
	assert.equal(stepLabel("weird", "odd"), "odd"); // unknown spells itself
});

test("seatName and turnLine: names, position, priority", () => {
	assert.equal(seatName(state, 1), "Collin");
	assert.equal(seatName(state, 9), "Seat 9");
	assert.equal(seatName(null, 2), "Seat 2");
	assert.equal(turnLine({ name: "Friday Commander" }, state),
		"Friday Commander · T7 · Collin's turn · precombat main · priority: Collin");
	// No priority holder (untap, cleanup) reads without the tail.
	assert.equal(turnLine(null, { ...state, priority_seat: 0 }),
		"T7 · Collin's turn · precombat main");
});

test("formatCount: unknown is a value, not zero", () => {
	assert.equal(formatCount({ known: true, n: 7 }), "7");
	assert.equal(formatCount({ known: false, n: 0 }), "?");
	assert.equal(formatCount(null), "?");
});

test("objectName: card, token, or an honest unknown", () => {
	assert.equal(objectName(state.objects[11]), "Sol Ring");
	assert.equal(objectName({ identity: { token: { name: "Soldier" } } }), "Soldier");
	assert.equal(objectName({ identity: {} }), "unknown card");
	assert.equal(objectName(null), "");
});

/* ---------- computed characteristics ---------- */

test("computedPT walks pt_set before pt_modify, counters after, swap last", () => {
	// Atraxa: base 4/4 + Giant Growth 3/3 + two +1/+1 counters = 9/9.
	assert.deepEqual(computedPT(state.objects[12]), { power: 9, toughness: 9, known: true });
	// A set layer overrides the base before modifies apply (Turn to Frog
	// then Giant Growth: 1/1 + 3/3).
	const frogged = { base: { power: 4, toughness: 4 }, modifiers: [
		{ layer: "pt_modify", delta: { power: 3, toughness: 3 } },
		{ layer: "pt_set", delta: { set_power: 1, set_toughness: 1 } },
	] };
	assert.deepEqual(computedPT(frogged), { power: 4, toughness: 4, known: true });
	// Counters apply after pt_modify; -1/-1 subtracts.
	const counters = { base: { power: 2, toughness: 2 },
		counters: { "+1/+1": 1, "-1/-1": 2 } };
	assert.deepEqual(computedPT(counters), { power: 1, toughness: 1, known: true });
	// The switch swaps last of all.
	const swapped = { base: { power: 1, toughness: 3 }, modifiers: [
		{ layer: "pt_switch", delta: { swap: true } },
	] };
	assert.deepEqual(computedPT(swapped), { power: 3, toughness: 1, known: true });
	// A bonus on a stat nobody declared is not a stat.
	assert.deepEqual(computedPT({ base: {}, modifiers: [
		{ layer: "pt_modify", delta: { power: 2, toughness: 2 } },
	] }), { power: null, toughness: null, known: false });
	// Unknown base P/T on a real card stays unreadable, not invented.
	assert.equal(ptLine(state.objects[11]), "");
});

test("computedTypes and isType walk the type layer", () => {
	assert.equal(isType(state.objects[12], "Creature"), true);
	assert.equal(isType(state.objects[11], "Creature"), false);
	const animated = { base: { types: ["Land"] }, modifiers: [
		{ layer: "type", delta: { add_types: ["Creature"] } },
	] };
	assert.deepEqual(computedTypes(animated), ["Land", "Creature"]);
	const stripped = { base: { types: ["Land", "Creature"] }, modifiers: [
		{ layer: "type", delta: { remove_types: ["creature"] } },
	] };
	assert.deepEqual(computedTypes(stripped), ["Land"]); // case-insensitive
});

test("counterChips hides zeros and sorts by name", () => {
	assert.deepEqual(counterChips({ poison: 0, energy: 4 }),
		[{ name: "energy", n: 4 }]);
	assert.deepEqual(counterChips(undefined), []);
});

test("commanderTax derives from the fold's cast count", () => {
	assert.equal(commanderTax(state, 1), 2);
	assert.equal(commanderTax(state, 2), 0);
	assert.equal(commanderTax(state, 9), 0);
});

test("zoneTally counts object zones and passes the count-only pair through", () => {
	assert.deepEqual(zoneTally(state, 2),
		{ hand: { known: true, n: 5 }, library: { known: true, n: 92 }, graveyard: 1, exile: 0, command: 0 });
});

/* ---------- acting seat and pass enablement ---------- */

test("defaultActingSeat follows priority, then the turn; canPass needs a holder", () => {
	assert.equal(defaultActingSeat(state), 1);
	assert.equal(defaultActingSeat({ ...state, priority_seat: 0 }), 1); // turn seat
	assert.equal(defaultActingSeat({ status: "setup" }), 0);
	assert.equal(canPass(state), true);
	assert.equal(canPass({ ...state, priority_seat: 0 }), false);
	assert.equal(canPass(null), false);
});

/* ---------- the log's voice ---------- */

test("describeEvent reads the way a table talks", () => {
	const line = (ev) => describeEvent(ev, state);
	assert.equal(line({ kind: "TURN_STARTED", turn: 7, turn_seat: 1 }), "Turn 7 — Collin");
	assert.equal(line({ kind: "STEP_ENTERED", phase: "combat", step: "declare_attackers" }),
		"→ declare attackers");
	assert.equal(line({ kind: "LAND_PLAYED", actor_seat: 2, card: "Forest" }), "Bob plays Forest");
	assert.equal(line({ kind: "CAST", actor_seat: 1, card: "Rhystic Study" }),
		"Collin casts Rhystic Study");
	assert.equal(line({ kind: "CAST", actor_seat: 1, card: "Atraxa, Praetors' Voice", from: "command" }),
		"Collin casts Atraxa, Praetors' Voice from command");
	assert.equal(line({ kind: "LIFE_CHANGED", target_seat: 2, delta: -3, source_card: "Lightning Bolt" }),
		"Bob loses 3 life (Lightning Bolt)");
	assert.equal(line({ kind: "LIFE_CHANGED", target_seat: 1, to: 40 }), "Collin's life becomes 40");
	assert.equal(line({ kind: "TAP_CHANGED", object: 11, tapped: true }), "Sol Ring taps");
	assert.equal(line({ kind: "TAP_CHANGED", object: 11 }), "Sol Ring untaps");
	assert.equal(line({ kind: "ZONE_CHANGED", object: 11, cause: "sacrifice", actor_seat: 1 }),
		"Collin sacrifices Sol Ring");
	assert.equal(line({ kind: "ZONE_CHANGED", object: 12, cause: "destroy", actor_seat: 1 }),
		"Collin destroys Atraxa, Praetors' Voice");
	assert.equal(line({ kind: "DIED", object: 12, cause: "sba_lethal_damage" }),
		"Atraxa, Praetors' Voice dies (sba_lethal_damage)");
	assert.equal(line({ kind: "PLAYER_LEFT", target_seat: 2, cause: "concession" }),
		"Bob leaves the game");
	assert.equal(line({ kind: "PLAYER_LEFT", target_seat: 2, cause: "sba_poison" }),
		"Bob leaves the game (sba_poison)");
	assert.equal(line({ kind: "CARD_DRAWN", target_seat: 1, count: 2 }), "Collin draws 2");
	assert.equal(line({ kind: "DAMAGE_DEALT", target_seat: 1, amount: 6, combat: true }),
		"Collin takes 6 damage (combat)");
	assert.equal(line({ kind: "FLAG_CHANGED", target_seat: 1, flag: "monarch", value: "yes" }),
		"Collin becomes the monarch");
	assert.equal(line({ kind: "COUNTER_CHANGED", object: 12, name: "+1/+1", delta: 2 }),
		"Atraxa, Praetors' Voice gets 2 +1/+1 counters");
	assert.equal(line({ kind: "TRIGGER_FIRED", card: "Rhystic Study" }), "Rhystic Study triggers");
	assert.equal(line({ kind: "PRIORITY_PASSED", actor_seat: 1 }), "Collin passes");
	// An object the fold no longer holds (post-rewind tail) names by id,
	// never by a guessed card.
	assert.equal(line({ kind: "TAP_CHANGED", object: 99, tapped: true }), "object 99 taps");
});

/* ---------- the current action ---------- */

test("lastActionBatch: the contiguous tail sharing one cause is the undo target", () => {
	const cause = JSON.stringify({ kind: "CAST", seat: 1, card: "Rhystic Study", source: "tap" });
	const sweep = JSON.stringify({ kind: "SBA", source: "system" });
	const log = [
		{ ord: 1, id: "a", cause: JSON.stringify({ kind: "ADVANCE", seat: 1 }) },
		{ ord: 2, id: "b", cause: cause },
		{ ord: 3, id: "c", cause: cause },
		{ ord: 4, id: "d", cause: cause },
	];
	const batch = lastActionBatch(log);
	assert.equal(batch.from, 2);
	assert.equal(batch.to, 4);
	assert.equal(batch.undoTo, 1);
	assert.equal(batch.action.card, "Rhystic Study");
	assert.equal(batch.events.length, 3);

	// The whole log from one cause (GAME_STARTED's batch) rewinds to zero.
	const fresh = lastActionBatch([{ ord: 1, id: "x", cause }, { ord: 2, id: "y", cause }]);
	assert.equal(fresh.undoTo, 0);

	assert.equal(lastActionBatch([]), null);
	assert.equal(lastActionBatch(null), null);

	// A cause that is not action JSON is carried but not parsed.
	const opaque = lastActionBatch([{ ord: 5, id: "z", cause: "not json" }]);
	assert.equal(opaque.action, null);
	assert.equal(opaque.undoTo, 4);
});

test("actionSummary spells the action the pane announces", () => {
	assert.equal(actionSummary({ kind: "CAST", seat: 1, card: "Rhystic Study" }, state),
		"Collin casts Rhystic Study");
	assert.equal(actionSummary({ kind: "PLAY_LAND", seat: 2, card: "Forest" }, state),
		"Bob plays Forest");
	assert.equal(actionSummary({ kind: "CHANGE_LIFE", seat: 1, target_seat: 2, delta: -3 }, state),
		"Bob loses 3 life");
	assert.equal(actionSummary({ kind: "CREATE_TOKEN", seat: 1, count: 3,
		token: { name: "Soldier", power: 1, toughness: 1 } }, state),
		"Collin makes 3× 1/1 Soldier");
	assert.equal(actionSummary({ kind: "DRAW", seat: 1, count: 2 }, state), "Collin draws 2");
	assert.equal(actionSummary(null, state), "");
});

/* ---------- card lookup → declared base ---------- */

test("baseCharsFromCard declares only what the lookup actually carries", () => {
	const base = baseCharsFromCard({
		name: "Atraxa, Praetors' Voice",
		type_line: "Legendary Creature — Phyrexian Angel",
		mana_cost: "{2}{G}{W}{U}{B}",
		power: "4", toughness: "4",
	});
	assert.equal(base.name, "Atraxa, Praetors' Voice");
	assert.deepEqual(base.types, ["Legendary", "Creature", "Phyrexian", "Angel"]);
	assert.deepEqual(base.colors.sort(), ["B", "G", "U", "W"]);
	assert.equal(base.power, 4);
	assert.equal(base.toughness, 4);
	assert.equal(base.loyalty, null);

	// "*" is unknown, hybrid pips carry both halves, loyalty parses.
	const sparky = baseCharsFromCard({ name: "Chandra", type_line: "Legendary Planeswalker — Chandra",
		mana_cost: "{2}{R}", loyalty: "4", power: "*", toughness: "*" });
	assert.equal(sparky.power, null);
	assert.equal(sparky.loyalty, 4);
	const hybrid = baseCharsFromCard({ name: "Figure of Destiny", mana_cost: "{R/W}" });
	assert.deepEqual(hybrid.colors.sort(), ["R", "W"]);
	assert.deepEqual(baseCharsFromCard(null), {});
});
