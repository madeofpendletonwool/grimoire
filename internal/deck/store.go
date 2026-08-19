// Package deck is the Commander deck builder's engine room: saved decks, a
// decklist parser, mana-base math, a deterministic synergy engine over the
// local card database, and the deck analyzer. Everything here works offline;
// EDHREC enrichment is layered on by the server, never required.
package deck

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrNotFound is returned when a deck does not exist for the caller. Foreign
// owners are indistinguishable from missing ids: a 404 either way.
var ErrNotFound = errors.New("deck not found")

// Entry is one card line in a deck: a name, a count, and which board it sits
// on (maindeck by default, "commander" for the commander, "sideboard" for the
// sideboard).
type Entry struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
	Board string `json:"board,omitempty"`
	Note  string `json:"note,omitempty"`
}

// Deck is a saved deck. Analysis is never stored — it is recomputed from the
// entries on every read, the same discipline the encounter builder keeps.
type Deck struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Commander string    `json:"commander"`
	Cards     []Entry   `json:"cards"`
	Notes     string    `json:"notes"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Store persists decks in the shared SQLite database, following the
// per-package schema pattern (sibling of the encounters/reviews tables; an
// index reset never touches it).
type Store struct {
	db *sql.DB
}

// New builds a deck store on an open database handle and ensures its schema
// exists.
func New(db *sql.DB) (*Store, error) {
	s := &Store{db: db}
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("deck migrate: %w", err)
	}
	return s, nil
}

const schema = `
CREATE TABLE IF NOT EXISTS decks (
	id          TEXT PRIMARY KEY,
	owner_id    TEXT NOT NULL,
	name        TEXT NOT NULL DEFAULT '',
	commander   TEXT NOT NULL DEFAULT '',
	cards       TEXT NOT NULL DEFAULT '[]',
	notes       TEXT NOT NULL DEFAULT '',
	created_at  INTEGER NOT NULL,
	updated_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS decks_owner ON decks(owner_id, updated_at);
`

// newID mints an unguessable row id: 16 bytes of crypto randomness, hex.
func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("d%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// maxDeckRows bounds a stored deck: enough for a full 100-card list with
// sideboard, small enough that nothing can wedge the server analyzing it.
const maxDeckRows = 200

// maxCopies bounds one entry's count. Commander allows any number of a basic
// land and one of everything else; 99 is the theoretical ceiling either way.
const maxCopies = 99

// Create saves a new deck for the owner.
func (s *Store) Create(ctx context.Context, owner, name, commander string, cards []Entry, notes string) (Deck, error) {
	normalized, err := normalizeEntries(cards)
	if err != nil {
		return Deck{}, err
	}
	d := Deck{
		ID: newID(), Name: strings.TrimSpace(name), Commander: strings.TrimSpace(commander),
		Cards: normalized, Notes: strings.TrimSpace(notes),
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	cardsJSON, err := json.Marshal(d.Cards)
	if err != nil {
		return Deck{}, fmt.Errorf("encode cards: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO decks (id, owner_id, name, commander, cards, notes, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, owner, d.Name, d.Commander, string(cardsJSON), d.Notes,
		d.CreatedAt.UnixMilli(), d.UpdatedAt.UnixMilli()); err != nil {
		return Deck{}, fmt.Errorf("insert deck: %w", err)
	}
	return d, nil
}

// List returns the owner's decks, most recently updated first.
func (s *Store) List(ctx context.Context, owner string) ([]Deck, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, commander, cards, notes, created_at, updated_at
		  FROM decks WHERE owner_id = ? ORDER BY updated_at DESC`, owner)
	if err != nil {
		return nil, fmt.Errorf("list decks: %w", err)
	}
	return scanAll(rows)
}

// Get returns one of the owner's decks.
func (s *Store) Get(ctx context.Context, owner, id string) (Deck, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, commander, cards, notes, created_at, updated_at
		  FROM decks WHERE owner_id = ? AND id = ?`, owner, id)
	if err != nil {
		return Deck{}, fmt.Errorf("get deck: %w", err)
	}
	out, err := scanAll(rows)
	if err != nil {
		return Deck{}, err
	}
	if len(out) == 0 {
		return Deck{}, ErrNotFound
	}
	return out[0], nil
}

// Update replaces the mutable fields of one of the owner's decks. nil/absent
// fields keep their stored value, so a rename does not have to re-send the
// card list.
func (s *Store) Update(ctx context.Context, owner, id string, name, commander, notes *string, cards []Entry, hasCards bool) (Deck, error) {
	d, err := s.Get(ctx, owner, id)
	if err != nil {
		return Deck{}, err
	}
	if name != nil {
		d.Name = strings.TrimSpace(*name)
	}
	if commander != nil {
		d.Commander = strings.TrimSpace(*commander)
	}
	if notes != nil {
		d.Notes = strings.TrimSpace(*notes)
	}
	if hasCards {
		normalized, err := normalizeEntries(cards)
		if err != nil {
			return Deck{}, err
		}
		d.Cards = normalized
	}
	cardsJSON, err := json.Marshal(d.Cards)
	if err != nil {
		return Deck{}, fmt.Errorf("encode cards: %w", err)
	}
	d.UpdatedAt = time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `
		UPDATE decks SET name = ?, commander = ?, cards = ?, notes = ?, updated_at = ?
		 WHERE owner_id = ? AND id = ?`,
		d.Name, d.Commander, string(cardsJSON), d.Notes, d.UpdatedAt.UnixMilli(), owner, id)
	if err != nil {
		return Deck{}, fmt.Errorf("update deck: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Deck{}, ErrNotFound
	}
	return d, nil
}

// Delete removes one of the owner's decks. Foreign ids report ErrNotFound,
// the same as missing ones.
func (s *Store) Delete(ctx context.Context, owner, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM decks WHERE owner_id = ? AND id = ?`, owner, id)
	if err != nil {
		return fmt.Errorf("delete deck: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// normalizeEntries trims names, clamps counts, merges duplicates, and bounds
// the row count.
func normalizeEntries(cards []Entry) ([]Entry, error) {
	if len(cards) > maxDeckRows {
		return nil, fmt.Errorf("decks are limited to %d card lines", maxDeckRows)
	}
	seen := map[string]int{} // key: board + name
	out := make([]Entry, 0, len(cards))
	for _, e := range cards {
		e.Name = strings.TrimSpace(e.Name)
		if e.Name == "" {
			continue
		}
		if e.Count < 1 {
			e.Count = 1
		}
		if e.Count > maxCopies {
			e.Count = maxCopies
		}
		key := e.Board + "\x00" + strings.ToLower(e.Name)
		if i, ok := seen[key]; ok {
			out[i].Count += e.Count
			if out[i].Count > maxCopies {
				out[i].Count = maxCopies
			}
			continue
		}
		seen[key] = len(out)
		out = append(out, e)
	}
	return out, nil
}

// scanAll drains an id/name/commander/cards/notes/created/updated cursor.
func scanAll(rows *sql.Rows) ([]Deck, error) {
	defer rows.Close()
	var out []Deck
	for rows.Next() {
		var d Deck
		var cardsJSON string
		var createdMilli, updatedMilli int64
		if err := rows.Scan(&d.ID, &d.Name, &d.Commander, &cardsJSON, &d.Notes, &createdMilli, &updatedMilli); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(cardsJSON), &d.Cards); err != nil {
			return nil, fmt.Errorf("decode cards: %w", err)
		}
		d.CreatedAt = time.UnixMilli(createdMilli).UTC()
		d.UpdatedAt = time.UnixMilli(updatedMilli).UTC()
		out = append(out, d)
	}
	return out, rows.Err()
}
