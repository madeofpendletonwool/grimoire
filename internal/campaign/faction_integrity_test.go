package campaign

import "testing"

// planView is the shorthand builder for faction plan views in the check
// tests below.
func planView(id, faction, current, status string, rate float64, lastAdvanced *int64, enemyReq bool) FactionPlanView {
	return FactionPlanView{
		ID: id, FactionEntity: faction, Name: "plan " + id, CurrentState: current,
		Status: status, RatePerDay: rate, LastAdvanced: lastAdvanced,
		HasEnemyRequirement: enemyReq,
		Machine: StateMachine{
			Initial: "mustering", States: []string{"mustering", "infiltrated", "bloomed"},
			Edges: []StateEdge{
				{From: "mustering", To: "infiltrated"}, {From: "infiltrated", To: "bloomed"},
			},
		},
	}
}

func TestCheckPlanIllegalState(t *testing.T) {
	snap := &Snapshot{CampaignID: "c1", FactionPlans: []FactionPlanView{
		planView("p1", "f1", "infiltrated", PlanActive, 1, nil, false), // sound
		planView("p2", "f2", "victorious", PlanActive, 1, nil, false),  // not declared
	}}
	// p3: a machine that no longer parses reads as empty and fires too.
	broken := planView("p3", "f3", "mustering", PlanActive, 1, nil, false)
	broken.Machine = StateMachine{}
	snap.FactionPlans = append(snap.FactionPlans, broken)

	got := checkPlanIllegalState(snap)
	if len(got) != 2 {
		t.Fatalf("want two findings, got %d: %+v", len(got), got)
	}
	for _, f := range got {
		if f.Check != CheckPlanIllegalState || f.Severity != SeverityError || f.RecordKind != "faction_plan" {
			t.Fatalf("wrong shape: %+v", f)
		}
	}
	if got[0].RecordID != "p2" && got[1].RecordID != "p2" {
		t.Fatalf("the undeclared state must be named: %+v", got)
	}
}

func TestCheckPlanWithoutFaction(t *testing.T) {
	day := int64(1)
	snap := &Snapshot{
		CampaignID: "c1",
		Entities: []Entity{
			{ID: "f1", CampaignID: "c1", Kind: KindFaction, Name: "Cult", Status: StatusActive},
			{ID: "f2", CampaignID: "c1", Kind: KindFaction, Name: "Dead Cult", Status: StatusDestroyed},
			{ID: "e3", CampaignID: "c1", Kind: KindLocation, Name: "Shrine", Status: StatusActive},
		},
		FactionPlans: []FactionPlanView{
			planView("p1", "f1", "mustering", PlanActive, 1, &day, false),    // sound
			planView("p2", "f2", "mustering", PlanActive, 1, &day, false),    // destroyed owner
			planView("p3", "e3", "mustering", PlanActive, 1, &day, false),    // wrong kind
			planView("p4", "ghost", "mustering", PlanActive, 1, &day, false), // missing owner
		},
	}
	got := checkPlanWithoutFaction(snap)
	if len(got) != 3 {
		t.Fatalf("want three findings, got %d: %+v", len(got), got)
	}
	for _, f := range got {
		if f.Check != CheckPlanWithoutFaction || f.Severity != SeverityError {
			t.Fatalf("wrong shape: %+v", f)
		}
	}
}

func TestCheckPlanStalled(t *testing.T) {
	day := int64(10)
	started := int64(5)
	cases := []struct {
		name string
		snap *Snapshot
		want int
	}{
		{
			name: "a month silent fires",
			snap: &Snapshot{CampaignID: "c1", Clock: 41, FactionPlans: []FactionPlanView{
				planView("p1", "f1", "mustering", PlanActive, 1, &day, false),
			}},
			want: 1,
		},
		{
			name: "just inside the window is clean",
			snap: &Snapshot{CampaignID: "c1", Clock: 39, FactionPlans: []FactionPlanView{
				planView("p1", "f1", "mustering", PlanActive, 1, &day, false),
			}},
			want: 0,
		},
		{
			name: "never advanced measures from started_day",
			snap: &Snapshot{CampaignID: "c1", Clock: 40, FactionPlans: []FactionPlanView{
				func() FactionPlanView {
					v := planView("p1", "f1", "mustering", PlanActive, 1, nil, false)
					v.StartedDay = &started
					return v
				}(),
			}},
			want: 1,
		},
		{
			name: "dormant plans do not stall",
			snap: &Snapshot{CampaignID: "c1", Clock: 400, FactionPlans: []FactionPlanView{
				planView("p1", "f1", "mustering", PlanDormant, 1, &day, false),
			}},
			want: 0,
		},
		{
			name: "a zero rate is not stalled, it is parked",
			snap: &Snapshot{CampaignID: "c1", Clock: 400, FactionPlans: []FactionPlanView{
				planView("p1", "f1", "mustering", PlanActive, 0, &day, false),
			}},
			want: 0,
		},
		{
			name: "no day to measure against is clean",
			snap: &Snapshot{CampaignID: "c1", Clock: 400, FactionPlans: []FactionPlanView{
				planView("p1", "f1", "mustering", PlanActive, 1, nil, false),
			}},
			want: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := checkPlanStalled(tc.snap)
			if len(got) != tc.want {
				t.Fatalf("want %d finding(s), got %d: %+v", tc.want, len(got), got)
			}
			for _, f := range got {
				if f.Check != CheckPlanStalled || f.Severity != SeverityWarn {
					t.Fatalf("wrong shape: %+v", f)
				}
			}
		})
	}
}

func TestCheckFactionNoAntagonist(t *testing.T) {
	day := int64(1)
	rel := func(from, to string) Relationship {
		return Relationship{ID: "r", FromEntity: from, RelType: "enemy_of", ToEntity: to}
	}
	cases := []struct {
		name string
		snap *Snapshot
		want int
	}{
		{
			name: "no enemy, no opposing precondition",
			snap: &Snapshot{CampaignID: "c1", FactionPlans: []FactionPlanView{
				planView("p1", "f1", "mustering", PlanActive, 1, &day, false),
			}},
			want: 1,
		},
		{
			name: "an enemy_of edge answers it",
			snap: &Snapshot{CampaignID: "c1",
				Relationships: []Relationship{rel("f2", "f1")},
				FactionPlans: []FactionPlanView{
					planView("p1", "f1", "mustering", PlanActive, 1, &day, false),
				}},
			want: 0,
		},
		{
			name: "an opposing precondition answers it",
			snap: &Snapshot{CampaignID: "c1", FactionPlans: []FactionPlanView{
				planView("p1", "f1", "mustering", PlanActive, 1, &day, true),
			}},
			want: 0,
		},
		{
			name: "dormant plans are not anybody's problem",
			snap: &Snapshot{CampaignID: "c1", FactionPlans: []FactionPlanView{
				planView("p1", "f1", "mustering", PlanDormant, 1, &day, false),
			}},
			want: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := checkFactionNoAntagonist(tc.snap)
			if len(got) != tc.want {
				t.Fatalf("want %d finding(s), got %d: %+v", tc.want, len(got), got)
			}
			for _, f := range got {
				if f.Check != CheckFactionNoAntagonist || f.Severity != SeverityInfo {
					t.Fatalf("wrong shape: %+v", f)
				}
			}
		})
	}
}

// TestRequiresJSONHasEnemy parses the shapes the loader meets.
func TestRequiresJSONHasEnemy(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{`[]`, false},
		{`[{"entity":"e1"}]`, false},
		{`[{"enemy_plan":{"state":"mobilized"}}]`, true},
		{`[{"entity":"e1"},{"enemy_plan":null}]`, false}, // an explicit null is absent
		{`not json`, false},
	}
	for _, tc := range cases {
		if got := requiresJSONHasEnemy(tc.raw); got != tc.want {
			t.Errorf("requiresJSONHasEnemy(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}
