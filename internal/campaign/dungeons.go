package campaign

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/madeofpendletonwool/grimoire/internal/dungeon"
)

// The dungeon designer's persistence (MAD-373): a dungeon is planning
// material in its own tables — the seeded room graph internal/dungeon
// computes, the dressing pass's names and detail, the DM's grid edits —
// and touches the campaign graph only when it is placed, through the
// proposal batch gate (MAD-359). Nothing here generates anything: the
// layout is pure and computed once at creation; every later write is an
// edit that never re-rolls the dungeon.

// Dungeon statuses: draft is the bare computed graph, dressed carries the
// model's (or the DM's) names and detail, placed has its proposal batch
// accepted and its entities in the graph.
const (
	DungeonDraft   = "draft"
	DungeonDressed = "dressed"
	DungeonPlaced  = "placed"
)

var dungeonStatuses = map[string]bool{
	DungeonDraft: true, DungeonDressed: true, DungeonPlaced: true,
}

// Dungeon is one designed dungeon: the params and seed the layout was
// computed from, the dressing's key item name, its placement state, and
// its rooms and edges (populated by the reads and by creation; the list
// read carries counts instead).
type Dungeon struct {
	ID               string
	CampaignID       string
	Name             string
	Theme            string
	Size             string
	Level            int
	ExpectedSessions int
	Seed             int64
	Params           dungeon.Params
	KeyItem          string
	Secret           string
	BossName         string
	LocationEntity   string
	Status           string
	Rooms            []DungeonRoom
	Edges            []DungeonEdge
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// DungeonRoom is one room row: the graph room's stable key, its purpose,
// its grid cell, its depth, the dressing's (or the DM's) name and detail,
// and the entity and encounter links — the entity written when the
// dungeon is placed, the encounter a deliberately soft column (MAD-317's
// to harden).
type DungeonRoom struct {
	ID          string
	DungeonID   string
	Key         string
	Name        string
	Purpose     string
	Detail      string
	X           int
	Y           int
	Depth       int
	EntityID    string
	EncounterID string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// DungeonEdge is one connection between two room keys.
type DungeonEdge struct {
	ID            string
	DungeonID     string
	FromRoom      string
	ToRoom        string
	Kind          string
	KeyItemEntity string
	OneWay        bool
	CreatedAt     time.Time
}

// DungeonInput is what a dungeon is created from: a name, the theme, and
// the layout params. Seed zero mints a fresh seed — recorded, not
// regenerated: re-rolling is a new dungeon with a new seed.
type DungeonInput struct {
	Name   string
	Theme  string
	Params dungeon.Params
	Seed   int64
}

// CreateDungeon computes the layout and stores it: one dungeons row, one
// row per room, one per edge. The layout happens here, once; everything
// after is an edit. DM-scope by construction (the server gates it); the
// store enforces the vocabularies the CHECK constraints carry anyway.
func (s *Store) CreateDungeon(ctx context.Context, campaignID string, in DungeonInput) (*Dungeon, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, fmt.Errorf("%w: dungeon name is required", ErrInvalid)
	}
	params := in.Params
	if params.Theme == "" {
		params.Theme = strings.TrimSpace(in.Theme)
	} else {
		params.Theme = strings.TrimSpace(params.Theme)
	}
	seed := in.Seed
	if seed == 0 {
		seed = time.Now().UTC().UnixNano()
	}
	graph, err := dungeon.Layout(params, seed)
	if err != nil {
		// The layout package is pure and carries no campaign vocabulary;
		// its refusals are this store's invalid input.
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if err := s.campaignExists(ctx, campaignID); err != nil {
		return nil, err
	}

	paramsJSON, err := json.Marshal(graph.Params)
	if err != nil {
		return nil, fmt.Errorf("encode dungeon params: %w", err)
	}
	now := time.Now().UTC().UnixMilli()
	d := &Dungeon{
		ID: uuid.NewString(), CampaignID: campaignID, Name: name,
		Theme: graph.Params.Theme, Size: graph.Params.Size, Level: graph.Params.Level,
		ExpectedSessions: graph.Params.ExpectedSessions, Seed: graph.Seed,
		Params: graph.Params, Status: DungeonDraft,
		CreatedAt: time.UnixMilli(now).UTC(), UpdatedAt: time.UnixMilli(now).UTC(),
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("dungeon tx: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO dungeons (id, campaign_id, name, theme, size, level, expected_sessions, seed, params, key_item, secret, boss_name, location_entity, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, '', '', '', NULL, ?, ?, ?)`,
		d.ID, d.CampaignID, d.Name, d.Theme, d.Size, d.Level, d.ExpectedSessions, d.Seed,
		string(paramsJSON), d.Status, now, now); err != nil {
		return nil, fmt.Errorf("insert dungeon: %w", err)
	}
	for _, r := range graph.Rooms {
		room := DungeonRoom{
			ID: uuid.NewString(), DungeonID: d.ID, Key: r.Key, Purpose: r.Purpose,
			X: r.X, Y: r.Y, Depth: r.Depth,
			CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt,
		}
		d.Rooms = append(d.Rooms, room)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO dungeon_rooms (id, dungeon_id, key, name, purpose, detail, x, y, depth, entity_id, encounter_id, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, '', ?, ?, ?, NULL, '', ?, ?)`,
			room.ID, d.ID, room.Key, room.Name, room.Purpose, room.X, room.Y, room.Depth, now, now); err != nil {
			return nil, fmt.Errorf("insert dungeon room: %w", err)
		}
	}
	for _, e := range graph.Edges {
		edge := DungeonEdge{
			ID: uuid.NewString(), DungeonID: d.ID, FromRoom: e.From, ToRoom: e.To,
			Kind: e.Kind, OneWay: e.OneWay, CreatedAt: d.CreatedAt,
		}
		d.Edges = append(d.Edges, edge)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO dungeon_edges (id, dungeon_id, from_room, to_room, kind, key_item_entity, one_way, created_at)
			VALUES (?, ?, ?, ?, ?, NULL, ?, ?)`,
			edge.ID, d.ID, edge.FromRoom, edge.ToRoom, edge.Kind, boolInt(edge.OneWay), now); err != nil {
			return nil, fmt.Errorf("insert dungeon edge: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("dungeon commit: %w", err)
	}
	return d, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

const dungeonCols = `id, campaign_id, name, theme, size, level, expected_sessions, seed, params, key_item, secret, boss_name, location_entity, status, created_at, updated_at`

func scanDungeon(row interface{ Scan(...any) error }) (*Dungeon, error) {
	var (
		d          Dungeon
		paramsJSON string
		location   sql.NullString
		created    int64
		updated    int64
	)
	if err := row.Scan(&d.ID, &d.CampaignID, &d.Name, &d.Theme, &d.Size, &d.Level,
		&d.ExpectedSessions, &d.Seed, &paramsJSON, &d.KeyItem, &d.Secret, &d.BossName, &location, &d.Status, &created, &updated); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(paramsJSON), &d.Params); err != nil {
		return nil, fmt.Errorf("dungeon %s carries invalid params JSON: %w", d.ID, err)
	}
	d.LocationEntity = location.String
	d.CreatedAt = time.UnixMilli(created).UTC()
	d.UpdatedAt = time.UnixMilli(updated).UTC()
	return &d, nil
}

// loadDungeonRooms reads a dungeon's rooms in declaration order (key).
func (s *Store) loadDungeonRooms(ctx context.Context, dungeonID string) ([]DungeonRoom, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, dungeon_id, key, name, purpose, detail, x, y, depth, entity_id, encounter_id, created_at, updated_at
		  FROM dungeon_rooms WHERE dungeon_id = ? ORDER BY key`, dungeonID)
	if err != nil {
		return nil, fmt.Errorf("list dungeon rooms: %w", err)
	}
	defer rows.Close()
	var out []DungeonRoom
	for rows.Next() {
		var (
			r       DungeonRoom
			entity  sql.NullString
			created int64
			updated int64
		)
		if err := rows.Scan(&r.ID, &r.DungeonID, &r.Key, &r.Name, &r.Purpose, &r.Detail,
			&r.X, &r.Y, &r.Depth, &entity, &r.EncounterID, &created, &updated); err != nil {
			return nil, err
		}
		r.EntityID = entity.String
		r.CreatedAt = time.UnixMilli(created).UTC()
		r.UpdatedAt = time.UnixMilli(updated).UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

// loadDungeonEdges reads a dungeon's edges in declaration order.
func (s *Store) loadDungeonEdges(ctx context.Context, dungeonID string) ([]DungeonEdge, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, dungeon_id, from_room, to_room, kind, key_item_entity, one_way, created_at
		  FROM dungeon_edges WHERE dungeon_id = ? ORDER BY rowid`, dungeonID)
	if err != nil {
		return nil, fmt.Errorf("list dungeon edges: %w", err)
	}
	defer rows.Close()
	var out []DungeonEdge
	for rows.Next() {
		var (
			e       DungeonEdge
			keyItem sql.NullString
			oneWay  int
			created int64
		)
		if err := rows.Scan(&e.ID, &e.DungeonID, &e.FromRoom, &e.ToRoom, &e.Kind, &keyItem, &oneWay, &created); err != nil {
			return nil, err
		}
		e.KeyItemEntity = keyItem.String
		e.OneWay = oneWay != 0
		e.CreatedAt = time.UnixMilli(created).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetDungeon returns one dungeon of a campaign with its rooms and edges.
// DM-scope reads only: a dungeon is planning material, secrets and all.
func (s *Store) GetDungeon(ctx context.Context, scope Scope, campaignID, id string) (*Dungeon, error) {
	if err := scope.requireDM(); err != nil {
		return nil, err
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT `+dungeonCols+` FROM dungeons WHERE id = ? AND campaign_id = ?`, id, campaignID)
	d, err := scanDungeon(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: dungeon %s", ErrNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	if d.Rooms, err = s.loadDungeonRooms(ctx, d.ID); err != nil {
		return nil, err
	}
	if d.Edges, err = s.loadDungeonEdges(ctx, d.ID); err != nil {
		return nil, err
	}
	return d, nil
}

// ListDungeons lists a campaign's dungeons by name. DM-scope reads only.
func (s *Store) ListDungeons(ctx context.Context, scope Scope, campaignID string) ([]Dungeon, error) {
	if err := scope.requireDM(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+dungeonCols+` FROM dungeons WHERE campaign_id = ? ORDER BY name COLLATE NOCASE`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("list dungeons: %w", err)
	}
	defer rows.Close()
	var out []Dungeon
	for rows.Next() {
		d, err := scanDungeon(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

// DungeonUpdate patches a dungeon's headline: its name and theme. The
// layout is not patchable — the seed and the params are the dungeon, and
// re-rolling is a new dungeon (MAD-373's recorded-decision rule).
type DungeonUpdate struct {
	Name  *string
	Theme *string
}

// UpdateDungeon applies a headline patch to one dungeon.
func (s *Store) UpdateDungeon(ctx context.Context, campaignID, dungeonID string, up DungeonUpdate) (*Dungeon, error) {
	if _, err := s.GetDungeon(ctx, ScopeDM, campaignID, dungeonID); err != nil {
		return nil, err
	}
	sets := []string{"updated_at = ?"}
	args := []any{time.Now().UTC().UnixMilli()}
	if up.Name != nil {
		name := strings.TrimSpace(*up.Name)
		if name == "" {
			return nil, fmt.Errorf("%w: dungeon name is required", ErrInvalid)
		}
		sets = append(sets, "name = ?")
		args = append(args, name)
	}
	if up.Theme != nil {
		sets = append(sets, "theme = ?")
		args = append(args, strings.TrimSpace(*up.Theme))
	}
	args = append(args, dungeonID, campaignID)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE dungeons SET `+strings.Join(sets, ", ")+` WHERE id = ? AND campaign_id = ?`, args...); err != nil {
		return nil, fmt.Errorf("update dungeon: %w", err)
	}
	return s.GetDungeon(ctx, ScopeDM, campaignID, dungeonID)
}

// DeleteDungeon removes the design: the dungeon row and its rooms and
// edges go; entities placed into the campaign graph by an accepted batch
// stay, because the graph is not a dungeon's to unwind. A placed dungeon
// refuses — dismiss or keep its placement instead of stranding rows that
// name it.
func (s *Store) DeleteDungeon(ctx context.Context, campaignID, dungeonID string) error {
	d, err := s.GetDungeon(ctx, ScopeDM, campaignID, dungeonID)
	if err != nil {
		return err
	}
	if d.Status == DungeonPlaced {
		return fmt.Errorf("%w: %q is placed in the world; its entities stay — dismiss or keep the placement", ErrInvalid, d.Name)
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM dungeons WHERE id = ? AND campaign_id = ?`, dungeonID, campaignID); err != nil {
		return fmt.Errorf("delete dungeon: %w", err)
	}
	return nil
}

/* ---------- room edits: the map is editable, and never re-rolls ---------- */

// DungeonRoomUpdate patches one room: the dressing's name and detail, the
// dragged cell (x, y), and the encounter link — the deliberately soft
// column MAD-317 will harden. Purpose and depth are the layout's; an
// edit may not re-roll them.
type DungeonRoomUpdate struct {
	Name        *string
	Detail      *string
	X           *int
	Y           *int
	EncounterID *string
}

// UpdateDungeonRoom applies one room patch. Moving a room onto an
// occupied cell is refused — the grid is one room per cell by design,
// and the map renders what is stored.
func (s *Store) UpdateDungeonRoom(ctx context.Context, campaignID, dungeonID, roomID string, up DungeonRoomUpdate) (*Dungeon, error) {
	d, err := s.GetDungeon(ctx, ScopeDM, campaignID, dungeonID)
	if err != nil {
		return nil, err
	}
	at := -1
	for i := range d.Rooms {
		if d.Rooms[i].ID == roomID {
			at = i
			break
		}
	}
	if at < 0 {
		return nil, fmt.Errorf("%w: room %s of dungeon %s", ErrNotFound, roomID, dungeonID)
	}
	room := d.Rooms[at]

	if up.X != nil || up.Y != nil {
		x, y := room.X, room.Y
		if up.X != nil {
			x = *up.X
		}
		if up.Y != nil {
			y = *up.Y
		}
		if x < 0 || y < 0 {
			return nil, fmt.Errorf("%w: the grid re-bases to zero; a room cannot sit at (%d,%d)", ErrInvalid, x, y)
		}
		for i, other := range d.Rooms {
			if i == at {
				continue
			}
			if other.X == x && other.Y == y {
				return nil, fmt.Errorf("%w: cell (%d,%d) already holds %s", ErrInvalid, x, y, roomLabel(other))
			}
		}
		room.X, room.Y = x, y
	}

	sets := []string{"updated_at = ?"}
	args := []any{time.Now().UTC().UnixMilli()}
	if up.Name != nil {
		sets = append(sets, "name = ?")
		args = append(args, strings.TrimSpace(*up.Name))
	}
	if up.Detail != nil {
		sets = append(sets, "detail = ?")
		args = append(args, strings.TrimSpace(*up.Detail))
	}
	if up.X != nil || up.Y != nil {
		sets = append(sets, "x = ?", "y = ?")
		args = append(args, room.X, room.Y)
	}
	if up.EncounterID != nil {
		sets = append(sets, "encounter_id = ?")
		args = append(args, strings.TrimSpace(*up.EncounterID))
	}
	args = append(args, roomID, dungeonID)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE dungeon_rooms SET `+strings.Join(sets, ", ")+` WHERE id = ? AND dungeon_id = ?`, args...); err != nil {
		return nil, fmt.Errorf("update dungeon room: %w", err)
	}
	return s.GetDungeon(ctx, ScopeDM, campaignID, dungeonID)
}

func roomLabel(r DungeonRoom) string {
	if r.Name != "" {
		return r.Name
	}
	return r.Key + " (" + r.Purpose + ")"
}

/* ---------- edge edits: adding and cutting connections ---------- */

// DungeonEdgeInput is one new connection: the two room keys, the kind,
// whether it is one-way, and — for a locked door — the key item entity
// that opens it.
type DungeonEdgeInput struct {
	FromRoom      string
	ToRoom        string
	Kind          string
	OneWay        bool
	KeyItemEntity string
}

// AddDungeonEdge writes one connection between two rooms of the dungeon.
// Both ends must be declared rooms; a duplicate either direction is the
// same door twice and is refused; a room cannot connect to itself — a
// corridor that loops back into its own room is a room, not an edge.
func (s *Store) AddDungeonEdge(ctx context.Context, campaignID, dungeonID string, in DungeonEdgeInput) (*Dungeon, error) {
	d, err := s.GetDungeon(ctx, ScopeDM, campaignID, dungeonID)
	if err != nil {
		return nil, err
	}
	if !dungeon.ValidEdgeKind(in.Kind) {
		return nil, fmt.Errorf("%w: edge kind %q is not one of %s", ErrInvalid, in.Kind, strings.Join(dungeon.EdgeKinds(), ", "))
	}
	keys := map[string]bool{}
	for _, r := range d.Rooms {
		keys[r.Key] = true
	}
	if !keys[in.FromRoom] || !keys[in.ToRoom] {
		return nil, fmt.Errorf("%w: an edge must name two declared rooms (%q, %q)", ErrInvalid, in.FromRoom, in.ToRoom)
	}
	if in.FromRoom == in.ToRoom {
		return nil, fmt.Errorf("%w: a room cannot connect to itself", ErrInvalid)
	}
	for _, e := range d.Edges {
		if (e.FromRoom == in.FromRoom && e.ToRoom == in.ToRoom) || (e.FromRoom == in.ToRoom && e.ToRoom == in.FromRoom) {
			return nil, fmt.Errorf("%w: %s and %s are already connected", ErrAlreadyExists, in.FromRoom, in.ToRoom)
		}
	}
	var keyItem any
	if id := strings.TrimSpace(in.KeyItemEntity); id != "" {
		var one int
		err := s.db.QueryRowContext(ctx,
			`SELECT 1 FROM entities WHERE id = ? AND campaign_id = ?`, id, campaignID).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: key item entity %s", ErrNotFound, id)
		}
		if err != nil {
			return nil, fmt.Errorf("check key item: %w", err)
		}
		keyItem = id
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO dungeon_edges (id, dungeon_id, from_room, to_room, kind, key_item_entity, one_way, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		uuid.NewString(), dungeonID, in.FromRoom, in.ToRoom, in.Kind, keyItem,
		boolInt(in.OneWay), time.Now().UTC().UnixMilli()); err != nil {
		return nil, fmt.Errorf("insert dungeon edge: %w", err)
	}
	return s.GetDungeon(ctx, ScopeDM, campaignID, dungeonID)
}

// RemoveDungeonEdge cuts one connection by edge id. The critical path is
// the DM's to judge — an edit that strands rooms is a map the DM drew,
// not a generator output; the dressing and placement passes work on what
// is stored.
func (s *Store) RemoveDungeonEdge(ctx context.Context, campaignID, dungeonID, edgeID string) (*Dungeon, error) {
	d, err := s.GetDungeon(ctx, ScopeDM, campaignID, dungeonID)
	if err != nil {
		return nil, err
	}
	for _, e := range d.Edges {
		if e.ID != edgeID {
			continue
		}
		res, err := s.db.ExecContext(ctx,
			`DELETE FROM dungeon_edges WHERE id = ? AND dungeon_id = ?`, edgeID, dungeonID)
		if err != nil {
			return nil, fmt.Errorf("delete dungeon edge: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil, fmt.Errorf("%w: edge %s", ErrNotFound, edgeID)
		}
		return s.GetDungeon(ctx, ScopeDM, campaignID, dungeonID)
	}
	return nil, fmt.Errorf("%w: edge %s of dungeon %s", ErrNotFound, edgeID, dungeonID)
}

/* ---------- the dressing and placement writes ---------- */

// Dressing is one room's dressing: the name and the detail the model (or
// the DM) wrote.
type Dressing struct {
	Name   string
	Detail string
}

// DressDungeonRooms writes the dressing pass's result: names, detail, the
// key item's name for the boss's locked door, the dungeon's secret
// statement, the boss's own name, and the status flip to dressed. Called
// by internal/canon's generator; the topology is already validated
// against the stored graph by the caller. The key item itself is an
// entity the placing batch stages; its id lands on the locked edge when
// that batch is decided.
func (s *Store) DressDungeonRooms(ctx context.Context, campaignID, dungeonID string, rooms map[string]Dressing, keyItem, secret, bossName string) error {
	d, err := s.GetDungeon(ctx, ScopeDM, campaignID, dungeonID)
	if err != nil {
		return err
	}
	if d.Status == DungeonPlaced {
		return fmt.Errorf("%w: %q is placed; its design is not re-dressed", ErrInvalid, d.Name)
	}
	now := time.Now().UTC().UnixMilli()
	for _, r := range d.Rooms {
		dr, ok := rooms[r.Key]
		if !ok {
			continue
		}
		if _, err := s.db.ExecContext(ctx, `
			UPDATE dungeon_rooms SET name = ?, detail = ?, updated_at = ? WHERE id = ? AND dungeon_id = ?`,
			dr.Name, dr.Detail, now, r.ID, dungeonID); err != nil {
			return fmt.Errorf("dress room %s: %w", r.Key, err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE dungeons SET status = ?, key_item = ?, secret = ?, boss_name = ?, updated_at = ? WHERE id = ? AND campaign_id = ?`,
		DungeonDressed, strings.TrimSpace(keyItem), strings.TrimSpace(secret), strings.TrimSpace(bossName), now, dungeonID, campaignID); err != nil {
		return fmt.Errorf("dress dungeon: %w", err)
	}
	return nil
}

// MarkDungeonPlaced records a decided placement batch: the dungeon's
// location entity, each room's entity, the key item's entity on the
// locked edge, and the status flip. The mapping is room key -> entity
// id. Called by the canon store's batch finalizer; partial is refused —
// a half-placed dungeon is worse than unplaced.
func (s *Store) MarkDungeonPlaced(ctx context.Context, campaignID, dungeonID, locationEntity, keyItemEntity string, roomEntities map[string]string) error {
	d, err := s.GetDungeon(ctx, ScopeDM, campaignID, dungeonID)
	if err != nil {
		return err
	}
	if d.Status == DungeonPlaced {
		return nil // idempotent: a re-run finalize heals nothing further
	}
	if locationEntity == "" {
		return fmt.Errorf("%w: a placement needs the dungeon's location entity", ErrInvalid)
	}
	for _, r := range d.Rooms {
		if roomEntities[r.Key] == "" {
			return fmt.Errorf("%w: room %s has no placed entity; the placement is partial", ErrInvalid, r.Key)
		}
	}
	now := time.Now().UTC().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("place tx: %w", err)
	}
	defer tx.Rollback()
	for key, entity := range roomEntities {
		if _, err := tx.ExecContext(ctx, `
			UPDATE dungeon_rooms SET entity_id = ?, updated_at = ? WHERE dungeon_id = ? AND key = ?`,
			entity, now, dungeonID, key); err != nil {
			return fmt.Errorf("place room %s: %w", key, err)
		}
	}
	if keyItemEntity != "" {
		if _, err := tx.ExecContext(ctx, `
			UPDATE dungeon_edges SET key_item_entity = ? WHERE dungeon_id = ? AND kind = 'locked_door'`,
			keyItemEntity, dungeonID); err != nil {
			return fmt.Errorf("place key item: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE dungeons SET location_entity = ?, status = ?, updated_at = ? WHERE id = ? AND campaign_id = ?`,
		locationEntity, DungeonPlaced, now, dungeonID, campaignID); err != nil {
		return fmt.Errorf("place dungeon: %w", err)
	}
	return tx.Commit()
}
