// The Grimoire — client interactions

const state = {
	corpus: "mtg",
	mode: "search",
	meta: null,
};

const $ = (id) => document.getElementById(id);

document.addEventListener("DOMContentLoaded", () => {
	bindCorpus();
	bindMode();
	$("search-form").addEventListener("submit", onSearch);
	$("ask-form").addEventListener("submit", onAsk);
	loadMeta();
	// focus search on load
	$("search-input").focus();
});

/* ---------- Meta ---------- */
async function loadMeta() {
	try {
		const res = await fetch("/api/meta");
		const data = await res.json();
		state.meta = data;
		updateCorpusMeta();
		if (data.chat_configured === false) {
			$("chat-foot").textContent = "Q&A chat needs an API key (ANTHROPIC_API_KEY) to be enabled on the server.";
		} else if (data.chat_model) {
			$("chat-foot").textContent = "Sage model: " + data.chat_model;
		}
	} catch (e) {
		/* non-fatal */
	}
}

function updateCorpusMeta() {
	if (!state.meta) return;
	const c = (state.meta.corpora || []).find((x) => x.id === state.corpus);
	if (!c) return;
	const v = c.version ? " · v" + c.version : "";
	$("corpus-meta").textContent = `${c.name}${v} · ${c.count.toLocaleString()} entries`;
}

/* ---------- Corpus switch ---------- */
function bindCorpus() {
	document.querySelectorAll(".corpus-tab").forEach((btn) => {
		btn.addEventListener("click", () => setCorpus(btn.dataset.corpus));
	});
}
function setCorpus(c) {
	state.corpus = c;
	document.documentElement.setAttribute("data-corpus", c);
	document.querySelectorAll(".corpus-tab").forEach((btn) => {
		const on = btn.dataset.corpus === c;
		btn.classList.toggle("active", on);
		btn.setAttribute("aria-selected", on ? "true" : "false");
	});
	updateCorpusMeta();
	// re-focus the active pane
	if (state.mode === "search") $("search-input").focus();
	else $("ask-input").focus();
}

/* ---------- Mode switch ---------- */
function bindMode() {
	document.querySelectorAll(".mode-tab").forEach((btn) => {
		btn.addEventListener("click", () => setMode(btn.dataset.mode));
	});
}
function setMode(m) {
	state.mode = m;
	document.querySelectorAll(".mode-tab").forEach((btn) => {
		btn.classList.toggle("active", btn.dataset.mode === m);
	});
	$("pane-search").classList.toggle("hidden", m !== "search");
	$("pane-ask").classList.toggle("hidden", m !== "ask");
	if (m === "search") $("search-input").focus();
	else $("ask-input").focus();
}

/* ---------- Search ---------- */
async function onSearch(e) {
	e.preventDefault();
	const q = $("search-input").value.trim();
	if (!q) return;
	$("search-status").textContent = "Consulting the tome…";
	$("search-results").innerHTML = "";
	try {
		const res = await fetch(`/api/search?corpus=${encodeURIComponent(state.corpus)}&q=${encodeURIComponent(q)}&limit=20`);
		const data = await res.json();
		renderSearch(q, data.results || []);
	} catch (err) {
		$("search-status").textContent = "The tome refuses to open: " + err;
	}
}

function renderSearch(q, results) {
	if (results.length === 0) {
		$("search-status").textContent = "No matching entries found.";
		$("search-results").innerHTML = '<div class="empty">No entries match your query. Try different words or a rule number.</div>';
		return;
	}
	$("search-status").textContent = `${results.length} ${results.length === 1 ? "entry" : "entries"} found`;
	const wrap = document.createElement("div");
	wrap.className = "results";
	for (const r of results) {
		const el = document.createElement("article");
		el.className = "rule";
		const num = r.number ? `<span class="rule-num">${esc(r.number)}</span>` : "";
		const title = r.title ? `<span class="rule-title">${esc(r.title)}</span>` : "";
		const src = r.source ? `<span class="rule-source">${esc(r.source)}</span>` : "";
		el.innerHTML = `
			<div class="rule-head">${num}${title}${src}</div>
			<div class="rule-body">${highlight(r.body, q)}</div>`;
		wrap.appendChild(el);
	}
	$("search-results").innerHTML = "";
	$("search-results").appendChild(wrap);
}

/* ---------- Ask (Q&A chat) ---------- */
async function onAsk(e) {
	e.preventDefault();
	const input = $("ask-input");
	const q = input.value.trim();
	if (!q) return;
	input.value = "";
	addBubble("user", q);
	const writing = addBubble("sage", "The sage consults the entries…", true);
	$("ask-btn").disabled = true;
	try {
		const res = await fetch("/api/ask", {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({ corpus: state.corpus, question: q }),
		});
		const data = await res.json();
		writing.querySelector(".chat-bubble").classList.remove("writing");
		writing.querySelector(".chat-bubble").textContent = data.answer || "(no answer)";
		if (data.sources && data.sources.length) {
			renderSources(writing.querySelector(".chat-bubble"), data.sources);
		}
		if (data.configured === false) {
			$("chat-foot").textContent = "Q&A chat needs an API key (ANTHROPIC_API_KEY) to be enabled on the server.";
		}
	} catch (err) {
		writing.querySelector(".chat-bubble").textContent = "The sage could not be reached: " + err;
	} finally {
		$("ask-btn").disabled = false;
		$("ask-input").focus();
	}
}

function addBubble(who, text, writing) {
	const log = $("chat-log");
	const row = document.createElement("div");
	row.className = "chat-" + who;
	const bub = document.createElement("div");
	bub.className = "chat-bubble" + (writing ? " writing" : "");
	bub.textContent = text;
	row.appendChild(bub);
	log.appendChild(row);
	log.scrollTop = log.scrollHeight;
	return row;
}

function renderSources(bubble, sources) {
	const wrap = document.createElement("div");
	wrap.className = "sources";
	wrap.appendChild(document.createTextNode("Cited entries: "));
	sources.slice(0, 8).forEach((s, i) => {
		const chip = document.createElement("span");
		chip.className = "src-chip";
		chip.textContent = s.number || s.title || "•";
		chip.title = (s.title ? s.title + " — " : "") + truncate(s.body, 120);
		wrap.appendChild(chip);
		if (i < Math.min(sources.length, 8) - 1) wrap.appendChild(document.createTextNode(" "));
	});
	bubble.appendChild(wrap);
}

/* ---------- Helpers ---------- */
function esc(s) {
	const d = document.createElement("div");
	d.textContent = s == null ? "" : s;
	return d.innerHTML;
}

function truncate(s, n) {
	if (!s) return "";
	return s.length > n ? s.slice(0, n) + "…" : s;
}

function highlight(body, query) {
	const safe = esc(body);
	const terms = query.split(/\s+/).filter((t) => t.length >= 3).map(escapeRegexp);
	if (terms.length === 0) return safe;
	const re = new RegExp("(" + terms.join("|") + ")", "gi");
	return safe.replace(re, "<mark>$1</mark>");
}

function escapeRegexp(s) {
	return s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}
