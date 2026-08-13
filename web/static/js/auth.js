// The gate: sign in, or claim an unsealed grimoire by making the first keeper.
//
// Standalone on purpose — it shares none of the chat modules, so a signed-out
// browser never downloads the app it cannot use. The two exceptions are the
// icon and backdrop modules: the gate is the first thing anyone sees, and it
// should already look like Grimoire rather than like a form that will later
// become Grimoire.

import { hydrate } from "./icons.js";
import { initScene } from "./scene.js";

const el = (id) => document.getElementById(id);

// mode is "login" or "setup"; setup asks for the passphrase twice because
// there is no reset flow behind it.
let mode = "login";
let registrationOpen = false;

const copy = {
	login: {
		sub: "Speak your name, keeper.",
		submit: "Enter",
		toggle: "Create an account",
		autocomplete: "current-password",
	},
	setup: {
		sub: "No keeper yet. Claim the tome.",
		submit: "Seal it",
		toggle: "I already have an account",
		autocomplete: "new-password",
	},
};

function setMode(next) {
	mode = next;
	const c = copy[mode];
	el("gate-sub").textContent = c.sub;
	el("gate-submit").textContent = c.submit;
	el("gate-password").autocomplete = c.autocomplete;
	el("gate-confirm-field").hidden = mode !== "setup";
	el("gate-confirm").required = mode === "setup";
	el("gate-toggle").textContent = c.toggle;
	showError("");
}

function showError(message) {
	const box = el("gate-error");
	box.textContent = message;
	box.hidden = !message;
}

async function post(path, body) {
	const res = await fetch(path, {
		method: "POST",
		headers: { "content-type": "application/json" },
		body: JSON.stringify(body),
	});
	if (!res.ok) {
		let detail = res.statusText;
		try {
			const payload = await res.json();
			if (payload && payload.error) detail = payload.error;
		} catch (_) { /* keep the status text */ }
		throw new Error(detail);
	}
}

async function submit(event) {
	event.preventDefault();
	const username = el("gate-username").value.trim();
	const password = el("gate-password").value;

	if (mode === "setup" && password !== el("gate-confirm").value) {
		showError("The two passphrases do not match.");
		return;
	}

	const button = el("gate-submit");
	button.disabled = true;
	showError("");
	try {
		await post(mode === "setup" ? "/api/auth/setup" : "/api/auth/login", { username, password });
		// The session cookie is set; / now renders the app.
		window.location.assign("/");
	} catch (err) {
		showError(err.message || "Something went wrong.");
		button.disabled = false;
		el("gate-password").focus();
		el("gate-password").select();
	}
}

async function start() {
	hydrate();
	initScene();

	let state = {};
	try {
		state = await (await fetch("/api/auth/state")).json();
	} catch (_) { /* fall through to the login form */ }

	if (state.authenticated) {
		window.location.assign("/");
		return;
	}
	registrationOpen = Boolean(state.registration_open);
	setMode(state.setup_required ? "setup" : "login");

	// Switching modes only makes sense once a keeper exists and the operator
	// left the door open; on a fresh install there is nothing to switch to.
	el("gate-foot").hidden = state.setup_required || !registrationOpen;

	el("gate-form").addEventListener("submit", submit);
	el("gate-toggle").addEventListener("click", () => setMode(mode === "login" ? "setup" : "login"));
	el("gate-username").focus();
}

start();
