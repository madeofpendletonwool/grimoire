// Package homebrew is the homebrew linter (MAD-385): a reviewer, not a
// referee.
//
// The framing this issue was written against — "the strongest reuse of the
// grounded rules corpus" — needs correcting before any code claims it:
// internal/rulings is Scryfall, Magic: the Gathering, and has nothing to do
// with D&D; the D&D corpus in internal/index is the 5e SRD as indexed
// prose, with no machine-checkable model of any mechanic behind it. Nothing
// in this app can decide whether a feat creates an infinite loop, because
// nothing in this app models what a feat does. A linter that claimed to
// would be the most confidently wrong surface Grimoire ships.
//
// So the linter runs exactly the three checks the app can stand behind,
// and says plainly, everywhere it travels, that it is a reviewer and not a
// referee:
//
//  1. Numbers, against the maths that exists. For a monster,
//     statblock.ComputeCR — the offensive/defensive split and the specific
//     shortfall against the requested CR. For an item, the corpus rarity
//     distribution — "every +3 weapon in the SRD is Legendary". Checkable
//     claims with a stated basis.
//  2. Structure and vocabulary, against declared schemas. Deterministic
//     rules over the structured forms the monster and item designers
//     already enforce: a condition outside the game's conditions, damage
//     types the game does not have, usage grammar that does not parse,
//     and — the honest, bounded version of "infinite loop" — a recharge
//     cycle with no cost: an ability that restores the resource it spends,
//     detectable because the resource grammar is declared. A narrow
//     structural check with a named scope, not general loop detection.
//  3. "What is this closest to?", which is what retrieval is for. The
//     nearest official mechanics from the indexed corpus, side by side
//     with the numbers each one uses, deep-linked into the reader.
//
// The model's role is to write the comparison up from retrieved passages
// and computed findings. It may not originate a finding, and it may not
// assert a rules claim without a citation or a computed basis — the same
// constraint the encounter builder puts on it. Its prose is gated: any
// figure that traces to nothing the engine produced, or a legal/illegal
// verdict, rejects the whole write-up. Findings are computed before the
// model runs and cannot be altered by it, structurally.
//
// Findings, not a verdict: each finding carries a severity (error —
// structurally invalid; warning — disagrees with the maths; note — worth a
// look), what produced it (computed, structural, or retrieved), and its
// citation or arithmetic. No response at any layer expresses a legal or
// illegal verdict — there is no field for one to live in.
package homebrew

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/data"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/items"
	"github.com/madeofpendletonwool/grimoire/internal/statblock"
)

/* ---------- findings ---------- */

// Severity of a finding. error means structurally invalid; warning means it
// disagrees with the maths; note means worth a look. The vocabulary matches
// the canon engine's so a DM learns one set of words.
type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
	SeverityNote    Severity = "note"
)

// Origin is what produced a finding.
type Origin string

const (
	// OriginComputed: the DMG arithmetic (or the corpus distribution)
	// produced it, and the basis names the numbers.
	OriginComputed Origin = "computed"
	// OriginStructural: a deterministic rule over a declared schema
	// produced it, and the basis names the rule. These run with no model
	// and no network.
	OriginStructural Origin = "structural"
	// OriginRetrieved: the indexed corpus produced it, and the basis is a
	// citation deep-linked into the reader.
	OriginRetrieved Origin = "retrieved"
)

// Citation points at a corpus document. The number is the record number the
// reader API resolves — the same deep link a search citation carries.
type Citation struct {
	Corpus  string `json:"corpus"`
	Number  string `json:"number"`
	Title   string `json:"title"`
	Snippet string `json:"snippet,omitempty"`
}

// Basis is why a finding exists. Every finding carries one, and every
// basis carries either its arithmetic, the rule it enforced, or a
// citation — a test holds that line at the type boundary.
type Basis struct {
	Origin Origin `json:"origin"`
	// Arithmetic is the computed basis: the numbers and where they came
	// from, e.g. "defensive CR 9 (168 effective HP at AC 15); offensive
	// CR 4 (47 DPR at +4)".
	Arithmetic string `json:"arithmetic,omitempty"`
	// Rule is the structural basis: the declared schema or vocabulary the
	// finding enforces.
	Rule string `json:"rule,omitempty"`
	// Citation is the retrieved basis.
	Citation *Citation `json:"citation,omitempty"`
}

// Finding is one lint result. There is deliberately no field that states,
// computes or implies whether the homebrew is "legal" — the linter is a
// reviewer, not a referee.
type Finding struct {
	Check    string   `json:"check"`
	Severity Severity `json:"severity"`
	// Subject locates the finding inside the homebrew: "actions[2]",
	// "effects[0]", "immune", "cr", "rarity".
	Subject string `json:"subject,omitempty"`
	Message string `json:"message"`
	Basis   Basis  `json:"basis"`
}

/* ---------- check codes ---------- */

// Check codes are stable strings, the way the canon engine's are.
const (
	CheckMonsterIdentity    = "monster.structure.identity"
	CheckMonsterScores      = "monster.structure.ability_scores"
	CheckMonsterSaves       = "monster.structure.save_skill_keys"
	CheckMonsterDefense     = "monster.structure.defense"
	CheckMonsterMovement    = "monster.structure.movement"
	CheckMonsterDamage      = "monster.structure.damage_vocabulary"
	CheckMonsterSaveAbility = "monster.structure.save_ability"
	CheckMonsterUsage       = "monster.structure.usage_grammar"
	CheckMonsterCycle       = "monster.structure.recharge_cycle"
	CheckMonsterCR          = "monster.cr_disagrees"
	CheckMonsterConfidence  = "monster.cr_confidence"

	CheckItemDesign    = "item.structure.design_rule"
	CheckItemDamage    = "item.structure.damage_vocabulary"
	CheckItemCondition = "item.structure.condition_vocabulary"
	CheckItemCycle     = "item.structure.recharge_cycle"
	CheckItemRarity    = "item.rarity_disagrees"
	CheckItemUnmatched = "item.rarity_unmatched"

	CheckNearest = "retrieval.nearest"
)

/* ---------- the report ---------- */

// Neighbour is one official mechanic the indexed corpus holds closest to
// the homebrew. Number is the reader-resolvable record number — the deep
// link works the way search citations do.
type Neighbour struct {
	Title   string  `json:"title"`
	Number  string  `json:"number"`
	Corpus  string  `json:"corpus"`
	Snippet string  `json:"snippet,omitempty"`
	Score   float64 `json:"score"`
}

// Report is the linter's whole answer. Findings carry their basis; the
// neighbours are the retrieval result; notices are engine-level statements
// (an empty index, an empty mirror) that are about the run, not the
// homebrew. Nothing here, at any layer, expresses a legal or illegal
// verdict — there is no field for one to live in.
type Report struct {
	Kind       string      `json:"kind"` // "monster" | "item"
	Name       string      `json:"name"`
	Findings   []Finding   `json:"findings"`
	Neighbours []Neighbour `json:"neighbours,omitempty"`
	Notices    []string    `json:"notices,omitempty"`

	// WriteUp is the model's comparison, written from the retrieved
	// passages and the computed findings — never from its own authority.
	// WrittenUp records what became of it: "written" when the prose gate
	// passed, "rejected" (with WriteUpNote saying why) when the model
	// asserted a number the engine never produced or handed down a
	// legal/illegal verdict, and "unavailable" when no model is
	// configured or the call failed. The gate fails closed: a rejected
	// write-up is not shown.
	WriteUp     string `json:"write_up,omitempty"`
	WrittenUp   string `json:"write_up_state,omitempty"`
	WriteUpNote string `json:"write_up_note,omitempty"`
}

// hasBasis reports whether a finding carries what the contract requires.
// It is exported for the test that holds the line at the type boundary:
// no finding reaches an API response without a basis.
func (f Finding) hasBasis() bool {
	if f.Basis.Origin == "" {
		return false
	}
	switch f.Basis.Origin {
	case OriginComputed:
		return strings.TrimSpace(f.Basis.Arithmetic) != ""
	case OriginStructural:
		return strings.TrimSpace(f.Basis.Rule) != ""
	case OriginRetrieved:
		return f.Basis.Citation != nil && f.Basis.Citation.Number != ""
	}
	return false
}

/* ---------- the engine ---------- */

// Engine runs the linter. Both optional dependencies degrade honestly:
// a nil Index skips retrieval (with a notice), a nil Model skips the
// write-up (marked unavailable). The structural and computed checks need
// neither — they are deterministic and run anywhere.
type Engine struct {
	// Index is the FTS store over the SRD. nil means retrieval is off.
	Index *index.Store
	// Corpus selects which corpus retrieval reads. Defaults to D&D.
	Corpus data.Corpus
	// Model, when set, writes the comparison up. It can neither originate
	// nor alter a finding; see writeup.go for the gate that holds.
	Model ModelClient
}

// MonsterInput is one homebrew monster to lint: the structured statblock
// and the CR the brief asked for, if any. The engine recomputes the CR
// itself — the stored rating is never trusted over the arithmetic.
type MonsterInput struct {
	Statblock   statblock.Statblock
	RequestedCR string
}

// ItemInput is one homebrew item to lint: the structured design and the
// corpus to place it against (the catalog's whole shelf). An empty corpus
// skips the rarity comparison with a notice — there is nothing to compare
// against, and a claim without a corpus behind it would be a guess.
type ItemInput struct {
	Design items.Design
	Corpus []items.Item
}

// LintMonster runs the three checks over one homebrew monster. It never
// fails: unavailability degrades into notices, because a reviewer that
// cannot run half its checks owes the DM that sentence, not an error.
func (e *Engine) LintMonster(ctx context.Context, in MonsterInput) *Report {
	sb := in.Statblock
	rep := &Report{Kind: "monster", Name: sb.Name, Findings: []Finding{}}

	// Structure and vocabulary first: deterministic, no model, no network.
	rep.Findings = append(rep.Findings, lintMonsterStructure(sb)...)

	// Numbers: the CR arithmetic, recomputed here so the finding's basis
	// is this run's own arithmetic.
	rating := statblock.ComputeCR(sb)
	rep.Findings = append(rep.Findings, lintMonsterCR(sb, in.RequestedCR, rating)...)

	e.retrieve(ctx, monsterQueries(sb), rep)
	e.writeUp(ctx, rep, in.RequestedCR, rating)
	return rep
}

// LintItem runs the three checks over one homebrew item.
func (e *Engine) LintItem(ctx context.Context, in ItemInput) *Report {
	d := in.Design
	rep := &Report{Kind: "item", Name: strings.TrimSpace(d.Name), Findings: []Finding{}}

	rep.Findings = append(rep.Findings, lintItemStructure(d)...)

	if len(in.Corpus) == 0 {
		rep.Notices = append(rep.Notices,
			"the item mirror is empty — the rarity comparison needs the SRD shelf, so it was skipped rather than guessed")
	} else {
		rep.Findings = append(rep.Findings, lintItemRarity(d, in.Corpus)...)
	}

	metrics := items.MetricsOfDesign(d)
	e.retrieve(ctx, itemQueries(d), rep)
	e.writeUp(ctx, rep, strings.TrimSpace(d.Rarity), metrics)
	return rep
}

/* ---------- retrieval ---------- */

// retrieve runs the nearest-mechanic search and pins the top neighbour as
// a retrieved finding. Every failure degrades to a notice: a reviewer that
// cannot reach the shelf says so and keeps its computed findings.
func (e *Engine) retrieve(ctx context.Context, queries []string, rep *Report) {
	if e.Index == nil || len(queries) == 0 {
		return
	}
	corpus := e.Corpus
	if corpus == "" {
		corpus = data.CorpusDND
	}
	const want = 3
	best := map[string]Neighbour{}
	searched := false
	for _, q := range queries {
		if strings.TrimSpace(q) == "" {
			continue
		}
		hits, err := e.Index.Search(ctx, corpus, q, 8)
		if err != nil {
			rep.Notices = append(rep.Notices, fmt.Sprintf(
				"retrieval could not run (%v) — the computed and structural findings stand on their own", err))
			return
		}
		searched = true
		for _, h := range hits {
			if prev, ok := best[h.Number]; ok && prev.Score >= h.Score {
				continue
			}
			best[h.Number] = Neighbour{
				Title:   h.Title,
				Number:  h.Number,
				Corpus:  string(corpus),
				Snippet: snippet(h.Body),
				Score:   h.Score,
			}
		}
	}
	if !searched {
		return
	}
	out := make([]Neighbour, 0, len(best))
	for _, n := range best {
		out = append(out, n)
	}
	// Deterministic order: score ascending (FTS rank is a distance), then
	// title, so the same input always yields the same report.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score < out[j].Score
		}
		return out[i].Title < out[j].Title
	})
	if len(out) > want {
		out = out[:want]
	}
	rep.Neighbours = out
	if len(out) > 0 {
		top := out[0]
		rep.Findings = append(rep.Findings, Finding{
			Check:    CheckNearest,
			Severity: SeverityNote,
			Message: fmt.Sprintf(
				"Closest official material on the shelf: %q — retrieved from the indexed SRD, deep-linked for side-by-side reading.",
				top.Title),
			Basis: Basis{
				Origin: OriginRetrieved,
				Citation: &Citation{
					Corpus: top.Corpus, Number: top.Number,
					Title: top.Title, Snippet: top.Snippet,
				},
			},
		})
	} else {
		rep.Notices = append(rep.Notices,
			"retrieval found no official material close to this — the index may be empty or the query too narrow")
	}
}

// snippet trims a corpus body into a readable neighbourhood.
func snippet(body string) string {
	body = strings.Join(strings.Fields(body), " ")
	if len(body) > 240 {
		body = body[:240]
		if i := strings.LastIndexByte(body, ' '); i > 0 {
			body = body[:i]
		}
		body += "…"
	}
	return body
}

/* ---------- retrieval queries ---------- */

// monsterQueries builds the deterministic retrieval queries for a
// statblock: the name, the creature type with its signature damage type,
// and the strongest parsed action with its damage. No model is asked.
func monsterQueries(sb statblock.Statblock) []string {
	var queries []string
	if name := queryWords(sb.Name); name != "" {
		queries = append(queries, name)
	}
	sig := signatureDamage(sb)
	if t := queryWords(sb.Type); t != "" {
		q := t
		if sig != "" {
			q += " " + sig
		}
		queries = append(queries, q)
	}
	for _, a := range sb.Actions {
		if !a.Parsed || !a.Attack.Parsed() {
			continue
		}
		if n := queryWords(a.Name); n != "" {
			q := n
			if sig != "" {
				q += " " + sig
			}
			queries = append(queries, q)
			break
		}
	}
	return dedupe(queries)
}

// itemQueries builds the retrieval queries for a design: the name, the
// type with its base item, and any named spell an effect produces.
func itemQueries(d items.Design) []string {
	var queries []string
	if name := queryWords(d.Name); name != "" {
		queries = append(queries, name)
	}
	if t := queryWords(d.Type); t != "" {
		q := t
		if base := queryWords(d.Base); base != "" {
			q += " " + base
		}
		queries = append(queries, q)
	}
	for _, e := range d.Effects {
		if s := queryWords(e.Spell); s != "" {
			queries = append(queries, s)
		}
	}
	return dedupe(queries)
}

// queryWords squashes a phrase into the lowercase words an FTS query wants:
// letters and digits only, parentheses and punctuation dropped.
func queryWords(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '('); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte(' ')
		}
	}
	fields := strings.Fields(b.String())
	// A query of many words ANDs into near-certain silence; keep the four
	// most distinctive leading words.
	if len(fields) > 4 {
		fields = fields[:4]
	}
	return strings.Join(fields, " ")
}

// signatureDamage picks the statblock's most repeated damage type — the
// word that makes a retrieval query find its relatives.
func signatureDamage(sb statblock.Statblock) string {
	count := map[string]int{}
	best, bestN := "", 0
	note := func(ts []statblock.Damage) {
		for _, d := range ts {
			t := strings.ToLower(strings.TrimSpace(d.Type))
			if t == "" {
				continue
			}
			count[t]++
			if count[t] > bestN {
				best, bestN = t, count[t]
			}
		}
	}
	for _, a := range sb.Actions {
		note(a.Attack.Damage)
	}
	// Fall back to the resistance profile: a cold-themed creature is
	// usually immune to cold before it deals it.
	for _, clause := range sb.Immune {
		if t := matchDamageType(clause); t != "" {
			if count[t]++; count[t] > bestN {
				best, bestN = t, count[t]
			}
		}
	}
	return best
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// writeUpJSON is the context the model writes from. It is marshalled into
// the prompt, and it is also the gate's allowlist source: any number the
// write-up asserts must appear here, because here is everything the engine
// knows.
type writeUpJSON struct {
	Kind       string      `json:"kind"`
	Name       string      `json:"name"`
	Requested  string      `json:"requested,omitempty"` // the brief's CR, or the DM's rarity label
	Findings   []Finding   `json:"findings"`
	Neighbours []Neighbour `json:"neighbours,omitempty"`
	Numbers    any         `json:"numbers,omitempty"`
}

// contextJSON marshals the engine's own output for both the prompt and the
// gate. Marshalling once and reusing the bytes guarantees the gate's
// allowlist is exactly what the model was shown.
func contextJSON(rep *Report, requested string, numbers any) []byte {
	ctx := writeUpJSON{
		Kind: rep.Kind, Name: rep.Name, Requested: requested,
		Findings: rep.Findings, Neighbours: rep.Neighbours, Numbers: numbers,
	}
	b, err := json.Marshal(ctx)
	if err != nil {
		// The context is plain data; a marshal failure is a programming
		// error. Fail the write-up, never the findings.
		return nil
	}
	return b
}
