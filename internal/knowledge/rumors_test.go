package knowledge

// The rumour mill's tests (MAD-374): the heard rules (a true rumour leaves
// suspects, a false one leaves believes_false, knowledge is never
// downgraded by gossip, a fact-less rumour is carried), the transition
// table's refusal of an illegal stance move, and the scope line — truth,
// fact_id and DM-only rumours never reach a non-DM scope.

import (
	"context"
	"errors"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
)

// seedRumors plants the mill the tests read: a true rumour attesting the
// secret key fact, a false one naming the fact it contradicts, a fact-less
// false one, and one DM-only rumour.
func seedRumors(t *testing.T, s *Store, fx *campaign.Fixture) (trueR, falseR, bareR, dmOnlyR *campaign.Rumor) {
	t.Helper()
	ctx := context.Background()
	cid := fx.Campaign.ID
	var err error
	trueR, err = s.CreateRumor(ctx, cid, RumorInput{
		Statement: "They say the silver key in the chapel treasury opens something under the monastery.",
		Truth:     campaign.RumorTruthTrue, AboutEntity: fx.Monastery, FactID: fx.FactKeyOpensCrypt,
		Origin: "the miller heard it from a pilgrim", Spread: campaign.RumorSpreadRegional,
		CreatedBy: "keeper",
	})
	if err != nil {
		t.Fatalf("seed true rumor: %v", err)
	}
	falseR, err = s.CreateRumor(ctx, cid, RumorInput{
		Statement: "The Duke's steward was seen buying silver by the pound.",
		Truth:     campaign.RumorTruthFalse, AboutEntity: fx.Duke, FactID: fx.FactMinesOwned,
		Origin: "overheard at the Waystone", CreatedBy: "keeper",
	})
	if err != nil {
		t.Fatalf("seed false rumor: %v", err)
	}
	bareR, err = s.CreateRumor(ctx, cid, RumorInput{
		Statement: "The miller's second son went into the trees and came back wrong.",
		Truth:     campaign.RumorTruthFalse, AboutEntity: fx.Blackwater,
		Origin: "talk at the inn", CreatedBy: "keeper",
	})
	if err != nil {
		t.Fatalf("seed bare rumor: %v", err)
	}
	dmOnlyR, err = s.CreateRumor(ctx, cid, RumorInput{
		Statement: "The thing in the trees pays the reeve in old coin.",
		Truth:     campaign.RumorTruthTrue, AboutEntity: fx.Blackwater, DMOnly: true,
		CreatedBy: "keeper",
	})
	if err != nil {
		t.Fatalf("seed dm-only rumor: %v", err)
	}
	return trueR, falseR, bareR, dmOnlyR
}

func TestRumorHeardTrueLeavesSuspects(t *testing.T) {
	s, fx, _ := seeded(t)
	ctx := context.Background()
	trueR, _, _, _ := seedRumors(t, s, fx)

	res, err := s.RumorHeard(ctx, fx.Campaign.ID, trueR.ID, fx.Thalia, fx.EventAmbush)
	if err != nil {
		t.Fatalf("heard: %v", err)
	}
	if res.Outcome != RumorHeardGranted || res.Stance != StanceSuspects {
		t.Fatalf("outcome = %s stance = %s, want granted/suspects", res.Outcome, res.Stance)
	}
	rows, err := s.Awareness(ctx, ScopeDM, fx.Campaign.ID, fx.Thalia, fx.FactKeyOpensCrypt)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Stance != StanceSuspects {
		t.Fatalf("awareness = %+v, want one suspects row", rows)
	}
	if rows[0].SinceEvent != fx.EventAmbush {
		t.Fatalf("since_event = %q, want %q", rows[0].SinceEvent, fx.EventAmbush)
	}
}

func TestRumorHeardFalseLeavesBelievesFalse(t *testing.T) {
	s, fx, _ := seeded(t)
	ctx := context.Background()
	_, falseR, _, _ := seedRumors(t, s, fx)

	res, err := s.RumorHeard(ctx, fx.Campaign.ID, falseR.ID, fx.Bran, "")
	if err != nil {
		t.Fatalf("heard: %v", err)
	}
	if res.Outcome != RumorHeardGranted || res.Stance != StanceBelievesFalse {
		t.Fatalf("outcome = %s stance = %s, want granted/believes_false", res.Outcome, res.Stance)
	}
	rows, err := s.Awareness(ctx, ScopeDM, fx.Campaign.ID, fx.Bran, fx.FactMinesOwned)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Stance != StanceBelievesFalse {
		t.Fatalf("awareness = %+v, want one believes_false row", rows)
	}
}

func TestRumorHeardNeverDowngradesKnowledge(t *testing.T) {
	s, fx, _ := seeded(t)
	ctx := context.Background()
	// Keth already knows the mines fact — Elara-level knowledge, DM
	// granted. Gossip to the contrary must leave him alone.
	if _, err := s.SetAwareness(ctx, fx.Campaign.ID, fx.Keth, fx.FactMinesOwned,
		StanceKnows, 1, "", ""); err != nil {
		t.Fatalf("grant knowledge: %v", err)
	}
	_, falseR, _, _ := seedRumors(t, s, fx)

	res, err := s.RumorHeard(ctx, fx.Campaign.ID, falseR.ID, fx.Keth, "")
	if err != nil {
		t.Fatalf("heard: %v", err)
	}
	if res.Outcome != RumorHeardKnowsAlready {
		t.Fatalf("outcome = %s, want knows_already", res.Outcome)
	}
	rows, err := s.Awareness(ctx, ScopeDM, fx.Campaign.ID, fx.Keth, fx.FactMinesOwned)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Stance != StanceKnows {
		t.Fatalf("knowledge downgraded by gossip: %+v", rows)
	}
}

func TestRumorHeardFactlessRumorIsCarried(t *testing.T) {
	s, fx, _ := seeded(t)
	ctx := context.Background()
	_, _, bareR, _ := seedRumors(t, s, fx)

	res, err := s.RumorHeard(ctx, fx.Campaign.ID, bareR.ID, campaign.PartyKnower, fx.EventAmbush)
	if err != nil {
		t.Fatalf("heard: %v", err)
	}
	if res.Outcome != RumorHeardCarried {
		t.Fatalf("outcome = %s, want carried", res.Outcome)
	}
	holders, err := s.Holders(ctx, ScopeDM, fx.Campaign.ID, bareR.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(holders) != 1 || holders[0].EntityID != campaign.PartyKnower {
		t.Fatalf("holders = %+v, want the party carrying it", holders)
	}
}

func TestRumorHeardRepeatIsUnchanged(t *testing.T) {
	s, fx, _ := seeded(t)
	ctx := context.Background()
	trueR, _, _, _ := seedRumors(t, s, fx)
	if _, err := s.RumorHeard(ctx, fx.Campaign.ID, trueR.ID, fx.Thalia, ""); err != nil {
		t.Fatal(err)
	}
	res, err := s.RumorHeard(ctx, fx.Campaign.ID, trueR.ID, fx.Thalia, "")
	if err != nil {
		t.Fatalf("second heard: %v", err)
	}
	if res.Outcome != RumorHeardUnchanged {
		t.Fatalf("outcome = %s, want unchanged", res.Outcome)
	}
}

func TestIllegalStanceMoveIsRefusedNotWritten(t *testing.T) {
	s, fx, _ := seeded(t)
	ctx := context.Background()
	if CanTransition(StanceKnows, StanceUnaware) {
		t.Fatal("knows -> unaware must be illegal: a character does not un-learn")
	}
	if _, err := s.SetAwareness(ctx, fx.Campaign.ID, fx.Thalia, fx.FactMinesOwned,
		StanceKnows, 1, "", ""); err != nil {
		t.Fatalf("grant: %v", err)
	}
	_, err := s.SetAwareness(ctx, fx.Campaign.ID, fx.Thalia, fx.FactMinesOwned,
		StanceUnaware, 1, "", "")
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("illegal move err = %v, want ErrInvalid", err)
	}
	rows, err := s.Awareness(ctx, ScopeDM, fx.Campaign.ID, fx.Thalia, fx.FactMinesOwned)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Stance != StanceKnows {
		t.Fatalf("a refused move must not write: %+v", rows)
	}
}

func TestRumorScopesNeverCarryTruth(t *testing.T) {
	s, fx, _ := seeded(t)
	ctx := context.Background()
	trueR, _, _, dmOnlyR := seedRumors(t, s, fx)

	// The DM reads everything.
	dm, err := s.Rumors(ctx, ScopeDM, fx.Campaign.ID, RumorFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(dm) != 4 {
		t.Fatalf("dm rumors = %d, want 4", len(dm))
	}
	dmRumor, err := s.Rumor(ctx, ScopeDM, fx.Campaign.ID, trueR.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dmRumor.Truth != campaign.RumorTruthTrue || dmRumor.FactID != fx.FactKeyOpensCrypt {
		t.Fatalf("dm read lost truth/fact: %+v", dmRumor)
	}

	// Every non-DM scope reads the statement and never the columns.
	for _, scope := range []Scope{ScopeParty, ScopeCharacter(fx.Thalia), ScopeNPC(fx.Tom)} {
		list, err := s.Rumors(ctx, scope, fx.Campaign.ID, RumorFilter{})
		if err != nil {
			t.Fatalf("%s: %v", scope, err)
		}
		for _, r := range list {
			if r.ID == dmOnlyR.ID {
				t.Fatalf("%s: a DM-only rumour reached the scope", scope)
			}
			if r.Truth != "" || r.FactID != "" {
				t.Fatalf("%s: leak — truth=%q fact_id=%q", scope, r.Truth, r.FactID)
			}
		}
		one, err := s.Rumor(ctx, scope, fx.Campaign.ID, trueR.ID)
		if err != nil {
			t.Fatalf("%s single: %v", scope, err)
		}
		if one.Truth != "" || one.FactID != "" {
			t.Fatalf("%s single-read leak — truth=%q fact_id=%q", scope, one.Truth, one.FactID)
		}
		// The DM-only rumour is indistinguishable from a missing one.
		if _, err := s.Rumor(ctx, scope, fx.Campaign.ID, dmOnlyR.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s: dm-only read err = %v, want ErrNotFound", scope, err)
		}
		// The truth filter is the DM's question.
		if _, err := s.Rumors(ctx, scope, fx.Campaign.ID, RumorFilter{Truth: campaign.RumorTruthFalse}); !errors.Is(err, ErrScope) {
			t.Fatalf("%s: truth filter err = %v, want ErrScope", scope, err)
		}
	}
}

func TestRumorHolderScopesFollowTheRumor(t *testing.T) {
	s, fx, _ := seeded(t)
	ctx := context.Background()
	trueR, _, _, dmOnlyR := seedRumors(t, s, fx)
	if _, err := s.SetRumorHolder(ctx, fx.Campaign.ID, trueR.ID, fx.Tom, "key opens a tomb, he says", ""); err != nil {
		t.Fatalf("holder: %v", err)
	}
	if _, err := s.SetRumorHolder(ctx, fx.Campaign.ID, dmOnlyR.ID, fx.Tom, "", ""); err != nil {
		t.Fatalf("dm-only holder: %v", err)
	}

	tom, err := s.Holders(ctx, ScopeNPC(fx.Tom), fx.Campaign.ID, trueR.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tom) != 1 || tom[0].Variant != "key opens a tomb, he says" {
		t.Fatalf("npc holders = %+v", tom)
	}
	if list, err := s.Holders(ctx, ScopeParty, fx.Campaign.ID, dmOnlyR.ID); err != nil || len(list) != 0 {
		t.Fatalf("dm-only holders at party scope = %+v err = %v, want none", list, err)
	}

	// The upsert is the variant drifting, not a second row.
	if _, err := s.SetRumorHolder(ctx, fx.Campaign.ID, trueR.ID, fx.Tom, "opens the crypt, he says now", ""); err != nil {
		t.Fatal(err)
	}
	tom, err = s.Holders(ctx, ScopeDM, fx.Campaign.ID, trueR.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tom) != 1 || tom[0].Variant != "opens the crypt, he says now" {
		t.Fatalf("variant drift = %+v", tom)
	}

	if err := s.RemoveRumorHolder(ctx, fx.Campaign.ID, trueR.ID, fx.Tom); err != nil {
		t.Fatal(err)
	}
	if list, err := s.Holders(ctx, ScopeDM, fx.Campaign.ID, trueR.ID); err != nil || len(list) != 0 {
		t.Fatalf("holders after removal = %+v err = %v", list, err)
	}
}

func TestRumorLifecycle(t *testing.T) {
	s, fx, _ := seeded(t)
	ctx := context.Background()
	trueR, _, _, _ := seedRumors(t, s, fx)

	debunked := campaign.RumorStatusDebunked
	updated, err := s.UpdateRumor(ctx, fx.Campaign.ID, trueR.ID, RumorUpdate{Status: &debunked})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Status != campaign.RumorStatusDebunked {
		t.Fatalf("status = %s", updated.Status)
	}
	if err := s.DeleteRumor(ctx, fx.Campaign.ID, trueR.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.Rumor(ctx, ScopeDM, fx.Campaign.ID, trueR.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted rumor err = %v, want ErrNotFound", err)
	}

	// Shape validation surfaces as ErrInvalid before the constraint does.
	if _, err := s.CreateRumor(ctx, fx.Campaign.ID, RumorInput{Statement: "x", Truth: "maybe"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad truth err = %v, want ErrInvalid", err)
	}
	if _, err := s.CreateRumor(ctx, fx.Campaign.ID, RumorInput{Statement: "  ", Truth: campaign.RumorTruthTrue}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty statement err = %v, want ErrInvalid", err)
	}
	if _, err := s.CreateRumor(ctx, fx.Campaign.ID, RumorInput{
		Statement: "x", Truth: campaign.RumorTruthTrue, FactID: "no-such-fact"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign fact err = %v, want ErrNotFound", err)
	}
}
