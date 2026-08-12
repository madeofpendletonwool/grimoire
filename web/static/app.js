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
	bindModal();
	bindCardAutocomplete();
	$("search-form").addEventListener("submit", onSearch);
	$("card-form").addEventListener("submit", onCard);
	$("ask-form").addEventListener("submit", onAsk);
	loadMeta();
	// focus search on load
	$("search-input").focus();
});

/* ---------- Modal ---------- */
function bindModal() {
	const modal = $("modal");
	modal.addEventListener("click", (e) => {
		if (e.target.closest("[data-modal-close]")) closeModal();
	});
	document.addEventListener("keydown", (e) => {
		if (e.key === "Escape" && !modal.classList.contains("hidden")) closeModal();
	});
}

let modalReturnFocus = null;

function openModal(titleText, contentNode) {
	const modal = $("modal");
	$("modal-title").textContent = titleText;
	const body = $("modal-body");
	body.innerHTML = "";
	if (contentNode) body.appendChild(contentNode);
	modal.classList.remove("hidden");
	document.body.classList.add("modal-open");
	modalReturnFocus = document.activeElement;
	modal.querySelector(".modal-close").focus();
}

function closeModal() {
	const modal = $("modal");
	modal.classList.add("hidden");
	document.body.classList.remove("modal-open");
	$("modal-body").innerHTML = "";
	if (modalReturnFocus && modalReturnFocus.focus) modalReturnFocus.focus();
	modalReturnFocus = null;
}

function openCardModal(card) {
	const wrap = document.createElement("div");
	wrap.className = "modal-rules";
	wrap.appendChild(renderCard(card, false));
	openModal(card.name || "Card", wrap);
}

// openRuleModal shows a single cited rule, and when it is a numbered MTG rule
// it expands to the full mechanic section (parent + sub-rules) via /api/section
// so the reader sees the complete rule, not just the cited fragment.
function openRuleModal(rule) {
	const single = renderRuleBlock("", rule, true);
	const titleText = rule.number ? `${rule.number}${rule.title ? " · " + rule.title : ""}` : (rule.title || "Rule");
	openModal(titleText, single);

	if (!rule.number || !sectionKeyOf(rule)) return;

	const host = $("modal-body");
	const note = document.createElement("p");
	note.className = "section-loading";
	note.textContent = "Loading full section…";
	host.appendChild(note);

	fetch(`/api/section?corpus=${encodeURIComponent(state.corpus)}&number=${encodeURIComponent(rule.number)}`)
		.then((r) => r.json())
		.then((data) => {
			const entries = [];
			if (data.parent) entries.push(data.parent);
			if (data.children) entries.push(...data.children);
			if (entries.length === 0) return;
			// Replace the single-rule view with the full section.
			host.innerHTML = "";
			const section = document.createElement("div");
			section.className = "section-body";
			host.appendChild(section);
			renderSectionItemsInto(section, entries, "");
			if (data.parent && data.parent.body && data.parent.body.length <= 60) {
				$("modal-title").textContent = `${data.parent.number} · ${data.parent.body}`;
			}
		})
		.catch(() => { /* keep the single-rule view */ });
}

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
	// Collapse the card autocomplete dropdown when leaving card mode so it
	// doesn't briefly reappear with stale suggestions if the user returns.
	if (m !== "card") hideCardSuggestions();
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
	$("search-results").innerHTML = "";
	const wrap = document.createElement("div");
	wrap.className = "results";
	$("search-results").appendChild(wrap);

	// Cluster numbered results by their section root (e.g. 702.2a, 702.2b -> 702.2)
	// so a mechanic search surfaces the full rule instead of scattered snippets.
	// Non-numbered entries (glossary, D&D) stay standalone.
	const groups = [];
	const bySection = new Map();
	for (const r of results) {
		const sk = sectionKeyOf(r);
		if (sk) {
			let g = bySection.get(sk);
			if (!g) {
				g = { sectionKey: sk, items: [], cluster: true };
				bySection.set(sk, g);
				groups.push(g);
			}
			g.items.push(r);
		} else {
			groups.push({ sectionKey: null, items: [r], cluster: false });
		}
	}

	// Expand the first multi-result section into a full mechanic section
	// (parent + every sub-rule); render the rest as individual entries.
	let expanded = false;
	for (const g of groups) {
		if (!expanded && g.cluster && g.items.length >= 2) {
			expanded = true;
			wrap.appendChild(renderSectionBlock(q, g));
			continue;
		}
		for (const r of g.items) wrap.appendChild(renderRuleBlock(q, r));
	}
}

// renderRuleBlock builds a single rule article. When inModal is true the source
// label is hidden (the modal already names the corpus via its title).
function renderRuleBlock(q, r, inModal) {
	const el = document.createElement("article");
	el.className = "rule";
	const num = r.number ? `<span class="rule-num">${esc(r.number)}</span>` : "";
	const title = r.title ? `<span class="rule-title">${esc(r.title)}</span>` : "";
	const src = (!inModal && r.source) ? `<span class="rule-source">${esc(r.source)}</span>` : "";
	el.innerHTML = `
		<div class="rule-head">${num}${title}${src}</div>
		<div class="rule-body">${highlight(r.body || "", q)}</div>`;
	return el;
}

// renderSectionBlock renders a mechanic section card and backfills the full set
// of sub-rules (including siblings not in the search hits) via /api/section.
function renderSectionBlock(q, group) {
	const wrap = document.createElement("article");
	wrap.className = "rule section-block";
	const parentItem = group.items.find((r) => r.number === group.sectionKey) || group.items[0];
	const name = (parentItem && parentItem.body && parentItem.body.length <= 60) ? parentItem.body : (parentItem && parentItem.title) || "";

	const head = document.createElement("div");
	head.className = "section-head";
	head.innerHTML = `<span class="rule-num">${esc(group.sectionKey)}</span>${name ? `<span class="rule-title">${esc(name)}</span>` : ""}<span class="section-tag">full section</span>`;
	wrap.appendChild(head);

	const body = document.createElement("div");
	body.className = "section-body";
	wrap.appendChild(body);
	const loading = document.createElement("p");
	loading.className = "section-loading";
	loading.textContent = "Loading full section…";
	body.appendChild(loading);

	fetch(`/api/section?corpus=${encodeURIComponent(state.corpus)}&number=${encodeURIComponent(group.sectionKey)}`)
		.then((r) => r.json())
		.then((data) => {
			const entries = [];
			if (data.parent) entries.push(data.parent);
			if (data.children) entries.push(...data.children);
			renderSectionItemsInto(body, entries.length ? entries : group.items, q);
		})
		.catch(() => renderSectionItemsInto(body, group.items, q));
	return wrap;
}

function renderSectionItemsInto(container, entries, q) {
	container.innerHTML = "";
	for (const r of entries) {
		const item = document.createElement("div");
		item.className = "section-item";
		const num = r.number ? `<div class="section-item-head"><span class="rule-num sub">${esc(r.number)}</span></div>` : "";
		item.innerHTML = `${num}<div class="rule-body">${highlight(r.body || "", q)}</div>`;
		container.appendChild(item);
	}
}

// sectionKeyOf returns the section root for a numbered rule ("702.2a" -> "702.2")
// or null for unnumbered entries, which never cluster.
function sectionKeyOf(r) {
	if (!r || !r.number) return null;
	const m = r.number.match(/^(\d{1,3}(?:\.\d+)+)([a-z]+)?$/);
	return m ? m[1] : null;
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

/* ---------- Card autocomplete ---------- */
// Card lookup autofills as you type: each keystroke (debounced) hits
// /api/card/search and pops a dropdown of name suggestions below the input so
// you can click rather than type out "Our Market Research…" or
// "Asmoranomardicadaistinaculdacar". Selecting a suggestion fills the input
// and submits the form, reusing the existing onCard flow for the full reveal.

const CARD_SUGGEST_MIN = 2;     // characters before we start suggesting
const CARD_SUGGEST_DELAY = 250; // ms debounce
const CARD_SUGGEST_LIMIT = 8;   // max suggestions

let cardSuggestState = {
	items: [],        // current suggestion list (cardView objects)
	activeIndex: -1,  // -1 = none highlighted, 0..n = highlighted item
	debounceTimer: null,
	abortController: null,
};

function bindCardAutocomplete() {
	const input = $("card-input");
	const form = $("card-form");
	input.addEventListener("input", onCardInput);
	input.addEventListener("keydown", onCardKeydown);
	// Close the dropdown when the user clicks elsewhere or tabs away. The
	// small delay on blur lets click events on <li> items fire first.
	input.addEventListener("blur", () => setTimeout(hideCardSuggestions, 150));
	document.addEventListener("click", (e) => {
		if (!form.contains(e.target)) hideCardSuggestions();
	});
}

function onCardInput() {
	const q = $("card-input").value.trim();
	clearTimeout(cardSuggestState.debounceTimer);
	if (q.length < CARD_SUGGEST_MIN) {
		// Cancel any in-flight request too, so stale results for a longer
		// query don't pop in after the user cleared the input.
		if (cardSuggestState.abortController) {
			cardSuggestState.abortController.abort();
			cardSuggestState.abortController = null;
		}
		hideCardSuggestions();
		return;
	}
	cardSuggestState.debounceTimer = setTimeout(() => {
		fetchCardSuggestions(q);
	}, CARD_SUGGEST_DELAY);
}

async function fetchCardSuggestions(q) {
	// Cancel any in-flight request — only the latest keystroke's result matters.
	if (cardSuggestState.abortController) {
		cardSuggestState.abortController.abort();
	}
	const controller = new AbortController();
	cardSuggestState.abortController = controller;
	try {
		const res = await fetch(`/api/card/search?q=${encodeURIComponent(q)}&limit=${CARD_SUGGEST_LIMIT}`, {
			signal: controller.signal,
		});
		const data = await res.json();
		renderCardSuggestions(data.matches || []);
	} catch (err) {
		if (err.name !== "AbortError") {
			// Silently ignore — the dropdown just won't update this round.
		}
	} finally {
		if (cardSuggestState.abortController === controller) {
			cardSuggestState.abortController = null;
		}
	}
}

function renderCardSuggestions(matches) {
	const list = $("card-suggest");
	list.innerHTML = "";
	cardSuggestState.items = matches;
	cardSuggestState.activeIndex = -1;
	if (matches.length === 0) {
		hideCardSuggestions();
		return;
	}
	matches.forEach((c, i) => {
		const li = document.createElement("li");
		li.setAttribute("role", "option");
		li.dataset.index = i;
		const name = `<span class="suggest-name">${esc(c.name)}</span>`;
		const cost = c.mana_cost ? `<span class="suggest-cost">${esc(c.mana_cost)}</span>` : "";
		const type = c.type_line ? `<span class="suggest-type">${esc(c.type_line)}</span>` : "";
		li.innerHTML = `<span class="suggest-left">${name}${type}</span>${cost}`;
		li.addEventListener("click", () => selectCardSuggestion(c.name));
		list.appendChild(li);
	});
	showCardSuggestions();
}

function selectCardSuggestion(name) {
	// Cancel any pending autocomplete work so a late response doesn't
	// re-open the dropdown after we've just committed to a card.
	if (cardSuggestState.abortController) {
		cardSuggestState.abortController.abort();
		cardSuggestState.abortController = null;
	}
	clearTimeout(cardSuggestState.debounceTimer);
	hideCardSuggestions();
	$("card-input").value = name;
	$("card-form").requestSubmit();
}

function showCardSuggestions() {
	$("card-suggest").classList.remove("hidden");
	$("card-input").setAttribute("aria-expanded", "true");
}

function hideCardSuggestions() {
	const list = $("card-suggest");
	list.classList.add("hidden");
	list.querySelectorAll("li.active").forEach((li) => li.classList.remove("active"));
	$("card-input").setAttribute("aria-expanded", "false");
	cardSuggestState.activeIndex = -1;
}

function onCardKeydown(e) {
	const list = $("card-suggest");
	if (list.classList.contains("hidden")) return;
	const items = list.querySelectorAll("li");
	if (items.length === 0) return;
	if (e.key === "ArrowDown") {
		e.preventDefault();
		cardSuggestState.activeIndex = (cardSuggestState.activeIndex + 1) % items.length;
		updateCardActiveSuggestion(items);
	} else if (e.key === "ArrowUp") {
		e.preventDefault();
		cardSuggestState.activeIndex = (cardSuggestState.activeIndex - 1 + items.length) % items.length;
		updateCardActiveSuggestion(items);
	} else if (e.key === "Enter" && cardSuggestState.activeIndex >= 0) {
		e.preventDefault();
		selectCardSuggestion(cardSuggestState.items[cardSuggestState.activeIndex].name);
	} else if (e.key === "Escape") {
		hideCardSuggestions();
	}
}

function updateCardActiveSuggestion(items) {
	items.forEach((li, i) => {
		const on = i === cardSuggestState.activeIndex;
		li.classList.toggle("active", on);
		if (on) {
			li.setAttribute("aria-selected", "true");
			li.scrollIntoView({ block: "nearest" });
		} else {
			li.removeAttribute("aria-selected");
		}
	});
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
	const max = Math.min(sources.length, 8);
	sources.slice(0, 8).forEach((s, i) => {
		const btn = document.createElement("button");
		btn.type = "button";
		btn.className = "src-chip src-btn";
		btn.textContent = s.number || s.title || "•";
		btn.title = (s.title ? s.title + " — " : "") + truncate(s.body, 160);
		btn.addEventListener("click", () => openRuleModal(s));
		wrap.appendChild(btn);
		if (i < max - 1) wrap.appendChild(document.createTextNode(" "));
	});
	bubble.appendChild(wrap);
}

function renderCardsCited(bubble, cards) {
	const wrap = document.createElement("div");
	wrap.className = "sources";
	wrap.appendChild(document.createTextNode("Cards looked up: "));
	const max = Math.min(cards.length, 8);
	cards.slice(0, 8).forEach((c, i) => {
		const btn = document.createElement("button");
		btn.type = "button";
		btn.className = "src-chip card-chip src-btn";
		btn.textContent = c.name;
		btn.title = (c.type_line ? c.type_line + " — " : "") + truncate(c.oracle_text, 160);
		btn.addEventListener("click", () => openCardModal(c));
		wrap.appendChild(btn);
		if (i < max - 1) wrap.appendChild(document.createTextNode(" "));
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
