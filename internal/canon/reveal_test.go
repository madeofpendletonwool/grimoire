package canon

// NPC reveal staging tests (MAD-313): the "ask as the Duke" write path. The
// load-bearing assertions: staging is idempotent on (npc, statement); an
// accepted reveal becomes a canon fact whose provenance says the model
// proposed it and a human accepted it; a dismissed reveal writes nothing.

import (
	"context"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
)

// revealStores builds the graph + canon stack the accept path needs, over the
// seeded campaign fixture.
func revealStores(t *testing.T) (*Store, *campaign.Fixture) {
	t.Helper()
	db := openDB(t)
	if _, err := db.Exec(
		`INSERT INTO users (id, username, password_hash, is_admin, created_at) VALUES ('keeper', 'keeper', 'x', 0, 0)`); err != nil {
		t.Fatalf("insert owner: %v", err)
	}
	fx, err := campaign.Seed(context.Background(), db, "keeper", "")
	if err != nil {
		t.Fatalf("campaign seed: %v", err)
	}
	camps, err := campaign.New(db)
	if err != nil {
		t.Fatalf("campaign store: %v", err)
	}
	ks, err := knowledge.New(db)
	if err != nil {
		t.Fatalf("knowledge store: %v", err)
	}
	cs, err := NewOffline(db)
	if err != nil {
		t.Fatalf("canon store: %v", err)
	}
	cs = cs.WithGraphStores(camps, ks)
	return cs, fx
}

func TestStageNPCRevealIsIdempotent(t *testing.T) {
	cs, fx := revealStores(t)
	ctx := context.Background()
	in := StageRevealInput{
		CampaignID: fx.Campaign.ID, NPCID: fx.Elara, NPCName: "Lady Elara",
		Statement: "Elara keeps the chapel key on her person at all times.",
		Rationale: "her goals require controlling access to the crypt",
		Question:  "where does she keep the chapel key?",
	}
	rev, fresh, err := cs.StageNPCReveal(ctx, in)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if !fresh || rev.Status != ReviewOpen || rev.Kind != ReviewNPCReveal {
		t.Fatalf("first stage: fresh=%v status=%s kind=%s", fresh, rev.Status, rev.Kind)
	}
	if rev.Payload["statement"] != in.Statement {
		t.Fatalf("payload statement: %v", rev.Payload["statement"])
	}

	again, fresh, err := cs.StageNPCReveal(ctx, in)
	if err != nil {
		t.Fatalf("re-stage: %v", err)
	}
	if fresh || again.ID != rev.ID {
		t.Fatalf("re-stage must be a no-op on the same item: fresh=%v id=%s want %s", fresh, again.ID, rev.ID)
	}
	// A trivially re-worded duplicate still dedups: whitespace only.
	spacey := in
	spacey.Statement = "  Elara keeps the chapel key   on her person at all times. "
	if _, fresh, err := cs.StageNPCReveal(ctx, spacey); err != nil || fresh {
		t.Fatalf("whitespace variant must dedup: fresh=%v err=%v", fresh, err)
	}
	// A genuinely different reveal opens a new item.
	other := in
	other.Statement = "Elara has a second identity in Blackwater."
	if _, fresh, err = cs.StageNPCReveal(ctx, other); err != nil || !fresh {
		t.Fatalf("different statement must stage new: fresh=%v err=%v", fresh, err)
	}
}

func TestAcceptNPCRevealWritesCanonWithHumanProvenance(t *testing.T) {
	cs, fx := revealStores(t)
	ctx := context.Background()
	rev, _, err := cs.StageNPCReveal(ctx, StageRevealInput{
		CampaignID: fx.Campaign.ID, NPCID: fx.Elara, NPCName: "Lady Elara",
		Statement: "Elara has met the party before, in disguise.",
	})
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	decided, err := cs.DecideReview(ctx, fx.Campaign.ID, rev.ID, DecisionAccept, "", "keeper", nil)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if decided.Status != ReviewAccepted || decided.ResultRef == "" {
		t.Fatalf("accept status=%s result=%s", decided.Status, decided.ResultRef)
	}

	// The fact is canon, subject the NPC, provenance ai_proposed + the
	// accepting human.
	f, err := cs.campaigns.GetFact(ctx, campaign.ScopeDM, fx.Campaign.ID, decided.ResultRef)
	if err != nil {
		t.Fatalf("load accepted fact: %v", err)
	}
	if f.Confidence != campaign.ConfidenceCanon {
		t.Fatalf("confidence %s, want canon", f.Confidence)
	}
	if f.SubjectEntity != fx.Elara || f.Predicate != "reveals" {
		t.Fatalf("subject/predicate: %s %s", f.SubjectEntity, f.Predicate)
	}
	prov, err := cs.campaigns.FactProvenance(ctx, campaign.ScopeDM, fx.Campaign.ID, f.ID)
	if err != nil || len(prov) == 0 {
		t.Fatalf("provenance: %v (%d rows)", err, len(prov))
	}
	if prov[0].Method != campaign.MethodAIProposed || prov[0].AcceptedBy != "keeper" {
		t.Fatalf("provenance method=%s accepted_by=%q, want ai_proposed accepted by keeper",
			prov[0].Method, prov[0].AcceptedBy)
	}
}

func TestDismissAndModifyNPCReveal(t *testing.T) {
	cs, fx := revealStores(t)
	ctx := context.Background()

	// Dismiss writes nothing.
	rev, _, err := cs.StageNPCReveal(ctx, StageRevealInput{
		CampaignID: fx.Campaign.ID, NPCID: fx.Duke, NPCName: "Duke Aldric Vane",
		Statement: "The Duke keeps hunting leashes in the crypt.",
	})
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if _, err = cs.DecideReview(ctx, fx.Campaign.ID, rev.ID, DecisionDismiss, "not interesting", "keeper", nil); err != nil {
		t.Fatalf("dismiss: %v", err)
	}
	facts, err := cs.campaigns.ListFacts(ctx, campaign.ScopeDM, fx.Campaign.ID, campaign.FactFilter{})
	if err != nil {
		t.Fatalf("list facts: %v", err)
	}
	for _, f := range facts {
		if f.Statement == "The Duke keeps hunting leashes in the crypt." {
			t.Fatalf("dismissed reveal must not become a fact")
		}
	}

	// Modify then accept writes the corrected statement.
	rev2, _, err := cs.StageNPCReveal(ctx, StageRevealInput{
		CampaignID: fx.Campaign.ID, NPCID: fx.Duke, NPCName: "Duke Aldric Vane",
		Statement: "The Duke keeps his hunting dogs in the crypt.",
	})
	if err != nil {
		t.Fatalf("stage 2: %v", err)
	}
	modified, err := cs.DecideReview(ctx, fx.Campaign.ID, rev2.ID, DecisionModify, "", "keeper",
		[]byte(`{"subject":"`+fx.Duke+`","predicate":"keeps","object_literal":"his hunting dogs in the eastern crypt","statement":"The Duke keeps his hunting dogs in the eastern crypt."}`))
	if err != nil {
		t.Fatalf("modify: %v", err)
	}
	f, err := cs.campaigns.GetFact(ctx, campaign.ScopeDM, fx.Campaign.ID, modified.ResultRef)
	if err != nil {
		t.Fatalf("load modified fact: %v", err)
	}
	if f.Predicate != "keeps" || f.Statement != "The Duke keeps his hunting dogs in the eastern crypt." {
		t.Fatalf("modified fact predicate=%q statement=%q", f.Predicate, f.Statement)
	}
}
