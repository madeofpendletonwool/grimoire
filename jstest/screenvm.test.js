// The DM screen's view model (screenvm.js) is DOM-free by design — these
// are the derivations the screen strip paints from, kept honest so the
// play-mode layout never drifts from what the live-context read carries.

import test from "node:test";
import assert from "node:assert/strict";

import {
	KINDS, ROLES, currentScene, sceneMeta, castChips, elapsedLabel, noteLines,
} from "../web/static/js/screenvm.js";

const scenes = [
	{ id: "s1", session_id: "live", kind: "social", name: "The Waystone at midnight", cast: [
		{ entity_id: "e2", role: "present" },
		{ entity_id: "e1", role: "focus" },
	] },
	{ id: "s2", kind: "combat", name: "Ambush on the causeway", cast: [] },
];

test("currentScene prefers the DM's pick, then the seated scene, then order", () => {
	assert.equal(currentScene(scenes, "live").id, "s1");
	assert.equal(currentScene(scenes, "live", "s2").id, "s2");
	// A pick the read no longer carries (scene finished) falls through,
	// never to a blank card.
	assert.equal(currentScene(scenes, "live", "gone").id, "s1");
	assert.equal(currentScene(scenes, "other").id, "s1"); // nothing seated → first
	assert.equal(currentScene(scenes, "other", "s2").id, "s2");
	assert.equal(currentScene([], "live"), null);
	assert.equal(currentScene(null, ""), null);
});

test("sceneMeta spells kind and setting, empty when there is no scene", () => {
	assert.equal(sceneMeta(scenes[1]), "combat");
	assert.equal(sceneMeta(scenes[0], "The Waystone Inn"), "social · at The Waystone Inn");
	assert.equal(sceneMeta(null), "");
	assert.equal(sceneMeta({ kind: "weird" }), "weird");
});

test("castChips resolves names, focus first, never a blank chip", () => {
	const entities = [
		{ id: "e1", name: "Duke Aldric Vane", kind: "npc" },
		{ id: "e3", name: "The Waystone Inn", kind: "location" },
	];
	const chips = castChips(scenes[0], entities);
	assert.deepEqual(chips.map((c) => c.name), ["Duke Aldric Vane", "e2"]);
	assert.equal(chips[0].role, "focus");
	assert.equal(chips[0].kind, "npc");
	// An unknown entity still chips, by a short id.
	assert.equal(chips[1].name, "e2");
	assert.deepEqual(castChips(null), []);
	assert.deepEqual(castChips({ cast: [] }), []);
});

test("elapsedLabel counts H:MM:SS from started_at and clamps skew at zero", () => {
	const now = Date.parse("2026-09-08T21:04:00Z");
	assert.equal(elapsedLabel("2026-09-08T21:00:00Z", now), "4:00");
	assert.equal(elapsedLabel("2026-09-08T19:59:30Z", now), "1:04:30");
	assert.equal(elapsedLabel("2026-09-08T21:04:30Z", now), "0:00"); // client behind
	assert.equal(elapsedLabel("", now), "");
	assert.equal(elapsedLabel("not a date", now), "");
});

test("noteLines keeps the parking lot newest-first, blanks dropped", () => {
	const events = [
		{ id: "a", kind: "note", summary: "ruling: torches burn 1 hour", created_at: "t1" },
		{ id: "b", kind: "roll", summary: "d20", created_at: "t2" },
		{ id: "c", kind: "note", summary: "", detail: "", created_at: "t3" },
		{ id: "d", kind: "note", summary: "the duke lied about the letter", created_at: "t4" },
	];
	const lines = noteLines(events);
	assert.deepEqual(lines.map((l) => l.text), [
		"the duke lied about the letter",
		"ruling: torches burn 1 hour",
	]);
	assert.deepEqual(noteLines([]), []);
	// The list is capped — a long session keeps the last eight in view.
	assert.ok(noteLines(Array.from({ length: 30 }, (_, i) => ({
		id: `n${i}`, kind: "note", summary: `note ${i}`,
	}))).length <= 8);
});

test("the vocabularies mirror the spine's", () => {
	assert.deepEqual(Object.keys(KINDS), ["social", "exploration", "combat", "revelation", "downtime", "travel"]);
	assert.deepEqual(ROLES, ["focus", "present", "offstage", "mentioned"]);
});
