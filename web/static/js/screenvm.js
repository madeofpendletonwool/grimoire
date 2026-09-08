// The DM screen's view model: the DOM-free half of screen.js, here so
// node --test can run it (ADR 14's rule for jstest/ — no DOM, no fetch,
// just the derivations the screen paints from).
//
// Everything here shapes the live-context read (MAD-485) for display:
// which scene the card shows, what the clock says, which chips are on
// stage. Nothing recomputes state — the server already decided what the
// DM may see, and the derivations only spell it.

/** The scene kinds, spelled the way the spine's own vocabulary runs. */
export const KINDS = Object.freeze({
	social: "social",
	exploration: "exploration",
	combat: "combat",
	revelation: "revelation",
	downtime: "downtime",
	travel: "travel",
});

/** The cast roles, in the order the card offers them. */
export const ROLES = Object.freeze(["focus", "present", "offstage", "mentioned"]);

/**
 * The scene the card shows. An explicit DM pick wins (they tapped another
 * active scene to read it); then the scene seated in the live session —
 * "the scene for the live session" is the one the planner bound to it;
 * then the first active scene, because a scene can run without a seat.
 */
export function currentScene(scenes, sessionID, pickedID = "") {
	const list = scenes || [];
	if (pickedID) {
		const pick = list.find((sc) => sc.id === pickedID);
		if (pick) return pick;
	}
	if (sessionID) {
		const seated = list.find((sc) => sc.session_id === sessionID);
		if (seated) return seated;
	}
	return list[0] || null;
}

/**
 * The card's sub-line: kind and setting, joined the way the card reads
 * them. An empty scene (nothing active) is an empty string, not "unknown".
 */
export function sceneMeta(scene, settingName = "") {
	if (!scene) return "";
	const parts = [KINDS[scene.kind] || scene.kind];
	if (settingName) parts.push(`at ${settingName}`);
	return parts.join(" · ");
}

/**
 * The chips on stage: the scene's cast resolved to names through the
 * campaign's entities, focus first (they are the scene), then the spine's
 * role order. An unknown entity falls back to a short id, never a blank
 * chip the DM cannot act on.
 */
export function castChips(scene, entities = []) {
	if (!scene) return [];
	const byID = new Map(entities.map((e) => [e.id, e]));
	const rank = (role) => Math.max(0, ROLES.indexOf(role));
	return [...(scene.cast || [])]
		.sort((a, b) => rank(a.role) - rank(b.role) || a.entity_id.localeCompare(b.entity_id))
		.map((c) => {
			const ent = byID.get(c.entity_id);
			return {
				entity_id: c.entity_id,
				name: (ent && ent.name) || c.entity_id.slice(0, 8),
				kind: (ent && ent.kind) || "",
				role: c.role,
			};
		});
}

/**
 * The clock: elapsed play time as H:MM:SS over the session's started_at.
 * A missing or unparsable stamp is the empty string — the screen says
 * "not live" plainly rather than counting from zero without permission.
 * Clock skew (client behind the server) clamps at zero.
 */
export function elapsedLabel(startedAt, now = Date.now()) {
	if (!startedAt) return "";
	const start = Date.parse(startedAt);
	if (!Number.isFinite(start)) return "";
	const secs = Math.max(0, Math.floor((now - start) / 1000));
	const h = Math.floor(secs / 3600);
	const m = Math.floor((secs % 3600) / 60);
	const s = secs % 60;
	const mm = String(m).padStart(2, "0");
	const ss = String(s).padStart(2, "0");
	return h > 0 ? `${h}:${mm}:${ss}` : `${m}:${ss}`;
}

/**
 * The parking lot: the session's note events as display rows, newest
 * first — the mid-play "don't forget" list the scratch pad keeps.
 */
export function noteLines(events, limit = 8) {
	const notes = (events || [])
		.filter((ev) => ev.kind === "note")
		.map((ev) => ({
			id: ev.id,
			text: ev.summary || ev.detail || "",
			at: ev.created_at || "",
		}))
		.filter((n) => n.text);
	return notes.reverse().slice(0, limit);
}
