// Workspace presets: the shapes a fresh account seeds its slots from, per
// game and per seat (ADR 22). Split out of workspaces.js so the table is
// data a test can read — a preset naming a tool the seat cannot open is a
// bug the loader would only surface at someone's first boot.
//
// Presets seed and nothing more (the MAD-487 rule): the moment a slot is
// rearranged it is the user's, saved server-side, and the preset is only a
// thing to reset back to. Seeding happens once per slot the account has
// never saved, so the seat a later visit resolves to does not rewrite
// anyone's saved layouts.

import * as T from "./tree.js";
import { inCorpus, inSeat } from "./registry.js";

const leaf = T.leaf;
const row = (...kids) => T.split("row", kids);
const tabbed = (...kids) => T.tabs(kids, 0);

/**
 * The shapes a DM actually works in. Prep and play want different tools on
 * screen at once, and rebuilding that by hand every time is the friction this
 * whole redesign exists to remove.
 *
 * Each entry is a thunk: trees carry generated ids, so a preset has to be
 * built fresh each time rather than shared between slots.
 */
const DM = {
	dnd: [
		{ slot: 1, name: "Prep", build: () => row(leaf("planner"), tabbed(leaf("campaign"), leaf("cchat"))) },
		// At the table (MAD-318): play mode. The screen strip carries the
		// scene, the clock, the notes and the copilot mount; the tracker
		// dominates; vitals, dice, the director's advisory panel and the
		// rest sit behind the board's tab — advice beside the tracker,
		// never crowding the controls the DM is mid-tap on.
		// One Alt+2, nothing rearranged.
		{ slot: 2, name: "At the table", build: () =>
			T.split("row", [
				leaf("screen"),
				leaf("combat"),
				tabbed(leaf("board"), leaf("dice"), leaf("director"), leaf("sessions"), leaf("encounter"), leaf("handouts")),
			], [0.24, 0.42, 0.34]) },
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

/**
 * The player seat (ADR 22): the table, the world, the books. The board is
 * the party, live; the dice window's quick rolls bind the player's own
 * character server-side; the handouts tab is what the DM has handed the
 * party — letters and maps, published material only. The Campaign tool
 * carries the known world at the player's scope — met entities and place
 * dossiers, the quest journal, and their own sheet. The Grimoire answers
 * as what the party has learned. Observers get the same seat: nothing
 * character-shaped reaches one, and the tools degrade themselves — no
 * sheet, no quick-roll chips.
 */
const PLAYER = {
	dnd: [
		{ slot: 1, name: "The table", build: () =>
			T.split("row", [
				leaf("board"),
				tabbed(leaf("dice"), leaf("handouts"), leaf("cchat")),
			], [0.56, 0.44]) },
		{ slot: 2, name: "The world", build: () => row(leaf("campaign"), leaf("cchat")) },
		{ slot: 3, name: "Study", build: () => row(leaf("reader"), leaf("study")) },
	],
	mtg: [], // campaigns are a D&D surface; Magic keeps the DM shapes
};

/** The preset sets there are, keyed by corpus then seat. */
export const PRESETS = { dm: DM, player: PLAYER };

/** One seat's presets for one game, in slot order. */
export const presetsFor = (corpus, seat) => (PRESETS[seat] || {})[corpus] || [];

/** One seat's preset for one slot, or null when that slot has none. */
export function presetFor(corpus, seat, slot) {
	return presetsFor(corpus, seat).find((p) => p.slot === slot) || null;
}

/**
 * Does a preset's every leaf name a tool its own seat may open? The loader
 * trusts this table; the test does not.
 */
export function presetIsOfferable(preset, corpus, seat) {
	if (!preset) return true;
	return T.leaves(preset.build()).every((node) =>
		inCorpus(node.tool, corpus) && inSeat(node.tool, seat));
}
