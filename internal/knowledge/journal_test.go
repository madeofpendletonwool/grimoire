package knowledge

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
)

// journalQuests builds one secret and one public quest over a rich machine:
// labels on every state, detail on one, an edge with a label and a requires
// clause, a terminal ending, and an unvisited branch the journal must never
// reveal. The public quest is moved one step so "visited" has teeth.
func journalQuests(t *testing.T, cs *campaign.Store, fx *campaign.Fixture) {
	t.Helper()
	ctx := context.Background()
	cid := fx.Campaign.ID
	machine := campaign.StateMachine{
		Initial: "unknown",
		States: []campaign.State{
			{Key: "unknown", Label: "The miners are missing"},
			{Key: "found", Label: "The caravan is found", Detail: "DM only: the robed figures took them east."},
			{Key: "trusted", Label: "The survivor is trusted"},
			{Key: "secretly_betrayed", Label: "The survivor lies for the cult"},
			{Key: "done", Label: "The miners come home", Terminal: campaign.TerminalSuccess},
		},
		Edges: []campaign.StateEdge{
			{From: "unknown", To: "found", Label: "find the caravan"},
			{From: "found", To: "trusted", Label: "trust the survivor", Requires: []string{fx.FactMinesOwned}},
			{From: "found", To: "secretly_betrayed", Label: "press too hard"},
			{From: "trusted", To: "done", Label: "bring them home"},
		},
	}
	if _, err := cs.CreateQuest(ctx, cid, campaign.QuestInput{
		Name: "The Secret Search", Summary: "DM planning material", Machine: machine,
	}); err != nil {
		t.Fatalf("secret quest: %v", err)
	}
	pub, err := cs.CreateQuest(ctx, cid, campaign.QuestInput{
		Name: "The Missing Miners", Summary: "Find the miners.",
		Machine: machine, Visibility: campaign.QuestVisibilityPublic,
	})
	if err != nil {
		t.Fatalf("public quest: %v", err)
	}
	if _, err := cs.TransitionQuest(ctx, cid, pub.ID, "found", fx.EventAmbush); err != nil {
		t.Fatalf("move: %v", err)
	}
}

// TestQuestJournalIsLeakSafe is the quest half of the leak gate: a player
// scope sees the public quest, its summary, the current state's label and the
// states already visited — and never an unvisited branch (key or label), a
// state's detail, an edge's label or requires, or a secret-visibility quest's
// existence.
func TestQuestJournalIsLeakSafe(t *testing.T) {
	s, fx, _ := seeded(t)
	ctx := context.Background()
	cid := fx.Campaign.ID
	cs, err := campaign.New(s.db)
	if err != nil {
		t.Fatal(err)
	}
	journalQuests(t, cs, fx)

	body := func(t *testing.T, entries []QuestJournalEntry) string {
		t.Helper()
		b, err := json.Marshal(entries)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	// The party scope reads through the narrow player view too.
	view, err := s.PlayerViewOf(ScopeParty)
	if err != nil {
		t.Fatal(err)
	}
	partyEntries, err := view.QuestJournal(ctx, cid)
	if err != nil {
		t.Fatalf("party journal: %v", err)
	}
	direct, err := s.QuestJournal(ctx, ScopeParty, cid)
	if err != nil {
		t.Fatalf("party journal (wide store): %v", err)
	}
	if len(partyEntries) != len(direct) {
		t.Fatalf("player view and store disagree: %d vs %d", len(partyEntries), len(direct))
	}

	var public *QuestJournalEntry
	for i := range partyEntries {
		if partyEntries[i].Name == "The Secret Search" {
			t.Fatal("LEAK: the secret-visibility quest appeared in the journal")
		}
		if partyEntries[i].Name == "The Missing Miners" {
			public = &partyEntries[i]
		}
	}
	if public == nil {
		t.Fatalf("the public quest must journal: %+v", partyEntries)
	}

	// What the entry carries.
	if public.Summary != "Find the miners." || public.CurrentState.Key != "found" ||
		public.CurrentState.Label != "The caravan is found" {
		t.Fatalf("the journal entry lost its public shape: %+v", public)
	}
	visited := map[string]bool{}
	for _, st := range public.Visited {
		visited[st.Key] = true
	}
	if !visited["unknown"] || !visited["found"] || len(public.Visited) != 2 {
		t.Fatalf("visited must be the initial state plus the moves, in order: %+v", public.Visited)
	}

	// What it must never carry.
	dump := body(t, partyEntries)
	for _, leak := range []string{
		"secretly_betrayed", "The survivor lies", "dm only",
		"requires", "trust the survivor", "The Secret Search",
	} {
		if strings.Contains(strings.ToLower(dump), strings.ToLower(leak)) {
			t.Fatalf("LEAK: the journal body carries %q: %s", leak, dump)
		}
	}

	// The character scope sees the same journal; the DM reads the same
	// leak-safe projection.
	charEntries, err := s.QuestJournal(ctx, ScopeCharacter(fx.Thalia), cid)
	if err != nil || len(charEntries) != len(partyEntries) {
		t.Fatalf("character journal: %d entries, err %v", len(charEntries), err)
	}
	dmEntries, err := s.QuestJournal(ctx, ScopeDM, cid)
	if err != nil || len(dmEntries) != 1 || dmEntries[0].Name != "The Missing Miners" {
		t.Fatalf("the dm journal is the same projection: %+v %v", dmEntries, err)
	}
}
