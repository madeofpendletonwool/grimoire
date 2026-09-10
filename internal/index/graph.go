package index

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/data"
)

// The Comprehensive Rules are a graph, not a bag of paragraphs. A rule that
// decides an interaction is routinely named only by a cross-reference from the
// rule the search actually matched: nothing in 702.11 ("Hexproof") mentions
// when a spell's targets are rechecked, but 115.10 points at 608, and the
// glossary entry for "Illegal Target" points straight at 608.2b. Retrieval
// that only ranks paragraphs by keyword overlap can never make that hop, so
// the model is handed the rules that share the question's vocabulary and none
// of the rules that answer it.
//
// Two derived tables close that gap, both built from the rule text itself
// rather than from a hand-written list that would rot at the next CR update:
//
//   rule_xrefs  src rule -> every rule its text cites ("see rule 608.2b")
//   rule_terms  a name -> the rules that define it, from the glossary's own
//               "See rule ..." pointers and from the keyword-ability chapters,
//               where rule 702.6's body is the single word "Equip"
//
// Terms are what make a card's oracle text addressable: "Equipment" in a type
// line resolves through the glossary to 301 and 702.6 without anyone hardcoding
// that mapping.

const graphSchema = `
CREATE TABLE IF NOT EXISTS rule_xrefs (
	corpus TEXT NOT NULL,
	src    TEXT NOT NULL,
	dst    TEXT NOT NULL,
	PRIMARY KEY (corpus, src, dst)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS rule_terms (
	corpus TEXT NOT NULL,
	term   TEXT NOT NULL,
	number TEXT NOT NULL,
	PRIMARY KEY (corpus, term, number)
) WITHOUT ROWID;
`

// graphBuildDDL is the staging twin of the graph tables, so a rebuild can
// stage in chunked transactions and swap atomically alongside docs.
const graphBuildDDL = `
CREATE TABLE IF NOT EXISTS rule_xrefs_build (
	corpus TEXT NOT NULL,
	src    TEXT NOT NULL,
	dst    TEXT NOT NULL,
	PRIMARY KEY (corpus, src, dst)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS rule_terms_build (
	corpus TEXT NOT NULL,
	term   TEXT NOT NULL,
	number TEXT NOT NULL,
	PRIMARY KEY (corpus, term, number)
) WITHOUT ROWID;
`

// dottedRuleRe matches a fully qualified rule number wherever it appears in
// rule prose. In the CR a three-digit dotted number is always a citation —
// ordinary numbers in rule text (life totals, power) never reach three digits
// before a decimal point — so these need no "see rule" preamble to be trusted.
var dottedRuleRe = regexp.MustCompile(`\b\d{3}\.\d+[a-z]?\b`)

// chapterRefRe matches a citation of a whole chapter, which is only
// recognizable by the word introducing it: "see rule 301, \"Artifacts\"".
// The optional trailing group is not thrown away — it is how a dotted citation
// is told apart from a chapter one. RE2 has no lookahead, so without it
// "rule 702.11" would also read as a citation of all of chapter 702.
var chapterRefRe = regexp.MustCompile(`(?i)\brules?\s+(\d{3})(\.\d+[a-z]?)?`)

// keywordRuleRe matches the parent rule of a keyword action (701) or keyword
// ability (702) — the rule whose entire body is the keyword's name. The
// lettered sub-rules that follow it are prose and must not match.
var keywordRuleRe = regexp.MustCompile(`^70[12]\.\d+$`)

// maxTermWords bounds what counts as a keyword name. Real ones are one to four
// words ("Equip", "First Strike", "Living Weapon"); anything longer is a rule
// that happens to be short, not a name.
const maxTermWords = 4

// xrefTargets returns the rules a rule's body cites, excluding self-references
// and citations of the rule's own ancestors — a rule pointing at its own
// section teaches retrieval nothing it doesn't already have.
func xrefTargets(number, body string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(t string) {
		if t == "" || seen[t] || t == number {
			return
		}
		// "608.2" cited from inside 608.2b is the section already in hand.
		if number != "" && strings.HasPrefix(number, t) {
			return
		}
		seen[t] = true
		out = append(out, t)
	}
	for _, m := range dottedRuleRe.FindAllString(body, -1) {
		add(m)
	}
	for _, m := range chapterRefRe.FindAllStringSubmatch(body, -1) {
		if m[2] != "" {
			continue // a dotted citation; already collected above
		}
		add(m[1])
	}
	return out
}

// termName returns the name a record defines, or "" if it defines none. Two
// shapes carry one: a glossary entry, whose title is the term and whose number
// is empty, and a keyword-chapter parent rule, whose body is the term.
func termName(number, title, body string) string {
	if number == "" {
		return strings.TrimSpace(title)
	}
	if !keywordRuleRe.MatchString(number) {
		return ""
	}
	b := strings.TrimSpace(body)
	if b == "" || strings.HasSuffix(b, ".") || len(strings.Fields(b)) > maxTermWords {
		return ""
	}
	return b
}

// termEntry is one name and the rules it resolves to.
type termEntry struct {
	term    string
	numbers []string
}

// buildGraph derives the cross-reference and term tables from a dataset's
// records. It is pure so the extraction can be tested without a database.
func buildGraph(recs []data.Record) (xrefs map[data.Corpus][][2]string, terms map[data.Corpus][]termEntry) {
	xrefs = map[data.Corpus][][2]string{}
	byTerm := map[data.Corpus]map[string][]string{}
	for _, r := range recs {
		if r.Number != "" {
			for _, dst := range xrefTargets(r.Number, r.Body) {
				xrefs[r.Corpus] = append(xrefs[r.Corpus], [2]string{r.Number, dst})
			}
		}
		name := termName(r.Number, r.Title, r.Body)
		if name == "" {
			continue
		}
		var targets []string
		if r.Number != "" {
			targets = []string{r.Number} // a keyword rule defines itself
		} else {
			targets = xrefTargets("", r.Body) // a glossary entry points elsewhere
		}
		if len(targets) == 0 {
			continue
		}
		if byTerm[r.Corpus] == nil {
			byTerm[r.Corpus] = map[string][]string{}
		}
		key := strings.ToLower(name)
		for _, t := range targets {
			byTerm[r.Corpus][key] = appendUnique(byTerm[r.Corpus][key], t)
		}
	}
	terms = map[data.Corpus][]termEntry{}
	for c, m := range byTerm {
		for term, numbers := range m {
			terms[c] = append(terms[c], termEntry{term: term, numbers: numbers})
		}
		sort.Slice(terms[c], func(i, j int) bool { return terms[c][i].term < terms[c][j].term })
	}
	return xrefs, terms
}

func appendUnique(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

// graphChunkSize bounds one staging transaction, for the same reason as
// docsChunkSize: every store shares one SQLite connection, so a rebuild that
// wrote the whole graph in a single transaction would hold up live requests.
var graphChunkSize = 2000

// EnsureGraph derives the cross-reference graph from the docs already indexed,
// when an install has rules but no graph. It reports whether it built one.
//
// It exists for the upgrade: an index built before the graph existed is
// perfectly good rules data, and refetching the rulebooks over the network to
// derive something already computable from the local table would be absurd.
// Without it the graph tables sit empty and every citation hop silently does
// nothing, which is the worst kind of failure — a feature that is off and says
// so nowhere.
func (s *Store) EnsureGraph(ctx context.Context) (bool, error) {
	var xrefs, docs int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM rule_xrefs`).Scan(&xrefs); err != nil {
		return false, fmt.Errorf("graph check: %w", err)
	}
	if xrefs > 0 {
		return false, nil
	}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM docs`).Scan(&docs); err != nil {
		return false, fmt.Errorf("graph check: %w", err)
	}
	if docs == 0 {
		return false, nil // nothing indexed yet; the next build makes the graph
	}

	recs, err := s.allRecords(ctx)
	if err != nil {
		return false, err
	}
	if err := s.stageGraphRecords(ctx, recs); err != nil {
		s.dropStaging()
		return false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := swapGraph(ctx, tx); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// allRecords reads the indexed docs back out as records, so derived tables can
// be rebuilt without the original download.
func (s *Store) allRecords(ctx context.Context) ([]data.Record, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT corpus, number, title, body, source FROM docs`)
	if err != nil {
		return nil, fmt.Errorf("read docs: %w", err)
	}
	defer rows.Close()
	var out []data.Record
	for rows.Next() {
		var r data.Record
		var corpus string
		if err := rows.Scan(&corpus, &r.Number, &r.Title, &r.Body, &r.Source); err != nil {
			return nil, err
		}
		r.Corpus = data.Corpus(corpus)
		out = append(out, r)
	}
	return out, rows.Err()
}

// stageGraph writes the derived graph into the staging tables in chunked
// transactions. The live tables are untouched until swapDocs renames these in.
func (s *Store) stageGraph(ctx context.Context, ds *data.Dataset) error {
	return s.stageGraphRecords(ctx, ds.Records)
}

func (s *Store) stageGraphRecords(ctx context.Context, recs []data.Record) error {
	if _, err := s.db.ExecContext(ctx,
		`DROP TABLE IF EXISTS rule_xrefs_build; DROP TABLE IF EXISTS rule_terms_build; `+graphBuildDDL); err != nil {
		return fmt.Errorf("stage graph: %w", err)
	}
	xrefs, terms := buildGraph(recs)

	var edges [][3]string
	for c, list := range xrefs {
		for _, e := range list {
			edges = append(edges, [3]string{string(c), e[0], e[1]})
		}
	}
	if err := s.insertGraphChunks(ctx,
		`INSERT OR IGNORE INTO rule_xrefs_build(corpus, src, dst) VALUES(?, ?, ?)`, edges); err != nil {
		return err
	}

	var rows [][3]string
	for c, list := range terms {
		for _, t := range list {
			for _, n := range t.numbers {
				rows = append(rows, [3]string{string(c), t.term, n})
			}
		}
	}
	return s.insertGraphChunks(ctx,
		`INSERT OR IGNORE INTO rule_terms_build(corpus, term, number) VALUES(?, ?, ?)`, rows)
}

func (s *Store) insertGraphChunks(ctx context.Context, insert string, rows [][3]string) error {
	for start := 0; start < len(rows); start += graphChunkSize {
		end := min(start+graphChunkSize, len(rows))
		if err := s.insertGraphChunk(ctx, insert, rows[start:end]); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) insertGraphChunk(ctx context.Context, insert string, rows [][3]string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, insert)
	if err != nil {
		return fmt.Errorf("prepare graph insert: %w", err)
	}
	defer stmt.Close()
	for _, r := range rows {
		if _, err := stmt.ExecContext(ctx, r[0], r[1], r[2]); err != nil {
			return fmt.Errorf("insert graph row: %w", err)
		}
	}
	return tx.Commit()
}

// swapGraph replaces the live graph tables with the staged ones. It runs inside
// the same transaction as the docs swap, so the graph can never describe a
// version of the rules the index no longer holds.
func swapGraph(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `
		DROP TABLE IF EXISTS rule_xrefs;
		DROP TABLE IF EXISTS rule_terms;
		ALTER TABLE rule_xrefs_build RENAME TO rule_xrefs;
		ALTER TABLE rule_terms_build RENAME TO rule_terms;`)
	if err != nil {
		return fmt.Errorf("swap graph: %w", err)
	}
	return nil
}

// Xrefs returns the rules cited by any of the given rules, in first-seen order
// and excluding the inputs themselves. An empty input, or a corpus with no
// graph built, yields nothing rather than an error: cross-reference expansion
// is an enrichment, never a precondition for answering.
func (s *Store) Xrefs(ctx context.Context, corpus data.Corpus, numbers []string) ([]string, error) {
	if len(numbers) == 0 {
		return nil, nil
	}
	have := map[string]bool{}
	args := []any{string(corpus)}
	holes := make([]string, 0, len(numbers))
	for _, n := range numbers {
		if n == "" || have[n] {
			continue
		}
		have[n] = true
		holes = append(holes, "?")
		args = append(args, n)
	}
	if len(holes) == 0 {
		return nil, nil
	}
	q := `SELECT src, dst FROM rule_xrefs WHERE corpus = ? AND src IN (` + strings.Join(holes, ",") + `)`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("xrefs: %w", err)
	}
	defer rows.Close()

	// Group by source so the ordering follows the input's rank order: the
	// top-ranked seed's citations are the ones most worth the budget.
	bySrc := map[string][]string{}
	for rows.Next() {
		var src, dst string
		if err := rows.Scan(&src, &dst); err != nil {
			return nil, err
		}
		bySrc[src] = append(bySrc[src], dst)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []string
	seen := map[string]bool{}
	for _, n := range numbers {
		dsts := bySrc[n]
		sort.Strings(dsts)
		for _, d := range dsts {
			if have[d] || seen[d] {
				continue
			}
			seen[d] = true
			out = append(out, d)
		}
	}
	return out, nil
}

// termChunk bounds one term query's variable count, well under SQLite's
// parameter limit.
const termChunk = 200

// TermRules resolves names to the rules that define them. The caller passes
// every phrase a question (or a card's oracle text) could plausibly name; this
// keeps the ones the rules actually define, in the order the caller offered
// them, so the most specific phrase a caller tried leads.
//
// It is queried per question rather than cached in memory because the term
// index is rebuilt by a reindex, and a cached copy would keep answering from
// the rules of a previous CR release.
func (s *Store) TermRules(ctx context.Context, corpus data.Corpus, phrases []string) ([]string, error) {
	want := make([]string, 0, len(phrases))
	seen := map[string]bool{}
	for _, p := range phrases {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		want = append(want, p)
	}
	if len(want) == 0 {
		return nil, nil
	}

	byTerm := map[string][]string{}
	for start := 0; start < len(want); start += termChunk {
		end := min(start+termChunk, len(want))
		chunk := want[start:end]
		args := make([]any, 0, len(chunk)+1)
		args = append(args, string(corpus))
		holes := make([]string, len(chunk))
		for i, p := range chunk {
			holes[i] = "?"
			args = append(args, p)
		}
		rows, err := s.db.QueryContext(ctx,
			`SELECT term, number FROM rule_terms WHERE corpus = ? AND term IN (`+strings.Join(holes, ",")+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("term rules: %w", err)
		}
		for rows.Next() {
			var term, number string
			if err := rows.Scan(&term, &number); err != nil {
				rows.Close()
				return nil, err
			}
			byTerm[term] = append(byTerm[term], number)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}

	var out []string
	added := map[string]bool{}
	for _, p := range want {
		nums := byTerm[p]
		sort.Strings(nums)
		for _, n := range nums {
			if added[n] {
				continue
			}
			added[n] = true
			out = append(out, n)
		}
	}
	return out, nil
}

// citedByFrequency returns the rules cited by the given rules, ordered by how
// many of them cite it.
//
// The final expansion hop has only a small reserve to spend, and following
// citations in document order spends it on whatever the first paragraph
// happened to mention. Frequency is a far better signal: when a dozen rules
// gathered for one question all point at the same rule, that rule is the one
// the question keeps circling — and it is usually the one no keyword search
// would ever have ranked.
func (s *Store) citedByFrequency(ctx context.Context, corpus data.Corpus, numbers []string) ([]string, error) {
	if len(numbers) == 0 {
		return nil, nil
	}
	have := map[string]bool{}
	args := []any{string(corpus)}
	holes := make([]string, 0, len(numbers))
	for _, n := range numbers {
		if n == "" || have[n] {
			continue
		}
		have[n] = true
		holes = append(holes, "?")
		args = append(args, n)
	}
	if len(holes) == 0 {
		return nil, nil
	}

	counts := map[string]int{}
	var order []string
	for start := 0; start < len(holes); start += termChunk {
		end := min(start+termChunk, len(holes))
		chunkArgs := append([]any{string(corpus)}, args[1+start:1+end]...)
		q := `SELECT dst FROM rule_xrefs WHERE corpus = ? AND src IN (` +
			strings.Join(holes[start:end], ",") + `)`
		rows, err := s.db.QueryContext(ctx, q, chunkArgs...)
		if err != nil {
			return nil, fmt.Errorf("cited by frequency: %w", err)
		}
		for rows.Next() {
			var dst string
			if err := rows.Scan(&dst); err != nil {
				rows.Close()
				return nil, err
			}
			if have[dst] {
				continue // already in the context
			}
			if counts[dst] == 0 {
				order = append(order, dst)
			}
			counts[dst]++
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	sort.SliceStable(order, func(i, j int) bool { return counts[order[i]] > counts[order[j]] })
	return order, nil
}
