package encounter

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

// ErrNotFound is returned when an encounter does not exist for the caller.
// Foreign owners are indistinguishable from missing ids: a 404 either way.
var ErrNotFound = errors.New("encounter not found")

// Encounter is a saved encounter: a party (character levels), a monster
// roster, a name, and the design notes the builder wrote for it. Difficulty
// and budget are never stored — they are recomputed from the party and roster
// on every read.
type Encounter struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Party     []int     `json:"party"`
	Monsters  []Monster `json:"monsters"`
	Notes     string    `json:"notes"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Store persists encounters in the shared SQLite database, following the
// per-package schema pattern (sibling of the chats/reviews tables; an index
// reset never touches it).
type Store struct {
	db *sql.DB
}

// New builds an encounter store on an open database handle and ensures its
// schema exists.
func New(db *sql.DB) (*Store, error) {
	s := &Store{db: db}
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("encounter migrate: %w", err)
	}
	if err := addColumnIfMissing(db, "encounters", "name", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return nil, fmt.Errorf("encounter migrate: %w", err)
	}
	// The design notes arrived with the encounter designer; installs that
	// saved encounters before it grow the column in place.
	if err := addColumnIfMissing(db, "encounters", "notes", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return nil, fmt.Errorf("encounter migrate: %w", err)
	}
	return s, nil
}

const schema = `
CREATE TABLE IF NOT EXISTS encounters (
	id          TEXT PRIMARY KEY,
	owner_id    TEXT NOT NULL,
	name        TEXT NOT NULL DEFAULT '',
	notes       TEXT NOT NULL DEFAULT '',
	party       TEXT NOT NULL DEFAULT '[]',
	monsters    TEXT NOT NULL DEFAULT '[]',
	created_at  INTEGER NOT NULL,
	updated_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS encounters_owner ON encounters(owner_id, updated_at);
`

// addColumnIfMissing runs ALTER TABLE ADD COLUMN when the column is absent,
// so a schema grown after first deploy migrates in place.
func addColumnIfMissing(db *sql.DB, table, column, decl string) error {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notNull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, decl))
	return err
}

// newID mints an unguessable row id: 16 bytes of crypto randomness, hex.
func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing is catastrophic by definition; fall back to a
		// time-derived id rather than panicking inside a request.
		return fmt.Sprintf("e%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// Create saves a new encounter for the owner.
func (s *Store) Create(ctx context.Context, owner, name string, party []int, monsters []Monster, notes string) (Encounter, error) {
	e := Encounter{
		ID: newID(), Name: strings.TrimSpace(name), Party: party, Monsters: monsters, Notes: notes,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	partyJSON, err := json.Marshal(e.Party)
	if err != nil {
		return Encounter{}, fmt.Errorf("encode party: %w", err)
	}
	monstersJSON, err := json.Marshal(e.Monsters)
	if err != nil {
		return Encounter{}, fmt.Errorf("encode monsters: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO encounters (id, owner_id, name, notes, party, monsters, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, owner, e.Name, e.Notes, string(partyJSON), string(monstersJSON), e.CreatedAt.UnixMilli(), e.UpdatedAt.UnixMilli()); err != nil {
		return Encounter{}, fmt.Errorf("insert encounter: %w", err)
	}
	return e, nil
}

// List returns the owner's encounters, most recently updated first.
func (s *Store) List(ctx context.Context, owner string) ([]Encounter, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, notes, party, monsters, created_at, updated_at
		  FROM encounters WHERE owner_id = ? ORDER BY updated_at DESC`, owner)
	if err != nil {
		return nil, fmt.Errorf("list encounters: %w", err)
	}
	return scanAll(rows)
}

// Get returns one of the owner's encounters.
func (s *Store) Get(ctx context.Context, owner, id string) (Encounter, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, notes, party, monsters, created_at, updated_at
		  FROM encounters WHERE owner_id = ? AND id = ?`, owner, id)
	if err != nil {
		return Encounter{}, fmt.Errorf("get encounter: %w", err)
	}
	out, err := scanAll(rows)
	if err != nil {
		return Encounter{}, err
	}
	if len(out) == 0 {
		return Encounter{}, ErrNotFound
	}
	return out[0], nil
}

// Update replaces the mutable fields of one of the owner's encounters.
// Empty monsters means "leave the roster alone" so a rename does not have to
// re-send the encounter body; party updates likewise only apply when sent.
func (s *Store) Update(ctx context.Context, owner, id string, name *string, party []int, monsters []Monster, notes *string, hasParty, hasMonsters bool) (Encounter, error) {
	e, err := s.Get(ctx, owner, id)
	if err != nil {
		return Encounter{}, err
	}
	if name != nil {
		e.Name = strings.TrimSpace(*name)
	}
	if notes != nil {
		e.Notes = *notes
	}
	if hasParty {
		e.Party = party
	}
	if hasMonsters {
		e.Monsters = monsters
	}
	partyJSON, err := json.Marshal(e.Party)
	if err != nil {
		return Encounter{}, fmt.Errorf("encode party: %w", err)
	}
	monstersJSON, err := json.Marshal(e.Monsters)
	if err != nil {
		return Encounter{}, fmt.Errorf("encode monsters: %w", err)
	}
	e.UpdatedAt = time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `
		UPDATE encounters SET name = ?, notes = ?, party = ?, monsters = ?, updated_at = ?
		 WHERE owner_id = ? AND id = ?`,
		e.Name, e.Notes, string(partyJSON), string(monstersJSON), e.UpdatedAt.UnixMilli(), owner, id)
	if err != nil {
		return Encounter{}, fmt.Errorf("update encounter: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Encounter{}, ErrNotFound
	}
	return e, nil
}

// Delete removes one of the owner's encounters. Foreign ids report
// ErrNotFound, the same as missing ones.
func (s *Store) Delete(ctx context.Context, owner, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM encounters WHERE owner_id = ? AND id = ?`, owner, id)
	if err != nil {
		return fmt.Errorf("delete encounter: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// scanAll drains a id/name/notes/party/monsters/created/updated cursor into
// encounters.
func scanAll(rows *sql.Rows) ([]Encounter, error) {
	defer rows.Close()
	var out []Encounter
	for rows.Next() {
		var e Encounter
		var partyJSON, monstersJSON string
		var createdMilli, updatedMilli int64
		if err := rows.Scan(&e.ID, &e.Name, &e.Notes, &partyJSON, &monstersJSON, &createdMilli, &updatedMilli); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(partyJSON), &e.Party); err != nil {
			return nil, fmt.Errorf("decode party: %w", err)
		}
		if err := json.Unmarshal([]byte(monstersJSON), &e.Monsters); err != nil {
			return nil, fmt.Errorf("decode monsters: %w", err)
		}
		e.CreatedAt = time.UnixMilli(createdMilli).UTC()
		e.UpdatedAt = time.UnixMilli(updatedMilli).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}
