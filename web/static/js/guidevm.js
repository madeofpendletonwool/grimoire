// The Guide's view model: the DOM-free half of guide.js (ADR 14's rule for
// jstest/ — no DOM, no fetch, just the derivations the cards paint from).
//
// Two things live here and nowhere else.
//
// The first is the track table: what a DM and a player are each walked
// through, in order. It is data rather than a sequence of screens because
// the app already states this workflow once — wm/presets.js seeds `Prep`,
// `At the table`, `Canon` for the DM and `The table`, `The world` for a
// player — and a second, prose copy of it in a tour script would drift from
// the first the moment a preset changed.
//
// The second is the gate. Every step carries `done(snap)`, a pure predicate
// over one server snapshot, and that is the *only* way a step completes.
// Nothing here counts clicks or remembers that a card was dismissed: an
// onboarding that ticks itself off when you press Next teaches the Next
// button. Completing the real act in the real tool is the acceptance, which
// also means a DM who founded their campaign before ever opening the Guide
// finds that step already behind them.

import { SEAT_DM, SEAT_PLAYER } from "./wm/registry.js";

export const TRACK_DM = "dm";
export const TRACK_PLAYER = "player";

/** The pref key the fork records its answer under (uistate merges, so this
    cannot clobber `corpus`). */
export const TRACK_PREF = "guide.track";

/* ---------- the snapshot ---------- */

/**
 * Normalise one /api/onboarding/state body into the shape the predicates
 * read.
 *
 * Defensive on purpose: this payload arrives over the wire, and a step
 * predicate that throws would take the whole Guide down with it — the one
 * surface a confused user is most likely to have open. An absent field is
 * zero, never undefined, so `>` comparisons stay meaningful.
 */
export function snapshot(body) {
	const b = body || {};
	const count = (v) => (Number.isFinite(v) && v > 0 ? Math.floor(v) : 0);
	return {
		campaigns: count(b.campaigns),
		role: String(b.role || "").trim(),
		hasSpine: !!b.has_spine,
		decided: count(b.decided),
		pending: count(b.pending),
		encounters: count(b.encounters),
		members: count(b.members),
		invites: count(b.invites),
		sessions: count(b.sessions),
		rolls: count(b.rolls),
		hasCharacter: !!b.has_character,
		journalEntries: count(b.journal_entries),
	};
}

/** The snapshot an account has before anything has been read. */
export const EMPTY = snapshot(null);

/* ---------- the tracks ---------- */

// `show` is what the step's "Show me" does: open a tool, or switch to a
// workspace slot. It never points *inside* another tool — openTool resolves
// its module on a later tick and there is no mounted event to wait on, so a
// coach mark aimed at another tool's innards would fire at empty space.

const DM_STEPS = [
	{
		id: "found",
		title: "Found the table",
		blurb: "A campaign is the container for everything else — the world, the party, the sessions. Name one, or hand the skeleton generator a premise and let it draft the spine for you.",
		done: (s) => s.campaigns > 0,
		show: { tool: "campaign" },
	},
	{
		id: "canon",
		title: "Decide what's true",
		blurb: "Whatever the generators produced is waiting in Review as proposals. Read a batch and accept the parts you want.",
		// The single most load-bearing idea in the app, and the one that makes
		// it look broken when nobody says it out loud: a DM who cannot find
		// the NPCs the skeleton "made" has not lost them, they simply have
		// not been decided yet.
		teaches: "Nothing a model writes is canon until you accept it. Proposals are invisible to players — and to the rest of the app — until they pass this gate.",
		done: (s) => s.decided > 0,
		show: { tool: "review" },
	},
	{
		id: "prep",
		title: "Lay out the spine",
		blurb: "Acts, scenes, the cast that walks through them and the secrets they carry. This is the shape of the campaign, not a script — the Planner is happy with three lines per scene.",
		done: (s) => s.hasSpine,
		show: { tool: "planner" },
	},
	{
		id: "encounter",
		title: "Build an encounter",
		blurb: "Pick a roster and the builder checks it against your party's real level and size, so you know what you are pointing at them before they are in it.",
		done: (s) => s.encounters > 0,
		show: { tool: "encounter" },
	},
	{
		id: "party",
		title: "Invite the party",
		blurb: "Generate a join code and send it to your players. Bind each of them to a character and the dice, the board and their sheet all know who they are.",
		done: (s) => s.members > 0 || s.invites > 0,
		show: { tool: "campaign" },
	},
	{
		id: "play",
		title: "Run it",
		blurb: "Start a session and the play workspace has everything at once: the scene and clock on the left, the combat tracker in the middle, the party and the dice beside them.",
		// The workspace idea is taught by using one, not by describing one.
		// No key is named here on purpose: this module stays DOM-free for
		// node --test, so it cannot ask keys.js whether the leader prints as
		// ⌘ or Ctrl — and a hard-coded "Ctrl" is simply wrong on a Mac. The
		// tour names keys; this names the idea.
		teaches: "The tabs along the top are arrangements of the same app, not modes. Switching to the play workspace leaves your prep layout exactly where you left it.",
		done: (s) => s.sessions > 0,
		show: { slot: 2 },
	},
];

const PLAYER_STEPS = [
	{
		id: "seated",
		title: "Take your seat",
		blurb: "You are in someone's campaign. What you can see is what the party has met and been told — the DM's notes, drafts and secrets are not merely hidden from the page, they never reach your browser.",
		done: (s) => s.campaigns > 0,
		show: { tool: "campaign" },
	},
	{
		id: "character",
		title: "Your character",
		blurb: "Your sheet is yours to edit — inventory and notes always, and the mechanics too if your DM has opened that up. The dice window's quick rolls are already bound to whatever is on it.",
		done: (s) => s.hasCharacter,
		show: { tool: "campaign" },
		skipFor: ["observer"],   // an observer has no binding, by design (ADR 22)
	},
	{
		id: "roll",
		title: "At the table",
		blurb: "The board is the party live — hit points, conditions, who is actually here. Roll from the dice window and the whole table sees it land.",
		done: (s) => s.rolls > 0,
		show: { tool: "dice" },
	},
	{
		id: "learned",
		title: "What the party knows",
		blurb: "Ask the campaign anything, then write down what you think it means. Your journal is the party's account of the session, and the DM reads it.",
		// Both halves of the epistemics, in the words a player needs. Without
		// this card the Grimoire looks like an AI that lies to you.
		teaches: "The campaign Grimoire answers as what the party has learned — not as what is true. If it says a thing you know to be wrong, that is your character being wrong, not the app. Your journal is recorded the same way: as a belief you hold, never as a fact about the world.",
		done: (s) => s.journalEntries > 0,
		show: { tool: "cchat" },
		skipFor: ["observer"],   // no character, no journal
	},
];

export const TRACKS = Object.freeze({
	[TRACK_DM]: Object.freeze(DM_STEPS),
	[TRACK_PLAYER]: Object.freeze(PLAYER_STEPS),
});

export const isTrack = (t) => t === TRACK_DM || t === TRACK_PLAYER;

/**
 * The steps a track offers this standing, in order.
 *
 * An observer is the player seat deliberately without a self (ADR 22): no
 * binding, no sheet, no journal. Offering them a step the server will never
 * let them finish would leave the track permanently stuck one short.
 */
export function stepsFor(track, role = "") {
	const steps = TRACKS[track];
	if (!steps) return [];
	const standing = String(role || "").trim();
	return steps.filter((step) => !(step.skipFor || []).includes(standing));
}

/* ---------- which track ---------- */

/**
 * The track to walk, given the shell's seat, the fork's recorded answer and
 * the account's real campaign standings.
 *
 * Real standing outranks the pref in both directions: someone who answered
 * "I'm joining a table" and then founded one is a DM now, and the pref must
 * not keep handing them a track that cannot advance. The pref only breaks
 * the tie the server cannot — an account with no campaigns at all, where
 * the seat resolves to DM for want of any evidence (seat.js, ADR 22) and
 * "waiting on a join code" is indistinguishable from "about to create one".
 */
export function trackFor(seat, pref, campaigns) {
	const list = Array.isArray(campaigns) ? campaigns : [];
	const dmSomewhere = list.some((c) => c && (c.my_role === "dm" || c.my_role === "keeper"));
	if (dmSomewhere) return TRACK_DM;
	if (list.length > 0) return TRACK_PLAYER;
	if (isTrack(pref)) return pref;
	return seat === SEAT_PLAYER ? TRACK_PLAYER : TRACK_DM;
}

/**
 * Does this account still need the fork?
 *
 * Only an account with no campaigns and no recorded answer: everyone else
 * has already told us what they are, either by founding a table or by
 * joining one. A failed read is not a verdict — an unauthenticated or
 * unreadable state means no fork, because the alternative is greeting a
 * returning DM with "are you new?" every time /api/campaigns times out.
 */
export function needsFork(auth, campaigns, prefs) {
	if (!auth || auth.authenticated === false) return false;
	if (!Array.isArray(campaigns) || campaigns.length > 0) return false;
	return !isTrack((prefs || {})[TRACK_PREF]);
}

/* ---------- progress ---------- */

/**
 * Fold a snapshot over a track: which steps are behind you, which one is
 * next, and how far along the whole thing is.
 *
 * Steps are judged independently rather than as a chain. Order is the order
 * we *suggest*, not a lock — a DM who built an encounter before touching the
 * Planner has genuinely built an encounter, and telling them otherwise to
 * protect the sequence would be the app lying about its own state.
 */
export function progress(steps, snap) {
	const list = Array.isArray(steps) ? steps : [];
	const s = snap || EMPTY;
	const rows = list.map((step) => {
		let done = false;
		// A predicate that throws must cost one checkmark, not the panel.
		try {
			done = !!step.done(s);
		} catch (_) {
			done = false;
		}
		return { id: step.id, step, done };
	});
	const doneCount = rows.filter((r) => r.done).length;
	return {
		rows,
		done: rows.filter((r) => r.done).map((r) => r.id),
		next: rows.find((r) => !r.done)?.step || null,
		complete: list.length > 0 && doneCount === list.length,
		total: list.length,
		count: doneCount,
		pct: list.length ? Math.round((doneCount / list.length) * 100) : 0,
	};
}

/** The line under the title: where you are, in words rather than a bar. */
export function progressLine(p) {
	if (!p || !p.total) return "";
	if (p.complete) return "Every step done — the loop is yours now.";
	return `${p.count} of ${p.total} done`;
}

export { SEAT_DM, SEAT_PLAYER };
