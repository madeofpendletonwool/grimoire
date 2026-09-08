// The combat tracker's view model: the DOM-free half of combat.js, kept
// here so node --test can run it (ADR 14's rule for jstest/ — no DOM, no
// fetch, just the derivations the screen paints from).
//
// Everything here shapes server truth for display; nothing recomputes
// engine semantics. The turn response already carries the prompts — these
// functions only spell them.

/** The fifteen conditions, for an install whose effects vocabulary route
    answers 503. Same words the engine enforces. */
export const CONDITIONS = Object.freeze([
	"blinded", "charmed", "deafened", "exhaustion", "frightened", "grappled",
	"incapacitated", "invisible", "paralyzed", "petrified", "poisoned",
	"prone", "restrained", "stunned", "unconscious",
]);

/** The damage types the one-tap box offers. Free text still travels —
    the engine matches against the snapshot's own lists. */
export const DAMAGE_TYPES = Object.freeze([
	"acid", "bludgeoning", "cold", "fire", "force", "lightning", "necrotic",
	"piercing", "poison", "psychic", "radiant", "slashing", "thunder",
]);

/**
 * The prompt surface: one row per thing the last engine step asked of the
 * table. A next-turn response carries lair reminders, recharge rolls,
 * death saves and what the round wore away; a damage response carries
 * concentration checks. Both flatten through here into display rows
 * {kind, text} — kind drives the row's tone, text is the DM's reading.
 */
export function promptRows(step) {
	const rows = [];
	if (!step) return rows;
	if (step.lair_reminder) {
		rows.push({ kind: "lair", text: "Lair action — initiative count 20, losing ties." });
	}
	for (const p of step.prompts || []) {
		if (p.kind === "recharge") {
			rows.push({ kind: "recharge", text: `${p.name}: roll ${p.usage || "its recharge"}.` });
		} else if (p.kind === "death_save") {
			rows.push({ kind: "death", text: `${p.name} is dying — death save. (${p.detail || "0 successes, 0 failures"})` });
		} else {
			rows.push({ kind: p.kind || "note", text: p.detail || p.name || "" });
		}
	}
	for (const c of step.expired_conditions || []) {
		rows.push({ kind: "expired", text: `${c.name || "a condition"} wore off.` });
	}
	for (const e of step.expired_effects || []) {
		rows.push({ kind: "expired", text: `${e.target_name || "someone"}: ${e.name || "an effect"} ended — duration up.` });
	}
	return rows.filter((r) => r.text);
}

/** Concentration checks ride the damage response, not the turn — their
    own rows so the hit that prompted them can show both. */
export function concentrationRows(checks) {
	const rows = [];
	for (const c of checks || []) {
		rows.push({ kind: "concentration", text: `${c.spell || "concentration"}: DC ${c.dc} Constitution save or lose it.` });
	}
	return rows;
}

/**
 * The lineup roster: monster lines merged by statblock name, the way the
 * encounter builder's roster works. addLine bumps a count in place or
 * appends; stepLine adjusts or drops; the payload spells StartInput.
 */
export function addLine(lines, name, count = 1) {
	const found = lines.find((l) => l.name === name);
	if (found) found.count += count;
	else lines.push({ name, count });
	return lines;
}

export function stepLine(lines, i, delta) {
	const line = lines[i];
	if (!line) return lines;
	line.count += delta;
	if (line.count < 1) lines.splice(i, 1);
	return lines;
}

/** StartInput's body, spelled for POST /combat. companions carry their
    own name field; monsters are pure statblock lines. */
export function startPayload({ name, encounterID, sessionID, pcs, monsters, companions }) {
	return {
		name: (name || "").trim(),
		encounter_id: encounterID || "",
		session_id: sessionID || "",
		pcs: pcs || [],
		monsters: (monsters || []).map((m) => ({ name: m.name, count: m.count })),
		companions: (companions || []).map((c) => ({ statblock: c.name, name: c.companionName || "", count: c.count })),
	};
}

/** True when the lineup has anyone at all — the start button's gate. */
export function lineupCount(pcs, monsters, companions) {
	return (pcs ? pcs.length : 0) +
		(monsters || []).reduce((n, m) => n + (m.count > 0 ? m.count : 0), 0) +
		(companions || []).reduce((n, c) => n + (c.count > 0 ? c.count : 0), 0);
}

/** The health word for a foe the room reads as a word (the table's own
    grammar: the DM picks the reveal, the word follows the fraction). */
export function healthWord(hp, max) {
	if (hp <= 0) return "down";
	if (max > 0 && hp * 2 <= max) return "bloodied";
	return "hale";
}

/** The turn label: whose move it is, with the parked-before-the-first
    turn position spelled as the fight's opening. */
export function turnLabel(combat, order) {
	if (!combat) return "";
	if (combat.turn_index < 0 || !order || !order.length) return "the fight begins";
	const c = order.find((x) => x.position === combat.turn_index) || order[combat.turn_index];
	return c ? `${c.name}'s move` : "";
}
