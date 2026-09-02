package campaign

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// seedFixture runs the full graph fixture for the link tests: named
// entities and facts to point quest links at.
func seedFixture(t *testing.T, s *Store) *Fixture {
	t.Helper()
	userIDs(t, s.db, "keeper")
	fx, err := Seed(context.Background(), s.db, "keeper", "")
	if err != nil {
		t.Fatalf("seed fixture: %v", err)
	}
	return fx
}

// The old plain-string machine shape is every quest on main today; it must
// parse, transition and re-serialise without losing a state (MAD-369).
func TestStateMachineParsesBothStateShapes(t *testing.T) {
	old := `{"initial":"a","states":["a","b","c"],"edges":[{"from":"a","to":"b"},{"from":"b","to":"c"}]}`
	m, err := ParseStateMachine(old)
	if err != nil {
		t.Fatalf("old shape must parse: %v", err)
	}
	if len(m.States) != 3 || m.States[0].Key != "a" || m.States[2].Key != "c" {
		t.Fatalf("old shape must become keyed states: %+v", m.States)
	}
	for _, st := range m.States {
		if st.Label != "" || st.Terminal != TerminalNone {
			t.Fatalf("plain strings carry no label or terminal: %+v", st)
		}
	}
	if !m.HasEdge("a", "b") || !m.IsTerminal("c") && m.Terminal("c") != TerminalNone {
		t.Fatalf("the parsed machine misreads its own edges or terminals")
	}
	// Round-trip: the keyed shape comes back with every state intact.
	out, err := m.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(out, `"key":"a"`) || !strings.Contains(out, `"key":"c"`) {
		t.Fatalf("marshal must write the new shape: %s", out)
	}
	again, err := ParseStateMachine(out)
	if err != nil {
		t.Fatalf("round-trip parse: %v", err)
	}
	if len(again.States) != len(m.States) {
		t.Fatalf("round-trip lost states: %d -> %d", len(m.States), len(again.States))
	}

	// The new shape with labels, terminals and edge requirements.
	rich := `{"initial":"start","states":[
		{"key":"start","label":"The hunt begins"},
		{"key":"won","label":"Victory","terminal":"success"},
		{"key":"lost","terminal":"failure"}],
		"edges":[{"from":"start","to":"won","label":"win","requires":["f1"]},
		         {"from":"start","to":"lost","label":"lose"}]}`
	r, err := ParseStateMachine(rich)
	if err != nil {
		t.Fatalf("rich shape must parse: %v", err)
	}
	if st, _ := r.State("won"); st.Label != "Victory" || st.Terminal != TerminalSuccess {
		t.Fatalf("state fields lost: %+v", st)
	}
	if e, _ := r.Edge("start", "won"); e.Label != "win" || len(e.Requires) != 1 || e.Requires[0] != "f1" {
		t.Fatalf("edge fields lost: %+v", e)
	}
	if !r.IsTerminal("lost") || r.IsTerminal("start") {
		t.Fatal("terminal markers misread")
	}

	// A bad terminal marker is refused.
	if _, err := ParseStateMachine(`{"initial":"a","states":[{"key":"a","terminal":"maybe"}]}`); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown terminal must be ErrInvalid, got %v", err)
	}
	// The API decodes straight into the type; both shapes must work there.
	var viaAPI StateMachine
	if err := json.Unmarshal([]byte(old), &viaAPI); err != nil || len(viaAPI.States) != 3 {
		t.Fatalf("handler-shape decode: %v %+v", err, viaAPI)
	}
}

// Editing a machine may not orphan history: a state or edge a recorded
// transition used must survive, and the refusal names the transition.
func TestUpdateQuestRefusesToOrphanHistory(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	c := seedCampaign(t, s)

	q, err := s.CreateQuest(ctx, c.ID, QuestInput{Name: "Q", Machine: StateMachine{
		Initial: "a",
		States:  States("a", "b", "c"),
		Edges:   []StateEdge{{From: "a", To: "b"}, {From: "b", To: "c"}},
	}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.TransitionQuest(ctx, c.ID, q.ID, "b", ""); err != nil {
		t.Fatalf("move: %v", err)
	}
	moves, _ := s.QuestTransitions(ctx, ScopeDM, c.ID, q.ID)
	if len(moves) != 1 {
		t.Fatalf("fixture: want one move, got %d", len(moves))
	}
	moveID := moves[0].ID

	// Removing a state the move left (the quest no longer sits in it).
	cut := StateMachine{Initial: "b", States: States("b", "c"), Edges: []StateEdge{{From: "b", To: "c"}}}
	_, err = s.UpdateQuest(ctx, c.ID, q.ID, QuestUpdate{Machine: &cut})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("removing a used state must be refused, got %v", err)
	}
	if !strings.Contains(err.Error(), moveID) {
		t.Fatalf("the refusal must name the transition: %v", err)
	}

	// Keeping both states but cutting the edge between them.
	noEdge := StateMachine{Initial: "a", States: States("a", "b", "c"),
		Edges: []StateEdge{{From: "a", To: "c"}}}
	_, err = s.UpdateQuest(ctx, c.ID, q.ID, QuestUpdate{Machine: &noEdge})
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), moveID) {
		t.Fatalf("removing a used edge must be refused naming the move: %v", err)
	}

	// Removing the state the quest sits in, before any move names it.
	fresh, err := s.CreateQuest(ctx, c.ID, QuestInput{Name: "Q", Machine: StateMachine{
		Initial: "x", States: States("x", "y"), Edges: []StateEdge{{From: "x", To: "y"}},
	}})
	if err != nil {
		t.Fatalf("create fresh: %v", err)
	}
	drop := StateMachine{Initial: "y", States: States("y")}
	if _, err := s.UpdateQuest(ctx, c.ID, fresh.ID, QuestUpdate{Machine: &drop}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("removing the occupied state must be refused, got %v", err)
	}

	// A machine that keeps the history passes — adding a branch is fine.
	grown := StateMachine{Initial: "a", States: States("a", "b", "c", "d"),
		Edges: []StateEdge{{From: "a", To: "b"}, {From: "b", To: "c"}, {From: "b", To: "d"}}}
	updated, err := s.UpdateQuest(ctx, c.ID, q.ID, QuestUpdate{Machine: &grown})
	if err != nil {
		t.Fatalf("a history-preserving edit must pass: %v", err)
	}
	if !updated.Machine.HasEdge("b", "d") {
		t.Fatal("the new machine did not land")
	}
}

func TestUpdateQuestPatchesColumns(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	c := seedCampaign(t, s)
	q, err := s.CreateQuest(ctx, c.ID, QuestInput{Name: "Q", Summary: "first", Machine: StateMachine{Initial: "a", States: States("a")}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if q.Visibility != QuestVisibilitySecret || q.Status != QuestActive {
		t.Fatalf("defaults: visibility %q status %q", q.Visibility, q.Status)
	}
	str := func(v string) *string { return &v }
	questID := q.ID
	q, err = s.UpdateQuest(ctx, c.ID, questID, QuestUpdate{
		Name: str("Renamed"), Summary: str("second"),
		Visibility: str(QuestVisibilityPublic), Status: str(QuestComplete),
	})
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	if q.Name != "Renamed" || q.Summary != "second" || q.Visibility != QuestVisibilityPublic || q.Status != QuestComplete {
		t.Fatalf("patch did not land: %+v", q)
	}
	if _, err = s.UpdateQuest(ctx, c.ID, questID, QuestUpdate{Status: str("paused")}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown status must be refused, got %v", err)
	}
	if _, err = s.UpdateQuest(ctx, c.ID, questID, QuestUpdate{Visibility: str("hidden")}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown visibility must be refused, got %v", err)
	}
	if _, err := s.UpdateQuest(ctx, c.ID, "missing", QuestUpdate{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing quest: %v", err)
	}
}

func TestDeleteQuestSoftAbandons(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	c := seedCampaign(t, s)
	q, err := s.CreateQuest(ctx, c.ID, QuestInput{Name: "Q", Machine: StateMachine{
		Initial: "a", States: States("a", "b"), Edges: []StateEdge{{From: "a", To: "b"}},
	}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.TransitionQuest(ctx, c.ID, q.ID, "b", ""); err != nil {
		t.Fatalf("move: %v", err)
	}
	gone, err := s.DeleteQuest(ctx, c.ID, q.ID)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if gone.Status != QuestAbandoned {
		t.Fatalf("delete must abandon, got %q", gone.Status)
	}
	// Everything survives.
	kept, err := s.GetQuest(ctx, ScopeDM, c.ID, q.ID)
	if err != nil || kept.Status != QuestAbandoned || kept.CurrentState != "b" {
		t.Fatalf("the abandoned quest must survive whole: %+v %v", kept, err)
	}
	if moves, _ := s.QuestTransitions(ctx, ScopeDM, c.ID, q.ID); len(moves) != 1 {
		t.Fatalf("transitions must survive the soft delete: %d", len(moves))
	}
	// An abandoned quest does not move.
	if _, err := s.TransitionQuest(ctx, c.ID, q.ID, "a", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an abandoned quest must not move, got %v", err)
	}
}

func TestQuestEntityLinks(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	fx := seedFixture(t, s)
	cid := fx.Campaign.ID

	q, err := s.CreateQuest(ctx, cid, QuestInput{Name: "The Missing Miners", Machine: StateMachine{
		Initial: "unknown", States: States("unknown", "found"), Edges: []StateEdge{{From: "unknown", To: "found"}},
	}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.AddQuestEntity(ctx, cid, q.ID, fx.Mira, QuestRoleGiver); err != nil {
		t.Fatalf("link: %v", err)
	}
	if _, err := s.AddQuestEntity(ctx, cid, q.ID, fx.Mira, QuestRoleGiver); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate link must conflict, got %v", err)
	}
	if _, err := s.AddQuestEntity(ctx, cid, q.ID, fx.Mines, QuestRoleSite); err != nil {
		t.Fatalf("second role for the same entity is a distinct link: %v", err)
	}
	if _, err := s.AddQuestEntity(ctx, cid, q.ID, fx.Duke, "sidekick"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("off-vocabulary role must be refused, got %v", err)
	}
	if _, err := s.AddQuestEntity(ctx, cid, q.ID, "ghost", QuestRoleGiver); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign entity must be refused, got %v", err)
	}

	links, err := s.QuestEntities(ctx, ScopeDM, cid, q.ID)
	if err != nil || len(links) != 2 {
		t.Fatalf("want two links, got %d: %v", len(links), err)
	}
	// Removing one role leaves the other.
	if err := s.RemoveQuestEntity(ctx, cid, q.ID, fx.Mira, QuestRoleGiver); err != nil {
		t.Fatalf("remove one role: %v", err)
	}
	if links, _ = s.QuestEntities(ctx, ScopeDM, cid, q.ID); len(links) != 1 || links[0].Role != QuestRoleSite {
		t.Fatalf("the site link must survive: %+v", links)
	}
	// Removing with no role clears the entity entirely.
	if err := s.RemoveQuestEntity(ctx, cid, q.ID, fx.Mines, ""); err != nil {
		t.Fatalf("remove all roles: %v", err)
	}
	if links, _ = s.QuestEntities(ctx, ScopeDM, cid, q.ID); len(links) != 0 {
		t.Fatalf("the entity must be fully unlinked: %+v", links)
	}
	if err := s.RemoveQuestEntity(ctx, cid, q.ID, fx.Mines, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unlinking what is not linked must be not-found, got %v", err)
	}
}

func TestQuestStateFacts(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	fx := seedFixture(t, s)
	cid := fx.Campaign.ID

	q, err := s.CreateQuest(ctx, cid, QuestInput{Name: "Q", Machine: StateMachine{
		Initial: "unknown", States: States("unknown", "found"), Edges: []StateEdge{{From: "unknown", To: "found"}},
	}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.SetQuestStateFact(ctx, cid, q.ID, "found", fx.FactMinesOwned, QuestFactReveals); err != nil {
		t.Fatalf("set: %v", err)
	}
	if _, err := s.SetQuestStateFact(ctx, cid, q.ID, "found", fx.FactMinesOwned, QuestFactRequires); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("the same fact on the same state must conflict, got %v", err)
	}
	if _, err := s.SetQuestStateFact(ctx, cid, q.ID, "dreamed", fx.FactMinesOwned, QuestFactReveals); !errors.Is(err, ErrInvalid) {
		t.Fatalf("undeclared state must be refused, got %v", err)
	}
	if _, err := s.SetQuestStateFact(ctx, cid, q.ID, "found", "ghost", QuestFactReveals); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign fact must be refused, got %v", err)
	}
	rows, err := s.QuestStateFacts(ctx, ScopeDM, cid, q.ID)
	if err != nil || len(rows) != 1 || rows[0].Disposition != QuestFactReveals {
		t.Fatalf("want one reveals row: %+v %v", rows, err)
	}
}
