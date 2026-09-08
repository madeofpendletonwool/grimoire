// The encounter director panel's view model: the DOM-free half of
// director.js, here so node --test can run it (ADR 14's rule for jstest/ —
// no DOM, no fetch, just the derivations the panel paints from).
//
// The heart of this file is the render gate. The server's citation gate
// (internal/director, MAD-427) already drops any suggestion that cites
// nothing — but the render boundary enforces the same rule again: a
// suggestion that reaches the panel without basis cannot become a row,
// whatever the wire said. The gate is the acceptance criterion, spelled
// as code and tested in jstest/directorvm.test.js.

/** One citation shaped for the row that shows it: the id badge, the kind
    tint (statblock text or live state), the source line and the full
    text — kept whole, never truncated or collapsed away. A citation
    without text is not a citation. */
export function citationLines(basis) {
	const out = [];
	for (const b of basis || []) {
		if (!b) continue;
		const text = String(b.text || "").trim();
		if (!text) continue;
		out.push({
			id: String(b.id || "").trim(),
			kind: b.kind === "statblock" ? "statblock" : "state",
			source: String(b.source || "").trim(),
			text,
		});
	}
	return out;
}

/** The render gate: the suggestions that may paint, each with its
    citations resolved onto it. A suggestion whose basis is missing,
    empty or blank cannot render — it is dropped here exactly as the
    server's gate dropped it upstream. */
export function suggestionRows(body) {
	const rows = [];
	for (const s of (body && body.suggestions) || []) {
		if (!s) continue;
		const citations = citationLines(s.basis);
		if (!citations.length) continue;
		rows.push({
			actor: String(s.actor || "").trim(),
			action: String(s.action || "").trim(),
			reasoning: String(s.reasoning || "").trim(),
			citations,
		});
	}
	return rows;
}

/** The honest tail: how many suggestions the gate caught, what the
    grounding could not read (its caveats), and which model did the
    talking — shaped for the strip that renders them, not hides them. */
export function transparency(body) {
	const b = body || {};
	const dropped = Number.isFinite(b.dropped) ? Math.max(0, Math.floor(b.dropped)) : 0;
	return {
		dropped,
		droppedNote: dropped > 0
			? `${dropped} suggestion${dropped === 1 ? "" : "s"} dropped for want of a citable basis`
			: "",
		caveats: (Array.isArray(b.caveats) ? b.caveats : []).map(String).map((c) => c.trim()).filter(Boolean),
		model: String(b.model || "").trim(),
	};
}

/** The battle line the advice rode with: the fight's name, the round,
    whose move it is — the frame the suggestions read against. An absent
    battle is an empty string, never a invented one. */
export function combatLine(combat) {
	if (!combat) return "";
	const parts = [];
	const name = String(combat.name || "").trim();
	if (name) parts.push(name);
	if (Number.isFinite(combat.round) && combat.round > 0) parts.push(`round ${combat.round}`);
	const turn = String(combat.turn || "").trim();
	if (turn) parts.push(`${turn} to act`);
	return parts.join(" · ");
}
