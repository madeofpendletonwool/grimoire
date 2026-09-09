// The seat (ADR 22): which Grimoire an account is shown. seatRoleOf is pure
// — every input it reads is a payload the shell already receives — so the
// whole shape of the decision is testable without a server (ADR 14).

import test from "node:test";
import assert from "node:assert/strict";

import { seatRoleOf, seatRole, loadSeat } from "../web/static/js/seat.js";
import { SEAT_DM, SEAT_PLAYER } from "../web/static/js/wm/registry.js";

const camp = (my_role) => ({ id: "c", name: "A campaign", my_role });

test("an account with no campaigns sits at the DM's front door", () => {
	assert.equal(seatRoleOf(null, []), SEAT_DM);
	assert.equal(seatRoleOf({ username: "x" }, []), SEAT_DM);
	assert.equal(seatRoleOf({ username: "x" }, null), SEAT_DM);
});

test("a DM anywhere keeps the DM shell", () => {
	assert.equal(seatRoleOf(null, [camp("player"), camp("dm")]), SEAT_DM);
	assert.equal(seatRoleOf(null, [camp("dm"), camp("observer")]), SEAT_DM);
	assert.equal(seatRoleOf(null, [camp("keeper")]), SEAT_DM);
});

test("a member-only account gets the player seat", () => {
	assert.equal(seatRoleOf(null, [camp("player")]), SEAT_PLAYER);
	assert.equal(seatRoleOf(null, [camp("observer")]), SEAT_PLAYER);
	assert.equal(seatRoleOf(null, [camp("player"), camp("observer")]), SEAT_PLAYER);
});

test("the keeper may always look, whatever their member rows say", () => {
	assert.equal(seatRoleOf({ is_admin: true }, [camp("observer")]), SEAT_DM);
	assert.equal(seatRoleOf({ is_admin: true }, [camp("player"), camp("player")]), SEAT_DM);
});

test("a blank role is not a DM standing", () => {
	// A member row the list could not resolve carries no my_role; it must
	// not fall through to the DM seat by accident.
	assert.equal(seatRoleOf(null, [camp("")]), SEAT_PLAYER);
	assert.equal(seatRoleOf(null, [camp(undefined)]), SEAT_PLAYER);
});

test("the shell starts at the DM seat and loadSeat's failures keep it there", async () => {
	assert.equal(seatRole(), SEAT_DM);
	// api fetches throw here (no server, ADR 14's DOM-free rule): both
	// reads fail, and a failed read is no verdict.
	const role = await loadSeat();
	assert.equal(role, SEAT_DM);
	assert.equal(seatRole(), SEAT_DM);
});
