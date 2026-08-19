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

// PlayableInCommander reports whether the card is legal in the format as
// printed. MTGJSON leaves the legality blank for cards that were never in it —
// the Alchemy rebalances ("A-Rulik Mons"), digital-only and Un-set cards — and
// suggesting one of those is the same failure as suggesting a card that does
// not exist: the player cannot sleeve it.
func (c *Card) PlayableInCommander() bool { return c.LegalCommander == "Legal" }

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
//
// The name column carries the weight here, unlike the synergy searches below:
// this is the lookup a written card name goes through, so a hit in the name
// must outrank a card that merely mentions those words in its rules text.
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
			 ORDER BY bm25(cards_fts, 10.0, 1.0, 0.5) LIMIT ?`, match, limit)
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
// (0 = any identity), best fit for the idea's terms first.
//
// Ranking is deliberately not raw FTS relevance. An idea names a tribe and a
// pair of colours ("green and red goblins"), and what the player means is a
// commander that *is* those colours and cares about that tribe — so a term hit
// in the type line or the name counts for far more than the same word buried
// in rules text, and a commander whose identity is exactly the colours asked
// for outranks one that merely fits inside them. EDHREC popularity breaks ties
// rather than deciding the order, because the on-theme legend is often the
// obscure one.
func (s *Store) Commanders(ctx context.Context, allowedMask int, terms []string, limit int) ([]*Card, error) {
	if limit <= 0 {
		limit = 12
	}
	pool := map[string]*Card{}
	collect := func(cards []*Card) {
		for _, c := range cards {
			if !c.CommanderLegal || !c.PlayableInCommander() {
				continue
			}
			if allowedMask != 0 && c.colorMask()&^allowedMask != 0 {
				continue
			}
			pool[strings.ToLower(c.Name)] = c
		}
	}

	if match := ftsQuery(strings.Join(terms, " ")); match != "" {
		rows, err := s.db.QueryContext(ctx, `
			SELECT c.name, c.mana_cost, c.mana_value, c.type_line, c.oracle_text,
			       c.color_identity, c.edhrec_rank, c.edhrec_saltiness,
			       c.commander_legal, c.legal_commander, c.game_changer
			  FROM cards_fts f JOIN cards c ON c.name = f.name
			 WHERE cards_fts MATCH ?
			 ORDER BY bm25(cards_fts, 4.0, 4.0, 1.0) LIMIT 600`, match)
		if err == nil {
			out, err := scanCards(rows)
			if err != nil {
				return nil, err
			}
			collect(out)
		}
	}
	// Always mix in the popular legends of the identity, so a narrow or
	// misspelled idea still comes back with something playable.
	cond, args := identityCond(allowedMask)
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+cardColumns+` FROM cards
		 WHERE commander_legal = 1 AND legal_commander = 'Legal'`+cond+`
		 ORDER BY edhrec_rank IS NULL, edhrec_rank LIMIT ?`, append(args, 200)...)
	if err != nil {
		return nil, err
	}
	popular, err := scanCards(rows)
	if err != nil {
		return nil, err
	}
	collect(popular)

	ranked := make([]*Card, 0, len(pool))
	for _, c := range pool {
		ranked = append(ranked, c)
	}
	scores := make(map[string]float64, len(ranked))
	for _, c := range ranked {
		scores[c.Name] = commanderScore(c, terms, allowedMask)
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if scores[ranked[i].Name] != scores[ranked[j].Name] {
			return scores[ranked[i].Name] > scores[ranked[j].Name]
		}
		return lessByRank(ranked[i], ranked[j])
	})
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}
	return ranked, nil
}

// commanderScore rates one legend against the idea's terms and the requested
// colours. Weights are ordered by how strongly each signal predicts "this is
// the commander they meant": its exact colours, then its creature types, then
// its name, and only then its rules text.
func commanderScore(c *Card, terms []string, allowedMask int) float64 {
	score := 0.0
	if allowedMask != 0 && c.colorMask() == allowedMask {
		score += 3
	}
	typeLine := strings.ToLower(c.TypeLine)
	name := strings.ToLower(c.Name)
	oracle := strings.ToLower(c.OracleText)
	for _, t := range terms {
		switch {
		case termInWords(typeLine, t):
			score += 2.5
		case termInWords(name, t):
			score += 1.5
		case strings.Contains(oracle, t):
			score += 0.5
		}
	}
	// Popularity is a tiebreak, capped so it cannot outweigh a theme match.
	if c.EDHRECRank > 0 {
		score += min(1.0, 1000/float64(c.EDHRECRank+200))
	}
	return score
}

// termInWords reports whether a term from the player's idea appears as a whole
// word in the text, in either number — ideas are written "goblins" and type
// lines read "Goblin Warrior".
func termInWords(haystack, term string) bool {
	if containsWord(haystack, term) {
		return true
	}
	if s, ok := strings.CutSuffix(term, "s"); ok && len(s) > 2 {
		return containsWord(haystack, s)
	}
	return containsWord(haystack, term+"s")
}

// containsWord reports whether haystack contains needle as a whole word, so
// "goblin" matches "Creature — Goblin Warrior" but "rat" does not match
// "Pirate".
func containsWord(haystack, needle string) bool {
	if needle == "" {
		return false
	}
	for i := 0; ; {
		j := strings.Index(haystack[i:], needle)
		if j < 0 {
			return false
		}
		start := i + j
		end := start + len(needle)
		beforeOK := start == 0 || !isWordByte(haystack[start-1])
		// Tolerate the plural an idea is usually written with.
		afterOK := end == len(haystack) || !isWordByte(haystack[end]) ||
			(haystack[end] == 's' && (end+1 == len(haystack) || !isWordByte(haystack[end+1])))
		if beforeOK && afterOK {
			return true
		}
		i = start + 1
	}
}

func isWordByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}

// lessByRank orders two cards by EDHREC popularity, unranked cards last.
func lessByRank(a, b *Card) bool {
	if a.EDHRECRank == 0 {
		return false
	}
	if b.EDHRECRank == 0 {
		return true
	}
	return a.EDHRECRank < b.EDHRECRank
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

func filterIdentity(cards []*Card, allowedMask int, exclude map[string]bool, limit int, commandersOnly bool) []*Card {
	out := make([]*Card, 0, limit)
	for _, c := range cards {
		if !c.PlayableInCommander() {
			continue
		}
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
	}
	// Rank order across the whole filtered set, then trim. Trimming first
	// would hand back whatever the text search happened to list first and
	// merely reorder that, which is not the same thing at all.
	sort.SliceStable(out, func(i, j int) bool { return lessByRank(out[i], out[j]) })
	if len(out) > limit {
		out = out[:limit]
	}
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
