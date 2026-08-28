// The campaign chat (MAD-311): the DM Grimoire and the Player Grimoire. The
// server resolves the caller's scope from their membership row, pins it on
// the thread, and grounds every answer in what that perspective may read —
// this module renders whatever streams back and never decides visibility
// itself. "You don't know that" is the record speaking, not the model
// improvising.

import { openTool } from "./wm/wm.js";
import { $, el, clear, isNarrow } from "./dom.js";
import { api, streamCampaignAnswer } from "./api.js";
import { renderAnswer, renderCitations } from "./render.js";
import { reviewFor } from "./review.js";

let campaigns = [];
let campaignID = null; // { id, my_role }
let thread = null; // the open conversation
let threads = [];
let streaming = false;
let abort = null;

function wire() {
	$("cchat-campaign").addEventListener("change", onPickCampaign);
	$("cchat-new").addEventListener("click", onNewThread);
	$("cchat-composer").addEventListener("submit", onSubmit);
	$("cchat-stop").addEventListener("click", stopStreaming);
	$("cchat-history").addEventListener("click", onHistoryClick);
	$("cchat-transcript").addEventListener("click", onCommandStripClick);
	$("cchat-go-campaign").addEventListener("click", () => {
		openTool("campaign");
	});
	$("cchat-input").addEventListener("keydown", (e) => {
		if (e.key === "Enter" && !e.shiftKey) {
			e.preventDefault();
			$("cchat-composer").requestSubmit();
		}
	});
}

async function openCampaignChat() {
	await loadCampaigns();
}

function closeCampaignChat() {
	stopStreaming();
}

/* ---------- campaigns and threads ---------- */

async function loadCampaigns() {
	let data;
	try {
		data = await api.campaignList();
	} catch (err) {
		renderMeta(err.message, true);
		return;
	}
	campaigns = data.campaigns || [];
	const sel = clear($("cchat-campaign"));
	if (!campaigns.length) {
		sel.append(el("option", { text: "No campaigns yet…", attrs: { value: "" } }));
		$("cchat-body").hidden = true;
		$("cchat-empty").hidden = false;
		return;
	}
	$("cchat-body").hidden = false;
	$("cchat-empty").hidden = true;
	for (const c of campaigns) sel.append(el("option", { text: c.name, attrs: { value: c.id } }));
	if (!campaigns.some((c) => c.id === campaignID)) campaignID = campaigns[0].id;
	sel.value = campaignID;
	await loadThreads();
}

function onPickCampaign() {
	const id = $("cchat-campaign").value;
	if (!id || id === campaignID) return;
	campaignID = id;
	thread = null;
	loadThreads();
}

async function loadThreads() {
	if (!campaignID) return;
	let data;
	try {
		data = await api.campaignChatList(campaignID);
	} catch (err) {
		renderMeta(err.message, true);
		return;
	}
	threads = data.chats || [];
	renderHistory();
	renderPerspective(data.perspective);
	if (threads.length > 0) openThread(threads[0].id);
	else openThread(null);
}

async function onNewThread() {
	if (!campaignID || streaming) return;
	let data;
	try {
		data = await api.campaignChatCreate(campaignID);
	} catch (err) {
		renderMeta(err.message, true);
		return;
	}
	threads = [data.chat, ...threads];
	renderHistory();
	renderPerspective(data.perspective);
	openThread(data.chat.id);
	$("cchat-input").focus();
}

function renderHistory() {
	const nav = clear($("cchat-history"));
	if (!threads.length) {
		nav.append(el("p", { class: "camp-status", text: "No threads yet." }));
		return;
	}
	for (const t of threads) {
		const row = el("button", {
			class: "cchat-thread" + (thread && thread.id === t.id ? " is-active" : ""),
			attrs: { type: "button", "data-thread": t.id },
		});
		row.append(el("span", { class: "cchat-thread-title", text: t.title || "New thread" }));
		row.append(el("span", {
			class: "cchat-thread-del",
			text: "×",
			attrs: { role: "button", "aria-label": "Delete thread", "data-del": t.id, title: "Delete thread" },
		}));
		nav.append(row);
	}
}

function onHistoryClick(e) {
	const del = e.target.closest("[data-del]");
	if (del) {
		e.stopPropagation();
		deleteThread(del.dataset.del);
		return;
	}
	const row = e.target.closest("[data-thread]");
	if (row) openThread(row.dataset.thread);
}

async function deleteThread(id) {
	try {
		await api.campaignChatDelete(campaignID, id);
	} catch (err) {
		renderMeta(err.message, true);
		return;
	}
	threads = threads.filter((t) => t.id !== id);
	if (thread && thread.id === id) {
		thread = null;
		openThread(threads.length ? threads[0].id : null);
	} else {
		renderHistory();
	}
}

/* ---------- the transcript ---------- */

async function openThread(id) {
	if (streaming) stopStreaming();
	const box = clear($("cchat-transcript"));
	if (!id) {
		thread = null;
		renderHistory();
		box.append(el("p", { class: "camp-status", text: "A fresh thread — ask your campaign anything your perspective might know." }));
		return;
	}
	box.append(el("p", { class: "camp-status", text: "Opening the thread…" }));
	try {
		const data = await api.campaignChatGet(campaignID, id);
		thread = data.chat;
		renderHistory();
		const list = clear($("cchat-transcript"));
		const msgs = data.messages || [];
		if (!msgs.length) {
			list.append(el("p", { class: "camp-status", text: "A fresh thread — ask your campaign anything your perspective might know." }));
		}
		for (const m of msgs) list.append(m.role === "user" ? userMessage(m.content) : sageMessage(m));
		scrollTranscript();
	} catch (err) {
		const list = clear($("cchat-transcript"));
		list.append(el("p", { class: "camp-status warn", text: err.message }));
	}
}

function userMessage(text) {
	return el("div", { class: "cchat-msg is-user" },
		el("div", { class: "cchat-bubble", text }));
}

/** An assistant turn: the answer prose plus its citations — campaign facts
 *  first (the record this perspective answered from), then any rules
 *  excerpts and reference entries the sage consulted. */
function sageMessage(m, meta) {
	const camp = (meta && meta.campaign) || parseCampaignPayload(m.campaign);
	const sources = (meta && meta.sources) || m.sources || [];
	const entities = (meta && meta.entities) || m.entities || [];
	const unresolved = meta ? meta.unresolved : null;
	const wrap = el("div", { class: "cchat-msg" });
	const prose = el("div", { class: "cchat-answer prose" });
	renderAnswer(prose, m.content || "", "dnd");
	wrap.append(el("div", { class: "cchat-mark", text: "the Grimoire" }), prose);

	// A command turn (MAD-363): the server routed a slash command to the
	// command engine and the payload rides in the meta / campaign column.
	// The candidates render as chips; a staged batch gets the door into
	// the review queue.
	const command = (meta && meta.command) || commandFromPayload(camp);
	if (command) {
		const strip = renderCommandStrip(command);
		if (strip) wrap.append(strip);
	} else {
		const record = renderRecord(camp);
		if (record) wrap.append(record);
	}
	const cites = renderCitations(sources, null, entities, unresolved, "dnd");
	if (cites) wrap.append(cites);
	return wrap;
}

// commandFromPayload recognizes a persisted command turn's payload (the
// campaign column carries the engine's result JSON).
function commandFromPayload(camp) {
	if (camp && typeof camp === "object" && typeof camp.kind === "string" && typeof camp.message === "string") {
		return camp;
	}
	return null;
}

// renderCommandStrip draws a command result's affordances: the staged
// batch's items with a door into the review queue, or the question's
// candidates as one-click names that fill the composer.
function renderCommandStrip(command) {
	const strip = el("div", { class: "citations cchat-cites cchat-command" });
	strip.append(el("span", { class: "citations-label", text: "Command:" }));
	if (command.kind === "question" && command.question && (command.question.candidates || []).length) {
		for (const c of command.question.candidates) {
			const label = c.kind ? `${c.name} (${c.kind})` : c.name;
			const chip = el("button", { class: "chip", text: label, attrs: { type: "button", "data-cmd-name": c.name } });
			if (c.note) chip.title = c.note;
			strip.append(chip);
		}
		return strip;
	}
	if (command.kind === "batch" && command.batch) {
		for (const it of (command.batch.items || []).slice(0, 6)) {
			strip.append(el("span", { class: "chip", text: it.subject || it.kind }));
		}
		strip.append(el("button", {
			class: "chip is-review",
			text: "review the proposal →",
			attrs: { type: "button", "data-cmd-review": command.batch.id },
		}));
		return strip;
	}
	return null;
}

function parseCampaignPayload(raw) {
	if (!raw) return null;
	try {
		return typeof raw === "string" ? JSON.parse(raw) : raw;
	} catch (_) {
		return null;
	}
}

/** "From the record:" the campaign rows this answer rests on. Secret facts
 *  are tagged — only the DM's citations can ever carry one. */
function renderRecord(camp) {
	if (!camp) return null;
	const facts = camp.facts || [];
	const ents = camp.entities || [];
	const events = camp.events || [];
	if (!facts.length && !ents.length && !events.length) return null;
	const strip = el("div", { class: "citations cchat-cites" });
	strip.append(el("span", { class: "citations-label", text: "Record:" }));
	for (const f of facts.slice(0, 8)) {
		const chip = el("span", { class: "chip" + (f.visibility === "secret" ? " is-secret" : ""), text: f.statement });
		if (f.provenance && f.provenance.length) {
			chip.title = f.provenance.map((p) => p.quote ? `${p.method}: “${p.quote}”` : p.method).join("\n");
		}
		strip.append(chip);
	}
	for (const e of ents.slice(0, 6)) strip.append(el("span", { class: "chip", text: e.name }));
	for (const ev of events.slice(0, 4)) strip.append(el("span", { class: "chip", text: ev.summary }));
	return strip;
}

/* ---------- asking ---------- */

function onSubmit(e) {
	e.preventDefault();
	const input = $("cchat-input");
	const text = input.value.trim();
	if (!text || streaming) return;
	if (!thread) {
		renderMeta("Start a thread first.", true);
		return;
	}
	input.value = "";
	ask(text);
}

async function ask(question) {
	streaming = true;
	abort = new AbortController();
	$("cchat-stop").hidden = false;
	$("cchat-send").disabled = true;

	const box = $("cchat-transcript");
	if ($("cchat-status")) $("cchat-status").remove();
	box.append(userMessage(question));
	const pending = el("div", { class: "cchat-msg is-pending" },
		el("div", { class: "cchat-mark", text: "the Grimoire" }),
		el("p", { class: "camp-status cchat-thinking", text: "Consulting the record…" }));
	box.append(pending);
	scrollTranscript();

	let answer = null;
	let prose = null;
	let meta = null;
	try {
		await streamCampaignAnswer(campaignID, thread.id, question, {
			onMeta: (payload) => {
				meta = payload;
				if (payload.title && thread && !thread.title) {
					thread.title = payload.title;
					renderHistory();
				}
				if (payload.perspective) renderPerspective(payload.perspective);
			},
			onDelta: (text) => {
				if (answer === null) {
					answer = "";
					clear(pending);
					prose = el("div", { class: "cchat-answer prose" });
					pending.append(el("div", { class: "cchat-mark", text: "the Grimoire" }), prose);
				}
				answer += text;
				renderAnswer(prose, answer, "dnd");
				scrollTranscript();
			},
			onDone: () => {
				const finished = sageMessage({ content: answer || "(no answer)" }, meta);
				pending.replaceWith(finished);
				scrollTranscript();
			},
			onError: (errText) => {
				if (answer !== null) {
					pending.classList.remove("is-pending");
					pending.append(el("p", { class: "camp-status warn", text: `— cut short: ${errText}` }));
				} else {
					clear(pending).append(el("p", { class: "camp-status warn", text: errText }));
				}
			},
		}, abort.signal);
	} catch (_) {
		// the reader went away or the request failed; onError already spoke
	} finally {
		streaming = false;
		abort = null;
		$("cchat-stop").hidden = true;
		$("cchat-send").disabled = false;
	}
}

function stopStreaming() {
	if (abort) abort.abort();
}

// onCommandStripClick handles a command result's affordances: the door
// into the review queue, and the candidate chips that drop a name into
// the composer for the next command.
function onCommandStripClick(e) {
	const review = e.target.closest("[data-cmd-review]");
	if (review) {
		reviewFor(campaignID);
		return;
	}
	const name = e.target.closest("[data-cmd-name]");
	if (name) {
		const input = $("cchat-input");
		input.value = (input.value ? input.value.replace(/\s+$/, "") + " " : "") + name.dataset.cmdName;
		input.focus();
	}
}

function scrollTranscript() {
	const box = $("cchat-transcript");
	box.scrollTop = box.scrollHeight;
}

/* ---------- chrome ---------- */

function renderMeta(message, warn) {
	const meta = $("cchat-meta");
	meta.textContent = message || "";
	meta.classList.toggle("warn", !!warn);
}

/** The perspective badge: whose knowledge this thread answers from. */
function renderPerspective(perspective) {
	if (perspective === "dm") {
		renderMeta("DM Grimoire — the whole record, secrets included.");
		$("cchat-input").placeholder = "Ask anything — or /command the world (“/create an npc called Vess”, “/undo”)…";
	} else if (perspective === "party") {
		renderMeta("Player Grimoire — what the party has discovered.");
	} else if (perspective) {
		renderMeta(`Player Grimoire — what ${perspective} has learned.`);
	}
}

/* ---------- the window-manager contract ---------- */

// mount() adopts the surface that already exists in index.html rather than
// building one, which is what made migrating nine surfaces tractable: every
// $("cchat-transcript") lookup inside this module keeps working untouched. The
// cost is one window per tool; see the note on `instances` in wm/registry.js.
let wired = false;

export const tool = {
	mount(host) {
		const view = $("cchat-view");
		host.append(view);
		view.hidden = false;
		if (!wired) {
			wire();
			wired = true;
		}
		openCampaignChat();
		return { destroy: closeCampaignChat };
	},
};
