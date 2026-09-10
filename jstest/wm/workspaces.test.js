// Which slots the workspace strip draws.
//
// Regression test for the workspace that could not be got back to. Closing
// the last window in a slot persisted `tree: null` under the slot's own name,
// and the strip's rule was "no tree and no preset and not active — skip it".
// So on the next load the workspace was a saved row the strip refused to
// draw: gone from the UI, its layout still on the server, and reachable only
// by Alt+N — a keyboard-only escape from a state one mouse click could reach.
//
// showsInStrip is pure so this stays DOM-free (ADR 14).

import test from "node:test";
import assert from "node:assert/strict";

import { showsInStrip } from "../../web/static/js/wm/workspaces.js";

/** A slot as loadSet builds one from a row the server returned. */
const savedRow = (slot, tree) => ({
	slot, name: `Workspace ${slot}`, tree, focus: null, zoom: null,
	seeded: false, saved: true,
});

/** A slot nobody has ever touched: no preset seeded it, no row exists. */
const untouched = (slot) => ({
	slot, name: `Workspace ${slot}`, tree: null, focus: null, zoom: null,
	seeded: false, saved: false,
});

const seeded = (slot) => ({ ...untouched(slot), tree: { t: "leaf", id: "w1", tool: "chat" }, seeded: true });

test("a workspace the user emptied keeps its place in the strip", () => {
	// The exact shape the bug produced: saved under its own name, no tree,
	// not the active slot, never seeded by a preset.
	assert.equal(showsInStrip(savedRow(5, null), 1), true);
});

test("a slot nobody has touched stays out of the strip", () => {
	// The other half of the rule, and the reason the strip is not nine tabs
	// wide on a fresh account.
	assert.equal(showsInStrip(untouched(7), 1), false);
});

test("a saved slot with a layout shows, seeded or not", () => {
	assert.equal(showsInStrip(savedRow(2, { t: "leaf", id: "w1", tool: "chat" }), 1), true);
	assert.equal(showsInStrip(seeded(3), 1), true);
});

test("the active slot always shows, so switching to one is never a blank strip", () => {
	assert.equal(showsInStrip(untouched(9), 9), true);
});

test("a missing slot is not a strip entry", () => {
	// list() fills gaps with emptySlot(), but freeSlot() and remove() both
	// ask about slots the map has forgotten.
	assert.equal(showsInStrip(undefined, 1), false);
	assert.equal(showsInStrip(null, 1), false);
});
