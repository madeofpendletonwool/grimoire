package story_test

// Store tests over a real migrated database, on the campaign seed fixture
// (entities, facts, a quest with a branching machine, sessions). The
// load-bearing cases: the vocabularies the CHECK constraints mirror reject
// cleanly through the store; an outcome naming a quest edge the machine does
// not have is refused at write time; reads refuse every scope but the DM's.

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/migrate"
	"github.com/madeofpendletonwool/grimoire/internal/story"
	_ "modernc.org/sqlite" // same pure-Go driver the app opens the real file with
)

// openStoryDB opens a scratch database the way the app does and applies the
// migrations — the spine tables exist only through the runner.
func openStoryDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "story.db")
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(wal)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if err := migrate.Up(db); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	return db
}

// seededStory boots the store over the campaign seed and one session.
type storyEnv struct {
	store *story.Store
	fx    *campaign.Fixture
	sess1 string
}

func seededStory(t *testing.T) *storyEnv {
	t.Helper()
	db := openStoryDB(t)
	ctx := context.Background()
	if _, err := db.Exec(
		`INSERT INTO users (id, username, password_hash, is_admin, created_at) VALUES ('keeper', 'keeper', 'x', 0, 0)`); err != nil {
		t.Fatalf("insert owner: %v", err)
	}
	fx, err := campaign.Seed(ctx, db, "keeper", "")
	if err != nil {
		t.Fatalf("campaign seed: %v", err)
	}
	var sid string
	if err := db.QueryRow(`
		INSERT INTO game_sessions (id, campaign_id, ordinal, name, status, created_at, updated_at)
		VALUES ('sess-1', ?, 1, 'Session 1', 'planned', 0, 0)
		RETURNING id`, fx.Campaign.ID).Scan(&sid); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	s, err := story.New(db)
	if err != nil {
		t.Fatalf("story store: %v", err)
	}
	return &storyEnv{store: s, fx: fx, sess1: sid}
}

/* ---------- acts ---------- */

func TestActCRUDAndOrdering(t *testing.T) {
	env := seededStory(t)
	ctx := context.Background()
	cid := env.fx.Campaign.ID

	first, err := env.store.CreateAct(ctx, cid, "The Letter", "A summons nobody wanted.", 1, 4)
	if err != nil {
		t.Fatalf("create act: %v", err)
	}
	second, err := env.store.CreateAct(ctx, cid, "The March", "", 5, 10)
	if err != nil {
		t.Fatalf("create act: %v", err)
	}
	if first.Ordinal != 1 || second.Ordinal != 2 {
		t.Errorf("ordinals %d/%d, want 1/2 (max+1 within the campaign)", first.Ordinal, second.Ordinal)
	}

	acts, err := env.store.ListActs(ctx, campaign.ScopeDM, cid)
	if err != nil {
		t.Fatalf("list acts: %v", err)
	}
	if len(acts) != 2 || acts[0].ID != first.ID {
		t.Fatalf("list acts: got %+v", acts)
	}

	status := "active"
	updated, err := env.store.UpdateAct(ctx, cid, second.ID, nil, nil, nil, nil, &status)
	if err != nil {
		t.Fatalf("update act: %v", err)
	}
	if updated.Status != "active" {
		t.Errorf("status %q, want active", updated.Status)
	}

	if err := env.store.DeleteAct(ctx, cid, first.ID); err != nil {
		t.Fatalf("delete act: %v", err)
	}
	if _, err := env.store.GetAct(ctx, campaign.ScopeDM, cid, first.ID); err == nil {
		t.Error("a deleted act must not resolve")
	}
}

func TestActValidation(t *testing.T) {
	env := seededStory(t)
	ctx := context.Background()
	cid := env.fx.Campaign.ID
	if _, err := env.store.CreateAct(ctx, cid, "  ", "", 1, 4); err == nil {
		t.Error("an empty act name must be rejected")
	}
	if _, err := env.store.CreateAct(ctx, cid, "Inverted", "", 6, 2); err == nil {
		t.Error("an inverted level band must be rejected")
	}
	if _, err := env.store.CreateAct(ctx, cid, "Out of range", "", 0, 25); err == nil {
		t.Error("a band outside levels 1-20 must be rejected")
	}
	if _, err := env.store.CreateAct(ctx, "no-such-campaign", "Ghost", "", 1, 2); err == nil {
		t.Error("an act for a foreign campaign must be a 404, not a row")
	}
}

func TestActReadsRefuseNonDMScopes(t *testing.T) {
	env := seededStory(t)
	ctx := context.Background()
	cid := env.fx.Campaign.ID
	act, err := env.store.CreateAct(ctx, cid, "The Letter", "", 1, 4)
	if err != nil {
		t.Fatalf("create act: %v", err)
	}
	for _, scope := range []campaign.Scope{campaign.ScopeParty, campaign.ScopeCharacter(env.fx.Thalia), campaign.ScopeNPC(env.fx.Duke)} {
		if _, err := env.store.GetAct(ctx, scope, cid, act.ID); err == nil {
			t.Errorf("scope %s read an act; the spine is DM material", scope)
		}
		if _, err := env.store.ListActs(ctx, scope, cid); err == nil {
			t.Errorf("scope %s listed acts; the spine is DM material", scope)
		}
	}
}

/* ---------- scenes, cast, secrets, outcomes ---------- */

func TestSceneLifecycleWithCastSecretsAndOutcomes(t *testing.T) {
	env := seededStory(t)
	ctx := context.Background()
	cid := env.fx.Campaign.ID

	act, err := env.store.CreateAct(ctx, cid, "The Letter", "", 1, 4)
	if err != nil {
		t.Fatalf("create act: %v", err)
	}
	sc, err := env.store.CreateScene(ctx, cid, act.ID, env.sess1, "social",
		"The Waystone at midnight", "Put Tom and the Duke's man in one room.", env.fx.Blackwater)
	if err != nil {
		t.Fatalf("create scene: %v", err)
	}
	if sc.Ordinal != 1 {
		t.Errorf("scene ordinal %d, want 1", sc.Ordinal)
	}

	if _, err := env.store.AddCast(ctx, cid, sc.ID, env.fx.Tom, "innkeeper"); err == nil {
		t.Error("a cast role outside the vocabulary must be rejected")
	}
	cast, err := env.store.AddCast(ctx, cid, sc.ID, env.fx.Tom, story.RoleFocus)
	if err != nil {
		t.Fatalf("add cast: %v", err)
	}
	if len(cast) != 1 || cast[0].EntityID != env.fx.Tom {
		t.Fatalf("cast: %+v", cast)
	}
	// Recasting is a role change, not an error.
	cast, err = env.store.AddCast(ctx, cid, sc.ID, env.fx.Tom, story.RoleMentioned)
	if err != nil || len(cast) != 1 || cast[0].Role != story.RoleMentioned {
		t.Fatalf("recast: %+v err %v", cast, err)
	}
	if _, err := env.store.AddCast(ctx, cid, sc.ID, "no-such-entity", story.RolePresent); err == nil {
		t.Error("cast for a foreign entity must be rejected")
	}

	// The seed's secret fact: the Silver Key opens the crypt.
	if _, err := env.store.SetSecret(ctx, cid, sc.ID, env.fx.FactKeyOpensCrypt, "maybe"); err == nil {
		t.Error("a disposition outside the vocabulary must be rejected")
	}
	secrets, err := env.store.SetSecret(ctx, cid, sc.ID, env.fx.FactKeyOpensCrypt, story.DispositionInPlay)
	if err != nil {
		t.Fatalf("set secret: %v", err)
	}
	if len(secrets) != 1 || secrets[0].Disposition != story.DispositionInPlay {
		t.Fatalf("secrets: %+v", secrets)
	}
	// Replanting moves, it does not duplicate.
	secrets, err = env.store.SetSecret(ctx, cid, sc.ID, env.fx.FactKeyOpensCrypt, story.DispositionWithheld)
	if err != nil || len(secrets) != 1 || secrets[0].Disposition != story.DispositionWithheld {
		t.Fatalf("replant: %+v err %v", secrets, err)
	}

	// A quest transition the seed's machine actually has.
	tLegal := &story.QuestTransition{QuestID: env.fx.QuestID, From: "survivors_found", To: "trusted_survivor"}
	outcomes, err := env.store.AddOutcome(ctx, cid, sc.ID, "A",
		"They take the survivor at their word.", "", tLegal)
	if err != nil {
		t.Fatalf("add outcome: %v", err)
	}
	if len(outcomes) != 1 || outcomes[0].QuestTransition == nil || outcomes[0].QuestTransition.To != "trusted_survivor" {
		t.Fatalf("outcome: %+v", outcomes)
	}
	// An edge the machine does not have is refused at write time.
	tIllegal := &story.QuestTransition{QuestID: env.fx.QuestID, From: "unknown", To: "cult_revealed"}
	if _, err := env.store.AddOutcome(ctx, cid, sc.ID, "B", "A shortcut the machine forbids.", "", tIllegal); err == nil {
		t.Error("an outcome promising a move the machine does not have must be refused")
	}

	// GetScene carries the whole scene.
	full, err := env.store.GetScene(ctx, campaign.ScopeDM, cid, sc.ID)
	if err != nil {
		t.Fatalf("get scene: %v", err)
	}
	if len(full.Cast) != 1 || len(full.Secrets) != 1 || len(full.Outcomes) != 1 {
		t.Errorf("full scene is missing attachments: %+v", full)
	}

	// Update and delete.
	name := "The Waystone, later"
	kind := "revelation"
	if _, err := env.store.UpdateScene(ctx, cid, sc.ID, nil, &kind, &name, nil, nil, nil); err != nil {
		t.Fatalf("update scene: %v", err)
	}
	if err := env.store.DeleteScene(ctx, cid, sc.ID); err != nil {
		t.Fatalf("delete scene: %v", err)
	}
	if _, err := env.store.GetScene(ctx, campaign.ScopeDM, cid, sc.ID); err == nil {
		t.Error("a deleted scene must not resolve")
	}
}

func TestSceneValidation(t *testing.T) {
	env := seededStory(t)
	ctx := context.Background()
	cid := env.fx.Campaign.ID
	act, err := env.store.CreateAct(ctx, cid, "The Letter", "", 1, 4)
	if err != nil {
		t.Fatalf("create act: %v", err)
	}
	if _, err := env.store.CreateScene(ctx, cid, "no-such-act", "", "social", "Orphan", "", ""); err == nil {
		t.Error("a scene for a foreign act must be rejected")
	}
	if _, err := env.store.CreateScene(ctx, cid, act.ID, "", "heist", "Bad kind", "", ""); err == nil {
		t.Error("a scene kind outside the vocabulary must be rejected")
	}
	if _, err := env.store.CreateScene(ctx, cid, act.ID, "no-such-session", "social", "Ghost seat", "", ""); err == nil {
		t.Error("a scene seated in a foreign session must be rejected")
	}
	if _, err := env.store.CreateScene(ctx, cid, act.ID, "", "social", "Nowhere", "", "no-such-entity"); err == nil {
		t.Error("a setting entity that does not exist must be rejected")
	}
}

func TestSceneReadsRefuseNonDMScopes(t *testing.T) {
	env := seededStory(t)
	ctx := context.Background()
	cid := env.fx.Campaign.ID
	act, _ := env.store.CreateAct(ctx, cid, "The Letter", "", 1, 4)
	sc, err := env.store.CreateScene(ctx, cid, act.ID, "", "social", "The Waystone", "", "")
	if err != nil {
		t.Fatalf("create scene: %v", err)
	}
	for _, scope := range []campaign.Scope{campaign.ScopeParty, campaign.ScopeCharacter(env.fx.Thalia)} {
		if _, err := env.store.GetScene(ctx, scope, cid, sc.ID); err == nil {
			t.Errorf("scope %s read a scene; the spine is DM material", scope)
		}
		if _, err := env.store.ListScenes(ctx, scope, cid, ""); err == nil {
			t.Errorf("scope %s listed scenes; the spine is DM material", scope)
		}
	}
}

/* ---------- session plans ---------- */

func TestPlanUpsertAndReads(t *testing.T) {
	env := seededStory(t)
	ctx := context.Background()
	cid := env.fx.Campaign.ID
	act, _ := env.store.CreateAct(ctx, cid, "The Letter", "", 1, 4)

	p, err := env.store.PutPlan(ctx, cid, env.sess1, act.ID, "Introduce the Waystone.", "Map of Blackwater.", nil)
	if err != nil {
		t.Fatalf("put plan: %v", err)
	}
	if p.Status != story.PlanStatusPlanned {
		t.Errorf("a new plan starts planned, got %q", p.Status)
	}

	// The upsert: same session, new goal, status carried forward unless set.
	ready := story.PlanStatusReady
	p, err = env.store.PutPlan(ctx, cid, env.sess1, "", "Introduce Tom and the letter.", "Map of Blackwater.", &ready)
	if err != nil {
		t.Fatalf("reput plan: %v", err)
	}
	if p.Goal != "Introduce Tom and the letter." {
		t.Errorf("goal %q, want the updated one", p.Goal)
	}
	if p.Status != story.PlanStatusReady {
		t.Errorf("status %q, want ready", p.Status)
	}
	if p.ActID != act.ID {
		t.Errorf("a nil act must keep the stored act, got %q", p.ActID)
	}

	// The reads a player may not make.
	for _, scope := range []campaign.Scope{campaign.ScopeParty, campaign.ScopeCharacter(env.fx.Thalia)} {
		if _, err := env.store.GetPlan(ctx, scope, cid, env.sess1); err == nil {
			t.Errorf("scope %s read a session plan; prep notes are DM material", scope)
		}
		if _, err := env.store.ListPlans(ctx, scope, cid); err == nil {
			t.Errorf("scope %s listed session plans; prep notes are DM material", scope)
		}
	}

	if err := env.store.DeletePlan(ctx, cid, env.sess1); err != nil {
		t.Fatalf("delete plan: %v", err)
	}
	if _, err := env.store.GetPlan(ctx, campaign.ScopeDM, cid, env.sess1); err == nil {
		t.Error("a deleted plan must not resolve")
	}
}

func TestPlanValidation(t *testing.T) {
	env := seededStory(t)
	ctx := context.Background()
	cid := env.fx.Campaign.ID
	if _, err := env.store.PutPlan(ctx, cid, "no-such-session", "", "", "", nil); err == nil {
		t.Error("a plan for a foreign session must be rejected")
	}
	bad := "half-ready"
	if _, err := env.store.PutPlan(ctx, cid, env.sess1, "", "", "", &bad); err == nil {
		t.Error("a plan status outside the vocabulary must be rejected")
	}
}
