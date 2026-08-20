// The public share page: the tome opened to one answer for anyone holding
// the link. Standalone on purpose, like the gate — it loads no app modules,
// only the backdrop, the icons, and the same Markdown renderer the chat uses,
// so a shared page and the conversation it came from can never disagree on
// formatting. No session is read, no API is called: the token in the URL is
// the whole access model.

import { $ } from "./dom.js";
import { hydrate } from "./icons.js";
import { initScene } from "./scene.js";
import { renderMarkdown } from "./markdown.js";

hydrate();
initScene();

// The template parks the raw answer Markdown in a hidden element; the browser
// decodes it back through textContent, and the renderer escapes everything
// before any markup is inserted — nothing the model wrote can become live
// HTML here any more than it can in the app.
const src = $("share-src");
const out = $("share-answer");
if (src && out) {
	const mtg = document.documentElement.dataset.corpus === "mtg";
	// No rule-ref buttons: there is no drawer to open, so numbers stay prose.
	out.innerHTML = renderMarkdown(src.textContent, { rules: false, mana: mtg });
	src.remove();
}
