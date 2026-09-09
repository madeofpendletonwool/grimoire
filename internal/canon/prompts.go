package canon

import (
	"fmt"
	"sort"
	"strings"
)

// PROMPT_VERSION keys the extraction ledger: a unit is skipped only under the
// same version AND an unchanged input checksum. Changing prompt content in a
// way that affects outputs means bumping this string — never editing the
// prompt bodies in place — which re-extracts the corpus under the new
// contract. Arda's rule, kept verbatim in spirit: prompts are code.
//
// 002: the belief loop (MAD-489). Player journal sources gain the campaign
// facts list and the beliefs section of the wire contract.
const PROMPT_VERSION = "canon-extract-002"

// wireContract is the JSON shape the model must return. It is part of the
// prompt (the model sees it) and part of the code (wire.go parses it); when
// it changes in a way that affects outputs, PROMPT_VERSION bumps with it.
//
// Entity references ("ref" below) are either an existing campaign entity id
// from the task message or a new_entities local_id from this same payload —
// anything else is dropped at validation as a dangling reference.
const wireContract = `{
  "new_entities": [
    {"local_id": "brother-venn", "kind": "npc|pc|faction|location|item|deity|organization|creature|concept",
     "name": "Brother Venn", "summary": "one sentence", "quote": "verbatim words from the chunk",
     "confidence": 0.8}
  ],
  "facts": [
    {"local_id": "venn-serves-cult", "statement": "one atomic statement in your own words",
     "subject": "<entity ref>", "predicate": "a short verb phrase, e.g. serves",
     "object_entity": "<entity ref>", "object_literal": null,
     "visibility": "public|secret",
     "quote": "verbatim words from the chunk that state or directly imply it",
     "confidence": 0.85}
  ],
  "events": [
    {"local_id": "chapel-fight", "summary": "one-sentence summary of what happened",
     "clock_at": null,
     "location": "<entity ref or null>",
     "participants": [{"entity": "<entity ref>", "role": "defender"}],
     "quote": "verbatim words from the chunk", "confidence": 0.8}
  ],
  "discoveries": [
    {"fact": "<local_id of a fact from this same payload>",
     "discovered_by": "<entity ref of a pc or npc, or 'party'>",
     "stance": "knows|suspects|believes_false",
     "method": "how they learned it, e.g. 'read the mining ledger'",
     "quote": "verbatim words from the chunk where they perceive it",
     "confidence": 0.7}
  ],
  "relationships": [
    {"from_entity": "<entity ref>", "rel_type": "one of the allowed types",
     "to_entity": "<entity ref>",
     "quote": "verbatim words from the chunk", "confidence": 0.9}
  ],
  "beliefs": [
    {"local_id": "mira-thinks-merchant-vampire",
     "statement": "the claim in the author's own words, one sentence",
     "subject": "<entity ref>", "predicate": "a short verb phrase, matching the campaign fact it concerns",
     "object_entity": "<entity ref>", "object_literal": null,
     "discovered_by": "<entity ref, usually the journal's author>",
     "stance": "knows|suspects|believes_false",
     "method": "how they came to hold it, e.g. 'the dust never settled on his ledger'",
     "quote": "verbatim words from the chunk that state or directly imply the claim",
     "confidence": 0.8}
  ]
}
object_entity and object_literal are mutually exclusive: a fact's object is
an entity or a literal value, never both, never neither. The beliefs list is
for player journal sources only.`

// systemPrompt is the standing instruction. The prime directive is the whole
// game: the model extracts and cites; it never decides what is true.
func systemPrompt() string {
	return `You are the extraction historian for a D&D campaign canon engine. You read session transcripts, DM notes and player journals, and EXTRACT what they say, with citations. You never decide what is true in the campaign from your own knowledge.

PRIME DIRECTIVE — EXTRACTOR, NOT ORACLE. Every candidate you emit must be stated in or directly implied by the provided chunk, and must carry a verbatim quote of the words that support it. Facts you know from D&D lore, from earlier sessions, or from the campaign's established canon — but that this chunk does not carry — are FORBIDDEN. When the chunk and your knowledge disagree, the chunk wins or the candidate is omitted.

THE SPAN RULE (hard requirement — candidates violating it are discarded):
1. Every candidate of every kind MUST carry a "quote": text copied VERBATIM (byte-for-byte, including typos) from the chunk you were given.
2. Copy the quote exactly as it appears in the chunk. Do not paraphrase, do not fix, do not shorten beyond what still supports the candidate, do not stitch together words that are not adjacent.
3. A candidate you cannot quote is omitted. Never invent a quote.

WHAT TO EXTRACT:
- facts: atomic, one fact each, no compound statements. Predicates are short verb phrases ("serves", "owns", "killed"). visibility "secret" is for things the transcript establishes as hidden from the party.
- events: things that HAPPENED in the fiction, with who participated. clock_at is the in-world day if the chunk or the campaign clock makes it clear; otherwise null.
- discoveries: who LEARNED a fact from this payload's facts, how, and what they now hold. A discovery is the highest-stakes candidate: only emit one when a character, in the fiction, actually perceived the information — the DM saying something out loud is not a character learning it. Ask: which character perceived this, and where in the quote do they perceive it?
- relationships: changes in standing between entities, using ONLY the allowed relationship types given in the task message.
- new_entities: people, places, factions or things the chunk introduces that are not in the entity list. New entities need a quote too — the words that introduce them.
- beliefs (player journals only): a journal asserting something as fact about a subject the campaign already records is the AUTHOR'S BELIEF, never canon. Emit it as a belief: the claim stated as the journal holds it, the campaign fact's predicate reused, and the author's stance — believes_false when the journal states outright as truth something the campaign's facts contradict, suspects for hedged language ("I think", "it must be"), knows when the journal records the author taking a matching claim as learned truth. Never emit a belief whose subject+predicate the campaign does not already record somewhere — that is a fact + discovery, not a belief. The player is never corrected: extract what they wrote, as what they hold.

ENTITY RULES:
- Reference existing campaign entities by their id from the entity list whenever the chunk is about them; do not redefine them under a new_entities entry. Duplicating an existing entity is the classic extraction failure.
- local_id values are lowercase slugs (lowercase letters, digits, hyphens/underscores only), unique within your output.
- Every entity reference must be either an id from the entity list or a local_id you define in new_entities in this same payload. Dangling references are dropped.

CONFIDENCE is your 0-1 score for how clearly the chunk supports the candidate: 1.0 stated outright, lower for inferred.

OUTPUT FORMAT:
Return ONE JSON object and nothing else — no prose, no markdown fences. Shape:
` + wireContract + `
Empty lists are fine. Emit nothing rather than something you cannot quote.`
}

// promptEntity is one row of the entity list the model references against.
type promptEntity struct {
	ID   string
	Kind string
	Name string
	// Aliases are the entity's non-canonical names, so "Thomas Vane" and
	// "Tom the innkeeper" both resolve to the same id.
	Aliases []string
}

// promptFact is one row of the live-fact list a journal extraction resolves
// beliefs against (MAD-489): the raw triple plus the statement, exactly what
// the deterministic join keys on.
type promptFact struct {
	ID            string
	Subject       string
	Predicate     string
	ObjectEntity  string
	ObjectLiteral string
	Statement     string
	Visibility    string
}

// maxPromptFacts bounds the fact list a journal prompt carries. Deterministic
// (oldest first), so the same campaign state always produces the same list
// and checksum; a campaign past the cap shows it in the prompt.
const maxPromptFacts = 400

// JournalSourceKind is the source kind beliefs are extracted from. Local
// constant because the canon engine reads sources through its own SQL and
// does not import the gamesession vocabulary.
const JournalSourceKind = "player_journal"

// taskContext is everything the user prompt renders beyond the chunk itself.
type taskContext struct {
	CampaignName  string
	CampaignClock int64
	SourceKind    string
	SourceAuthor  string
	SourceTitle   string
	Entities      []promptEntity
	Roster        []promptEntity // the party's pcs, subset of Entities
	RelTypes      []string
	// Facts is the campaign's live fact list, loaded for journal sources —
	// the join beliefs resolve against and the list the prompt shows.
	Facts []promptFact
}

// userPrompt renders the per-chunk task message: the vocabularies, the
// entity list, the roster, and the chunk with its byte range called out so
// the model knows exactly what text it was given.
func userPrompt(tctx taskContext, chunkText string, chunkStart, chunkEnd int64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "TASK: Extract canon candidates from this chunk of a session source.\n\n")

	fmt.Fprintf(&b, "Campaign: %s (in-world day %d)\n", tctx.CampaignName, tctx.CampaignClock)
	fmt.Fprintf(&b, "Source: %s", tctx.SourceKind)
	if tctx.SourceAuthor != "" {
		fmt.Fprintf(&b, " by %s", tctx.SourceAuthor)
	}
	if tctx.SourceTitle != "" {
		fmt.Fprintf(&b, " — %s", tctx.SourceTitle)
	}
	b.WriteString("\n\n")

	b.WriteString("Allowed relationship rel_types:\n")
	b.WriteString(strings.Join(tctx.RelTypes, ", "))
	b.WriteString("\n\n")

	b.WriteString("PARTY ROSTER (the player characters; discoveries may name them by id, or 'party' for the group):\n")
	for _, e := range tctx.Roster {
		fmt.Fprintf(&b, "- %s (%s, %s)\n", e.ID, e.Kind, e.Name)
	}
	b.WriteString("\n")

	b.WriteString("CAMPAIGN ENTITIES (reference these by id; do not redefine them):\n")
	if len(tctx.Entities) == 0 {
		b.WriteString("(none yet — everyone the chunk names is a new_entities candidate)\n")
	}
	for _, e := range tctx.Entities {
		line := fmt.Sprintf("- %s (%s, %s", e.ID, e.Kind, e.Name)
		if len(e.Aliases) > 0 {
			line += "; aka " + strings.Join(e.Aliases, ", ")
		}
		line += ")\n"
		b.WriteString(line)
	}
	b.WriteString("\n")

	// A journal is a player's account, not narration. The facts list is the
	// join beliefs resolve against: the campaign's current record, shown so
	// the model reuses recorded predicates and characterizes stances against
	// what canon says. Only journal sources carry it — transcripts and notes
	// stage facts, they do not hold beliefs about them.
	if tctx.SourceKind == JournalSourceKind {
		author := tctx.SourceAuthor
		if e, ok := entityByRef(tctx.Entities, tctx.SourceAuthor); ok {
			author = fmt.Sprintf("%s (%s)", e.ID, e.Name)
		}
		fmt.Fprintf(&b, "THIS SOURCE IS A PLAYER'S JOURNAL — the author's own account, not narration:\n")
		if author != "" {
			fmt.Fprintf(&b, "- the author is %s: discoveries in this journal are the author's (discovered_by the author), and claims the journal asserts as truth are the author's beliefs.\n", author)
		} else {
			b.WriteString("- discoveries in this journal are the author's, and claims the journal asserts as truth are the author's beliefs.\n")
		}
		b.WriteString("- a claim about a subject+predicate the campaign already records (below) is a beliefs entry, never a facts entry — the author believing the campaign is wrong about something is itself the record.\n\n")

		b.WriteString("CAMPAIGN FACTS (what the campaign currently records — beliefs concern these; reuse their predicates):\n")
		if len(tctx.Facts) == 0 {
			b.WriteString("(nothing recorded yet — every claim in this journal is new material: facts + discoveries)\n")
		}
		for _, f := range tctx.Facts {
			object := f.ObjectEntity
			if object == "" {
				object = f.ObjectLiteral
			}
			mark := ""
			if f.Visibility == "secret" {
				mark = " [secret]"
			}
			fmt.Fprintf(&b, "- %s — %s — %s — %q%s\n", f.Subject, f.Predicate, object, f.Statement, mark)
		}
		if len(tctx.Facts) >= maxPromptFacts {
			fmt.Fprintf(&b, "(first %d facts shown, oldest first)\n", maxPromptFacts)
		}
		b.WriteString("\n")
	}

	fmt.Fprintf(&b, "CHUNK (session source bytes %d..%d — quote ONLY from this text):\n", chunkStart, chunkEnd)
	b.WriteString(chunkText)
	b.WriteString("\n\nExtract the facts, events, discoveries, relationships and new entities this chunk supports, following every rule in the system message. Return only the JSON object.")
	return b.String()
}

// entityByRef finds an entity by id in the prompt list.
func entityByRef(entities []promptEntity, ref string) (promptEntity, bool) {
	for _, e := range entities {
		if e.ID == ref {
			return e, true
		}
	}
	return promptEntity{}, false
}

// sortEntityList orders the entity list by name for a stable prompt — the
// same source and campaign state must produce the same input checksum, and
// map iteration or insertion-order drift would re-extract for no reason.
func sortEntityList(entities []promptEntity) {
	sort.SliceStable(entities, func(i, j int) bool {
		if entities[i].Name != entities[j].Name {
			return entities[i].Name < entities[j].Name
		}
		return entities[i].ID < entities[j].ID
	})
}
