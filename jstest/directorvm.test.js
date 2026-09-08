// The encounter director panel's view model (directorvm.js) is DOM-free
// by design — these are the derivations the panel paints from, kept
// honest so the advisory surface never drifts from what the director
// route carries. The load-bearing one is the render gate: a suggestion
// without basis cannot render, whatever the wire said.

import test from "node:test";
import assert from "node:assert/strict";

import {
	citationLines, suggestionRows, transparency, combatLine,
} from "../web/static/js/directorvm.js";

const body = {
	combat: { id: "c1", name: "The Bridge at Night", round: 3, turn: "Old Gnaw" },
	suggestions: [
		{
			actor: "Old Gnaw",
			action: "Two bites on the downed cleric — finish her before the healer's turn.",
			reasoning: "She is dying at 2 hp and nobody has a reaction left to punish it.",
			basis: [
				{ id: "S1", kind: "statblock", source: "Old Gnaw — Bite", text: "Bite: Melee Weapon Attack: +5 to hit, 2d6+3 piercing damage." },
				{ id: "L2", kind: "state", source: "live state — Mira", text: "Mira (pc, party): HP 2/24, downed and dying (1 death save failure, 0 successes)." },
			],
		},
		// The gate's client-side half: each of these must not render.
		{ actor: "The bridge trolls", action: "Swarm.", reasoning: "Numbers.", basis: [] },
		{ actor: "A shade", action: "Drift.", reasoning: "Malice.", basis: [{ id: "", kind: "state", source: "", text: "   " }] },
		{ actor: "No basis key at all", action: "Act.", reasoning: "Because." },
		null,
	],
	basis: [],
	dropped: 3,
	model: "claude-sonnet-4-5",
	caveats: ["“Dire Wolf” no longer resolves against the bestiary — cited from the live state only", ""],
};

test("the render gate: a suggestion without basis cannot render", () => {
	const rows = suggestionRows(body);
	assert.equal(rows.length, 1);
	assert.equal(rows[0].actor, "Old Gnaw");
	// And the gate holds on degenerate inputs, not just crafted ones.
	assert.deepEqual(suggestionRows(null), []);
	assert.deepEqual(suggestionRows({ suggestions: [] }), []);
	assert.deepEqual(suggestionRows({ suggestions: [{ actor: "x", action: "y", reasoning: "z" }] }), []);
});

test("every rendered suggestion carries its citations, whole", () => {
	const [row] = suggestionRows(body);
	assert.equal(row.citations.length, 2);
	assert.deepEqual(row.citations.map((c) => c.id), ["S1", "L2"]);
	assert.deepEqual(row.citations.map((c) => c.kind), ["statblock", "state"]);
	// The text rides in full — the state that justifies it, not a
	// summary of it.
	assert.ok(row.citations[0].text.startsWith("Bite: Melee Weapon Attack"));
	assert.equal(row.citations[1].source, "live state — Mira");
});

test("citationLines drops blanks and normalizes kinds, keeps text whole", () => {
	const lines = citationLines(body.suggestions[0].basis);
	assert.equal(lines.length, 2);
	assert.deepEqual(citationLines([]), []);
	assert.deepEqual(citationLines(null), []);
	assert.deepEqual(citationLines([{ id: "L9", kind: "weird", text: "an unknown kind reads as state" }]),
		[{ id: "L9", kind: "state", source: "", text: "an unknown kind reads as state" }]);
	assert.deepEqual(citationLines([{ id: "L9", kind: "state", text: "" }]), []);
});

test("transparency spells the honest tail: dropped, caveats, model", () => {
	const t = transparency(body);
	assert.equal(t.dropped, 3);
	assert.equal(t.droppedNote, "3 suggestions dropped for want of a citable basis");
	assert.deepEqual(t.caveats, ["“Dire Wolf” no longer resolves against the bestiary — cited from the live state only"]);
	assert.equal(t.model, "claude-sonnet-4-5");
	// A clean pass stays quiet, and a singular drop reads as one.
	assert.equal(transparency({ dropped: 0 }).droppedNote, "");
	assert.equal(transparency({ dropped: 1 }).droppedNote, "1 suggestion dropped for want of a citable basis");
	assert.deepEqual(transparency(null), { dropped: 0, droppedNote: "", caveats: [], model: "" });
	assert.equal(transparency({ dropped: -2 }).dropped, 0);
});

test("combatLine joins the fight, the round and whose move it is", () => {
	assert.equal(combatLine(body.combat), "The Bridge at Night · round 3 · Old Gnaw to act");
	assert.equal(combatLine({ name: "Skirmish", round: 1 }), "Skirmish · round 1");
	assert.equal(combatLine({ round: 2, turn: "Gribbs" }), "round 2 · Gribbs to act");
	assert.equal(combatLine({}), "");
	assert.equal(combatLine(null), "");
});
