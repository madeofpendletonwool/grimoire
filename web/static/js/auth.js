// The gate: sign in, claim an unsealed grimoire by making the first keeper, or
// follow an admin's invite link to take a name.
//
// Standalone on purpose — it shares none of the chat modules, so a signed-out
// browser never downloads the app it cannot use. The two exceptions are the
// icon and backdrop modules: the gate is the first thing anyone sees, and it
// should already look like Grimoire rather than like a form that will later
// become Grimoire.

import { hydrate } from "./icons.js";
import { initScene } from "./scene.js";

const el = (id) => document.getElementById(id);

// mode is "login", "setup", or "register". primaryMode is the non-login mode
// relevant to this visit (setup on a fresh install, register when following an
// invite link, setup when the operator left self-service open); the toggle
// flips between it and login. setup and register both ask for the passphrase
// twice because neither has a reset flow behind it.
let mode = "login";
let primaryMode = "login";
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
	register: {
		sub: "An invite awaits. Take a name.",
		submit: "Join",
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
	// setup and register both confirm the passphrase; login does not.
	const confirmable = mode === "setup" || mode === "register";
	el("gate-confirm-field").hidden = !confirmable;
	el("gate-confirm").required = confirmable;
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

// endpointFor returns the signup/signin route for the current mode. register
// carries the invite code pulled from the link the invitee followed.
function endpointFor(invite) {
	if (mode === "login") return { path: "/api/auth/login", body: (u, p) => ({ username: u, password: p }) };
	if (mode === "setup") return { path: "/api/auth/setup", body: (u, p) => ({ username: u, password: p }) };
	return { path: "/api/auth/register", body: (u, p) => ({ username: u, password: p, invite }) };
}

async function submit(event) {
	event.preventDefault();
	const username = el("gate-username").value.trim();
	const password = el("gate-password").value;

	if ((mode === "setup" || mode === "register") && password !== el("gate-confirm").value) {
		showError("The two passphrases do not match.");
		return;
	}

	const button = el("gate-submit");
	button.disabled = true;
	showError("");
	try {
		const { path, body } = endpointFor(inviteCode);
		await post(path, body(username, password));
		// The session cookie is set; land on the app and drop the invite param.
		window.location.assign("/");
	} catch (err) {
		showError(err.message || "Something went wrong.");
		button.disabled = false;
		el("gate-password").focus();
		el("gate-password").select();
	}
}

// inviteCode is the secret carried by an admin's invite link (/?invite=CODE).
// It is read once at load; a register-mode submit folds it into the body.
let inviteCode = null;

async function start() {
	hydrate();
	initScene();

	inviteCode = new URLSearchParams(window.location.search).get("invite");

	let state = {};
	try {
		state = await (await fetch("/api/auth/state")).json();
	} catch (_) { /* fall through to the login form */ }

	if (state.authenticated) {
		window.location.assign("/");
		return;
	}
	registrationOpen = Boolean(state.registration_open);

	if (inviteCode) {
		// Following an invite link: register is the point of the visit, though
		// an existing keeper can still flip to login.
		primaryMode = "register";
		setMode("register");
		el("gate-foot").hidden = false;
	} else if (state.setup_required) {
		// Fresh install: the first keeper is also the admin. Nothing to switch to.
		primaryMode = "setup";
		setMode("setup");
		el("gate-foot").hidden = true;
	} else if (registrationOpen) {
		primaryMode = "setup";
		setMode("login");
		el("gate-foot").hidden = false;
	} else {
		primaryMode = "login";
		setMode("login");
		el("gate-foot").hidden = true;
	}

	el("gate-form").addEventListener("submit", submit);
	el("gate-toggle").addEventListener("click", () => setMode(mode === "login" ? primaryMode : "login"));
	el("gate-username").focus();
}

start();
