// The DM screen's view model (screenvm.js) is DOM-free by design — these
// are the derivations the screen strip paints from, kept honest so the
// play-mode layout never drifts from what the live-context read carries.

import test from "node:test";
import assert from "node:assert/strict";

import {
	KINDS, ROLES, currentScene, sceneMeta, castChips, elapsedLabel, noteLines,
	sceneContext, actingCombatant, combatContext, entityRefs, capturePayload,
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

/* ---------- in-play capture (MAD-483) ---------- */

test("sceneContext carries id and name, null without a scene", () => {
	assert.deepEqual(sceneContext(scenes[0]), { scene_id: "s1", scene_name: "The Waystone at midnight" });
	assert.equal(sceneContext(null), null);
});

test("actingCombatant finds the turn-holder, null when nobody holds it", () => {
	const order = [
		{ id: "c1", name: "Goblin One", is_turn: false },
		{ id: "c2", name: "Mira Thorn", is_turn: true, entity_id: "e2" },
	];
	assert.equal(actingCombatant(order).id, "c2");
	assert.equal(actingCombatant([{ id: "c1" }]), null);
	assert.equal(actingCombatant([]), null);
	assert.equal(actingCombatant(), null);
});

test("combatContext names the fight and who held the turn", () => {
	const acting = { id: "c2", name: "Mira Thorn", is_turn: true, entity_id: "e2" };
	assert.deepEqual(combatContext({ id: "b1" }, acting), {
		combat_id: "b1",
		combatant: { id: "c2", name: "Mira Thorn", entity_id: "e2" },
	});
	// A monster has no entity — the name is still the context.
	const foe = { id: "c9", name: "Ochre Jelly", is_turn: true };
	assert.deepEqual(combatContext({ id: "b1" }, foe).combatant, { id: "c9", name: "Ochre Jelly", entity_id: "" });
	assert.equal(combatContext(null, acting), null);
	assert.equal(combatContext({ id: "b1" }, null), null);
});

test("entityRefs links focus first, then cast, then the acting combatant — deduped", () => {
	const entities = [
		{ id: "e1", name: "Duke Aldric Vane" },
		{ id: "e3", name: "The Waystone Inn" },
	];
	// scenes[0] casts e2 present + e1 focus; the acting combatant is e2 again.
	const refs = entityRefs(scenes[0], { entity_id: "e2" }, entities);
	assert.deepEqual(refs, [
		{ id: "e1", name: "Duke Aldric Vane", source: "focus" },
		{ id: "e2", name: "e2", source: "cast" }, // unknown entity still links, by short id
	]);
	// The acting combatant's entity joins when the cast lacks it.
	const more = entityRefs(scenes[1], { entity_id: "e3" }, entities);
	assert.deepEqual(more, [{ id: "e3", name: "The Waystone Inn", source: "combat" }]);
	assert.deepEqual(entityRefs(null, null, entities), []);
});

test("capturePayload drops the blocks whose context is not live", () => {
	const scene = { id: "s1", name: "The Waystone at midnight", cast: [{ entity_id: "e1", role: "focus" }] };
	const combat = { id: "b1" };
	const acting = { id: "c2", name: "Mira Thorn", is_turn: true, entity_id: "e2" };
	const full = capturePayload(scene, combat, acting, [{ id: "e1", name: "Duke Aldric Vane" }]);
	assert.deepEqual(full, {
		scene: { scene_id: "s1", scene_name: "The Waystone at midnight" },
		combat: { combat_id: "b1", combatant: { id: "c2", name: "Mira Thorn", entity_id: "e2" } },
		entities: [
			{ id: "e1", name: "Duke Aldric Vane", source: "focus" },
			{ id: "e2", name: "e2", source: "combat" }, // the acting entity, unknown here
		],
	});
	// No scene, no fight: just the entity ref that survives.
	assert.deepEqual(capturePayload(null, null, acting, [{ id: "e2", name: "Mira Thorn" }]), {
		entities: [{ id: "e2", name: "Mira Thorn", source: "combat" }],
	});
	// Nothing live: an empty payload, not one full of nulls.
	assert.deepEqual(capturePayload(null, null, null, []), {});
});
