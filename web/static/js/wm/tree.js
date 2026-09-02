// The layout tree — pure data, no DOM.
//
// A workspace is a tree of three node kinds:
//
//   leaf   one window holding one tool
//   split  children side by side, `dir` row or col, `fr` their fractions
//   tabs   children stacked, `active` the visible one
//
// Nothing here touches the document, which is why it is the one front-end
// module with tests (ADR 14): a layout engine that silently corrupts a tree
// loses the user's workspace, and the corruption is invisible until reload.
//
// Every mutator returns a **new root** and leaves its argument alone. Callers
// hold one root and replace it wholesale; that keeps undo and the debounced
// save honest, and it is why there are no parent pointers — a tree with cycles
// cannot be serialised, and serialising is the whole point.

let seq = 0;
/** Ids only have to be unique within a document, not across reloads. */
export const nextID = (p = "w") => `${p}${++seq}`;

/** Reset the counter. Tests only — never call this from the app. */
export function _resetIDs() { seq = 0; }

/* ---------- construction ---------- */

export const leaf = (tool, id = nextID("w")) => ({ t: "leaf", id, tool });

export function split(dir, kids, fr) {
	if (dir !== "row" && dir !== "col") throw new Error(`bad split dir: ${dir}`);
	return { t: "split", id: nextID("s"), dir, kids, fr: fr || even(kids.length) };
}

export const tabs = (kids, active = 0) => ({ t: "tabs", id: nextID("t"), kids, active });

const even = (n) => Array.from({ length: n }, () => 1 / n);
const isContainer = (n) => n.t === "split" || n.t === "tabs";

/* ---------- queries ---------- */

/** Depth-first walk, parents before children. */
export function walk(node, fn) {
	if (!node) return;
	fn(node);
	if (isContainer(node)) node.kids.forEach((k) => walk(k, fn));
}

export function leaves(node) {
	const out = [];
	walk(node, (n) => { if (n.t === "leaf") out.push(n); });
	return out;
}

export function find(root, id) {
	let hit = null;
	walk(root, (n) => { if (n.id === id) hit = n; });
	return hit;
}

/** The chain from root down to `id`, inclusive. Null when absent. */
export function pathTo(root, id) {
	if (!root) return null;
	if (root.id === id) return [root];
	if (!isContainer(root)) return null;
	for (const k of root.kids) {
		const sub = pathTo(k, id);
		if (sub) return [root, ...sub];
	}
	return null;
}

export function parentOf(root, id) {
	const p = pathTo(root, id);
	return p && p.length >= 2 ? p[p.length - 2] : null;
}

/** The leaf a container shows first — the active tab, or the leftmost child. */
export function firstLeaf(node) {
	if (!node) return null;
	if (node.t === "leaf") return node;
	if (node.t === "tabs") return firstLeaf(node.kids[node.active] || node.kids[0]);
	return firstLeaf(node.kids[0]);
}

/** Every tool id in the tree, for "is this tool already open?" checks. */
export const toolsIn = (root) => new Set(leaves(root).map((l) => l.tool));

export const findByTool = (root, tool) => leaves(root).find((l) => l.tool === tool) || null;

/* ---------- structural edits ---------- */

const clone = (n) => (isContainer(n) ? { ...n, kids: n.kids.slice(), fr: n.fr ? n.fr.slice() : undefined } : { ...n });

/**
 * Rebuild the tree, replacing whatever `id` names with `fn`'s result.
 * Returning null drops the node; the parent collapses it away.
 */
function replace(node, id, fn) {
	if (!node) return null;
	if (node.id === id) return fn(node);
	if (!isContainer(node)) return node;

	const kids = [];
	const fr = [];
	let changed = false;
	// Which child was showing, so a tabbed container keeps showing it even
	// when an earlier sibling is removed and every index shifts down.
	const wasActive = node.t === "tabs" ? node.kids[node.active] : null;
	let active = -1;

	node.kids.forEach((k, i) => {
		const next = replace(k, id, fn);
		if (next !== k) changed = true;
		if (next) {
			if (k === wasActive || next === wasActive) active = kids.length;
			kids.push(next);
			if (node.fr) fr.push(node.fr[i]);
		} else {
			changed = true;
		}
	});
	if (!changed) return node;

	const out = { ...node, kids, fr: node.fr ? renorm(fr) : undefined };
	if (node.t === "tabs") {
		// The visible tab was the one removed: fall to whatever took its
		// place, or to the last tab if it was the last.
		out.active = active >= 0 ? active : clamp(node.active, 0, kids.length - 1);
	}
	return collapse(out);
}

/** A container with one child is not a container. With none, it is nothing. */
function collapse(node) {
	if (!isContainer(node)) return node;
	if (node.kids.length === 0) return null;
	if (node.kids.length === 1) return node.kids[0];
	return node;
}

const renorm = (fr) => {
	const sum = fr.reduce((a, b) => a + b, 0);
	return sum > 0 ? fr.map((f) => f / sum) : even(fr.length);
};

/**
 * Put `node` next to the leaf `targetId`.
 *
 * `how` is one of row-before / row-after / col-before / col-after / tab. When
 * the target's parent already runs along the requested axis the new node joins
 * it as a sibling rather than nesting a fresh split — otherwise every insert
 * would deepen the tree by one and the fractions would stop meaning anything.
 */
export function insert(root, targetId, node, how = "row-after") {
	if (!root) return node;

	const target = find(root, targetId);
	if (!target) return root;

	if (how === "tab") {
		// Join the container the target is already in, rather than wrapping the
		// target in a second one. Nesting gave a tab strip per tool — four of
		// them stacked above one window — instead of one strip with four tabs.
		const host = parentOf(root, targetId);
		if (host && host.t === "tabs") {
			const at = host.kids.indexOf(target) + 1;
			const kids = host.kids.slice();
			kids.splice(at, 0, node);
			return replace(root, host.id, () => ({ ...host, kids, active: at }));
		}
		return replace(root, targetId, (n) =>
			n.t === "tabs"
				? { ...n, kids: [...n.kids, node], active: n.kids.length }
				: tabs([n, node], 1));
	}

	const [axis, side] = how.split("-");
	const dir = axis === "row" ? "row" : "col";
	const parent = parentOf(root, targetId);

	if (parent && parent.t === "split" && parent.dir === dir) {
		// Join the existing split: halve the target's share with the newcomer,
		// so the other siblings keep the width they were given.
		const i = parent.kids.indexOf(target);
		const kids = parent.kids.slice();
		const fr = parent.fr.slice();
		const share = fr[i] / 2;
		fr[i] = share;
		const at = side === "before" ? i : i + 1;
		kids.splice(at, 0, node);
		fr.splice(at, 0, share);
		return replace(root, parent.id, () => ({ ...parent, kids, fr }));
	}

	return replace(root, targetId, (n) =>
		split(dir, side === "before" ? [node, n] : [n, node]));
}

export function remove(root, id) {
	if (!root) return null;
	if (root.id === id) return null;
	return replace(root, id, () => null);
}

/** Swap a container between side-by-side and tabbed. */
export function toggleContainer(root, id) {
	return replace(root, id, (n) => {
		if (n.t === "split") return { ...tabs(n.kids, 0), id: n.id };
		if (n.t === "tabs") return { ...split("row", n.kids), id: n.id };
		return n;
	});
}

export function setSplitDir(root, id, dir) {
	return replace(root, id, (n) => (n.t === "split" ? { ...n, dir } : n));
}

export function setActive(root, id, index) {
	return replace(root, id, (n) =>
		n.t === "tabs" ? { ...n, active: clamp(index, 0, n.kids.length - 1) } : n);
}

const clamp = (v, lo, hi) => Math.min(hi, Math.max(lo, v));

/**
 * Drag a gutter. `gutter` is the index of the boundary, so gutter 0 sits
 * between kids 0 and 1. Both neighbours are floored at 5% so a window can
 * never be dragged out of existence.
 */
export function resize(root, splitID, gutter, delta) {
	return replace(root, splitID, (n) => {
		if (n.t !== "split" || gutter < 0 || gutter >= n.kids.length - 1) return n;
		const fr = n.fr.slice();
		const a = fr[gutter], b = fr[gutter + 1];
		const d = clamp(delta, -(a - 0.05), b - 0.05);
		fr[gutter] = a + d;
		fr[gutter + 1] = b - d;
		return { ...n, fr };
	});
}

/* ---------- directional movement ---------- */

const AXIS = { left: "row", right: "row", up: "col", down: "col" };
const FORWARD = { left: false, right: true, up: false, down: true };

/**
 * The leaf that should take focus when moving `dir` from `id`.
 *
 * Walks up until it finds an ancestor laid out along the movement axis with a
 * sibling on that side, then descends into it. Tabbed containers are opaque —
 * moving right out of a tab lands on the container's neighbour, not on a
 * hidden sibling tab, because you cannot focus what you cannot see.
 */
export function focusDir(root, id, dir) {
	const axis = AXIS[dir];
	if (!axis) return null;
	const path = pathTo(root, id);
	if (!path) return null;

	for (let i = path.length - 2; i >= 0; i--) {
		const parent = path[i];
		if (parent.t !== "split" || parent.dir !== axis) continue;
		const idx = parent.kids.indexOf(path[i + 1]);
		const next = idx + (FORWARD[dir] ? 1 : -1);
		if (next < 0 || next >= parent.kids.length) continue;
		const target = firstLeaf(parent.kids[next]);
		if (target) return target.id;
	}
	return null;
}

/**
 * Move the window itself. Within its own split this is a reorder; across a
 * boundary it detaches and re-inserts beside the neighbour it lands on.
 */
export function moveDir(root, id, dir) {
	const axis = AXIS[dir];
	if (!axis) return root;
	const node = find(root, id);
	if (!node) return root;

	const parent = parentOf(root, id);
	if (parent && parent.t === "split" && parent.dir === axis) {
		const i = parent.kids.indexOf(node);
		const j = i + (FORWARD[dir] ? 1 : -1);
		if (j >= 0 && j < parent.kids.length) {
			const kids = parent.kids.slice();
			const fr = parent.fr.slice();
			[kids[i], kids[j]] = [kids[j], kids[i]];
			[fr[i], fr[j]] = [fr[j], fr[i]];
			return replace(root, parent.id, () => ({ ...parent, kids, fr }));
		}
	}

	const landing = focusDir(root, id, dir);
	if (!landing) return root;

	const detached = remove(root, id);
	if (!detached) return root;
	// The landing leaf survives the detach unless it was the last of its
	// container, in which case there is nowhere left to land.
	if (!find(detached, landing)) return root;
	const side = FORWARD[dir] ? "after" : "before";
	return insert(detached, landing, { ...node }, `${axis}-${side}`);
}

/* ---------- serialisation ---------- */

const MAX_NODES = 200;
const MAX_DEPTH = 12;

export function serialize(root) {
	if (!root) return null;
	if (root.t === "leaf") return { t: "leaf", tool: root.tool };
	const base = { t: root.t, kids: root.kids.map(serialize) };
	if (root.t === "split") return { ...base, dir: root.dir, fr: root.fr };
	return { ...base, active: root.active };
}

/**
 * Rebuild a tree from stored JSON.
 *
 * Hostile input is expected — the payload has been through a database and a
 * release where tools were renamed or removed. Unknown tools are dropped and
 * reported rather than throwing, because a stale tool id must cost the user
 * one window, never the whole workspace. Depth and node caps mirror the
 * server-side validator.
 */
export function parse(data, opts = {}) {
	const known = opts.isKnownTool || (() => true);
	const dropped = [];
	let count = 0;

	function build(n, depth) {
		if (!n || typeof n !== "object" || depth > MAX_DEPTH || ++count > MAX_NODES) return null;

		if (n.t === "leaf") {
			if (typeof n.tool !== "string") return null;
			if (!known(n.tool)) { dropped.push(n.tool); return null; }
			return leaf(n.tool);
		}
		if (n.t !== "split" && n.t !== "tabs") return null;
		if (!Array.isArray(n.kids)) return null;

		const kids = n.kids.map((k) => build(k, depth + 1)).filter(Boolean);
		if (kids.length === 0) return null;
		if (kids.length === 1) return kids[0];

		if (n.t === "tabs") return tabs(kids, clamp(Number(n.active) || 0, 0, kids.length - 1));

		const dir = n.dir === "col" ? "col" : "row";
		const fr = Array.isArray(n.fr) && n.fr.length === kids.length && n.fr.every((f) => Number.isFinite(f) && f > 0)
			? renorm(n.fr)
			: even(kids.length);
		return split(dir, kids, fr);
	}

	return { root: build(data, 0), dropped };
}
