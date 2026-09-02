package canon

// The quest designer's tests (MAD-371): the hook read, the cast scored out
// of the campaign, the machine asserted on the graph with the model faked,
// the validate-then-repair contract, and the branch operation.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
)

const questTestHook = "The silver caravan from the eastern mines never arrived. Something has been disappearing on that road for months."

// questTestFill builds a valid model fill for a topology: distinct state
// names, a cast drawn from the campaign, and one reveal slot pointed at an
// existing secret.
func questTestFill(topo campaign.QuestTopology, existingSecret string) map[string]any {
	m := map[string]any{
		"quest_name":    "The Caravan That Never Came",
		"quest_summary": "The silver caravan is missing and the road is eating people. The party finds what took it, or it takes them too.",
		"giver":         "Tom the Innkeeper",
		"site":          "Blackwater",
		"obstacle":      "Cult of the Root",
	}
	names := map[string]string{
		"beat-1": "The Hook Lands", "beat-2": "The Ledger Trail",
		"beat-3": "The Road Again", "beat-4": "The Confrontation",
		"beat-5": "The Last Mile", "beat-6": "The Long Watch",
		"beat-7": "The Deep Road", "beat-8": "The Final Toll",
		"fork-1-a": "Trust the Survivor", "fork-1-b": "Accuse the Survivor",
		"fork-2-a": "Deliver the Truth", "fork-2-b": "Bury the Truth",
		"fork-3-a": "Force the Hand", "fork-3-b": "Wait and Watch",
		"fork-4-a": "Burn the Ledgers", "fork-4-b": "Keep the Ledgers",
		"ending-success": "The Caravan Found", "ending-failure": "The Trail Dies",
		"ending-cold": "The Case Goes Cold", "ending-betrayed": "The Betrayal Lands",
	}
	for _, slot := range topo.States {
		name, ok := names[slot.ID]
		if !ok {
			panic("no test name for slot " + slot.ID)
		}
		m["state_"+slot.ID+"_name"] = name
		if slot.Role != campaign.QuestSlotEnding {
			m["state_"+slot.ID+"_detail"] = "The situation tightens; the party has something concrete to do here."
		}
		if slot.Reveal {
			key := "secret_" + slot.ID
			if slot.ID == "fork-1-a" && existingSecret != "" {
				m[key] = existingSecret
				continue
			}
			m[key] = "new"
			m[key+"_new"] = fmt.Sprintf("What %s reveals: someone paid for the road to stay dark.", name)
		}
	}
	return m
}

func questJSON(t *testing.T, v map[string]any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// plantUnreachableSecret adds a secret fact with no awareness row and no
// scene: exactly what unreachable_secret flags.
func plantUnreachableSecret(t *testing.T, s *Store, campaignID, subject string) string {
	t.Helper()
	f, err := s.campaigns.CreateFact(context.Background(), campaignID,
		subject, "hides", "", "the caravan's cargo",
		"The caravan's silver cargo was the Duke's war chest, moved in secret.",
		campaign.ConfidenceCanon, campaign.VisibilitySecret, "test",
		[]campaign.ProvenanceInput{{Method: campaign.MethodDMAuthored, SessionID: "seed"}})
	if err != nil {
		t.Fatalf("plant secret: %v", err)
	}
	return f.ID
}

func TestReadQuestHook(t *testing.T) {
	s := ReadQuestHook("The caravan never arrived and people are missing — who is taking them?")
	if s.Mode != campaign.QuestModeInvestigate {
		t.Errorf("mode = %q, want investigate", s.Mode)
	}
	if len(s.Terms) == 0 {
		t.Error("no terms read from the hook")
	}
	f := ReadQuestHook("The town is besieged by a horde; defend the walls")
	if f.Mode != campaign.QuestModeFight {
		t.Errorf("mode = %q, want fight", f.Mode)
	}
	b := ReadQuestHook("There is a traitor in the guild, an informant turned double agent")
	if b.Mode != campaign.QuestModeBetray {
		t.Errorf("mode = %q, want betrayal", b.Mode)
	}
	g := ReadQuestHook("The innkeeper's steward was murdered")
	if len(g.GiverKinds) == 0 {
		t.Error("no giver kinds read from the hook")
	}
}

func TestGenerateQuest_MachineIsArithmetic(t *testing.T) {
	s, campaignID, fx := batchStore(t)
	plantUnreachableSecret(t, s, campaignID, fx.Duke)
	topo, err := campaign.BuildQuestTopology("investigation", 3, 5)
	if err != nil {
		t.Fatal(err)
	}
	model := &fakeModel{responses: []string{
		questJSON(t, questTestFill(topo, "The caravan's silver cargo was the Duke's war chest, moved in secret.")),
	}}
	s.model = model

	res, err := s.GenerateQuest(context.Background(), QuestDesignInput{
		CampaignID: campaignID, Hook: questTestHook,
		Kind: "investigation", BranchPoints: 3, Depth: 5,
		Anchor: fx.Duke, CreatedBy: "dm",
	})
	if err != nil {
		t.Fatalf("GenerateQuest: %v", err)
	}
	// Three branch points, three forks: our arithmetic, with the model faked.
	outgoing := map[string]int{}
	for _, e := range res.Machine.Edges {
		outgoing[e.From]++
	}
	forks := 0
	for _, st := range res.Machine.States {
		if outgoing[st.Key] == 2 {
			forks++
		}
	}
	if forks != 3 {
		t.Fatalf("forks = %d, want 3", forks)
	}
	// The machine passes every check a staged machine must pass, and the
	// two branches of a fork provably cannot reach each other.
	if err := res.Machine.Validate(); err != nil {
		t.Fatalf("machine invalid: %v", err)
	}
	if problems := campaign.ForksExclusive(res.Machine); len(problems) > 0 {
		t.Fatalf("forks not exclusive: %v", problems)
	}
	if findings := questMachineFindings(res.Machine); len(findings) > 0 {
		t.Fatalf("quest checks fired: %v", findings)
	}

	// The cast came from the campaign: the giver is Tom, not a second Tom.
	questItem := res.Batch.Items[findByKind(res.Batch.Items, ReviewProposedQuest)]
	payload := decodePayload(t, questItem.Detail)
	links := payload["entities"].([]any)
	wantRefs := map[string]string{
		"giver": fx.Tom, "site": fx.Blackwater, "obstacle": fx.Cult,
		campaign.QuestRoleSubject: fx.Duke,
	}
	seen := map[string]string{}
	for _, l := range links {
		lm := l.(map[string]any)
		seen[lm["role"].(string)] = lm["entity"].(string)
	}
	for role, want := range wantRefs {
		if seen[role] != want {
			t.Errorf("quest %s ref = %v, want %s", role, seen[role], want)
		}
	}
	// No duplicate entity was staged for the reused cast.
	for _, it := range res.Batch.Items {
		if it.Kind == ReviewProposedEntity {
			t.Errorf("entity item staged for a cast the campaign already has: %s", it.Subject)
		}
	}
}

// questTopology builds a topology the test's fill matches slot for slot.
func questTopology(t *testing.T, kind string, b, d int) campaign.QuestTopology {
	t.Helper()
	topo, err := campaign.BuildQuestTopology(kind, b, d)
	if err != nil {
		t.Fatal(err)
	}
	return topo
}

func findByKind(items []Review, kind string) int {
	for i := range items {
		if items[i].Kind == kind {
			return i
		}
	}
	return -1
}

func decodePayload(t *testing.T, detail string) map[string]any {
	t.Helper()
	var p map[string]any
	if err := json.Unmarshal([]byte(detail), &p); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	return p
}

func TestGenerateQuest_RevealsExistingSecretAndStagesInDependencyOrder(t *testing.T) {
	s, campaignID, fx := batchStore(t)
	secretID := plantUnreachableSecret(t, s, campaignID, fx.Duke)
	statement := "The caravan's silver cargo was the Duke's war chest, moved in secret."
	topo := questTopology(t, "investigation", 2, 4)
	model := &fakeModel{responses: []string{questJSON(t, questTestFill(topo, statement))}}
	s.model = model

	res, err := s.GenerateQuest(context.Background(), QuestDesignInput{
		CampaignID: campaignID, Hook: questTestHook, BranchPoints: 2, Depth: 4,
	})
	if err != nil {
		t.Fatalf("GenerateQuest: %v", err)
	}

	// The existing secret is reused: no fact item for it, and the quest
	// item's state_facts point the reveal state at the real fact id.
	var factItems, questDeps []string
	for _, it := range res.Batch.Items {
		if it.Kind == ReviewProposedFact {
			factItems = append(factItems, it.ID)
		}
	}
	questItem := res.Batch.Items[findByKind(res.Batch.Items, ReviewProposedQuest)]
	for _, dep := range questItem.DependsOn {
		questDeps = append(questDeps, dep)
	}
	for _, id := range factItems {
		found := false
		for _, d := range questDeps {
			if d == id {
				found = true
			}
		}
		if !found {
			t.Errorf("fact item %s is not a dependency of the quest item (deps %v)", id, questDeps)
		}
	}
	payload := decodePayload(t, questItem.Detail)
	ties := payload["state_facts"].([]any)
	reusedExisting := false
	for _, tie := range ties {
		tm := tie.(map[string]any)
		if tm["fact"] == secretID {
			reusedExisting = true
		}
	}
	if !reusedExisting {
		t.Fatalf("no state fact ties a reveal state to the planted secret %s (ties %v)", secretID, ties)
	}
	// The remaining reveal slots staged new secrets, in dependency order.
	if len(factItems) != len(topo.RevealSlots())-1 {
		t.Fatalf("new fact items = %d, want %d (one reveal reused an existing secret)",
			len(factItems), len(topo.RevealSlots())-1)
	}
}

func TestGenerateQuest_GiverNamedByTheModelIsReusedNotDuplicated(t *testing.T) {
	s, campaignID, fx := batchStore(t)
	topo := questTopology(t, "investigation", 1, 3)
	fill := questTestFill(topo, "")
	// The model says "new" — but names the Duke the campaign already has.
	fill["giver"] = "new"
	fill["giver_new_name"] = "Duke Aldric Vane"
	fill["giver_new_summary"] = "The ruler of the eastern marches, pale and precise."
	model := &fakeModel{responses: []string{questJSON(t, fill)}}
	s.model = model

	res, err := s.GenerateQuest(context.Background(), QuestDesignInput{
		CampaignID: campaignID, Hook: questTestHook,
		BranchPoints: 1, Depth: 3,
	})
	if err != nil {
		t.Fatalf("GenerateQuest: %v", err)
	}
	for _, it := range res.Batch.Items {
		if it.Kind == ReviewProposedEntity {
			t.Fatalf("a second %q was proposed (item %s) — the Duke already exists", "Duke Aldric Vane", it.Subject)
		}
	}
	questItem := res.Batch.Items[findByKind(res.Batch.Items, ReviewProposedQuest)]
	payload := decodePayload(t, questItem.Detail)
	for _, l := range payload["entities"].([]any) {
		lm := l.(map[string]any)
		if lm["role"] == "giver" && lm["entity"] != fx.Duke {
			t.Fatalf("giver ref = %v, want the Duke's id %s", lm["entity"], fx.Duke)
		}
	}
	if len(res.Reused) == 0 {
		t.Fatal("the reuse was not reported")
	}
}

func TestGenerateQuest_RepairsOnce(t *testing.T) {
	s, campaignID, _ := batchStore(t)
	topo := questTopology(t, "investigation", 2, 4)
	bad := questTestFill(topo, "")
	// Two states whose names slug to the same key: a collision the machine
	// validation must catch and the one repair retry must fix.
	bad["state_beat-2_name"] = "The Same Trail"
	bad["state_beat-3_name"] = "the same trail!"
	good := questTestFill(topo, "")
	model := &fakeModel{responses: []string{questJSON(t, bad), questJSON(t, good)}}
	s.model = model

	res, err := s.GenerateQuest(context.Background(), QuestDesignInput{
		CampaignID: campaignID, Hook: questTestHook, BranchPoints: 2, Depth: 4,
	})
	if err != nil {
		t.Fatalf("GenerateQuest: %v", err)
	}
	if len(model.calls) != 2 {
		t.Fatalf("model calls = %d, want 2 (one fill, one repair)", len(model.calls))
	}
	if !strings.Contains(model.calls[1], "name every state distinctly") {
		t.Errorf("repair prompt did not quote the collision problem: %.200s", model.calls[1])
	}
	if err := res.Machine.Validate(); err != nil {
		t.Fatalf("repaired machine invalid: %v", err)
	}

	// A fill that fails validation twice gives up with the problems named.
	model = &fakeModel{responses: []string{questJSON(t, bad), questJSON(t, bad)}}
	s.model = model
	_, err = s.GenerateQuest(context.Background(), QuestDesignInput{
		CampaignID: campaignID, Hook: questTestHook, BranchPoints: 2, Depth: 4,
	})
	if err == nil || !strings.Contains(err.Error(), "failed validation twice") {
		t.Fatalf("err = %v, want a two-strikes failure", err)
	}
}

func TestGenerateQuest_InputValidation(t *testing.T) {
	s, campaignID, _ := batchStore(t)
	ctx := context.Background()
	for _, bad := range []QuestDesignInput{
		{CampaignID: campaignID, Hook: "   "},
		{CampaignID: campaignID, Hook: questTestHook, Kind: "musical"},
		{CampaignID: campaignID, Hook: questTestHook, BranchPoints: 5},
		{CampaignID: campaignID, Hook: questTestHook, Depth: 9},
		{CampaignID: campaignID, Hook: questTestHook, BranchPoints: 3, Depth: 3},
		{CampaignID: campaignID, Hook: questTestHook, Anchor: "no-such-entity"},
	} {
		if _, err := s.GenerateQuest(ctx, bad); err == nil {
			t.Errorf("input %+v accepted", bad)
		}
	}
}

/* ---------- the acceptance run, end to end ---------- */

// TestGenerateQuest_EndToEnd is MAD-371's acceptance over a temp SQLite
// database with a fake LLM client replaying a fixture: the planted secret
// is flagged unreachable_secret, the designed quest is staged and accepted
// whole, and canon check comes back with no error-severity finding and the
// secret no longer flagged — the quest is its clue path.
func TestGenerateQuest_EndToEnd(t *testing.T) {
	s, campaignID, fx := batchStore(t)
	ctx := context.Background()
	secretID := plantUnreachableSecret(t, s, campaignID, fx.Duke)
	statement := "The caravan's silver cargo was the Duke's war chest, moved in secret."

	// Before: the planted secret has no clue path any character can reach.
	flags, err := s.CheckCampaign(ctx, campaignID, DefaultCheckOptions())
	if err != nil {
		t.Fatalf("check before: %v", err)
	}
	flagged := false
	for _, f := range flags {
		if f.CheckCode == CheckUnreachableSecret && f.RecordID == secretID && f.Status == FlagOpen {
			flagged = true
		}
	}
	if !flagged {
		t.Fatal("the planted secret was not flagged unreachable_secret before the quest existed")
	}

	// The design exchange, model faked.
	topo := questTopology(t, "investigation", 2, 4)
	model := &fakeModel{responses: []string{questJSON(t, questTestFill(topo, statement))}}
	s.model = model
	res, err := s.GenerateQuest(ctx, QuestDesignInput{
		CampaignID: campaignID, Hook: questTestHook, BranchPoints: 2, Depth: 4,
		Anchor: fx.Duke, CreatedBy: "dm",
	})
	if err != nil {
		t.Fatalf("GenerateQuest: %v", err)
	}

	// Accept the batch whole.
	decided, err := s.DecideBatch(ctx, campaignID, res.Batch.ID, DecisionAccept, nil, "dm")
	if err != nil {
		t.Fatalf("DecideBatch: %v", err)
	}
	var questID string
	for _, it := range decided.Items {
		if it.Kind == ReviewProposedQuest {
			if it.Status != ReviewAccepted {
				t.Fatalf("quest item status = %s (%s)", it.Status, it.Reason)
			}
			questID = it.ResultRef
		}
	}
	if questID == "" {
		t.Fatal("the accepted batch wrote no quest")
	}

	// The quest landed with its cast and its clue path.
	q, err := s.campaigns.GetQuest(ctx, campaign.ScopeDM, campaignID, questID)
	if err != nil {
		t.Fatal(err)
	}
	if q.Name != "The Caravan That Never Came" {
		t.Errorf("quest name = %q", q.Name)
	}
	links, err := s.campaigns.QuestEntities(ctx, campaign.ScopeDM, campaignID, questID)
	if err != nil {
		t.Fatal(err)
	}
	byRole := map[string]string{}
	for _, l := range links {
		byRole[l.Role] = l.EntityID
	}
	if byRole["giver"] != fx.Tom || byRole["site"] != fx.Blackwater || byRole["obstacle"] != fx.Cult {
		t.Errorf("cast = %v", byRole)
	}
	if byRole[campaign.QuestRoleSubject] != fx.Duke {
		t.Errorf("anchor subject = %v, want the Duke", byRole[campaign.QuestRoleSubject])
	}
	ties, err := s.campaigns.QuestStateFacts(ctx, campaign.ScopeDM, campaignID, questID)
	if err != nil {
		t.Fatal(err)
	}
	tied := false
	for _, r := range ties {
		if r.FactID == secretID && r.Disposition == campaign.QuestFactReveals {
			tied = true
		}
	}
	if !tied {
		t.Fatalf("the planted secret is not tied to a reveal state (ties %v)", ties)
	}

	// After: no error-severity finding anywhere, and the secret the quest
	// reveals is no longer flagged unreachable_secret.
	flags, err = s.CheckCampaign(ctx, campaignID, DefaultCheckOptions())
	if err != nil {
		t.Fatalf("check after: %v", err)
	}
	for _, f := range flags {
		if f.Severity == string(campaign.SeverityError) && f.Status == FlagOpen {
			t.Errorf("error-severity flag after accepting the quest: %s %s (%s)", f.CheckCode, f.Message, f.RecordID)
		}
		if f.CheckCode == CheckUnreachableSecret && f.RecordID == secretID && f.Status == FlagOpen {
			t.Errorf("the revealed secret is still flagged unreachable_secret")
		}
	}
}

/* ---------- branch this quest ---------- */

func TestGenerateQuestBranch_TwoExclusiveOutcomes(t *testing.T) {
	s, campaignID, _ := batchStore(t)
	ctx := context.Background()

	machine := campaign.StateMachine{Initial: "offered"}
	machine.States = campaign.States("offered", "find-the-survivor", "triumph", "disaster")
	machine.States[2].Terminal = campaign.TerminalSuccess
	machine.States[3].Terminal = campaign.TerminalFailure
	machine.Edges = []campaign.StateEdge{
		{From: "offered", To: "find-the-survivor"},
		{From: "find-the-survivor", To: "triumph"},
		{From: "find-the-survivor", To: "disaster"},
	}
	q, err := s.campaigns.CreateQuest(ctx, campaignID, campaign.QuestInput{
		Name: "The Missing Caravan", Machine: machine,
	})
	if err != nil {
		t.Fatal(err)
	}
	// A recorded move the edit must respect.
	if _, err := s.campaigns.TransitionQuest(ctx, campaignID, q.ID, "find-the-survivor", ""); err != nil {
		t.Fatal(err)
	}

	fill := map[string]any{
		"branch_a_name":   "Trust the Survivor",
		"branch_a_detail": "The survivor is hidden somewhere in Blackwater.",
		"branch_a_ending": "The Survivor Saved",
		"branch_b_name":   "Accuse the Survivor",
		"branch_b_detail": "The survivor is the one who organized the raids.",
		"branch_b_ending": "The Wrong Party Condemned",
	}
	model := &fakeModel{responses: []string{questJSON(t, fill)}}
	s.model = model

	res, err := s.GenerateQuestBranch(ctx, QuestBranchInput{
		CampaignID: campaignID, QuestID: q.ID, StateKey: "find-the-survivor",
	})
	if err != nil {
		t.Fatalf("GenerateQuestBranch: %v", err)
	}
	// Two new arms, two new endings, everything the machine declared kept.
	if got := len(res.Machine.States); got != 8 {
		t.Fatalf("states = %d, want 8", got)
	}
	if !res.Machine.HasEdge("find-the-survivor", "trust-the-survivor") ||
		!res.Machine.HasEdge("find-the-survivor", "accuse-the-survivor") {
		t.Fatal("the chosen state did not gain its two outcomes")
	}
	if !res.Machine.HasEdge("find-the-survivor", "triumph") {
		t.Fatal("an existing edge was dropped by the branch")
	}
	if problems := campaign.ForksExclusive(res.Machine); len(problems) > 0 {
		t.Fatalf("branched forks not exclusive: %v", problems)
	}

	// Accept the one-item batch and the edit lands through UpdateQuest,
	// recorded transitions intact.
	_, err = s.DecideBatch(ctx, campaignID, res.Batch.ID, DecisionAccept, nil, "dm")
	if err != nil {
		t.Fatalf("DecideBatch: %v", err)
	}
	updated, err := s.campaigns.GetQuest(ctx, campaign.ScopeDM, campaignID, q.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := updated.Machine.State("trust-the-survivor"); !ok {
		t.Fatal("the accepted branch did not land in the quest's machine")
	}
	transitions, err := s.campaigns.QuestTransitions(ctx, campaign.ScopeDM, campaignID, q.ID)
	if err != nil || len(transitions) != 1 {
		t.Fatalf("transitions = %v (err %v); recorded history must survive", transitions, err)
	}

	// The refusals: an unknown state, and an ending.
	model = &fakeModel{responses: []string{questJSON(t, fill)}}
	s.model = model
	if _, err := s.GenerateQuestBranch(ctx, QuestBranchInput{CampaignID: campaignID, QuestID: q.ID, StateKey: "nope"}); err == nil {
		t.Error("unknown state accepted")
	}
	if _, err := s.GenerateQuestBranch(ctx, QuestBranchInput{CampaignID: campaignID, QuestID: q.ID, StateKey: "triumph"}); err == nil {
		t.Error("terminal state accepted")
	}
}
