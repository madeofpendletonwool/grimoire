// HTTP surface for the Grimoire API.

// A 401 means the session lapsed while the app was open. Reloading lands on
// the gate, which is more useful than an error banner on a dead app.
function gateOn401(res) {
	if (res.status === 401) window.location.assign("/");
	return res;
}

async function json(res) {
	if (!res.ok) {
		gateOn401(res);
		let detail = res.statusText;
		try {
			const body = await res.json();
			if (body && body.error) detail = body.error;
		} catch (_) { /* keep the status text */ }
		throw new Error(detail);
	}
	return res.status === 204 ? null : res.json();
}

export const api = {
	meta: () => fetch("/api/meta").then(json),

	authState: () => fetch("/api/auth/state").then(json),

	logout: () => fetch("/api/auth/logout", { method: "POST" }).then(json),

	// Sign up from an admin's invite link (the signed-out gate). Self-service
	// creation stays off; an invite code is the only way in past the first user.
	register: (username, password, invite) =>
		fetch("/api/auth/register", {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({ username, password, invite }),
		}).then(json),

	// Invite management — admin only. The server returns each invite's raw code
	// exactly once, at creation; the list never carries it.
	createInvite: (note) =>
		fetch("/api/invites", {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify(note ? { note } : {}),
		}).then(json),

	listInvites: () => fetch("/api/invites").then(json),

	revokeInvite: (id) =>
		fetch(`/api/invites/${encodeURIComponent(id)}`, { method: "DELETE" }).then(json),

	search: (corpus, q, limit = 20, signal) =>
		fetch(`/api/search?corpus=${encodeURIComponent(corpus)}&q=${encodeURIComponent(q)}&limit=${limit}`,
			{ signal }).then(json),

	section: (corpus, number) =>
		fetch(`/api/section?corpus=${encodeURIComponent(corpus)}&number=${encodeURIComponent(number)}`).then(json),

	card: (q) => fetch(`/api/card?q=${encodeURIComponent(q)}`).then(json),

	cardSearch: (q, limit = 8, signal) =>
		fetch(`/api/card/search?q=${encodeURIComponent(q)}&limit=${limit}`, { signal }).then(json),

	listChats: () => fetch("/api/chats").then(json),

	createChat: (corpus) =>
		fetch("/api/chats", {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({ corpus }),
		}).then(json),

	getChat: (id) => fetch(`/api/chats/${encodeURIComponent(id)}`).then(json),

	renameChat: (id, title) =>
		fetch(`/api/chats/${encodeURIComponent(id)}`, {
			method: "PATCH",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({ title }),
		}).then(json),

	deleteChat: (id) =>
		fetch(`/api/chats/${encodeURIComponent(id)}`, { method: "DELETE" }).then(json),

	// Shared answer links — a snapshot of one Q&A under an unguessable token,
	// readable by anyone with the link until revoked.
	shareMessage: (chatID, messageID) =>
		fetch(`/api/chats/${encodeURIComponent(chatID)}/messages/${encodeURIComponent(messageID)}/share`, {
			method: "POST",
		}).then(json),

	listShares: () => fetch("/api/shares").then(json),

	revokeShare: (token) =>
		fetch(`/api/shares/${encodeURIComponent(token)}`, { method: "DELETE" }).then(json),

	studyQueue: (corpus, limit = 20, signal, topic = "") =>
		fetch(`/api/study/queue?corpus=${encodeURIComponent(corpus)}&limit=${limit}` +
			(topic ? `&topic=${encodeURIComponent(topic)}` : ""), { signal }).then(json),

	// Reading surface — the book-shaped view of the corpora.
	readerGuides: (corpus, signal) =>
		fetch(`/api/reader/guides?corpus=${encodeURIComponent(corpus)}`, { signal }).then(json),

	readerTOC: (corpus, guide, signal) =>
		fetch(`/api/reader/toc?corpus=${encodeURIComponent(corpus)}&guide=${encodeURIComponent(guide)}`, { signal }).then(json),

	// Either guide+number for a known stop, or a bare number (a rule or record
	// reference) that the server resolves onto its page.
	readerPage: (corpus, guide, number, signal) => {
		const params = new URLSearchParams({ corpus, number });
		if (guide) params.set("guide", guide);
		return fetch(`/api/reader/page?${params}`, { signal }).then(json);
	},

	// Campaign OS interface state (MAD-366): saved workspace layouts and the
	// small preferences that used to live in localStorage. Per user, per
	// corpus — switching games swaps the whole set of workspaces.
	//
	// Saves are fire-and-forget on the caller's side: a layout that fails to
	// reach the server is a lost arrangement, never a lost window, so the
	// window manager never waits on one.
	uiLayouts: (corpus, signal) =>
		fetch(`/api/ui/layouts?corpus=${encodeURIComponent(corpus)}`, { signal }).then(json),

	uiSaveLayout: (corpus, slot, name, tree) =>
		fetch("/api/ui/layouts", {
			method: "PUT",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({ corpus, slot, name, tree }),
		}).then(json),

	uiDeleteLayout: (corpus, slot) =>
		fetch(`/api/ui/layouts/${slot}?corpus=${encodeURIComponent(corpus)}`, { method: "DELETE" }).then(json),

	uiPrefs: (signal) => fetch("/api/ui/prefs", { signal }).then(json),

	uiSavePrefs: (prefs) =>
		fetch("/api/ui/prefs", {
			method: "PUT",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({ prefs }),
		}).then(json),

	// Index rebuild — admin only. Start returns 202 and the rebuild continues
	// in the background; poll status until running turns false.
	reindexStart: () => fetch("/api/admin/reindex", { method: "POST" }).then(json),
	reindexStatus: () => fetch("/api/admin/reindex").then(json),

	studyGrade: (key, corpus, topic, grade) =>
		fetch("/api/study/grade", {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({ key, corpus, topic, grade }),
		}).then(json),

	// Encounter builder: SRD monster search, difficulty preview, saved
	// encounters. The server recomputes every verdict — the client never
	// sends one.
	encounterMonsters: (q, signal) =>
		fetch(`/api/encounter/monsters?q=${encodeURIComponent(q)}`, { signal }).then(json),

	encounterEvaluate: (party, monsters, signal) =>
		fetch("/api/encounters/evaluate", {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({ party, monsters }),
			signal,
		}).then(json),

	// The tactical analysis (MAD-381): what this roster will do and to whom,
	// always the server's arithmetic, recomputed as the roster is edited.
	// A campaign id fills the party block in (DM scope); the objective
	// carries the wave note for a survive fight.
	encounterTactics: (party, monsters, campaignId, objective, terrain, signal) =>
		fetch("/api/encounters/tactics", {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({
				party,
				monsters,
				campaign_id: campaignId || "",
				objective: objective || { kind: "defeat" },
				...(terrain ? { terrain } : {}),
			}),
			signal,
		}).then(json),

	listEncounters: () => fetch("/api/encounters").then(json),

	getEncounter: (id) => fetch(`/api/encounters/${encodeURIComponent(id)}`).then(json),

	saveEncounter: (id, name, party, monsters, notes, objective, terrain) =>
		fetch(id ? `/api/encounters/${encodeURIComponent(id)}` : "/api/encounters", {
			method: id ? "PATCH" : "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({
				name, party, monsters, notes: notes || "",
				objective: objective || { kind: "defeat" },
				...(terrain ? { terrain } : {}),
			}),
		}).then(json),

	deleteEncounter: (id) =>
		fetch(`/api/encounters/${encodeURIComponent(id)}`, { method: "DELETE" }).then(json),

	// One creature's full SRD statblock out of the mirrored bestiary, so the
	// roster can expand in place instead of sending the DM elsewhere.
	encounterStatblock: (name, signal) =>
		fetch(`/api/encounter/statblock?name=${encodeURIComponent(name)}`, { signal }).then(json),

	// What the party can afford at a target difficulty and objective. The
	// DMG tables stay on the server, the same as the verdict; an objective
	// makes the aim (and the terrain generated with it) part of the answer.
	// A campaign id is optional: with one and no party, the budget is
	// computed for the campaign's declared party block — that is what
	// prefills the builder's party boxes.
	encounterBudget: (party, difficulty, campaignId, objective, signal) =>
		fetch("/api/encounter/budget", {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({
				party, difficulty, campaign_id: campaignId || "",
				objective: objective || { kind: "defeat" },
			}),
			signal,
		}).then(json),

	// The campaign's declared party block (MAD-378): the whole sheet each pc
	// carries, the levels the encounter math runs on, and the keys that could
	// not be read. DM only.
	campaignParty: (cid) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/party`).then(json),

	// A campaign-scoped encounter: the one record a planned fight has, visible
	// to the continuity engine. Creating with no party uses the campaign's
	// declared levels.
	saveCampaignEncounter: (cid, name, party, monsters, notes, objective, terrain) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/encounters`, {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({
				name, party, monsters, notes: notes || "",
				objective: objective || { kind: "defeat" },
				...(terrain ? { terrain } : {}),
			}),
		}).then(json),

	// Deck builder: commander proposals, the streamed draft, list analysis,
	// Spellbook combos, and saved decks. Every card the draft returns was
	// verified server-side against the card database.
	deckPropose: (idea, colors) =>
		fetch("/api/deck/propose", {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({ idea, colors }),
		}).then(json),

	deckAnalyze: (decklist, commander) =>
		fetch("/api/deck/analyze", {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({ decklist, commander }),
		}).then(json),

	deckCombos: (card, signal) =>
		fetch(`/api/deck/combos?card=${encodeURIComponent(card)}`, { signal }).then(json),

	listDecks: () => fetch("/api/decks").then(json),

	getDeck: (id) => fetch(`/api/decks/${encodeURIComponent(id)}`).then(json),

	saveDeck: (id, name, commander, cards, notes) =>
		fetch(id ? `/api/decks/${encodeURIComponent(id)}` : "/api/decks", {
			method: id ? "PATCH" : "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({ name, commander, cards, notes }),
		}).then(json),

	deleteDeck: (id) =>
		fetch(`/api/decks/${encodeURIComponent(id)}`, { method: "DELETE" }).then(json),

	// The campaign world. Every read runs at the caller's resolved scope on
	// the server; writes are DM-only and the server enforces that too.
	campaignList: () => fetch("/api/campaigns").then(json),

	campaignCreate: (name, system, premise) =>
		fetch("/api/campaigns", {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({ name, system, premise }),
		}).then(json),

	campaignJoin: (code) =>
		fetch("/api/campaigns/join", {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({ code }),
		}).then(json),

	campaignEntities: (cid, kind) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/entities` +
			(kind ? `?kind=${encodeURIComponent(kind)}` : "")).then(json),

	// The typed character sheet (MAD-418): the pc's definition as data. The
	// read is the DM's or the owning player's; writes and imports are the
	// DM's. `data` is the export verbatim (an object for JSON formats, a
	// string for XML).
	characterSheet: (cid, eid) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/characters/${encodeURIComponent(eid)}/sheet`).then(json),

	characterSheetPut: (cid, eid, sheet) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/characters/${encodeURIComponent(eid)}/sheet`, {
			method: "PUT",
			headers: { "content-type": "application/json" },
			body: JSON.stringify(sheet),
		}).then(json),

	characterImport: (cid, format, data, name) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/characters/import`, {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({ format, data, name: name || "" }),
		}).then(json),

	campaignEntity: (cid, eid) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/entities/${encodeURIComponent(eid)}`).then(json),

	campaignEntityCreate: (cid, kind, name, summary) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/entities`, {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({ kind, name, summary }),
		}).then(json),

	campaignFactCreate: (cid, fact) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/facts`, {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify(fact),
		}).then(json),

	campaignFact: (cid, fid) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/facts/${encodeURIComponent(fid)}`).then(json),

	campaignFactSupersede: (cid, fid, fact) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/facts/${encodeURIComponent(fid)}/supersede`, {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify(fact),
		}).then(json),

	campaignAwarenessSet: (cid, knower, factId, stance) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/awareness`, {
			method: "PUT",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({ knower, fact_id: factId, stance }),
		}).then(json),

	campaignSearch: (cid, q) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/search?q=${encodeURIComponent(q)}`).then(json),

	campaignGraph: (cid, center, hops) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/graph?center=${encodeURIComponent(center)}&hops=${hops}`).then(json),

	// The campaign clock (MAD-365): calendar, clock+weather+strip, the
	// schedule, and travel. Reads the calendar so dates are legible to
	// players too; everything that moves time is DM-only server-side.
	campaignCalendar: (cid) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/calendar`).then(json),

	campaignCalendarPut: (cid, calendar, seed) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/calendar`, {
			method: "PUT",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({ calendar, seed }),
		}).then(json),

	campaignClock: (cid, stripDays, dueDays, location) => {
		const q = new URLSearchParams();
		if (stripDays) q.set("strip", stripDays);
		if (dueDays) q.set("due", dueDays);
		if (location) q.set("location", location);
		const qs = q.toString();
		return fetch(`/api/campaigns/${encodeURIComponent(cid)}/clock${qs ? `?${qs}` : ""}`).then(json);
	},

	campaignClockAdvance: (cid, move, reason, note) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/clock/advance`, {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({ ...move, reason, note }),
		}).then(json),

	campaignSchedule: (cid, dueDays) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/schedule` +
			(dueDays ? `?due=${dueDays}` : "")).then(json),

	campaignScheduleCreate: (cid, entry) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/schedule`, {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify(entry),
		}).then(json),

	campaignScheduleUpdate: (cid, sid, patch) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/schedule/${encodeURIComponent(sid)}`, {
			method: "PATCH",
			headers: { "content-type": "application/json" },
			body: JSON.stringify(patch),
		}).then(json),

	campaignScheduleDelete: (cid, sid) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/schedule/${encodeURIComponent(sid)}`, {
			method: "DELETE",
		}).then(json),

	campaignTravel: (cid, from, to, days) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/travel`, {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({ from, to, ...(days != null ? { days } : {}) }),
		}).then(json),

	// The simulation tick (MAD-367): preview a window of world-state
	// outcomes (nothing written but the tick's own row), then stage it as
	// one proposal batch behind the review gate. Deciding that batch — on
	// the ordinary proposals surface — moves the clock.
	campaignSimulate: (cid, days, seed) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/simulate`, {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({ days, ...(seed != null ? { seed } : {}) }),
		}).then(json),

	campaignSimulateStage: (cid, tickID) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/simulate/${encodeURIComponent(tickID)}/stage`, {
			method: "POST",
		}).then(json),

	// Journeys (MAD-375): plan a road at a density — the seeded day table
	// comes back and nothing is written but the journey's own rows — then
	// resolve days at the table and the whole road through the batch gate.
	campaignJourneys: (cid) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/journeys`).then(json),

	campaignJourneyPlan: (cid, plan) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/journeys`, {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify(plan),
		}).then(json),

	campaignJourney: (cid, jid) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/journeys/${encodeURIComponent(jid)}`).then(json),

	campaignJourneyPatch: (cid, jid, patch) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/journeys/${encodeURIComponent(jid)}`, {
			method: "PATCH",
			headers: { "content-type": "application/json" },
			body: JSON.stringify(patch),
		}).then(json),

	campaignJourneyDayResolve: (cid, jid, day, body) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/journeys/${encodeURIComponent(jid)}/days/${day}/resolve`, {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify(body || {}),
		}).then(json),

	campaignJourneyResolve: (cid, jid) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/journeys/${encodeURIComponent(jid)}/resolve`, {
			method: "POST",
		}).then(json),

	// The faction surface (MAD-366): the scope-filtered dossier and the
	// DM-only plans. A player's dossier carries the public face and aware
	// edges; plans never cross a player scope, server-side by construction.
	campaignFactions: (cid) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/factions`).then(json),

	campaignFaction: (cid, eid) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/factions/${encodeURIComponent(eid)}`).then(json),

	campaignFactionPlans: (cid, eid) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/factions/${encodeURIComponent(eid)}/plans`).then(json),

	campaignFactionPlanCreate: (cid, eid, plan) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/factions/${encodeURIComponent(eid)}/plans`, {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify(plan),
		}).then(json),

	campaignFactionPlanUpdate: (cid, pid, patch) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/plans/${encodeURIComponent(pid)}`, {
			method: "PATCH",
			headers: { "content-type": "application/json" },
			body: JSON.stringify(patch),
		}).then(json),

	campaignFactionPlanTransition: (cid, pid, to, reason) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/plans/${encodeURIComponent(pid)}/transition`, {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({ to, reason }),
		}).then(json),

	// The location surface (MAD-370): the places listing and the
	// scope-resolved dossier. The place block editor is DM-only; a player's
	// dossier carries the block's public half and none of the rest,
	// server-side by construction.
	campaignLocations: (cid) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/locations`).then(json),

	campaignLocation: (cid, eid) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/locations/${encodeURIComponent(eid)}`).then(json),

	campaignPlacePut: (cid, eid, place) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/locations/${encodeURIComponent(eid)}/place`, {
			method: "PUT",
			headers: { "content-type": "application/json" },
			body: JSON.stringify(place),
		}).then(json),

	// The rumour mill (MAD-374): statements in circulation and who
	// repeats them. Reads resolve the caller's scope server-side — a
	// player's list never carries a truth value; writes and hearing are
	// DM-only; generation stages a batch behind the review gate.
	campaignRumors: (cid, params) => {
		const q = new URLSearchParams(params || {}).toString();
		return fetch(`/api/campaigns/${encodeURIComponent(cid)}/rumors${q ? "?" + q : ""}`).then(json);
	},

	campaignRumor: (cid, rid) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/rumors/${encodeURIComponent(rid)}`).then(json),

	campaignRumorCreate: (cid, rumor) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/rumors`, {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify(rumor),
		}).then(json),

	campaignRumorUpdate: (cid, rid, patch) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/rumors/${encodeURIComponent(rid)}`, {
			method: "PATCH",
			headers: { "content-type": "application/json" },
			body: JSON.stringify(patch),
		}).then(json),

	campaignRumorDelete: (cid, rid) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/rumors/${encodeURIComponent(rid)}`, { method: "DELETE" }).then(json),

	campaignRumorHolderSet: (cid, rid, holder) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/rumors/${encodeURIComponent(rid)}/holders`, {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify(holder),
		}).then(json),

	campaignRumorHolderDelete: (cid, rid, eid) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/rumors/${encodeURIComponent(rid)}/holders/${encodeURIComponent(eid)}`, {
			method: "DELETE",
		}).then(json),

	campaignRumorHeard: (cid, rid, knower, sinceEvent) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/rumors/${encodeURIComponent(rid)}/heard`, {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({ knower, since_event: sinceEvent || "" }),
		}).then(json),

	campaignRumorGenerate: (cid, req) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/rumors/generate`, {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify(req),
		}).then(json),

	// The campaign chat (MAD-311): threads pinned to the campaign and to the
	// caller's resolved scope. The scope is decided server-side from the
	// membership row — this surface never chooses a perspective.
	campaignChatList: (cid) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/chats`).then(json),

	campaignChatCreate: (cid) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/chats`, { method: "POST" }).then(json),

	campaignChatGet: (cid, chatID) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/chats/${encodeURIComponent(chatID)}`).then(json),

	campaignChatDelete: (cid, chatID) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/chats/${encodeURIComponent(chatID)}`, { method: "DELETE" }).then(json),

	campaignMembers: (cid) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/members`).then(json),

	campaignMemberUpdate: (cid, uid, patch) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/members/${encodeURIComponent(uid)}`, {
			method: "PATCH",
			headers: { "content-type": "application/json" },
			body: JSON.stringify(patch),
		}).then(json),

	// The campaign session layer: campaigns to hang sessions on, sessions,
	// verbatim sources, addressable spans, the event log, and the Markdown
	// export. Sources are immutable; spans are byte offsets into the stored
	// content.
	listCampaigns: () => fetch("/api/campaigns").then(json),

	createCampaign: (name, system) =>
		fetch("/api/campaigns", {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({ name, system }),
		}).then(json),

	listSessions: (campaignID) =>
		fetch(`/api/campaigns/${encodeURIComponent(campaignID)}/sessions`).then(json),

	createSession: (campaignID, name) =>
		fetch(`/api/campaigns/${encodeURIComponent(campaignID)}/sessions`, {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({ name }),
		}).then(json),

	updateSession: (campaignID, sessionID, patch) =>
		fetch(`/api/campaigns/${encodeURIComponent(campaignID)}/sessions/${encodeURIComponent(sessionID)}`, {
			method: "PATCH",
			headers: { "content-type": "application/json" },
			body: JSON.stringify(patch),
		}).then(json),

	campaignMemberRemove: (cid, uid) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/members/${encodeURIComponent(uid)}`, { method: "DELETE" }).then(json),

	campaignInviteCreate: (cid, role) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/invites`, {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({ role }),
		}).then(json),

	campaignInvites: (cid) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/invites`).then(json),

	listSources: (campaignID, sessionID) =>
		fetch(`/api/campaigns/${encodeURIComponent(campaignID)}/sessions/${encodeURIComponent(sessionID)}/sources`).then(json),

	getSource: (campaignID, sessionID, sourceID) =>
		fetch(`/api/campaigns/${encodeURIComponent(campaignID)}/sessions/${encodeURIComponent(sessionID)}/sources/${encodeURIComponent(sourceID)}`).then(json),

	addSource: (campaignID, sessionID, payload) =>
		fetch(`/api/campaigns/${encodeURIComponent(campaignID)}/sessions/${encodeURIComponent(sessionID)}/sources`, {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify(payload),
		}).then(json),

	uploadSource: (campaignID, sessionID, file, kind) => {
		const body = new FormData();
		body.set("file", file);
		body.set("kind", kind || "transcript");
		return fetch(`/api/campaigns/${encodeURIComponent(campaignID)}/sessions/${encodeURIComponent(sessionID)}/sources`, {
			method: "POST",
			body,
		}).then(json);
	},

	// The optional audio→transcript hook (MAD-320): the recording goes to the
	// configured endpoint chunk by chunk in the background; poll
	// getTranscription until status is completed/failed/cancelled.
	uploadAudio: (campaignID, sessionID, file) => {
		const body = new FormData();
		body.set("file", file);
		return fetch(`/api/campaigns/${encodeURIComponent(campaignID)}/sessions/${encodeURIComponent(sessionID)}/transcriptions`, {
			method: "POST",
			body,
		}).then(json);
	},

	getTranscription: (campaignID, sessionID, jobID) =>
		fetch(`/api/campaigns/${encodeURIComponent(campaignID)}/sessions/${encodeURIComponent(sessionID)}` +
			`/transcriptions/${encodeURIComponent(jobID)}`).then(json),

	resolveSpan: (campaignID, sessionID, sourceID, start, end) =>
		fetch(`/api/campaigns/${encodeURIComponent(campaignID)}/sessions/${encodeURIComponent(sessionID)}` +
			`/span?source_id=${encodeURIComponent(sourceID)}&start=${start}&end=${end}`).then(json),

	addEvent: (campaignID, sessionID, event) =>
		fetch(`/api/campaigns/${encodeURIComponent(campaignID)}/sessions/${encodeURIComponent(sessionID)}/events`, {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify(event),
		}).then(json),

	listEvents: (campaignID, sessionID) =>
		fetch(`/api/campaigns/${encodeURIComponent(campaignID)}/sessions/${encodeURIComponent(sessionID)}/events`).then(json),

	// The canon review queue (MAD-310): the DM's human gate. Build refreshes
	// the queue from the three upstream passes; decide accepts, modifies or
	// dismisses one open item; export downloads the applied changes.
	reviewBuild: (campaignID) =>
		fetch(`/api/campaigns/${encodeURIComponent(campaignID)}/canon/reviews/build`, { method: "POST" }).then(json),

	reviews: (campaignID, status) =>
		fetch(`/api/campaigns/${encodeURIComponent(campaignID)}/canon/reviews` +
			(status ? `?status=${encodeURIComponent(status)}` : "")).then(json),

	reviewDecide: (campaignID, reviewID, decision, note, payload) =>
		fetch(`/api/campaigns/${encodeURIComponent(campaignID)}/canon/reviews/${encodeURIComponent(reviewID)}/decision`, {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({ decision, note, payload }),
		}).then(json),

	// The queue's one batch affordance: accept every open proposed_* item the
	// adversarial pass agreed on at or above the given threshold. There is
	// deliberately no "accept everything".
	reviewAcceptAgree: (campaignID, minAgreement) =>
		fetch(`/api/campaigns/${encodeURIComponent(campaignID)}/canon/reviews/accept-agree`, {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({ min_agreement: minAgreement }),
		}).then(json),

	reviewExportURL: (campaignID, sessionID) =>
		`/api/campaigns/${encodeURIComponent(campaignID)}/canon/reviews/export` +
		(sessionID ? `?session_id=${encodeURIComponent(sessionID)}` : ""),

	// Proposal batches (MAD-359): a generator's multi-object proposal behind
	// the one review gate. List reads the batches; get reads one with its
	// items; decide accepts or dismisses the whole batch, with per-item
	// overrides (modify payloads, per-item dismissals) in the body.
	proposals: (campaignID, status) =>
		fetch(`/api/campaigns/${encodeURIComponent(campaignID)}/proposals` +
			(status ? `?status=${encodeURIComponent(status)}` : "")).then(json),

	proposal: (campaignID, batchID) =>
		fetch(`/api/campaigns/${encodeURIComponent(campaignID)}/proposals/${encodeURIComponent(batchID)}`).then(json),

	proposalDecide: (campaignID, batchID, decision, items) =>
		fetch(`/api/campaigns/${encodeURIComponent(campaignID)}/proposals/${encodeURIComponent(batchID)}/decision`, {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({ decision, items }),
		}).then(json),

	// The campaign skeleton generator (MAD-361): a premise becomes a
	// proposal batch plus the spine's acts and session plans. DM-only.
	skeleton: (campaignID, body) =>
		fetch(`/api/campaigns/${encodeURIComponent(campaignID)}/design/skeleton`, {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify(body),
		}).then(json),

	// The natural-language command interface (MAD-363): text in; a proposal
	// batch, a clarifying question with its candidates, a plain refusal, or
	// a spine write out. DM-only. The campaign chat's slash commands and the
	// campaign view's command bar are the two surfaces onto this endpoint.
	campaignCommand: (campaignID, text) =>
		fetch(`/api/campaigns/${encodeURIComponent(campaignID)}/command`, {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({ text }),
		}).then(json),

	campaignCommandLog: (campaignID, limit) =>
		fetch(`/api/campaigns/${encodeURIComponent(campaignID)}/command/log` +
			(limit ? `?limit=${limit}` : "")).then(json),

	sessionExportURL: (campaignID, sessionID) =>
		`/api/campaigns/${encodeURIComponent(campaignID)}/sessions/${encodeURIComponent(sessionID)}/export`,

	// The narrative spine (MAD-360): acts, scenes, cast, secrets, outcomes
	// and session plans. DM-only — the server refuses every other scope;
	// the shapes and pace helpers are pure math and need no campaign.
	story: (campaignID) =>
		fetch(`/api/campaigns/${encodeURIComponent(campaignID)}/story`).then(json),

	storyShapes: () => fetch("/api/story/shapes").then(json),

	storyPace: (from, to, acts) =>
		fetch(`/api/story/pace?from=${from}&to=${to}&acts=${acts}`).then(json),

	campaignQuests: (cid) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/quests`).then(json),

	// The quest graph (MAD-369): the DM's board and the player's journal.
	campaignQuestDetail: (cid, questID) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/quests/${encodeURIComponent(questID)}`).then(json),

	campaignQuestUpdate: (cid, questID, patch) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/quests/${encodeURIComponent(questID)}`, {
			method: "PATCH",
			headers: { "content-type": "application/json" },
			body: JSON.stringify(patch),
		}).then(json),

	campaignQuestDelete: (cid, questID) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/quests/${encodeURIComponent(questID)}`, { method: "DELETE" }).then(json),

	campaignQuestEntityAdd: (cid, questID, entityID, role) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/quests/${encodeURIComponent(questID)}/entities`, {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({ entity_id: entityID, role }),
		}).then(json),

	campaignQuestEntityRemove: (cid, questID, entityID, role) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/quests/${encodeURIComponent(questID)}/entities/${encodeURIComponent(entityID)}` +
			(role ? `?role=${encodeURIComponent(role)}` : ""), { method: "DELETE" }).then(json),

	campaignQuestJournal: (cid) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/quests/journal`).then(json),

	campaignQuestTransition: (cid, questID, to) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/quests/${encodeURIComponent(questID)}/transition`, {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({ to }),
		}).then(json),

	// The quest designer (MAD-371): a hook becomes a staged branching
	// quest, and "branch this quest" proposes two exclusive outcomes off a
	// live state. Both come back as proposal batches for the review queue.
	questDesign: (cid, body) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/design/quest`, {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify(body),
		}).then(json),

	questBranch: (cid, questID, body) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/quests/${encodeURIComponent(questID)}/design/branch`, {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify(body),
		}).then(json),

	// The place designer (MAD-372): a premise becomes a settlement, and a
	// location that already exists gets fleshed out around what is there.
	// Both come back as proposal batches for the review queue.
	locationDesign: (cid, body) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/design/location`, {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify(body),
		}).then(json),

	locationFleshOut: (cid, entityID, body) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/locations/${encodeURIComponent(entityID)}/design/flesh-out`, {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify(body),
		}).then(json),

	// The dungeon designer (MAD-373): the seeded room graph, its map,
	// and the dressing pass. Creating and editing need no model; dressing
	// is the model pass; placing stages a proposal batch.
	dungeons: (cid) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/dungeons`).then(json),

	dungeonCreate: (cid, body) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/dungeons`, {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify(body),
		}).then(json),

	dungeonGet: (cid, did) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/dungeons/${encodeURIComponent(did)}`).then(json),

	dungeonDelete: (cid, did) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/dungeons/${encodeURIComponent(did)}`, { method: "DELETE" }).then(json),

	dungeonRoomPatch: (cid, did, roomID, patch) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/dungeons/${encodeURIComponent(did)}/rooms/${encodeURIComponent(roomID)}`, {
			method: "PATCH",
			headers: { "content-type": "application/json" },
			body: JSON.stringify(patch),
		}).then(json),

	dungeonEdgeAdd: (cid, did, body) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/dungeons/${encodeURIComponent(did)}/edges`, {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify(body),
		}).then(json),

	dungeonEdgeDelete: (cid, did, edgeID) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/dungeons/${encodeURIComponent(did)}/edges/${encodeURIComponent(edgeID)}`, { method: "DELETE" }).then(json),

	dungeonDress: (cid, did) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/dungeons/${encodeURIComponent(did)}/dress`, {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: "{}",
		}).then(json),

	dungeonPlace: (cid, did) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/dungeons/${encodeURIComponent(did)}/place`, {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: "{}",
		}).then(json),

	actCreate: (cid, act) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/acts`, {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify(act),
		}).then(json),

	actUpdate: (cid, actID, patch) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/acts/${encodeURIComponent(actID)}`, {
			method: "PATCH",
			headers: { "content-type": "application/json" },
			body: JSON.stringify(patch),
		}).then(json),

	actDelete: (cid, actID) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/acts/${encodeURIComponent(actID)}`, { method: "DELETE" }).then(json),

	sceneCreate: (cid, scene) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/scenes`, {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify(scene),
		}).then(json),

	sceneGet: (cid, sceneID) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/scenes/${encodeURIComponent(sceneID)}`).then(json),

	sceneUpdate: (cid, sceneID, patch) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/scenes/${encodeURIComponent(sceneID)}`, {
			method: "PATCH",
			headers: { "content-type": "application/json" },
			body: JSON.stringify(patch),
		}).then(json),

	sceneDelete: (cid, sceneID) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/scenes/${encodeURIComponent(sceneID)}`, { method: "DELETE" }).then(json),

	sceneCastAdd: (cid, sceneID, cast) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/scenes/${encodeURIComponent(sceneID)}/cast`, {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify(cast),
		}).then(json),

	sceneCastRemove: (cid, sceneID, entityID) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/scenes/${encodeURIComponent(sceneID)}` +
			`/cast/${encodeURIComponent(entityID)}`, { method: "DELETE" }).then(json),

	sceneSecretAdd: (cid, sceneID, secret) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/scenes/${encodeURIComponent(sceneID)}/secrets`, {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify(secret),
		}).then(json),

	sceneSecretRemove: (cid, sceneID, factID) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/scenes/${encodeURIComponent(sceneID)}` +
			`/secrets/${encodeURIComponent(factID)}`, { method: "DELETE" }).then(json),

	sceneOutcomeAdd: (cid, sceneID, outcome) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/scenes/${encodeURIComponent(sceneID)}/outcomes`, {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify(outcome),
		}).then(json),

	sceneOutcomeRemove: (cid, sceneID, label) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/scenes/${encodeURIComponent(sceneID)}` +
			`/outcomes/${encodeURIComponent(label)}`, { method: "DELETE" }).then(json),

	sessionPlanGet: (cid, sessionID) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/sessions/${encodeURIComponent(sessionID)}/plan`).then(json),

	sessionPlanPut: (cid, sessionID, plan) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/sessions/${encodeURIComponent(sessionID)}/plan`, {
			method: "PUT",
			headers: { "content-type": "application/json" },
			body: JSON.stringify(plan),
		}).then(json),

	sessionPlanDelete: (cid, sessionID) =>
		fetch(`/api/campaigns/${encodeURIComponent(cid)}/sessions/${encodeURIComponent(sessionID)}/plan`, { method: "DELETE" }).then(json),
};

/**
 * Consume a server-sent events stream from a POST endpoint. Handlers:
 * onMeta(payload), onDelta(text), onDone(payload), onError(message, payload).
 * Returns a promise that settles when the stream closes.
 *
 * Uses fetch + a stream reader rather than EventSource, which cannot POST.
 */
async function postSSE(url, payload, handlers, signal) {
	const res = await fetch(url, {
		method: "POST",
		headers: { "content-type": "application/json" },
		body: JSON.stringify(payload),
		signal,
	});
	if (!res.ok || !res.body) {
		gateOn401(res);
		let detail = res.statusText;
		try {
			const body = await res.json();
			if (body && body.error) detail = body.error;
		} catch (_) { /* keep the status text */ }
		handlers.onError?.(detail);
		return;
	}

	const reader = res.body.getReader();
	const decoder = new TextDecoder();
	let buffer = "";

	for (;;) {
		const { value, done } = await reader.read();
		if (done) break;
		buffer += decoder.decode(value, { stream: true });

		// Events are separated by a blank line; keep any partial tail buffered.
		let split;
		while ((split = buffer.indexOf("\n\n")) !== -1) {
			const frame = buffer.slice(0, split);
			buffer = buffer.slice(split + 2);
			dispatch(frame, handlers);
		}
	}
	if (buffer.trim()) dispatch(buffer, handlers);
}

/** Post a chat question and consume the answer as server-sent events. */
export function streamAnswer(chatID, question, handlers, signal) {
	return postSSE(`/api/chats/${encodeURIComponent(chatID)}/messages`, { question }, handlers, signal);
}

/** Ask a campaign thread a question and consume the answer as SSE. The
 *  campaign's citation payload rides the meta frame alongside the rules
 *  sources. */
export function streamCampaignAnswer(cid, chatID, question, handlers, signal) {
	return postSSE(
		`/api/campaigns/${encodeURIComponent(cid)}/chats/${encodeURIComponent(chatID)}/messages`,
		{ question }, handlers, signal);
}

/** Post a board + sequence to the resolver and consume the trace as SSE. */
export function streamResolve(board, sequence, note, handlers, signal) {
	return postSSE("/api/resolve", { board, sequence, note }, handlers, signal);
}

/** Ask the designer for a whole encounter and consume it as server-sent
 *  events. Everything in the payload is optional — an empty brief is the case
 *  the designer exists for. */
export function streamEncounterDesign(payload, handlers, signal) {
	return postSSE("/api/encounter/design", payload, handlers, signal);
}

/** Post a deck build request and consume the draft as server-sent events. */
export function streamDeckBuild(idea, commander, feedback, current, handlers, signal) {
	return postSSE("/api/deck/build", { idea, commander, feedback, current }, handlers, signal);
}

/** Ask a question about the deck on screen and consume the answer as SSE. The
 *  deck travels with the question because the surface is stateless — an
 *  unsaved draft is as discussable as a saved one. */
export function streamDeckChat(commander, cards, question, history, handlers, signal) {
	return postSSE("/api/deck/chat", { commander, cards, question, history }, handlers, signal);
}

function dispatch(frame, handlers) {
	let event = "message";
	const data = [];
	for (const line of frame.split("\n")) {
		if (line.startsWith("event:")) event = line.slice(6).trim();
		else if (line.startsWith("data:")) data.push(line.slice(5).trim());
	}
	if (data.length === 0) return;
	let payload;
	try {
		payload = JSON.parse(data.join("\n"));
	} catch (_) {
		return;
	}
	switch (event) {
		case "meta": handlers.onMeta?.(payload); break;
		case "delta": handlers.onDelta?.(payload.text || ""); break;
		case "done": handlers.onDone?.(payload); break;
		case "error": handlers.onError?.(payload.error || "unknown error", payload); break;
	}
}
