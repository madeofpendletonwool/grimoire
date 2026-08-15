// Package index wraps a SQLite + FTS5 store for rules documents.
package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/data"
	_ "modernc.org/sqlite" // pure-Go SQLite driver (FTS5 enabled)
)

// Store is a searchable FTS5-backed document store.
type Store struct {
	db       *sql.DB
	embedder Embedder // nil => FTS5-only retrieval (the default)
}

// Result is a single search hit.
type Result struct {
	Number string
	Title  string
	Body   string
	Source string
	Score  float64
}

// Open opens (or creates) the SQLite index at path.
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(wal)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1) // SQLite serial writes
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the underlying handle so sibling stores (chat history) can share
// the same SQLite file and connection pool. Reset only clears the docs tables,
// so a reindex leaves those sibling tables intact.
func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) migrate() error {
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	if _, err := s.db.Exec(vectorSchema); err != nil {
		return fmt.Errorf("migrate vectors: %w", err)
	}
	return nil
}

const schema = `
CREATE TABLE IF NOT EXISTS corpus_meta (
	corpus        TEXT PRIMARY KEY,
	name          TEXT NOT NULL,
	version       TEXT NOT NULL,
	source_url    TEXT NOT NULL,
	record_count  INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS index_meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE VIRTUAL TABLE IF NOT EXISTS docs USING fts5(
	corpus UNINDEXED,
	number,
	title,
	body,
	source UNINDEXED,
	tokenize = 'porter unicode61 remove_diacritics 2'
);
CREATE TABLE IF NOT EXISTS card_names (
	name TEXT PRIMARY KEY
);
CREATE TABLE IF NOT EXISTS entity_names (
	name TEXT PRIMARY KEY
);
`

// Reset drops and rebuilds the docs tables (used on reindex).
func (s *Store) Reset() error {
	_, err := s.db.Exec(`DELETE FROM docs; DELETE FROM corpus_meta; DELETE FROM card_names; DELETE FROM entity_names; DELETE FROM doc_vectors;`)
	return err
}

// Index loads a full dataset into the store, replacing prior contents.
func (s *Store) Index(ctx context.Context, ds *data.Dataset) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM docs; DELETE FROM corpus_meta; DELETE FROM doc_vectors;`); err != nil {
		return fmt.Errorf("clear docs: %w", err)
	}

	stmt, err := tx.PrepareContext(ctx, `INSERT INTO docs(corpus, number, title, body, source) VALUES(?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare insert: %w", err)
	}
	defer stmt.Close()

	for _, r := range ds.Records {
		if strings.TrimSpace(r.Body) == "" && strings.TrimSpace(r.Title) == "" {
			continue
		}
		if _, err := stmt.ExecContext(ctx, string(r.Corpus), r.Number, r.Title, r.Body, r.Source); err != nil {
			return fmt.Errorf("insert doc: %w", err)
		}
	}

	metaStmt, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO corpus_meta(corpus, name, version, source_url, record_count) VALUES(?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer metaStmt.Close()
	for c, m := range ds.Meta {
		if _, err := metaStmt.ExecContext(ctx, string(c), m.Name, m.Version, m.SourceURL, m.RecordCount); err != nil {
			return err
		}
	}

	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO index_meta(key, value) VALUES('built_at', ?)`, nowRFC3339()); err != nil {
		return err
	}

	return tx.Commit()
}

// IndexCards replaces the card-name dictionary, populated from MTGJSON's
// AtomicCards during an index build. The chat uses it to spot card mentions
// the text heuristics miss (lowercase, unquoted). Safe to call with an empty
// slice, which clears the table. card_names lives alongside the rules tables
// and is rebuilt by Reset, but is independent of the rules Dataset.
func (s *Store) IndexCards(ctx context.Context, names []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM card_names`); err != nil {
		return fmt.Errorf("clear card_names: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO card_names(name) VALUES(?)`)
	if err != nil {
		return fmt.Errorf("prepare card_names: %w", err)
	}
	defer stmt.Close()
	for _, n := range names {
		if strings.TrimSpace(n) == "" {
			continue
		}
		if _, err := stmt.ExecContext(ctx, n); err != nil {
			return fmt.Errorf("insert card name: %w", err)
		}
	}
	return tx.Commit()
}

// LoadCardNames returns every stored card name, used at server startup to
// build the in-memory detection dictionary.
func (s *Store) LoadCardNames(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM card_names`)
	if err != nil {
		return nil, fmt.Errorf("load card_names: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// IndexEntityNames replaces the D&D entity-name dictionary, populated from
// Open5e's SRD listings during an index build. The D&D chat uses it to spot
// spell/creature/item/feat mentions the text heuristics miss (lowercase,
// unquoted) — the counterpart of the MTG card-name dictionary. Safe to call
// with an empty slice, which clears the table.
func (s *Store) IndexEntityNames(ctx context.Context, names []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM entity_names`); err != nil {
		return fmt.Errorf("clear entity_names: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO entity_names(name) VALUES(?)`)
	if err != nil {
		return fmt.Errorf("prepare entity_names: %w", err)
	}
	defer stmt.Close()
	for _, n := range names {
		if strings.TrimSpace(n) == "" {
			continue
		}
		if _, err := stmt.ExecContext(ctx, n); err != nil {
			return fmt.Errorf("insert entity name: %w", err)
		}
	}
	return tx.Commit()
}

// LoadEntityNames returns every stored D&D entity name, used at server startup
// to build the in-memory detection dictionary.
func (s *Store) LoadEntityNames(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM entity_names`)
	if err != nil {
		return nil, fmt.Errorf("load entity_names: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ruleNumberRe detects a direct rule-number reference like "205.1a".
var ruleNumberRe = regexp.MustCompile(`^\d{1,3}(?:\.\d+)+[a-z]?$`)

// Search runs a full-text query against a corpus. If the query is a bare rule
// number, it performs an exact lookup first.
func (s *Store) Search(ctx context.Context, corpus data.Corpus, query string, limit int) ([]Result, error) {
	q := strings.TrimSpace(query)
	if limit <= 0 {
		limit = 20
	}

	// Direct rule-number lookup (MTG).
	if corpus == data.CorpusMTG && q != "" && ruleNumberRe.MatchString(q) {
		if hits, err := s.lookupNumber(ctx, corpus, q); err == nil && len(hits) > 0 {
			return hits, nil
		}
	}

	matches, err := toFTSQuery(q)
	if err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx, ftsSearchSQL, matches, corpus, limit)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	defer rows.Close()

	var out []Result
	for rows.Next() {
		var r Result
		if err := rows.Scan(&r.Number, &r.Title, &r.Body, &r.Source, &r.Score); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

const ftsSearchSQL = `
SELECT number, title, body, source, rank AS score
FROM docs
WHERE docs MATCH ? AND corpus = ?
ORDER BY rank
LIMIT ?;
`

func (s *Store) lookupNumber(ctx context.Context, corpus data.Corpus, number string) ([]Result, error) {
	// FTS5 supports a column-scoped phrase match on the number column.
	q := fmt.Sprintf("number:%q", number)
	rows, err := s.db.QueryContext(ctx, ftsSearchSQL, q, corpus, 30)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Result
	for rows.Next() {
		var r Result
		if err := rows.Scan(&r.Number, &r.Title, &r.Body, &r.Source, &r.Score); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ruleNumberFullRe matches a numbered rule with at least one sub-level and an
// optional trailing letter run, e.g. "702.2" or "702.2a". Group 1 is the
// numeric section root; group 2 is the trailing letters (may be empty).
var ruleNumberFullRe = regexp.MustCompile(`^(\d{1,3}(?:\.\d+)+)([a-z]+)?$`)

const ftsSectionSQL = `
SELECT number, title, body, source
FROM docs
WHERE docs MATCH ? AND corpus = ?;
`

// Section returns the complete set of rules that share a numbered rule's
// section: the parent rule (e.g. "702.2") plus every lettered sub-rule
// ("702.2a", "702.2b", ...). Results are ordered parent-first then by number.
// A query that is not a numbered rule (or a section with no stored rules)
// returns nil with no error. MTG-style numbered rules only; other corpora
// simply return nil.
func (s *Store) Section(ctx context.Context, corpus data.Corpus, number string) ([]Result, error) {
	num := strings.TrimSpace(number)
	m := ruleNumberFullRe.FindStringSubmatch(num)
	if m == nil {
		return nil, nil
	}
	root := m[1] // e.g. "702.2"
	chapter := strings.SplitN(root, ".", 2)[0]

	// Coarse fetch: every rule in the chapter whose number column leads with
	// the chapter token, then keep only the requested section in Go. The
	// chapter token is digits-only (from the regex), so it is safe to splice
	// into the MATCH expression. Go filtering guarantees we exclude sibling
	// sections (e.g. "702.21" is not part of "702.2").
	match := fmt.Sprintf("number:%s*", chapter)
	rows, err := s.db.QueryContext(ctx, ftsSectionSQL, match, corpus)
	if err != nil {
		return nil, fmt.Errorf("section: %w", err)
	}
	defer rows.Close()

	var out []Result
	for rows.Next() {
		var r Result
		if err := rows.Scan(&r.Number, &r.Title, &r.Body, &r.Source); err != nil {
			return nil, err
		}
		if sectionKey(r.Number) != root {
			continue
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Parent first (the rule whose number is the bare root), then children
	// ascending so "702.2a" precedes "702.2b".
	sort.SliceStable(out, func(i, j int) bool {
		return lessRuleNumber(out[i].Number, out[j].Number)
	})
	return out, nil
}

// lessRuleNumber orders numbered rules the way the rulebook does: each dotted
// segment compares numerically (so "613.2" precedes "613.10"), and a bare rule
// precedes its lettered sub-rules ("702.2" before "702.2a").
func lessRuleNumber(a, b string) bool {
	an, al := splitRuleNumber(a)
	bn, bl := splitRuleNumber(b)
	for i := 0; i < len(an) && i < len(bn); i++ {
		if an[i] != bn[i] {
			return an[i] < bn[i]
		}
	}
	if len(an) != len(bn) {
		return len(an) < len(bn)
	}
	return al < bl
}

// splitRuleNumber breaks "613.10a" into its numeric segments ([613, 10]) and
// its trailing letters ("a"). A number it cannot parse yields no segments, so
// it sorts before parsable ones by its letter tail (the raw string).
func splitRuleNumber(number string) ([]int, string) {
	num := strings.TrimRight(number, "abcdefghijklmnopqrstuvwxyz")
	letters := number[len(num):]
	if num == "" {
		return nil, number
	}
	var segs []int
	for _, part := range strings.Split(num, ".") {
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil, number
		}
		segs = append(segs, n)
	}
	return segs, letters
}

// sectionKey reduces a numbered rule to its section root by stripping any
// trailing letters: "702.2a" -> "702.2", "702.2" -> "702.2".
func sectionKey(number string) string {
	return strings.TrimRight(number, "abcdefghijklmnopqrstuvwxyz")
}

// CorpusMeta returns stored metadata for a corpus.
func (s *Store) CorpusMeta(ctx context.Context) (map[data.Corpus]data.CorpusMeta, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT corpus, name, version, source_url, record_count FROM corpus_meta`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[data.Corpus]data.CorpusMeta{}
	for rows.Next() {
		var c, name, version, url string
		var count int
		if err := rows.Scan(&c, &name, &version, &url, &count); err != nil {
			return nil, err
		}
		out[data.Corpus(c)] = data.CorpusMeta{
			Name: name, Version: version, SourceURL: url, RecordCount: count,
		}
	}
	return out, rows.Err()
}

// SetMeta stores a key/value marker in index_meta (alongside built_at). Used
// for change detection between boots, e.g. the local-books fingerprint.
func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO index_meta(key, value) VALUES(?, ?)`, key, value)
	return err
}

// GetMeta reads a marker written by SetMeta. An absent key yields "" with no
// error.
func (s *Store) GetMeta(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM index_meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// Indexed reports whether the store has any documents.
func (s *Store) Indexed(ctx context.Context) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM docs`).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ErrEmptyQuery is returned when a search query sanitizes to nothing.
var ErrEmptyQuery = errors.New("empty query")

// toFTSQuery converts a free-text query into a safe FTS5 MATCH expression.
// Each whitespace-separated token becomes a prefix phrase; tokens are AND-ed.
func toFTSQuery(q string) (string, error) {
	var parts []string
	for _, tok := range strings.Fields(q) {
		tok = strings.Trim(tok, "\"'*()")
		if tok == "" {
			continue
		}
		// double internal quotes by SQL string literal escaping via %q
		parts = append(parts, fmt.Sprintf("%q*", tok))
	}
	if len(parts) == 0 {
		return "", ErrEmptyQuery
	}
	return strings.Join(parts, " "), nil
}

func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// stopwords are dropped before building RAG retrieval queries so that common
// question words ("what does the …") don't drown out the salient terms.
var stopwords = map[string]bool{
	"the": true, "a": true, "an": true, "and": true, "or": true, "of": true,
	"to": true, "in": true, "is": true, "it": true, "its": true, "for": true,
	"on": true, "with": true, "what": true, "do": true, "does": true, "how": true,
	"why": true, "when": true, "who": true, "can": true, "are": true, "be": true,
	"my": true, "i": true, "you": true, "this": true, "that": true, "at": true,
	"as": true, "if": true, "me": true, "so": true,
}

// toFTSQueryOR builds an OR-joined FTS5 query with stopwords removed. Used for
// RAG retrieval, where recall matters more than precision: bm25 ranking still
// floats the rare, relevant terms (e.g. "ward") above the common ones.
func toFTSQueryOR(q string) (string, error) {
	var parts []string
	for _, tok := range strings.Fields(q) {
		tok = strings.Trim(tok, "\"'*()?,.;:!")
		if tok == "" {
			continue
		}
		if stopwords[strings.ToLower(tok)] {
			continue
		}
		parts = append(parts, fmt.Sprintf("%q*", tok))
	}
	if len(parts) == 0 {
		return "", ErrEmptyQuery
	}
	return strings.Join(parts, " OR "), nil
}

// maxGroundingDocs caps how many rules Expand hands back for one question.
const maxGroundingDocs = 60

// maxGroupDocs is the largest whole rule group we will pull in. Groups above
// it (702, the keyword-ability list, is nearly 800 rules) fall back to
// per-seed section expansion.
const maxGroupDocs = 60

// maxExpandGroups bounds how many whole rule groups one question pulls in.
const maxExpandGroups = 2

// maxChildDocs is the largest subsection tree one seed pulls in. A focused
// section ("Equipment — Weapons", 9 records) and a whole class (29) both fit;
// anything chapter-sized does not, and falls back to the seed's own section.
const maxChildDocs = 40

// maxExpandChildren bounds how many seeds pull their subsection tree, so one
// broad hit cannot spend the whole grounding budget on its descendants.
const maxExpandChildren = 3

// Expand grows a set of search hits into the grounding context handed to the
// Q&A model.
//
// A flat top-N search returns fragments of a mechanic — a layers question
// comes back with 613.6 but not 613.1, the rule that actually lists the
// layers — so the model can only report that the rules it needs are missing.
// Expand fixes that by pulling in whole units around the hits: the rule groups
// the hits cluster in (all of 613), and each remaining hit's own section
// (702.21 plus its sub-rules). Seeds stay first and in rank order; expansion
// follows in rule order.
//
// D&D records carry path-style numbers ("spells/0003/0042.1") and get the
// same treatment through their path keys: whole top-level sections (the group
// key, the analogue of a chapter) for the clusters the search kept hitting,
// then each seed's own section (all chunks of the same heading section).
func (s *Store) Expand(ctx context.Context, corpus data.Corpus, seeds []Result) ([]Result, error) {
	if len(seeds) == 0 {
		return nil, nil
	}
	out := make([]Result, 0, len(seeds))
	seen := map[string]bool{}
	// push adds a doc, reporting false once the budget is spent.
	push := func(r Result) bool {
		key := r.Number
		if key == "" {
			key = r.Title + "\x00" + r.Body // unnumbered corpora fallback
		}
		if seen[key] {
			return true
		}
		if len(out) >= maxGroundingDocs {
			return false
		}
		seen[key] = true
		out = append(out, r)
		return true
	}
	for _, r := range seeds {
		if !push(r) {
			return out, nil
		}
	}

	// Rank groups by how many seeds landed in them: the mechanic a question is
	// really about is the one the search kept hitting. MTG rules group by
	// chapter token; D&D records by their path-style group key.
	counts := map[string]int{}
	var order []string
	for _, r := range seeds {
		g := ruleGroup(r.Number)
		if g == "" {
			g = data.DNDGroupKey(r.Number)
		}
		if g == "" {
			continue
		}
		if counts[g] == 0 {
			order = append(order, g)
		}
		counts[g]++
	}
	sort.SliceStable(order, func(i, j int) bool { return counts[order[i]] > counts[order[j]] })

	expanded := map[string]bool{}
	for _, g := range order {
		if len(expanded) >= maxExpandGroups {
			break
		}
		docs, err := s.groupDocs(ctx, corpus, g)
		if err != nil {
			return nil, err
		}
		if len(docs) == 0 || len(docs) > maxGroupDocs {
			continue
		}
		expanded[g] = true
		for _, d := range docs {
			if !push(d) {
				return out, nil
			}
		}
	}

	// Seeds in groups too large to pull whole still get their own section.
	for _, r := range seeds {
		if expanded[ruleGroup(r.Number)] || expanded[data.DNDGroupKey(r.Number)] {
			continue
		}
		var sec []Result
		var err error
		if data.DNDSectionKey(r.Number) != "" {
			sec, err = s.dndSectionDocs(ctx, corpus, data.DNDSectionKey(r.Number))
		} else {
			sec, err = s.Section(ctx, corpus, r.Number)
		}
		if err != nil {
			return nil, err
		}
		for _, d := range sec {
			if !push(d) {
				return out, nil
			}
		}
	}

	// Last tier: a seed's own subsection tree. A hit routinely lands on a
	// parent section whose substance lives underneath it — the SRD's weapons
	// table is a child of "Equipment — Weapons", not a sibling of it — and the
	// two tiers above cannot reach it. Sibling chunks stop at the heading, and
	// the enclosing chapter (144 records for equipment) is far past the budget,
	// so the section a question actually needs sits one hop away, structurally
	// unreachable. Numbered MTG rules never had this gap: Section expands 205
	// into 205.1a by number prefix, and D&D path numbers nest the same way.
	//
	// It runs last so the focused tiers claim the budget first, and it is
	// bounded twice — a tree larger than maxChildDocs is refused outright, and
	// only the first maxExpandChildren seeds pull one at all.
	trees := 0
	for _, r := range seeds {
		if trees >= maxExpandChildren {
			break
		}
		if expanded[ruleGroup(r.Number)] || expanded[data.DNDGroupKey(r.Number)] {
			continue // the whole group is already in; its children came with it
		}
		key := data.DNDSectionKey(r.Number)
		if key == "" {
			continue // MTG rules get subrule expansion from Section
		}
		kids, err := s.dndSubtreeDocs(ctx, corpus, key)
		if err != nil {
			return nil, err
		}
		if len(kids) == 0 || len(kids) > maxChildDocs {
			continue
		}
		trees++
		for _, d := range kids {
			if !push(d) {
				return out, nil
			}
		}
	}
	return out, nil
}

// ruleGroup returns the chapter token of a numbered rule ("613.6c" -> "613").
// A number that isn't a numbered rule yields "".
func ruleGroup(number string) string {
	m := ruleNumberFullRe.FindStringSubmatch(strings.TrimSpace(number))
	if m == nil {
		return ""
	}
	return strings.SplitN(m[1], ".", 2)[0]
}

// groupDocs returns every rule in a group, in rule order. A group is either an
// MTG chapter token (bare digits, matched by number-prefix) or a D&D path
// group key ("spells/0003", matched by leading number phrase and filtered to
// the exact group in Go).
func (s *Store) groupDocs(ctx context.Context, corpus data.Corpus, group string) ([]Result, error) {
	if group == "" {
		return nil, nil
	}
	var match string
	dnd := strings.Contains(group, "/")
	if isBareChapter(group) {
		match = fmt.Sprintf("number:%s*", group)
	} else if dnd {
		match = fmt.Sprintf("number:%q*", group)
	} else {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, ftsSectionSQL, match, corpus)
	if err != nil {
		return nil, fmt.Errorf("group: %w", err)
	}
	defer rows.Close()
	var out []Result
	for rows.Next() {
		var r Result
		if err := rows.Scan(&r.Number, &r.Title, &r.Body, &r.Source); err != nil {
			return nil, err
		}
		// The MATCH is a prefix/phrase match on the number column; keep only
		// exact group members.
		if dnd {
			if data.DNDGroupKey(r.Number) != group {
				continue
			}
		} else if ruleGroup(r.Number) != group {
			continue
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if dnd {
		sort.SliceStable(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	} else {
		sort.SliceStable(out, func(i, j int) bool { return lessRuleNumber(out[i].Number, out[j].Number) })
	}
	return out, nil
}

// dndSectionDocs returns every chunk of one D&D heading section, in chunk
// order — the path-numbered analogue of Section. A key without "/" yields no
// docs.
func (s *Store) dndSectionDocs(ctx context.Context, corpus data.Corpus, key string) ([]Result, error) {
	if !strings.Contains(key, "/") {
		return nil, nil
	}
	// "spells/0003/0042" tokenizes to consecutive words in the number column;
	// match the leading phrase, then filter to the exact section in Go.
	rows, err := s.db.QueryContext(ctx, ftsSectionSQL, fmt.Sprintf("number:%q*", key), corpus)
	if err != nil {
		return nil, fmt.Errorf("dnd section: %w", err)
	}
	defer rows.Close()
	var out []Result
	for rows.Next() {
		var r Result
		if err := rows.Scan(&r.Number, &r.Title, &r.Body, &r.Source); err != nil {
			return nil, err
		}
		if data.DNDSectionKey(r.Number) != key {
			continue
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out, nil
}

// Chapter returns every numbered rule in a chapter — the leading number of a
// dotted rule, e.g. "613" returns 613.1 through the last 613.x sub-rule — in
// rule order. A chapter token that is not bare digits returns nil with no
// error. The interaction resolver uses it to ground a whole mechanic
// (priority/stack, triggers, layers, replacement effects) at once.
func (s *Store) Chapter(ctx context.Context, corpus data.Corpus, chapter string) ([]Result, error) {
	if !isBareChapter(chapter) {
		return nil, nil
	}
	return s.groupDocs(ctx, corpus, chapter)
}

// isBareChapter reports whether a token is digits only, the form a chapter
// prefix must take before it is safe to splice into a number: prefix match.
func isBareChapter(chapter string) bool {
	if chapter == "" {
		return false
	}
	for _, r := range chapter {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// DNDChildren returns every D&D record whose path number starts with the
// given file slug ("spells/" → all records from spells.md), in number order.
// Used by study mode to build decks over a document's entries. A slug without
// a "/" yields no docs.
func (s *Store) DNDChildren(ctx context.Context, corpus data.Corpus, slug string) ([]Result, error) {
	if strings.Contains(strings.Trim(slug, "/"), "/") {
		return nil, nil
	}
	return s.dndSubtreeDocs(ctx, corpus, slug)
}

// dndSubtreeDocs returns every D&D record nested under a path prefix — the
// records whose number begins with prefix + "/" — in number order. A file slug
// yields the whole document; a section key yields that heading's subsections.
func (s *Store) dndSubtreeDocs(ctx context.Context, corpus data.Corpus, prefix string) ([]Result, error) {
	prefix = strings.Trim(prefix, "/")
	if prefix == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, ftsSectionSQL, fmt.Sprintf("number:%q*", prefix), corpus)
	if err != nil {
		return nil, fmt.Errorf("dnd subtree: %w", err)
	}
	defer rows.Close()
	var out []Result
	for rows.Next() {
		var r Result
		if err := rows.Scan(&r.Number, &r.Title, &r.Body, &r.Source); err != nil {
			return nil, err
		}
		// The MATCH is a prefix match on the tokenized number column; keep only
		// records actually nested under the prefix.
		if !strings.HasPrefix(r.Number, prefix+"/") {
			continue
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out, nil
}

// Retrieve runs a lenient OR search used to gather grounding context for the
// Q&A model. It never returns an error for an unsalvageable query — it simply
// returns no docs, letting the model answer without context.
//
// When an embedder is configured, a top-N vector scan runs alongside FTS5 and
// its hits are merged in. FTS5 stays the backbone (its results lead, so an
// exact rule-number lookup still works); embeddings only add semantic recall
// for questions that share no keywords with the rule they need. An embeddings
// failure is best-effort: the FTS5 results are returned and the miss is logged
// so the optional layer never takes retrieval down with it.
func (s *Store) Retrieve(ctx context.Context, corpus data.Corpus, query string, limit int) ([]Result, error) {
	q := strings.TrimSpace(query)
	if limit <= 0 {
		limit = 6
	}
	fts, err := s.ftsRetrieve(ctx, corpus, q, limit)
	if err != nil {
		return nil, fmt.Errorf("retrieve: %w", err)
	}
	if s.embedder == nil {
		return fts, nil
	}
	vec, err := s.vectorRetrieve(ctx, corpus, query, limit)
	if err != nil {
		log.Printf("embeddings retrieve skipped: %v", err)
		return fts, nil
	}
	return mergeResults(fts, vec), nil
}

// ftsRetrieve is the FTS5-only retrieval path (the pre-embeddings behavior).
func (s *Store) ftsRetrieve(ctx context.Context, corpus data.Corpus, q string, limit int) ([]Result, error) {
	matches, err := toFTSQueryOR(q)
	if err != nil {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, ftsSearchSQL, matches, corpus, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Result
	for rows.Next() {
		var r Result
		if err := rows.Scan(&r.Number, &r.Title, &r.Body, &r.Source, &r.Score); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
