// The Guide's view model (guidevm.js) is DOM-free by design — the track
// tables, the gate and the fold over a snapshot (ADR 14).
//
// The load-bearing ones here are the gate and the seat: a step may only
// complete because the server says the act happened, and a track may only
// offer steps the walker's own seat can actually finish. Both are the kind
// of bug that shows up at a stranger's first boot and nowhere else.

import test from "node:test";
import assert from "node:assert/strict";

import {
	TRACKS, TRACK_DM, TRACK_PLAYER, TRACK_PREF, EMPTY,
	snapshot, stepsFor, trackFor, needsFork, progress, progressLine, isTrack,
} from "../web/static/js/guidevm.js";
import { inCorpus, inSeat, SEAT_DM, SEAT_PLAYER } from "../web/static/js/wm/registry.js";

const camp = (my_role) => ({ id: "c", name: "A campaign", my_role });

/** The seat each track is walked at. */
const SEAT_OF = { [TRACK_DM]: SEAT_DM, [TRACK_PLAYER]: SEAT_PLAYER };

/* ---------- the tables ---------- */

test("every step's Show me names a tool its own seat may open, in D&D", () => {
	for (const [track, steps] of Object.entries(TRACKS)) {
		const seat = SEAT_OF[track];
		for (const step of steps) {
			const tool = step.show?.tool;
			if (!tool) continue;   // a workspace-slot step names no tool
			assert.ok(inCorpus(tool, "dnd"), `${track}/${step.id}: ${tool} is not a D&D tool`);
			assert.ok(inSeat(tool, seat), `${track}/${step.id}: ${tool} is outside the ${seat} seat`);
		}
	}
});

test("a step either opens a tool or switches a workspace, never both or neither", () => {
	for (const [track, steps] of Object.entries(TRACKS)) {
		for (const step of steps) {
			const { tool, slot } = step.show || {};
			assert.ok(!(tool && slot), `${track}/${step.id} names both a tool and a slot`);
			assert.ok(tool || slot, `${track}/${step.id} names nothing to show`);
			if (slot) assert.ok(Number.isInteger(slot) && slot >= 1 && slot <= 9,
				`${track}/${step.id}: slot ${slot} is outside 1-9`);
		}
	}
});

test("step ids are unique within a track, and every step carries its copy", () => {
	for (const [track, steps] of Object.entries(TRACKS)) {
		const seen = new Set();
		for (const step of steps) {
			assert.ok(!seen.has(step.id), `${track} repeats step id ${step.id}`);
			seen.add(step.id);
			assert.ok(step.title && step.blurb, `${track}/${step.id} is missing its copy`);
			assert.equal(typeof step.done, "function", `${track}/${step.id} has no gate`);
		}
	}
});

/* ---------- the gate ---------- */

test("nothing is done on an empty snapshot", () => {
	for (const track of [TRACK_DM, TRACK_PLAYER]) {
		const p = progress(stepsFor(track), EMPTY);
		assert.equal(p.count, 0, `${track} starts part-finished`);
		assert.equal(p.complete, false);
		assert.equal(p.pct, 0);
		assert.equal(p.next.id, stepsFor(track)[0].id, `${track} does not start at its first step`);
	}
});

// The whole point of the design: a step completes because the server says the
// act happened. If a predicate ever stopped reading the snapshot, this is the
// test that would notice.
test("every step is reachable — a full snapshot completes both tracks", () => {
	const full = snapshot({
		campaigns: 1, role: "dm", has_spine: true, decided: 3, pending: 0,
		encounters: 2, members: 4, invites: 1, sessions: 1, rolls: 12,
		has_character: true, journal_entries: 2,
	});
	for (const track of [TRACK_DM, TRACK_PLAYER]) {
		const p = progress(stepsFor(track), full);
		assert.equal(p.complete, true, `${track} cannot be completed`);
		assert.equal(p.next, null);
		assert.equal(p.pct, 100);
	}
});

test("each step gates on its own fact, not on the step before it", () => {
	// An encounter built before the Planner was ever opened is still built.
	const outOfOrder = snapshot({ campaigns: 1, encounters: 1 });
	const p = progress(stepsFor(TRACK_DM), outOfOrder);
	assert.deepEqual(p.done, ["found", "encounter"]);
	// …and "next" is still the first thing actually outstanding.
	assert.equal(p.next.id, "canon");
});

test("a throwing predicate costs one checkmark, not the panel", () => {
	const steps = [
		{ id: "ok", title: "t", blurb: "b", done: () => true, show: { tool: "chat" } },
		{ id: "boom", title: "t", blurb: "b", done: () => { throw new Error("wire"); }, show: { tool: "chat" } },
	];
	const p = progress(steps, EMPTY);
	assert.deepEqual(p.done, ["ok"]);
	assert.equal(p.next.id, "boom");
});

test("progress survives degenerate input", () => {
	assert.equal(progress(null, null).total, 0);
	assert.equal(progress([], EMPTY).complete, false);   // an empty track is not a finished one
	assert.equal(progressLine(null), "");
	assert.equal(progressLine(progress(stepsFor(TRACK_DM), EMPTY)), "0 of 6 done");
});

/* ---------- the snapshot ---------- */

test("snapshot normalises anything the wire can carry", () => {
	assert.deepEqual(snapshot(null), snapshot({}));
	const s = snapshot({ campaigns: -4, decided: 2.7, sessions: "3", has_spine: 1, role: "  dm  " });
	assert.equal(s.campaigns, 0, "a negative count is no count");
	assert.equal(s.decided, 2, "a fractional count floors");
	assert.equal(s.sessions, 0, "a string count is no count");
	assert.equal(s.hasSpine, true);
	assert.equal(s.role, "dm");
	// Absent fields are zero, so every `>` comparison in a gate stays honest.
	for (const [k, v] of Object.entries(snapshot({}))) {
		assert.ok(v === 0 || v === false || v === "", `${k} defaulted to ${v}`);
	}
});

/* ---------- the observer ---------- */

test("an observer is offered no step their seat cannot finish", () => {
	const steps = stepsFor(TRACK_PLAYER, "observer");
	const ids = steps.map((s) => s.id);
	// No binding and no journal reaches an observer (ADR 22), so neither
	// step could ever tick over — the track would sit stuck one short.
	assert.ok(!ids.includes("character"), "an observer is asked to bind a character");
	assert.ok(!ids.includes("learned"), "an observer is asked to write a journal");
	assert.ok(ids.length > 0, "an observer is offered nothing at all");

	// And the reduced track is completable on what an observer can actually do.
	const watched = snapshot({ campaigns: 1, role: "observer", rolls: 1 });
	assert.equal(progress(steps, watched).complete, true);
});

test("a seated player keeps the whole track", () => {
	assert.equal(stepsFor(TRACK_PLAYER, "player").length, TRACKS[TRACK_PLAYER].length);
	assert.equal(stepsFor(TRACK_PLAYER).length, TRACKS[TRACK_PLAYER].length);
});

test("stepsFor is total over nonsense", () => {
	assert.deepEqual(stepsFor("nope"), []);
	assert.deepEqual(stepsFor(undefined), []);
	assert.equal(isTrack("dm"), true);
	assert.equal(isTrack("keeper"), false);
});

/* ---------- which track ---------- */

test("real standing outranks the fork's answer, in both directions", () => {
	// Answered "player", then founded a table: the DM track, or the walk
	// they are on can never advance.
	assert.equal(trackFor(SEAT_DM, TRACK_PLAYER, [camp("dm")]), TRACK_DM);
	// Answered "DM", then only ever joined one: the player track.
	assert.equal(trackFor(SEAT_PLAYER, TRACK_DM, [camp("player")]), TRACK_PLAYER);
	// The keeper reads every table, and keeps the DM walk.
	assert.equal(trackFor(SEAT_DM, TRACK_PLAYER, [camp("keeper")]), TRACK_DM);
	// A DM anywhere is a DM.
	assert.equal(trackFor(SEAT_DM, null, [camp("player"), camp("dm")]), TRACK_DM);
});

test("the pref only breaks the tie the server cannot", () => {
	// No campaigns: "waiting on a join code" and "about to create one" are
	// the same row set, and the seat resolves to DM for want of evidence.
	assert.equal(trackFor(SEAT_DM, TRACK_PLAYER, []), TRACK_PLAYER);
	assert.equal(trackFor(SEAT_DM, TRACK_DM, []), TRACK_DM);
	// Unanswered, it falls back to the seat the shell resolved.
	assert.equal(trackFor(SEAT_DM, null, []), TRACK_DM);
	assert.equal(trackFor(SEAT_PLAYER, null, []), TRACK_PLAYER);
	assert.equal(trackFor(SEAT_DM, "garbage", []), TRACK_DM);
	assert.equal(trackFor(SEAT_DM, null, null), TRACK_DM);
});

/* ---------- the fork ---------- */

test("only a signed-in account with no campaigns and no answer sees the fork", () => {
	const authed = { authenticated: true };
	assert.equal(needsFork(authed, [], {}), true);
	assert.equal(needsFork(authed, [], { [TRACK_PREF]: TRACK_DM }), false, "already answered");
	assert.equal(needsFork(authed, [camp("player")], {}), false, "already joined a table");
	assert.equal(needsFork(authed, [camp("dm")], {}), false, "already runs a table");
});

test("a failed read is not a verdict — no fork rather than a wrong one", () => {
	const authed = { authenticated: true };
	// Greeting a returning DM with "are you new?" because /api/campaigns
	// timed out is worse than not greeting them at all.
	assert.equal(needsFork(authed, null, {}), false);
	assert.equal(needsFork(null, [], {}), false);
	assert.equal(needsFork({ authenticated: false }, [], {}), false);
	// A garbled pref is no answer, so the fork still stands.
	assert.equal(needsFork(authed, [], { [TRACK_PREF]: "sideways" }), true);
	assert.equal(needsFork(authed, [], null), true);
});
