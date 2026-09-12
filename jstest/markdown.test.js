// The Markdown renderer is DOM-free until something calls into mana.js, so
// the escaping-and-linking core is testable here.
import { test } from "node:test";
import assert from "node:assert/strict";
import { renderMarkdown } from "../web/static/js/markdown.js";

test("a bare URL in prose becomes a link, query string intact", () => {
	const html = renderMarkdown("Full list at https://scryfall.com/search?order=name&q=art%3Asphere&unique=art.");
	assert.match(html, /<a href="https:\/\/scryfall\.com\/search\?order=name&amp;q=art%3Asphere&amp;unique=art" target="_blank" rel="noopener">https:\/\/scryfall\.com\/search\?order=name&amp;q=art%3Asphere&amp;unique=art<\/a>\./);
});

test("a Markdown link is not linked twice", () => {
	const html = renderMarkdown("see [the list](https://scryfall.com/search?q=c%3D4) now");
	assert.equal((html.match(/<a /g) || []).length, 1);
	assert.match(html, />the list<\/a>/);
});

test("trailing punctuation and an unbalanced paren stay outside the link", () => {
	assert.match(renderMarkdown("(see https://scryfall.com/x)"), /href="https:\/\/scryfall\.com\/x"[^>]*>https:\/\/scryfall\.com\/x<\/a>\)/);
	assert.match(renderMarkdown("go to https://a.b/c(d)"), /href="https:\/\/a\.b\/c\(d\)"/);
	assert.match(renderMarkdown("go to https://a.b/c, then"), /href="https:\/\/a\.b\/c"[^>]*>https:\/\/a\.b\/c<\/a>, then/);
});

test("a quoted URL ends at the quote and nothing unsafe survives escaping", () => {
	const html = renderMarkdown('"https://a.b/c" and javascript:alert(1) and <script>x</script>');
	assert.match(html, /href="https:\/\/a\.b\/c"[^>]*>https:\/\/a\.b\/c<\/a>&quot;/);
	assert.doesNotMatch(html, /href="javascript/);
	assert.doesNotMatch(html, /<script>/);
});

// Regression: with rule linking on, a number inside a URL's path was wrapped
// in a rule button, splicing markup into the href.
test("rule numbers inside a URL are left alone", () => {
	const html = renderMarkdown("Rule 702.2 and https://example.com/v/702.2/x", { rules: true });
	assert.match(html, /<button[^>]*data-rule="702\.2"/);
	assert.match(html, /href="https:\/\/example\.com\/v\/702\.2\/x"/);
	assert.equal((html.match(/<button/g) || []).length, 1);
});
