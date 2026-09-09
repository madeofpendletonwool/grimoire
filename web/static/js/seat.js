// The campaign seat (ADR 22): which Grimoire this account is shown.
//
// Identity and membership are server facts (ADR 4): a member's reads arrive
// already scoped, so the seat adds no security — the server refuses every
// DM write a player attempts. What it decides is the *shape of the shell*:
// a member whose campaigns never resolve to the DM perspective gets a
// player-shaped rail, command menu, cheat sheet and workspace presets, so
// the app stops being a DM cockpit wearing a player costume.
//
// The seat is computed once at boot from two reads the shell already makes:
// /api/auth/state (the keeper flag) and /api/campaigns (each row carries
// my_role). A DM anywhere keeps the DM shell — the DM tools' own campaign
// pickers already narrow to the campaigns they run. A member-only account
// (player and/or observer rows, nothing else) gets the player seat.
//
// The observer experience is the player seat by design: watch the board,
// the quest journal and the party-scope Grimoire chat. An observer has no
// character binding, so nothing character-shaped reaches them — no sheet
// (the server refuses any bound character), no character-scoped chat, no
// quick-roll chips. They may still roll public as the table.
//
// Failure keeps the DM seat: a failed read is no verdict, and the DM shell
// is what every existing account already had.

import { api } from "./api.js";
import { SEAT_DM, SEAT_PLAYER } from "./wm/registry.js";

const state = {
	role: SEAT_DM,
	campaigns: [],
};

/** The seat this shell is shaped as: "dm" or "player". */
export const seatRole = () => state.role;

/** The campaigns the account belongs to, as the picker sees them. */
export const seatCampaigns = () => state.campaigns.slice();

const isDMStanding = (c) => c && (c.my_role === "dm" || c.my_role === "keeper");

/**
 * The seat an account's standings resolve to — pure, so the shape of the
 * decision is testable without a server.
 *
 * The keeper may always look (their access is resolveCampaignAccess's), and
 * an account with no campaigns yet is on the DM's front door: the create and
 * join forms. Only a member of campaigns who never resolves to the DM
 * perspective lands in the player seat.
 */
export function seatRoleOf(auth, campaigns) {
	if (auth && auth.is_admin) return SEAT_DM;
	const list = Array.isArray(campaigns) ? campaigns : [];
	if (list.length === 0) return SEAT_DM;
	if (list.some(isDMStanding)) return SEAT_DM;
	return SEAT_PLAYER;
}

/**
 * Resolve the seat at boot. Both reads run together, and either failing —
 * including the fetch itself throwing before a promise exists — keeps the
 * DM seat (a login lap or a campaigns-less install is not a verdict).
 */
export async function loadSeat() {
	const [auth, campaigns] = await Promise.all([
		readSafe(api.authState),
		readSafe(api.campaignList),
	]);
	state.campaigns = campaigns?.campaigns || [];
	state.role = seatRoleOf(auth, state.campaigns);
	return state.role;
}

async function readSafe(load) {
	try {
		return await load();
	} catch (_) {
		return null;
	}
}
