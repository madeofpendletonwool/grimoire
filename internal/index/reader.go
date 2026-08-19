// Reader storage: the book-shaped view of the corpora, kept beside the FTS5
// index in plain SQLite tables. Search bodies are flattened for matching;
// reader bodies are the reading-fidelity text (raw markdown, whole rule
// sections), so the two stores never constrain each other.

package index

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/data"
)

const readerSchema = `
CREATE TABLE IF NOT EXISTS reader_nodes (
	id INTEGER PRIMARY KEY,
	corpus       TEXT NOT NULL,
	guide        TEXT NOT NULL,
	guide_title  TEXT NOT NULL,
	guide_kind   TEXT NOT NULL DEFAULT '',
	number       TEXT NOT NULL,
	title        TEXT NOT NULL,
	level        INTEGER NOT NULL,
	position     INTEGER NOT NULL,
	body         TEXT NOT NULL DEFAULT '',
	source       TEXT NOT NULL DEFAULT '',
	UNIQUE(corpus, guide, number)
);
CREATE INDEX IF NOT EXISTS reader_nodes_toc ON reader_nodes(corpus, guide, position);
CREATE TABLE IF NOT EXISTS reader_guides (
	corpus      TEXT NOT NULL,
	guide       TEXT NOT NULL,
	title       TEXT NOT NULL,
	kind        TEXT NOT NULL DEFAULT '',
	position    INTEGER NOT NULL DEFAULT 0,
	node_count  INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY(corpus, guide)
);
`

// ReaderGuide describes one readable book in a corpus.
type ReaderGuide struct {
	Corpus string
	Guide  string
	Title  string
	Kind   string // "rules" | "srd" | "book"
	Order  int    // display order within the corpus
	Nodes  int    // how many stops the guide has
}

// ReaderTOC is one stop in a guide's table of contents.
type ReaderTOC struct {
	Number   string
	Title    string
	Level    int
	Position int
	HasBody  bool
}

// ReaderPage is the full reading view of one stop: the node itself, its
// ancestor chain (crumb titles), and the neighbouring stops for prev/next.
type ReaderPage struct {
	Corpus     string
	Guide      string
	GuideTitle string
	GuideKind  string
	Number     string
	Title      string
	Level      int
	Body       string
	Source     string
	Crumbs     []ReaderTOC
	Prev       *ReaderTOC
	Next       *ReaderTOC
}

// indexReader loads the dataset's reader tree into its tables, replacing
// prior contents. Called from Index inside its transaction.
func (s *Store) indexReader(ctx context.Context, tx *sql.Tx, nodes []data.ReaderNode) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM reader_nodes; DELETE FROM reader_guides;`); err != nil {
		return fmt.Errorf("clear reader: %w", err)
	}

	nodeStmt, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO reader_nodes(corpus, guide, guide_title, guide_kind, number, title, level, position, body, source) VALUES(?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return fmt.Errorf("prepare reader node: %w", err)
	}
	defer nodeStmt.Close()

	guides := map[string]*ReaderGuide{}
	for _, n := range nodes {
		if _, err := nodeStmt.ExecContext(ctx, string(n.Corpus), n.Guide, n.GuideTitle, n.GuideKind, n.Number, n.Title, n.Level, n.Position, n.Body, n.Source); err != nil {
			return fmt.Errorf("insert reader node: %w", err)
		}
		key := string(n.Corpus) + "\x00" + n.Guide
		g, ok := guides[key]
		if !ok {
			g = &ReaderGuide{Corpus: string(n.Corpus), Guide: n.Guide, Title: n.GuideTitle, Kind: n.GuideKind, Order: len(guides)}
			guides[key] = g
		}
		g.Nodes++
	}

	guideStmt, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO reader_guides(corpus, guide, title, kind, position, node_count) VALUES(?,?,?,?,?,?)`)
	if err != nil {
		return fmt.Errorf("prepare reader guide: %w", err)
	}
	defer guideStmt.Close()
	for _, g := range guides {
		if _, err := guideStmt.ExecContext(ctx, g.Corpus, g.Guide, g.Title, g.Kind, g.Order, g.Nodes); err != nil {
			return fmt.Errorf("insert reader guide: %w", err)
		}
	}
	return nil
}

// ReaderGuides lists the readable books for a corpus, in indexed order:
// the corpus's own rules first, SRD documents next, local books last.
func (s *Store) ReaderGuides(ctx context.Context, corpus data.Corpus) ([]ReaderGuide, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT corpus, guide, title, kind, position, node_count FROM reader_guides WHERE corpus = ? ORDER BY position`, string(corpus))
	if err != nil {
		return nil, fmt.Errorf("reader guides: %w", err)
	}
	defer rows.Close()
	var out []ReaderGuide
	for rows.Next() {
		var g ReaderGuide
		if err := rows.Scan(&g.Corpus, &g.Guide, &g.Title, &g.Kind, &g.Order, &g.Nodes); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// ReaderTOC returns a guide's table of contents in book order. Leaf bodies are
// omitted; HasBody tells the client which stops carry reading text.
func (s *Store) ReaderTOC(ctx context.Context, corpus data.Corpus, guide string) ([]ReaderTOC, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT number, title, level, position, body <> '' FROM reader_nodes WHERE corpus = ? AND guide = ? ORDER BY position`, string(corpus), guide)
	if err != nil {
		return nil, fmt.Errorf("reader toc: %w", err)
	}
	defer rows.Close()
	var out []ReaderTOC
	for rows.Next() {
		var t ReaderTOC
		if err := rows.Scan(&t.Number, &t.Title, &t.Level, &t.Position, &t.HasBody); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ReaderPage returns the full reading view of one stop: the node, its crumb
// chain, and its neighbours. The number must name a node exactly; callers
// resolve looser references (rule numbers, record numbers) beforehand.
func (s *Store) ReaderPage(ctx context.Context, corpus data.Corpus, guide, number string) (*ReaderPage, error) {
	row := s.db.QueryRowContext(ctx, `SELECT corpus, guide, guide_title, guide_kind, number, title, level, position, body, source
		FROM reader_nodes WHERE corpus = ? AND guide = ? AND number = ?`, string(corpus), guide, number)
	var p ReaderPage
	var pos int
	err := row.Scan(&p.Corpus, &p.Guide, &p.GuideTitle, &p.GuideKind, &p.Number, &p.Title, &p.Level, &pos, &p.Body, &p.Source)
	if err != nil {
		return nil, err
	}

	// Ancestors: the running stack of less-indented stops before this one.
	toc, err := s.ReaderTOC(ctx, corpus, guide)
	if err != nil {
		return nil, err
	}
	var stack []ReaderTOC
	for _, t := range toc {
		if t.Position == pos {
			break
		}
		if t.Level < p.Level {
			// Place the stop at its depth, growing through any skipped level.
			for t.Level-1 > len(stack) {
				stack = append(stack, ReaderTOC{})
			}
			if t.Level-1 == len(stack) {
				stack = append(stack, t)
			} else {
				stack[t.Level-1] = t
				stack = stack[:t.Level]
			}
		}
	}
	p.Crumbs = stack

	// Neighbours: the stops either side in book order, skipping pure
	// containers so "next" always lands on something readable.
	var prevLeaf, nextLeaf *ReaderTOC
	for i := range toc {
		if toc[i].Position == pos {
			for j := i - 1; j >= 0; j-- {
				if toc[j].HasBody {
					t := toc[j]
					prevLeaf = &t
					break
				}
			}
			for j := i + 1; j < len(toc); j++ {
				if toc[j].HasBody {
					t := toc[j]
					nextLeaf = &t
					break
				}
			}
			break
		}
	}
	p.Prev = prevLeaf
	p.Next = nextLeaf
	return &p, nil
}

// ReaderResolve maps a rules reference onto the reader page that carries it:
// an MTG rule number ("205.1a") resolves to its section's node ("205"), and a
// D&D record number ("spells/0003/0042.1") resolves to its heading node. A
// miss returns "" — the reference may simply predate the reader tree.
func (s *Store) ReaderResolve(ctx context.Context, corpus data.Corpus, number string) (guide, node string, err error) {
	num := strings.TrimSpace(number)
	if num == "" {
		return "", "", nil
	}
	if corpus == data.CorpusMTG {
		m := ruleNumberFullRe.FindStringSubmatch(num)
		if m == nil {
			return "", "", nil
		}
		// Reader sections are the 3-digit headers, so any rule inside one —
		// "205.1a", "702.2" — resolves to its section node ("205", "702").
		n := strings.SplitN(m[1], ".", 2)[0]
		err := s.db.QueryRowContext(ctx,
			`SELECT guide, number FROM reader_nodes WHERE corpus = ? AND number = ? LIMIT 1`, string(corpus), n).
			Scan(&guide, &node)
		if err != nil {
			return "", "", nil // not found is not an error here
		}
		return guide, node, nil
	}
	// D&D: strip the trailing ".chunk" from a record number and try the exact
	// section key, then its parent path (an entry cited inside a subsection).
	key := num
	if i := strings.LastIndex(key, "."); i >= 0 {
		key = key[:i]
	}
	for key != "" {
		err := s.db.QueryRowContext(ctx,
			`SELECT guide, number FROM reader_nodes WHERE corpus = ? AND number = ? LIMIT 1`, string(corpus), key).
			Scan(&guide, &node)
		if err == nil {
			return guide, node, nil
		}
		if i := strings.LastIndex(key, "/"); i > 0 {
			key = key[:i]
		} else {
			break
		}
	}
	return "", "", nil
}

// ReaderIndexed reports whether the reader tables hold any nodes — the flag
// main uses to schedule a background rebuild for installs whose FTS index
// predates the reader.
func (s *Store) ReaderIndexed(ctx context.Context) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM reader_nodes`).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}
