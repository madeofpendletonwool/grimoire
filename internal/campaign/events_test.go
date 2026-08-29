package campaign

import (
	"context"
	"errors"
	"testing"
)

func TestEventDualOrdering(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	c := seedCampaign(t, s)

	day := func(v int64) *int64 { return &v }
	// Play order: ambush first, then a flashback the DM narrated after.
	ambush, err := s.CreateEvent(ctx, c.ID, "", "Caravan found wrecked.", day(34), "")
	if err != nil {
		t.Fatalf("create ambush: %v", err)
	}
	flash, err := s.CreateEvent(ctx, c.ID, "", "Flashback: miners marched away.", day(31), "")
	if err != nil {
		t.Fatalf("create flashback: %v", err)
	}
	undated, err := s.CreateEvent(ctx, c.ID, "", "Somewhere, a bell rings.", nil, "")
	if err != nil {
		t.Fatalf("create undated: %v", err)
	}
	if ambush.RealOrdinal != 1 || flash.RealOrdinal != 2 || undated.RealOrdinal != 3 {
		t.Fatalf("play order must be assigned monotonically: %d %d %d",
			ambush.RealOrdinal, flash.RealOrdinal, undated.RealOrdinal)
	}

	// The listing is play order even though the in-world dates disagree.
	timeline, err := s.ListEvents(ctx, ScopeDM, c.ID)
	if err != nil || len(timeline) != 3 {
		t.Fatalf("timeline: %v %v", timeline, err)
	}
	if timeline[0].ID != ambush.ID || timeline[1].ID != flash.ID {
		t.Fatalf("timeline must follow real_ordinal: %+v", timeline)
	}
	if timeline[2].ClockAt != nil {
		t.Fatalf("undated event must keep a nil clock: %+v", timeline[2])
	}
	if got := flash.ClockAt; got == nil || *got != 31 {
		t.Fatalf("flashback keeps its in-world date: %v", got)
	}

	// Blank summaries are rejected.
	if _, err := s.CreateEvent(ctx, c.ID, "", "  ", nil, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("blank summary must be ErrInvalid, got %v", err)
	}
}

func TestEventParticipantsAndLinks(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	c := seedCampaign(t, s)

	party, _ := s.CreateEntity(ctx, c.ID, KindPC, "Thalia", "", nil)
	ambush, _ := s.CreateEvent(ctx, c.ID, "", "Ambush!", nil, "")
	flash, _ := s.CreateEvent(ctx, c.ID, "", "Flashback.", nil, "")

	if err := s.AddParticipant(ctx, c.ID, ambush.ID, party.ID, "party"); err != nil {
		t.Fatalf("add participant: %v", err)
	}
	if err := s.AddParticipant(ctx, c.ID, ambush.ID, party.ID, "witness"); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("repeat participant must be ErrAlreadyExists, got %v", err)
	}

	if err := s.LinkEvents(ctx, c.ID, flash.ID, ambush.ID, LinkCaused); err != nil {
		t.Fatalf("link: %v", err)
	}
	if err := s.LinkEvents(ctx, c.ID, ambush.ID, ambush.ID, LinkCaused); !errors.Is(err, ErrInvalid) {
		t.Fatalf("self link must be ErrInvalid, got %v", err)
	}
	if err := s.LinkEvents(ctx, c.ID, flash.ID, ambush.ID, LinkCaused); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate link must be ErrAlreadyExists, got %v", err)
	}
	if err := s.LinkEvents(ctx, c.ID, flash.ID, ambush.ID, "prevented"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("off-vocabulary link must be ErrInvalid, got %v", err)
	}

	got, err := s.GetEvent(ctx, ScopeDM, c.ID, ambush.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Participants) != 1 || got.Participants[0].EntityID != party.ID {
		t.Fatalf("participants: %+v", got.Participants)
	}
	if len(got.Links) != 1 || got.Links[0].Link != LinkCaused {
		t.Fatalf("links: %+v", got.Links)
	}
}

func TestStateMachineValidationTable(t *testing.T) {
	cases := []struct {
		name    string
		machine StateMachine
		wantErr bool
	}{
		{
			name: "linear",
			machine: StateMachine{Initial: "a", States: States("a", "b"),
				Edges: []StateEdge{{From: "a", To: "b"}}},
		},
		{
			name: "branching",
			machine: StateMachine{Initial: "start", States: States("start", "left", "right", "done"),
				Edges: []StateEdge{{From: "start", To: "left"}, {From: "start", To: "right"},
					{From: "left", To: "done"}, {From: "right", To: "done"}}},
		},
		{
			name:    "no states",
			machine: StateMachine{Initial: "a"},
			wantErr: true,
		},
		{
			name:    "initial not declared",
			machine: StateMachine{Initial: "z", States: States("a")},
			wantErr: true,
		},
		{
			name:    "duplicate state",
			machine: StateMachine{Initial: "a", States: States("a", "a")},
			wantErr: true,
		},
		{
			name: "edge from undeclared state",
			machine: StateMachine{Initial: "a", States: States("a", "b"),
				Edges: []StateEdge{{From: "ghost", To: "b"}}},
			wantErr: true,
		},
		{
			name: "edge to undeclared state",
			machine: StateMachine{Initial: "a", States: States("a", "b"),
				Edges: []StateEdge{{From: "a", To: "ghost"}}},
			wantErr: true,
		},
		{
			name:    "blank state name",
			machine: StateMachine{Initial: "a", States: States("a", " ")},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.machine.Validate()
			if tc.wantErr && !errors.Is(err, ErrInvalid) {
				t.Fatalf("want ErrInvalid, got %v", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}

	if _, err := ParseStateMachine(`{"initial":"a","states":["a","b"],"edges":[{"from":"a","to":"b"}]}`); err != nil {
		t.Fatalf("valid JSON machine should parse: %v", err)
	}
	if _, err := ParseStateMachine(`not json`); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid JSON must be ErrInvalid, got %v", err)
	}
}

func TestQuestTransitionsFollowTheMachine(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	c := seedCampaign(t, s)

	machine := StateMachine{
		Initial: "unknown",
		States:  States("unknown", "found", "trusted", "accused", "done"),
		Edges: []StateEdge{
			{From: "unknown", To: "found"},
			{From: "found", To: "trusted"},
			{From: "found", To: "accused"},
			{From: "trusted", To: "done"},
			{From: "accused", To: "done"},
		},
	}
	q, err := s.CreateQuest(ctx, c.ID, QuestInput{Name: "The Missing Miners", Machine: machine})
	if err != nil {
		t.Fatalf("create quest: %v", err)
	}
	if q.CurrentState != "unknown" {
		t.Fatalf("quest starts at the initial state: %s", q.CurrentState)
	}

	cause, _ := s.CreateEvent(ctx, c.ID, "", "Wreckage found.", nil, "")
	if _, err := s.TransitionQuest(ctx, c.ID, q.ID, "done", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("skipping states must be ErrInvalid, got %v", err)
	}
	if _, err := s.TransitionQuest(ctx, c.ID, q.ID, "found", cause.ID); err != nil {
		t.Fatalf("legal move: %v", err)
	}
	if _, err := s.TransitionQuest(ctx, c.ID, q.ID, "trusted", ""); err != nil {
		t.Fatalf("branch move: %v", err)
	}
	if _, err := s.TransitionQuest(ctx, c.ID, q.ID, "accused", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("the branch not taken is not reachable, got %v", err)
	}

	moves, err := s.QuestTransitions(ctx, ScopeDM, c.ID, q.ID)
	if err != nil || len(moves) != 2 {
		t.Fatalf("want two recorded moves: %v %v", moves, err)
	}
	if moves[0].FromState != "unknown" || moves[0].ToState != "found" || moves[0].EventID != cause.ID {
		t.Fatalf("move must record from, to and the causing event: %+v", moves[0])
	}

	got, _ := s.GetQuest(ctx, ScopeDM, c.ID, q.ID)
	if got.CurrentState != "trusted" {
		t.Fatalf("current state must follow the moves: %s", got.CurrentState)
	}

	// A quest whose machine a DM broke in the database should fail loudly on
	// read rather than serving garbage transitions.
	if _, err := s.db.Exec(`UPDATE quests SET state_machine = '{"initial":"x","states":[],"edges":[]}' WHERE id = ?`, q.ID); err != nil {
		t.Fatalf("break machine: %v", err)
	}
	if _, err := s.GetQuest(ctx, ScopeDM, c.ID, q.ID); err == nil {
		t.Fatal("reading a quest with a broken machine must fail")
	}
}
