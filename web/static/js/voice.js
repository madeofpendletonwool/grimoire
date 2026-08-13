// Voice input via the browser's built-in Web Speech API. Adds a mic button
// beside the composer that transcribes speech into the textarea — nothing is
// sent until the user reviews the text and hits send.

import { $ } from "./dom.js";
import { setFoot } from "./chat.js";

const SpeechRecognitionCtor =
	window.SpeechRecognition || window.webkitSpeechRecognition;

/**
 * Wire the mic button. When the browser exposes no SpeechRecognition (Firefox,
 * Safari) or the page is not in a secure context (Web Speech needs HTTPS or
 * localhost), the button stays disabled with an explanatory tooltip.
 */
export function initVoice() {
	const button = $("voice-btn");
	if (!button) return;

	if (!SpeechRecognitionCtor || !window.isSecureContext) {
		button.disabled = true;
		button.title = "Voice input isn't available here — try Chrome or Edge over HTTPS.";
		return;
	}

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
