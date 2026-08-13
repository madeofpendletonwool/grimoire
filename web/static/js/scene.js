// The chamber the app sits in: a layered pixel backdrop that drifts with the
// pointer, and the small settings popup that chooses it.
//
// Picking a scene picks a whole theme, not just a picture. Each one's hue was
// measured from its own art at build time and applied to the chrome's stone
// ramp and nine-slice frames — see scripts/build-assets.py and themes.css. The
// parchment, the ink and the gold never change: the room changes, the tome
// does not.
//
// The art is a set of 384x216 parallax layers, scaled by whole numbers only so
// the pixels stay square.

import { $, el, clear } from "./dom.js";
import { gi, sprite } from "./icons.js";

const SCENE_KEY = "grimoire:scene";
const CURSOR_KEY = "grimoire:pixel-cursor";

/** Layer counts are what `scripts/build-assets.py` copied out of each pack. */
export const SCENES = Object.freeze({
	cave: { label: "Cavern", layers: 8, note: "Deep and blue-lit" },
	deadforest: { label: "Dead forest", layers: 7, note: "Bare trunks, low sun" },
	snowy: { label: "Snowy peaks", layers: 6, note: "Cold and far off" },
	plains: { label: "Plains", layers: 9, note: "Open sky, brightest" },
});
const DEFAULT_SCENE = "cave";

/**
 * How far the nearest layer travels, corner to corner, in pixels. Generous on
 * purpose: at a dozen pixels the effect is invisible, which reads as a static
 * image rather than as restraint.
 */
const DRIFT = 52;

/**
 * Depth is not linear. Raising it to a power keeps the distant layers almost
 * still while letting the foreground sweep, which is what actually reads as
 * distance — an even spread just looks like the whole picture sliding.
 */
const DEPTH_CURVE = 2.2;

const state = {
	scene: DEFAULT_SCENE,
	layers: [],
	target: { x: 0, y: 0 },
	frame: 0,
};

const reduced = () => window.matchMedia("(prefers-reduced-motion: reduce)").matches;

function stored(key, fallback) {
	try {
		return localStorage.getItem(key) ?? fallback;
	} catch {
		return fallback;
	}
}

function remember(key, value) {
	try {
		localStorage.setItem(key, value);
	} catch {
		/* private browsing — the choice just will not survive the reload */
	}
}

/* ---------- the backdrop ---------- */

/**
 * The art is 384x216 and has to cover the viewport. Anything smaller leaves a
 * band of flat colour where the sky should be, and that band reads as a seam,
 * not as distance — so the zoom is whatever whole number reaches the window's
 * height. Whole numbers only: a fractional scale resamples the pixels. Width
 * takes care of itself, since nearly every layer tiles and repeat-x covers
 * whatever the art does not reach.
 */
function rescale() {
	const root = $("scene");
	if (!root) return;
	const cover = Math.ceil(window.innerHeight / 216);
	root.style.setProperty("--scene-scale", Math.min(6, Math.max(3, cover)));
}

function build(name) {
	const root = $("scene");
	if (!root) return;

	clear(root);
	state.layers = [];
	state.scene = name;
	// Drives themes.css: the stone ramp, the frame sprites and the sky colour
	// all key off this one attribute.
	document.documentElement.dataset.scene = name;

	const scene = SCENES[name];
	if (scene) {
		root.append(el("div", { class: "scene-sky" }));
		// The packs number their layers front to back: 0 is the foreground and
		// the highest index is the sky. So they are appended in reverse, back
		// first, and 0 ends up painting on top — stacking them the other way
		// buries the whole scene under its own sky.
		for (let i = scene.layers - 1; i >= 0; i--) {
			const layer = el("div", { class: "scene-layer" });
			layer.style.backgroundImage = `url("/static/assets/scenes/${name}/${i}.png")`;
			// Depth 1 is the foreground and travels furthest; 0 is the sky and
			// holds still.
			const depth = 1 - i / Math.max(1, scene.layers - 1);
			layer.dataset.depth = String(Math.pow(depth, DEPTH_CURVE));
			root.append(layer);
			state.layers.push(layer);
		}
	}
	root.append(el("div", { class: "scene-veil" }), el("div", { class: "scene-glow" }));
	rescale();
	apply();
}

function apply() {
	state.frame = 0;
	for (const layer of state.layers) {
		const depth = Number(layer.dataset.depth);
		const x = state.target.x * DRIFT * depth;
		const y = state.target.y * DRIFT * depth * 0.4;
		layer.style.transform = `translate3d(${x.toFixed(2)}px, ${y.toFixed(2)}px, 0)`;
	}
}

function onPointer(e) {
	// -1..1 from the centre of the viewport, inverted so the scene falls away
	// from the cursor the way a real depth of field would.
	state.target.x = -((e.clientX / window.innerWidth) * 2 - 1);
	state.target.y = -((e.clientY / window.innerHeight) * 2 - 1);
	if (!state.frame) state.frame = requestAnimationFrame(apply);
}

/* ---------- the settings popup ---------- */

const CREDITS = [
	["Book UI", "Crusenho Agus Hennihuno", "https://crusenho.itch.io/complete-ui-book-styles-pack"],
	["Icons", "Shikashi", "https://cheekyinkling.itch.io/shikashis-fantasy-icons-pack"],
	["Backdrops", "Admurin", "https://admurin.itch.io"],
	["Vector icons", "Lorc & Delapouite, CC BY 3.0", "https://game-icons.net"],
	["Mana symbols", "Andrew Gioia", "https://mana.andrewgioia.com"],
];

function sceneOption(key, current, onPick) {
	const scene = SCENES[key];
	const label = scene ? scene.label : "Bare stone";
	const note = scene ? scene.note : "No backdrop at all";
	const btn = el("button", {
		class: "set-opt" + (key === current ? " is-active" : ""),
		attrs: { type: "button", role: "radio", "aria-checked": key === current },
		on: { click: () => onPick(key) },
	},
		el("span", { class: "set-opt-name", text: label }),
		el("span", { class: "set-opt-note", text: note }),
	);
	return btn;
}

function buildPopup() {
	const popup = $("settings-popup");
	if (!popup) return;
	clear(popup);

	popup.append(el("h2", { class: "set-title", text: "Settings" }));

	const scenes = el("div", { class: "set-group", attrs: { role: "radiogroup", "aria-label": "Theme" } });
	scenes.append(el("p", { class: "set-label", text: "Theme" }));
	const pick = (key) => {
		remember(SCENE_KEY, key);
		build(key);
		buildPopup();
	};
	for (const key of [...Object.keys(SCENES), "none"]) {
		scenes.append(sceneOption(key, state.scene, pick));
	}
	popup.append(scenes);

	const cursorOn = stored(CURSOR_KEY, "off") === "on";
	popup.append(el("div", { class: "set-group" },
		el("p", { class: "set-label", text: "Pointer" }),
		el("button", {
			class: "set-toggle" + (cursorOn ? " is-on" : ""),
			attrs: { type: "button", role: "switch", "aria-checked": cursorOn },
			on: {
				click: () => {
					setCursor(!cursorOn);
					buildPopup();
				},
			},
		},
			el("span", { class: "set-opt-name", text: "Pixel pointer" }),
			el("span", { class: "set-switch", attrs: { "aria-hidden": "true" } }),
		),
	));

	const credits = el("div", { class: "set-group set-credits" });
	credits.append(el("p", { class: "set-label", text: "Art by" }));
	for (const [what, who, href] of CREDITS) {
		credits.append(el("p", { class: "set-credit" },
			el("span", { class: "set-credit-what", text: what }),
			el("a", { text: who, attrs: { href, target: "_blank", rel: "noopener" } }),
		));
	}
	popup.append(credits);
}

function setCursor(on) {
	remember(CURSOR_KEY, on ? "on" : "off");
	document.documentElement.classList.toggle("pixel-cursor", on);
}

function togglePopup(open) {
	const popup = $("settings-popup");
	const btn = $("rail-settings");
	if (!popup || !btn) return;
	const next = open ?? popup.hidden;
	popup.hidden = !next;
	btn.setAttribute("aria-expanded", String(next));
}

/* ---------- wiring ---------- */

/** Mount the backdrop. Safe to call on any page that has a #scene element. */
export function initScene() {
	build(stored(SCENE_KEY, DEFAULT_SCENE));
	setCursor(stored(CURSOR_KEY, "off") === "on");

	window.addEventListener("resize", rescale);
	if (!reduced() && window.matchMedia("(hover: hover)").matches) {
		window.addEventListener("pointermove", onPointer, { passive: true });
	}
}

/** Mount the settings popup. Only the app shell has one; the gate does not. */
export function initSettings() {
	const btn = $("rail-settings");
	if (!btn) return;

	btn.replaceChildren(gi("settings"), el("span", { text: "Settings" }));
	// Build once up front rather than on first open: the popup then has content
	// whenever it is shown, and opening it costs no layout.
	buildPopup();
	btn.addEventListener("click", (e) => {
		e.stopPropagation();
		togglePopup();
	});

	document.addEventListener("click", (e) => {
		const popup = $("settings-popup");
		if (popup && !popup.hidden && !popup.contains(e.target)) togglePopup(false);
	});
	document.addEventListener("keydown", (e) => {
		if (e.key === "Escape") togglePopup(false);
	});
}

/** The mark shown beside a scene name, reused by the welcome screen. */
export function sceneMark() {
	return sprite(state.scene === "none" ? "candle" : "lantern");
}
