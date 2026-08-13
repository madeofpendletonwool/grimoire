// Shared client state.
//
// `corpus` is the rule set for the *next* new chat. An open conversation uses
// its own locked corpus (chat.corpus) — grounding differs per rule set, so a
// thread that switched corpora mid-way would carry incoherent history.

export const state = {
	corpus: "mtg",     // corpus for new chats
	mode: "ask",       // "ask" (free Q&A) | "resolve" (MTG interaction resolver)
	chat: null,        // the open conversation, or null on the welcome screen
	chats: [],         // sidebar list
	meta: null,        // /api/meta payload
	streaming: false,
};

const KEY = "grimoire.corpus";

export function loadCorpusPreference() {
	try {
		const saved = localStorage.getItem(KEY);
		if (saved === "mtg" || saved === "dnd") state.corpus = saved;
	} catch (_) { /* private mode — the default is fine */ }
}

export function saveCorpusPreference(corpus) {
	try {
		localStorage.setItem(KEY, corpus);
	} catch (_) { /* non-fatal */ }
}

/** The corpus that should be displayed and themed right now. */
export function activeCorpus() {
	return state.chat ? state.chat.corpus : state.corpus;
}

export function corpusLabel(corpus) {
	return corpus === "dnd" ? "⚔ D&D 5e SRD" : "✦ Magic: The Gathering";
}

/** Card lookup is MTG-only — Scryfall is the Magic card authority. */
export const supportsCards = (corpus) => corpus === "mtg";

/** The interaction resolver is MTG-only — it grounds in Magic's stack/trigger/layer/replacement rules. */
export const supportsResolve = (corpus) => corpus === "mtg";
