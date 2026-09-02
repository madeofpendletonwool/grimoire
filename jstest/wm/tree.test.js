// Tests for the layout tree. Run with: node --test jstest/
//
// Every case here is a workspace the user could actually build, and the
// assertions are about the properties that must never break: fractions sum to
// one, containers never outlive their children, and a round trip through
// storage returns what went in.

import test from "node:test";
import assert from "node:assert/strict";

import {
	_resetIDs, leaf, split, tabs, find, leaves, parentOf, pathTo, firstLeaf,
	insert, remove, toggleContainer, setSplitDir, setActive, resize,
	focusDir, moveDir, serialize, parse, toolsIn, findByTool,
} from "../../web/static/js/wm/tree.js";

const tools = (root) => leaves(root).map((l) => l.tool);
const sum = (a) => a.reduce((x, y) => x + y, 0);
const near = (a, b) => Math.abs(a - b) < 1e-9;

/** Every split in the tree must have one fraction per child, summing to 1. */
function assertInvariants(root) {
	if (!root) return;
	const seen = new Set();
	(function check(n) {
		assert.ok(!seen.has(n.id), `duplicate id ${n.id}`);
		seen.add(n.id);
		if (n.t === "leaf") return;
		assert.ok(n.kids.length >= 2, `container ${n.id} has ${n.kids.length} kids`);
		if (n.t === "split") {
			assert.equal(n.fr.length, n.kids.length, `fr/kids mismatch at ${n.id}`);
			assert.ok(near(sum(n.fr), 1), `fractions sum to ${sum(n.fr)} at ${n.id}`);
			assert.ok(n.fr.every((f) => f > 0), `non-positive fraction at ${n.id}`);
		}
		if (n.t === "tabs") {
			assert.ok(n.active >= 0 && n.active < n.kids.length, `active out of range at ${n.id}`);
		}
		n.kids.forEach(check);
	})(root);
}

test.beforeEach(() => _resetIDs());

/* ---------- construction & queries ---------- */

test("a single leaf is a valid tree", () => {
	const root = leaf("chat");
	assert.equal(firstLeaf(root).tool, "chat");
	assert.deepEqual(tools(root), ["chat"]);
	assert.equal(parentOf(root, root.id), null);
});

test("pathTo returns the chain from root to node", () => {
	const a = leaf("chat"), b = leaf("planner");
	const root = split("row", [a, b]);
	assert.deepEqual(pathTo(root, b.id).map((n) => n.id), [root.id, b.id]);
	assert.equal(pathTo(root, "nope"), null);
});

test("firstLeaf follows the active tab, not the first child", () => {
	const root = tabs([leaf("chat"), leaf("planner")], 1);
	assert.equal(firstLeaf(root).tool, "planner");
});

test("toolsIn and findByTool locate open tools", () => {
	const root = split("row", [leaf("chat"), tabs([leaf("planner"), leaf("review")])]);
	assert.deepEqual([...toolsIn(root)].sort(), ["chat", "planner", "review"]);
	assert.equal(findByTool(root, "review").tool, "review");
	assert.equal(findByTool(root, "deck"), null);
});

/* ---------- insert ---------- */

test("inserting beside a lone leaf creates a split", () => {
	const a = leaf("chat");
	const root = insert(a, a.id, leaf("planner"), "row-after");
	assert.equal(root.t, "split");
	assert.deepEqual(tools(root), ["chat", "planner"]);
	assert.deepEqual(root.fr, [0.5, 0.5]);
	assertInvariants(root);
});

test("row-before puts the newcomer on the left", () => {
	const a = leaf("chat");
	const root = insert(a, a.id, leaf("planner"), "row-before");
	assert.deepEqual(tools(root), ["planner", "chat"]);
});

test("inserting along the parent's axis joins it instead of nesting", () => {
	const a = leaf("chat");
	let root = insert(a, a.id, leaf("planner"), "row-after");
	const target = findByTool(root, "planner");
	root = insert(root, target.id, leaf("review"), "row-after");

	assert.equal(root.t, "split", "should still be one split, not nested");
	assert.equal(root.kids.length, 3);
	assert.ok(root.kids.every((k) => k.t === "leaf"), "no nesting");
	assert.deepEqual(tools(root), ["chat", "planner", "review"]);
	assertInvariants(root);
});

test("joining a split splits the target's share, leaving siblings alone", () => {
	const a = leaf("chat");
	let root = insert(a, a.id, leaf("planner"), "row-after");   // [.5, .5]
	const target = findByTool(root, "planner");
	root = insert(root, target.id, leaf("review"), "row-after");

	assert.ok(near(root.fr[0], 0.5), "untouched sibling keeps its width");
	assert.ok(near(root.fr[1], 0.25));
	assert.ok(near(root.fr[2], 0.25));
	assertInvariants(root);
});

test("inserting across the axis nests a new split", () => {
	const a = leaf("chat");
	let root = insert(a, a.id, leaf("planner"), "row-after");
	const target = findByTool(root, "planner");
	root = insert(root, target.id, leaf("sessions"), "col-after");

	assert.equal(root.t, "split");
	assert.equal(root.dir, "row");
	assert.equal(root.kids[1].t, "split");
	assert.equal(root.kids[1].dir, "col");
	assert.deepEqual(tools(root), ["chat", "planner", "sessions"]);
	assertInvariants(root);
});

test("how=tab wraps a leaf into a tabs container and focuses the newcomer", () => {
	const a = leaf("chat");
	const root = insert(a, a.id, leaf("planner"), "tab");
	assert.equal(root.t, "tabs");
	assert.equal(root.active, 1, "the new tab is the visible one");
	assertInvariants(root);
});

test("how=tab on an existing tabs container appends rather than nesting", () => {
	let root = tabs([leaf("chat"), leaf("planner")], 0);
	root = insert(root, root.id, leaf("review"), "tab");
	assert.equal(root.t, "tabs");
	assert.equal(root.kids.length, 3);
	assert.equal(root.active, 2);
	assertInvariants(root);
});

test("inserting into an empty tree yields the node itself", () => {
	const n = leaf("chat");
	assert.equal(insert(null, "anything", n), n);
});

/* ---------- remove ---------- */

test("removing the only leaf empties the tree", () => {
	const a = leaf("chat");
	assert.equal(remove(a, a.id), null);
});

test("removing one of two collapses the split away", () => {
	const a = leaf("chat");
	const root = insert(a, a.id, leaf("planner"), "row-after");
	const gone = remove(root, findByTool(root, "planner").id);
	assert.equal(gone.t, "leaf", "the surviving leaf replaces the split");
	assert.equal(gone.tool, "chat");
});

test("removing renormalises the survivors' fractions", () => {
	let root = split("row", [leaf("chat"), leaf("planner"), leaf("review")], [0.5, 0.25, 0.25]);
	root = remove(root, findByTool(root, "chat").id);
	assert.ok(near(sum(root.fr), 1), `fractions sum to ${sum(root.fr)}`);
	assert.deepEqual(tools(root), ["planner", "review"]);
	assertInvariants(root);
});

test("removing an unknown id leaves the tree untouched", () => {
	const root = split("row", [leaf("chat"), leaf("planner")]);
	assert.equal(remove(root, "ghost"), root);
});

test("removing collapses nested containers all the way up", () => {
	const inner = split("col", [leaf("planner"), leaf("sessions")]);
	let root = split("row", [leaf("chat"), inner]);
	root = remove(root, findByTool(root, "sessions").id);
	root = remove(root, findByTool(root, "planner").id);
	assert.equal(root.t, "leaf");
	assert.equal(root.tool, "chat");
});

/* ---------- containers ---------- */

test("toggleContainer swaps split and tabs, keeping the id and children", () => {
	const root = split("row", [leaf("chat"), leaf("planner")]);
	const tabbed = toggleContainer(root, root.id);
	assert.equal(tabbed.t, "tabs");
	assert.equal(tabbed.id, root.id, "id survives so focus is not lost");
	assert.deepEqual(tools(tabbed), ["chat", "planner"]);

	const back = toggleContainer(tabbed, tabbed.id);
	assert.equal(back.t, "split");
	assertInvariants(back);
});

test("setSplitDir flips a split's axis", () => {
	const root = split("row", [leaf("chat"), leaf("planner")]);
	assert.equal(setSplitDir(root, root.id, "col").dir, "col");
});

// Removing a tab used to leave `active` pointing past the end, so no panel
// matched and the container rendered blank.
test("removing an earlier tab keeps the same one showing", () => {
	const root = tabs([leaf("a"), leaf("b"), leaf("c")], 2);
	const out = remove(root, findByTool(root, "a").id);
	assert.equal(out.active, 1);
	assert.equal(firstLeaf(out).tool, "c", "the visible tab must not change under the user");
	assertInvariants(out);
});

test("removing the visible tab falls to what took its place", () => {
	const root = tabs([leaf("a"), leaf("b"), leaf("c")], 2);
	const out = remove(root, findByTool(root, "c").id);
	assert.equal(out.active, 1);
	assert.equal(firstLeaf(out).tool, "b");
	assertInvariants(out);
});

test("removing a middle tab keeps the visible one visible", () => {
	const root = tabs([leaf("a"), leaf("b"), leaf("c"), leaf("d")], 3);
	const out = remove(root, findByTool(root, "b").id);
	assert.equal(firstLeaf(out).tool, "d");
	assertInvariants(out);
});

test("active stays in range however tabs are removed", () => {
	for (const victim of ["a", "b", "c"]) {
		for (let active = 0; active < 3; active++) {
			const root = tabs([leaf("a"), leaf("b"), leaf("c")], active);
			const out = remove(root, findByTool(root, victim).id);
			if (out.t !== "tabs") continue;   // collapsed to a lone leaf
			assert.ok(out.active >= 0 && out.active < out.kids.length,
				`active ${out.active} out of range removing ${victim} with active ${active}`);
			assert.ok(firstLeaf(out), "no visible tab");
		}
	}
});

test("setActive clamps out-of-range tab indices", () => {
	const root = tabs([leaf("chat"), leaf("planner")], 0);
	assert.equal(setActive(root, root.id, 9).active, 1);
	assert.equal(setActive(root, root.id, -3).active, 0);
});

/* ---------- resize ---------- */

test("resize moves a boundary and conserves the total", () => {
	const root = split("row", [leaf("chat"), leaf("planner")]);
	const out = resize(root, root.id, 0, 0.2);
	assert.ok(near(out.fr[0], 0.7));
	assert.ok(near(out.fr[1], 0.3));
	assertInvariants(out);
});

test("resize floors both neighbours at 5% so a window cannot vanish", () => {
	const root = split("row", [leaf("chat"), leaf("planner")]);
	const out = resize(root, root.id, 0, 99);
	assert.ok(near(out.fr[1], 0.05), `neighbour got ${out.fr[1]}`);
	assert.ok(near(sum(out.fr), 1));

	const back = resize(root, root.id, 0, -99);
	assert.ok(near(back.fr[0], 0.05));
});

test("resize only touches the two neighbours of the dragged gutter", () => {
	const root = split("row", [leaf("a"), leaf("b"), leaf("c")]);
	const out = resize(root, root.id, 0, 0.1);
	assert.ok(near(out.fr[2], root.fr[2]), "the far sibling must not move");
	assert.ok(near(sum(out.fr), 1));
});

test("resize ignores a gutter index that is not a boundary", () => {
	const root = split("row", [leaf("a"), leaf("b")]);
	assert.equal(resize(root, root.id, 5, 0.1), root);
	assert.equal(resize(root, root.id, -1, 0.1), root);
});

/* ---------- directional focus ---------- */

test("focusDir crosses to the sibling on that side", () => {
	const root = split("row", [leaf("chat"), leaf("planner")]);
	const chat = findByTool(root, "chat");
	assert.equal(find(root, focusDir(root, chat.id, "right")).tool, "planner");
	assert.equal(focusDir(root, chat.id, "left"), null, "nothing to the left of the leftmost");
});

test("focusDir skips ancestors laid out on the other axis", () => {
	//  chat | [ planner ]
	//         [ sessions]
	const inner = split("col", [leaf("planner"), leaf("sessions")]);
	const root = split("row", [leaf("chat"), inner]);
	const sessions = findByTool(root, "sessions");

	// Moving left from sessions has no col-sibling, so it walks up to the row.
	assert.equal(find(root, focusDir(root, sessions.id, "left")).tool, "chat");
	assert.equal(find(root, focusDir(root, sessions.id, "up")).tool, "planner");
	assert.equal(focusDir(root, sessions.id, "down"), null);
});

test("focusDir into a container lands on its visible leaf", () => {
	const inner = tabs([leaf("planner"), leaf("review")], 1);
	const root = split("row", [leaf("chat"), inner]);
	const chat = findByTool(root, "chat");
	assert.equal(find(root, focusDir(root, chat.id, "right")).tool, "review",
		"lands on the active tab, not a hidden one");
});

test("focusDir rejects a bogus direction or unknown id", () => {
	const root = split("row", [leaf("chat"), leaf("planner")]);
	assert.equal(focusDir(root, root.kids[0].id, "sideways"), null);
	assert.equal(focusDir(root, "ghost", "right"), null);
});

/* ---------- moving windows ---------- */

test("moveDir reorders within the same split", () => {
	const root = split("row", [leaf("chat"), leaf("planner"), leaf("review")], [0.5, 0.3, 0.2]);
	const out = moveDir(root, findByTool(root, "chat").id, "right");
	assert.deepEqual(tools(out), ["planner", "chat", "review"]);
	assert.ok(near(sum(out.fr), 1));
	assertInvariants(out);
});

test("moveDir carries the window's own width with it", () => {
	const root = split("row", [leaf("chat"), leaf("planner")], [0.7, 0.3]);
	const out = moveDir(root, findByTool(root, "chat").id, "right");
	assert.deepEqual(tools(out), ["planner", "chat"]);
	assert.ok(near(out.fr[1], 0.7), "chat keeps 70% after the swap");
});

test("moveDir at the edge is a no-op", () => {
	const root = split("row", [leaf("chat"), leaf("planner")]);
	assert.equal(moveDir(root, findByTool(root, "chat").id, "left"), root);
});

test("moveDir across a boundary detaches and re-inserts", () => {
	const inner = split("col", [leaf("planner"), leaf("sessions")]);
	const root = split("row", [leaf("chat"), inner]);
	const out = moveDir(root, findByTool(root, "chat").id, "right");

	assert.deepEqual(tools(out).sort(), ["chat", "planner", "sessions"]);
	assert.equal(leaves(out).length, 3, "no window was lost in transit");
	assertInvariants(out);
});

test("moveDir never destroys a window", () => {
	let root = split("row", [leaf("a"), split("col", [leaf("b"), leaf("c")])]);
	for (const dir of ["right", "down", "left", "up", "right", "up"]) {
		const before = leaves(root).length;
		root = moveDir(root, findByTool(root, "a").id, dir);
		assert.equal(leaves(root).length, before, `lost a window moving ${dir}`);
		assert.ok(findByTool(root, "a"), `"a" vanished moving ${dir}`);
		assertInvariants(root);
	}
});

test("moveDir with a bogus direction or unknown id is a no-op", () => {
	const root = split("row", [leaf("chat"), leaf("planner")]);
	assert.equal(moveDir(root, root.kids[0].id, "nowhere"), root);
	assert.equal(moveDir(root, "ghost", "right"), root);
});

/* ---------- serialisation ---------- */

test("a tree survives a round trip", () => {
	const root = split("row", [
		leaf("chat"),
		tabs([leaf("planner"), leaf("review")], 1),
	], [0.6, 0.4]);

	const { root: back, dropped } = parse(JSON.parse(JSON.stringify(serialize(root))));
	assert.deepEqual(dropped, []);
	assert.deepEqual(tools(back), tools(root));
	assert.equal(back.dir, "row");
	assert.ok(near(back.fr[0], 0.6));
	assert.equal(back.kids[1].t, "tabs");
	assert.equal(back.kids[1].active, 1);
	assertInvariants(back);
});

test("serialize omits ids so stored layouts do not pin runtime identity", () => {
	const json = serialize(split("row", [leaf("chat"), leaf("planner")]));
	assert.ok(!("id" in json));
	assert.ok(!("id" in json.kids[0]));
});

test("parse drops unknown tools and reports them", () => {
	const stored = serialize(split("row", [leaf("chat"), leaf("retired-tool")]));
	const { root, dropped } = parse(stored, { isKnownTool: (t) => t === "chat" });
	assert.deepEqual(dropped, ["retired-tool"]);
	assert.equal(root.t, "leaf", "the survivor is promoted, not left in a one-child split");
	assert.equal(root.tool, "chat");
});

test("parse returns null rather than an empty container when everything is unknown", () => {
	const stored = serialize(split("row", [leaf("gone-a"), leaf("gone-b")]));
	const { root, dropped } = parse(stored, { isKnownTool: () => false });
	assert.equal(root, null);
	assert.deepEqual(dropped, ["gone-a", "gone-b"]);
});

test("parse rejects junk without throwing", () => {
	for (const junk of [null, undefined, 42, "chat", [], {}, { t: "bogus" }, { t: "split" }, { t: "leaf" }]) {
		assert.equal(parse(junk).root, null, `threw or accepted: ${JSON.stringify(junk)}`);
	}
});

test("parse repairs fractions that do not match the children", () => {
	const { root } = parse({
		t: "split", dir: "row", fr: [5, 5, 5],
		kids: [{ t: "leaf", tool: "chat" }, { t: "leaf", tool: "planner" }],
	});
	assert.equal(root.fr.length, 2);
	assert.ok(near(sum(root.fr), 1));
});

test("parse repairs negative and non-numeric fractions", () => {
	const { root } = parse({
		t: "split", dir: "row", fr: [-1, "x"],
		kids: [{ t: "leaf", tool: "chat" }, { t: "leaf", tool: "planner" }],
	});
	assert.deepEqual(root.fr, [0.5, 0.5]);
});

test("parse caps depth so a hostile payload cannot blow the stack", () => {
	let deep = { t: "leaf", tool: "chat" };
	for (let i = 0; i < 50; i++) {
		deep = { t: "split", dir: "row", kids: [deep, { t: "leaf", tool: "chat" }] };
	}
	const { root } = parse(deep);
	assert.ok(root, "should degrade, not throw");
	assert.ok(leaves(root).length <= 200);
	assertInvariants(root);
});

test("parse caps total node count", () => {
	const kids = Array.from({ length: 500 }, () => ({ t: "leaf", tool: "chat" }));
	const { root } = parse({ t: "split", dir: "row", fr: kids.map(() => 1), kids });
	assert.ok(leaves(root).length <= 200);
	assertInvariants(root);
});

test("parse clamps an out-of-range active tab", () => {
	const { root } = parse({
		t: "tabs", active: 99,
		kids: [{ t: "leaf", tool: "chat" }, { t: "leaf", tool: "planner" }],
	});
	assert.equal(root.active, 1);
});

test("serialize of an empty workspace is null", () => {
	assert.equal(serialize(null), null);
});

test("tabbing onto a leaf that is already tabbed joins its strip", () => {
	// Nesting instead of joining gave one tab strip per tool — four of them
	// stacked above a single window — which is what happens once a workspace
	// runs out of room to split and every new tool arrives as a tab.
	_resetIDs();
	let root = tabs([leaf("chat"), leaf("planner")], 0);
	const target = root.kids[0];

	root = insert(root, target.id, leaf("sessions"), "tab");
	assert.equal(root.t, "tabs");
	assert.equal(root.kids.length, 3);
	assert.deepEqual(root.kids.map((k) => k.tool), ["chat", "sessions", "planner"]);
	assert.equal(root.kids[root.active].tool, "sessions", "the newcomer is the tab you land on");

	// And again, from the tab that is now showing: still one strip.
	root = insert(root, root.kids[root.active].id, leaf("review"), "tab");
	assert.equal(root.kids.length, 4);
	assert.equal(leaves(root).length, 4);
	assert.equal(root.kids.every((k) => k.t === "leaf"), true, "no container nested inside the strip");
});
