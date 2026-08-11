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
	$("card-form").addEventListener("submit", onCard);
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
	// Card lookup is MTG-only (Scryfall is the MTG card authority).
	const cardSupported = c === "mtg";
	$("mode-card").classList.toggle("hidden", !cardSupported);
	if (!cardSupported && state.mode === "card") {
		setMode("search");
	}
	updateCorpusMeta();
	// re-focus the active pane
	if (state.mode === "search") $("search-input").focus();
	else if (state.mode === "card") $("card-input").focus();
	else $("ask-input").focus();
}

/* ---------- Mode switch ---------- */
function bindMode() {
	document.querySelectorAll(".mode-tab").forEach((btn) => {
		btn.addEventListener("click", () => setMode(btn.dataset.mode));
	});
}
function setMode(m) {
	// Guard: card mode only exists for MTG.
	if (m === "card" && state.corpus !== "mtg") {
		m = "search";
	}
	state.mode = m;
	document.querySelectorAll(".mode-tab").forEach((btn) => {
		btn.classList.toggle("active", btn.dataset.mode === m);
	});
	$("pane-search").classList.toggle("hidden", m !== "search");
	$("pane-card").classList.toggle("hidden", m !== "card");
	$("pane-ask").classList.toggle("hidden", m !== "ask");
	if (m === "search") $("search-input").focus();
	else if (m === "card") $("card-input").focus();
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

/* ---------- Card lookup ---------- */
async function onCard(e) {
	e.preventDefault();
	const q = $("card-input").value.trim();
	if (!q) return;
	$("card-status").textContent = "Consulting Scryfall…";
	$("card-results").innerHTML = "";
	try {
		const res = await fetch(`/api/card?q=${encodeURIComponent(q)}`);
		const data = await res.json();
		renderCardResponse(q, data);
	} catch (err) {
		$("card-status").textContent = "The card could not be reached: " + err;
	}
}

function renderCardResponse(q, data) {
	if (data.card) {
		$("card-status").textContent = "";
		$("card-results").innerHTML = "";
		$("card-results").appendChild(renderCard(data.card));
		return;
	}
	if (data.error) {
		$("card-status").textContent = data.error;
		$("card-results").innerHTML = "";
		return;
	}
	const matches = data.matches || [];
	if (matches.length === 0) {
		$("card-status").textContent = `No card found for “${q}”.`;
		$("card-results").innerHTML = '<div class="empty">Try the full card name, or check the spelling.</div>';
		return;
	}
	$("card-status").textContent = `No exact match — did you mean one of these?`;
	const wrap = document.createElement("div");
	wrap.className = "results";
	matches.forEach((m) => wrap.appendChild(renderCard(m, true)));
	$("card-results").innerHTML = "";
	$("card-results").appendChild(wrap);
}

function renderCard(c, compact) {
	const el = document.createElement("article");
	el.className = "card";
	let head = `<span class="card-name">${esc(c.name)}</span>`;
	if (c.mana_cost) head += `<span class="card-cost">${esc(c.mana_cost)}</span>`;
	let img = "";
	if (c.image_url && !compact) {
		img = `<img class="card-img" src="${esc(c.image_url)}" alt="${esc(c.name)}" loading="lazy">`;
	}
	let stats = "";
	if (c.power && c.toughness) stats = `<span class="card-pt">${esc(c.power)}/${esc(c.toughness)}</span>`;
	else if (c.loyalty) stats = `<span class="card-pt">${esc(c.loyalty)}</span>`;
	const uri = c.scryfall_uri ? `<a class="card-link" href="${esc(c.scryfall_uri)}" target="_blank" rel="noopener">Scryfall ↗</a>` : "";

	const facesHTML = (c.faces && c.faces.length)
		? c.faces.map((f) => renderCardFace(f)).join("")
		: `<div class="card-oracle">${highlight(c.oracle_text || "(no oracle text)", c.name)}</div>`;

	el.innerHTML = `
		<div class="card-inner">
			<div class="card-head">${head}</div>
			<div class="card-type">${esc(c.type_line || "")}${stats}</div>
			<div class="card-text">${facesHTML}</div>
			<div class="card-foot">${c.set ? `<span class="card-set">${esc(c.set.toUpperCase())}</span>` : ""}${uri}</div>
		</div>${img}`;
	return el;
}

function renderCardFace(f) {
	let head = `<span class="card-face-name">${esc(f.name)}</span>`;
	if (f.mana_cost) head += `<span class="card-cost">${esc(f.mana_cost)}</span>`;
	let stats = "";
	if (f.power && f.toughness) stats = ` <span class="card-pt">${esc(f.power)}/${esc(f.toughness)}</span>`;
	return `<div class="card-face">
		<div class="card-face-head">${head}</div>
		<div class="card-face-type">${esc(f.type_line || "")}${stats}</div>
		<div class="card-oracle">${esc(f.oracle_text || "")}</div>
	</div>`;
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
		const bubble = writing.querySelector(".chat-bubble");
		if (data.cards && data.cards.length) {
			renderCardsCited(bubble, data.cards);
		}
		if (data.sources && data.sources.length) {
			renderSources(bubble, data.sources);
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

function renderCardsCited(bubble, cards) {
	const wrap = document.createElement("div");
	wrap.className = "sources";
	wrap.appendChild(document.createTextNode("Cards looked up: "));
	cards.slice(0, 8).forEach((c, i) => {
		const chip = document.createElement("span");
		chip.className = "src-chip card-chip";
		chip.textContent = c.name;
		chip.title = (c.type_line ? c.type_line + " — " : "") + truncate(c.oracle_text, 120);
		if (c.scryfall_uri) {
			const a = document.createElement("a");
			a.href = c.scryfall_uri;
			a.target = "_blank";
			a.rel = "noopener";
			a.textContent = c.name;
			a.title = chip.title;
			chip.textContent = "";
			chip.appendChild(a);
		}
		wrap.appendChild(chip);
		if (i < Math.min(cards.length, 8) - 1) wrap.appendChild(document.createTextNode(" "));
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
