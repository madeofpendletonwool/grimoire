package carddb

import (
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultMTGJSONURL mirrors data.DefaultMTGJSONURL: the canonical AtomicCards
// endpoint. Declared separately so carddb has no dependency on the data
// package (and vice versa); the caller passes whichever URL it configured.
const DefaultMTGJSONURL = "https://mtgjson.com/api/v5/AtomicCards.json.gz"

// fetchTimeout bounds the AtomicCards download during an index build.
const fetchTimeout = 5 * time.Minute

// maxPayloadBytes is a safety ceiling on the decompressed payload (the real
// size is ~100 MiB).
const maxPayloadBytes = 512 << 20

// chunkSize is how many cards one staging transaction inserts, so the shared
// SQLite connection returns to the pool between chunks while a rebuild runs.
const chunkSize = 1000

// Populate downloads MTGJSON AtomicCards (url empty = canonical endpoint),
// decodes it streaming, and replaces the card tables. It returns the card
// names so one download can also refresh the chat's card-name dictionary.
//
// The rebuild is staged: rows land in cards_build/cards_fts_build chunk by
// chunk, then swap in with the live tables in one short transaction — readers
// see either the old set or the new one, never a half-built mix.
func Populate(ctx context.Context, db *sql.DB, url string) (names []string, err error) {
	s := &Store{db: db}
	if url == "" {
		url = DefaultMTGJSONURL
	}
	inner, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(inner, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("accept", "application/json")
	req.Header.Set("user-agent", "grimoire/1.0 (+https://github.com/madeofpendletonwool/grimoire)")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch mtgjson: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mtgjson: %s", resp.Status)
	}
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("gunzip mtgjson: %w", err)
	}
	defer zr.Close()

	if err := s.stageBegin(); err != nil {
		return nil, err
	}
	names, err = s.stageStream(io.LimitReader(zr, maxPayloadBytes))
	if err != nil {
		_, _ = db.Exec(`DROP TABLE IF EXISTS cards_build; DROP TABLE IF EXISTS cards_fts_build`)
		return nil, err
	}
	if err := s.swap(); err != nil {
		return nil, err
	}
	return names, nil
}

// atomicFace is the subset of MTGJSON's Card (Atomic) model carddb captures.
// Unknown fields are ignored by the decoder, so model additions are safe.
type atomicFace struct {
	ManaCost        string   `json:"manaCost"`
	ManaValue       float64  `json:"manaValue"`
	Type            string   `json:"type"`
	Text            string   `json:"text"`
	ColorIdentity   []string `json:"colorIdentity"`
	EDHRECRank      *int     `json:"edhrecRank"`
	EDHRECSaltiness *float64 `json:"edhrecSaltiness"`
	Leadership      *struct {
		Commander bool `json:"Commander"`
	} `json:"leadershipSkills"`
	Legalities    map[string]string `json:"legalities"`
	IsGameChanger bool              `json:"isGameChanger"`
}

// stageBegin drops any leftover staging tables from a failed run and declares
// fresh ones.
func (s *Store) stageBegin() error {
	if _, err := s.db.Exec(`DROP TABLE IF EXISTS cards_build; DROP TABLE IF EXISTS cards_fts_build; ` + buildSchema); err != nil {
		return fmt.Errorf("stage cards: %w", err)
	}
	return nil
}

const buildSchema = `
CREATE TABLE cards_build (
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
CREATE VIRTUAL TABLE cards_fts_build USING fts5(
	name,
	type_line,
	oracle_text,
	tokenize = 'porter unicode61 remove_diacritics 2'
);
`

// stageStream decodes the AtomicCards payload name by name, folding each
// name's face array into one Card and writing it in chunked transactions.
func (s *Store) stageStream(r io.Reader) ([]string, error) {
	dec := json.NewDecoder(r)
	if err := expectDelim(dec, '{'); err != nil {
		return nil, err
	}
	var names []string
	var chunk []Card
	flush := func() error {
		if len(chunk) == 0 {
			return nil
		}
		if err := s.insertChunk(chunk); err != nil {
			return err
		}
		chunk = chunk[:0]
		return nil
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, fmt.Errorf("mtgjson: expected object key, got %T", keyTok)
		}
		if key != "data" {
			if err := skipValue(dec); err != nil {
				return nil, err
			}
			continue
		}
		if err := expectDelim(dec, '{'); err != nil {
			return nil, err
		}
		for dec.More() {
			nameTok, err := dec.Token()
			if err != nil {
				return nil, err
			}
			name, ok := nameTok.(string)
			if !ok {
				return nil, fmt.Errorf("mtgjson: expected card name key, got %T", nameTok)
			}
			var faces []atomicFace
			if err := dec.Decode(&faces); err != nil {
				return nil, fmt.Errorf("decode %q: %w", name, err)
			}
			names = append(names, name)
			chunk = append(chunk, foldCard(name, faces))
			if len(chunk) >= chunkSize {
				if err := flush(); err != nil {
					return nil, err
				}
			}
		}
		// Consume the data object's closing brace.
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return names, nil
}

// foldCard merges a name's faces (one for most cards, two for split/adventure
// cards) into a single row. Face-specific fields take the first face; set-like
// fields (identity, commander eligibility) are unions.
func foldCard(name string, faces []atomicFace) Card {
	if len(faces) == 0 {
		return Card{Name: name}
	}
	c := Card{Name: name}
	var identity string
	var costs, types, texts []string
	for i, f := range faces {
		if f.ManaCost != "" {
			costs = append(costs, f.ManaCost)
		}
		if f.Type != "" {
			types = append(types, f.Type)
		}
		if f.Text != "" {
			texts = append(texts, f.Text)
		}
		for _, col := range f.ColorIdentity {
			identity += col
		}
		if f.ManaValue > c.ManaValue {
			c.ManaValue = f.ManaValue
		}
		if i == 0 {
			if f.EDHRECRank != nil {
				c.EDHRECRank = *f.EDHRECRank
			}
			if f.EDHRECSaltiness != nil {
				c.EDHRECSaltiness = *f.EDHRECSaltiness
			}
			c.LegalCommander = f.Legalities["commander"]
		}
		if f.Leadership != nil && f.Leadership.Commander {
			c.CommanderLegal = true
		}
		if f.IsGameChanger {
			c.GameChanger = true
		}
	}
	c.ManaCost = joinNonEmpty(costs, " // ")
	c.TypeLine = joinNonEmpty(types, " // ")
	c.OracleText = joinNonEmpty(texts, " // ")
	c.ColorIdentity = NormalizeIdentity(identity)
	return c
}

func joinNonEmpty(parts []string, sep string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, sep)
}

// insertChunk writes one chunk of cards plus their FTS rows in a single
// transaction.
func (s *Store) insertChunk(cards []Card) error {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.Prepare(`
		INSERT OR REPLACE INTO cards_build
			(name, mana_cost, mana_value, type_line, oracle_text, color_identity,
			 color_mask, edhrec_rank, edhrec_saltiness, commander_legal, legal_commander, game_changer)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare card insert: %w", err)
	}
	defer stmt.Close()
	fts, err := tx.Prepare(`INSERT INTO cards_fts_build(name, type_line, oracle_text) VALUES (?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare fts insert: %w", err)
	}
	defer fts.Close()
	for _, c := range cards {
		var rank any
		if c.EDHRECRank > 0 {
			rank = c.EDHRECRank
		}
		if _, err := stmt.Exec(c.Name, c.ManaCost, c.ManaValue, c.TypeLine, c.OracleText,
			c.ColorIdentity, ColorMask(c.ColorIdentity), rank, c.EDHRECSaltiness,
			c.CommanderLegal, c.LegalCommander, c.GameChanger); err != nil {
			return fmt.Errorf("insert card %q: %w", c.Name, err)
		}
		if _, err := fts.Exec(c.Name, c.TypeLine, c.OracleText); err != nil {
			return fmt.Errorf("insert card fts %q: %w", c.Name, err)
		}
	}
	return tx.Commit()
}

// swap replaces the live tables with the staged ones in one short transaction.
func (s *Store) swap() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, q := range []string{
		`DROP TABLE IF EXISTS cards`,
		`DROP TABLE IF EXISTS cards_fts`,
		`ALTER TABLE cards_build RENAME TO cards`,
		`ALTER TABLE cards_fts_build RENAME TO cards_fts`,
		`CREATE INDEX IF NOT EXISTS cards_rank ON cards(edhrec_rank)`,
	} {
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("swap cards: %w", err)
		}
	}
	return tx.Commit()
}

// expectDelim reads one token and requires it to be the given delimiter.
func expectDelim(dec *json.Decoder, want rune) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	d, ok := tok.(json.Delim)
	if !ok || rune(d) != want {
		return fmt.Errorf("mtgjson: expected %q, got %v", string(want), tok)
	}
	return nil
}

// skipValue advances the decoder past a single JSON value without buffering it.
func skipValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	if d != '{' && d != '[' {
		return fmt.Errorf("mtgjson: unexpected delimiter %v", d)
	}
	for depth := 1; depth > 0; {
		t, err := dec.Token()
		if err != nil {
			return err
		}
		if dd, ok := t.(json.Delim); ok {
			switch dd {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
	}
	return nil
}
