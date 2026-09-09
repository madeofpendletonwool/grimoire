// Preset invariants (ADR 22). The loader trusts this table: a preset naming
// a tool the seat cannot open would only surface at someone's first boot,
// as a window that either errors or refuses everything it reads.

import test from "node:test";
import assert from "node:assert/strict";

import { PRESETS, presetsFor, presetFor, presetIsOfferable } from "../../web/static/js/wm/presets.js";
import { TOOL_IDS, toolsFor, SEAT_DM, SEAT_PLAYER } from "../../web/static/js/wm/registry.js";
import * as T from "../../web/static/js/wm/tree.js";

// The tree module owns leaf-walking; presets are its only dependency, so
// this imports it the same way the loader does and stays DOM-free (ADR 14).
const leavesOf = (preset) => T.leaves(preset.build()).map((node) => node.tool);

const SEATS = [SEAT_DM, SEAT_PLAYER];
const CORPORA = ["mtg", "dnd"];

test("every preset names tools its own seat may open, in its own game", () => {
	for (const seat of SEATS) {
		for (const corpus of CORPORA) {
			for (const preset of presetsFor(corpus, seat)) {
				assert.ok(presetIsOfferable(preset, corpus, seat),
					`${seat}/${corpus} slot ${preset.slot} ("${preset.name}") names a tool outside its seat or game`);
			}
		}
	}
});

test("the player seat seeds the player workspace, never the DM cockpit", () => {
	const player = presetsFor("dnd", SEAT_PLAYER);
	const names = player.map((p) => p.name);
	assert.deepEqual(names, ["The table", "The world", "Study"]);

	// The named surfaces of the seat (MAD-493, grown by MAD-490): my
	// character and the known world live in the Campaign tool, quick
	// rolls in Dice, the party in the Board, the Player Grimoire in
	// cchat, the DM's handed material in Handouts. Every leaf of the
	// preset set must be a member tool.
	const offered = new Set(toolsFor("dnd", SEAT_PLAYER));
	for (const preset of player) {
		for (const id of leavesOf(preset)) {
			assert.ok(offered.has(id), `${id} seeds a player slot but is not a member tool`);
		}
	}

	const seeded = new Set(player.flatMap(leavesOf));
	for (const id of ["board", "dice", "cchat", "campaign", "handouts"]) {
		assert.ok(seeded.has(id), `the player seat does not seed ${id}`);
	}
	for (const id of ["planner", "combat", "screen", "encounter", "director", "review"]) {
		assert.ok(!seeded.has(id), `${id} seeds a player slot`);
	}
});

test("the DM shapes are unchanged: same slots, same names, same tools", () => {
	const dm = presetsFor("dnd", SEAT_DM);
	assert.deepEqual(dm.map((p) => `${p.slot} ${p.name}`), [
		"1 Prep",
		"2 At the table",
		"3 Canon",
		"4 Study",
	]);
	assert.deepEqual(presetsFor("mtg", SEAT_DM).map((p) => `${p.slot} ${p.name}`), [
		"1 Ask",
		"2 Brew",
		"3 Study",
	]);
});

test("Magic has no player shapes — campaigns are a D&D surface", () => {
	assert.deepEqual(presetsFor("mtg", SEAT_PLAYER), []);
});

test("preset slots are unique and in range, names are set", () => {
	for (const seat of SEATS) {
		for (const corpus of CORPORA) {
			const slots = new Set();
			for (const preset of presetsFor(corpus, seat)) {
				assert.ok(preset.slot >= 1 && preset.slot <= 9, `${seat}/${corpus}: slot ${preset.slot}`);
				assert.ok(!slots.has(preset.slot), `${seat}/${corpus}: two presets claim slot ${preset.slot}`);
				slots.add(preset.slot);
				assert.ok(preset.name, `${seat}/${corpus} slot ${preset.slot}: no name`);
				assert.equal(typeof preset.build, "function");
			}
		}
	}
});

test("presetFor finds one slot and misses another", () => {
	assert.equal(presetFor("dnd", SEAT_PLAYER, 1)?.name, "The table");
	assert.equal(presetFor("dnd", SEAT_DM, 1)?.name, "Prep");
	assert.equal(presetFor("dnd", SEAT_PLAYER, 8), null);
	assert.equal(presetFor("pokemon", SEAT_DM, 1), null);
});

test("PRESETS keys are exactly the seats, and every entry tool exists", () => {
	assert.deepEqual(Object.keys(PRESETS).sort(), [SEAT_DM, SEAT_PLAYER]);
	for (const seat of SEATS) {
		for (const corpus of CORPORA) {
			for (const id of presetsFor(corpus, seat).flatMap(leavesOf)) {
				assert.ok(TOOL_IDS.includes(id), `${id} is not a tool`);
			}
		}
	}
});
