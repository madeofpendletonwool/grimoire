package canon

// The rumour mill's tests (MAD-374): the two health checks pure over
// hand-built snapshots, the rumour clue path clearing unreachable_secret,
// and the generator's arithmetic asserted with the model faked — the truth
// mix honoured exactly, true rumours drawn from real secret facts, false
// ones filtered off facts the party holds.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
)

/* ---------- the checks ---------- */

func TestRumorOrphan(t *testing.T) {
	build := func(status string, holders int) *Snapshot {
		s := pureSnap(baseGraph())
		s.Rumors = []campaign.Rumor{{
			ID: "r1", CampaignID: "c1", Statement: "the miller's boy came back wrong",
			Truth: campaign.RumorTruthFalse, Status: status,
		}}
		for i := 0; i < holders; i++ {
			s.RumorHolders = append(s.RumorHolders, campaign.RumorHolder{RumorID: "r1", EntityID: string(rune('a' + i))})
		}
		return s
	}

	// A circulating rumour nobody repeats can never be heard.
	if n := has(CheckSnapshot(build(campaign.RumorStatusCirculating, 0), DefaultCheckOptions()), CheckRumorOrphan, "r1"); n != 1 {
		t.Fatalf("holderless circulating rumour: got %d findings, want 1", n)
	}
	// A holder gives it a mouth.
	if n := has(CheckSnapshot(build(campaign.RumorStatusCirculating, 1), DefaultCheckOptions()), CheckRumorOrphan, "r1"); n != 0 {
		t.Fatalf("held rumour: got %d findings, want 0", n)
	}
	// Nobody repeating a debunked rumour is the rumour having died.
	if n := has(CheckSnapshot(build(campaign.RumorStatusDebunked, 0), DefaultCheckOptions()), CheckRumorOrphan, "r1"); n != 0 {
		t.Fatalf("debunked rumour: got %d findings, want 0", n)
	}
	if n := has(CheckSnapshot(build(campaign.RumorStatusDormant, 0), DefaultCheckOptions()), CheckRumorOrphan, "r1"); n != 0 {
		t.Fatalf("dormant rumour: got %d findings, want 0", n)
	}
}

func TestRumorDeadEnd(t *testing.T) {
	build := func(liveStoryline bool) *Snapshot {
		g := baseGraph()
		canonFact(g, "f1", campaign.VisibilityPublic)
		s := pureSnap(g)
		about := "duke" // the base graph's npc: a fact touches him, so he has a storyline
		if !liveStoryline {
			about = "thalia"
			// Thalia's only live reference is the fact above about the
			// duke; strip it by pointing the rumour at an entity nothing
			// touches — give the graph a bare location.
			g.Entities = append(g.Entities, campaign.Entity{ID: "ruin", CampaignID: "c1", Kind: campaign.KindLocation, Name: "The Ruin", Status: campaign.StatusActive})
			about = "ruin"
		}
		s.Rumors = []campaign.Rumor{{
			ID: "r1", CampaignID: "c1", Statement: "the duke digs at night",
			Truth: campaign.RumorTruthFalse, AboutEntity: about, Status: campaign.RumorStatusCirculating,
		}}
		s.RumorHolders = []campaign.RumorHolder{{RumorID: "r1", EntityID: "thalia"}}
		return s
	}

	if n := has(CheckSnapshot(build(true), DefaultCheckOptions()), CheckRumorDeadEnd, "r1"); n != 0 {
		t.Fatalf("rumour about a live subject: got %d findings, want 0", n)
	}
	if n := has(CheckSnapshot(build(false), DefaultCheckOptions()), CheckRumorDeadEnd, "r1"); n != 1 {
		t.Fatalf("rumour about a dead-end subject: got %d findings, want 1", n)
	}
	// The dead-end finding is info severity: it rides the report, never
	// the ledger.
	findings := CheckSnapshot(build(false), DefaultCheckOptions())
	for _, f := range findings {
		if f.Check == CheckRumorDeadEnd && f.Severity != campaign.SeverityInfo {
			t.Fatalf("rumor_dead_end severity = %v, want info", f.Severity)
		}
	}
}

func TestRumorIsACluePathForUnreachableSecret(t *testing.T) {
	build := func(status string, withFact bool) *Snapshot {
		g := baseGraph()
		canonFact(g, "f1", campaign.VisibilitySecret)
		s := pureSnap(g)
		if withFact {
			r := campaign.Rumor{ID: "r1", CampaignID: "c1", Statement: "the key opens the crypt",
				Truth: campaign.RumorTruthTrue, FactID: "f1", Status: status}
			s.Rumors = []campaign.Rumor{r}
		}
		return s
	}

	// Without the rumour the secret is unreachable.
	if n := has(CheckSnapshot(build("", false), DefaultCheckOptions()), CheckUnreachableSecret, "f1"); n != 1 {
		t.Fatalf("plain secret: got %d findings, want 1", n)
	}
	// A circulating rumour attesting the secret is the clue path — the
	// health report's "this secret is unreachable" gains its button.
	if n := has(CheckSnapshot(build(campaign.RumorStatusCirculating, true), DefaultCheckOptions()), CheckUnreachableSecret, "f1"); n != 0 {
		t.Fatalf("rumoured secret: got %d findings, want 0", n)
	}
	// Confirmed works too; debunked and dormant are not paths.
	if n := has(CheckSnapshot(build(campaign.RumorStatusConfirmed, true), DefaultCheckOptions()), CheckUnreachableSecret, "f1"); n != 0 {
		t.Fatalf("confirmed rumour: got %d findings, want 0", n)
	}
	if n := has(CheckSnapshot(build(campaign.RumorStatusDebunked, true), DefaultCheckOptions()), CheckUnreachableSecret, "f1"); n != 1 {
		t.Fatalf("debunked rumour: got %d findings, want 1", n)
	}
	if n := has(CheckSnapshot(build(campaign.RumorStatusDormant, true), DefaultCheckOptions()), CheckUnreachableSecret, "f1"); n != 1 {
		t.Fatalf("dormant rumour: got %d findings, want 1", n)
	}
}

/* ---------- the generator, model faked ---------- */

// rumorBatchStore wires the batch path the way the server runs it: canon
// over a seeded campaign with the graph stores attached.
func rumorBatchStore(t *testing.T) (*Store, string, *campaign.Fixture) {
	t.Helper()
	return batchStore(t)
}

func TestGenerateRumorsHonorsTheMixExactly(t *testing.T) {
	s, cid, fx := rumorBatchStore(t)

	// The seed's one secret fact touching the monastery is the Silver
	// Key fact (object = monastery); nothing holds it, so the false and
	// distorted slots can both draw from it too.
	fill := map[string]any{
		"t1": "Pilgrims say a silver key in the chapel opens the crypt under the monastery.",
		"f1": "The monks under the monastery were all run out years ago, they say.",
		"d1": "They say the silver key opens the monastery's treasure vault.",
	}
	s.model = &fakeModel{responses: []string{rumorJSON(t, fill)}}

	res, err := s.GenerateRumors(context.Background(), RumorDesignInput{
		CampaignID: cid, About: fx.Monastery,
		TrueCount: 1, FalseCount: 1, DistortedCount: 1, CreatedBy: "dm",
	})
	if err != nil {
		t.Fatalf("GenerateRumors: %v", err)
	}
	if res.Batch == nil || res.Batch.ItemCount != 3 {
		t.Fatalf("batch items = %d, want exactly the requested mix of 3", res.Batch.ItemCount)
	}
	if res.Batch.Source != BatchSourceRumor {
		t.Fatalf("source = %s, want rumor", res.Batch.Source)
	}

	var truths, withFact, trueFacts []string
	for _, it := range res.Batch.Items {
		if it.Kind != ReviewProposedRumor {
			t.Fatalf("item kind = %s, want proposed_rumor", it.Kind)
		}
		var p map[string]any
		if err := json.Unmarshal([]byte(it.Detail), &p); err != nil {
			t.Fatal(err)
		}
		truth, _ := p["truth"].(string)
		truths = append(truths, truth)
		if fact, _ := p["fact_id"].(string); fact != "" {
			withFact = append(withFact, fact)
			if truth == campaign.RumorTruthTrue {
				trueFacts = append(trueFacts, fact)
			}
		}
	}
	// The mix, exactly: one of each.
	counted := map[string]int{}
	for _, tr := range truths {
		counted[tr]++
	}
	if counted[campaign.RumorTruthTrue] != 1 || counted[campaign.RumorTruthFalse] != 1 || counted[campaign.RumorTruthDistorted] != 1 {
		t.Fatalf("truth mix = %v, want 1/1/1", counted)
	}
	// Every true rumour points at a fact that already exists — the seed's
	// secret key fact, drawn from the campaign itself.
	if len(trueFacts) != 1 || trueFacts[0] != fx.FactKeyOpensCrypt {
		t.Fatalf("true rumour fact = %v, want the seed's secret %s", trueFacts, fx.FactKeyOpensCrypt)
	}
	// The distorted one names the fact it distorts.
	distortedNamed := 0
	for _, it := range res.Batch.Items {
		var p map[string]any
		if err := json.Unmarshal([]byte(it.Detail), &p); err != nil {
			t.Fatal(err)
		}
		if p["truth"] == campaign.RumorTruthDistorted && p["fact_id"] != "" && p["fact_id"] != nil {
			distortedNamed++
		}
	}
	if distortedNamed != 1 {
		t.Fatalf("distorted rumours naming their fact = %d, want 1", distortedNamed)
	}
	// Distribution was a join: Tom lives in Blackwater, the monastery is
	// in Blackwater, so the mill proposes him as a voice.
	holderItems := 0
	for _, it := range res.Batch.Items {
		var p map[string]any
		if err := json.Unmarshal([]byte(it.Detail), &p); err != nil {
			t.Fatal(err)
		}
		if hs, ok := p["holders"].([]any); ok && len(hs) > 0 {
			holderItems++
			h0 := hs[0].(map[string]any)
			if h0["entity"] != fx.Tom {
				t.Fatalf("holder = %v, want the seed's Blackwater npc %s", h0["entity"], fx.Tom)
			}
		}
	}
	if holderItems == 0 {
		t.Fatal("no rumour carried holders; the distribution join never ran")
	}
}

func TestGenerateRumorsFiltersPartyHeldFacts(t *testing.T) {
	s, cid, fx := rumorBatchStore(t)
	ctx := context.Background()

	// Without the grant, the false rumour contradicts the mines fact —
	// the only live, non-contested fact about the Duke.
	s.model = &fakeModel{responses: []string{rumorJSON(t, map[string]any{
		"f1": "They say the Duke sold the mines to the crown.",
	})}}
	res, err := s.GenerateRumors(ctx, RumorDesignInput{
		CampaignID: cid, About: fx.Duke, FalseCount: 1, CreatedBy: "dm",
	})
	if err != nil {
		t.Fatalf("GenerateRumors: %v", err)
	}
	var p map[string]any
	if err := json.Unmarshal([]byte(res.Batch.Items[0].Detail), &p); err != nil {
		t.Fatal(err)
	}
	if p["fact_id"] != fx.FactMinesOwned {
		t.Fatalf("unfiltered fact_id = %v, want %s", p["fact_id"], fx.FactMinesOwned)
	}

	// The party learns the mines fact (the charter discovery); now a
	// false rumour contradicting it is instantly disprovable noise, so
	// the pool is empty and the rumour is invented whole — the mix is
	// still honoured, nothing is drawn from held facts.
	if _, err := s.knowledge.SetAwareness(ctx, cid,
		campaign.PartyKnower, fx.FactMinesOwned, "knows", 1, "", ""); err != nil {
		t.Fatalf("grant party knowledge: %v", err)
	}
	s.model = &fakeModel{responses: []string{rumorJSON(t, map[string]any{
		"f1": "They say the Duke's steward sells the mines' silver himself.",
	})}}
	res, err = s.GenerateRumors(ctx, RumorDesignInput{
		CampaignID: cid, About: fx.Duke, FalseCount: 1, CreatedBy: "dm",
	})
	if err != nil {
		t.Fatalf("GenerateRumors after grant: %v", err)
	}
	var p2 map[string]any
	if err := json.Unmarshal([]byte(res.Batch.Items[0].Detail), &p2); err != nil {
		t.Fatal(err)
	}
	if fact, _ := p2["fact_id"].(string); fact != "" {
		t.Fatalf("a false rumour drew %s — the fact the party holds; instantly disprovable noise", fact)
	}
}

func TestGenerateRumorsRefusesUnbackableTrueRumors(t *testing.T) {
	s, cid, fx := rumorBatchStore(t)
	// The tomb (the key fact's subject) has no secret fact about it the
	// way the monastery does; a request for two true rumours about the
	// monastery — which has one — is refused with the count.
	_, err := s.GenerateRumors(context.Background(), RumorDesignInput{
		CampaignID: cid, About: fx.Monastery,
		TrueCount: 2, CreatedBy: "dm",
	})
	if err == nil {
		t.Fatal("a true rumour count above the campaign's secret facts must be refused, never padded")
	}
	if !strings.Contains(err.Error(), "secret fact") {
		t.Fatalf("refusal = %v, want the count in the message", err)
	}
}

func TestGenerateRumorsAcceptsIntoTheMill(t *testing.T) {
	s, cid, fx := rumorBatchStore(t)
	// Before: the key secret is unreachable, and the flag is open.
	if _, err := s.CheckCampaign(context.Background(), cid, DefaultCheckOptions()); err != nil {
		t.Fatal(err)
	}
	fill := map[string]any{
		"t1": "Pilgrims say a silver key in the chapel opens the crypt under the monastery.",
	}
	s.model = &fakeModel{responses: []string{rumorJSON(t, fill)}}
	res, err := s.GenerateRumors(context.Background(), RumorDesignInput{
		CampaignID: cid, About: fx.Monastery, TrueCount: 1, CreatedBy: "dm",
	})
	if err != nil {
		t.Fatalf("GenerateRumors: %v", err)
	}
	// One accept decides the one-item batch; the rumour lands in the mill
	// through the same apply path every accepted batch item takes.
	if _, err := s.DecideBatch(context.Background(), cid, res.Batch.ID, DecisionAccept, nil, "dm"); err != nil {
		t.Fatalf("DecideBatch: %v", err)
	}
	// The accepted rumour attests the secret, so the check stops
	// reporting it and the open flag clears.
	flags, err := s.CheckCampaign(context.Background(), cid, DefaultCheckOptions())
	if err != nil {
		t.Fatal(err)
	}
	f := flagFor(t, flags, CheckUnreachableSecret, fx.FactKeyOpensCrypt)
	if f.Status != FlagCleared {
		t.Fatalf("unreachable_secret flag = %s after the rumour landed, want cleared", f.Status)
	}
	// The mill itself holds it, holders attached.
	rumors, err := s.knowledge.Rumors(context.Background(), campaign.ScopeDM, cid, knowledge.RumorFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rumors) != 1 || rumors[0].Truth != campaign.RumorTruthTrue {
		t.Fatalf("mill after accept = %+v, want one true rumour", rumors)
	}
	holders, err := s.knowledge.Holders(context.Background(), campaign.ScopeDM, cid, rumors[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(holders) == 0 {
		t.Fatal("accepted rumour landed with no holders; the distribution never applied")
	}
}

func rumorJSON(t *testing.T, v map[string]any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
