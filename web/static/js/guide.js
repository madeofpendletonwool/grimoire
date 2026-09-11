// The Guide: onboarding as a tool rather than an overlay.
//
// It is a window like every other tool, which is the whole design. A coach
// mark that walks you through a tiling shell has to fight the shell — tools
// mount on a later tick, windows move, a split invalidates every cached
// rectangle — and when it is done it is gone, so the thing you half-learned
// in week one is unavailable in week six. A window can sit beside the tool it
// is teaching, stays open as long as it is useful, and is reopened from the
// picker like anything else.
//
// The gate lives in guidevm.js and reads one server snapshot: a step is done
// because the act happened, never because a Next button was pressed. What is
// left here is paint and plumbing.

import { $, el, clear } from "./dom.js";
import { api } from "./api.js";
import { sprite, gi } from "./icons.js";
import { seatRole } from "./seat.js";
import { inSeat } from "./wm/registry.js";
import { openTool } from "./wm/wm.js";
import * as ws from "./wm/workspaces.js";
import * as VM from "./guidevm.js";
import { openTour } from "./tour.js";

let wired = false;
let mounted = false;
let abort = null;

let campaigns = [];
let campaignID = "";
let prefs = {};
let snap = VM.EMPTY;
let lastRead = 0;

// A floor under the refresh rate. Every trigger below is a deliberate act
// (a mount, a button, coming back to the tab), but they can coincide — an
// alt-tab that lands on a fresh mount would otherwise fire twice.
const MIN_REFRESH_MS = 3000;
const STALE_MS = 15000;

/* ---------- loading ---------- */

/** The campaign the Guide is reporting on: the picker's choice, else the
    first campaign the account stands in. */
function pickCampaign() {
	if (campaignID && campaigns.some((c) => c.id === campaignID)) return campaignID;
	campaignID = campaigns[0]?.id || "";
	return campaignID;
}

async function load() {
	abort?.abort();
	abort = new AbortController();
	const signal = abort.signal;
	try {
		const [list, stored] = await Promise.all([
			api.campaignList(signal).catch(() => null),
			api.uiPrefs(signal).catch(() => null),
		]);
		if (!mounted) return;
		if (list) campaigns = list.campaigns || [];
		if (stored) prefs = stored.prefs || {};
		await refresh({ force: true });
	} catch (err) {
		if (!signal.aborted) note(err.message || "The Guide could not read its state.", true);
	}
}

/**
 * Re-read the snapshot and repaint.
 *
 * Deliberately not on a timer and not on the window manager's change event:
 * that fires on every gutter drag and tab switch, which would turn resizing
 * a window into a request storm.
 */
async function refresh({ force = false } = {}) {
	if (!mounted) return;
	const now = Date.now();
	if (!force && now - lastRead < MIN_REFRESH_MS) return;
	lastRead = now;
	try {
		const body = await api.onboardingState(pickCampaign(), abort?.signal);
		if (!mounted) return;
		snap = VM.snapshot(body);
		render();
	} catch (err) {
		if (abort?.signal.aborted) return;
		// A snapshot we could not read is not a campaign with nothing in it:
		// say so, and leave the last good picture on screen.
		note(err.message || "Could not check your progress.", true);
	}
}

/* ---------- painting ---------- */

function note(message, warn = false) {
	const meta = $("guide-meta");
	meta.textContent = message || "";
	meta.classList.toggle("warn", !!warn);
}

function render() {
	const showFork = VM.needsFork({ authenticated: true }, campaigns, prefs);
	$("guide-fork").hidden = !showFork;
	$("guide-body").hidden = showFork;
	renderPicker();
	if (showFork) {
		note("");
		return;
	}

	const track = VM.trackFor(seatRole(), prefs[VM.TRACK_PREF], campaigns);
	const steps = VM.stepsFor(track, snap.role);
	const p = VM.progress(steps, snap);

	$("guide-title").textContent = track === VM.TRACK_DM ? "Running a table" : "At the table";
	note(VM.progressLine(p));

	const list = clear($("guide-steps"));
	p.rows.forEach((row, i) => list.append(stepCard(row, i, p)));

	$("guide-foot-note").textContent = p.complete
		? "Prep, play, then decide what became true — and around again."
		: "Done something? Check again.";
}

function renderPicker() {
	const select = $("guide-campaign");
	// Rebuilding the options on every render would fight the open dropdown;
	// only do it when the list actually changed.
	const signature = campaigns.map((c) => c.id).join("|");
	if (select.dataset.signature !== signature) {
		clear(select);
		if (!campaigns.length) {
			select.append(el("option", { text: "No campaigns yet…", attrs: { value: "" } }));
		}
		for (const c of campaigns) {
			select.append(el("option", { text: c.name, attrs: { value: c.id } }));
		}
		select.dataset.signature = signature;
	}
	select.value = pickCampaign();
	// One campaign is not a choice; the picker only earns its space past that.
	select.parentElement.hidden = campaigns.length < 2;
}

function stepCard(row, index, p) {
	const isNext = p.next && p.next.id === row.id;
	const card = el("li", {
		class: `guide-step${row.done ? " is-done" : ""}${isNext ? " is-next" : ""}`,
	});

	const mark = el("span", { class: "guide-step-mark", attrs: { "aria-hidden": "true" } });
	// The checkmark is the only thing on the card that claims anything, so it
	// says the same thing to a screen reader that it does to an eye.
	mark.append(row.done ? gi("check") : el("span", { text: String(index + 1) }));
	card.append(mark);

	const main = el("div", { class: "guide-step-main" });
	main.append(el("h3", { class: "guide-step-title", text: row.step.title }));
	main.append(el("p", {
		class: "guide-step-state",
		text: row.done ? "Done" : isNext ? "Next" : "Not yet",
	}));
	main.append(el("p", { class: "guide-step-blurb", text: row.step.blurb }));
	if (row.step.teaches) {
		main.append(el("p", { class: "guide-teaches" },
			sprite("candle"),
			el("span", { text: row.step.teaches }),
		));
	}
	const action = showAction(row.step);
	if (action) main.append(action);
	card.append(main);
	return card;
}

/**
 * The "Show me" button, or nothing.
 *
 * It opens or reveals a tool, or switches to a workspace — it never points
 * *inside* another tool. openTool resolves the tool's module on a later tick
 * and the shell offers no mounted event, so a mark aimed at another tool's
 * innards would be drawn over empty space as often as not. Opening the right
 * window and saying what to look for is honest about what the shell can
 * promise.
 */
function showAction(step) {
	const { tool, slot } = step.show || {};
	if (slot) {
		return el("button", {
			class: "enc-btn", attrs: { type: "button" },
			on: { click: () => ws.switchTo(slot) },
		}, el("span", { text: "Take me there" }));
	}
	if (!tool) return null;
	// The shell only opens what it offers (ADR 22). The Guide is a second
	// door onto every tool, so it restates the rule rather than assuming the
	// track table can never be wrong.
	if (!inSeat(tool, seatRole())) return null;
	return el("button", {
		class: "enc-btn", attrs: { type: "button" },
		on: { click: () => openTool(tool) },
	}, el("span", { text: "Show me" }));
}

/* ---------- the fork ---------- */

async function chooseTrack(track) {
	prefs = { ...prefs, [VM.TRACK_PREF]: track };
	render();
	try {
		await api.uiSavePrefs({ [VM.TRACK_PREF]: track });
	} catch (_) {
		// The answer still holds for this session; it is a preference, not a
		// fact, and refusing to continue over it would be absurd.
	}
	if (track === VM.TRACK_PLAYER) {
		$("guide-fork").hidden = false;
		$("guide-body").hidden = true;
		$("guide-join-form").hidden = false;
		$("guide-join-code").focus();
		forkNote("Paste the code your DM sent you.");
	}
}

function forkNote(message, warn = false) {
	const box = $("guide-fork-note");
	box.textContent = message || "";
	box.classList.toggle("warn", !!warn);
}

async function onJoin(event) {
	event.preventDefault();
	const code = $("guide-join-code").value.trim();
	if (!code) return;
	forkNote("Taking your seat…");
	try {
		await api.campaignJoin(code);
	} catch (err) {
		forkNote(err.message || "That code was not accepted.", true);
		return;
	}
	$("guide-join-code").value = "";
	forkNote("");
	await load();
}

/* ---------- wiring ---------- */

function wire() {
	$("guide-campaign").addEventListener("change", (e) => {
		campaignID = e.target.value;
		refresh({ force: true });
	});
	$("guide-recheck").addEventListener("click", () => refresh({ force: true }));
	// The chrome the Guide cannot teach from inside a window — workspaces,
	// the tool picker, the window menu — has its own five-step pass.
	$("guide-tour").addEventListener("click", () => openTour());
	$("guide-fork-dm").addEventListener("click", () => chooseTrack(VM.TRACK_DM));
	$("guide-fork-player").addEventListener("click", () => chooseTrack(VM.TRACK_PLAYER));
	$("guide-join-form").addEventListener("submit", onJoin);

	// Coming back to the tab is the one moment worth re-reading without being
	// asked: the usual reason to leave is to go and do the step.
	document.addEventListener("visibilitychange", () => {
		if (!mounted || document.hidden) return;
		if (Date.now() - lastRead > STALE_MS) refresh();
	});
}

export const tool = {
	mount(host) {
		const view = $("guide-view");
		host.append(view);
		view.hidden = false;
		mounted = true;
		if (!wired) {
			wire();
			wired = true;
		}
		load();
		return {
			destroy() {
				mounted = false;
				abort?.abort();
				abort = null;
			},
		};
	},
};
