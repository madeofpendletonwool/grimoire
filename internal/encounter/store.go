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
//
// Since MAD-378 an encounter may also belong to a campaign, and then it is the
// one record a planned fight has: CampaignID scopes it, SessionEventID names
// the 'encounter' session event it is the long form of (the event stays the
// canonical marker that something is planned for a session — ADR 9), SceneID
// the spine scene whose roster it is. Every one of those is empty on an
// ordinary owner-scoped encounter, which is exactly what the builder has
// always saved.
//
// Objective and Terrain are reserved by migration 0026 for the objectives
// issue; this package reads and round-trips them so that issue needs no
// migration, and nothing writes them yet.
type Encounter struct {
	ID             string         `json:"id"`
	Name           string         `json:"name"`
	Party          []int          `json:"party"`
	Monsters       []Monster      `json:"monsters"`
	Notes          string         `json:"notes"`
	CampaignID     string         `json:"campaign_id,omitempty"`
	SessionEventID string         `json:"session_event_id,omitempty"`
	SceneID        string         `json:"scene_id,omitempty"`
	Objective      string         `json:"objective,omitempty"`
	Terrain        map[string]any `json:"terrain,omitempty"`
	Status         string         `json:"status"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
}

// The statuses a campaign-scoped encounter moves through. An owner-scoped
// encounter carries StatusPlanned too: it is the column default, and "planned"
// is what an encounter in the builder is.
const (
	StatusPlanned   = "planned"
	StatusRun       = "run"
	StatusDiscarded = "discarded"
)

// validStatus is the vocabulary the CHECK constraint enforces, checked here
// first so a bad value is ErrInvalid rather than a SQL constraint error.
var validStatus = map[string]bool{StatusPlanned: true, StatusRun: true, StatusDiscarded: true}

// ErrInvalid marks input that violates a vocabulary or shape constraint.
var ErrInvalid = errors.New("invalid encounter")

// Scope is the campaign half of a new encounter: which campaign it belongs
// to, which session event it is the long form of, which scene's roster it is,
// and what state it is in. The zero Scope is an ordinary owner-scoped
// encounter — the builder's default, and the one MAD-299 shipped.
type Scope struct {
	CampaignID     string
	SessionEventID string
	SceneID        string
	Status         string
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
	// The campaign columns arrived with MAD-378. Migration 0026 is what grows
	// them on a real install — it rebuilds the table so the CHECK on status
	// lands too — and these keep a package-first database (the schema-compat
	// test, and this package's own tests) in the same shape.
	for _, col := range [...]struct{ name, decl string }{
		{"campaign_id", "TEXT NOT NULL DEFAULT ''"},
		{"session_event_id", "TEXT NOT NULL DEFAULT ''"},
		{"scene_id", "TEXT NOT NULL DEFAULT ''"},
		{"objective", "TEXT NOT NULL DEFAULT ''"},
		{"terrain", "TEXT NOT NULL DEFAULT '{}'"},
		{"status", "TEXT NOT NULL DEFAULT 'planned'"},
	} {
		if err := addColumnIfMissing(db, "encounters", col.name, col.decl); err != nil {
			return nil, fmt.Errorf("encounter migrate: %w", err)
		}
	}
	// The campaign index names a column the loop above may have just grown,
	// so it cannot live in the schema const: a table that predates MAD-378
	// would fail CREATE INDEX on a column that does not exist yet. Boot runs
	// migrate.Up before any store constructs, so real installs arrive here
	// already grown — this ordering is for the package-first shapes the
	// schema-compat test builds.
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS encounters_campaign ON encounters(campaign_id, updated_at)`); err != nil {
		return nil, fmt.Errorf("encounter migrate: %w", err)
	}
	return s, nil
}

const schema = `
CREATE TABLE IF NOT EXISTS encounters (
	id               TEXT PRIMARY KEY,
	owner_id         TEXT NOT NULL,
	name             TEXT NOT NULL DEFAULT '',
	notes            TEXT NOT NULL DEFAULT '',
	party            TEXT NOT NULL DEFAULT '[]',
	monsters         TEXT NOT NULL DEFAULT '[]',
	campaign_id      TEXT NOT NULL DEFAULT '',
	session_event_id TEXT NOT NULL DEFAULT '',
	scene_id         TEXT NOT NULL DEFAULT '',
	objective        TEXT NOT NULL DEFAULT '',
	terrain          TEXT NOT NULL DEFAULT '{}',
	status           TEXT NOT NULL DEFAULT 'planned'
	                   CHECK (status IN ('planned','run','discarded')),
	created_at       INTEGER NOT NULL,
	updated_at       INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS encounters_owner ON encounters(owner_id, updated_at);
`

// encounterCols is the read projection every scanAll cursor must supply, in
// order. One constant so a column added to the table is added to one query.
const encounterCols = `id, name, notes, party, monsters, campaign_id, session_event_id,
	                    scene_id, objective, terrain, status, created_at, updated_at`

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

// Create saves a new encounter for the owner, belonging to no campaign. It is
// what the builder has always called and its behaviour is unchanged.
func (s *Store) Create(ctx context.Context, owner, name string, party []int, monsters []Monster, notes string) (Encounter, error) {
	return s.CreateIn(ctx, owner, Scope{}, name, party, monsters, notes)
}

// CreateIn saves a new encounter inside a campaign scope. The zero Scope is
// exactly Create: an owner-scoped encounter with no campaign, which is the
// fallback for a DM who has no campaign and must not regress.
//
// The owner is recorded either way. A campaign-scoped encounter is read back
// through the campaign (ListCampaign / GetCampaign, DM-gated at the HTTP
// layer, per ADR 2 — a roster is DM material by definition), but knowing who
// wrote it is worth keeping and it is what lets the builder's own saved list
// still show it.
func (s *Store) CreateIn(ctx context.Context, owner string, scope Scope, name string, party []int, monsters []Monster, notes string) (Encounter, error) {
	status := strings.TrimSpace(scope.Status)
	if status == "" {
		status = StatusPlanned
	}
	if !validStatus[status] {
		return Encounter{}, fmt.Errorf("%w: status %q", ErrInvalid, scope.Status)
	}
	if scope.CampaignID == "" && (scope.SessionEventID != "" || scope.SceneID != "") {
		return Encounter{}, fmt.Errorf("%w: a session event or scene needs a campaign", ErrInvalid)
	}
	now := time.Now().UTC()
	e := Encounter{
		ID: newID(), Name: strings.TrimSpace(name), Party: party, Monsters: monsters, Notes: notes,
		CampaignID: scope.CampaignID, SessionEventID: scope.SessionEventID, SceneID: scope.SceneID,
		Status: status, CreatedAt: now, UpdatedAt: now,
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
		INSERT INTO encounters (id, owner_id, name, notes, party, monsters,
		                        campaign_id, session_event_id, scene_id, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, owner, e.Name, e.Notes, string(partyJSON), string(monstersJSON),
		e.CampaignID, e.SessionEventID, e.SceneID, e.Status,
		e.CreatedAt.UnixMilli(), e.UpdatedAt.UnixMilli()); err != nil {
		return Encounter{}, fmt.Errorf("insert encounter: %w", err)
	}
	return e, nil
}

// List returns the owner's encounters, most recently updated first —
// campaign-scoped ones included, because they are still theirs and the
// builder's saved picker is where they made them.
func (s *Store) List(ctx context.Context, owner string) ([]Encounter, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+encounterCols+`
		  FROM encounters WHERE owner_id = ? ORDER BY updated_at DESC`, owner)
	if err != nil {
		return nil, fmt.Errorf("list encounters: %w", err)
	}
	return scanAll(rows)
}

// Get returns one of the owner's encounters.
func (s *Store) Get(ctx context.Context, owner, id string) (Encounter, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+encounterCols+`
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

// ListCampaign returns a campaign's encounters, most recently updated first,
// regardless of which member of the table wrote them. An empty campaign id
// returns nothing rather than every ownerless encounter in the database:
// "belongs to no campaign" is not a campaign anyone can ask about.
func (s *Store) ListCampaign(ctx context.Context, campaignID string) ([]Encounter, error) {
	if strings.TrimSpace(campaignID) == "" {
		return nil, fmt.Errorf("%w: a campaign id is required", ErrInvalid)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+encounterCols+`
		  FROM encounters WHERE campaign_id = ? ORDER BY updated_at DESC`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("list campaign encounters: %w", err)
	}
	return scanAll(rows)
}

// GetCampaign returns one of a campaign's encounters. An id from another
// campaign is indistinguishable from a missing one, the same rule Get applies
// to a foreign owner.
func (s *Store) GetCampaign(ctx context.Context, campaignID, id string) (Encounter, error) {
	if strings.TrimSpace(campaignID) == "" {
		return Encounter{}, fmt.Errorf("%w: a campaign id is required", ErrInvalid)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+encounterCols+`
		  FROM encounters WHERE campaign_id = ? AND id = ?`, campaignID, id)
	if err != nil {
		return Encounter{}, fmt.Errorf("get campaign encounter: %w", err)
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

// scanAll drains an encounterCols cursor into encounters.
//
// Terrain is decoded leniently: it is a column this issue reserves and does
// not write, so a hand-edited or empty value costs the terrain block and never
// the encounter.
func scanAll(rows *sql.Rows) ([]Encounter, error) {
	defer rows.Close()
	var out []Encounter
	for rows.Next() {
		var e Encounter
		var partyJSON, monstersJSON, terrainJSON string
		var createdMilli, updatedMilli int64
		if err := rows.Scan(&e.ID, &e.Name, &e.Notes, &partyJSON, &monstersJSON,
			&e.CampaignID, &e.SessionEventID, &e.SceneID, &e.Objective, &terrainJSON, &e.Status,
			&createdMilli, &updatedMilli); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(partyJSON), &e.Party); err != nil {
			return nil, fmt.Errorf("decode party: %w", err)
		}
		if err := json.Unmarshal([]byte(monstersJSON), &e.Monsters); err != nil {
			return nil, fmt.Errorf("decode monsters: %w", err)
		}
		if terrainJSON != "" && terrainJSON != "{}" {
			var terrain map[string]any
			if json.Unmarshal([]byte(terrainJSON), &terrain) == nil {
				e.Terrain = terrain
			}
		}
		e.CreatedAt = time.UnixMilli(createdMilli).UTC()
		e.UpdatedAt = time.UnixMilli(updatedMilli).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}
