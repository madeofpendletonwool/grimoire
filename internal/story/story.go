// Package story is the narrative spine: acts, scenes, cast, secrets and
// outcomes, and the planned face of a session (MAD-360, stage 1 of MAD-314's
// generators).
//
// The campaign graph (internal/campaign) models what is true. This package
// models what is planned: which acts the campaign moves through, which scenes
// carry them, who is on stage, which secrets a scene puts in play, and what
// each session is for. Every stage-5 generator writes into this shape, so the
// shape exists — and is authorable by hand — before any of them is built.
//
// The engine half follows the shape internal/canon/engine.go sets: pure Go
// over a snapshot, every rule unit-testable, no database access inside a
// rule. Pace derives session counts from the XP tables internal/encounter
// already carries; Shape names the legal act structures; Validate checks a
// whole spine and emits campaign.Finding values, so its findings flow into
// the canon flag ledger and `grimoire canon check` rather than a second
// parallel findings system.
//
// The schema is owned by migration 0013; this package creates no tables.
package story

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/madeofpendletonwool/grimoire/internal/campaign"
)

// Errors. The spine reuses the campaign vocabulary, the same way
// internal/gamesession does: callers branch on one set of sentinels across
// every campaign package.
var (
	ErrNotFound = campaign.ErrNotFound
	ErrInvalid  = campaign.ErrInvalid
)

/* ---------- vocabularies ---------- */

// Act and scene status. The same three words for both: a thing is planned,
// it is being played (active), or it is done.
const (
	StatusPlanned = "planned"
	StatusActive  = "active"
	StatusDone    = "done"
)

// Scene kinds: the six things a scene can be. An encounter is a combat
// scene's roster — internal/encounter owns rosters.
const (
	KindSocial      = "social"
	KindExploration = "exploration"
	KindCombat      = "combat"
	KindRevelation  = "revelation"
	KindDowntime    = "downtime"
	KindTravel      = "travel"
)

// Cast roles.
const (
	RoleFocus     = "focus"
	RolePresent   = "present"
	RoleOffstage  = "offstage"
	RoleMentioned = "mentioned"
)

// Secret dispositions: how a scene engages a secret fact.
const (
	DispositionInPlay     = "in_play"
	DispositionRevealedIf = "revealed_if"
	DispositionWithheld   = "withheld"
)

// Session plan status: a plan is being written (planned), the prep is done
// (ready), or the session it was for has been played (done).
const (
	PlanStatusPlanned = "planned"
	PlanStatusReady   = "ready"
	PlanStatusDone    = "done"
)

var validStatus = map[string]bool{
	StatusPlanned: true, StatusActive: true, StatusDone: true,
}

var validKinds = map[string]bool{
	KindSocial: true, KindExploration: true, KindCombat: true,
	KindRevelation: true, KindDowntime: true, KindTravel: true,
}

var validCastRoles = map[string]bool{
	RoleFocus: true, RolePresent: true, RoleOffstage: true, RoleMentioned: true,
}

var validDispositions = map[string]bool{
	DispositionInPlay: true, DispositionRevealedIf: true, DispositionWithheld: true,
}

var validPlanStatus = map[string]bool{
	PlanStatusPlanned: true, PlanStatusReady: true, PlanStatusDone: true,
}

// SceneKinds lists the scene kinds in display order.
var SceneKinds = []string{
	KindSocial, KindExploration, KindCombat, KindRevelation, KindDowntime, KindTravel,
}

// CastRoles lists the cast roles in display order.
var CastRoles = []string{RoleFocus, RolePresent, RoleOffstage, RoleMentioned}

// Dispositions lists the secret dispositions in display order.
var Dispositions = []string{DispositionInPlay, DispositionRevealedIf, DispositionWithheld}

/* ---------- rows ---------- */

// Act is one movement of the campaign: a level band, a premise, a place in
// the order.
type Act struct {
	ID         string    `json:"id"`
	CampaignID string    `json:"campaign_id"`
	Ordinal    int64     `json:"ordinal"`
	Name       string    `json:"name"`
	Premise    string    `json:"premise"`
	LevelStart int       `json:"level_start"`
	LevelEnd   int       `json:"level_end"`
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// CastMember is one entity's place in one scene.
type CastMember struct {
	SceneID   string    `json:"scene_id"`
	EntityID  string    `json:"entity_id"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
}

// SceneSecret places one secret fact in one scene.
type SceneSecret struct {
	SceneID     string    `json:"scene_id"`
	FactID      string    `json:"fact_id"`
	Disposition string    `json:"disposition"`
	CreatedAt   time.Time `json:"created_at"`
}

// QuestTransition is the machine move an outcome names: which quest, along
// which edge. It is stored as JSON in scene_outcomes.quest_transition.
type QuestTransition struct {
	QuestID string `json:"quest"`
	From    string `json:"from"`
	To      string `json:"to"`
}

// SceneOutcome is one branch a scene can resolve along (A-D by convention;
// any non-empty label is legal and unique within its scene).
type SceneOutcome struct {
	ID              string           `json:"id"`
	SceneID         string           `json:"scene_id"`
	Label           string           `json:"label"`
	Summary         string           `json:"summary"`
	LeadsToScene    string           `json:"leads_to_scene,omitempty"`
	QuestTransition *QuestTransition `json:"quest_transition,omitempty"`
	CreatedAt       time.Time        `json:"created_at"`
}

// Scene is one beat inside an act.
type Scene struct {
	ID            string    `json:"id"`
	CampaignID    string    `json:"campaign_id"`
	ActID         string    `json:"act_id"`
	SessionID     string    `json:"session_id,omitempty"`
	Ordinal       int64     `json:"ordinal"`
	Kind          string    `json:"kind"`
	Name          string    `json:"name"`
	Purpose       string    `json:"purpose"`
	SettingEntity string    `json:"setting_entity,omitempty"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	// Cast, Secrets and Outcomes are attached by GetScene and the spine
	// loader; the flat list reads leave them empty.
	Cast     []CastMember   `json:"cast,omitempty"`
	Secrets  []SceneSecret  `json:"secrets,omitempty"`
	Outcomes []SceneOutcome `json:"outcomes,omitempty"`
}

// SessionPlan is the planned face of a game_sessions row.
type SessionPlan struct {
	SessionID  string    `json:"session_id"`
	CampaignID string    `json:"campaign_id"`
	ActID      string    `json:"act_id,omitempty"`
	Goal       string    `json:"goal"`
	PrepNotes  string    `json:"prep_notes"`
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// ErrScope re-exported: a spine read attempted under a perspective the read
// path does not serve.
var ErrScope = campaign.ErrScope

// requireDM refuses any scope but the DM's — the same rule the campaign
// graph's own reads follow (campaign's unexported helper, mirrored here the
// way internal/knowledge mirrors it).
func requireDM(scope campaign.Scope) error {
	if !scope.IsDM() {
		return fmt.Errorf("%w: %s", ErrScope, scope)
	}
	return nil
}

/* ---------- the store ---------- */

// Store reads and writes the narrative spine on the shared database handle.
// The schema must already be applied (migrate.Up runs before anything
// serves).
type Store struct {
	db *sql.DB
}

// New builds a story store on an open, migrated database handle.
func New(db *sql.DB) (*Store, error) {
	if db == nil {
		return nil, errors.New("story: nil database handle")
	}
	return &Store{db: db}, nil
}

// DB exposes the underlying handle for the canon engine's spine load.
func (s *Store) DB() *sql.DB { return s.db }

/* ---------- shared helpers ---------- */

// campaignExists reports ErrNotFound when the campaign row is missing, so a
// spine row for a foreign id is a 404 rather than a constraint traceback.
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

// actInCampaign resolves an act and confirms it belongs to the campaign.
func (s *Store) actInCampaign(ctx context.Context, actID, campaignID string) (*Act, error) {
	a, err := s.GetAct(ctx, campaign.ScopeDM, campaignID, actID)
	if err != nil {
		return nil, err
	}
	return a, nil
}

// sessionInCampaign confirms a game session belongs to the campaign, so a
// scene or plan for another campaign's session is a plain 404.
func (s *Store) sessionInCampaign(ctx context.Context, sessionID, campaignID string) error {
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM game_sessions WHERE id = ? AND campaign_id = ?`, sessionID, campaignID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: session %s", ErrNotFound, sessionID)
	}
	if err != nil {
		return fmt.Errorf("check session: %w", err)
	}
	return nil
}

// sceneInCampaign loads a scene and confirms it belongs to the campaign.
func (s *Store) sceneInCampaign(ctx context.Context, sceneID, campaignID string) (*Scene, error) {
	return s.GetScene(ctx, campaign.ScopeDM, campaignID, sceneID)
}

// entityInCampaign confirms an entity belongs to the campaign (the shared
// helper campaign.Store keeps private, mirrored here for the spine's own
// references).
func (s *Store) entityInCampaign(ctx context.Context, entityID, campaignID string) error {
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM entities WHERE id = ? AND campaign_id = ?`, entityID, campaignID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: entity %s", ErrNotFound, entityID)
	}
	if err != nil {
		return fmt.Errorf("check entity: %w", err)
	}
	return nil
}

// factInCampaign confirms a fact belongs to the campaign.
func (s *Store) factInCampaign(ctx context.Context, factID, campaignID string) error {
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM facts WHERE id = ? AND campaign_id = ?`, factID, campaignID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: fact %s", ErrNotFound, factID)
	}
	if err != nil {
		return fmt.Errorf("check fact: %w", err)
	}
	return nil
}

// questMachine loads a quest's state machine, campaign-scoped.
func (s *Store) questMachine(ctx context.Context, questID, campaignID string) (campaign.StateMachine, error) {
	var raw string
	err := s.db.QueryRowContext(ctx,
		`SELECT state_machine FROM quests WHERE id = ? AND campaign_id = ?`, questID, campaignID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return campaign.StateMachine{}, fmt.Errorf("%w: quest %s", ErrNotFound, questID)
	}
	if err != nil {
		return campaign.StateMachine{}, fmt.Errorf("check quest: %w", err)
	}
	return campaign.ParseStateMachine(raw)
}

// newID is the one place ids are minted.
func newID() string { return uuid.NewString() }
