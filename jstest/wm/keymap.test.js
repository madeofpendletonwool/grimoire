// Tests for key binding resolution. Run with: node --test "jstest/**/*.test.js"
//
// The cases that matter are the ones the old shell got wrong: which layer wins
// Escape, and whether a binding means the same thing on a Mac as on Linux.

import test from "node:test";
import assert from "node:assert/strict";

import {
	LAYERS, normalize, parseSpec, isReserved, describe, createKeymap,
} from "../../web/static/js/wm/keymap.js";

const ev = (key, mods = {}) => ({
	key,
	ctrlKey: !!mods.ctrl, metaKey: !!mods.meta, altKey: !!mods.alt, shiftKey: !!mods.shift,
});

/* ---------- normalize ---------- */

test("normalize is case- and order-insensitive", () => {
	assert.equal(normalize("Shift+Mod+K"), normalize("mod+shift+k"));
	assert.equal(normalize("MOD+K"), "mod+k");
});

test("normalize folds every spelling of the primary modifier", () => {
	for (const spec of ["mod+k", "ctrl+k", "Control+K", "cmd+k", "meta+k"]) {
		assert.equal(normalize(spec), "mod+k", spec);
	}
});

test("normalize expands the common key aliases", () => {
	assert.equal(normalize("esc"), "escape");
	assert.equal(normalize("Return"), "enter");
	assert.equal(normalize("mod+space"), "mod+ ");
});

test("normalize keeps a stable modifier order", () => {
	assert.equal(normalize("shift+alt+mod+arrowleft"), "mod+alt+shift+arrowleft");
});

test("normalize rejects nonsense", () => {
	for (const bad of ["", "   ", "mod+", "mod+a+b", "+"]) {
		assert.throws(() => normalize(bad), `accepted ${JSON.stringify(bad)}`);
	}
});

/* ---------- chords ---------- */

test("parseSpec splits a chord into its steps", () => {
	assert.deepEqual(parseSpec("mod+g w"), ["mod+g", "w"]);
	assert.deepEqual(parseSpec("mod+k"), ["mod+k"]);
});

test("parseSpec refuses more than two steps", () => {
	assert.throws(() => parseSpec("mod+g w x"));
});

/* ---------- reserved keys ---------- */

test("the browser's own shortcuts are flagged reserved", () => {
	for (const spec of ["mod+w", "mod+t", "mod+1", "mod+9", "mod+tab", "cmd+W"]) {
		assert.ok(isReserved(spec), `${spec} should be reserved`);
	}
});

test("the bindings the window manager actually uses are not reserved", () => {
	for (const spec of ["mod+k", "alt+1", "alt+w", "alt+arrowleft", "mod+shift+p", "escape", "?"]) {
		assert.ok(!isReserved(spec), `${spec} should be available`);
	}
});

test("adding a reserved binding fails loudly rather than silently never firing", () => {
	const km = createKeymap();
	assert.throws(() => km.add("global", "mod+w", () => {}), /reserved/);
});

test("a reserved combination is still allowed as a chord continuation", () => {
	// The leader has already been swallowed, so the second key is ours.
	const km = createKeymap();
	assert.doesNotThrow(() => km.add("global", "mod+g w", () => {}));
});

/* ---------- describe ---------- */

test("describe names a keystroke the way a spec is written", () => {
	assert.equal(describe(ev("k", { ctrl: true })), "mod+k");
	assert.equal(describe(ev("ArrowLeft", { alt: true })), "alt+arrowleft");
	assert.equal(describe(ev("Escape")), "escape");
});

test("mod is Ctrl off a Mac and Cmd on one", () => {
	assert.equal(describe(ev("k", { ctrl: true }), false), "mod+k");
	assert.equal(describe(ev("k", { meta: true }), false), "k",
		"Cmd is not the primary modifier off a Mac");

	assert.equal(describe(ev("k", { meta: true }), true), "mod+k");
	assert.equal(describe(ev("k", { ctrl: true }), true), "k",
		"Ctrl is not the primary modifier on a Mac");
});

test("describe ignores shift for keys shift already transforms", () => {
	// The browser reports "@" for shift+2; demanding "shift+@" would never match.
	assert.equal(describe(ev("@", { shift: true })), "@");
	assert.equal(describe(ev("?", { shift: true })), "?");
});

test("describe keeps shift for named keys", () => {
	assert.equal(describe(ev("ArrowLeft", { alt: true, shift: true })), "alt+shift+arrowleft");
	assert.equal(describe(ev(" ", { alt: true, shift: true })), "alt+shift+ ");
});

test("a described keystroke matches the normalized spec for it", () => {
	const pairs = [
		["mod+k", ev("k", { ctrl: true })],
		["alt+shift+arrowright", ev("ArrowRight", { alt: true, shift: true })],
		["escape", ev("Escape")],
		["alt+1", ev("1", { alt: true })],
	];
	for (const [spec, event] of pairs) {
		assert.equal(describe(event), normalize(spec), spec);
	}
});

/* ---------- resolution & layers ---------- */

test("a binding resolves to its handler", () => {
	const km = createKeymap();
	const h = () => "hit";
	km.add("global", "mod+k", h);
	const got = km.resolve("mod+k");
	assert.equal(got.kind, "action");
	assert.equal(got.handler, h);
});

test("an unbound key resolves to nothing", () => {
	const km = createKeymap();
	km.add("global", "mod+k", () => {});
	assert.equal(km.resolve("mod+j"), null);
});

// The bug this whole module exists to fix: three modules bound Escape and the
// winner was decided by import order in app.js.
test("the highest active layer wins a contested key", () => {
	const km = createKeymap();
	km.add("global", "escape", () => "global");
	km.add("window", "escape", () => "window");
	km.add("modal", "escape", () => "modal");

	assert.equal(km.resolve("escape", LAYERS).handler(), "modal");
	assert.equal(km.resolve("escape", ["window", "global"]).handler(), "window");
	assert.equal(km.resolve("escape", ["global"]).handler(), "global");
});

test("a layer that is not active is not consulted", () => {
	const km = createKeymap();
	km.add("modal", "escape", () => "modal");
	assert.equal(km.resolve("escape", ["window", "global"]), null);
});

test("add rejects an unknown layer", () => {
	const km = createKeymap();
	assert.throws(() => km.add("floating", "mod+k", () => {}), /unknown layer/);
});

/* ---------- chord resolution ---------- */

test("a leader resolves to a prefix, then its continuation resolves to the action", () => {
	const km = createKeymap();
	km.add("global", "mod+g w", () => "close");

	const lead = km.resolve("mod+g");
	assert.equal(lead.kind, "prefix");
	assert.equal(lead.key, "mod+g");

	const done = km.resolve("w", LAYERS, "mod+g");
	assert.equal(done.kind, "action");
	assert.equal(done.handler(), "close");
});

test("one leader carries many continuations", () => {
	const km = createKeymap();
	km.add("global", "mod+g w", () => "close");
	km.add("global", "mod+g v", () => "split");

	assert.equal(km.resolve("w", LAYERS, "mod+g").handler(), "close");
	assert.equal(km.resolve("v", LAYERS, "mod+g").handler(), "split");
});

test("an unknown second key cancels instead of falling through", () => {
	const km = createKeymap();
	km.add("global", "mod+g w", () => "close");
	km.add("global", "k", () => "palette");

	const got = km.resolve("k", LAYERS, "mod+g");
	assert.equal(got.kind, "cancel", "the leader must not leak into a global binding");
});

test("a key can be both a leader and a plain binding on different layers", () => {
	const km = createKeymap();
	km.add("global", "mod+g w", () => "chord");
	km.add("modal", "mod+g", () => "modal");

	assert.equal(km.resolve("mod+g", LAYERS).handler(), "modal", "the modal layer wins");
	assert.equal(km.resolve("mod+g", ["global"]).kind, "prefix");
});

test("continuations lists what a leader offers, for the hint bar", () => {
	const km = createKeymap();
	km.add("global", "mod+g w", () => {}, { label: "Close window" });
	km.add("global", "mod+g v", () => {}, { label: "Split vertical" });
	km.add("global", "mod+k", () => {}, { label: "Palette" });

	const got = km.continuations("mod+g").map((m) => m.label).sort();
	assert.deepEqual(got, ["Close window", "Split vertical"]);
});

/* ---------- the cheat sheet ---------- */

test("list returns every binding with its spec, layer and metadata", () => {
	const km = createKeymap();
	km.add("global", "mod+k", () => {}, { label: "Palette", group: "Find" });
	km.add("window", "alt+w", () => {}, { label: "Close", group: "Windows" });

	const got = km.list();
	assert.equal(got.length, 2);
	assert.deepEqual(got.map((m) => m.label), ["Palette", "Close"]);
	assert.deepEqual(got.map((m) => m.layer), ["global", "window"]);
	assert.deepEqual(got.map((m) => m.spec), ["mod+k", "alt+w"]);
	assert.equal(got[0].group, "Find");
});

test("every listed binding is reachable by the keystroke it advertises", () => {
	const km = createKeymap();
	const specs = ["mod+k", "alt+w", "alt+shift+arrowleft", "escape", "mod+g v"];
	specs.forEach((s) => km.add("global", s, () => s));

	for (const meta of km.list()) {
		const steps = parseSpec(meta.spec);
		const got = steps.length === 1
			? km.resolve(steps[0])
			: km.resolve(steps[1], LAYERS, steps[0]);
		assert.equal(got?.kind, "action", `${meta.spec} is advertised but does not resolve`);
		assert.equal(got.handler(), meta.spec);
	}
});

/* ---------- the two ways a binding can be printed and still do nothing ---------- */

test("describe keeps shift for a letter, so mod+shift+p is reachable", () => {
	// The browser reports "P" for Ctrl+Shift+P. Lowercasing it dropped the
	// shift back out, and the descriptor came out "mod+p" — which nothing was
	// bound to, so the command menu had a shortcut that silently did nothing.
	assert.equal(describe(ev("P", { ctrl: true, shift: true })), normalize("mod+shift+p"));
	assert.equal(describe(ev("W", { shift: true })), normalize("shift+w"));
	assert.equal(describe(ev("w")), "w");
});

test("describe reads the physical key when the OS mangled the character", () => {
	// macOS treats Option as a dead-key modifier: Option+W arrives as "∑",
	// Option+1 as "¡". Without this every alt binding is unreachable there.
	const opt = (key, code) => ({ key, code, altKey: true, ctrlKey: false, metaKey: false, shiftKey: false });
	assert.equal(describe(opt("∑", "KeyW")), "alt+w");
	assert.equal(describe(opt("¡", "Digit1")), "alt+1");
	assert.equal(describe(opt("“", "BracketLeft")), "alt+[");
	// A plain ASCII character is always trusted over the code, so a Dvorak or
	// AZERTY keyboard still binds the key its cap actually shows.
	assert.equal(describe({ key: ",", code: "KeyW", altKey: true }), "alt+,");
});

test("a second binding on the same keystroke is a mistake, not a silent overwrite", () => {
	// "mod+g v" meant both "split right" and "Review": the registry's
	// accelerators were added last and won, while the sheet went on printing
	// the shortcut that no longer did anything.
	const km = createKeymap();
	km.add("global", "mod+g v", () => "split");
	assert.throws(() => km.add("global", "mod+g v", () => "review"), /already bound/);

	km.add("global", "alt+w", () => "close");
	assert.throws(() => km.add("global", "alt+w", () => "other"), /already bound/);

	// The same keystroke in a different layer is fine — that is what layers are.
	assert.doesNotThrow(() => km.add("window", "alt+w", () => "window close"));
});

test("a leader cannot also be bound on its own", () => {
	const km = createKeymap();
	km.add("global", "mod+g w", () => {});
	assert.throws(() => km.add("global", "mod+g", () => {}), /already bound/);

	const other = createKeymap();
	other.add("global", "mod+j", () => {});
	assert.throws(() => other.add("global", "mod+j k", () => {}), /cannot also lead/);
});
