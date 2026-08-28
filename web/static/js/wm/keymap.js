// Key binding resolution — pure, no DOM.
//
// The window manager needs one dispatcher because the shell had four
// competing `document` keydown listeners whose Escape precedence was decided
// by module init order (see the guards in drawer.js and palette.js). Ordering
// by accident is how Escape ends up closing the wrong thing.
//
// Three ideas here:
//
//   Layers    "modal" beats "window" beats "global", first match wins. A
//             floating dialog claims Escape without every other module having
//             to ask whether a dialog is open.
//
//   mod       The primary modifier: Cmd on a Mac, Ctrl everywhere else. Specs
//             are written "mod+k" once and read correctly on both.
//
//   Chords    A leader plus a key ("mod+g w"). Browsers reserve Ctrl+W,
//             Ctrl+T, Ctrl+N and Ctrl+1..9 and will not surrender them, so a
//             window manager that wants a full command set needs a prefix
//             the browser has no opinion about.

export const LAYERS = ["modal", "window", "global"];

/** Keys the browser keeps for itself; binding one is a silent no-op at best. */
const RESERVED = new Set([
	"mod+w", "mod+t", "mod+n", "mod+q", "mod+shift+w", "mod+shift+t", "mod+shift+n",
	"mod+1", "mod+2", "mod+3", "mod+4", "mod+5", "mod+6", "mod+7", "mod+8", "mod+9",
	"mod+tab", "mod+shift+tab",
]);

/**
 * Canonicalise one key spec: lowercase, modifiers in a fixed order, so
 * "Shift+Mod+K" and "mod+shift+k" are the same binding.
 */
export function normalize(spec) {
	const parts = String(spec).trim().toLowerCase().split("+").map((p) => p.trim()).filter(Boolean);
	if (parts.length === 0) throw new Error(`empty key spec: ${JSON.stringify(spec)}`);

	const mods = new Set();
	let key = null;
	for (const p of parts) {
		if (p === "mod" || p === "ctrl" || p === "control" || p === "cmd" || p === "meta") mods.add("mod");
		else if (p === "alt" || p === "option") mods.add("alt");
		else if (p === "shift") mods.add("shift");
		else if (key !== null) throw new Error(`two keys in one spec: ${spec}`);
		else key = p;
	}
	if (!key) throw new Error(`spec has modifiers but no key: ${spec}`);

	// Long names are written as they arrive from KeyboardEvent.key, so a
	// binding and an event describe the same thing.
	const alias = { esc: "escape", del: "delete", return: "enter", space: " ", spacebar: " " };
	key = alias[key] || key;

	return [...["mod", "alt", "shift"].filter((m) => mods.has(m)), key].join("+");
}

/** Split "mod+g w" into its steps. A single-step spec is a one-element array. */
export function parseSpec(spec) {
	const steps = String(spec).trim().split(/\s+/).filter(Boolean);
	if (steps.length === 0) throw new Error(`empty key spec: ${JSON.stringify(spec)}`);
	if (steps.length > 2) throw new Error(`chords are at most two steps: ${spec}`);
	return steps.map(normalize);
}

export const isReserved = (spec) => RESERVED.has(normalize(spec));

/**
 * Describe a keystroke the same way a spec is written.
 *
 * Takes a plain object rather than a KeyboardEvent so this stays testable:
 * `{ key, ctrlKey, metaKey, altKey, shiftKey }`. `mac` decides whether Cmd or
 * Ctrl counts as "mod"; the other one is then ignored, so Ctrl+K on a Mac does
 * not fire the Cmd+K binding.
 */
export function describe(ev, mac = false) {
	const primary = mac ? ev.metaKey : ev.ctrlKey;
	const mods = [];
	if (primary) mods.push("mod");
	if (ev.altKey) mods.push("alt");

	let key = String(ev.key || "").toLowerCase();
	// Shift is part of the identity only for keys it does not already
	// transform: shift+2 is "@" on its own, and demanding "shift+@" would
	// never match.
	if (ev.shiftKey && (key.length > 1 || key === " ")) mods.push("shift");

	if (key === "spacebar") key = " ";
	return [...mods, key].join("+");
}

/**
 * A keymap: bindings grouped by layer, resolved highest layer first.
 *
 * Handlers are opaque — this module decides *which* binding matched and never
 * calls anything, so the DOM half owns every side effect.
 */
export function createKeymap() {
	// layer -> Map<step, {handler, meta} | {chord: Map<step, entry>}>
	const layers = new Map(LAYERS.map((l) => [l, new Map()]));
	const all = [];

	function add(layer, spec, handler, meta = {}) {
		if (!layers.has(layer)) throw new Error(`unknown layer: ${layer}`);
		const steps = parseSpec(spec);
		if (steps.length === 1 && RESERVED.has(steps[0])) {
			throw new Error(`${spec} is reserved by the browser — use a chord or an alt binding`);
		}

		const table = layers.get(layer);
		const record = { handler, meta: { ...meta, spec, layer } };

		if (steps.length === 1) {
			table.set(steps[0], record);
		} else {
			const [lead, second] = steps;
			let entry = table.get(lead);
			if (!entry || !entry.chord) {
				entry = { chord: new Map() };
				table.set(lead, entry);
			}
			entry.chord.set(second, record);
		}
		all.push(record.meta);
		return record.meta;
	}

	/**
	 * Resolve one keystroke.
	 *
	 * `active` lists the layers currently in play, in priority order. Returns
	 * a chord prefix, a matched action, or null. `pending` is the leader that
	 * was pressed on the previous keystroke, if any.
	 */
	function resolve(descriptor, active = LAYERS, pending = null) {
		if (pending) {
			for (const layer of active) {
				const entry = layers.get(layer)?.get(pending);
				const hit = entry?.chord?.get(descriptor);
				if (hit) return { kind: "action", handler: hit.handler, meta: hit.meta };
			}
			// An unrecognised second key cancels rather than falling through to
			// a global binding — otherwise "mod+g" then "k" would open the
			// palette, which is not what the leader promised.
			return { kind: "cancel" };
		}

		for (const layer of active) {
			const entry = layers.get(layer)?.get(descriptor);
			if (!entry) continue;
			if (entry.chord) return { kind: "prefix", key: descriptor };
			return { kind: "action", handler: entry.handler, meta: entry.meta };
		}
		return null;
	}

	/** Every binding, for the cheat sheet. Grouped in insertion order. */
	const list = () => all.slice();

	/** The continuations of a leader, for the which-key hint bar. */
	function continuations(leader, active = LAYERS) {
		const out = [];
		for (const layer of active) {
			const entry = layers.get(layer)?.get(leader);
			if (entry?.chord) for (const rec of entry.chord.values()) out.push(rec.meta);
		}
		return out;
	}

	return { add, resolve, list, continuations };
}
