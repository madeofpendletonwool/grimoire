// The tool registry — the one file a new tool touches.
//
// Before this existed, adding a surface meant coordinated edits in four
// places: markup and a rail button in index.html, an import plus a `safe()`
// call plus an entry in closeForeignSurfaces in app.js, an `is-*` rule and a
// corpus visibility rule in style.css, and a registration in pixel.css's
// selector manifest. Four files, four chances to forget one, and the MTG
// Table Play stack (docs/table/, ADRs 9-13) is about to add several tools at
// once.
//
// Now: one entry here plus one module exporting `tool`. Everything else — the
// rail, the command menu, the keyboard cheat sheet, corpus filtering, what a
// saved layout is allowed to name — reads this table.
//
// Fields
//   title      what the titlebar, rail and command menu call it
//   icon       a sprite name from ICONS in icons.js (32px pixel art)
//   corpus     "dnd", "mtg", or "*" for the tools both games share
//   accel      the letter after the leader: Ctrl+G then this opens the tool
//   instances  "single" — one window per tool. See the note at the bottom.
//   min        [width, height] in px, below which the tiler will not shrink it
//   load       a dynamic import, so a tool's code arrives when first opened
//              rather than all of it at boot

export const ANY_CORPUS = "*";

/** The games the app knows. A corpus outside this offers no tools at all —
    the value reaches us from stored preferences, and a half-populated shell
    is worse than an obviously empty one. */
export const CORPORA = Object.freeze(["mtg", "dnd"]);
export const isCorpus = (c) => CORPORA.includes(c);

export const TOOLS = Object.freeze({
	/* ---- both games ---- */
	chat: {
		title: "Chat", icon: "scroll", corpus: ANY_CORPUS, accel: "c",
		instances: "single", min: [380, 300],
		blurb: "Ask the sage, grounded in the rules",
		load: () => import("../chat.js"),
	},
	study: {
		title: "Study", icon: "card", corpus: ANY_CORPUS, accel: "s",
		instances: "single", min: [380, 360],
		blurb: "Spaced repetition over the corpus",
		load: () => import("../study.js"),
	},
	reader: {
		title: "Read", icon: "openBook", corpus: ANY_CORPUS, accel: "r",
		instances: "single", min: [420, 360],
		blurb: "The rule sets as books",
		load: () => import("../reader.js"),
	},

	/* ---- D&D ---- */
	planner: {
		title: "Planner", icon: "scrollOpen", corpus: "dnd", accel: "p",
		instances: "single", min: [520, 360],
		blurb: "The narrative spine: acts, scenes, cast, secrets",
		load: () => import("../planner.js"),
	},
	campaign: {
		title: "Campaign", icon: "map", corpus: "dnd", accel: "m",
		instances: "single", min: [480, 360],
		blurb: "The world graph: entities, facts, timeline",
		load: () => import("../campaign.js"),
	},
	cchat: {
		title: "Ask campaign", icon: "runestone", corpus: "dnd", accel: "a",
		instances: "single", min: [380, 320],
		blurb: "The DM and player Grimoires",
		load: () => import("../campaignchat.js"),
	},
	sessions: {
		title: "Sessions", icon: "hourglass", corpus: "dnd", accel: "g",
		instances: "single", min: [480, 360],
		blurb: "What was actually played, with citable spans",
		load: () => import("../sessions.js"),
	},
	review: {
		title: "Review", icon: "shield", corpus: "dnd", accel: "v",
		instances: "single", min: [460, 360],
		blurb: "The canon queue: proposals become truth here",
		load: () => import("../review.js"),
	},
	encounter: {
		title: "Encounter", icon: "dice", corpus: "dnd", accel: "e",
		instances: "single", min: [460, 380],
		blurb: "Budget, roster and statblocks",
		load: () => import("../encounter.js"),
	},

	/* ---- Magic ---- */
	deck: {
		title: "Deck", icon: "chest", corpus: "mtg", accel: "d",
		instances: "single", min: [480, 380],
		blurb: "Commander brewing, grounded in real card text",
		load: () => import("../deck.js"),
	},

	// Table Play (docs/table/model.md, ADRs 9-13) lands here as more entries
	// with corpus: "mtg". Nothing outside this object changes.
});

/** Ids in a stable order — the rail, the command menu and Ctrl+G all use it. */
export const TOOL_IDS = Object.freeze(Object.keys(TOOLS));

export const isTool = (id) => Object.hasOwn(TOOLS, id);

/** A tool's definition, or null. Never throws: ids arrive from stored data. */
export const toolDef = (id) => (isTool(id) ? TOOLS[id] : null);

/** Does this tool belong to the given game? */
export function inCorpus(id, corpus) {
	const def = toolDef(id);
	if (!def || !isCorpus(corpus)) return false;
	return def.corpus === ANY_CORPUS || def.corpus === corpus;
}

/**
 * The tools one game offers, in registry order.
 *
 * This is the whole of corpus separation. It replaced closeForeignSurfaces()
 * — a hand-maintained list of DOM ids to close when the game changed — and the
 * html[data-corpus] display:none rules that hid the other game's rail buttons.
 * Both were lists that had to be edited per tool; this is derived.
 */
export const toolsFor = (corpus) => TOOL_IDS.filter((id) => inCorpus(id, corpus));

/** Passed to tree.parse so a layout naming a retired tool drops that leaf. */
export const knownTool = (id) => isTool(id);

// On `instances`
// -----------------------------------------------------------------
// Every tool is "single" today: its markup already exists in index.html as a
// <section>, and mount() moves that section into the window body. That is what
// made migrating nine surfaces tractable — the thousands of $("planner-board")
// lookups inside each module keep working untouched.
//
// The cost is that one tool cannot have two windows: no two Campaign windows
// on two different campaigns. Lifting it means authoring markup as a <template>
// and scoping those lookups to the window root. The field is here so a tool
// can opt in without a second migration, and new tools should be written that
// way from the start.
