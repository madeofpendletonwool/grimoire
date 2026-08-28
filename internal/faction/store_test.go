package faction

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/migrate"
	_ "modernc.org/sqlite"
)

// openDB opens a scratch database with the app's DSN shape and applies the
// migrations — the faction tables exist only through the runner.
func openDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "faction.db")
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

// fixture is the seeded graph the store tests start from: a campaign, two
// factions facing each other, a location the cult holds, and a live fact.
type fixture struct {
	campaignID                string
	cult, order, shrine, tomb string
}

func seedFixture(t *testing.T, s *Store) fixture {
	t.Helper()
	ctx := context.Background()
	if _, err := s.db.Exec(
		`INSERT INTO users (id, username, password_hash, is_admin, created_at) VALUES ('keeper', 'keeper', 'x', 0, 0)`); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	camps, _ := campaign.New(s.db)
	c, err := camps.CreateCampaign(ctx, "keeper", "The Withering Kingdom", "dnd5e", "A premise.")
	if err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	mk := func(kind, name string) string {
		e, err := camps.CreateEntity(ctx, c.ID, kind, name, "", nil)
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return e.ID
	}
	fx := fixture{
		campaignID: c.ID,
		cult:       mk(campaign.KindFaction, "Cult of the Root"),
		order:      mk(campaign.KindFaction, "The Greywatch Order"),
		shrine:     mk(campaign.KindLocation, "The Northern Shrine"),
		tomb:       mk(campaign.KindLocation, "The Old Tomb"),
	}
	if _, err := camps.CreateRelationship(ctx, c.ID, fx.cult, "owns", fx.shrine, 0, "", ""); err != nil {
		t.Fatalf("seed owns edge: %v", err)
	}
	if _, err := camps.CreateRelationship(ctx, c.ID, fx.order, "enemy_of", fx.cult, 0, "", ""); err != nil {
		t.Fatalf("seed enmity: %v", err)
	}
	if _, err := camps.CreateFact(ctx, c.ID, fx.cult, "seeks", "", "the Verdant God's return",
		"The cult seeks the Verdant God's return.",
		campaign.ConfidenceCanon, campaign.VisibilityPublic, "keeper",
		[]campaign.ProvenanceInput{{Method: campaign.MethodDMAuthored, Quote: "the roots remember"}}); err != nil {
		t.Fatalf("seed fact: %v", err)
	}
	return fx
}

// planInput is the two-step plan the store tests advance: infiltrate then
// bloom, each costing 10, rate 1/day.
func planInput(fx fixture) PlanInput {
	return PlanInput{
		Name: "The Root Takes Hold",
		Machine: campaign.StateMachine{
			Initial: "mustering",
			States:  []string{"mustering", "infiltrated", "bloomed"},
			Edges: []campaign.StateEdge{
				{From: "mustering", To: "infiltrated"},
				{From: "infiltrated", To: "bloomed"},
			},
		},
		Steps: []Step{
			{State: "infiltrated", Name: "Infiltrate the mines", Cost: 10},
			{State: "bloomed", Name: "Bloom beneath Blackwater", Cost: 10},
		},
		RatePerDay: 1,
	}
}

func newStore(t *testing.T) (*Store, fixture) {
	t.Helper()
	s, err := New(openDB(t))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	return s, seedFixture(t, s)
}

func mustCreatePlan(t *testing.T, s *Store, fx fixture, in PlanInput) *Plan {
	t.Helper()
	p, err := s.CreatePlan(context.Background(), fx.campaignID, fx.cult, in)
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	return p
}

func mustActivate(t *testing.T, s *Store, fx fixture, p *Plan) *Plan {
	t.Helper()
	status := PlanActive
	p, err := s.UpdatePlan(context.Background(), fx.campaignID, p.ID, UpdatePlanInput{Status: &status})
	if err != nil {
		t.Fatalf("activate plan: %v", err)
	}
	return p
}

func TestCreatePlanValidatesOwner(t *testing.T) {
	s, fx := newStore(t)
	ctx := context.Background()

	if _, err := s.CreatePlan(ctx, fx.campaignID, fx.shrine, planInput(fx)); !errors.Is(err, campaign.ErrInvalid) {
		t.Fatalf("a location cannot own a plan: %v", err)
	}
	if _, err := s.CreatePlan(ctx, fx.campaignID, "missing-entity", planInput(fx)); !errors.Is(err, campaign.ErrNotFound) {
		t.Fatalf("a missing owner reads as not found: %v", err)
	}
	in := planInput(fx)
	in.Machine.Initial = "unheard_of"
	if _, err := s.CreatePlan(ctx, fx.campaignID, fx.cult, in); !errors.Is(err, campaign.ErrInvalid) {
		t.Fatalf("an invalid machine is rejected: %v", err)
	}
	in = planInput(fx)
	in.Steps = nil
	if _, err := s.CreatePlan(ctx, fx.campaignID, fx.cult, in); !errors.Is(err, campaign.ErrInvalid) {
		t.Fatalf("a stepless plan is rejected: %v", err)
	}
	// A destroyed faction cannot take new plans.
	destroyed := campaign.StatusDestroyed
	if _, err := s.camps.UpdateEntity(ctx, fx.campaignID, fx.order, nil, nil, &destroyed, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreatePlan(ctx, fx.campaignID, fx.order, planInput(fx)); !errors.Is(err, campaign.ErrInvalid) {
		t.Fatalf("a destroyed faction cannot own a plan: %v", err)
	}
}

func TestPlanReadsAreDMOnly(t *testing.T) {
	s, fx := newStore(t)
	ctx := context.Background()
	p := mustCreatePlan(t, s, fx, planInput(fx))
	for _, scope := range []campaign.Scope{campaign.ScopeParty, campaign.ScopeCharacter("pc1"), campaign.ScopeNPC("npc1")} {
		if _, err := s.GetPlan(ctx, scope, fx.campaignID, p.ID); !errors.Is(err, campaign.ErrScope) {
			t.Fatalf("GetPlan at %s must refuse: %v", scope, err)
		}
		if _, err := s.ListPlans(ctx, scope, fx.campaignID); !errors.Is(err, campaign.ErrScope) {
			t.Fatalf("ListPlans at %s must refuse: %v", scope, err)
		}
	}
	if _, err := s.GetPlan(ctx, campaign.ScopeDM, fx.campaignID, p.ID); err != nil {
		t.Fatalf("DM reads the plan: %v", err)
	}
}

func TestTransitionPlanEnforcesMachineEdges(t *testing.T) {
	s, fx := newStore(t)
	ctx := context.Background()
	p := mustCreatePlan(t, s, fx, planInput(fx))

	if _, err := s.TransitionPlan(ctx, fx.campaignID, p.ID, "bloomed", "", ""); !errors.Is(err, campaign.ErrInvalid) {
		t.Fatalf("mustering -> bloomed is not an edge: %v", err)
	}
	moved, err := s.TransitionPlan(ctx, fx.campaignID, p.ID, "infiltrated", "", "the DM moved it by hand")
	if err != nil {
		t.Fatalf("legal move: %v", err)
	}
	if moved.CurrentState != "infiltrated" || moved.Progress != 0 {
		t.Fatalf("manual move lands clean: %+v", moved)
	}
	transitions, err := s.PlanTransitions(ctx, campaign.ScopeDM, fx.campaignID, p.ID)
	if err != nil || len(transitions) != 1 || transitions[0].Reason != "the DM moved it by hand" {
		t.Fatalf("the move is on the ledger: %v %+v", err, transitions)
	}
	// Reaching the last step completes the plan.
	done, err := s.TransitionPlan(ctx, fx.campaignID, p.ID, "bloomed", "", "")
	if err != nil {
		t.Fatalf("final move: %v", err)
	}
	if done.Status != PlanComplete {
		t.Fatalf("entering the last step completes the plan, got %q", done.Status)
	}
}

func TestAdvancePlanPersistsCarryAndLedger(t *testing.T) {
	s, fx := newStore(t)
	ctx := context.Background()
	p := mustActivate(t, s, fx, mustCreatePlan(t, s, fx, planInput(fx)))

	// Five days: partial payment, no move.
	got, pr, err := s.AdvancePlan(ctx, fx.campaignID, p.ID, 5)
	if err != nil {
		t.Fatal(err)
	}
	if got.CurrentState != "mustering" || got.Progress != 5 || len(pr.Moves) != 0 {
		t.Fatalf("partial advance: %+v %+v", got, pr)
	}

	// Ten more: the first step enters with a five-day carry.
	got, pr, err = s.AdvancePlan(ctx, fx.campaignID, p.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got.CurrentState != "infiltrated" || got.Progress != 5 || len(pr.Moves) != 1 {
		t.Fatalf("carrying advance: %+v %+v", got, pr)
	}
	ledger, err := s.PlanTransitions(ctx, campaign.ScopeDM, fx.campaignID, p.ID)
	if err != nil || len(ledger) != 1 {
		t.Fatalf("one ledger row per entered state: %v %+v", err, ledger)
	}
	if ledger[0].Reason == "" {
		t.Fatal("the ledger row must carry the arithmetic that caused it")
	}
	if got.LastAdvanced == nil {
		t.Fatal("the advance stamps last_advanced_day")
	}
}

func TestAdvancePlanDerivesModifiersFromGraph(t *testing.T) {
	s, fx := newStore(t)
	ctx := context.Background()

	t.Run("a lost owns edge stalls the step that needs it", func(t *testing.T) {
		in := planInput(fx)
		in.Steps[0].Requires = []Requirement{{
			Label:    "northern shrine",
			Edge:     &EdgeReq{From: fx.cult, Type: "owns", To: fx.shrine},
			IfBroken: &Reaction{Factor: 0, Reason: "without the shrine the rites cannot proceed"},
		}}
		p := mustActivate(t, s, fx, mustCreatePlan(t, s, fx, in))
		if err := s.camps.DeleteRelationship(ctx, fx.campaignID,
			relID(t, s, fx.cult, "owns", fx.shrine)); err != nil {
			t.Fatal(err)
		}
		got, pr, err := s.AdvancePlan(ctx, fx.campaignID, p.ID, 20)
		if err != nil {
			t.Fatal(err)
		}
		if got.Progress != 0 || pr.Gain != 0 || got.CurrentState != "mustering" {
			t.Fatalf("the stall applies: %+v %+v", got, pr)
		}
		if len(pr.Terms) != 2 || pr.Terms[1].Label != "northern shrine" {
			t.Fatalf("the terms name the stall: %+v", pr.Terms)
		}
	})

	t.Run("a broken fact requirement reacts as authored", func(t *testing.T) {
		in := planInput(fx)
		in.Steps[0].Requires = []Requirement{{
			Label:    "the god still answers",
			Fact:     &FactReq{Subject: fx.cult, Predicate: "seeks", Object: "the Verdant God's return"},
			IfBroken: &Reaction{Factor: -1, Reason: "the silence undoes weeks of work"},
		}}
		p := mustActivate(t, s, fx, mustCreatePlan(t, s, fx, in))
		got, pr, err := s.AdvancePlan(ctx, fx.campaignID, p.ID, 5)
		if err != nil {
			t.Fatal(err)
		}
		if got.Progress != 5 || got.CurrentState != "mustering" || len(pr.Terms) != 1 {
			t.Fatalf("the fact holds, the reaction does not fire: %+v %+v", got, pr)
		}
	})

	t.Run("a destroyed precondition entity accelerates the plan", func(t *testing.T) {
		in := planInput(fx)
		in.Steps[0].Requires = []Requirement{{
			Label:    "the northern shrine stands",
			Entity:   fx.tomb, // an entity that exists but is not the shrine; destroyed below
			IfBroken: &Reaction{Factor: 2, Reason: "the martyrdom inflames the cells"},
		}}
		p := mustActivate(t, s, fx, mustCreatePlan(t, s, fx, in))
		destroyed := campaign.StatusDestroyed
		if _, err := s.camps.UpdateEntity(ctx, fx.campaignID, fx.tomb, nil, nil, &destroyed, nil); err != nil {
			t.Fatal(err)
		}
		_, pr, err := s.AdvancePlan(ctx, fx.campaignID, p.ID, 10)
		if err != nil {
			t.Fatal(err)
		}
		if pr.Gain != 20 {
			t.Fatalf("the authored acceleration applies: %+v", pr)
		}
	})

	t.Run("an enemy plan reaching the named state reacts as authored", func(t *testing.T) {
		in := planInput(fx)
		in.Steps[0].Requires = []Requirement{{
			Label:     "the order has not mobilized",
			EnemyPlan: &EnemyReq{State: "mobilized"},
			IfBroken:  &Reaction{Factor: 0.5, Reason: "they are coming; rush the work"},
		}}
		p := mustActivate(t, s, fx, mustCreatePlan(t, s, fx, in))

		// The Greywatch Order's own plan sits at "mobilized".
		orderIn := PlanInput{
			Name: "The Greywatch Mobilizes",
			Machine: campaign.StateMachine{
				Initial: "watching",
				States:  []string{"watching", "mobilized"},
				Edges:   []campaign.StateEdge{{From: "watching", To: "mobilized"}},
			},
			Steps:      []Step{{State: "mobilized", Name: "Mobilize", Cost: 5}},
			RatePerDay: 1,
		}
		orderPlan, err := s.CreatePlan(ctx, fx.campaignID, fx.order, orderIn)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.TransitionPlan(ctx, fx.campaignID, orderPlan.ID, "mobilized", "", ""); err != nil {
			t.Fatal(err)
		}

		_, pr, err := s.AdvancePlan(ctx, fx.campaignID, p.ID, 10)
		if err != nil {
			t.Fatal(err)
		}
		if pr.Gain != 5 {
			t.Fatalf("the enemy's progress halves the cult's: %+v", pr)
		}
		statuses, err := s.RequirementStatuses(ctx, p)
		if err != nil || len(statuses) != 1 || statuses[0].Met {
			t.Fatalf("the status explains itself: %v %+v", err, statuses)
		}
	})
}

func TestAdvancePlanOnDormantPlanIsANoop(t *testing.T) {
	s, fx := newStore(t)
	ctx := context.Background()
	p := mustCreatePlan(t, s, fx, planInput(fx)) // dormant
	got, _, err := s.AdvancePlan(ctx, fx.campaignID, p.ID, 30)
	if err != nil {
		t.Fatal(err)
	}
	if got.Progress != 0 || got.CurrentState != "mustering" {
		t.Fatalf("a dormant plan does not move: %+v", got)
	}
	if ledger, _ := s.PlanTransitions(ctx, campaign.ScopeDM, fx.campaignID, p.ID); len(ledger) != 0 {
		t.Fatalf("a dormant plan writes no ledger rows: %+v", ledger)
	}
}

func TestUpdatePlanActivatingStampsStartedDay(t *testing.T) {
	s, fx := newStore(t)
	ctx := context.Background()
	p := mustCreatePlan(t, s, fx, planInput(fx))
	if p.StartedDay != nil {
		t.Fatal("a fresh plan has no started day")
	}
	active := mustActivate(t, s, fx, p)
	if active.StartedDay == nil || *active.StartedDay != 0 {
		t.Fatalf("activation stamps the campaign clock day: %+v", active.StartedDay)
	}
	rate := 2.5
	patched, err := s.UpdatePlan(ctx, fx.campaignID, p.ID, UpdatePlanInput{RatePerDay: &rate})
	if err != nil || patched.RatePerDay != 2.5 {
		t.Fatalf("rate patches: %v %+v", err, patched)
	}
	bad := "triumphant"
	if _, err := s.UpdatePlan(ctx, fx.campaignID, p.ID, UpdatePlanInput{Status: &bad}); !errors.Is(err, campaign.ErrInvalid) {
		t.Fatalf("unknown status rejected: %v", err)
	}
}

// relID finds one edge's row id, for deleting it.
func relID(t *testing.T, s *Store, from, relType, to string) string {
	t.Helper()
	var id string
	if err := s.db.QueryRow(
		`SELECT id FROM relationships WHERE from_entity = ? AND rel_type = ? AND to_entity = ?`,
		from, relType, to).Scan(&id); err != nil {
		t.Fatalf("find edge: %v", err)
	}
	return id
}
