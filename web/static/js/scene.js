// The chamber the app sits in: a layered pixel backdrop that drifts with the
// pointer, and the small settings popup that chooses it.
//
// The art is a set of 384x216 parallax layers, back to front. They are scaled
// only by whole numbers and blended for luminosity, so what reaches the screen
// is the scene's shape lit in Grimoire's own colours rather than a picture
// competing with the text on top of it.

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

/** How far the nearest layer travels, corner to corner, in pixels. */
const DRIFT = 14;

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

/** Layers are 216px tall; scale to the next whole multiple that covers us. */
function rescale() {
	const root = $("scene");
	if (root) root.style.setProperty("--scene-scale", Math.max(3, Math.ceil(window.innerHeight / 216)));
}

function build(name) {
	const root = $("scene");
	if (!root) return;

	clear(root);
	state.layers = [];
	state.scene = name;

	const scene = SCENES[name];
	if (scene) {
		for (let i = 0; i < scene.layers; i++) {
			const layer = el("div", { class: "scene-layer" });
			layer.style.backgroundImage = `url("/static/assets/scenes/${name}/${i}.png")`;
			// Back layers barely move; the foreground carries the parallax.
			layer.dataset.depth = String(i / Math.max(1, scene.layers - 1));
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
		const y = state.target.y * DRIFT * depth * 0.45;
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

	const scenes = el("div", { class: "set-group", attrs: { role: "radiogroup", "aria-label": "Backdrop" } });
	scenes.append(el("p", { class: "set-label", text: "Backdrop" }));
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
