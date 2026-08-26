package story

// The spine's store: CRUD for acts, scenes, cast, secrets, outcomes and
// session plans. Reads that serve the DM surface take a campaign.Scope and
// refuse everything but the DM's — the spine is planning material, and the
// campaign package's rule (scope decides in the store, not the route)
// applies here unchanged. Writes are gated one layer up, in the handlers;
// what this file enforces is shape: vocabularies, campaign scoping, and the
// quest-edge check an outcome's transition must survive.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
)

/* ---------- acts ---------- */

const actCols = `id, campaign_id, ordinal, name, premise, level_start, level_end, status, created_at, updated_at`

func scanAct(row interface{ Scan(...any) error }) (*Act, error) {
	var (
		a              Act
		created, dated int64
	)
	if err := row.Scan(&a.ID, &a.CampaignID, &a.Ordinal, &a.Name, &a.Premise,
		&a.LevelStart, &a.LevelEnd, &a.Status, &created, &dated); err != nil {
		return nil, err
	}
	a.CreatedAt = time.UnixMilli(created).UTC()
	a.UpdatedAt = time.UnixMilli(dated).UTC()
	return &a, nil
}

// CreateAct appends an act to a campaign. The ordinal is max+1 within the
// campaign, assigned inside the INSERT itself so two DMs planning at once
// cannot take the same number.
func (s *Store) CreateAct(ctx context.Context, campaignID, name, premise string, levelStart, levelEnd int) (*Act, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("%w: act name is required", ErrInvalid)
	}
	if err := validateLevelBand(levelStart, levelEnd); err != nil {
		return nil, err
	}
	if err := s.campaignExists(ctx, campaignID); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	a := &Act{
		ID: newID(), CampaignID: campaignID, Name: name, Premise: premise,
		LevelStart: levelStart, LevelEnd: levelEnd, Status: StatusPlanned,
		CreatedAt: now, UpdatedAt: now,
	}
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO acts (id, campaign_id, ordinal, name, premise, level_start, level_end, status, created_at, updated_at)
		VALUES (?, ?, (SELECT COALESCE(MAX(ordinal), 0) + 1 FROM acts WHERE campaign_id = ?), ?, ?, ?, ?, ?, ?, ?)
		RETURNING ordinal`,
		a.ID, a.CampaignID, a.CampaignID, a.Name, a.Premise,
		a.LevelStart, a.LevelEnd, a.Status, now.UnixMilli(), now.UnixMilli()).Scan(&a.Ordinal)
	if err != nil {
		return nil, fmt.Errorf("insert act: %w", err)
	}
	return a, nil
}

// validateLevelBand checks a 1-20 band against the levels the game has.
func validateLevelBand(start, end int) error {
	if start < 1 || end > 20 {
		return fmt.Errorf("%w: a level band lives inside levels 1-20 (got %d-%d)", ErrInvalid, start, end)
	}
	if start > end {
		return fmt.Errorf("%w: a level band starts before it ends (got %d-%d)", ErrInvalid, start, end)
	}
	return nil
}

// GetAct returns one act of a campaign. DM-scope reads only: the spine is DM
// planning material.
func (s *Store) GetAct(ctx context.Context, scope campaign.Scope, campaignID, id string) (*Act, error) {
	if err := requireDM(scope); err != nil {
		return nil, err
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT `+actCols+` FROM acts WHERE id = ? AND campaign_id = ?`, id, campaignID)
	a, err := scanAct(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: act %s", ErrNotFound, id)
	}
	return a, err
}

// ListActs returns a campaign's acts in order. DM-scope reads only.
func (s *Store) ListActs(ctx context.Context, scope campaign.Scope, campaignID string) ([]Act, error) {
	if err := requireDM(scope); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+actCols+` FROM acts WHERE campaign_id = ? ORDER BY ordinal`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("list acts: %w", err)
	}
	defer rows.Close()
	var out []Act
	for rows.Next() {
		a, err := scanAct(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// UpdateAct replaces the mutable fields. nil arguments leave the
// corresponding field alone.
func (s *Store) UpdateAct(ctx context.Context, campaignID, id string, name, premise *string, levelStart, levelEnd *int, status *string) (*Act, error) {
	a, err := s.GetAct(ctx, campaign.ScopeDM, campaignID, id)
	if err != nil {
		return nil, err
	}
	if name != nil {
		a.Name = strings.TrimSpace(*name)
		if a.Name == "" {
			return nil, fmt.Errorf("%w: act name is required", ErrInvalid)
		}
	}
	if premise != nil {
		a.Premise = *premise
	}
	if levelStart != nil {
		a.LevelStart = *levelStart
	}
	if levelEnd != nil {
		a.LevelEnd = *levelEnd
	}
	if err := validateLevelBand(a.LevelStart, a.LevelEnd); err != nil {
		return nil, err
	}
	if status != nil {
		st := strings.TrimSpace(*status)
		if !validStatus[st] {
			return nil, fmt.Errorf("%w: act status %q", ErrInvalid, st)
		}
		a.Status = st
	}
	a.UpdatedAt = time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `
		UPDATE acts SET name = ?, premise = ?, level_start = ?, level_end = ?, status = ?, updated_at = ?
		 WHERE id = ? AND campaign_id = ?`,
		a.Name, a.Premise, a.LevelStart, a.LevelEnd, a.Status, a.UpdatedAt.UnixMilli(), id, campaignID)
	if err != nil {
		return nil, fmt.Errorf("update act: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, fmt.Errorf("%w: act %s", ErrNotFound, id)
	}
	return a, nil
}

// DeleteAct removes an act and everything attached to it (scenes cascade).
func (s *Store) DeleteAct(ctx context.Context, campaignID, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM acts WHERE id = ? AND campaign_id = ?`, id, campaignID)
	if err != nil {
		return fmt.Errorf("delete act: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: act %s", ErrNotFound, id)
	}
	return nil
}

/* ---------- scenes ---------- */

const sceneCols = `id, campaign_id, act_id, session_id, ordinal, kind, name, purpose, setting_entity, status, created_at, updated_at`

func scanScene(row interface{ Scan(...any) error }) (*Scene, error) {
	var (
		sc             Scene
		session, set   sql.NullString
		created, dated int64
	)
	if err := row.Scan(&sc.ID, &sc.CampaignID, &sc.ActID, &session, &sc.Ordinal, &sc.Kind,
		&sc.Name, &sc.Purpose, &set, &sc.Status, &created, &dated); err != nil {
		return nil, err
	}
	sc.SessionID = session.String
	sc.SettingEntity = set.String
	sc.CreatedAt = time.UnixMilli(created).UTC()
	sc.UpdatedAt = time.UnixMilli(dated).UTC()
	return &sc, nil
}

// CreateScene appends a scene to an act. The ordinal is max+1 within the
// act. An optional session seats the scene; an optional setting entity says
// where it happens. Both must belong to the same campaign when given.
func (s *Store) CreateScene(ctx context.Context, campaignID, actID, sessionID, kind, name, purpose, settingEntity string) (*Scene, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("%w: scene name is required", ErrInvalid)
	}
	kind = strings.TrimSpace(kind)
	if !validKinds[kind] {
		return nil, fmt.Errorf("%w: scene kind %q", ErrInvalid, kind)
	}
	act, err := s.actInCampaign(ctx, actID, campaignID)
	if err != nil {
		return nil, err
	}
	if sessionID != "" {
		if err := s.sessionInCampaign(ctx, sessionID, campaignID); err != nil {
			return nil, err
		}
	}
	if settingEntity != "" {
		if err := s.entityInCampaign(ctx, settingEntity, campaignID); err != nil {
			return nil, err
		}
	}
	now := time.Now().UTC()
	sc := &Scene{
		ID: newID(), CampaignID: campaignID, ActID: act.ID, SessionID: sessionID,
		Kind: kind, Name: name, Purpose: purpose, SettingEntity: settingEntity,
		Status: StatusPlanned, CreatedAt: now, UpdatedAt: now,
	}
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO scenes (id, campaign_id, act_id, session_id, ordinal, kind, name, purpose, setting_entity, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, (SELECT COALESCE(MAX(ordinal), 0) + 1 FROM scenes WHERE act_id = ?), ?, ?, ?, ?, ?, ?, ?)
		RETURNING ordinal`,
		sc.ID, sc.CampaignID, sc.ActID, nullString(sc.SessionID), sc.ActID,
		sc.Kind, sc.Name, sc.Purpose, nullString(sc.SettingEntity), sc.Status,
		now.UnixMilli(), now.UnixMilli()).Scan(&sc.Ordinal)
	if err != nil {
		return nil, fmt.Errorf("insert scene: %w", err)
	}
	return sc, nil
}

// nullString maps "" to SQL NULL.
func nullString(sv string) any {
	if sv == "" {
		return nil
	}
	return sv
}

// GetScene returns one scene with its cast, secrets and outcomes attached.
// DM-scope reads only.
func (s *Store) GetScene(ctx context.Context, scope campaign.Scope, campaignID, id string) (*Scene, error) {
	if err := requireDM(scope); err != nil {
		return nil, err
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT `+sceneCols+` FROM scenes WHERE id = ? AND campaign_id = ?`, id, campaignID)
	sc, err := scanScene(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: scene %s", ErrNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	if err := s.attachSceneDetail(ctx, sc); err != nil {
		return nil, err
	}
	return sc, nil
}

// attachSceneDetail loads a scene's cast, secrets and outcomes in place.
func (s *Store) attachSceneDetail(ctx context.Context, sc *Scene) error {
	var err error
	if sc.Cast, err = s.sceneCast(ctx, sc.ID); err != nil {
		return err
	}
	if sc.Secrets, err = s.sceneSecrets(ctx, sc.ID); err != nil {
		return err
	}
	sc.Outcomes, err = s.sceneOutcomes(ctx, sc.ID)
	return err
}

// ListScenes returns a campaign's scenes in act/ordinal order, optionally
// narrowed to one act. DM-scope reads only. The flat form: cast, secrets and
// outcomes are not attached.
func (s *Store) ListScenes(ctx context.Context, scope campaign.Scope, campaignID, actID string) ([]Scene, error) {
	if err := requireDM(scope); err != nil {
		return nil, err
	}
	q := `SELECT ` + sceneCols + ` FROM scenes WHERE campaign_id = ?`
	args := []any{campaignID}
	if actID != "" {
		q += ` AND act_id = ?`
		args = append(args, actID)
	}
	q += ` ORDER BY act_id, ordinal`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list scenes: %w", err)
	}
	defer rows.Close()
	var out []Scene
	for rows.Next() {
		sc, err := scanScene(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *sc)
	}
	return out, rows.Err()
}

// UpdateScene replaces the mutable fields. nil arguments leave the
// corresponding field alone; an empty sessionID/settingEntity pointer clears
// the field.
func (s *Store) UpdateScene(ctx context.Context, campaignID, id string, sessionID *string, kind, name, purpose, settingEntity, status *string) (*Scene, error) {
	sc, err := s.GetScene(ctx, campaign.ScopeDM, campaignID, id)
	if err != nil {
		return nil, err
	}
	if sessionID != nil {
		if *sessionID != "" {
			if err := s.sessionInCampaign(ctx, *sessionID, campaignID); err != nil {
				return nil, err
			}
		}
		sc.SessionID = strings.TrimSpace(*sessionID)
	}
	if kind != nil {
		k := strings.TrimSpace(*kind)
		if !validKinds[k] {
			return nil, fmt.Errorf("%w: scene kind %q", ErrInvalid, k)
		}
		sc.Kind = k
	}
	if name != nil {
		sc.Name = strings.TrimSpace(*name)
		if sc.Name == "" {
			return nil, fmt.Errorf("%w: scene name is required", ErrInvalid)
		}
	}
	if purpose != nil {
		sc.Purpose = *purpose
	}
	if settingEntity != nil {
		if *settingEntity != "" {
			if err := s.entityInCampaign(ctx, *settingEntity, campaignID); err != nil {
				return nil, err
			}
		}
		sc.SettingEntity = strings.TrimSpace(*settingEntity)
	}
	if status != nil {
		st := strings.TrimSpace(*status)
		if !validStatus[st] {
			return nil, fmt.Errorf("%w: scene status %q", ErrInvalid, st)
		}
		sc.Status = st
	}
	sc.UpdatedAt = time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `
		UPDATE scenes SET session_id = ?, kind = ?, name = ?, purpose = ?, setting_entity = ?, status = ?, updated_at = ?
		 WHERE id = ? AND campaign_id = ?`,
		nullString(sc.SessionID), sc.Kind, sc.Name, sc.Purpose, nullString(sc.SettingEntity),
		sc.Status, sc.UpdatedAt.UnixMilli(), id, campaignID)
	if err != nil {
		return nil, fmt.Errorf("update scene: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, fmt.Errorf("%w: scene %s", ErrNotFound, id)
	}
	return sc, nil
}

// DeleteScene removes a scene and everything attached to it.
func (s *Store) DeleteScene(ctx context.Context, campaignID, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM scenes WHERE id = ? AND campaign_id = ?`, id, campaignID)
	if err != nil {
		return fmt.Errorf("delete scene: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: scene %s", ErrNotFound, id)
	}
	return nil
}

/* ---------- cast ---------- */

// AddCast seats an entity in a scene. A second row for the same pair is a
// role change, not an error — recasting is ordinary planning.
func (s *Store) AddCast(ctx context.Context, campaignID, sceneID, entityID, role string) ([]CastMember, error) {
	if !validCastRoles[role] {
		return nil, fmt.Errorf("%w: cast role %q", ErrInvalid, role)
	}
	if _, err := s.sceneInCampaign(ctx, sceneID, campaignID); err != nil {
		return nil, err
	}
	if err := s.entityInCampaign(ctx, entityID, campaignID); err != nil {
		return nil, err
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO scene_cast (id, scene_id, entity_id, role, created_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (scene_id, entity_id) DO UPDATE SET role = excluded.role`,
		newID(), sceneID, entityID, role, time.Now().UTC().UnixMilli()); err != nil {
		return nil, fmt.Errorf("add cast: %w", err)
	}
	return s.sceneCast(ctx, sceneID)
}

// RemoveCast drops an entity from a scene's cast.
func (s *Store) RemoveCast(ctx context.Context, campaignID, sceneID, entityID string) error {
	if _, err := s.sceneInCampaign(ctx, sceneID, campaignID); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM scene_cast WHERE scene_id = ? AND entity_id = ?`, sceneID, entityID); err != nil {
		return fmt.Errorf("remove cast: %w", err)
	}
	return nil
}

// sceneCast lists a scene's cast in role order.
func (s *Store) sceneCast(ctx context.Context, sceneID string) ([]CastMember, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT scene_id, entity_id, role, created_at FROM scene_cast WHERE scene_id = ?
		 ORDER BY role, entity_id`, sceneID)
	if err != nil {
		return nil, fmt.Errorf("list cast: %w", err)
	}
	defer rows.Close()
	var out []CastMember
	for rows.Next() {
		var (
			c       CastMember
			created int64
		)
		if err := rows.Scan(&c.SceneID, &c.EntityID, &c.Role, &created); err != nil {
			return nil, err
		}
		c.CreatedAt = time.UnixMilli(created).UTC()
		out = append(out, c)
	}
	return out, rows.Err()
}

/* ---------- secrets ---------- */

// SetSecret places a fact in a scene with a disposition. Setting the same
// fact twice moves it, it does not duplicate.
func (s *Store) SetSecret(ctx context.Context, campaignID, sceneID, factID, disposition string) ([]SceneSecret, error) {
	if !validDispositions[disposition] {
		return nil, fmt.Errorf("%w: secret disposition %q", ErrInvalid, disposition)
	}
	if _, err := s.sceneInCampaign(ctx, sceneID, campaignID); err != nil {
		return nil, err
	}
	if err := s.factInCampaign(ctx, factID, campaignID); err != nil {
		return nil, err
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO scene_secrets (id, scene_id, fact_id, disposition, created_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (scene_id, fact_id) DO UPDATE SET disposition = excluded.disposition`,
		newID(), sceneID, factID, disposition, time.Now().UTC().UnixMilli()); err != nil {
		return nil, fmt.Errorf("set secret: %w", err)
	}
	return s.sceneSecrets(ctx, sceneID)
}

// RemoveSecret takes a fact back out of a scene.
func (s *Store) RemoveSecret(ctx context.Context, campaignID, sceneID, factID string) error {
	if _, err := s.sceneInCampaign(ctx, sceneID, campaignID); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM scene_secrets WHERE scene_id = ? AND fact_id = ?`, sceneID, factID); err != nil {
		return fmt.Errorf("remove secret: %w", err)
	}
	return nil
}

// sceneSecrets lists a scene's secrets in disposition order.
func (s *Store) sceneSecrets(ctx context.Context, sceneID string) ([]SceneSecret, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT scene_id, fact_id, disposition, created_at FROM scene_secrets WHERE scene_id = ?
		 ORDER BY disposition, fact_id`, sceneID)
	if err != nil {
		return nil, fmt.Errorf("list secrets: %w", err)
	}
	defer rows.Close()
	var out []SceneSecret
	for rows.Next() {
		var (
			sc      SceneSecret
			created int64
		)
		if err := rows.Scan(&sc.SceneID, &sc.FactID, &sc.Disposition, &created); err != nil {
			return nil, err
		}
		sc.CreatedAt = time.UnixMilli(created).UTC()
		out = append(out, sc)
	}
	return out, rows.Err()
}

/* ---------- outcomes ---------- */

// AddOutcome adds one branch to a scene. A quest transition, when named, is
// validated against that quest's own machine right here — the same rule
// TransitionQuest enforces on a recorded move — so an outcome can never name
// a move the quest cannot make at write time. (story.Validate re-checks
// later, because a machine may change under a planned outcome.)
func (s *Store) AddOutcome(ctx context.Context, campaignID, sceneID, label, summary, leadsToScene string, transition *QuestTransition) ([]SceneOutcome, error) {
	label = strings.TrimSpace(label)
	if label == "" {
		return nil, fmt.Errorf("%w: outcome label is required", ErrInvalid)
	}
	if _, err := s.sceneInCampaign(ctx, sceneID, campaignID); err != nil {
		return nil, err
	}
	if leadsToScene != "" {
		if _, err := s.sceneInCampaign(ctx, leadsToScene, campaignID); err != nil {
			return nil, err
		}
	}
	transitionJSON := ""
	if transition != nil {
		m, err := s.questMachine(ctx, transition.QuestID, campaignID)
		if err != nil {
			return nil, err
		}
		if !m.HasEdge(transition.From, transition.To) {
			return nil, fmt.Errorf("%w: quest %s has no edge %s -> %s; the outcome cannot promise that move",
				ErrInvalid, transition.QuestID, transition.From, transition.To)
		}
		b, err := json.Marshal(*transition)
		if err != nil {
			return nil, fmt.Errorf("encode quest transition: %w", err)
		}
		transitionJSON = string(b)
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO scene_outcomes (id, scene_id, label, summary, leads_to_scene, quest_transition, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (scene_id, label) DO UPDATE SET
			summary = excluded.summary, leads_to_scene = excluded.leads_to_scene,
			quest_transition = excluded.quest_transition`,
		newID(), sceneID, label, summary, nullString(leadsToScene), transitionJSON,
		time.Now().UTC().UnixMilli()); err != nil {
		return nil, fmt.Errorf("add outcome: %w", err)
	}
	return s.sceneOutcomes(ctx, sceneID)
}

// RemoveOutcome drops one branch of a scene by label.
func (s *Store) RemoveOutcome(ctx context.Context, campaignID, sceneID, label string) error {
	if _, err := s.sceneInCampaign(ctx, sceneID, campaignID); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM scene_outcomes WHERE scene_id = ? AND label = ?`, sceneID, label); err != nil {
		return fmt.Errorf("remove outcome: %w", err)
	}
	return nil
}

// sceneOutcomes lists a scene's outcomes in label order.
func (s *Store) sceneOutcomes(ctx context.Context, sceneID string) ([]SceneOutcome, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, scene_id, label, summary, leads_to_scene, quest_transition, created_at
		  FROM scene_outcomes WHERE scene_id = ? ORDER BY label`, sceneID)
	if err != nil {
		return nil, fmt.Errorf("list outcomes: %w", err)
	}
	defer rows.Close()
	var out []SceneOutcome
	for rows.Next() {
		var (
			o       SceneOutcome
			leads   sql.NullString
			raw     sql.NullString
			created int64
		)
		if err := rows.Scan(&o.ID, &o.SceneID, &o.Label, &o.Summary, &leads, &raw, &created); err != nil {
			return nil, err
		}
		o.LeadsToScene = leads.String
		if raw.Valid && strings.TrimSpace(raw.String) != "" {
			var t QuestTransition
			if err := json.Unmarshal([]byte(raw.String), &t); err == nil && t.QuestID != "" {
				o.QuestTransition = &t
			}
		}
		o.CreatedAt = time.UnixMilli(created).UTC()
		out = append(out, o)
	}
	return out, rows.Err()
}

/* ---------- session plans ---------- */

const planCols = `session_id, campaign_id, act_id, goal, prep_notes, status, created_at, updated_at`

func scanPlan(row interface{ Scan(...any) error }) (*SessionPlan, error) {
	var (
		p              SessionPlan
		act            sql.NullString
		created, dated int64
	)
	if err := row.Scan(&p.SessionID, &p.CampaignID, &act, &p.Goal, &p.PrepNotes,
		&p.Status, &created, &dated); err != nil {
		return nil, err
	}
	p.ActID = act.String
	p.CreatedAt = time.UnixMilli(created).UTC()
	p.UpdatedAt = time.UnixMilli(dated).UTC()
	return &p, nil
}

// PutPlan writes a session's plan — the planned face of a game_sessions row,
// one per session, so this is an upsert. A nil status keeps the stored one
// (or 'planned' for a new row); a nil actID keeps the stored act.
func (s *Store) PutPlan(ctx context.Context, campaignID, sessionID, actID, goal, prepNotes string, status *string) (*SessionPlan, error) {
	if err := s.sessionInCampaign(ctx, sessionID, campaignID); err != nil {
		return nil, err
	}
	if actID != "" {
		if _, err := s.actInCampaign(ctx, actID, campaignID); err != nil {
			return nil, err
		}
	}
	existing, err := s.GetPlan(ctx, campaign.ScopeDM, campaignID, sessionID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	p := &SessionPlan{
		SessionID: sessionID, CampaignID: campaignID, ActID: actID,
		Goal: goal, PrepNotes: prepNotes, Status: PlanStatusPlanned,
	}
	if existing != nil {
		if actID == "" {
			p.ActID = existing.ActID
		}
		p.Status = existing.Status
		p.CreatedAt = existing.CreatedAt
	}
	if status != nil {
		st := strings.TrimSpace(*status)
		if !validPlanStatus[st] {
			return nil, fmt.Errorf("%w: plan status %q", ErrInvalid, st)
		}
		p.Status = st
	}
	now := time.Now().UTC()
	p.UpdatedAt = now
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO session_plans (session_id, campaign_id, act_id, goal, prep_notes, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (session_id) DO UPDATE SET
			act_id = excluded.act_id, goal = excluded.goal, prep_notes = excluded.prep_notes,
			status = excluded.status, updated_at = excluded.updated_at`,
		p.SessionID, p.CampaignID, nullString(p.ActID), p.Goal, p.PrepNotes, p.Status,
		p.CreatedAt.UnixMilli(), p.UpdatedAt.UnixMilli()); err != nil {
		return nil, fmt.Errorf("put plan: %w", err)
	}
	return p, nil
}

// GetPlan returns one session's plan. DM-scope reads only: prep notes are DM
// material. ErrNotFound when the session has no plan yet.
func (s *Store) GetPlan(ctx context.Context, scope campaign.Scope, campaignID, sessionID string) (*SessionPlan, error) {
	if err := requireDM(scope); err != nil {
		return nil, err
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT `+planCols+` FROM session_plans WHERE session_id = ? AND campaign_id = ?`, sessionID, campaignID)
	p, err := scanPlan(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: plan for session %s", ErrNotFound, sessionID)
	}
	return p, err
}

// ListPlans returns a campaign's session plans by session ordinal.
// DM-scope reads only.
func (s *Store) ListPlans(ctx context.Context, scope campaign.Scope, campaignID string) ([]SessionPlan, error) {
	if err := requireDM(scope); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.session_id, p.campaign_id, p.act_id, p.goal, p.prep_notes, p.status, p.created_at, p.updated_at
		  FROM session_plans p JOIN game_sessions gs ON gs.id = p.session_id
		 WHERE p.campaign_id = ? ORDER BY gs.ordinal`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("list plans: %w", err)
	}
	defer rows.Close()
	var out []SessionPlan
	for rows.Next() {
		p, err := scanPlan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// DeletePlan drops a session's plan.
func (s *Store) DeletePlan(ctx context.Context, campaignID, sessionID string) error {
	if err := s.sessionInCampaign(ctx, sessionID, campaignID); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM session_plans WHERE session_id = ?`, sessionID); err != nil {
		return fmt.Errorf("delete plan: %w", err)
	}
	return nil
}
