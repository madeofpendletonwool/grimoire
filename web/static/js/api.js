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

	listEncounters: () => fetch("/api/encounters").then(json),

	getEncounter: (id) => fetch(`/api/encounters/${encodeURIComponent(id)}`).then(json),

	saveEncounter: (id, name, party, monsters) =>
		fetch(id ? `/api/encounters/${encodeURIComponent(id)}` : "/api/encounters", {
			method: id ? "PATCH" : "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({ name, party, monsters }),
		}).then(json),

	deleteEncounter: (id) =>
		fetch(`/api/encounters/${encodeURIComponent(id)}`, { method: "DELETE" }).then(json),

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

/** Post a board + sequence to the resolver and consume the trace as SSE. */
export function streamResolve(board, sequence, note, handlers, signal) {
	return postSSE("/api/resolve", { board, sequence, note }, handlers, signal);
}

/** Post a deck build request and consume the draft as server-sent events. */
export function streamDeckBuild(idea, commander, feedback, current, handlers, signal) {
	return postSSE("/api/deck/build", { idea, commander, feedback, current }, handlers, signal);
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
