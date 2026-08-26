package canon

// Proposal batches (MAD-359): the multi-object proposal gate's acceptance
// run. The load-bearing tests mirror the issue's acceptance criteria: a
// batch whose items reference each other by name produces a graph whose
// edges point at the entities that same batch created; dismissing an item
// another item depends on refuses the dependent and says so, writing
// nothing for it; a batch decided twice is a no-op; and the staging path
// rejects a cyclic or unresolvable depends_on graph before any write.

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
)

// batchStore builds a canon store with the graph and knowledge stores
// wired, over the seeded fixture campaign — the same stack the review
// queue's tests use.
func batchStore(t *testing.T) (*Store, string, *campaign.Fixture) {
	t.Helper()
	db, fx, _ := seeded(t)
	s, err := NewWithValidator(db, &fakeModel{}, &fakeModel{}, testConfig())
	if err != nil {
		t.Fatalf("canon store: %v", err)
	}
	cs, err := campaign.New(db)
	if err != nil {
		t.Fatal(err)
	}
	ks, err := knowledge.New(db)
	if err != nil {
		t.Fatal(err)
	}
	return s.WithGraphStores(cs, ks), fx.Campaign.ID, fx
}

// skeletonBatch is one generator run: two entities that reference each
// other by name through a secret fact, a relationship over the same pair,
// an event whose participants are the batch's own entities, and a
// discovery of the batch's own fact. Every kind is exercised.
func skeletonBatch(campaignID string) BatchInput {
	return BatchInput{
		CampaignID: campaignID,
		Source:     BatchSourceSkeleton,
		Prompt:     "Design a campaign about a duchy on the brink",
		CreatedBy:  "keeper",
		Items: []BatchItemInput{
			{ID: "aldric", Kind: "entity", Payload: map[string]any{
				"local_id": "duke-aldric", "kind": "npc", "name": "Duke Aldric",
				"summary": "The aging duke of the marches.",
			}},
			{ID: "vane", Kind: "entity", Payload: map[string]any{
				"local_id": "house-vane", "kind": "faction", "name": "House Vane",
				"summary": "The duchy's richest trading house.",
			}},
			{ID: "control", Kind: "fact", DependsOn: []string{"aldric", "vane"}, Payload: map[string]any{
				"local_id": "control-fact", "statement": "Duke Aldric secretly controls House Vane.",
				"subject": "Duke Aldric", "predicate": "secretly_controls",
				"object_entity": "House Vane", "visibility": "secret",
			}},
			{ID: "edge", Kind: "relationship", DependsOn: []string{"aldric", "vane"}, Payload: map[string]any{
				"from_entity": "Duke Aldric", "rel_type": "secretly_controls", "to_entity": "House Vane",
			}},
			{ID: "feast", Kind: "event", DependsOn: []string{"aldric"}, Payload: map[string]any{
				"local_id": "feast", "summary": "The Duke hosts House Vane at a tense feast.",
				"participants": []any{
					map[string]any{"entity": "Duke Aldric", "role": "host"},
					map[string]any{"entity": "House Vane", "role": "guest"},
				},
			}},
			{ID: "leak", Kind: "discovery", DependsOn: []string{"control"}, Payload: map[string]any{
				"fact": "control-fact", "discovered_by": "party", "stance": "suspects",
				"method": "a forged ledger",
			}},
		},
	}
}

func itemByInputID(t *testing.T, b *Batch, dedupInputID string) *Review {
	t.Helper()
	// The staged reviews carry no input id; find them by what the payload
	// declares, via the local_id the fixture items all set.
	for i := range b.Items {
		if str(b.Items[i].Payload, "local_id") == dedupInputID {
			return &b.Items[i]
		}
	}
	// relationship items have no local_id: match on rel_type.
	for i := range b.Items {
		if _, ok := b.Items[i].Payload["rel_type"]; ok {
			return &b.Items[i]
		}
	}
	t.Fatalf("no batch item matches %q", dedupInputID)
	return nil
}

func TestBatch_StageAndAcceptLinkedItems(t *testing.T) {
	s, campaignID, _ := batchStore(t)
	ctx := context.Background()

	batch, err := s.StageBatch(ctx, skeletonBatch(campaignID))
	if err != nil {
		t.Fatalf("StageBatch: %v", err)
	}
	if batch.Status != BatchOpen || len(batch.Items) != 6 || len(batch.Skipped) != 0 {
		t.Fatalf("staged batch = %s with %d items, %d skipped", batch.Status, len(batch.Items), len(batch.Skipped))
	}
	// depends_on landed as sibling review ids.
	factItem := itemByInputID(t, batch, "control-fact")
	if len(factItem.DependsOn) != 2 {
		t.Fatalf("fact item depends_on = %v, want its two entities", factItem.DependsOn)
	}
	for _, dep := range factItem.DependsOn {
		if dep == factItem.ID {
			t.Fatalf("fact item depends on itself: %v", factItem.DependsOn)
		}
	}

	res, err := s.DecideBatch(ctx, campaignID, batch.ID, DecisionAccept, nil, "keeper")
	if err != nil {
		t.Fatalf("DecideBatch accept: %v", err)
	}
	if res.Batch.Status != BatchAccepted {
		t.Fatalf("batch status = %s, want accepted (items %+v)", res.Batch.Status, res.Items)
	}
	for _, it := range res.Items {
		if it.Status != ReviewAccepted {
			t.Fatalf("item %s (%s) = %s (%s), want accepted", it.ReviewID, it.Subject, it.Status, it.Reason)
		}
		if it.ResultRef == "" {
			t.Fatalf("item %s recorded no result_ref", it.ReviewID)
		}
	}

	// The acceptance criterion: the relationship points at the entity that
	// this same batch created, and the fact's subject and object resolve
	// the same way. Result refs live on the decision's outcomes.
	outcomeOf := map[string]BatchItemOutcome{}
	for _, it := range res.Items {
		outcomeOf[it.ReviewID] = it
	}
	aldric := itemByInputID(t, batch, "duke-aldric")
	vane := itemByInputID(t, batch, "house-vane")
	edge := itemByInputID(t, batch, "rel_type")
	fact := itemByInputID(t, batch, "control-fact")

	var fromEntity, toEntity string
	if err := s.db.QueryRowContext(ctx,
		`SELECT from_entity, to_entity FROM relationships WHERE id = ?`, outcomeOf[edge.ID].ResultRef).
		Scan(&fromEntity, &toEntity); err != nil {
		t.Fatalf("load relationship: %v", err)
	}
	if fromEntity != outcomeOf[aldric.ID].ResultRef || toEntity != outcomeOf[vane.ID].ResultRef {
		t.Fatalf("relationship = %s..%s, want the batch-created entities %s..%s",
			fromEntity, toEntity, outcomeOf[aldric.ID].ResultRef, outcomeOf[vane.ID].ResultRef)
	}
	cs, err := campaign.New(s.db)
	if err != nil {
		t.Fatal(err)
	}
	f, err := cs.GetFact(ctx, campaign.ScopeDM, campaignID, outcomeOf[fact.ID].ResultRef)
	if err != nil {
		t.Fatalf("load fact: %v", err)
	}
	if f.SubjectEntity != outcomeOf[aldric.ID].ResultRef || f.ObjectEntity != outcomeOf[vane.ID].ResultRef {
		t.Fatalf("fact endpoints = %s..%s, want the batch-created entities %s..%s",
			f.SubjectEntity, f.ObjectEntity, outcomeOf[aldric.ID].ResultRef, outcomeOf[vane.ID].ResultRef)
	}
	if f.Visibility != campaign.VisibilitySecret || f.Confidence != campaign.ConfidenceCanon {
		t.Fatalf("fact visibility/confidence = %s/%s", f.Visibility, f.Confidence)
	}

	// Provenance: ai_proposed, no span, the human's acceptance recorded.
	var method, quote string
	var acceptedBy string
	var spanStart, spanEnd sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `
		SELECT method, quote, COALESCE(accepted_by, ''), span_start, span_end
		  FROM fact_provenance WHERE fact_id = ?`, outcomeOf[fact.ID].ResultRef).
		Scan(&method, &quote, &acceptedBy, &spanStart, &spanEnd); err != nil {
		t.Fatalf("load provenance: %v", err)
	}
	if method != campaign.MethodAIProposed {
		t.Fatalf("provenance method = %s, want %s", method, campaign.MethodAIProposed)
	}
	if spanStart.Valid || spanEnd.Valid {
		t.Fatalf("generated fact carries a span: %v..%v", spanStart, spanEnd)
	}
	if acceptedBy != "keeper" {
		t.Fatalf("provenance accepted_by = %q", acceptedBy)
	}

	// The discovery's awareness row points at the batch-created fact: the
	// party suspects the fact this same batch accepted.
	var leakRef string
	for _, it := range res.Items {
		if it.Kind == ReviewProposedDiscovery {
			leakRef = it.ResultRef
		}
	}
	if leakRef == "" {
		t.Fatal("no discovery outcome")
	}
	var aware int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM awareness WHERE campaign_id = ? AND knower = 'party' AND fact_id = ?`,
		campaignID, outcomeOf[fact.ID].ResultRef).Scan(&aware); err != nil {
		t.Fatal(err)
	}
	if aware != 1 {
		t.Fatalf("awareness rows for the batch-created fact = %d, want 1", aware)
	}
}

func TestBatch_DismissDependencyRefusesDependents(t *testing.T) {
	s, campaignID, fx := batchStore(t)
	ctx := context.Background()

	in := skeletonBatch(campaignID)
	// Keep only the vane entity, one fact over it (subjected to the
	// fixture's existing duke, so the only batch dependency is vane), and
	// the discovery on the fact: dismissing vane must refuse the fact, and
	// the refusal must cascade to the discovery.
	in.Items = []BatchItemInput{
		in.Items[1],
		{ID: "control", Kind: "fact", DependsOn: []string{"vane"}, Payload: map[string]any{
			"local_id": "control-fact", "statement": "The Duke secretly controls House Vane.",
			"subject": fx.Duke, "predicate": "secretly_controls",
			"object_entity": "House Vane", "visibility": "secret",
		}},
		in.Items[5],
	}
	batch, err := s.StageBatch(ctx, in)
	if err != nil {
		t.Fatalf("StageBatch: %v", err)
	}
	vane := itemByInputID(t, batch, "house-vane")
	fact := itemByInputID(t, batch, "control-fact")

	res, err := s.DecideBatch(ctx, campaignID, batch.ID, DecisionAccept,
		[]ItemDecision{{ItemID: vane.ID, Decision: DecisionDismiss}}, "keeper")
	if err != nil {
		t.Fatalf("DecideBatch: %v", err)
	}
	if res.Batch.Status != BatchDismissed {
		t.Fatalf("batch status = %s, want dismissed (everything dropped)", res.Batch.Status)
	}
	outcomes := map[string]BatchItemOutcome{}
	for _, it := range res.Items {
		outcomes[it.ReviewID] = it
	}
	if got := outcomes[vane.ID].Status; got != ReviewDismissed {
		t.Fatalf("dismissed item status = %s", got)
	}
	dep := outcomes[fact.ID]
	if dep.Status != ReviewDismissed {
		t.Fatalf("dependent status = %s, want refused (dismissed)", dep.Status)
	}
	if !strings.Contains(dep.Reason, "dismissed") {
		t.Fatalf("dependent reason = %q, want the refusal reported", dep.Reason)
	}
	// The cascade reached the discovery, and says why.
	var leakOutcome *BatchItemOutcome
	for i := range res.Items {
		if res.Items[i].Kind == ReviewProposedDiscovery {
			leakOutcome = &res.Items[i]
		}
	}
	if leakOutcome == nil {
		t.Fatal("discovery outcome missing")
	}
	if leakOutcome.Status != ReviewDismissed || leakOutcome.Reason == "" {
		t.Fatalf("cascaded discovery = %s (%s)", leakOutcome.Status, leakOutcome.Reason)
	}

	// Nothing partial: no entity, fact or discovery was written for any of
	// the batch's items.
	for _, check := range []struct {
		q    string
		what string
	}{
		{`SELECT COUNT(*) FROM entities WHERE campaign_id = ? AND name = 'House Vane'`, "entity"},
		{`SELECT COUNT(*) FROM facts WHERE campaign_id = ? AND statement = 'The Duke secretly controls House Vane.'`, "fact"},
		{`SELECT COUNT(*) FROM discoveries WHERE campaign_id = ? AND method = 'a forged ledger'`, "discovery"},
	} {
		var one int
		if err := s.db.QueryRowContext(ctx, check.q, campaignID).Scan(&one); err != nil {
			t.Fatal(err)
		}
		if one != 0 {
			t.Fatalf("%s written for a refused batch item (%d)", check.what, one)
		}
	}
}

func TestBatch_DecideTwiceIsNoop(t *testing.T) {
	s, campaignID, _ := batchStore(t)
	ctx := context.Background()

	batch, err := s.StageBatch(ctx, skeletonBatch(campaignID))
	if err != nil {
		t.Fatalf("StageBatch: %v", err)
	}
	first, err := s.DecideBatch(ctx, campaignID, batch.ID, DecisionAccept, nil, "keeper")
	if err != nil {
		t.Fatalf("first decide: %v", err)
	}
	var entityCount int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entities WHERE campaign_id = ?`, campaignID).Scan(&entityCount); err != nil {
		t.Fatal(err)
	}

	// The second decision — a dismissal this time — changes nothing: the
	// never-resurrect rule, held by the batch.
	second, err := s.DecideBatch(ctx, campaignID, batch.ID, DecisionDismiss, nil, "keeper")
	if err != nil {
		t.Fatalf("second decide: %v", err)
	}
	if second.Batch.Status != BatchAccepted {
		t.Fatalf("second decide flipped the batch to %s", second.Batch.Status)
	}
	for i, it := range second.Items {
		if it.Status != ReviewAccepted {
			t.Fatalf("second decide flipped item %d to %s", i, it.Status)
		}
	}
	var after int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entities WHERE campaign_id = ?`, campaignID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != entityCount {
		t.Fatalf("second decide wrote entities: %d -> %d", entityCount, after)
	}
	if first.Batch.Status != BatchAccepted {
		t.Fatalf("first result retroactively changed: %s", first.Batch.Status)
	}
}

func TestBatch_StageRejectsBadDependencyGraphs(t *testing.T) {
	s, campaignID, _ := batchStore(t)
	ctx := context.Background()

	entity := func(id string, deps []string) BatchItemInput {
		return BatchItemInput{ID: id, Kind: "entity", DependsOn: deps, Payload: map[string]any{
			"kind": "npc", "name": "Entity " + id, "summary": "x",
		}}
	}

	// A cycle: a -> b -> a.
	cycle := BatchInput{CampaignID: campaignID, Source: BatchSourceScene, Items: []BatchItemInput{
		entity("a", []string{"b"}), entity("b", []string{"a"}),
	}}
	if _, err := s.StageBatch(ctx, cycle); err == nil || !strings.Contains(err.Error(), "depends on") {
		t.Fatalf("cycle staged: err = %v", err)
	}
	// An unresolvable reference.
	ghost := BatchInput{CampaignID: campaignID, Source: BatchSourceScene, Items: []BatchItemInput{
		entity("a", []string{"ghost"}),
	}}
	if _, err := s.StageBatch(ctx, ghost); err == nil || !strings.Contains(err.Error(), "not in this batch") {
		t.Fatalf("ghost dep staged: err = %v", err)
	}
	// Nothing was written: no batch, no items.
	var one int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM proposal_batches WHERE campaign_id = ?`, campaignID).Scan(&one); err != nil {
		t.Fatal(err)
	}
	if one != 0 {
		t.Fatalf("%d batches written for rejected graphs", one)
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM canon_reviews WHERE campaign_id = ? AND batch_id IS NOT NULL`, campaignID).
		Scan(&one); err != nil {
		t.Fatal(err)
	}
	if one != 0 {
		t.Fatalf("%d batch items written for rejected graphs", one)
	}

	// An unknown source and a non-proposed kind are refused too.
	bad := BatchInput{CampaignID: campaignID, Source: "wish", Items: cycle.Items}
	if _, err := s.StageBatch(ctx, bad); err == nil {
		t.Fatal("unknown source staged")
	}
	badKind := BatchInput{CampaignID: campaignID, Source: BatchSourceScene, Items: []BatchItemInput{
		{ID: "a", Kind: "contradiction", Payload: map[string]any{"x": 1}},
	}}
	if _, err := s.StageBatch(ctx, badKind); err == nil {
		t.Fatal("non-proposed kind staged")
	}
}

func TestBatch_StageDedupSkipsIdenticalProposals(t *testing.T) {
	s, campaignID, _ := batchStore(t)
	ctx := context.Background()

	first, err := s.StageBatch(ctx, skeletonBatch(campaignID))
	if err != nil {
		t.Fatalf("first stage: %v", err)
	}
	// A different prompt, identical items: the dedup key covers kind and
	// payload, so every input resolves to the first batch's open items.
	again := skeletonBatch(campaignID)
	again.Prompt = "the same objects, re-proposed"
	second, err := s.StageBatch(ctx, again)
	if err != nil {
		t.Fatalf("second stage: %v", err)
	}
	if len(second.Items) != 0 {
		t.Fatalf("second stage minted %d items", len(second.Items))
	}
	if len(second.Skipped) != len(first.Items) {
		t.Fatalf("second stage skipped %d, want %d", len(second.Skipped), len(first.Items))
	}

	// Deciding the first batch decides the objects once; the second batch
	// is the same items, so deciding it reports them as already decided
	// and closes by its decision without writing anything.
	if _, err := s.DecideBatch(ctx, campaignID, first.ID, DecisionAccept, nil, "keeper"); err != nil {
		t.Fatalf("decide first: %v", err)
	}
	var entities int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entities WHERE campaign_id = ? AND name IN ('Duke Aldric', 'House Vane')`,
		campaignID).Scan(&entities); err != nil {
		t.Fatal(err)
	}
	if entities != 2 {
		t.Fatalf("accept wrote %d batch entities, want 2", entities)
	}
	res, err := s.DecideBatch(ctx, campaignID, second.ID, DecisionAccept, nil, "keeper")
	if err != nil {
		t.Fatalf("decide duplicate batch: %v", err)
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entities WHERE campaign_id = ? AND name IN ('Duke Aldric', 'House Vane')`,
		campaignID).Scan(&entities); err != nil {
		t.Fatal(err)
	}
	if entities != 2 {
		t.Fatalf("duplicate batch wrote entities: %d", entities)
	}
	if res.Batch.Status != BatchAccepted {
		t.Fatalf("duplicate batch status = %s", res.Batch.Status)
	}
}

func TestBatch_ModifyOverrideWritesCorrectedPayload(t *testing.T) {
	s, campaignID, _ := batchStore(t)
	ctx := context.Background()

	batch, err := s.StageBatch(ctx, skeletonBatch(campaignID))
	if err != nil {
		t.Fatalf("StageBatch: %v", err)
	}
	aldric := itemByInputID(t, batch, "duke-aldric")
	fixed, _ := json.Marshal(map[string]any{
		"local_id": "duke-aldric", "kind": "npc", "name": "Duchess Aldric",
		"summary": "The aging ruler of the marches.",
	})
	res, err := s.DecideBatch(ctx, campaignID, batch.ID, DecisionAccept,
		[]ItemDecision{{ItemID: aldric.ID, Decision: DecisionModify, Payload: fixed}}, "keeper")
	if err != nil {
		t.Fatalf("DecideBatch with modify: %v", err)
	}
	if res.Batch.Status != BatchAccepted {
		t.Fatalf("batch status = %s", res.Batch.Status)
	}
	outcomes := map[string]BatchItemOutcome{}
	for _, it := range res.Items {
		outcomes[it.ReviewID] = it
	}
	if got := outcomes[aldric.ID].Status; got != ReviewModified {
		t.Fatalf("modified item status = %s", got)
	}
	// The corrected name is what landed — and everything that referenced
	// "Duke Aldric" by name still resolved, because the modify replaced
	// the payload before anything applied and the resolution remembers
	// both the staged and the applied name.
	var name string
	if err := s.db.QueryRowContext(ctx,
		`SELECT name FROM entities WHERE id = ?`, outcomes[aldric.ID].ResultRef).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "Duchess Aldric" {
		t.Fatalf("entity name = %q, want the modified payload's", name)
	}
	if got := outcomes[itemByInputID(t, batch, "control-fact").ID].Status; got != ReviewAccepted {
		t.Fatalf("dependent of modified entity = %s", got)
	}
}

func TestBatch_ListsBatches(t *testing.T) {
	s, campaignID, _ := batchStore(t)
	ctx := context.Background()

	batch, err := s.StageBatch(ctx, skeletonBatch(campaignID))
	if err != nil {
		t.Fatalf("StageBatch: %v", err)
	}
	list, err := s.Batches(ctx, campaignID, BatchOpen)
	if err != nil {
		t.Fatalf("Batches: %v", err)
	}
	if len(list) != 1 || list[0].ID != batch.ID {
		t.Fatalf("open batches = %+v", list)
	}
	if list[0].ItemCount != 6 || list[0].OpenCount != 6 {
		t.Fatalf("counts = %d/%d", list[0].ItemCount, list[0].OpenCount)
	}
	got, err := s.GetBatch(ctx, campaignID, batch.ID)
	if err != nil {
		t.Fatalf("GetBatch: %v", err)
	}
	if len(got.Items) != 6 {
		t.Fatalf("GetBatch items = %d", len(got.Items))
	}
	// Items carry their payload from detail, like an npc_reveal.
	if itemByInputID(t, got, "duke-aldric").Payload["name"] != "Duke Aldric" {
		t.Fatalf("batch item payload missing: %v", itemByInputID(t, got, "duke-aldric").Payload)
	}
	// A foreign campaign's batch is invisible.
	if _, err := s.GetBatch(ctx, "no-such-campaign", batch.ID); err == nil {
		t.Fatal("foreign batch readable")
	}
}
