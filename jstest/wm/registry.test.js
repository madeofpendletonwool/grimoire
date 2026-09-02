// The registry is data, and these are the invariants that keep it honest.
// They exist because the registry's whole promise is "add a tool here and
// nothing else changes" — which only holds if a malformed entry is caught.

import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";

import {
	TOOLS, TOOL_IDS, ANY_CORPUS, isTool, toolDef, inCorpus, toolsFor, knownTool,
} from "../../web/static/js/wm/registry.js";

const entries = () => TOOL_IDS.map((id) => [id, TOOLS[id]]);

test("every tool declares the fields the shell reads", () => {
	for (const [id, def] of entries()) {
		assert.ok(def.title, `${id}: no title`);
		assert.ok(def.icon, `${id}: no icon`);
		assert.ok(def.corpus, `${id}: no corpus`);
		assert.ok(def.accel, `${id}: no accel`);
		assert.equal(typeof def.load, "function", `${id}: load must be a function`);
		assert.equal(def.instances, "single", `${id}: unknown instances mode`);
		assert.ok(Array.isArray(def.min) && def.min.length === 2, `${id}: min must be [w, h]`);
		assert.ok(def.min.every((n) => Number.isInteger(n) && n > 0), `${id}: bad min`);
	}
});

// Ids reach the server, the URL and stored layouts, and internal/uistate's
// validator only accepts this shape.
test("tool ids match the id the server will accept", () => {
	const serverPattern = /^[a-z0-9][a-z0-9-]{0,63}$/;
	for (const id of TOOL_IDS) {
		assert.ok(serverPattern.test(id), `${id} would be rejected by internal/uistate`);
	}
});

test("corpus is one of the two games or the shared marker", () => {
	for (const [id, def] of entries()) {
		assert.ok([ANY_CORPUS, "dnd", "mtg"].includes(def.corpus), `${id}: corpus ${def.corpus}`);
	}
});

test("no two tools claim the same leader accelerator", () => {
	const seen = new Map();
	for (const [id, def] of entries()) {
		const prev = seen.get(def.accel);
		assert.ok(!prev, `${id} and ${prev} both bind Ctrl+G ${def.accel}`);
		seen.set(def.accel, id);
	}
});

test("accelerators are a single character", () => {
	for (const [id, def] of entries()) {
		assert.equal(def.accel.length, 1, `${id}: accel ${JSON.stringify(def.accel)}`);
	}
});

// The registry names sprites by key; a typo would render an empty slot at boot
// with no error, which is exactly the failure that is easy to miss in review.
test("every icon exists in the sprite sheet's ICONS map", () => {
	const src = readFileSync(new URL("../../web/static/js/icons.js", import.meta.url), "utf8");
	const block = src.slice(src.indexOf("export const ICONS"));
	const known = new Set([...block.matchAll(/^\s*([A-Za-z][A-Za-z0-9]*)\s*:/gm)].map((m) => m[1]));
	for (const [id, def] of entries()) {
		assert.ok(known.has(def.icon), `${id}: icon "${def.icon}" is not in ICONS`);
	}
});

// The registry replaced closeForeignSurfaces() and the html[data-corpus]
// display:none rules. These are the behaviours those two used to encode.
test("corpus filtering offers each game its own tools plus the shared ones", () => {
	const dnd = toolsFor("dnd");
	const mtg = toolsFor("mtg");

	for (const shared of ["chat", "study", "reader"]) {
		assert.ok(dnd.includes(shared), `${shared} should serve D&D`);
		assert.ok(mtg.includes(shared), `${shared} should serve Magic`);
	}
	assert.ok(dnd.includes("planner") && !mtg.includes("planner"), "planner is D&D only");
	assert.ok(mtg.includes("deck") && !dnd.includes("deck"), "deck is Magic only");
});

test("every tool is offered to at least one game", () => {
	const offered = new Set([...toolsFor("dnd"), ...toolsFor("mtg")]);
	for (const id of TOOL_IDS) {
		assert.ok(offered.has(id), `${id} is unreachable from either game`);
	}
});

test("toolsFor preserves registry order, so the rail is stable", () => {
	const dnd = toolsFor("dnd");
	assert.deepEqual(dnd, TOOL_IDS.filter((id) => dnd.includes(id)));
});

test("an unknown corpus offers nothing rather than everything", () => {
	assert.deepEqual(toolsFor("pokemon"), []);
});

test("lookups tolerate ids that arrive from stored data", () => {
	assert.ok(isTool("planner"));
	for (const junk of ["", "nope", "__proto__", "constructor", "toString"]) {
		assert.equal(isTool(junk), false, `isTool(${JSON.stringify(junk)})`);
		assert.equal(toolDef(junk), null);
		assert.equal(inCorpus(junk, "dnd"), false);
		assert.equal(knownTool(junk), false);
	}
});

test("the registry cannot be mutated at runtime", () => {
	assert.throws(() => { TOOLS.planner = null; });
	assert.throws(() => { TOOL_IDS.push("bogus"); });
});

test("no two tools claim the same accelerator", () => {
	// One accelerator per tool is what makes the keyboard registry-driven; two
	// tools on one letter means the second is unreachable and its cheat-sheet
	// line is a lie. keys.js raises this at boot, which is too late to be a
	// useful message, so it is caught here instead.
	const seen = new Map();
	for (const id of TOOL_IDS) {
		const accel = TOOLS[id].accel;
		assert.match(accel, /^[a-z0-9]$/, `${id} has an unusable accelerator: ${accel}`);
		assert.equal(seen.has(accel), false, `${id} and ${seen.get(accel)} both claim "${accel}"`);
		seen.set(accel, id);
	}
});
