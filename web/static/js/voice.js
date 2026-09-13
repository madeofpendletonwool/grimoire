// Voice input. Two consumers, one file: the chat composer's toggle-mic
// button (initVoice) and the play surface's hold-to-talk control
// (dictate / recordClip, MAD-332). The browser-support and secure-context
// handling lives here once so both degrade the same way.
//
// Push-to-talk on the play surface has two paths and the caller picks by
// capability: Web Speech (Chrome/Edge — interim results stream live so a
// misheard word is visible before it becomes an action) or, when the
// browser has no Web Speech API, a MediaRecorder clip posted to the
// server's transcription endpoint (the ADR 5 seam — one configured
// endpoint, the campaign audio hook and this). Neither path sends
// anything anywhere until the caller says so; both hand back text.

import { $ } from "./dom.js";
import { setFoot } from "./chat.js";

const SpeechRecognitionCtor =
	window.SpeechRecognition || window.webkitSpeechRecognition;

/**
 * Whether this browser can run the Web Speech API here: the constructor
 * and a secure context, with the reason spelled out for the surfaces
 * that explain themselves instead of showing a dead button.
 */
export function webSpeechSupport() {
	if (!SpeechRecognitionCtor) {
		return { ok: false, reason: "This browser lacks the Web Speech API." };
	}
	if (!window.isSecureContext) {
		return { ok: false, reason: "Voice needs a secure connection (HTTPS or localhost)." };
	}
	return { ok: true, reason: "" };
}

/**
 * One push-to-talk dictation session over the Web Speech API. Interim
 * results arrive on onInterim as the growing transcript (finals so far
 * plus the live fragment); the session's last word arrives on onFinal
 * once — after stop(), or on its own if the recognition ends by itself
 * (silence, a hiccup). Silence is not an error: onFinal("") is how "the
 * button was held and nothing was said" reads. stop() is idempotent.
 */
export function dictate({ onInterim, onFinal, onError }) {
	const recognition = new SpeechRecognitionCtor();
	recognition.continuous = true;
	recognition.interimResults = true;
	recognition.lang = navigator.language || "en-US";

	let finalTranscript = "";
	let done = false;

	recognition.onresult = (event) => {
		let interim = "";
		for (let i = event.resultIndex; i < event.results.length; i++) {
			const result = event.results[i];
			if (result.isFinal) {
				finalTranscript += result[0].transcript;
			} else {
				interim += result[0].transcript;
			}
		}
		onInterim?.((finalTranscript + interim).trim());
	};

	const finish = () => {
		if (done) return;
		done = true;
		onFinal?.(finalTranscript.trim());
	};

	recognition.onend = finish;
	recognition.onerror = (event) => {
		if (event.error === "no-speech") {
			return; // onend follows; finish("") delivers the honest nothing
		}
		if (event.error === "not-allowed" || event.error === "service-not-allowed") {
			done = true;
			onError?.("Microphone access was blocked.");
			return;
		}
		if (event.error === "audio-capture") {
			done = true;
			onError?.("No microphone was found.");
			return;
		}
		if (event.error === "aborted") {
			return; // onend follows; keep whatever was heard
		}
		done = true;
		onError?.(`Voice input error: ${event.error}`);
	};

	try {
		recognition.start();
	} catch (_) {
		// start() throws if a session is already starting (the composer's
		// mic can be live); the holder hears it as an error, not silence.
		done = true;
		onError?.("Voice input is already running somewhere else.");
	}

	return {
		stop() {
			try {
				recognition.stop();
			} catch (_) { /* already ended; finish ran or will not */ }
		},
	};
}

/**
 * Whether this browser can record a clip for the server path: a mic
 * (getUserMedia) and MediaRecorder, on a secure context. This is the
 * path browsers without Web Speech (Firefox, Safari) take when the
 * install has a transcription endpoint configured.
 */
export function canRecordClip() {
	return !!navigator.mediaDevices?.getUserMedia && typeof window.MediaRecorder === "function"
		&& window.isSecureContext;
}

/** The clip filename whose extension tells the backend the container. */
function clipName(mimeType) {
	const type = String(mimeType || "");
	if (type.includes("ogg")) return "clip.ogg";
	if (type.includes("mp4")) return "clip.mp4";
	return "clip.webm";
}

/**
 * One push-to-talk recording session: getUserMedia + MediaRecorder.
 * stop() resolves with { blob, name } — the clip and the filename whose
 * extension names its container — and always releases the mic, held or
 * not. There are no interims on this path; the caller shows a listening
 * state and the transcript arrives when the server answers.
 */
export function recordClip() {
	const pick = ["audio/webm;codecs=opus", "audio/ogg;codecs=opus", "audio/mp4"]
		.find((t) => MediaRecorder.isTypeSupported(t));
	let recorder = null;
	let stream = null;
	let stopped = false;

	const begin = (async () => {
		stream = await navigator.mediaDevices.getUserMedia({ audio: true });
		recorder = new MediaRecorder(stream, pick ? { mimeType: pick } : undefined);
		recorder.start();
	})();

	const release = () => {
		for (const track of stream?.getTracks() || []) track.stop();
	};

	return {
		/** A promise for the mic grant — the holder awaits it to hear
		 *  permission problems at press time, not release time. */
		ready: begin,
		stop() {
			if (stopped) return Promise.reject(new Error("the clip already stopped"));
			stopped = true;
			return begin.then(() => new Promise((resolve, reject) => {
				const chunks = [];
				recorder.ondataavailable = (e) => {
					if (e.data?.size) chunks.push(e.data);
				};
				recorder.onstop = () => {
					release();
					const type = recorder.mimeType || pick || "audio/webm";
					resolve({ blob: new Blob(chunks, { type }), name: clipName(type) });
				};
				recorder.onerror = () => {
					release();
					reject(new Error("the recording failed"));
				};
				try {
					recorder.stop();
				} catch (err) {
					release();
					reject(err);
				}
			}));
		},
		abort() {
			if (stopped) return;
			stopped = true;
			begin.then(release).catch(release);
		},
	};
}

/**
 * Wire the composer's mic button. When the browser exposes no SpeechRecognition (Firefox,
 * Safari) or the page is not in a secure context (Web Speech needs HTTPS or
 * localhost), the button stays dimmed and reports why when clicked — a dead
 * disabled button with only a hover tooltip is easy to miss (and absent on
 * touch).
 */
export function initVoice() {
	const button = $("voice-btn");
	if (!button) return;

	const support = webSpeechSupport();
	if (!support.ok) {
		const reason = SpeechRecognitionCtor
			? support.reason
			: "Voice input needs Chrome or Edge — this browser lacks the Web Speech API.";
		markUnavailable(button, reason);
		return;
	}

	button.disabled = false;

	const input = $("composer-input");
	const recognition = new SpeechRecognitionCtor();
	recognition.continuous = true;
	recognition.interimResults = true;
	recognition.lang = navigator.language || "en-US";

	let listening = false;
	let finalTranscript = "";
	let base = "";   // composer text present when listening started
	let sep = "";    // separator inserted between base and dictated text

	recognition.onresult = (event) => {
		let interim = "";
		for (let i = event.resultIndex; i < event.results.length; i++) {
			const result = event.results[i];
			if (result.isFinal) {
				finalTranscript += result[0].transcript;
			} else {
				interim += result[0].transcript;
			}
		}
		writeComposer(base + sep + finalTranscript + interim);
	};

	recognition.onend = () => {
		// Commit the final transcript, drop any interim leftovers, reset UI.
		writeComposer(base + sep + finalTranscript);
		setListening(false);
	};

	recognition.onerror = (event) => {
		if (event.error === "no-speech" || event.error === "aborted") return;
		const denied = event.error === "not-allowed" || event.error === "service-not-allowed";
		setFoot(denied ? "Microphone access was blocked." : `Voice input error: ${event.error}`, true);
	};

	button.addEventListener("click", () => {
		if (listening) {
			recognition.stop();
		} else {
			startListening();
		}
	});

	function startListening() {
		finalTranscript = "";
		base = input.value;
		sep = base && !/\s$/.test(base) ? " " : "";
		try {
			recognition.start();
			setListening(true);
		} catch (_) {
			// start() throws if a session is already starting; reset to be safe.
			setListening(false);
		}
	}

	function setListening(on) {
		listening = on;
		button.classList.toggle("is-active", on);
		button.setAttribute("aria-pressed", on ? "true" : "false");
		button.title = on ? "Stop listening" : "Dictate";
	}

	/** Write text into the composer and fire `input` so autosize/hints update. */
	function writeComposer(text) {
		input.value = text;
		input.selectionStart = input.selectionEnd = text.length;
		input.dispatchEvent(new Event("input", { bubbles: true }));
	}
}

/**
 * Voice isn't supported here. Keep the button dimmed but clickable so the
 * reason surfaces in the composer footer on click — discoverable on touch and
 * mouse, unlike a hover-only tooltip on a disabled control.
 */
function markUnavailable(button, reason) {
	button.disabled = false;
	button.classList.add("is-unavailable");
	button.setAttribute("aria-disabled", "true");
	button.title = "Voice input unavailable — click for details";
	button.addEventListener("click", () => setFoot(reason, true));
}
