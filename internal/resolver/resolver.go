// Package resolver drives Grimoire's Magic: The Gathering interaction resolver.
//
// Given a board state and a proposed spell/ability sequence, it assembles the
// grounding (real card oracle text from Scryfall plus the relevant rules —
// priority/stack, triggered abilities, layers, replacement effects) and builds
// the specialized prompt that asks the model to walk the sequence step by step
// in the order the Comprehensive Rules prescribe, citing each rule. The package
// is transport-agnostic: it produces the prompt exchange; the server streams it
// to the model and back to the reader.
package resolver

import (
	"context"
	"fmt"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/cards"
	"github.com/madeofpendletonwool/grimoire/internal/data"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
)

// Deps are the external collaborators the resolver grounds against. A nil
// Cards leaves card oracle text out; a nil Store leaves rules out. The resolver
// still runs and produces a prompt — the model just answers with less grounding
// and is told to say so where it cannot verify.
type Deps struct {
	Cards    cards.Looker      // Scryfall looker (may be nil)
	CardDict *cards.Dictionary // optional dictionary for sequence card detection
	Store    *index.Store      // rules FTS5 store (may be nil)
}

// Grounding is the assembled context for one resolve: what the model is shown
// (docs + card text) and what the reader is shown as citations (sources + the
// cards that were looked up, plus names that could not be).
type Grounding struct {
	Docs       []llm.ContextDoc
	Cards      []*cards.Card
	Unresolved []string
	Sources    []index.Result
}

// Request is the prompt exchange the server sends to the model for a resolve.
type Request struct {
	System string
	User   string
}

// coreChapters are the MTG interaction chapters pulled wholesale so the model
// sees complete mechanics rather than fragments: 117 timing/priority/stack,
// 603 triggered abilities, 613 interaction of continuous effects (layers), 616
// interaction of replacement and/or prevention effects.
var coreChapters = []string{"117", "603", "613", "616"}

// maxResolveDocs caps the rule grounding handed to the model for one resolve.
// The core chapters are kept whole (the mechanics that matter); extra FTS hits
// are appended only up to this ceiling.
const maxResolveDocs = 120

// Ground resolves the named cards (board + sequence) and retrieves the rules
// the resolver needs. It never returns an error: a failed lookup or a missing
// store simply yields less grounding, and the prompt tells the model to report
// the gap rather than guess.
func Ground(ctx context.Context, deps Deps, in Input) Grounding {
	var g Grounding
	phrases := boardNames(in.Board)
	phrases = append(phrases, sequenceCardCandidates(in.Sequence, deps.CardDict)...)
	if deps.Cards != nil && len(phrases) > 0 {
		res := cards.Resolve(ctx, deps.Cards, phrases)
		g.Cards = res.Cards
		g.Unresolved = res.Unresolved
	}
	if deps.Store != nil {
		g.Docs, g.Sources = groundRules(ctx, deps.Store, in)
	}
	return g
}

// Prompt builds the resolver's prompt exchange from a grounding.
func Prompt(in Input, g Grounding) Request {
	return Request{System: SystemPrompt(), User: UserMessage(in, g)}
}

// boardNames returns the board's permanent names in mention order, deduped.
func boardNames(b Board) []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range b.Permanents {
		name := strings.TrimSpace(p.Name)
		if name == "" {
			continue
		}
		key := strings.ToLower(name)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, name)
	}
	return out
}

// sequenceCardCandidates extracts candidate card names from the sequence's step
// texts, so a spell or ability named in the sequence is grounded even when it
// is not on the board.
func sequenceCardCandidates(s Sequence, d *cards.Dictionary) []string {
	var out []string
	seen := map[string]bool{}
	for _, step := range s.Steps {
		for _, c := range cards.ExtractCandidatesWithDict(step.Text, d) {
			key := strings.ToLower(c)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, c)
		}
	}
	return out
}

// groundRules pulls the core interaction chapters wholesale, then layers in
// FTS5 retrieval over the sequence to surface the keyword abilities (702.x) and
// state-based actions (704) tied to the specific cards and actions — which the
// wholesale chapters do not cover. The FTS seeds are the sources shown to the
// reader as citations; the wholesale chapters feed the model directly.
func groundRules(ctx context.Context, store *index.Store, in Input) ([]llm.ContextDoc, []index.Result) {
	var rules []index.Result
	seen := map[string]bool{}
	add := func(rs []index.Result) {
		for _, r := range rs {
			if r.Number == "" || seen[r.Number] {
				continue
			}
			seen[r.Number] = true
			rules = append(rules, r)
		}
	}
	for _, ch := range coreChapters {
		rs, err := store.Chapter(ctx, data.CorpusMTG, ch)
		if err != nil {
			break
		}
		add(rs)
	}
	var sources []index.Result
	if q := retrievalQuery(in); q != "" {
		if seeds, err := store.Retrieve(ctx, data.CorpusMTG, q, 12); err == nil {
			add(seeds)
			sources = seeds
		}
	}
	if len(rules) > maxResolveDocs {
		rules = rules[:maxResolveDocs]
	}
	docs := make([]llm.ContextDoc, 0, len(rules))
	for _, r := range rules {
		docs = append(docs, llm.ContextDoc{Number: r.Number, Title: r.Title, Body: r.Body})
	}
	return docs, sources
}

// retrievalQuery builds the FTS5 query from the sequence and the free-text
// note. Board card names are deliberately excluded: the rules corpus rarely
// names specific cards, so they only dilute the mechanic-word retrieval that
// surfaces the relevant keyword abilities and state-based actions.
func retrievalQuery(in Input) string {
	var parts []string
	if note := strings.TrimSpace(in.Note); note != "" {
		parts = append(parts, note)
	}
	for _, s := range in.Sequence.Steps {
		if t := strings.TrimSpace(s.Text); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, " ")
}

// SystemPrompt is the specialized resolver system prompt: it tells the model to
// walk the sequence in Comprehensive-Rules order (APNAP triggers, replacement
// effects in dependency/timestamp order, layers, then stack top-down), to cite
// each rule, to answer only from the provided text, and to be honest that it is
// an assistant rather than a CR-equivalent oracle.
func SystemPrompt() string {
	const sep = "\n\n"
	b := strings.Builder{}
	b.WriteString("You are the Grimoire interaction resolver for Magic: The Gathering — a careful judge walking a proposed spell/ability sequence step by step. The reader is a Commander player with a board state and a proposed order of plays who wants to know exactly how the interactions resolve.")
	b.WriteString(sep)
	b.WriteString("HONESTY: you are an assistant, not a Comprehensive-Rules-equivalent oracle. Resolve what you can verify from the provided rules and card text, and say plainly when a situation is ambiguous, depends on a choice the reader has not stated, or is beyond this walkthrough.")
	b.WriteString(sep)
	b.WriteString("GROUNDING RULES — follow strictly:" +
		"\n1. Use ONLY the provided rule excerpts and card oracle text. Do NOT rely on your own memory of cards or rules for any specific fact." +
		"\n2. For any card, use ONLY the provided oracle text for its name, mana cost, type, and effects — quote it faithfully. Do not invent or guess." +
		"\n3. If a card is named but its oracle text was NOT provided, say the lookup failed — do NOT describe what you think it does." +
		"\n4. Cite the rule number for every step that depends on one (e.g. 603.3b, 613.6, 616.1a).")
	b.WriteString(sep)
	b.WriteString("RESOLUTION ORDER — apply in this order and narrate each phase before moving on:" +
		"\nA. Triggers: triggers that fire from the stated events go on the stack in APNAP order (603.2, 603.3b). For each player in turn order, that player puts the triggers they control on the stack in the order they choose. State which triggers exist, who controls each, and the resulting stack order." +
		"\nB. Replacement effects: as an event a replacement could replace would happen, apply the applicable replacement effects in dependency order, then timestamp order (616.1, 616.2). Name each replacement effect, its controller, and the order they apply." +
		"\nC. Continuous effects: apply in layers 613.1 through 613.7; note any dependency reordering within a layer (613.6, 613.7) and how timestamp breaks ties." +
		"\nD. The stack: resolve top-down, one object at a time, with priority passes between objects (117.4, 117.5). Objects resolve in the reverse of the order they were added.")
	b.WriteString(sep)
	b.WriteString("SELF-REFERENCE / CONTROLLER TRAPS — check before counting:" +
		"\n- A card's own name in its text means \"this object,\" not \"any object with that name\" (201.4). An ability worded \"When this creature dies\" triggers only once, for itself; it is not \"Whenever a creature dies.\"" +
		"\n- \"you\"/\"your\" refer to that object's controller, which may be a different player. Before applying any effect, identify who controls each permanent and who \"you\" is for that effect.")
	b.WriteString(sep)
	b.WriteString("OUTPUT: a numbered walkthrough. Each numbered step states what happens this step and cites the rule it follows. End with a short \"Result\" line stating the final board/state and any choices the reader still owes. Keep it tight and practical for a player at the table.")
	return b.String()
}

// UserMessage renders the board, the sequence, the grounding rules, and the
// resolved card oracle text into the user turn, in the shape the system prompt
// expects.
func UserMessage(in Input, g Grounding) string {
	var b strings.Builder
	b.WriteString("Resolve this Magic: The Gathering interaction.\n\n")

	b.WriteString("BOARD:\n")
	if len(in.Board.Permanents) == 0 {
		b.WriteString("(empty)\n")
	}
	for _, p := range in.Board.Permanents {
		b.WriteString("- ")
		if p.Controller != "" {
			fmt.Fprintf(&b, "[%s] ", p.Controller)
		}
		b.WriteString(p.Name)
		state := permanentState(p)
		if state != "" {
			b.WriteString("  (" + state + ")")
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")

	b.WriteString("SEQUENCE (in proposed order):\n")
	if len(in.Sequence.Steps) == 0 {
		b.WriteString("(none stated)\n")
	}
	for i, s := range in.Sequence.Steps {
		if s.Controller != "" {
			fmt.Fprintf(&b, "%d. [%s] %s\n", i+1, s.Controller, s.Text)
		} else {
			fmt.Fprintf(&b, "%d. %s\n", i+1, s.Text)
		}
	}
	b.WriteString("\n")

	if note := strings.TrimSpace(in.Note); note != "" {
		fmt.Fprintf(&b, "CLARIFICATIONS: %s\n\n", note)
	}

	if len(g.Cards) > 0 {
		b.WriteString("Card oracle text (authoritative — from Scryfall):\n\n")
		for _, c := range g.Cards {
			fmt.Fprintf(&b, "### %s\n", c.Name)
			if c.ManaCost != "" {
				fmt.Fprintf(&b, "Mana cost: %s  ·  ", c.ManaCost)
			}
			if c.TypeLine != "" {
				fmt.Fprintf(&b, "Type: %s\n", c.TypeLine)
			}
			body := strings.TrimSpace(c.OracleText)
			if body == "" {
				body = "(no oracle text)"
			}
			fmt.Fprintf(&b, "Oracle text: %s\n\n", body)
		}
	}

	if len(g.Unresolved) > 0 {
		fmt.Fprintf(&b, "Names that could not be looked up: %s\n"+
			"Do not describe these from memory — say the lookup failed.\n\n",
			strings.Join(g.Unresolved, ", "))
	}

	b.WriteString("Relevant rules:\n\n")
	if len(g.Docs) == 0 {
		b.WriteString("(no directly matching rules found)\n\n")
	}
	for _, d := range g.Docs {
		header := d.Title
		if d.Number != "" {
			if header != "" {
				header = d.Number + " — " + header
			} else {
				header = d.Number
			}
		}
		fmt.Fprintf(&b, "### %s\n%s\n\n", header, truncate(d.Body, 1500))
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

// permanentState renders a permanent's tapped/counters/note into one compact
// parenthetical for the board listing.
func permanentState(p Permanent) string {
	var parts []string
	if p.Tapped {
		parts = append(parts, "tapped")
	}
	if c := strings.TrimSpace(p.Counters); c != "" {
		parts = append(parts, c)
	}
	if n := strings.TrimSpace(p.Note); n != "" {
		parts = append(parts, n)
	}
	return strings.Join(parts, ", ")
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
