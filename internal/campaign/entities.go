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
)

// Entity is a typed node of the graph: pc, npc, faction, location, item,
// deity, organization, creature or concept. Payload is free-form JSON because
// a location and a deity have almost nothing in common, and a wide table of
// mostly-NULL columns would be worse than honest JSON.
type Entity struct {
	ID         string
	CampaignID string
	Kind       string
	Name       string
	Summary    string
	Payload    map[string]any
	Status     string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// EntityName is one of an entity's many names: canonical, alias or epithet.
// "Tom the innkeeper" is also "Thomas Vane"; without this table extraction
// mints a second Tom.
type EntityName struct {
	ID        string
	EntityID  string
	Name      string
	Kind      string
	CreatedAt time.Time
}

// CreateEntity adds a typed node to a campaign. The canonical name is also
// recorded in entity_aliases so every lookup surface is one table.
func (s *Store) CreateEntity(ctx context.Context, campaignID, kind, name, summary string, payload map[string]any) (*Entity, error) {
	if !validEntityKinds[kind] {
		return nil, fmt.Errorf("%w: entity kind %q", ErrInvalid, kind)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("%w: entity name is required", ErrInvalid)
	}
	if payload == nil {
		payload = map[string]any{}
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("%w: entity payload: %v", ErrInvalid, err)
	}
	if err := s.campaignExists(ctx, campaignID); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	e := &Entity{
		ID: uuid.NewString(), CampaignID: campaignID, Kind: kind, Name: name,
		Summary: summary, Payload: payload, Status: StatusActive,
		CreatedAt: now, UpdatedAt: now,
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("entity tx: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO entities (id, campaign_id, kind, name, summary, payload, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.CampaignID, e.Kind, e.Name, e.Summary, string(payloadJSON), e.Status,
		now.UnixMilli(), now.UnixMilli()); err != nil {
		return nil, fmt.Errorf("insert entity: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO entity_aliases (id, entity_id, name, kind, created_at) VALUES (?, ?, ?, ?, ?)`,
		uuid.NewString(), e.ID, e.Name, NameCanonical, now.UnixMilli()); err != nil {
		return nil, fmt.Errorf("insert canonical name: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("entity commit: %w", err)
	}
	return e, nil
}

const entityCols = `id, campaign_id, kind, name, summary, payload, status, created_at, updated_at`

func scanEntity(row interface{ Scan(...any) error }) (*Entity, error) {
	var (
		e            Entity
		payloadJSON  string
		createdMilli int64
		updatedMilli int64
	)
	if err := row.Scan(&e.ID, &e.CampaignID, &e.Kind, &e.Name, &e.Summary, &payloadJSON,
		&e.Status, &createdMilli, &updatedMilli); err != nil {
		return nil, err
	}
	e.Payload = map[string]any{}
	if payloadJSON != "" {
		_ = json.Unmarshal([]byte(payloadJSON), &e.Payload)
	}
	e.CreatedAt = time.UnixMilli(createdMilli).UTC()
	e.UpdatedAt = time.UnixMilli(updatedMilli).UTC()
	return &e, nil
}

// entityInCampaign loads an entity and checks it belongs to campaignID.
// Foreign ids read as ErrNotFound, the same as missing ones.
func (s *Store) entityInCampaign(ctx context.Context, id, campaignID string) (*Entity, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+entityCols+` FROM entities WHERE id = ? AND campaign_id = ?`, id, campaignID)
	e, err := scanEntity(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: entity %s", ErrNotFound, id)
	}
	return e, err
}

// GetEntity returns one entity. The campaign scoping makes a foreign id
// indistinguishable from a missing one.
func (s *Store) GetEntity(ctx context.Context, campaignID, id string) (*Entity, error) {
	return s.entityInCampaign(ctx, id, campaignID)
}

// ListEntities returns a campaign's entities, optionally filtered by kind,
// ordered by name so browser UIs get a stable list for free.
func (s *Store) ListEntities(ctx context.Context, campaignID, kind string) ([]Entity, error) {
	q := `SELECT ` + entityCols + ` FROM entities WHERE campaign_id = ?`
	args := []any{campaignID}
	if kind != "" {
		if !validEntityKinds[kind] {
			return nil, fmt.Errorf("%w: entity kind %q", ErrInvalid, kind)
		}
		q += ` AND kind = ?`
		args = append(args, kind)
	}
	q += ` ORDER BY name COLLATE NOCASE, id`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list entities: %w", err)
	}
	defer rows.Close()
	var out []Entity
	for rows.Next() {
		e, err := scanEntity(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

// UpdateEntity replaces the mutable fields of an entity. nil arguments leave
// the corresponding field alone. Renaming updates the canonical entity_aliases
// row in the same transaction, so the name tables never disagree.
func (s *Store) UpdateEntity(ctx context.Context, campaignID, id string, name, summary, status *string, payload map[string]any) (*Entity, error) {
	e, err := s.entityInCampaign(ctx, id, campaignID)
	if err != nil {
		return nil, err
	}
	if status != nil {
		if !validEntityStatuses[*status] {
			return nil, fmt.Errorf("%w: entity status %q", ErrInvalid, *status)
		}
		e.Status = *status
	}
	renamed := false
	if name != nil {
		e.Name = strings.TrimSpace(*name)
		if e.Name == "" {
			return nil, fmt.Errorf("%w: entity name is required", ErrInvalid)
		}
		renamed = true
	}
	if summary != nil {
		e.Summary = *summary
	}
	if payload != nil {
		e.Payload = payload
	}
	payloadJSON, err := json.Marshal(e.Payload)
	if err != nil {
		return nil, fmt.Errorf("%w: entity payload: %v", ErrInvalid, err)
	}
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("entity tx: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		UPDATE entities SET name = ?, summary = ?, payload = ?, status = ?, updated_at = ?
		 WHERE id = ? AND campaign_id = ?`,
		e.Name, e.Summary, string(payloadJSON), e.Status, now.UnixMilli(), id, campaignID); err != nil {
		return nil, fmt.Errorf("update entity: %w", err)
	}
	if renamed {
		if _, err := tx.ExecContext(ctx, `
			UPDATE entity_aliases SET name = ? WHERE entity_id = ? AND kind = 'canonical'`,
			e.Name, id); err != nil {
			return nil, fmt.Errorf("update canonical name: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("entity commit: %w", err)
	}
	e.UpdatedAt = now
	return e, nil
}

// DeleteEntity soft-deletes: status becomes 'deleted' and the row stays. The
// graph keeps its edges and facts, and the dangling_reference integrity check
// surfaces whatever still points here so the DM can decide what to retcon.
// Nothing in the campaign graph is ever a hard delete.
func (s *Store) DeleteEntity(ctx context.Context, campaignID, id string) error {
	status := StatusDeleted
	_, err := s.UpdateEntity(ctx, campaignID, id, nil, nil, &status, nil)
	return err
}

/* ---------- names ---------- */

// AddEntityName records an alias or epithet for an entity. Canonical names
// are managed by Create/UpdateEntity; this rejects them so there is exactly
// one canonical row per entity.
func (s *Store) AddEntityName(ctx context.Context, campaignID, entityID, name, kind string) (*EntityName, error) {
	if kind == NameCanonical {
		return nil, fmt.Errorf("%w: canonical names are set by creating or renaming the entity", ErrInvalid)
	}
	if !validNameKinds[kind] {
		return nil, fmt.Errorf("%w: name kind %q", ErrInvalid, kind)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("%w: name is required", ErrInvalid)
	}
	if _, err := s.entityInCampaign(ctx, entityID, campaignID); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	en := &EntityName{ID: uuid.NewString(), EntityID: entityID, Name: name, Kind: kind, CreatedAt: now}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO entity_aliases (id, entity_id, name, kind, created_at) VALUES (?, ?, ?, ?, ?)`,
		en.ID, en.EntityID, en.Name, en.Kind, now.UnixMilli())
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return nil, fmt.Errorf("%w: name %q on entity %s", ErrAlreadyExists, name, entityID)
		}
		return nil, fmt.Errorf("insert name: %w", err)
	}
	return en, nil
}

// EntityNames lists every name an entity carries, canonical first.
func (s *Store) EntityNames(ctx context.Context, campaignID, entityID string) ([]EntityName, error) {
	if _, err := s.entityInCampaign(ctx, entityID, campaignID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, entity_id, name, kind, created_at FROM entity_aliases
		 WHERE entity_id = ? ORDER BY kind, name COLLATE NOCASE`, entityID)
	if err != nil {
		return nil, fmt.Errorf("list names: %w", err)
	}
	defer rows.Close()
	var out []EntityName
	for rows.Next() {
		var (
			en      EntityName
			created int64
		)
		if err := rows.Scan(&en.ID, &en.EntityID, &en.Name, &en.Kind, &created); err != nil {
			return nil, err
		}
		en.CreatedAt = time.UnixMilli(created).UTC()
		out = append(out, en)
	}
	return out, rows.Err()
}

// ResolveName finds entities in a campaign carrying a name (canonical, alias
// or epithet), matched case-insensitively. More than one hit is the
// entity_merge_candidate problem surfacing at lookup time; the caller decides.
func (s *Store) ResolveName(ctx context.Context, campaignID, name string) ([]Entity, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("%w: name is required", ErrInvalid)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+entityCols+` FROM entities e
		 WHERE e.campaign_id = ?
		   AND EXISTS (SELECT 1 FROM entity_aliases n
		                WHERE n.entity_id = e.id AND n.name = ? COLLATE NOCASE)
		 ORDER BY e.name COLLATE NOCASE`, campaignID, name)
	if err != nil {
		return nil, fmt.Errorf("resolve name: %w", err)
	}
	defer rows.Close()
	var out []Entity
	for rows.Next() {
		e, err := scanEntity(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

/* ---------- relationships ---------- */

// Relationship is a typed edge from a controlled vocabulary, optionally
// justified by a fact (which carries provenance) and dated by an event.
type Relationship struct {
	ID              string
	FromEntity      string
	RelType         string
	ToEntity        string
	Strength        int64
	JustifiedByFact string
	SinceEvent      string
	CreatedAt       time.Time
}

// CreateRelationship adds a typed edge between two entities of the same
// campaign. The type must exist in the relationship_types vocabulary — the
// database enforces it too, but validating here returns a clean error. An
// identical edge is ErrAlreadyExists rather than a silent duplicate.
func (s *Store) CreateRelationship(ctx context.Context, campaignID, from, relType, to string, strength int64, justifiedByFact, sinceEvent string) (*Relationship, error) {
	fromEntity, err := s.entityInCampaign(ctx, from, campaignID)
	if err != nil {
		return nil, err
	}
	if fromEntity.Status == StatusDeleted {
		return nil, fmt.Errorf("%w: entity %s is deleted", ErrInvalid, from)
	}
	toEntity, err := s.entityInCampaign(ctx, to, campaignID)
	if err != nil {
		return nil, err
	}
	if toEntity.Status == StatusDeleted {
		return nil, fmt.Errorf("%w: entity %s is deleted", ErrInvalid, to)
	}
	if from == to {
		return nil, fmt.Errorf("%w: an entity cannot relate to itself", ErrInvalid)
	}
	if strength < -100 || strength > 100 {
		return nil, fmt.Errorf("%w: strength %d outside -100..100", ErrInvalid, strength)
	}
	if justifiedByFact != "" {
		if err := s.factInCampaign(ctx, justifiedByFact, campaignID); err != nil {
			return nil, err
		}
	}
	if sinceEvent != "" {
		if err := s.eventInCampaign(ctx, sinceEvent, campaignID); err != nil {
			return nil, err
		}
	}
	var exists int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM relationship_types WHERE name = ?`, relType).Scan(&exists); err != nil {
		return nil, fmt.Errorf("check relationship type: %w", err)
	}
	if exists == 0 {
		return nil, fmt.Errorf("%w: relationship type %q is not in the vocabulary", ErrInvalid, relType)
	}
	now := time.Now().UTC()
	r := &Relationship{
		ID: uuid.NewString(), FromEntity: from, RelType: relType, ToEntity: to,
		Strength: strength, JustifiedByFact: justifiedByFact, SinceEvent: sinceEvent,
		CreatedAt: now,
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO relationships (id, from_entity, rel_type, to_entity, strength, justified_by_fact, since_event, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.FromEntity, r.RelType, r.ToEntity, r.Strength,
		nullString(r.JustifiedByFact), nullString(r.SinceEvent), now.UnixMilli())
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return nil, fmt.Errorf("%w: %s %s %s", ErrAlreadyExists, from, relType, to)
		}
		return nil, fmt.Errorf("insert relationship: %w", err)
	}
	return r, nil
}

const relationshipCols = `id, from_entity, rel_type, to_entity, strength, justified_by_fact, since_event, created_at`

func scanRelationship(row interface{ Scan(...any) error }) (*Relationship, error) {
	var (
		r            Relationship
		justified    sql.NullString
		since        sql.NullString
		createdMilli int64
	)
	if err := row.Scan(&r.ID, &r.FromEntity, &r.RelType, &r.ToEntity, &r.Strength,
		&justified, &since, &createdMilli); err != nil {
		return nil, err
	}
	r.JustifiedByFact = justified.String
	r.SinceEvent = since.String
	r.CreatedAt = time.UnixMilli(createdMilli).UTC()
	return &r, nil
}

// ListRelationships returns every edge in a campaign.
func (s *Store) ListRelationships(ctx context.Context, campaignID string) ([]Relationship, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+relationshipCols+` FROM relationships r
		 WHERE r.from_entity IN (SELECT id FROM entities WHERE campaign_id = ?)
		 ORDER BY r.created_at`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("list relationships: %w", err)
	}
	defer rows.Close()
	var out []Relationship
	for rows.Next() {
		r, err := scanRelationship(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// RelationshipsOf returns the edges touching one entity, both directions,
// outgoing first.
func (s *Store) RelationshipsOf(ctx context.Context, campaignID, entityID string) ([]Relationship, error) {
	if _, err := s.entityInCampaign(ctx, entityID, campaignID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+relationshipCols+` FROM relationships
		 WHERE from_entity = ? OR to_entity = ?
		 ORDER BY CASE WHEN from_entity = ? THEN 0 ELSE 1 END, created_at`,
		entityID, entityID, entityID)
	if err != nil {
		return nil, fmt.Errorf("relationships of: %w", err)
	}
	defer rows.Close()
	var out []Relationship
	for rows.Next() {
		r, err := scanRelationship(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// DeleteRelationship removes an edge. Edges are derived structure, not
// history: deleting one is how a DM corrects a bad edge, and the fact that
// justified it stays untouched.
func (s *Store) DeleteRelationship(ctx context.Context, campaignID, id string) error {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM relationships WHERE id = ?
		 AND from_entity IN (SELECT id FROM entities WHERE campaign_id = ?)`, id, campaignID)
	if err != nil {
		return fmt.Errorf("delete relationship: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: relationship %s", ErrNotFound, id)
	}
	return nil
}

// campaignExists reports ErrNotFound when the campaign row is missing.
func (s *Store) campaignExists(ctx context.Context, id string) error {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM campaigns WHERE id = ?`, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: campaign %s", ErrNotFound, id)
	}
	if err != nil {
		return fmt.Errorf("check campaign: %w", err)
	}
	return nil
}
