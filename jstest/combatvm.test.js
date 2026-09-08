// The combat tracker's view model (combatvm.js) is DOM-free by design —
// these are the derivations the DM's screen paints from, kept honest so
// the surface never drifts from what the engine's responses carry.

import test from "node:test";
import assert from "node:assert/strict";

import {
	CONDITIONS, DAMAGE_TYPES, promptRows, concentrationRows,
	addLine, stepLine, startPayload, lineupCount, healthWord, turnLabel,
} from "../web/static/js/combatvm.js";

test("the condition vocabulary is the engine's fifteen", () => {
	assert.equal(CONDITIONS.length, 15);
	assert.ok(CONDITIONS.includes("blinded"));
	assert.ok(CONDITIONS.includes("unconscious"));
});

test("the damage types are offered, all lowercase for the grammar's match", () => {
	assert.ok(DAMAGE_TYPES.includes("fire"));
	assert.ok(DAMAGE_TYPES.includes("bludgeoning"));
	for (const t of DAMAGE_TYPES) assert.match(t, /^[a-z]+$/);
});

test("promptRows spells each prompt kind the turn response carries", () => {
	const rows = promptRows({
		lair_reminder: true,
		prompts: [
			{ kind: "recharge", name: "Breath Weapon", usage: "Recharge 5-6" },
			{ kind: "death_save", name: "Thalia", detail: "1 success, 0 failures" },
		],
		expired_conditions: [{ name: "prone" }],
		expired_effects: [{ name: "Bless", target_name: "Thalia" }],
	});
	assert.deepEqual(rows.map((r) => r.kind), ["lair", "recharge", "death", "expired", "expired"]);
	assert.match(rows[0].text, /count 20/);
	assert.match(rows[1].text, /Breath Weapon.*Recharge 5-6/);
	assert.match(rows[2].text, /Thalia is dying/);
	assert.match(rows[3].text, /prone wore off/);
	assert.match(rows[4].text, /Thalia.*Bless ended/);
});

test("promptRows keeps empty prompts empty rather than inventing rows", () => {
	assert.deepEqual(promptRows(null), []);
	assert.deepEqual(promptRows({ prompts: [], expired_conditions: [] }), []);
	// A prompt with neither name nor detail is dropped, not blanked.
	assert.deepEqual(promptRows({ prompts: [{ kind: "note" }] }), []);
});

test("concentrationRows carries the spell and the 2014 DC", () => {
	const rows = concentrationRows([{ spell: "Hold Person", dc: 15, source_id: "e1" }]);
	assert.equal(rows.length, 1);
	assert.equal(rows[0].kind, "concentration");
	assert.match(rows[0].text, /Hold Person.*DC 15/);
	assert.deepEqual(concentrationRows(undefined), []);
});

test("addLine merges by statblock, stepLine drops at zero", () => {
	const lines = [];
	addLine(lines, "Goblin", 2);
	addLine(lines, "Goblin", 1);
	addLine(lines, "Wolf", 1);
	assert.deepEqual(lines, [
		{ name: "Goblin", count: 3 },
		{ name: "Wolf", count: 1 },
	]);
	stepLine(lines, 0, -1);
	assert.equal(lines[0].count, 2);
	stepLine(lines, 0, -2);
	assert.equal(lines.length, 1); // the Goblin line dropped at zero
});

test("startPayload spells StartInput: pcs by id, monsters by name, companions with their own name", () => {
	const body = startPayload({
		name: "  The Bridge  ",
		encounterID: "enc1",
		sessionID: "s1",
		pcs: ["a", "b"],
		monsters: [{ name: "Goblin", count: 3, cr: "1/4" }],
		companions: [{ name: "Wolf", count: 1, companionName: "Whiskers" }],
	});
	assert.deepEqual(body, {
		name: "The Bridge",
		encounter_id: "enc1",
		session_id: "s1",
		pcs: ["a", "b"],
		monsters: [{ name: "Goblin", count: 3 }],
		companions: [{ statblock: "Wolf", name: "Whiskers", count: 1 }],
	});
});

test("lineupCount gates the start button on anyone at all", () => {
	assert.equal(lineupCount([], [], []), 0);
	assert.equal(lineupCount(["a"], [], []), 1);
	assert.equal(lineupCount([], [{ name: "Goblin", count: 3 }], []), 3);
	assert.equal(lineupCount(["a"], [{ name: "Goblin", count: 2 }], [{ name: "Wolf", count: 1 }]), 4);
	assert.equal(lineupCount([], [{ name: "Ghost", count: 0 }], []), 0);
});

test("healthWord follows the table's grammar", () => {
	assert.equal(healthWord(0, 30), "down");
	assert.equal(healthWord(15, 30), "bloodied");
	assert.equal(healthWord(16, 30), "hale");
});

test("turnLabel parks before the first turn and names the acting combatant after", () => {
	const order = [
		{ position: 0, name: "Thalia" },
		{ position: 1, name: "Goblin A" },
	];
	assert.equal(turnLabel({ turn_index: -1 }, order), "the fight begins");
	assert.equal(turnLabel({ turn_index: 0 }, order), "Thalia's move");
	assert.equal(turnLabel({ turn_index: 1 }, order), "Goblin A's move");
	assert.equal(turnLabel(null, order), "");
});
