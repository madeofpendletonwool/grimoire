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
	defaultActingSeat, canPass, describeEvent, lastActionBatch, actionBatchAt,
	actionSummary, baseCharsFromCard, confirmHighlight, voicePlan,
	ptRowText, ptRowMeta, changeRowText, deathHeadline, damageRowText, turnHeadline,
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

test("the writer's batch stamp is the authority — identical repeats stay separate", () => {
	const cause = JSON.stringify({ kind: "ADVANCE", seat: 1, source: "tap" });
	const log = [
		{ ord: 1, id: "a", batch: "b1", cause },
		{ ord: 2, id: "b", batch: "b1", cause },
		{ ord: 3, id: "c", batch: "b2", cause }, // the same action submitted again
		{ ord: 4, id: "d", batch: "b3", cause: JSON.stringify({ kind: "DRAW", seat: 1 }) },
	];
	// Undo removes exactly the last Submit — one advance, not all three.
	assert.equal(lastActionBatch(log).undoTo, 3);
	// Amend at any of a batch's rows addresses the whole batch.
	const mid = actionBatchAt(log, 1);
	assert.equal(mid.from, 1);
	assert.equal(mid.to, 2);
	assert.equal(mid.action.kind, "ADVANCE");
	// A stamp on one side and not the other is a boundary (legacy rows).
	const mixed = [
		{ ord: 1, id: "x", cause },
		{ ord: 2, id: "y", batch: "b9", cause },
	];
	assert.equal(actionBatchAt(mixed, 1).to, 1);
	assert.equal(actionBatchAt(mixed, 2).from, 2);
});

test("actionBatchAt finds the amend target from any log entry", () => {
	const cast = JSON.stringify({ kind: "CAST", seat: 1, card: "Rhystic Study", source: "tap" });
	const log = [
		{ ord: 1, id: "a", batch: "b1", cause: JSON.stringify({ kind: "ADVANCE", seat: 1 }) },
		{ ord: 2, id: "b", batch: "b2", cause: cast },
		{ ord: 3, id: "c", batch: "b2", cause: cast },
		{ ord: 4, id: "d", batch: "b3", cause: JSON.stringify({ kind: "DRAW", seat: 2 }) },
	];
	// Any row of the batch resolves to the batch: the ✎ on either log
	// entry corrects the same action.
	for (const ord of [2, 3]) {
		const batch = actionBatchAt(log, ord);
		assert.equal(batch.from, 2);
		assert.equal(batch.to, 3);
		assert.equal(batch.undoTo, 1);
		assert.equal(batch.action.card, "Rhystic Study");
		assert.equal(batch.events.length, 2);
	}
	assert.equal(actionBatchAt(log, 4).from, 4);
	assert.equal(actionBatchAt(log, 99), null);
	assert.equal(actionBatchAt([], 1), null);
	assert.equal(actionBatchAt(null, 1), null);
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

/* ---------- the confirmation ladder's pane emphasis (MAD-331) ---------- */

test("confirmHighlight marks an optimistic application until acknowledged", () => {
	const confirmBatch = { to: 12, action: { kind: "CAST", disposition: "confirm" } };
	const autoBatch = { to: 12, action: { kind: "CHANGE_LIFE", disposition: "auto" } };
	// Fresh and confirm-rung: marked.
	assert.equal(confirmHighlight(confirmBatch, 10), true);
	// Acknowledged: the mark clears even though the disposition rides on.
	assert.equal(confirmHighlight(confirmBatch, 12), false);
	assert.equal(confirmHighlight(confirmBatch, 20), false);
	// Auto never needed a look.
	assert.equal(confirmHighlight(autoBatch, 10), false);
	// No batch, no mark.
	assert.equal(confirmHighlight(null, 0), false);
});

/* ---------- push-to-talk's path decision (MAD-332) ---------- */

test("voicePlan: Web Speech wins, the server path backs it up, else absent", () => {
	// Web Speech here: interims are the point, even when the server
	// endpoint is also configured.
	assert.equal(voicePlan({ webSpeech: true, recorder: true, serverTranscribe: true }), "web");
	assert.equal(voicePlan({ webSpeech: true, recorder: false, serverTranscribe: false }), "web");
	// No Web Speech (Firefox, Safari): a recorder plus the configured
	// endpoint is the server path.
	assert.equal(voicePlan({ webSpeech: false, recorder: true, serverTranscribe: true }), "server");
	// The endpoint unset means the affordance is simply absent — the
	// EMBEDDINGS_* contract — and a browser that cannot record a clip
	// has no path either.
	assert.equal(voicePlan({ webSpeech: false, recorder: true, serverTranscribe: false }), null);
	assert.equal(voicePlan({ webSpeech: false, recorder: false, serverTranscribe: true }), null);
	assert.equal(voicePlan({ webSpeech: false, recorder: false, serverTranscribe: false }), null);
	assert.equal(voicePlan(), null);
});

/* ---------- provenance rows (MAD-334) ---------- */

// The engine's PTTrace rows, spelled the way the model doc's worked
// example reads — the stack a table argues with, one line per row.
test("ptRowText and ptRowMeta spell the 7/7 stack", () => {
	assert.equal(ptRowText({ kind: "base", label: "Grizzly Bears", power: 2, toughness: 2 }), "base 2/2");
	assert.equal(ptRowText({ kind: "modifier", label: "Glorious Anthem", power: 1, toughness: 1,
		layer: "pt_modify", duration: "while_source_present" }), "+1/+1 from Glorious Anthem");
	assert.equal(ptRowText({ kind: "counter", label: "+1/+1 counter", power: 1, toughness: 1 }), "+1/+1 +1/+1 counter");
	assert.equal(ptRowText({ kind: "modifier", label: "Giant Growth", power: 3, toughness: 3,
		layer: "pt_modify", duration: "until_end_of_turn" }), "+3/+3 from Giant Growth");
	assert.equal(ptRowText({ kind: "total", power: 7, toughness: 7 }), "7/7");
	assert.equal(ptRowText({ kind: "modifier", label: "Turn to Frog", power: 1, toughness: 1,
		layer: "pt_set", note: "base becomes" }), "base becomes 1/1 from Turn to Frog");

	// The side notes: layer, duration, and the honest source-gone case.
	assert.equal(ptRowMeta({ kind: "modifier", layer: "pt_modify", duration: "until_end_of_turn" }),
		"pt_modify · until end of turn");
	assert.equal(ptRowMeta({ kind: "modifier", layer: "pt_modify", duration: "permanent" }), "pt_modify");
	assert.equal(ptRowMeta({ kind: "modifier", layer: "pt_modify", duration: "while_source_present", source_gone: true }),
		"pt_modify · while source present · not applied — source has left the battlefield");
	assert.equal(ptRowMeta({ kind: "total", note: "P/T unknown" }), "P/T unknown");
});

test("changeRowText spells the non-P/T layers", () => {
	assert.equal(changeRowText({ label: "Act of Treason", change: "controller → Bob" }),
		"Act of Treason: controller → Bob");
	assert.equal(changeRowText({ label: "Conspiracy", change: "gains Goblin" }),
		"Conspiracy: gains Goblin");
	assert.equal(changeRowText(null), "");
});

test("deathHeadline and damageRowText carry the rule and the sources", () => {
	const rep = { name: "Snapdax", turn: 5, cause_note: "toughness was 0 or less (CR 704.5f)" };
	assert.equal(deathHeadline(rep, "Bob adds Last Gasp"),
		"Snapdax died on turn 5: toughness was 0 or less (CR 704.5f) — after Bob adds Last Gasp");
	assert.equal(deathHeadline({ name: "Bear", cause_note: "x" }, ""),
		"Bear died: x");
	assert.equal(damageRowText({ amount: 1, source: "King Cheetah", deathtouch: true }),
		"1 from King Cheetah (deathtouch — any amount is lethal)");
	assert.equal(damageRowText({ amount: 3, source: "Lightning Bolt" }), "3 from Lightning Bolt");
});

test("turnHeadline names the turn's seat", () => {
	assert.equal(turnHeadline(5, "Bob"), "Turn 5 — Bob");
	assert.equal(turnHeadline(2, ""), "Turn 2 — ");
});

/* ---------- the trigger registry (MAD-335) ---------- */

import {
	nudgeText, queueResolutionOrder, queueOrderAfterMove,
	triggerKindLabel, triggerRowText, TRIGGER_KINDS,
} from "../web/static/js/playvm.js";

test("nudgeText spells the three don't-forget reminders", () => {
	assert.equal(nudgeText({ kind: "waiting", card: "Phyrexian Arena", effect: "lose 1 life, draw a card" }),
		"Phyrexian Arena is waiting to stack — lose 1 life, draw a card");
	assert.equal(nudgeText({ kind: "unresolved", card: "Rhystic Study", effect: "may draw a card" }),
		"Rhystic Study's trigger is unresolved — may draw a card");
	assert.equal(nudgeText({ kind: "unused_attack", card: "Rampaging Raptor", effect: "deal 2 damage" }),
		"unused attack trigger: Rampaging Raptor — deal 2 damage");
	assert.equal(nudgeText(null), "");
});

test("triggerKindLabel and triggerRowText spell registrations for the strip", () => {
	assert.equal(triggerKindLabel("OPPONENT_CASTS_SPELL"), "whenever an opponent casts");
	assert.equal(triggerKindLabel("SOMETHING_ELSE"), "SOMETHING_ELSE");
	assert.equal(triggerRowText({ event_kind: "UPKEEP", effect: "draw a card" }),
		"at your upkeep — draw a card");
	assert.equal(triggerRowText(null), "");
	// The picker's vocabulary is exactly the engine's seven kinds.
	assert.equal(TRIGGER_KINDS.length, 7);
	for (const k of ["LAND_PLAYED", "CREATURE_ETB", "UPKEEP", "OPPONENT_CASTS_SPELL", "ATTACKS", "DIES", "END_STEP"]) {
		assert.ok(TRIGGER_KINDS.some((row) => row.value === k), `missing ${k}`);
	}
});

test("queueResolutionOrder lists what resolves first on top", () => {
	// The engine flushes the queue head first and pushed-first resolves
	// last — the LAST entry resolves FIRST.
	const queue = [
		{ fired_ord: 31, controller: 1, card: "A" },
		{ fired_ord: 32, controller: 2, card: "B" },
		{ fired_ord: 33, controller: 1, card: "C" },
	];
	const order = queueResolutionOrder(queue);
	assert.deepEqual(order.map((q) => q.card), ["C", "B", "A"]);
	assert.deepEqual(queueResolutionOrder(null), []);
});

test("queueOrderAfterMove moves only within the mover's own entries", () => {
	const queue = [
		{ fired_ord: 31, controller: 1, card: "A" },
		{ fired_ord: 32, controller: 1, card: "C" },
		{ fired_ord: 33, controller: 2, card: "B" },
	];
	// A resolves sooner: it swaps with C, the next of its controller —
	// cross-controller order is the APNAP sort's business, never the
	// mover's, so a move only finds its own entries.
	const sooner = queueOrderAfterMove(queue, 31, true);
	assert.deepEqual(sooner, [32, 31, 33]);
	// The same swap, seen from C's side: C resolves later.
	const later = queueOrderAfterMove(queue, 32, false);
	assert.deepEqual(later, [32, 31, 33]);
	// C resolving sooner has nowhere to go — it is already its
	// controller's first to resolve. Bob's lone entry moves nowhere.
	assert.equal(queueOrderAfterMove(queue, 32, true), null);
	assert.equal(queueOrderAfterMove(queue, 33, true), null);
	assert.equal(queueOrderAfterMove(queue, 33, false), null);
	// An unknown ordinal is nobody's move.
	assert.equal(queueOrderAfterMove(queue, 999, true), null);
	// Interleaved: the swap jumps the other seat's entry to reach its
	// own — harmless to the flush's stable APNAP sort.
	const mixed = [
		{ fired_ord: 41, controller: 1, card: "A" },
		{ fired_ord: 42, controller: 2, card: "B" },
		{ fired_ord: 43, controller: 1, card: "C" },
	];
	assert.deepEqual(queueOrderAfterMove(mixed, 41, true), [43, 42, 41]);
});

test("describeEvent spells the registry's rows", () => {
	assert.equal(describeEvent({ kind: "TRIGGER_FIRED", card: "Rhystic Study", effect: "may draw a card" }, state),
		"Rhystic Study triggers — may draw a card");
	assert.equal(describeEvent({ kind: "TRIGGERS_ORDERED", order: [3, 1, 2] }, state),
		"orders 3 waiting triggers");
	assert.equal(actionSummary({ kind: "ORDER_TRIGGERS", seat: 1 }, state),
		"Collin orders the waiting triggers");
});

/* ---------- replay and the coach (MAD-339) ---------- */

import {
	turnAnchors, clampOrd, stepOrd, turnStepOrd, replayLine,
	factRows, missedTriggerLine,
} from "../web/static/js/playvm.js";

const replayLog = [
	{ kind: "GAME_STARTED", ord: 1 },
	{ kind: "TURN_STARTED", ord: 2, turn: 1, turn_seat: 1 },
	{ kind: "STEP_ENTERED", ord: 3, phase: "beginning", step: "upkeep" },
	{ kind: "LAND_PLAYED", ord: 4, card: "Forest" },
	{ kind: "TURN_ENDED", ord: 5, turn: 1 },
	{ kind: "TURN_STARTED", ord: 6, turn: 2, turn_seat: 2 },
	{ kind: "CAST", ord: 7, card: "Sol Ring" },
	// turn 2 never closes — the log ends mid-turn
];

test("turnAnchors maps the scrubber's turn boundaries", () => {
	const anchors = turnAnchors(replayLog);
	assert.equal(anchors.length, 2);
	assert.deepEqual(anchors[0], { turn: 1, seat: 1, ord: 2, end: 5 });
	assert.deepEqual(anchors[1], { turn: 2, seat: 2, ord: 6, end: 0 });
	assert.deepEqual(turnAnchors([]), []);
});

test("clampOrd and stepOrd keep the scrub in the log's honest range", () => {
	assert.equal(clampOrd(-4, 10), 0);
	assert.equal(clampOrd(14, 10), 10);
	assert.equal(stepOrd(5, -1, 10), 4);
	assert.equal(stepOrd(10, 1, 10), 10);
	assert.equal(stepOrd(0, -1, 10), 0);
});

test("turnStepOrd lands on turn boundaries, not past the head", () => {
	const anchors = turnAnchors(replayLog);
	// Forward from before the first turn: the first anchor.
	assert.equal(turnStepOrd(anchors, 0, 1, 7), 2);
	// Forward from inside turn 1: turn 2's start.
	assert.equal(turnStepOrd(anchors, 4, 1, 7), 6);
	// Forward off the end: the head.
	assert.equal(turnStepOrd(anchors, 6, 1, 7), 7);
	// Back from inside turn 2: turn 2's own start replays.
	assert.equal(turnStepOrd(anchors, 7, -1, 7), 6);
	// Back from inside turn 1: turn 1's start — a chapter button, the
	// second tap reaches past it.
	assert.equal(turnStepOrd(anchors, 3, -1, 7), 2);
	assert.equal(turnStepOrd(anchors, 2, -1, 7), 0);
});

test("replayLine spells the position every pane agrees on", () => {
	assert.equal(replayLine(state, 0, 400), "#0 — before anything happened");
	const mid = { ...state, turn: 4, turn_seat: 2, phase: "combat", step: "declare_attackers" };
	assert.equal(replayLine(mid, 412, 500), "#412 · T4 · Bob's turn · declare attackers");
	const fin = { status: "finished" };
	assert.equal(replayLine(fin, 500, 500), "#500 · the final position");
});

test("factRows carries the deterministic facts the model reads", () => {
	const summary = {
		game: { status: "finished", format: "commander", turns: 9, reason: "last_standing",
			seats: [
				{ seat: 1, name: "Collin", life: 22, alive: true },
				{ seat: 2, name: "Bob", life: 0, alive: false, left_cause: "sba_zero_life" },
			] },
		seat: {
			seat: 1, name: "Collin", turns_played: 9, casts: 14, lands_played: 9, drawn: 22,
			hand_known_at_end: true, hand_at_end: 3, hand_known: ["Cyclonic Rift"],
			damage_dealt: 31, damage_taken: 18, life_lost: 18, life_gained: 0,
			final_turn: { turn: 9, ended_turn: true, lands: 2, lands_known: true, land_drops: 1, hand: 3 },
			missed_triggers: [{ card: "Rhystic Study", effect: "may draw a card", at: 412, happening: "Bob casts Sol Ring" }],
		},
		mana_note: "floating mana is never modelled",
	};
	const rows = factRows(summary);
	const byLabel = Object.fromEntries(rows.map((r) => [r.label, r]));
	assert.match(byLabel.game.text, /finished · commander · 9 turns · ended: last_standing/);
	assert.match(byLabel.table.text, /Collin: 22 life/);
	assert.match(byLabel.table.text, /Bob: out \(sba_zero_life\)/);
	assert.match(byLabel["hand at end"].text, /3 cards — had seen Cyclonic Rift/);
	assert.match(byLabel["final turn (T9)"].text, /2 unspent mana sources/);
	assert.equal(byLabel["missed triggers"].text, "1, with today's registry — see the debrief");
	assert.equal(rows[rows.length - 1].label, "missed triggers");

	// The clean case: no misses is its own fact, not a missing one.
	const clean = { ...summary, seat: { ...summary.seat, missed_triggers: [] } };
	const cleanRows = factRows(clean);
	assert.match(cleanRows[cleanRows.length - 1].text, /none derived/);

	assert.equal(missedTriggerLine(summary.seat.missed_triggers[0]),
		"Rhystic Study (may draw a card) — Bob casts Sol Ring at #412");
});
