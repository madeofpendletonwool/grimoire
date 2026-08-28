// Dragging the gutters between panes.
//
// Pointer events rather than mouse events, so a pen or a touch drag works
// without a second code path, and setPointerCapture so the drag survives the
// pointer leaving the 6px gutter — which it will, constantly, because the
// gutter is 6px.
//
// The tree is the authority here too: a drag reads the container's pixel size,
// converts the movement to a fraction, and hands that to tree.resize, which
// owns the 5% floor that stops a pane being dragged out of existence. Nothing
// in this file decides how wide anything is.

import { $ } from "../dom.js";
import * as wm from "./wm.js";

const KEY_STEP = 0.02;   // one arrow press, as a fraction of the split

export function initDrag() {
	const host = $("wm-root");
	host.addEventListener("pointerdown", onPointerDown);
	host.addEventListener("keydown", onGutterKey);
	// A gutter is a real control, so it takes focus and arrow keys — resizing
	// must not be pointer-only.
	host.addEventListener("focusin", (e) => {
		if (e.target.classList?.contains("wm-gutter")) e.target.classList.add("is-focused");
	});
	host.addEventListener("focusout", (e) => {
		if (e.target.classList?.contains("wm-gutter")) e.target.classList.remove("is-focused");
	});
}

function onPointerDown(e) {
	const gutter = e.target.closest(".wm-gutter");
	if (!gutter || e.button !== 0) return;

	const container = gutter.closest(".wm-split");
	if (!container) return;

	const splitID = gutter.dataset.split;
	const index = Number(gutter.dataset.gutter);
	const vertical = container.classList.contains("is-col");
	const span = vertical ? container.clientHeight : container.clientWidth;
	if (!span) return;

	const start = vertical ? e.clientY : e.clientX;
	let last = 0;

	e.preventDefault();
	gutter.setPointerCapture(e.pointerId);
	gutter.classList.add("is-dragging");
	document.body.classList.add(vertical ? "wm-resizing-v" : "wm-resizing-h");

	const move = (ev) => {
		const now = ((vertical ? ev.clientY : ev.clientX) - start) / span;
		// Deltas are relative to the previous frame: tree.resize clamps against
		// the current fractions, so feeding it the total each time would fight
		// its own floor once a neighbour hits the 5% minimum.
		wm.resizeSplit(splitID, index, now - last);
		last = now;
	};

	const end = () => {
		gutter.releasePointerCapture?.(e.pointerId);
		gutter.classList.remove("is-dragging");
		document.body.classList.remove("wm-resizing-v", "wm-resizing-h");
		gutter.removeEventListener("pointermove", move);
		gutter.removeEventListener("pointerup", end);
		gutter.removeEventListener("pointercancel", end);
		// One save per drag, not one per frame.
		wm.commitResize();
	};

	gutter.addEventListener("pointermove", move);
	gutter.addEventListener("pointerup", end);
	gutter.addEventListener("pointercancel", end);
}

function onGutterKey(e) {
	const gutter = e.target.closest?.(".wm-gutter");
	if (!gutter) return;

	const container = gutter.closest(".wm-split");
	const vertical = container?.classList.contains("is-col");
	const back = vertical ? "ArrowUp" : "ArrowLeft";
	const fwd = vertical ? "ArrowDown" : "ArrowRight";

	let delta = 0;
	if (e.key === back) delta = -KEY_STEP;
	else if (e.key === fwd) delta = KEY_STEP;
	else return;

	e.preventDefault();
	e.stopPropagation();   // the shell binds Alt+arrows; a focused gutter wins
	wm.resizeSplit(gutter.dataset.split, Number(gutter.dataset.gutter), delta);
	wm.commitResize();
}
