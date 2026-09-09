// The handout view-model (MAD-490): the pure half of the Handouts tool —
// everything about the surface that needs no DOM and no server, split out
// so node --test can hold it (ADR 14). The module half (handouts.js)
// paints what this decides.
//
// The DM never sees the player's list and the player never sees the DM's
// desk — the server decides whose rows arrive — but both seats read the
// same cards, so the shaping here is small: ordering, grouping, and which
// lifecycle actions apply to a row at a seat.

/** The lifecycle a handout row is in, as a person says it. */
export const STATUS_LABELS = Object.freeze({
	draft: "draft — yours alone",
	published: "in the party's hands",
	retired: "retired",
});

/** The actions one seat may take on one handout. Players get none; a
    retired row keeps its history and offers nothing; drafts publish;
    published rows can be taken back (unpublish) or closed (retire). */
export function handoutActions(handout, isDM) {
	if (!isDM) return [];
	switch (handout.status) {
		case "draft":
			return ["publish", "retire"];
		case "published":
			return ["unpublish", "retire"];
		default:
			return [];
	}
}

/** Order the cards the way the table meets them: maps and letters
    interleaved, most recently handed out first, drafts (DM desks only)
    pinned above the published so the unfinished work is never buried.
    Ties break by id so the order is stable across renders. */
export function orderHandouts(handouts) {
	const rank = (h) => (h.status === "draft" ? 0 : 1);
	const stamp = (h) => Date.parse(h.published_at || h.created_at) || 0;
	return [...handouts].sort((a, b) =>
		rank(a) - rank(b) ||
		stamp(b) - stamp(a) ||
		(a.id < b.id ? -1 : a.id > b.id ? 1 : 0));
}

/** Split one ordered list into the sections the tool renders: the maps
    first (a map unrolls on the table before the letters go round), then
    the reading material. Empty sections are omitted so a party holding
    only letters sees one heading. */
export function groupHandouts(handouts) {
	const ordered = orderHandouts(handouts);
	const groups = [];
	for (const kind of ["map", "handout"]) {
		const rows = ordered.filter((h) => h.kind === kind);
		if (rows.length) groups.push({ kind, rows });
	}
	return groups;
}
