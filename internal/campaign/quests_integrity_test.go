package campaign

import "testing"

/*
The quest graph checks (MAD-369). Every rule gets a test that fires it and a
test that does not — an untested consistency rule is worse than no rule,
because it is trusted (ADR 8). Snapshots are built in memory like the rest of
integrity_test.go: the checks are pure over the snapshot.
*/

func questSnapshot(machine StateMachine, current string) *Snapshot {
	return &Snapshot{
		CampaignID: "c1",
		Quests: []Quest{{ID: "q1", CampaignID: "c1", Name: "q", Status: QuestActive,
			CurrentState: current, Machine: machine}},
	}
}

func TestCheckQuestStateUnreachable(t *testing.T) {
	// Fires: "orphan" is declared but no edge reaches it.
	snap := questSnapshot(StateMachine{
		Initial: "a",
		States:  States("a", "b", "orphan"),
		Edges:   []StateEdge{{From: "a", To: "b"}},
	}, "a")
	got := checkQuestStateUnreachable(snap)
	if len(got) != 1 || got[0].Check != CheckQuestStateUnreachable || got[0].RecordID != "q1" ||
		got[0].Severity != SeverityWarn {
		t.Fatalf("want one warning on q1: %+v", got)
	}

	// Does not fire: every state reachable, including through a cycle.
	clean := questSnapshot(StateMachine{
		Initial: "a",
		States:  States("a", "b", "c"),
		Edges:   []StateEdge{{From: "a", To: "b"}, {From: "b", To: "c"}, {From: "c", To: "a"}},
	}, "b")
	if got := checkQuestStateUnreachable(clean); len(got) != 0 {
		t.Fatalf("a fully reachable machine must not fire: %+v", got)
	}
}

func TestCheckQuestDeadEnd(t *testing.T) {
	// Fires: a passing state with no way out.
	snap := questSnapshot(StateMachine{
		Initial: "a",
		States:  States("a", "trapped"),
		Edges:   []StateEdge{{From: "a", To: "trapped"}},
	}, "trapped")
	got := checkQuestDeadEnd(snap)
	if len(got) != 1 || got[0].Check != CheckQuestDeadEnd || got[0].Severity != SeverityWarn {
		t.Fatalf("want one warning: %+v", got)
	}

	// Does not fire: endings are allowed to close, passing states move on.
	clean := questSnapshot(StateMachine{
		Initial: "a",
		States: []State{
			{Key: "a"},
			{Key: "mid"},
			{Key: "won", Terminal: TerminalSuccess},
			{Key: "lost", Terminal: TerminalFailure},
		},
		Edges: []StateEdge{{From: "a", To: "mid"}, {From: "mid", To: "won"}, {From: "mid", To: "lost"}},
	}, "mid")
	if got := checkQuestDeadEnd(clean); len(got) != 0 {
		t.Fatalf("terminal states with no outgoing edge must not fire: %+v", got)
	}
}

func TestCheckQuestNoEnding(t *testing.T) {
	machine := StateMachine{
		Initial: "a",
		States: append(States("a", "b", "c"),
			State{Key: "won", Terminal: TerminalSuccess}),
		Edges: []StateEdge{{From: "a", To: "b"}, {From: "b", To: "c"}, {From: "c", To: "won"}},
	}
	// Fires: the quest sits in a branch that can no longer conclude.
	dead := StateMachine{
		Initial: "a",
		States:  States("a", "b", "c"),
		Edges:   []StateEdge{{From: "a", To: "b"}, {From: "b", To: "c"}},
	}
	snap := questSnapshot(dead, "c")
	got := checkQuestNoEnding(snap)
	if len(got) != 1 || got[0].Check != CheckQuestNoEnding || got[0].Severity != SeverityWarn {
		t.Fatalf("want one warning: %+v", got)
	}

	// Does not fire: an ending is reachable from where the quest sits.
	if got := checkQuestNoEnding(questSnapshot(machine, "b")); len(got) != 0 {
		t.Fatalf("a reachable ending must not fire: %+v", got)
	}
	// Does not fire: the quest is already over and owes no ending.
	finished := questSnapshot(dead, "c")
	finished.Quests[0].Status = QuestComplete
	if got := checkQuestNoEnding(finished); len(got) != 0 {
		t.Fatalf("a finished quest must not fire: %+v", got)
	}
}

func TestCheckQuestTransitionUngrounded(t *testing.T) {
	machine := func(requires ...string) StateMachine {
		return StateMachine{
			Initial: "a",
			States:  States("a", "b"),
			Edges:   []StateEdge{{From: "a", To: "b", Requires: requires}},
		}
	}
	move := QuestTransition{ID: "t1", QuestID: "q1", FromState: "a", ToState: "b"}
	pc := Entity{ID: "pc1", CampaignID: "c1", Kind: KindPC, Status: StatusActive}
	base := func(m StateMachine, aware ...AwarenessView) *Snapshot {
		return &Snapshot{
			CampaignID: "c1",
			Quests: []Quest{{ID: "q1", CampaignID: "c1", Name: "q", Status: QuestActive,
				CurrentState: "b", Machine: m}},
			QuestTransitions: []QuestTransition{move},
			Entities:         []Entity{pc},
			Awareness:        aware,
		}
	}

	// Fires: the edge requires a fact nobody holds.
	snap := base(machine("f1"))
	got := checkQuestTransitionUngrounded(snap)
	if len(got) != 1 || got[0].Check != CheckQuestTransitionUngrounded || got[0].RecordID != "t1" ||
		got[0].Severity != SeverityWarn {
		t.Fatalf("want one warning on the move: %+v", got)
	}

	// Does not fire: the party holds a granting stance.
	held := base(machine("f1"), AwarenessView{Knower: PartyKnower, FactID: "f1", Stance: "knows"})
	if got := checkQuestTransitionUngrounded(held); len(got) != 0 {
		t.Fatalf("a granted requirement must not fire: %+v", got)
	}
	// A pc's grant grounds it too; an npc's does not.
	pcHeld := base(machine("f1"), AwarenessView{Knower: "pc1", FactID: "f1", Stance: "suspects"})
	if got := checkQuestTransitionUngrounded(pcHeld); len(got) != 0 {
		t.Fatalf("a pc's suspicion grounds the move: %+v", got)
	}
	npcOnly := base(machine("f1"), AwarenessView{Knower: "npc9", FactID: "f1", Stance: "knows"})
	if got := checkQuestTransitionUngrounded(npcOnly); len(got) != 1 {
		t.Fatalf("an npc's knowledge does not ground the party's move: %+v", got)
	}
	// A deliberate unaware row is not a grant.
	unaware := base(machine("f1"), AwarenessView{Knower: PartyKnower, FactID: "f1", Stance: "unaware"})
	if got := checkQuestTransitionUngrounded(unaware); len(got) != 1 {
		t.Fatalf("unaware is not a grant: %+v", got)
	}
	// An edge with no requirements never fires.
	plain := base(machine())
	if got := checkQuestTransitionUngrounded(plain); len(got) != 0 {
		t.Fatalf("an unconditioned edge must not fire: %+v", got)
	}
}

func TestCheckDanglingQuestLinks(t *testing.T) {
	live := entity("e1", "The Mines", StatusActive)
	deleted := entity("e2", "The Duke", StatusDeleted)
	machine := StateMachine{Initial: "unknown", States: States("unknown", "found"),
		Edges: []StateEdge{{From: "unknown", To: "found"}}}
	quest := Quest{ID: "q1", CampaignID: "c1", Name: "q", CurrentState: "unknown", Machine: machine}

	// quest_entities at a deleted entity fires; at a live one it does not.
	snap := &Snapshot{Entities: []Entity{live, deleted}, Quests: []Quest{quest},
		QuestEntities: []QuestEntity{
			{ID: "l1", QuestID: "q1", EntityID: "e1", Role: QuestRoleSite},
			{ID: "l2", QuestID: "q1", EntityID: "e2", Role: QuestRoleGiver},
		},
		ActIDs: map[string]bool{}}
	got := checkDanglingReference(snap)
	if len(got) != 1 || got[0].RecordKind != "quest_entity" || got[0].RecordID != "l2" {
		t.Fatalf("want one dangling quest_entity on l2: %+v", got)
	}

	// quest_state_facts at a missing fact fires; at an undeclared state
	// fires; a sound row does not.
	snap = &Snapshot{Entities: []Entity{live}, Quests: []Quest{quest},
		Facts: []Fact{fact("f1", "e1", "hides", "", "something", ConfidenceCanon)},
		QuestStateFacts: []QuestStateFact{
			{ID: "sf1", QuestID: "q1", StateKey: "found", FactID: "f1", Disposition: QuestFactReveals},
			{ID: "sf2", QuestID: "q1", StateKey: "found", FactID: "gone", Disposition: QuestFactRequires},
			{ID: "sf3", QuestID: "q1", StateKey: "dreamed", FactID: "f1", Disposition: QuestFactReveals},
		},
		ActIDs: map[string]bool{}}
	got = checkDanglingReference(snap)
	if len(got) != 2 {
		t.Fatalf("want two dangling quest_state_fact rows: %+v", got)
	}
	for _, f := range got {
		if f.RecordKind != "quest_state_fact" || (f.RecordID != "sf2" && f.RecordID != "sf3") {
			t.Fatalf("wrong rows flagged: %+v", f)
		}
	}

	// act_id at a missing act fires; at a declared one it does not.
	hanging := quest
	hanging.ActID = "act9"
	snap = &Snapshot{Entities: []Entity{live}, Quests: []Quest{hanging}, ActIDs: map[string]bool{"act1": true}}
	got = checkDanglingReference(snap)
	if len(got) != 1 || got[0].RecordKind != "quest" || got[0].RecordID != "q1" {
		t.Fatalf("want one dangling act pointer: %+v", got)
	}
}
