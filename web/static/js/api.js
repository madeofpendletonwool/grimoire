// HTTP surface for the Grimoire API.

async function json(res) {
	if (!res.ok) {
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
};

/**
 * Post a question and consume the answer as server-sent events.
 *
 * Uses fetch + a stream reader rather than EventSource, which cannot POST.
 * Handlers: onMeta(citations), onDelta(text), onDone(info), onError(message).
 * Returns a promise that settles when the stream closes.
 */
export async function streamAnswer(chatID, question, handlers, signal) {
	const res = await fetch(`/api/chats/${encodeURIComponent(chatID)}/messages`, {
		method: "POST",
		headers: { "content-type": "application/json" },
		body: JSON.stringify({ question }),
		signal,
	});
	if (!res.ok || !res.body) {
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
