// The handout view-model (MAD-490): the pure half of the Handouts tool,
// held here the way ADR 14 holds every DOM-free module. The ordering and
// grouping rules are the contract the painted surface inherits — most
// recently handed out first, drafts pinned above published for the DM,
// maps before letters, ties stable.

import test from "node:test";
import assert from "node:assert/strict";

import {
	handoutActions, orderHandouts, groupHandouts, STATUS_LABELS,
} from "../web/static/js/handoutsvm.js";

const handout = (over = {}) => ({
	id: "h1", kind: "handout", title: "The charter", body: "Be it known…",
	status: "published", created_at: "2026-09-01T10:00:00Z",
	published_at: "2026-09-02T10:00:00Z", ...over,
});

test("a player gets no lifecycle actions at any status", () => {
	for (const status of ["draft", "published", "retired"]) {
		assert.deepEqual(handoutActions(handout({ status }), false), [],
			`player at ${status} must have no actions`);
	}
});

test("the dm's actions follow the lifecycle", () => {
	assert.deepEqual(handoutActions(handout({ status: "draft" }), true), ["publish", "retire"]);
	assert.deepEqual(handoutActions(handout({ status: "published" }), true), ["unpublish", "retire"]);
	assert.deepEqual(handoutActions(handout({ status: "retired" }), true), []);
});

test("drafts pin above published; the rest follow the hand-out stamp", () => {
	const rows = [
		handout({ id: "old", published_at: "2026-08-01T10:00:00Z" }),
		handout({ id: "draft2", status: "draft", published_at: "" }),
		handout({ id: "new", published_at: "2026-09-03T10:00:00Z" }),
		handout({ id: "draft1", status: "draft", published_at: "" }),
	];
	const ids = orderHandouts(rows).map((h) => h.id);
	assert.deepEqual(ids, ["draft1", "draft2", "new", "old"]);
});

test("ties break by id so the order is stable across renders", () => {
	const a = handout({ id: "b", published_at: "2026-09-01T10:00:00Z" });
	const b = handout({ id: "a", published_at: "2026-09-01T10:00:00Z" });
	assert.deepEqual(orderHandouts([a, b]).map((h) => h.id), ["a", "b"]);
	assert.deepEqual(orderHandouts([b, a]).map((h) => h.id), ["a", "b"]);
});

test("maps section first, letters second, empty sections omitted", () => {
	const groups = groupHandouts([
		handout({ id: "letter", kind: "handout" }),
		handout({ id: "map", kind: "map" }),
	]);
	assert.deepEqual(groups.map((g) => g.kind), ["map", "handout"]);
	assert.deepEqual(groups[0].rows.map((h) => h.id), ["map"]);

	const onlyLetters = groupHandouts([handout({ kind: "handout" })]);
	assert.equal(onlyLetters.length, 1);
	assert.equal(onlyLetters[0].kind, "handout");
});

test("every status the server can send has a person-readable label", () => {
	for (const status of ["draft", "published", "retired"]) {
		assert.ok(STATUS_LABELS[status], `${status} has no label`);
	}
});
