package cards

import (
	"regexp"
	"strings"
	"unicode"
)

// maxCandidates bounds how many card names we extract from one question, and
// therefore how many Scryfall lookups a single Q&A turn can spawn.
const maxCandidates = 6

// ExtractCandidates pulls candidate card names out of a free-text question.
//
// It is intentionally high-recall and lets Scryfall be the arbiter: if a
// candidate isn't a real card, the lookup returns ErrNotFound and we drop it.
// Sources, in priority order:
//  1. Double-quoted or [bracketed] phrases — explicit (`what does "Lightning
//     Bolt" do?`, `[Bolt]`).
//  2. "card named X", "named X", "called X" tails.
//  3. Title Case phrases — two or more capitalized words joined by connectors
//     (e.g. "Lightning Bolt", "Wrath of God", "Swords to Plowshares").
//  4. A lone capitalized word mid-sentence that isn't a common English word
//     (e.g. "Bolt" in "what does Bolt do?") — single-word card names.
//
// Results are deduped case-insensitively and capped at maxCandidates.
func ExtractCandidates(question string) []string {
	q := cleanForExtraction(question)
	if strings.TrimSpace(q) == "" {
		return nil
	}

	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(stripTrailingPunct(s))
		s = strings.Trim(s, `"'[]()`)
		if s == "" || !hasLetter(s) || isStopwordLower(s) {
			return
		}
		k := strings.ToLower(s)
		if seen[k] {
			return
		}
		seen[k] = true
		out = append(out, s)
	}

	// 1. Explicit quotes / brackets — capture, then blank the span so the
	// phrase scanner does not re-emit the same words.
	q = quotedRe.ReplaceAllStringFunc(q, func(m string) string {
		sub := quotedRe.FindStringSubmatch(m)
		if len(sub) >= 2 && sub[1] != "" {
			add(sub[1])
		}
		if len(sub) >= 3 && sub[2] != "" {
			add(sub[2])
		}
		return strings.Repeat(" ", len(m))
	})

	// 2. "named X" / "called X" tails — capture, then blank.
	q = namedRe.ReplaceAllStringFunc(q, func(m string) string {
		sub := namedRe.FindStringSubmatch(m)
		if len(sub) >= 2 {
			add(sub[1])
		}
		return strings.Repeat(" ", len(m))
	})

	// 3 + 4. Title Case phrases and lone capitalized words from what remains.
	for _, phrase := range titleCasePhrases(q) {
		add(phrase)
	}

	if len(out) > maxCandidates {
		out = out[:maxCandidates]
	}
	return out
}

// quotedRe captures double-quoted or [bracketed] phrases (bracketed must be
// non-empty).
var quotedRe = regexp.MustCompile(`"([^"]+)"|\[([^\]]+)\]`)

// namedRe captures the phrase following "named"/"called"/"card named". The
// keyword half is case-insensitive; the captured phrase is case-sensitive so
// it must start with a capital and continue with capitals or connectors only.
// "and"/"or" are deliberately NOT connectors here, so "named Bolt and Ring"
// does not collapse into one phrase.
var namedRe = regexp.MustCompile(`(?i:(?:card\s+named|named|called))\s+([A-Z][\w'\-]*(?:\s+(?:of|the|a|an|to|for|in|on|at|by|de|le|von|da|di|[A-Z][\w'\-]*))*)`)

// connectorWords are lowercase words allowed inside a Title Case card name.
// "and"/"or" are excluded so two cards joined by "and" stay separate.
var connectorWords = map[string]bool{
	"of": true, "the": true, "a": true, "an": true, "to": true, "for": true,
	"in": true, "on": true, "at": true, "by": true, "de": true, "le": true,
	"von": true, "da": true, "di": true,
}

// isPhraseCapital reports whether a token counts as a proper-noun capital for
// the purpose of building a Title Case phrase. A capitalized common English
// function word (Is, How, What, Will, ...) does NOT count — it is treated as a
// phrase boundary so it never seeds a card name.
func isPhraseCapital(w string) bool {
	if !startsUpper(w) {
		return false
	}
	return !isStopwordLower(strings.ToLower(w))
}

// titleCasePhrases scans tokens and emits maximal runs of [phrase capital |
// connector] that begin with a phrase capital and contain at least two
// capitals. A lone phrase capital is also emitted as a single-word candidate
// (for card names like "Counterspell").
func titleCasePhrases(q string) []string {
	tokens := tokenize(q)
	var out []string
	var run []string
	caps := 0
	flush := func() {
		if len(run) == 0 {
			return
		}
		if caps >= 2 {
			out = append(out, strings.Join(run, " "))
		} else {
			// single capitalized word: emit only the capital itself
			for _, w := range run {
				if isPhraseCapital(stripTrailingPunct(w)) {
					out = append(out, stripTrailingPunct(w))
				}
			}
		}
		run, caps = nil, 0
	}
	for _, tok := range tokens {
		w := stripTrailingPunct(tok.word)
		if w == "" {
			flush()
			continue
		}
		switch {
		case isPhraseCapital(w):
			run = append(run, w)
			caps++
		case connectorWords[strings.ToLower(w)] && len(run) > 0:
			run = append(run, w)
		default:
			flush()
		}
		if tok.sentenceEnd {
			flush()
		}
	}
	flush()
	return out
}

type token struct {
	word        string
	sentenceEnd bool
}

// wordRe keeps apostrophes and hyphens inside a word (e.g. "Sol'Kanar").
var wordRe = regexp.MustCompile(`[A-Za-z][A-Za-z'\-]*`)

func tokenize(q string) []token {
	var toks []token
	for _, raw := range strings.Fields(q) {
		w := strings.Trim(raw, "`\"()[]")
		if w == "" {
			continue
		}
		end := strings.HasSuffix(raw, ".") || strings.HasSuffix(raw, "?") || strings.HasSuffix(raw, "!")
		if m := wordRe.FindString(w); m != "" {
			w = m
		}
		if w == "" {
			continue
		}
		toks = append(toks, token{word: w, sentenceEnd: end})
	}
	return toks
}

func startsUpper(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		return unicode.IsUpper(r)
	}
	return false
}

func hasLetter(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) {
			return true
		}
	}
	return false
}

func stripTrailingPunct(s string) string {
	return strings.TrimRight(s, ".,;:!?")
}

// cleanForExtraction normalizes whitespace and curly quotes so the regexes and
// tokenizer see clean ASCII input.
func cleanForExtraction(s string) string {
	r := strings.NewReplacer(
		"\u2018", "'", "\u2019", "'",
		"\u201c", `"`, "\u201d", `"`,
		"\u2013", "-", "\u2014", "-",
		"\u00a0", " ",
	)
	s = r.Replace(s)
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

// stopwordsLower are common English words. They serve two roles: single-word
// candidates matching this set are dropped, and capitalized tokens matching
// this set (Is, How, What, Will, ...) are treated as phrase boundaries rather
// than proper-noun capitals.
var stopwordsLower = map[string]bool{
	"the": true, "a": true, "an": true, "and": true, "or": true, "of": true,
	"to": true, "in": true, "is": true, "it": true, "its": true, "for": true,
	"on": true, "with": true, "what": true, "do": true, "does": true, "how": true,
	"why": true, "when": true, "where": true, "who": true, "whom": true,
	"whose": true, "which": true, "can": true, "are": true, "be": true,
	"my": true, "i": true, "you": true, "your": true, "this": true, "that": true,
	"at": true, "as": true, "if": true, "me": true, "so": true, "there": true,
	"here": true, "they": true, "them": true, "he": true, "she": true,
	"we": true, "us": true, "from": true, "into": true, "about": true,
	"than": true, "then": true, "but": true, "not": true, "no": true,
	"yes": true, "will": true, "would": true, "could": true, "should": true,
	"may": true, "might": true, "must": true, "has": true, "have": true,
	"had": true, "was": true, "were": true, "been": true, "being": true,
	"did": true, "doing": true, "done": true, "am": true, "by": true,
	"card": true, "cards": true, "spell": true, "spells": true,
	"ability": true, "abilities": true, "permanent": true, "permanents": true,
	"creature": true, "creatures": true, "player": true, "players": true,
	"target": true, "targets": true, "resolve": true, "resolves": true,
	"resolved": true, "stack": true, "trigger": true, "triggers": true,
	"triggered": true, "effect": true, "effects": true, "counter": true,
	"counters": true, "opponent": true, "opponents": true, "controller": true,
	"owner": true, "combat": true, "damage": true, "turn": true, "phase": true,
	"step": true, "beginning": true, "end": true, "upkeep": true, "draw": true,
	"main": true, "declare": true, "attack": true, "attacks": true,
	"block": true, "blocks": true, "blocking": true, "cast": true, "play": true,
	"summoning": true, "tap": true, "tapped": true, "untap": true,
	"mtg": true, "magic": true, "gathering": true, "rule": true, "rules": true,
	"example": true, "see": true, "per": true, "each": true, "every": true,
	"any": true, "all": true, "other": true, "another": true, "same": true,
	"new": true, "old": true,
}

func isStopwordLower(s string) bool { return stopwordsLower[strings.ToLower(s)] }
