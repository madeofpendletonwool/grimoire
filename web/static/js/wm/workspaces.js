// Workspaces: named layouts, one set per game, switched with Alt+N.
//
// This is where corpus separation actually lives. The old shell kept one
// global "which surface is open" and closed the other game's surfaces by
// name when you switched (closeForeignSurfaces in app.js). Here, Magic and
// D&D each own nine slots; switching games swaps which set is in play and
// leaves the other exactly as it was, so going back is one keystroke rather
// than a rebuild.
//
// Presets seed an account and nothing more. The moment a slot is rearranged
// it is the user's, saved server-side, and the preset is only a thing to
// reset back to.

import { api } from "../api.js";
import { debounce } from "../dom.js";
import * as T from "./tree.js";
import { CORPORA, isCorpus, knownTool, toolsFor } from "./registry.js";
import * as wm from "./wm.js";

const SLOTS = 9;
const SAVE_DELAY = 600;   // a gutter drag emits a change per frame

/* ---------- presets ---------- */

const leaf = T.leaf;
const row = (...kids) => T.split("row", kids);
const col = (...kids) => T.split("col", kids);
const tabbed = (...kids) => T.tabs(kids, 0);

/**
 * The shapes a DM actually works in. Prep and play want different tools on
 * screen at once, and rebuilding that by hand every time is the friction this
 * whole redesign exists to remove.
 *
 * Each entry is a thunk: trees carry generated ids, so a preset has to be
 * built fresh each time rather than shared between slots.
 */
const PRESETS = {
	dnd: [
		{ slot: 1, name: "Prep", build: () => row(leaf("planner"), tabbed(leaf("campaign"), leaf("cchat"))) },
		{ slot: 2, name: "At the table", build: () => row(leaf("chat"), tabbed(leaf("sessions"), leaf("encounter"))) },
		{ slot: 3, name: "Canon", build: () => row(leaf("review"), leaf("sessions")) },
		{ slot: 4, name: "Study", build: () => row(leaf("reader"), leaf("study")) },
	],
	mtg: [
		{ slot: 1, name: "Ask", build: () => leaf("chat") },
		{ slot: 2, name: "Brew", build: () => row(leaf("deck"), leaf("chat")) },
		{ slot: 3, name: "Study", build: () => row(leaf("reader"), leaf("study")) },
		// Slot 4 is where Table Play lands (docs/table/): log, board, seats.
	],
};

const presetFor = (corpus, slot) => (PRESETS[corpus] || []).find((p) => p.slot === slot) || null;

const emptySlot = (slot) => ({ slot, name: `Workspace ${slot}`, tree: null, focus: null, seeded: false });

/** Build a slot from its preset, or an empty one if it has none. */
function seed(corpus, slot) {
	const preset = presetFor(corpus, slot);
	if (!preset) return emptySlot(slot);
	return { slot, name: preset.name, tree: preset.build(), focus: null, seeded: true };
}

/* ---------- state ---------- */

const state = {
	corpus: "mtg",
	// corpus -> slot number -> { slot, name, tree, focus }
	sets: new Map(CORPORA.map((c) => [c, new Map()])),
	active: new Map(CORPORA.map((c) => [c, 1])),
	loaded: new Set(),
	dropped: [],         // tools a stored layout named that no longer exist
	onUpdate: () => {},
};

const setFor = (corpus) => state.sets.get(corpus);

export const activeSlot = () => state.active.get(state.corpus);
export const currentCorpus = () => state.corpus;

/** Every slot in the active set, for the workspace strip. */
export function list(corpus = state.corpus) {
	const set = setFor(corpus);
	if (!set) return [];
	return Array.from({ length: SLOTS }, (_, i) => set.get(i + 1) || emptySlot(i + 1));
}

/** Tools a stored layout referenced that this release no longer ships. */
export const droppedTools = () => state.dropped.slice();

/* ---------- loading ---------- */

/**
 * Load one game's saved workspaces, seeding presets for slots the account has
 * never touched.
 *
 * A failed load is not fatal: presets are a working app, and refusing to open
 * because the layout service is down would be a worse outcome than opening in
 * a default arrangement.
 */
export async function loadSet(corpus) {
	if (!isCorpus(corpus) || state.loaded.has(corpus)) return;
	state.loaded.add(corpus);
	const set = setFor(corpus);

	let saved = [];
	try {
		saved = (await api.uiLayouts(corpus)).layouts || [];
	} catch (err) {
		console.error(`could not load ${corpus} layouts:`, err);
	}

	for (const row of saved) {
		const { root, dropped } = T.parse(row.tree, { isKnownTool: knownTool });
		if (dropped.length) state.dropped.push(...dropped);
		set.set(row.slot, { slot: row.slot, name: row.name, tree: root, focus: null, seeded: false });
	}

	// Only seed slots the account has never saved. A user who deliberately
	// emptied slot 1 must not find Prep back in it on the next sign-in.
	for (const preset of PRESETS[corpus] || []) {
		if (!set.has(preset.slot)) set.set(preset.slot, seed(corpus, preset.slot));
	}
	state.onUpdate();
}

/* ---------- switching ---------- */

/**
 * Switch games. The other set stays in memory untouched, which is the whole
 * point: coming back is instant and nothing was closed on your behalf.
 */
export async function switchCorpus(corpus) {
	if (!isCorpus(corpus) || corpus === state.corpus) return;
	captureCurrent();
	state.corpus = corpus;
	await loadSet(corpus);
	wm.setCorpusScope(corpus);
	applyActive();
}

export function switchTo(slot) {
	if (slot < 1 || slot > SLOTS || slot === activeSlot()) return;
	captureCurrent();
	state.active.set(state.corpus, slot);
	applyActive();
}

function applyActive() {
	const slot = activeSlot();
	const entry = setFor(state.corpus).get(slot) || emptySlot(slot);
	setFor(state.corpus).set(slot, entry);
	wm.setLayout(entry.tree, { focusTool: entry.focus });
	state.onUpdate();
}

/** Copy the live tree back into the slot before leaving it. */
function captureCurrent() {
	const slot = activeSlot();
	const set = setFor(state.corpus);
	const entry = set.get(slot) || emptySlot(slot);
	entry.tree = wm.currentTree();
	entry.focus = wm.focusedTool();
	set.set(slot, entry);
}

/* ---------- editing ---------- */

export function rename(slot, name) {
	const trimmed = String(name || "").trim().slice(0, 80);
	if (!trimmed) return;
	const set = setFor(state.corpus);
	const entry = set.get(slot) || emptySlot(slot);
	entry.name = trimmed;
	set.set(slot, entry);
	save(slot);
	state.onUpdate();
}

/** Put a slot back to its preset — the escape hatch from a wrecked layout. */
export function reset(slot = activeSlot()) {
	const fresh = seed(state.corpus, slot);
	setFor(state.corpus).set(slot, fresh);
	if (slot === activeSlot()) wm.setLayout(fresh.tree, { focusTool: null });
	// Clearing the row rather than writing the preset back means a future
	// change to the preset reaches accounts that never customised the slot.
	api.uiDeleteLayout(state.corpus, slot).catch(() => { /* presets still work */ });
	state.onUpdate();
}

/* ---------- persistence ---------- */

const savers = new Map();

/**
 * Save one slot, debounced. Dragging a gutter emits a change per frame and
 * every one of them is a legitimate new layout; the user only cares about
 * where they let go.
 */
function save(slot) {
	if (!savers.has(slot)) {
		savers.set(slot, debounce(async (corpus, s) => {
			const entry = setFor(corpus)?.get(s);
			if (!entry) return;
			try {
				await api.uiSaveLayout(corpus, s, entry.name, T.serialize(entry.tree));
			} catch (err) {
				// A layout that did not reach the server is a lost arrangement,
				// never a lost window — the live tree is unaffected.
				console.error(`could not save workspace ${s}:`, err);
			}
		}, SAVE_DELAY));
	}
	savers.get(slot)(state.corpus, slot);
}

/* ---------- wiring ---------- */

/**
 * Start the workspace layer. `onUpdate` fires whenever the strip should be
 * redrawn — a switch, a rename, a load.
 */
export async function initWorkspaces(corpus, onUpdate = () => {}) {
	state.corpus = isCorpus(corpus) ? corpus : "mtg";
	state.onUpdate = onUpdate;

	wm.onChange(() => {
		captureCurrent();
		save(activeSlot());
	});

	await loadSet(state.corpus);
	wm.setCorpusScope(state.corpus);
	applyActive();
}

/**
 * Tools from the other game are unreachable through the rail and the command
 * menu, but a layout saved before a tool moved games could still name one.
 * Anything out of scope is dropped on load rather than rendered.
 */
export function pruneForeign(corpus) {
	const allowed = new Set(toolsFor(corpus));
	let tree = wm.currentTree();
	for (const leafNode of T.leaves(tree)) {
		if (!allowed.has(leafNode.tool)) tree = T.remove(tree, leafNode.id);
	}
	return tree;
}
