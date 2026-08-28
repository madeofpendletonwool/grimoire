package faction

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/madeofpendletonwool/grimoire/internal/campaign"
)

// The faction plan store: the rows migration 0018 owns, read and written
// here. The pure model (Advance) stays pure; this file is where a plan meets
// the graph — validating its owner is a live faction, deriving its active
// step's modifiers from entity/edge/fact/enemy-plan state, and persisting
// every move to the transitions ledger with the arithmetic that caused it.
//
// Reads are DM-scope only, enforced with the same Scope every campaign
// retrieval takes: plans and a faction's PrivateTruth are DM material by
// construction (ADR 2). The dossier's player-facing half (public face,
// reputation, aware edges) is assembled by the server from scoped reads and
// never reaches this store.

// Store reads and writes faction plans on the shared database handle.
type Store struct {
	db    *sql.DB
	camps *campaign.Store
}

// New builds a faction store on an open, migrated database handle.
func New(db *sql.DB) (*Store, error) {
	if db == nil {
		return nil, errors.New("faction: nil database handle")
	}
	camps, err := campaign.New(db)
	if err != nil {
		return nil, err
	}
	return &Store{db: db, camps: camps}, nil
}

// PlanInput is the writable half of a plan on create.
type PlanInput struct {
	Name       string
	Machine    campaign.StateMachine
	Steps      []Step
	RatePerDay float64
	Status     string // empty defaults to dormant
	Visibility string // empty defaults to secret
}

// CreatePlan adds a plan for one faction, starting in its machine's initial
// state with nothing reached and nothing paid. The owner must be a live
// faction entity of the campaign; every step must name a declared state.
func (s *Store) CreatePlan(ctx context.Context, campaignID, factionEntityID string, in PlanInput) (*Plan, error) {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		return nil, fmt.Errorf("%w: plan name is required", campaign.ErrInvalid)
	}
	if in.Status == "" {
		in.Status = PlanDormant
	}
	if in.Visibility == "" {
		in.Visibility = campaign.VisibilitySecret
	}
	if in.RatePerDay < 0 {
		return nil, fmt.Errorf("%w: rate per day %v", campaign.ErrInvalid, in.RatePerDay)
	}
	if len(in.Steps) == 0 {
		return nil, fmt.Errorf("%w: a plan needs at least one step", campaign.ErrInvalid)
	}
	if err := s.campaignExists(ctx, campaignID); err != nil {
		return nil, err
	}
	owner, err := s.camps.GetEntity(ctx, campaign.ScopeDM, campaignID, factionEntityID)
	if err != nil {
		return nil, err
	}
	if owner.Kind != campaign.KindFaction {
		return nil, fmt.Errorf("%w: entity %q is a %s, not a faction", campaign.ErrInvalid, owner.Name, owner.Kind)
	}
	if owner.Status == campaign.StatusDestroyed || owner.Status == campaign.StatusDeleted {
		return nil, fmt.Errorf("%w: faction %q is %s", campaign.ErrInvalid, owner.Name, owner.Status)
	}
	p := &Plan{
		ID: uuid.NewString(), CampaignID: campaignID, FactionEntity: factionEntityID,
		Name: in.Name, Machine: in.Machine, CurrentState: in.Machine.Initial,
		Steps: in.Steps, Reached: []string{in.Machine.Initial},
		RatePerDay: in.RatePerDay, Status: in.Status, Visibility: in.Visibility,
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	machineJSON, err := p.Machine.Marshal()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("plan tx: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO faction_plans (id, campaign_id, faction_entity, name, state_machine,
		                           current_state, progress, rate_per_day, status, visibility, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?, ?)`,
		p.ID, p.CampaignID, p.FactionEntity, p.Name, machineJSON,
		p.CurrentState, p.RatePerDay, p.Status, p.Visibility, now, now); err != nil {
		return nil, fmt.Errorf("insert plan: %w", err)
	}
	for _, step := range p.Steps {
		requires, err := json.Marshal(step.Requires)
		if err != nil {
			return nil, fmt.Errorf("%w: step %s requires: %v", campaign.ErrInvalid, step.State, err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO faction_plan_steps (id, plan_id, state, name, detail, cost, requires_json)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			uuid.NewString(), p.ID, step.State,
			strings.TrimSpace(step.Name), step.Detail, step.Cost, string(requires)); err != nil {
			return nil, fmt.Errorf("insert step %s: %w", step.State, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("plan commit: %w", err)
	}
	return p, nil
}

const planCols = `id, campaign_id, faction_entity, name, state_machine, current_state,
                  progress, rate_per_day, status, visibility, started_day, last_advanced_day,
                  created_at, updated_at`

func (s *Store) scanPlan(row interface{ Scan(...any) error }) (*Plan, error) {
	var (
		p            Plan
		machineJSON  string
		started      sql.NullInt64
		lastAdvanced sql.NullInt64
		created      int64
		updated      int64
	)
	if err := row.Scan(&p.ID, &p.CampaignID, &p.FactionEntity, &p.Name, &machineJSON,
		&p.CurrentState, &p.Progress, &p.RatePerDay, &p.Status, &p.Visibility,
		&started, &lastAdvanced, &created, &updated); err != nil {
		return nil, err
	}
	m, err := campaign.ParseStateMachine(machineJSON)
	if err != nil {
		return nil, fmt.Errorf("plan %s carries an invalid state machine: %w", p.ID, err)
	}
	p.Machine = m
	if started.Valid {
		v := started.Int64
		p.StartedDay = &v
	}
	if lastAdvanced.Valid {
		v := lastAdvanced.Int64
		p.LastAdvanced = &v
	}
	return &p, nil
}

// loadSteps reads a plan's steps in pursuit order (rowid — insertion order,
// the same rule quest_transitions follows) and decodes their requirements.
func (s *Store) loadSteps(ctx context.Context, planID string) ([]Step, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT state, name, detail, cost, requires_json FROM faction_plan_steps
		 WHERE plan_id = ? ORDER BY rowid`, planID)
	if err != nil {
		return nil, fmt.Errorf("load steps: %w", err)
	}
	defer rows.Close()
	var out []Step
	for rows.Next() {
		var (
			step     Step
			cost     float64
			requires string
		)
		if err := rows.Scan(&step.State, &step.Name, &step.Detail, &cost, &requires); err != nil {
			return nil, err
		}
		step.Cost = cost
		if requires != "" && requires != "[]" {
			_ = json.Unmarshal([]byte(requires), &step.Requires) // malformed JSON degrades to no requirements, like a hand-edited payload
		}
		out = append(out, step)
	}
	return out, rows.Err()
}

// loadReached rebuilds the entered-state set from the transitions ledger:
// the machine's initial state plus every state a recorded move entered, in
// order. The ledger is the only source — a state reached twice stays once.
func (s *Store) loadReached(ctx context.Context, p *Plan) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT to_state FROM faction_plan_transitions WHERE plan_id = ? ORDER BY rowid`, p.ID)
	if err != nil {
		return nil, fmt.Errorf("load transitions: %w", err)
	}
	defer rows.Close()
	reached := []string{p.Machine.Initial}
	for rows.Next() {
		var to string
		if err := rows.Scan(&to); err != nil {
			return nil, err
		}
		known := false
		for _, r := range reached {
			if r == to {
				known = true
				break
			}
		}
		if !known {
			reached = append(reached, to)
		}
	}
	return reached, rows.Err()
}

func (s *Store) planInCampaign(ctx context.Context, id, campaignID string) (*Plan, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+planCols+` FROM faction_plans WHERE id = ? AND campaign_id = ?`, id, campaignID)
	p, err := s.scanPlan(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: plan %s", campaign.ErrNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	if p.Steps, err = s.loadSteps(ctx, p.ID); err != nil {
		return nil, err
	}
	if p.Reached, err = s.loadReached(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

// GetPlan returns one plan with its steps and reached states. DM-scope reads
// only: a faction's plans are its private machinery.
func (s *Store) GetPlan(ctx context.Context, scope campaign.Scope, campaignID, id string) (*Plan, error) {
	if !scope.IsDM() {
		return nil, fmt.Errorf("%w: %s", campaign.ErrScope, scope)
	}
	return s.planInCampaign(ctx, id, campaignID)
}

// ListPlans returns a campaign's plans with steps and reached states,
// ordered by faction then name. DM-scope reads only.
func (s *Store) ListPlans(ctx context.Context, scope campaign.Scope, campaignID string) ([]Plan, error) {
	return s.plansWhere(ctx, scope, campaignID, ``, nil)
}

// PlansOfFaction narrows ListPlans to one faction entity.
func (s *Store) PlansOfFaction(ctx context.Context, scope campaign.Scope, campaignID, factionEntityID string) ([]Plan, error) {
	return s.plansWhere(ctx, scope, campaignID, ` AND faction_entity = ?`, []any{factionEntityID})
}

func (s *Store) plansWhere(ctx context.Context, scope campaign.Scope, campaignID, extra string, args []any) ([]Plan, error) {
	if !scope.IsDM() {
		return nil, fmt.Errorf("%w: %s", campaign.ErrScope, scope)
	}
	if err := s.campaignExists(ctx, campaignID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+planCols+` FROM faction_plans WHERE campaign_id = ?`+extra+
			` ORDER BY faction_entity, name COLLATE NOCASE, id`, append([]any{campaignID}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("list plans: %w", err)
	}
	defer rows.Close()
	var out []Plan
	for rows.Next() {
		p, err := s.scanPlan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Steps, err = s.loadSteps(ctx, out[i].ID); err != nil {
			return nil, err
		}
		if out[i].Reached, err = s.loadReached(ctx, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Transition is one recorded plan move, mirroring quest transitions plus the
// clock day it happened on and the arithmetic that caused it when it was an
// advance.
type Transition struct {
	ID        string
	PlanID    string
	FromState string
	ToState   string
	EventID   string
	ClockDay  *int64
	Reason    string
	CreatedAt time.Time
}

// PlanTransitions lists a plan's moves in the order they happened.
// DM-scope reads only.
func (s *Store) PlanTransitions(ctx context.Context, scope campaign.Scope, campaignID, planID string) ([]Transition, error) {
	if !scope.IsDM() {
		return nil, fmt.Errorf("%w: %s", campaign.ErrScope, scope)
	}
	if _, err := s.planInCampaign(ctx, planID, campaignID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, plan_id, from_state, to_state, event_id, clock_day, reason, created_at
		  FROM faction_plan_transitions WHERE plan_id = ? ORDER BY rowid`, planID)
	if err != nil {
		return nil, fmt.Errorf("list plan transitions: %w", err)
	}
	defer rows.Close()
	var out []Transition
	for rows.Next() {
		var (
			t       Transition
			event   sql.NullString
			day     sql.NullInt64
			created int64
		)
		if err := rows.Scan(&t.ID, &t.PlanID, &t.FromState, &t.ToState, &event, &day, &t.Reason, &created); err != nil {
			return nil, err
		}
		t.EventID = event.String
		if day.Valid {
			v := day.Int64
			t.ClockDay = &v
		}
		t.CreatedAt = time.UnixMilli(created).UTC()
		out = append(out, t)
	}
	return out, rows.Err()
}

// UpdatePlanInput is the PATCH body: nil fields are left alone.
type UpdatePlanInput struct {
	Name       *string
	RatePerDay *float64
	Status     *string
	Visibility *string
	Progress   *float64
}

// UpdatePlan patches a plan's editable fields. Activating a dormant plan
// stamps started_day with the campaign's current clock day if it has none.
func (s *Store) UpdatePlan(ctx context.Context, campaignID, id string, in UpdatePlanInput) (*Plan, error) {
	p, err := s.planInCampaign(ctx, id, campaignID)
	if err != nil {
		return nil, err
	}
	if in.Name != nil {
		p.Name = strings.TrimSpace(*in.Name)
		if p.Name == "" {
			return nil, fmt.Errorf("%w: plan name is required", campaign.ErrInvalid)
		}
	}
	if in.RatePerDay != nil {
		if *in.RatePerDay < 0 {
			return nil, fmt.Errorf("%w: rate per day %v", campaign.ErrInvalid, *in.RatePerDay)
		}
		p.RatePerDay = *in.RatePerDay
	}
	if in.Status != nil {
		if !ValidPlanStatuses[*in.Status] {
			return nil, fmt.Errorf("%w: plan status %q", campaign.ErrInvalid, *in.Status)
		}
		p.Status = *in.Status
	}
	if in.Visibility != nil {
		if *in.Visibility != campaign.VisibilityPublic && *in.Visibility != campaign.VisibilitySecret {
			return nil, fmt.Errorf("%w: visibility %q", campaign.ErrInvalid, *in.Visibility)
		}
		p.Visibility = *in.Visibility
	}
	if in.Progress != nil {
		if *in.Progress < 0 {
			return nil, fmt.Errorf("%w: progress %v", campaign.ErrInvalid, *in.Progress)
		}
		p.Progress = *in.Progress
	}
	if p.Status == PlanActive && p.StartedDay == nil {
		day, err := s.campaignClock(ctx, campaignID)
		if err != nil {
			return nil, err
		}
		p.StartedDay = &day
	}
	var startedDay any
	if p.StartedDay != nil {
		startedDay = *p.StartedDay
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE faction_plans SET name = ?, rate_per_day = ?, status = ?, visibility = ?,
		                         progress = ?, started_day = ?, updated_at = ?
		 WHERE id = ? AND campaign_id = ?`,
		p.Name, p.RatePerDay, p.Status, p.Visibility, p.Progress,
		startedDay, time.Now().UTC().UnixMilli(), id, campaignID)
	if err != nil {
		return nil, fmt.Errorf("update plan: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, fmt.Errorf("%w: plan %s", campaign.ErrNotFound, id)
	}
	return s.planInCampaign(ctx, id, campaignID)
}

// TransitionPlan moves a plan along a declared edge — the TransitionQuest
// rule reused, not re-derived — recording the move with the event that
// caused it when one is given. A manual move pays no cost and carries
// nothing: progress resets, because the step it was paying for is either
// done or abandoned.
func (s *Store) TransitionPlan(ctx context.Context, campaignID, planID, toState, eventID, reason string) (*Plan, error) {
	p, err := s.planInCampaign(ctx, planID, campaignID)
	if err != nil {
		return nil, err
	}
	if !p.Machine.HasEdge(p.CurrentState, toState) {
		return nil, fmt.Errorf("%w: plan %q cannot move %s -> %s; the machine has no such edge",
			campaign.ErrInvalid, p.Name, p.CurrentState, toState)
	}
	if eventID != "" {
		if _, err := s.camps.GetEvent(ctx, campaign.ScopeDM, campaignID, eventID); err != nil {
			return nil, err
		}
	}
	day, err := s.campaignClock(ctx, campaignID)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("plan tx: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO faction_plan_transitions (id, plan_id, from_state, to_state, event_id, clock_day, reason, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		uuid.NewString(), planID, p.CurrentState, toState,
		nullString(eventID), day, reason, now); err != nil {
		return nil, fmt.Errorf("insert plan transition: %w", err)
	}
	status := p.Status
	if nextPlan := p.afterTransition(toState); nextPlan.ActiveStep() == nil && len(p.Steps) > 0 {
		status = PlanComplete
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE faction_plans SET current_state = ?, progress = 0, status = ?, updated_at = ?
		 WHERE id = ? AND campaign_id = ?`,
		toState, status, now, planID, campaignID); err != nil {
		return nil, fmt.Errorf("update plan state: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("plan commit: %w", err)
	}
	return s.planInCampaign(ctx, planID, campaignID)
}

// afterTransition returns the plan as if the given move had been recorded,
// so completeness can be decided without a reload.
func (p Plan) afterTransition(to string) Plan {
	next := p
	next.CurrentState = to
	next.Reached = append(append([]string{}, p.Reached...), to)
	return next
}

/* ---------- modifiers: interference is a rule, not a mood ---------- */

// RequirementStatus is one evaluated precondition: whether it currently
// holds and, when it does not, the author's reaction if one was declared.
type RequirementStatus struct {
	Requirement Requirement `json:"requirement"`
	Met         bool        `json:"met"`
	Why         string      `json:"why,omitempty"`
}

// Modifiers evaluates a plan's active step's preconditions against the
// graph and returns the modifier set an advance should apply: one modifier
// per broken requirement whose author declared a reaction. Satisfied
// preconditions contribute nothing (factor 1); a broken requirement without
// a declared reaction is the DM's business, surfaced by RequirementStatuses
// but not guessed at here.
func (s *Store) Modifiers(ctx context.Context, p *Plan) ([]Modifier, []RequirementStatus, error) {
	step := p.ActiveStep()
	if step == nil {
		return nil, nil, nil
	}
	var mods []Modifier
	var statuses []RequirementStatus
	for _, req := range step.Requires {
		met, why, err := s.requirementHolds(ctx, p, req)
		if err != nil {
			return nil, nil, err
		}
		statuses = append(statuses, RequirementStatus{Requirement: req, Met: met, Why: why})
		if met || req.IfBroken == nil {
			continue
		}
		label := req.Label
		if label == "" {
			label = "requirement"
		}
		mods = append(mods, Modifier{
			Label:  label,
			Factor: req.IfBroken.Factor,
			Reason: req.IfBroken.Reason,
		})
	}
	return mods, statuses, nil
}

// RequirementStatuses evaluates a plan's active step's prepositions without
// deriving anything — the dossier's "what does this step need right now"
// read.
func (s *Store) RequirementStatuses(ctx context.Context, p *Plan) ([]RequirementStatus, error) {
	_, statuses, err := s.Modifiers(ctx, p)
	return statuses, err
}

// requirementHolds is one precondition against live graph state.
func (s *Store) requirementHolds(ctx context.Context, p *Plan, req Requirement) (bool, string, error) {
	switch {
	case req.Entity != "":
		e, err := s.camps.GetEntity(ctx, campaign.ScopeDM, p.CampaignID, req.Entity)
		if errors.Is(err, campaign.ErrNotFound) {
			return false, fmt.Sprintf("entity %s does not exist", req.Entity), nil
		}
		if err != nil {
			return false, "", err
		}
		if e.Status == campaign.StatusDestroyed || e.Status == campaign.StatusDeleted {
			return false, fmt.Sprintf("%s is %s", e.Name, e.Status), nil
		}
		return true, "", nil

	case req.Edge != nil:
		var one int
		err := s.db.QueryRowContext(ctx,
			`SELECT 1 FROM relationships WHERE from_entity = ? AND rel_type = ? AND to_entity = ?`,
			req.Edge.From, req.Edge.Type, req.Edge.To).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return false, fmt.Sprintf("edge %s %s %s is gone", shortID(req.Edge.From), req.Edge.Type, shortID(req.Edge.To)), nil
		}
		if err != nil {
			return false, "", fmt.Errorf("check edge: %w", err)
		}
		return true, "", nil

	case req.Fact != nil:
		holds, err := s.factHolds(ctx, p.CampaignID, req.Fact)
		if err != nil {
			return false, "", err
		}
		if !holds {
			return false, fmt.Sprintf("no credible fact %s %s", shortID(req.Fact.Subject), req.Fact.Predicate), nil
		}
		return true, "", nil

	case req.EnemyPlan != nil:
		reached, who, err := s.enemyPlanAt(ctx, p.CampaignID, p.FactionEntity, req.EnemyPlan.State)
		if err != nil {
			return false, "", err
		}
		if reached {
			return false, fmt.Sprintf("an enemy faction's plan (%s) reached %s", shortID(who), req.EnemyPlan.State), nil
		}
		return true, "", nil

	default:
		// A requirement with no clause is vacuously satisfied; validation
		// on the write path discourages it, not forbids it.
		return true, "", nil
	}
}

// factHolds: a non-superseded canon or derived fact matching subject and
// predicate, and object (entity or literal) when set. Proposed and contested
// facts are not credible; retconned history is not current truth.
func (s *Store) factHolds(ctx context.Context, campaignID string, req *FactReq) (bool, error) {
	q := `SELECT COUNT(*) FROM facts
	       WHERE campaign_id = ? AND subject_entity = ? AND predicate = ?
	         AND superseded_by IS NULL
	         AND confidence IN ('canon','derived')`
	args := []any{campaignID, req.Subject, req.Predicate}
	if req.Object != "" {
		q += ` AND (object_entity = ? OR object_literal = ?)`
		args = append(args, req.Object, req.Object)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return false, fmt.Errorf("check fact: %w", err)
	}
	return n > 0, nil
}

// enemyPlanAt reports whether any faction joined to this one by enemy_of
// (either direction) has a plan whose current state is state, and which.
func (s *Store) enemyPlanAt(ctx context.Context, campaignID, factionEntityID, state string) (bool, string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.faction_entity, p.id FROM faction_plans p
		 WHERE p.campaign_id = ? AND p.current_state = ? AND p.status != 'abandoned'
		   AND (p.faction_entity IN (SELECT to_entity FROM relationships
		                              WHERE from_entity = ? AND rel_type = 'enemy_of')
		     OR p.faction_entity IN (SELECT from_entity FROM relationships
		                             WHERE to_entity = ? AND rel_type = 'enemy_of'))`,
		campaignID, state, factionEntityID, factionEntityID)
	if err != nil {
		return false, "", fmt.Errorf("check enemy plans: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var faction, id string
		if err := rows.Scan(&faction, &id); err != nil {
			return false, "", err
		}
		return true, faction, nil
	}
	return false, "", rows.Err()
}

/* ---------- advancing ---------- */

// AdvancePlan advances a plan days days: derives the active step's modifiers
// from graph state, runs the pure model, and persists the outcome — the new
// state, the carried progress, a transitions row per state entered carrying
// the arithmetic in its reason, and last_advanced_day. The caller owns what
// "days" means (the clock's delta since the last advance; the simulation
// tick is the next stage).
func (s *Store) AdvancePlan(ctx context.Context, campaignID, planID string, days int) (*Plan, *Progression, error) {
	p, err := s.planInCampaign(ctx, planID, campaignID)
	if err != nil {
		return nil, nil, err
	}
	mods, _, err := s.Modifiers(ctx, p)
	if err != nil {
		return nil, nil, err
	}
	pr := Advance(*p, days, mods)
	if days > 0 && p.Status == PlanActive &&
		(pr.Gain != 0 || pr.ToProgress != p.Progress || len(pr.Moves) > 0) {
		day, err := s.campaignClock(ctx, campaignID)
		if err != nil {
			return nil, nil, err
		}
		now := time.Now().UTC().UnixMilli()
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return nil, nil, fmt.Errorf("advance tx: %w", err)
		}
		defer tx.Rollback()
		from := p.CurrentState
		reason := pr.Summary()
		for i, m := range pr.Moves {
			r := reason
			if i > 0 {
				r = fmt.Sprintf("entered %s (carry %s)", m.To, formatNum(m.Carry))
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO faction_plan_transitions (id, plan_id, from_state, to_state, event_id, clock_day, reason, created_at)
				VALUES (?, ?, ?, ?, NULL, ?, ?, ?)`,
				uuid.NewString(), planID, from, m.To, day, r, now); err != nil {
				return nil, nil, fmt.Errorf("insert plan transition: %w", err)
			}
			from = m.To
		}
		status := p.Status
		next := p.afterTransition(pr.ToState)
		next.Progress = pr.ToProgress
		if next.ActiveStep() == nil && len(p.Steps) > 0 {
			status = PlanComplete
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE faction_plans SET current_state = ?, progress = ?, status = ?, last_advanced_day = ?, updated_at = ?
			 WHERE id = ? AND campaign_id = ?`,
			pr.ToState, pr.ToProgress, status, day, now, planID, campaignID); err != nil {
			return nil, nil, fmt.Errorf("update plan: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return nil, nil, fmt.Errorf("advance commit: %w", err)
		}
	}
	updated, err := s.planInCampaign(ctx, planID, campaignID)
	if err != nil {
		return nil, nil, err
	}
	return updated, &pr, nil
}

/* ---------- shared helpers ---------- */

// campaignExists reports ErrNotFound when the campaign row is missing.
func (s *Store) campaignExists(ctx context.Context, id string) error {
	_, err := s.camps.GetCampaign(ctx, id)
	return err
}

// campaignClock reads the campaign's current in-world day.
func (s *Store) campaignClock(ctx context.Context, campaignID string) (int64, error) {
	var day int64
	err := s.db.QueryRowContext(ctx,
		`SELECT clock FROM campaigns WHERE id = ?`, campaignID).Scan(&day)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("%w: campaign %s", campaign.ErrNotFound, campaignID)
	}
	if err != nil {
		return 0, fmt.Errorf("read clock: %w", err)
	}
	return day, nil
}

// nullString maps "" to SQL NULL.
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// shortID renders an entity id for a why-not message without leaking the
// whole uuid into the ledger prose.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
