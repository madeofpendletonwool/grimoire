// The table screen's painter (MAD-425, stage 8 of MAD-417): the room's
// public page, live. One EventSource braids the whole screen — the open
// frame paints everything, board events repaint the strips and the
// battle, roll events land the dice big. Nothing here hides anything:
// the server already decided what the room may see, so nothing hidden
// ever arrives.
//
// A projector's job is to stay up. The connection retries forever with
// a capped backoff — there is no window to reopen — and a gone event
// (the keeper closed the screen) is the one thing that stops it.

import { $, el, clear } from "./dom.js";

const token = document.body.dataset.token;
let rolls = [];
let closed = false;

const maxRolls = 6;

function connect() {
	if (closed || !token) return;
	const es = new EventSource(`/t/${encodeURIComponent(token)}/stream`);
	es.addEventListener("open", (ev) => {
		if (!ev.data) return; // the connection's own open, not the frame
		let payload;
		try { payload = JSON.parse(ev.data); } catch (_) { return; }
		if (payload.table) paintAll(payload.table);
	});
	es.addEventListener("board", (ev) => {
		let payload;
		try { payload = JSON.parse(ev.data); } catch (_) { return; }
		if (payload.table) paintBoard(payload.table);
	});
	es.addEventListener("roll", (ev) => {
		let payload;
		try { payload = JSON.parse(ev.data); } catch (_) { return; }
		if (payload.roll) addRoll(payload.roll);
	});
	es.addEventListener("gone", () => {
		closed = true;
		es.close();
		peace("The keeper closed this screen.");
	});
	es.onerror = () => {
		es.close();
		if (closed) return;
		setTimeout(connect, 2500);
	};
}

/* ---------- painting ---------- */

function paintAll(table) {
	$("table-campaign").textContent = table.campaign || "";
	paintBoard(table);
	rolls = (table.rolls || []).slice(-maxRolls);
	paintRolls();
}

function paintBoard(table) {
	paintBanner(table.combat || null);
	paintParty(table.members || []);
	paintFoes(table.monsters || []);
}

function peace(text) {
	const p = $("table-peace");
	if (text) p.textContent = text;
	p.hidden = false;
}

function paintBanner(combat) {
	const banner = $("table-banner");
	if (!combat) {
		banner.hidden = true;
		$("table-peace").hidden = false;
		return;
	}
	$("table-peace").hidden = true;
	banner.hidden = false;
	clear(banner);
	const line = el("div", { class: "table-turn-line" });
	line.append(el("b", { class: "table-round", text: `Round ${combat.round}` }));
	line.append(el("span", { class: "table-turn", text: combat.turn || "the fight begins" }));
	banner.append(line);
	const order = el("ol", { class: "table-order" });
	for (const entry of combat.order || []) {
		const li = el("li", {
			class: "table-order-entry" +
				(entry.active ? " is-active" : "") +
				(entry.dead ? " is-dead" : "") +
				(entry.pc ? "" : " is-foe"),
		});
		li.append(el("span", { class: "table-order-init", text: String(entry.initiative) }));
		li.append(el("span", { class: "table-order-name", text: entry.name }));
		if (entry.down) li.append(el("i", { class: "table-order-mark", text: "down" }));
		order.append(li);
	}
	banner.append(order);
}

function paintParty(members) {
	const host = clear($("table-party"));
	for (const m of members) host.append(stripEl(m));
	if (!host.childElementCount) {
		host.append(el("p", { class: "table-hint", text: "No one is bound to a character yet." }));
	}
}

function stripEl(m) {
	const strip = el("article", {
		class: "tstrip" + (m.down ? " is-down" : "") + (m.dead ? " is-dead" : ""),
	});
	strip.append(el("h3", { class: "tstrip-name", text: m.name }));

	const vitals = el("div", { class: "tstrip-vitals" });
	if (m.hp != null && m.max_hp != null) {
		const bar = el("span", { class: "tstrip-bar" });
		const frac = m.max_hp > 0 ? Math.max(0, Math.min(1, m.hp / m.max_hp)) : 0;
		bar.append(el("i", {
			class: "tstrip-bar-fill" + (frac <= 0.5 ? " is-low" : "") + (frac <= 0 ? " is-zero" : ""),
			attrs: { style: `width:${Math.round(frac * 100)}%` },
		}));
		if (m.temp_hp) {
			bar.append(el("i", {
				class: "tstrip-bar-temp",
				attrs: { style: `width:${Math.round(Math.min(1, m.temp_hp / m.max_hp) * 100)}%` },
				title: `${m.temp_hp} temp hp`,
			}));
		}
		vitals.append(bar);
		vitals.append(el("b", {
			class: "tstrip-hp" + (m.hp === 0 ? " is-zero" : (m.max_hp > 0 && m.hp * 2 <= m.max_hp ? " is-low" : "")),
			text: `${m.hp}`,
		}));
		vitals.append(el("span", { class: "tstrip-max", text: `/ ${m.max_hp}` }));
	} else if (m.health) {
		vitals.append(el("b", {
			class: "tstrip-word" + (m.health === "down" || m.health === "dead" ? " is-zero" : (m.health === "bloodied" ? " is-low" : "")),
			text: m.health,
		}));
	}
	strip.append(vitals);

	const chips = el("div", { class: "tstrip-chips" });
	if (m.concentrating) chips.append(el("span", { class: "tchip is-conc", text: `⟡ ${m.concentrating}` }));
	for (const c of m.conditions || []) chips.append(el("span", { class: "tchip", text: c.name }));
	for (const w of m.warnings || []) chips.append(el("span", { class: "tchip is-warn", text: w }));
	if (chips.childElementCount) strip.append(chips);
	return strip;
}

function paintFoes(monsters) {
	const side = $("table-foes");
	if (!monsters.length) {
		side.hidden = true;
		return;
	}
	side.hidden = false;
	clear(side);
	side.append(el("h2", { class: "table-side-title", text: "The other side" }));
	for (const mo of monsters) {
		const row = el("article", { class: "tstrip is-foe" + (mo.dead ? " is-dead" : "") });
		row.append(el("h3", { class: "tstrip-name", text: mo.name }));
		const vitals = el("div", { class: "tstrip-vitals" });
		if (mo.hp != null && mo.max_hp != null) {
			const bar = el("span", { class: "tstrip-bar" });
			const frac = mo.max_hp > 0 ? Math.max(0, Math.min(1, mo.hp / mo.max_hp)) : 0;
			bar.append(el("i", {
				class: "tstrip-bar-fill" + (frac <= 0.5 ? " is-low" : "") + (frac <= 0 ? " is-zero" : ""),
				attrs: { style: `width:${Math.round(frac * 100)}%` },
			}));
			vitals.append(bar);
			vitals.append(el("b", {
				class: "tstrip-hp" + (frac <= 0.5 ? " is-low" : "") + (frac <= 0 ? " is-zero" : ""),
				text: `${mo.hp}`,
			}));
			vitals.append(el("span", { class: "tstrip-max", text: `/ ${mo.max_hp}` }));
		} else if (mo.health) {
			vitals.append(el("b", {
				class: "tstrip-word" + (mo.health === "down" || mo.health === "dead" ? " is-zero" : (mo.health === "bloodied" ? " is-low" : "")),
				text: mo.health,
			}));
		}
		row.append(vitals);
		side.append(row);
	}
}

function addRoll(roll) {
	rolls.push(roll);
	if (rolls.length > maxRolls) rolls = rolls.slice(-maxRolls);
	paintRolls();
}

function paintRolls() {
	const host = clear($("table-dice"));
	if (!rolls.length) return;
	host.append(el("h2", { class: "table-side-title", text: "The dice" }));
	const list = el("ol", { class: "table-rolls" });
	for (const r of rolls) {
		const li = el("li", { class: "troll" + (r.natural_20 ? " is-crit" : "") + (r.natural_1 ? " is-fumble" : "") });
		const who = r.character_name || r.actor_name || "someone";
		const head = el("span", { class: "troll-who", text: who });
		if (r.detail) head.title = r.detail;
		li.append(head);
		li.append(el("span", { class: "troll-notation", text: r.notation || "" }));
		li.append(el("b", { class: "troll-total", text: String(r.total) }));
		list.append(li);
	}
	host.append(list);
}

connect();
