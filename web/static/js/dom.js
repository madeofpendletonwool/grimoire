// Tiny DOM helpers shared across the modules.

export const $ = (id) => document.getElementById(id);

/** Build an element with class, text, attributes and children in one call. */
export function el(tag, opts = {}, ...children) {
	const node = document.createElement(tag);
	if (opts.class) node.className = opts.class;
	if (opts.text != null) node.textContent = opts.text;
	if (opts.html != null) node.innerHTML = opts.html;
	for (const [k, v] of Object.entries(opts.attrs || {})) {
		if (v == null || v === false) continue;
		node.setAttribute(k, v === true ? "" : String(v));
	}
	for (const [k, v] of Object.entries(opts.on || {})) node.addEventListener(k, v);
	for (const c of children) {
		if (c == null) continue;
		node.append(c);
	}
	return node;
}

/** Escape text for safe interpolation into an HTML string. */
export function esc(s) {
	return String(s == null ? "" : s)
		.replace(/&/g, "&amp;")
		.replace(/</g, "&lt;")
		.replace(/>/g, "&gt;")
		.replace(/"/g, "&quot;")
		.replace(/'/g, "&#39;");
}

export function truncate(s, n) {
	if (!s) return "";
	return s.length > n ? s.slice(0, n) + "…" : s;
}

export function clear(node) {
	while (node.firstChild) node.removeChild(node.firstChild);
	return node;
}

/** Debounce fn by ms, returning a cancellable wrapper. */
export function debounce(fn, ms) {
	let t = null;
	const wrapped = (...args) => {
		clearTimeout(t);
		t = setTimeout(() => fn(...args), ms);
	};
	wrapped.cancel = () => clearTimeout(t);
	return wrapped;
}

/** True when the viewport is narrow enough that rail/drawer become overlays. */
export const isNarrow = () => window.matchMedia("(max-width: 900px)").matches;
