// Package campaign is the persistent world model every other campaign feature
// consumes: campaigns, members, entities, facts, provenance, contradictions,
// events, quests, and the relationships between them.
//
// The schema is owned by the migrations (internal/migrate, 0002 and up); this
// package creates no tables. It is the first package built that way and sets
// the pattern for the rest — a half-applied schema is a migrate failure, not
// something New() papers over.
//
// Two invariants live here and nowhere else:
//
//   - every fact carries at least one provenance row (CreateFact requires one
//     and the fact_without_provenance integrity check catches anything that
//     bypassed the API), and
//   - nothing deletes a fact. Superseding retcons it in place, keeping the
//     campaign's history of its own retcons.
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

// Errors callers are expected to branch on.
var (
	// ErrNotFound is returned when a campaign-scoped row does not exist for
	// the caller. Cross-campaign ids are indistinguishable from missing
	// ones: a 404 either way.
	ErrNotFound = errors.New("not found")
	// ErrInvalid marks input that violates a vocabulary or shape constraint.
	// The message carries the specifics.
	ErrInvalid = errors.New("invalid input")
	// ErrAlreadyExists marks a duplicate the caller should treat as a
	// conflict rather than an error screen.
	ErrAlreadyExists = errors.New("already exists")
)

/* ---------- vocabularies ---------- */

// Entity kinds: the typed nodes of the graph.
const (
	KindPC           = "pc"
	KindNPC          = "npc"
	KindFaction      = "faction"
	KindLocation     = "location"
	KindItem         = "item"
	KindDeity        = "deity"
	KindOrganization = "organization"
	KindCreature     = "creature"
	KindConcept      = "concept"
)

// EntityNameKind distinguishes canonical names from aliases and epithets.
const (
	NameCanonical = "canonical"
	NameAlias     = "alias"
	NameEpithet   = "epithet"
)

// Entity status. 'deleted' is the soft delete: the graph keeps pointing at the
// row (nothing is ever deleted), and the dangling_reference integrity check
// flags whatever still references it.
const (
	StatusActive    = "active"
	StatusInactive  = "inactive"
	StatusDead      = "dead"
	StatusDestroyed = "destroyed"
	StatusMissing   = "missing"
	StatusDeleted   = "deleted"
)

// Member roles. This is the whole player-identity story: a player is an
// ordinary auth user narrowed by a membership row. No row, no access.
const (
	RoleDM       = "dm"
	RolePlayer   = "player"
	RoleObserver = "observer"
)

// Fact confidence, ordered from least to most authoritative. The monotonicity
// rule (docs/campaign/epistemics.md): a machine pass may only downgrade or
// flag — never upgrade, never delete.
const (
	ConfidenceProposed  = "proposed"
	ConfidenceContested = "contested"
	ConfidenceDerived   = "derived"
	ConfidenceCanon     = "canon"
	ConfidenceRetconned = "retconned"
)

// confidenceRank orders the confidence vocabulary for downgrade checks.
// 'retconned' sits off the ladder: it is terminal and never transitions by
// machine.
var confidenceRank = map[string]int{
	ConfidenceProposed:  0,
	ConfidenceContested: 1,
	ConfidenceDerived:   2,
	ConfidenceCanon:     3,
	ConfidenceRetconned: -1,
}

// Fact visibility.
const (
	VisibilityPublic = "public"
	VisibilitySecret = "secret"
)

// Provenance methods.
const (
	MethodDMAuthored = "dm_authored"
	MethodAIProposed = "ai_proposed"
	MethodExtracted  = "extracted"
	MethodImported   = "imported"
)

// Event link kinds.
const (
	LinkCaused   = "caused"
	LinkEnabled  = "enabled"
	LinkRevealed = "revealed"
)

// Contradiction status.
const (
	ContradictionOpen             = "open"
	ContradictionResolvedByReview = "resolved_by_review"
)

// validEntityKinds is the set the CHECK constraint on entities enforces;
// mirrored here so callers get a clean error instead of a constraint traceback.
var validEntityKinds = map[string]bool{
	KindPC: true, KindNPC: true, KindFaction: true, KindLocation: true,
	KindItem: true, KindDeity: true, KindOrganization: true, KindCreature: true,
	KindConcept: true,
}

var validEntityStatuses = map[string]bool{
	StatusActive: true, StatusInactive: true, StatusDead: true,
	StatusDestroyed: true, StatusMissing: true, StatusDeleted: true,
}

var validNameKinds = map[string]bool{
	NameCanonical: true, NameAlias: true, NameEpithet: true,
}

var validRoles = map[string]bool{
	RoleDM: true, RolePlayer: true, RoleObserver: true,
}

var validConfidence = map[string]bool{
	ConfidenceProposed: true, ConfidenceCanon: true, ConfidenceDerived: true,
	ConfidenceContested: true, ConfidenceRetconned: true,
}

var validVisibility = map[string]bool{
	VisibilityPublic: true, VisibilitySecret: true,
}

var validMethods = map[string]bool{
	MethodDMAuthored: true, MethodAIProposed: true, MethodExtracted: true,
	MethodImported: true,
}

var validLinks = map[string]bool{
	LinkCaused: true, LinkEnabled: true, LinkRevealed: true,
}

/* ---------- the store ---------- */

// Store reads and writes the campaign graph on the shared database handle.
// The schema must already be applied (migrate.Up runs before anything serves).
type Store struct {
	db *sql.DB
}

// New builds a campaign store on an open, migrated database handle.
func New(db *sql.DB) (*Store, error) {
	if db == nil {
		return nil, errors.New("campaign: nil database handle")
	}
	return &Store{db: db}, nil
}

// DB exposes the underlying handle for the CLI subcommand and later stages.
func (s *Store) DB() *sql.DB { return s.db }

/* ---------- campaigns ---------- */

// Campaign is one campaign: owner, rules system, premise, the in-world clock,
// and a free-form settings payload.
type Campaign struct {
	ID      string
	OwnerID string
	Name    string
	System  string
	Premise string
	// Clock is the in-world day the campaign currently sits at. Day 0 is the
	// campaign's start by convention; events carry their own clock_at against
	// the same axis.
	Clock     int64
	Settings  map[string]any
	CreatedAt time.Time
	UpdatedAt time.Time
}

// CreateCampaign records a new campaign and makes the owner its DM member in
// one transaction, so membership is the single access gate from the first
// row: default deny is a missing row, and the owner never lacks one.
func (s *Store) CreateCampaign(ctx context.Context, ownerID, name, system, premise string) (*Campaign, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("%w: campaign name is required", ErrInvalid)
	}
	if ownerID == "" {
		return nil, fmt.Errorf("%w: campaign owner is required", ErrInvalid)
	}
	if err := s.userExists(ctx, ownerID); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	c := &Campaign{
		ID: uuid.NewString(), OwnerID: ownerID, Name: name,
		System: strings.TrimSpace(system), Premise: premise,
		Clock: 0, Settings: map[string]any{},
		CreatedAt: now, UpdatedAt: now,
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("campaign tx: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO campaigns (id, owner_id, name, system, premise, clock, settings, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, '{}', ?, ?)`,
		c.ID, c.OwnerID, c.Name, c.System, c.Premise, c.Clock, now.UnixMilli(), now.UnixMilli()); err != nil {
		return nil, fmt.Errorf("insert campaign: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO campaign_members (campaign_id, user_id, role, joined_at) VALUES (?, ?, ?, ?)`,
		c.ID, ownerID, RoleDM, now.UnixMilli()); err != nil {
		return nil, fmt.Errorf("insert owner membership: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("campaign commit: %w", err)
	}
	return c, nil
}

const campaignCols = `id, owner_id, name, system, premise, clock, settings, created_at, updated_at`

func scanCampaign(row interface{ Scan(...any) error }) (*Campaign, error) {
	var (
		c            Campaign
		settingsJSON string
		createdMilli int64
		updatedMilli int64
	)
	if err := row.Scan(&c.ID, &c.OwnerID, &c.Name, &c.System, &c.Premise, &c.Clock,
		&settingsJSON, &createdMilli, &updatedMilli); err != nil {
		return nil, err
	}
	c.Settings = map[string]any{}
	if settingsJSON != "" {
		_ = json.Unmarshal([]byte(settingsJSON), &c.Settings)
	}
	c.CreatedAt = time.UnixMilli(createdMilli).UTC()
	c.UpdatedAt = time.UnixMilli(updatedMilli).UTC()
	return &c, nil
}

// GetCampaign returns one campaign. Campaigns are not secret between members
// at this layer; the caller enforces membership (Role) before showing
// anything. A missing id returns ErrNotFound.
func (s *Store) GetCampaign(ctx context.Context, id string) (*Campaign, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+campaignCols+` FROM campaigns WHERE id = ?`, id)
	c, err := scanCampaign(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: campaign %s", ErrNotFound, id)
	}
	return c, err
}

// ListCampaigns returns every campaign the user can see: their own plus any
// they are a member of, most recently updated first.
func (s *Store) ListCampaigns(ctx context.Context, userID string) ([]Campaign, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+campaignCols+` FROM campaigns c
		WHERE c.owner_id = ?
		   OR EXISTS (SELECT 1 FROM campaign_members m WHERE m.campaign_id = c.id AND m.user_id = ?)
		ORDER BY c.updated_at DESC`, userID, userID)
	if err != nil {
		return nil, fmt.Errorf("list campaigns: %w", err)
	}
	defer rows.Close()
	var out []Campaign
	for rows.Next() {
		c, err := scanCampaign(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// UpdateCampaign replaces the mutable fields of a campaign owned by ownerID.
// nil arguments leave the corresponding field alone; a nil settings map leaves
// settings alone rather than clearing them.
//
// The clock is a ledger, not a settable integer: a PATCH that changes it
// records a 'manual' clock_advances row in the same transaction and sets
// campaigns.clock to the new head. A backwards move is legal (a DM fixing a
// typo) and recorded exactly like a forward one.
func (s *Store) UpdateCampaign(ctx context.Context, ownerID, id string, name, system, premise *string, clockSet *int64, settings map[string]any) (*Campaign, error) {
	c, err := s.GetCampaign(ctx, id)
	if err != nil {
		return nil, err
	}
	if c.OwnerID != ownerID {
		return nil, fmt.Errorf("%w: campaign %s", ErrNotFound, id)
	}
	if name != nil {
		c.Name = strings.TrimSpace(*name)
		if c.Name == "" {
			return nil, fmt.Errorf("%w: campaign name is required", ErrInvalid)
		}
	}
	if system != nil {
		c.System = strings.TrimSpace(*system)
	}
	if premise != nil {
		c.Premise = *premise
	}
	priorClock := c.Clock
	if clockSet != nil {
		c.Clock = *clockSet
	}
	settingsJSON := ""
	if settings != nil {
		c.Settings = settings
	}
	if c.Settings == nil {
		c.Settings = map[string]any{}
	}
	b, err := json.Marshal(c.Settings)
	if err != nil {
		return nil, fmt.Errorf("encode settings: %w", err)
	}
	settingsJSON = string(b)
	c.UpdatedAt = time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("campaign tx: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `
		UPDATE campaigns SET name = ?, system = ?, premise = ?, clock = ?, settings = ?, updated_at = ?
		 WHERE id = ? AND owner_id = ?`,
		c.Name, c.System, c.Premise, c.Clock, settingsJSON, c.UpdatedAt.UnixMilli(), id, ownerID)
	if err != nil {
		return nil, fmt.Errorf("update campaign: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, fmt.Errorf("%w: campaign %s", ErrNotFound, id)
	}
	if clockSet != nil && *clockSet != priorClock {
		// The ledger row: what moved, from where, to where, by whom.
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO clock_advances (id, campaign_id, from_day, to_day, reason, note, session_id, created_by, created_at)
			VALUES (?, ?, ?, ?, 'manual', 'campaign PATCH', NULL, ?, ?)`,
			uuid.NewString(), id, priorClock, *clockSet, ownerID, c.UpdatedAt.UnixMilli()); err != nil {
			return nil, fmt.Errorf("record manual advance: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("campaign commit: %w", err)
	}
	return c, nil
}

// DeleteCampaign removes a campaign owned by ownerID and everything attached
// to it. Campaign deletion cascades; fact history is not preserved across a
// campaign the owner deliberately removed.
func (s *Store) DeleteCampaign(ctx context.Context, ownerID, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM campaigns WHERE id = ? AND owner_id = ?`, id, ownerID)
	if err != nil {
		return fmt.Errorf("delete campaign: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: campaign %s", ErrNotFound, id)
	}
	return nil
}

/* ---------- members ---------- */

// Member is one user's standing in one campaign.
type Member struct {
	CampaignID  string
	UserID      string
	Role        string
	CharacterID string // the pc entity that makes a character:<id> perspective resolvable; empty when unset
	JoinedAt    time.Time
}

// AddMember gives a user a role in a campaign. Default deny is a missing row:
// this function is the only thing that mints them ahead of the invite flow
// (MAD-305). A second row for the same user is ErrAlreadyExists.
func (s *Store) AddMember(ctx context.Context, campaignID, userID, role, characterID string) error {
	return s.addMember(ctx, s.db, campaignID, userID, role, characterID)
}

// AddMemberTx is AddMember inside a caller-owned transaction, so the invite
// redeem path (internal/auth) can write the membership row in the same commit
// as the account it belongs to.
func (s *Store) AddMemberTx(ctx context.Context, tx *sql.Tx, campaignID, userID, role, characterID string) error {
	return s.addMember(ctx, tx, campaignID, userID, role, characterID)
}

// dbRunner is the subset the member insert needs; *sql.DB and *sql.Tx both
// satisfy it.
type dbRunner interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func (s *Store) addMember(ctx context.Context, q dbRunner, campaignID, userID, role, characterID string) error {
	if !validRoles[role] {
		return fmt.Errorf("%w: role %q", ErrInvalid, role)
	}
	if err := s.campaignExistsOn(ctx, q, campaignID); err != nil {
		return err
	}
	if err := s.userExistsOn(ctx, q, userID); err != nil {
		return err
	}
	if characterID != "" {
		if _, err := s.entityInCampaignOn(ctx, q, characterID, campaignID); err != nil {
			return err
		}
	}
	now := time.Now().UTC()
	_, err := q.ExecContext(ctx, `
		INSERT INTO campaign_members (campaign_id, user_id, role, character_id, joined_at)
		VALUES (?, ?, ?, ?, ?)`,
		campaignID, userID, role, nullString(characterID), now.UnixMilli())
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return fmt.Errorf("%w: member %s already in campaign %s", ErrAlreadyExists, userID, campaignID)
		}
		return fmt.Errorf("insert member: %w", err)
	}
	return nil
}

// RemoveMember drops a user's membership. Removing the owner's own row is
// allowed only by leaving the campaign ownerless-but-listed; the owner_id on
// the campaign is provenance, membership is access. Removing an unknown
// member is not an error.
func (s *Store) RemoveMember(ctx context.Context, campaignID, userID string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM campaign_members WHERE campaign_id = ? AND user_id = ?`, campaignID, userID); err != nil {
		return fmt.Errorf("remove member: %w", err)
	}
	return nil
}

// SetMemberRole changes a member's role. Only the campaign's DM (or the
// keeper) reaches this — the store trusts the caller's gate.
func (s *Store) SetMemberRole(ctx context.Context, campaignID, userID, role string) error {
	if !validRoles[role] {
		return fmt.Errorf("%w: role %q", ErrInvalid, role)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE campaign_members SET role = ? WHERE campaign_id = ? AND user_id = ?`,
		role, campaignID, userID)
	if err != nil {
		return fmt.Errorf("set member role: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: member %s in campaign %s", ErrNotFound, userID, campaignID)
	}
	return nil
}

// SetMemberCharacter points a member at the pc entity they play. The entity
// must belong to the same campaign.
func (s *Store) SetMemberCharacter(ctx context.Context, campaignID, userID, characterID string) error {
	if characterID != "" {
		if _, err := s.entityInCampaign(ctx, characterID, campaignID); err != nil {
			return err
		}
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE campaign_members SET character_id = ? WHERE campaign_id = ? AND user_id = ?`,
		nullString(characterID), campaignID, userID)
	if err != nil {
		return fmt.Errorf("set member character: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: member %s in campaign %s", ErrNotFound, userID, campaignID)
	}
	return nil
}

// Members lists a campaign's members, joiners first.
func (s *Store) Members(ctx context.Context, campaignID string) ([]Member, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT campaign_id, user_id, role, character_id, joined_at
		  FROM campaign_members WHERE campaign_id = ? ORDER BY joined_at, user_id`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("list members: %w", err)
	}
	defer rows.Close()
	var out []Member
	for rows.Next() {
		var (
			m         Member
			character sql.NullString
			joined    int64
		)
		if err := rows.Scan(&m.CampaignID, &m.UserID, &m.Role, &character, &joined); err != nil {
			return nil, err
		}
		m.CharacterID = character.String
		m.JoinedAt = time.UnixMilli(joined).UTC()
		out = append(out, m)
	}
	return out, rows.Err()
}

// Role reports the user's role in the campaign. The second return is false
// when there is no membership row — which is the answer "no access", not an
// error. Callers gate on this, not on a route.
func (s *Store) Role(ctx context.Context, campaignID, userID string) (string, bool, error) {
	var role string
	err := s.db.QueryRowContext(ctx,
		`SELECT role FROM campaign_members WHERE campaign_id = ? AND user_id = ?`, campaignID, userID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("member role: %w", err)
	}
	return role, true, nil
}

/* ---------- shared helpers ---------- */

// nullString maps "" to SQL NULL.
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// userExists reports ErrNotFound when the users row is missing. Campaign
// ownership and membership foreign-key users, so validating here turns a
// constraint traceback into a clean error.
func (s *Store) userExists(ctx context.Context, id string) error {
	return s.userExistsOn(ctx, s.db, id)
}

// userExistsOn is userExists over a runner (*sql.DB or *sql.Tx).
func (s *Store) userExistsOn(ctx context.Context, q dbRunner, id string) error {
	var one int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM users WHERE id = ?`, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: user %s", ErrNotFound, id)
	}
	if err != nil {
		return fmt.Errorf("check user: %w", err)
	}
	return nil
}

// unixMilli converts a stored millisecond timestamp to UTC time.
func unixMilli(ms int64) time.Time { return time.UnixMilli(ms).UTC() }
