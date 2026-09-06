// The board window (MAD-423, stage 6 of MAD-417): the party, live. One
// strip per member — hp (a number or the table's own word, whichever the
// campaign says), conditions, concentration, the slot summary, and who is
// at the table. A fight adds the public battle — round, turn, order —
// and the DM's read adds the other side's numbers and the visibility
// controls.
//
// Legibility is the design constraint (MAD-318's note, applied
// verbatim): readable at arm's length in a dim room, on a phone. Big
// numbers, high contrast, few words. The board is a table surface, not a
// dashboard.
//
// One stream drives it, the dice feed's shape applied to snapshots: the
// open frame paints the board, every change re-paints it whole, and the
// server already decided what this viewer may see — nothing here hides
// anything, because nothing hidden ever arrives.

import { $, el, clear } from "./dom.js";
import { api } from "./api.js";

let campaigns = [];
let campaignID = null;
let wired = false;
let mounted = false;
let streamCtl = null;
let lastBoard = null;

/* ---------- the stream ---------- */

function startStream() {
	stopStream();
	if (!campaignID) return;
	streamCtl = { campaign: campaignID };
	const ctl = streamCtl;

	const connect = () => {
		if (!streamCtl || streamCtl.campaign !== campaignID) return;
		const es = new EventSource(`/api/campaigns/${encodeURIComponent(campaignID)}/board/stream`);
		ctl.es = es;
		es.addEventListener("open", () => {
			ctl.failures = 0;
		});
		for (const kind of ["open", "board"]) {
			es.addEventListener(kind, (ev) => {
				let payload;
				try { payload = JSON.parse(ev.data); } catch (_) { return; }
				if (payload.board) paint(payload.board);
			});
		}
		es.onerror = () => {
			// Reconnect fresh (the URL carries no cursor to go stale);
			// a stream that keeps failing stops trying — the window's
			// next open starts a new one.
			es.close();
			if (!streamCtl || streamCtl.campaign !== campaignID) return;
			ctl.failures = (ctl.failures || 0) + 1;
			if (ctl.failures > 8) return;
			setTimeout(connect, 2500);
		};
	};
	connect();
}

function stopStream() {
	if (streamCtl && streamCtl.es) streamCtl.es.close();
	streamCtl = null;
}

/* ---------- painting ---------- */

function paint(board) {
	lastBoard = board;
	if (!mounted) return;
	renderMeta();
	renderBanner(board.combat || null);
	renderStrips(board);
	renderMonsters(board);
	renderSettings(board);
	renderPresence(board);
}

function renderMeta() {
	const meta = $("board-meta");
	if (!lastBoard) {
		meta.textContent = "";
		return;
	}
	const cfg = lastBoard.config || {};
	const mode = cfg.hp === "word" ? "health words" : "exact hp";
	meta.textContent = lastBoard.dm
		? `DM — every number, and the other side. The table sees ${mode}.`
		: `Live — ${mode} at this table.`;
}

function renderBanner(combat) {
	const banner = $("board-banner");
	if (!combat) {
		banner.hidden = true;
		return;
	}
	banner.hidden = false;
	clear(banner);
	banner.append(el("b", { class: "board-round", text: `Round ${combat.round}` }));
	banner.append(el("span", { class: "board-turn", text: `— ${combat.turn || "the fight begins"}'s move` }));
	const order = el("ol", { class: "board-order" });
	for (const entry of combat.order || []) {
		const li = el("li", {
			class: "board-order-entry" +
				(entry.active ? " is-active" : "") +
				(entry.dead ? " is-dead" : "") +
				(entry.pc ? "" : " is-foe"),
		});
		li.append(el("span", { class: "board-order-init", text: String(entry.initiative) }));
		li.append(el("span", { class: "board-order-name", text: entry.name }));
		if (entry.down) li.append(el("i", { class: "board-order-mark", text: "down" }));
		order.append(li);
	}
	banner.append(order);
}

function renderStrips(board) {
	const host = clear($("board-strips"));
	for (const m of board.members || []) host.append(stripEl(m, board.dm));
	if (!host.childElementCount) {
		host.append(el("p", { class: "board-hint", text: "No one is bound to a character yet — strips arrive when players take their seats." }));
	}
}

function stripEl(m, dm) {
	const strip = el("article", {
		class: "strip" +
			(m.down ? " is-down" : "") +
			(m.dead ? " is-dead" : "") +
			(m.present ? " is-present" : ""),
	});

	const head = el("div", { class: "strip-head" });
	const who = el("span", { class: "strip-name" });
	who.append(el("b", { text: m.name }));
	if (m.present) who.append(el("i", { class: "strip-here", title: "at the table", text: "•" }));
	head.append(who);
	const sub = [];
	if (m.classes) sub.push(m.classes);
	if (m.ac) sub.push(`AC ${m.ac}`);
	if (sub.length) head.append(el("span", { class: "strip-sub", text: sub.join(" · ") }));
	strip.append(head);

	// The headline: exact numbers or the table's word. Unstructured
	// sheets say so plainly rather than inventing zeros.
	const vitals = el("div", { class: "strip-vitals" });
	if (m.hp != null && m.max_hp != null) {
		const bar = el("span", { class: "strip-bar" });
		const frac = m.max_hp > 0 ? Math.max(0, Math.min(1, m.hp / m.max_hp)) : 0;
		bar.append(el("i", { class: "strip-bar-fill", attrs: { style: `width:${Math.round(frac * 100)}%` } }));
		if (m.temp_hp) bar.append(el("i", {
			class: "strip-bar-temp",
			attrs: { style: `width:${Math.round(Math.min(1, m.temp_hp / m.max_hp) * 100)}%` },
			title: `${m.temp_hp} temp hp`,
		}));
		vitals.append(bar);
		vitals.append(el("b", {
			class: "strip-hp" + (m.hp === 0 ? " is-zero" : (m.max_hp > 0 && m.hp * 2 <= m.max_hp ? " is-low" : "")),
			text: `${m.hp}`,
		}));
		vitals.append(el("span", { class: "strip-max", text: `/ ${m.max_hp}${m.temp_hp ? ` (+${m.temp_hp})` : ""}` }));
	} else if (m.health) {
		vitals.append(el("b", {
			class: "strip-word" + (m.health === "down" || m.health === "dead" ? " is-low" : (m.health === "bloodied" ? " is-warn" : "")),
			text: m.health,
		}));
	} else if (m.unstructured) {
		vitals.append(el("span", { class: "strip-sub", text: "no structured sheet" }));
	}
	strip.append(vitals);

	const chips = el("div", { class: "strip-chips" });
	if (m.concentrating) {
		chips.append(el("span", { class: "strip-chip is-conc", title: "holding concentration", text: `⟡ ${m.concentrating}` }));
	}
	for (const c of m.conditions || []) {
		chips.append(el("span", {
			class: "strip-chip is-cond",
			title: c.display || "",
			text: c.name,
		}));
	}
	for (const w of m.warnings || []) {
		chips.append(el("span", { class: "strip-chip is-warn", text: w }));
	}
	if (chips.childElementCount) strip.append(chips);

	if (m.slots && m.slots.length) {
		const slots = el("div", { class: "strip-slots" });
		for (const s of m.slots) {
			const ord = `${s.level}${s.level === 1 ? "st" : s.level === 2 ? "nd" : s.level === 3 ? "rd" : "th"}`;
			const pips = el("span", { class: "strip-pips", title: `${ord}-level slots` });
			for (let i = 0; i < s.max; i++) {
				pips.append(el("i", { class: "strip-pip" + (i < s.left ? " is-full" : "") }));
			}
			slots.append(pips);
		}
		strip.append(slots);
	}
	return strip;
}

function renderMonsters(board) {
	const side = $("board-monsters");
	const rows = clear($("board-monster-rows"));
	if (!board.dm || !board.monsters || !board.monsters.length) {
		side.hidden = true;
		return;
	}
	side.hidden = false;
	for (const mo of board.monsters) {
		const row = el("div", { class: "strip is-foe-strip" + (mo.dead ? " is-dead" : "") });
		const head = el("div", { class: "strip-head" });
		head.append(el("span", { class: "strip-name" }, el("b", { text: mo.name })));
		if (mo.ac) head.append(el("span", { class: "strip-sub", text: `AC ${mo.ac}` }));
		row.append(head);
		const vitals = el("div", { class: "strip-vitals" });
		const bar = el("span", { class: "strip-bar" });
		const frac = mo.max_hp > 0 ? Math.max(0, Math.min(1, mo.hp / mo.max_hp)) : 0;
		bar.append(el("i", { class: "strip-bar-fill", attrs: { style: `width:${Math.round(frac * 100)}%` } }));
		vitals.append(bar);
		vitals.append(el("b", { class: "strip-hp" + (frac <= 0.5 ? " is-low" : ""), text: `${mo.hp}` }));
		vitals.append(el("span", { class: "strip-max", text: `/ ${mo.max_hp}${mo.temp_hp ? ` (+${mo.temp_hp})` : ""}` }));
		row.append(vitals);
		const chips = el("div", { class: "strip-chips" });
		for (const c of mo.conditions || []) chips.append(el("span", { class: "strip-chip is-cond", text: c.name }));
		if (chips.childElementCount) row.append(chips);
		rows.append(row);
	}
}

function renderSettings(board) {
	const form = $("board-settings");
	if (!board.dm) {
		form.hidden = true;
		return;
	}
	form.hidden = false;
	$("board-set-hp").value = (board.config && board.config.hp) || "exact";
	$("board-set-slots").value = (board.config && board.config.slots) || "visible";
}

function renderPresence(board) {
	const p = $("board-presence");
	const names = board.present || [];
	p.textContent = names.length
		? `At the table — ${names.join(", ")}`
		: "No one else is connected.";
}

/* ---------- wiring ---------- */

function wire() {
	$("board-campaign").addEventListener("change", async () => {
		const id = $("board-campaign").value;
		if (!id || id === campaignID) return;
		campaignID = id;
		localStorage.setItem("grimoire-board-campaign", id);
		await loadBoard();
	});
	$("board-settings").addEventListener("submit", async (e) => {
		e.preventDefault();
		try {
			await api.boardSettingsPut(campaignID, {
				hp: $("board-set-hp").value,
				slots: $("board-set-slots").value,
			});
			$("board-set-note").textContent = "saved — the table re-renders live";
			setTimeout(() => ($("board-set-note").textContent = ""), 2500);
		} catch (err) {
			$("board-set-note").textContent = err.message;
		}
	});
}

async function loadCampaigns() {
	let data;
	try {
		data = await api.campaignList();
	} catch (err) {
		$("board-meta").textContent = err.message;
		return;
	}
	campaigns = data.campaigns || [];
	const sel = clear($("board-campaign"));
	if (!campaigns.length) {
		sel.append(el("option", { text: "No campaigns yet…", attrs: { value: "" } }));
		$("board-body").hidden = true;
		$("board-empty").hidden = false;
		return;
	}
	$("board-body").hidden = false;
	$("board-empty").hidden = true;
	for (const c of campaigns) sel.append(el("option", { text: c.name, attrs: { value: c.id } }));
	const stored = localStorage.getItem("grimoire-board-campaign");
	if (campaigns.some((c) => c.id === stored)) campaignID = stored;
	else if (!campaigns.some((c) => c.id === campaignID)) campaignID = campaigns[0].id;
	sel.value = campaignID;
	await loadBoard();
}

async function loadBoard() {
	if (!campaignID) return;
	let data;
	try {
		data = await api.boardSnapshot(campaignID);
	} catch (err) {
		$("board-meta").textContent = err.message;
		return;
	}
	paint(data.board);
	startStream();
}

/* ---------- boot: presence follows the campaign without this window ---------- */

// initBoard runs at app boot (app.js): it picks the campaign the board
// window last used (or the first) and opens the stream, so the user
// counts as at the table from the moment the app opens — presence is
// having the book open, not having the right page turned to.
export async function initBoard() {
	try {
		const data = await api.campaignList();
		const list = data.campaigns || [];
		if (!list.length) return;
		const stored = localStorage.getItem("grimoire-board-campaign");
		campaignID = list.some((c) => c.id === stored) ? stored : list[0].id;
		startStream();
	} catch (_) { /* logged out or offline — the window will sort itself out */ }
}

/* ---------- the window-manager contract ---------- */

export const tool = {
	mount(host) {
		const view = $("board-view");
		host.append(view);
		view.hidden = false;
		mounted = true;
		if (!wired) {
			wire();
			wired = true;
		}
		if (!campaigns.length) loadCampaigns();
		else {
			const sel = $("board-campaign");
			if (sel.value !== campaignID) sel.value = campaignID;
			if (!streamCtl) loadBoard();
			else if (lastBoard) paint(lastBoard);
		}
		return {
			destroy() {
				mounted = false;
				// The stream stays open: presence is being at the table,
				// window or not — and reopening paints from lastBoard
				// instantly, the stream correcting a moment later.
			},
		};
	},
};
