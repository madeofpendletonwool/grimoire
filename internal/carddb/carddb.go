// Package carddb is the local Magic card database: a per-card table built at
// index time from MTGJSON's AtomicCards dataset, plus an FTS5 index over name,
// type line, and oracle text for synergy search. It is the grounded source the
// deck builder validates every suggestion against — a card the model proposes
// must exist here before the reader ever sees it.
package carddb

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// Card is one unique card name with the fields the deck builder needs. Fields
// come from MTGJSON's Card (Atomic) model; anything the dataset omits (e.g.
// edhrecRank for unpopular printings) stays zero/NULL rather than guessed.
type Card struct {
	Name            string  `json:"name"`
	ManaCost        string  `json:"mana_cost,omitempty"`
	ManaValue       float64 `json:"mana_value"`
	TypeLine        string  `json:"type_line,omitempty"`
	OracleText      string  `json:"oracle_text,omitempty"`
	ColorIdentity   string  `json:"color_identity,omitempty"` // sorted, e.g. "BR"
	EDHRECRank      int     `json:"edhrec_rank,omitempty"`    // 0 = unranked
	EDHRECSaltiness float64 `json:"edhrec_saltiness,omitempty"`
	CommanderLegal  bool    `json:"commander_legal"`
	LegalCommander  string  `json:"legal_commander,omitempty"` // legality in the Commander format
	GameChanger     bool    `json:"game_changer"`
}

// IsLand reports whether the card's type line marks it as a land.
func (c *Card) IsLand() bool {
	return strings.Contains(c.TypeLine, "Land") || strings.Contains(c.TypeLine, "land")
}

// IsBasicLand reports whether the card is a basic land (the only cards a
// Commander deck may run any number of).
func (c *Card) IsBasicLand() bool {
	return strings.Contains(c.TypeLine, "Basic Land")
}

// Store persists cards in the shared SQLite database, following the per-package
// schema pattern: the cards and cards_fts tables are siblings of the docs and
// chat tables, so a rules reindex never touches them. Populated during an
// index build (Populate); queried by the deck builder at request time.
type Store struct {
	db *sql.DB
}

// New builds a card store on an open database handle and ensures its schema
// exists. The tables are empty until Populate runs (index build).
func New(db *sql.DB) (*Store, error) {
	s := &Store{db: db}
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("carddb migrate: %w", err)
	}
	return s, nil
}

// schema declares the live tables plus their staging twins. The staging twins
// let Populate rebuild without a window where no cards exist — the same
// stage-and-swap discipline the rules index uses.
const schema = `
CREATE TABLE IF NOT EXISTS cards (
	name             TEXT PRIMARY KEY,
	mana_cost        TEXT NOT NULL DEFAULT '',
	mana_value       REAL NOT NULL DEFAULT 0,
	type_line        TEXT NOT NULL DEFAULT '',
	oracle_text      TEXT NOT NULL DEFAULT '',
	color_identity   TEXT NOT NULL DEFAULT '',
	color_mask       INTEGER NOT NULL DEFAULT 0,
	edhrec_rank      INTEGER,
	edhrec_saltiness REAL NOT NULL DEFAULT 0,
	commander_legal  INTEGER NOT NULL DEFAULT 0,
	legal_commander  TEXT NOT NULL DEFAULT '',
	game_changer     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS cards_rank ON cards(edhrec_rank);
CREATE VIRTUAL TABLE IF NOT EXISTS cards_fts USING fts5(
	name,
	type_line,
	oracle_text,
	tokenize = 'porter unicode61 remove_diacritics 2'
);
CREATE TABLE IF NOT EXISTS cards_build (
	name             TEXT PRIMARY KEY,
	mana_cost        TEXT NOT NULL DEFAULT '',
	mana_value       REAL NOT NULL DEFAULT 0,
	type_line        TEXT NOT NULL DEFAULT '',
	oracle_text      TEXT NOT NULL DEFAULT '',
	color_identity   TEXT NOT NULL DEFAULT '',
	color_mask       INTEGER NOT NULL DEFAULT 0,
	edhrec_rank      INTEGER,
	edhrec_saltiness REAL NOT NULL DEFAULT 0,
	commander_legal  INTEGER NOT NULL DEFAULT 0,
	legal_commander  TEXT NOT NULL DEFAULT '',
	game_changer     INTEGER NOT NULL DEFAULT 0
);
CREATE VIRTUAL TABLE IF NOT EXISTS cards_fts_build USING fts5(
	name,
	type_line,
	oracle_text,
	tokenize = 'porter unicode61 remove_diacritics 2'
);
`

// ColorMask packs a color identity into a bitmask: W=1, U=2, B=4, R=8, G=16.
// A card is legal in a deck when every bit of its mask is present in the
// deck's allowed mask — expressible in SQL as `color_mask & ~allowed = 0`.
func ColorMask(identity string) int {
	mask := 0
	for _, c := range identity {
		switch c {
		case 'W':
			mask |= 1
		case 'U':
			mask |= 2
		case 'B':
			mask |= 4
		case 'R':
			mask |= 8
		case 'G':
			mask |= 16
		}
	}
	return mask
}

// ColorBits maps the single-letter color codes to their mask bits.
var colorBits = map[byte]int{'W': 1, 'U': 2, 'B': 4, 'R': 8, 'G': 16}

// MaskForColors builds an allowed-identity mask from color letters, e.g.
// "WUB" or "wub". An empty string means colorless-only; the caller composes
// the union with the commander's identity separately.
func MaskForColors(colors string) int {
	mask := 0
	for i := 0; i < len(colors); i++ {
		mask |= colorBits[norm(colors[i])]
	}
	return mask
}

func norm(c byte) byte {
	if c >= 'a' && c <= 'z' {
		return c - 'a' + 'A'
	}
	return c
}

// NormalizeIdentity upper-cases and sorts a color identity string ("rb" -> "BR").
func NormalizeIdentity(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	letters := []byte(s)
	sort.Slice(letters, func(i, j int) bool { return letters[i] < letters[j] })
	return string(letters)
}

// Count returns the number of cards in the table.
func (s *Store) Count() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM cards`).Scan(&n)
	return n, err
}

// Get looks up one card by name, case-insensitively. Unknown names return
// ErrNotFound — the caller flags rather than fabricates.
func (s *Store) Get(name string) (*Card, error) {
	row := s.db.QueryRow(`
		SELECT name, mana_cost, mana_value, type_line, oracle_text,
		       color_identity, edhrec_rank, edhrec_saltiness,
		       commander_legal, legal_commander, game_changer
		  FROM cards WHERE name = ? COLLATE NOCASE`, strings.TrimSpace(name))
	return scanCard(row)
}

// ErrNotFound reports an unknown card name.
var ErrNotFound = fmt.Errorf("card not found")

func scanCard(row *sql.Row) (*Card, error) {
	var c Card
	var rank sql.NullInt64
	if err := row.Scan(&c.Name, &c.ManaCost, &c.ManaValue, &c.TypeLine, &c.OracleText,
		&c.ColorIdentity, &rank, &c.EDHRECSaltiness,
		&c.CommanderLegal, &c.LegalCommander, &c.GameChanger); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if rank.Valid {
		c.EDHRECRank = int(rank.Int64)
	}
	return &c, nil
}

const cardColumns = `name, mana_cost, mana_value, type_line, oracle_text,
	color_identity, edhrec_rank, edhrec_saltiness, commander_legal, legal_commander, game_changer`

func scanCards(rows *sql.Rows) ([]*Card, error) {
	defer rows.Close()
	var out []*Card
	for rows.Next() {
		var c Card
		var rank sql.NullInt64
		if err := rows.Scan(&c.Name, &c.ManaCost, &c.ManaValue, &c.TypeLine, &c.OracleText,
			&c.ColorIdentity, &rank, &c.EDHRECSaltiness,
			&c.CommanderLegal, &c.LegalCommander, &c.GameChanger); err != nil {
			return nil, err
		}
		if rank.Valid {
			c.EDHRECRank = int(rank.Int64)
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

// SearchNames returns cards whose indexed text matches the query, best match
// first. The query is sanitized into FTS5 prefix tokens joined by OR so
// "seeker skybreak" finds Seeker of Skybreak; an empty result falls back to a
// substring scan so a lone unusual word still hits.
func (s *Store) SearchNames(ctx context.Context, q string, limit int) ([]*Card, error) {
	if limit <= 0 {
		limit = 20
	}
	if match := ftsQuery(q); match != "" {
		rows, err := s.db.QueryContext(ctx, `
			SELECT c.name, c.mana_cost, c.mana_value, c.type_line, c.oracle_text,
			       c.color_identity, c.edhrec_rank, c.edhrec_saltiness,
			       c.commander_legal, c.legal_commander, c.game_changer
			  FROM cards_fts f JOIN cards c ON c.name = f.name
			 WHERE cards_fts MATCH ?
			 ORDER BY bm25(cards_fts, 0, 3.0, 1.0) LIMIT ?`, match, limit)
		if err == nil {
			out, err := scanCards(rows)
			if err != nil {
				return nil, err
			}
			if len(out) > 0 {
				return out, nil
			}
		}
	}
	// Fallback: substring on the name, for tokens the tokenizer splits oddly.
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+cardColumns+` FROM cards
		 WHERE name LIKE ? COLLATE NOCASE
		 ORDER BY edhrec_rank IS NULL, edhrec_rank LIMIT ?`,
		"%"+sanitizeToken(q)+"%", limit)
	if err != nil {
		return nil, err
	}
	return scanCards(rows)
}

// Commanders returns commander-eligible cards within an allowed color mask
// (0 = any identity), ranked by EDHREC popularity. When terms are given the
// candidates are first narrowed by an FTS match over name/type/oracle text so
// a themed idea ("angels and dragons") surfaces on-theme commanders even when
// their global rank is modest.
func (s *Store) Commanders(ctx context.Context, allowedMask int, terms []string, limit int) ([]*Card, error) {
	if limit <= 0 {
		limit = 12
	}
	if match := ftsQuery(strings.Join(terms, " ")); match != "" {
		rows, err := s.db.QueryContext(ctx, `
			SELECT c.name, c.mana_cost, c.mana_value, c.type_line, c.oracle_text,
			       c.color_identity, c.edhrec_rank, c.edhrec_saltiness,
			       c.commander_legal, c.legal_commander, c.game_changer
			  FROM cards_fts f JOIN cards c ON c.name = f.name
			 WHERE cards_fts MATCH ?
			 ORDER BY bm25(cards_fts, 0, 3.0, 1.0) LIMIT 400`, match)
		if err == nil {
			out, err := scanCards(rows)
			if err != nil {
				return nil, err
			}
			return filterCommanders(out, allowedMask, limit), nil
		}
	}
	cond, args := identityCond(allowedMask)
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+cardColumns+` FROM cards
		 WHERE commander_legal = 1`+cond+`
		 ORDER BY edhrec_rank IS NULL, edhrec_rank LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	return scanCards(rows)
}

// Candidates returns non-land cards within an allowed color mask, ranked by
// EDHREC popularity, optionally narrowed by FTS terms. exclude drops names
// already drafted (a map for O(1) checks).
func (s *Store) Candidates(ctx context.Context, allowedMask int, terms []string, exclude map[string]bool, limit int) ([]*Card, error) {
	if limit <= 0 {
		limit = 200
	}
	if match := ftsQuery(strings.Join(terms, " ")); match != "" {
		rows, err := s.db.QueryContext(ctx, `
			SELECT c.name, c.mana_cost, c.mana_value, c.type_line, c.oracle_text,
			       c.color_identity, c.edhrec_rank, c.edhrec_saltiness,
			       c.commander_legal, c.legal_commander, c.game_changer
			  FROM cards_fts f JOIN cards c ON c.name = f.name
			 WHERE cards_fts MATCH ?
			 ORDER BY bm25(cards_fts, 0, 3.0, 1.0) LIMIT 800`, match)
		if err == nil {
			out, err := scanCards(rows)
			if err != nil {
				return nil, err
			}
			return filterIdentity(out, allowedMask, exclude, limit, false), nil
		}
	}
	cond, args := identityCond(allowedMask)
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+cardColumns+` FROM cards
		 WHERE type_line NOT LIKE '%Land%'`+cond+`
		 ORDER BY edhrec_rank IS NULL, edhrec_rank LIMIT ?`, append(args, limit+len(exclude))...)
	if err != nil {
		return nil, err
	}
	out, err := scanCards(rows)
	if err != nil {
		return nil, err
	}
	return filterIdentity(out, allowedMask, exclude, limit, false), nil
}

// identityCond builds the SQL color filter. A card is legal when all of its
// color bits appear in the allowed mask: color_mask & ~allowed = 0.
func identityCond(allowedMask int) (string, []any) {
	if allowedMask == 0 {
		return "", nil
	}
	return ` AND (color_mask & ~?) = 0`, []any{allowedMask}
}

// filterCommanders narrows FTS hits to commander-eligible cards inside the
// allowed identity, keeping EDHREC-rank order, then trims to limit.
func filterCommanders(cards []*Card, allowedMask, limit int) []*Card {
	return filterIdentity(cards, allowedMask, nil, limit, true)
}

func filterIdentity(cards []*Card, allowedMask int, exclude map[string]bool, limit int, commandersOnly bool) []*Card {
	out := make([]*Card, 0, limit)
	for _, c := range cards {
		if commandersOnly && !c.CommanderLegal {
			continue
		}
		if !commandersOnly && c.IsLand() {
			continue
		}
		if allowedMask != 0 && c.colorMask()&^allowedMask != 0 {
			continue
		}
		if exclude != nil && exclude[strings.ToLower(c.Name)] {
			continue
		}
		out = append(out, c)
		if len(out) >= limit {
			break
		}
	}
	// Rank order within the filtered set.
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := out[i].EDHRECRank, out[j].EDHRECRank
		if ri == 0 {
			return false
		}
		if rj == 0 {
			return true
		}
		return ri < rj
	})
	return out
}

// colorMask recomputes the identity mask from the stored identity string.
func (c *Card) colorMask() int { return ColorMask(c.ColorIdentity) }

// IdentityAllowed reports whether the card's color identity fits within the
// given allowed mask (the commander's identity, possibly widened).
func (c *Card) IdentityAllowed(allowedMask int) bool {
	return c.colorMask()&^allowedMask == 0
}

// ftsQuery turns free text into an FTS5 OR-of-prefix query. Tokens are
// lower-cased alphanumerics only; anything else is dropped, so quotes and
// operators from user input cannot escape the query grammar.
func ftsQuery(s string) string {
	var parts []string
	for _, tok := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !('a' <= r && r <= 'z' || '0' <= r && r <= '9')
	}) {
		if len(tok) < 2 || stopword(tok) {
			continue
		}
		parts = append(parts, `"`+tok+`"*`)
	}
	return strings.Join(parts, " OR ")
}

// sanitizeToken reduces free text to a bare lowercase alnum token for the
// LIKE fallback.
func sanitizeToken(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if 'a' <= r && r <= 'z' || '0' <= r && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// stopword drops the most common English filler so an idea's theme words do
// the matching, not "with" and "and".
var stopwords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true, "at": true,
	"be": true, "but": true, "by": true, "for": true, "from": true, "has": true,
	"have": true, "i": true, "in": true, "is": true, "it": true, "its": true,
	"of": true, "on": true, "or": true, "that": true, "the": true, "this": true,
	"to": true, "was": true, "we": true, "with": true, "you": true, "your": true,
	"my": true, "me": true, "want": true, "like": true, "deck": true,
	"decks": true, "commander": true, "edh": true, "build": true, "make": true,
	"some": true, "lots": true, "lot": true, "very": true, "really": true,
}

func stopword(tok string) bool { return stopwords[tok] }
