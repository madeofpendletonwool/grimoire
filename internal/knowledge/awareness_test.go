package knowledge

import (
	"context"
	"errors"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
)

func TestCanTransition(t *testing.T) {
	cases := []struct {
		from, to string
		ok       bool
	}{
		// Learning, from nothing.
		{"", StanceKnows, true},
		{"", StanceSuspects, true},
		{"", StanceBelievesFalse, true},
		{"", StanceUnaware, true},
		// Learning, from unaware.
		{StanceUnaware, StanceKnows, true},
		{StanceUnaware, StanceSuspects, true},
		{StanceUnaware, StanceBelievesFalse, true},
		{StanceUnaware, StanceUnaware, true},
		// Confirmation and correction.
		{StanceSuspects, StanceKnows, true},
		{StanceSuspects, StanceBelievesFalse, true},
		{StanceSuspects, StanceSuspects, true},
		{StanceBelievesFalse, StanceKnows, true},
		{StanceBelievesFalse, StanceSuspects, true},
		{StanceBelievesFalse, StanceBelievesFalse, true},
		// Doubt re-opens a settled belief; deception can overturn knowledge.
		{StanceKnows, StanceSuspects, true},
		{StanceKnows, StanceBelievesFalse, true},
		{StanceKnows, StanceKnows, true},
		// Nothing un-learns. A fact that stops being true is retconned
		// through SupersedeFact, not through the awareness rows.
		{StanceKnows, StanceUnaware, false},
		{StanceSuspects, StanceUnaware, false},
		{StanceBelievesFalse, StanceUnaware, false},
		// Unknown stances never validate.
		{"maybe", StanceKnows, false},
		{StanceKnows, "maybe", false},
	}
	for _, c := range cases {
		if got := CanTransition(c.from, c.to); got != c.ok {
			t.Errorf("CanTransition(%q, %q) = %v, want %v", c.from, c.to, got, c.ok)
		}
	}
}

func TestSetAwarenessEnforcesTransitions(t *testing.T) {
	s, fx, _ := seeded(t)
	ctx := context.Background()
	cid := fx.Campaign.ID

	// unaware -> knows: the discovery path.
	if _, err := s.SetAwareness(ctx, cid, fx.Bran, fx.FactMinesOwned, StanceKnows, 0.9, "", ""); err != nil {
		t.Fatalf("unaware -> knows: %v", err)
	}
	// knows -> suspects: doubt re-opens it.
	if _, err := s.SetAwareness(ctx, cid, fx.Bran, fx.FactMinesOwned, StanceSuspects, 0.5, "", ""); err != nil {
		t.Fatalf("knows -> suspects: %v", err)
	}
	// suspects -> believes_false: correction.
	if _, err := s.SetAwareness(ctx, cid, fx.Bran, fx.FactMinesOwned, StanceBelievesFalse, 0.7, "", ""); err != nil {
		t.Fatalf("suspects -> believes_false: %v", err)
	}
	// believes_false -> knows: they finally have it right.
	if _, err := s.SetAwareness(ctx, cid, fx.Bran, fx.FactMinesOwned, StanceKnows, 1, "", ""); err != nil {
		t.Fatalf("believes_false -> knows: %v", err)
	}
	// knows -> unaware: not a thing.
	_, err := s.SetAwareness(ctx, cid, fx.Bran, fx.FactMinesOwned, StanceUnaware, 1, "", "")
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("knows -> unaware must be ErrInvalid, got %v", err)
	}
	// The row still holds the last legal stance.
	rows, err := s.Awareness(ctx, ScopeDM, cid, fx.Bran, fx.FactMinesOwned)
	if err != nil || len(rows) != 1 || rows[0].Stance != StanceKnows {
		t.Fatalf("bran should still hold knows: %v %v", rows, err)
	}
}

func TestSetAwarenessValidates(t *testing.T) {
	s, fx, _ := seeded(t)
	ctx := context.Background()
	cid := fx.Campaign.ID

	if _, err := s.SetAwareness(ctx, cid, fx.Bran, fx.FactMinesOwned, "maybe", 1, "", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid stance: %v", err)
	}
	if _, err := s.SetAwareness(ctx, cid, fx.Bran, fx.FactMinesOwned, StanceKnows, 1.5, "", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("confidence 1.5: %v", err)
	}
	if _, err := s.SetAwareness(ctx, cid, fx.Bran, fx.FactMinesOwned, StanceKnows, -0.1, "", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("confidence -0.1: %v", err)
	}
	// A knower from another campaign reads as missing.
	other, err := campaign.New(s.db)
	if err != nil {
		t.Fatal(err)
	}
	userIDs(t, s.db, "keeper2")
	c2, err := other.CreateCampaign(ctx, "keeper2", "Elsewhere", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetAwareness(ctx, c2.ID, fx.Bran, fx.FactMinesOwned, StanceKnows, 1, "", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-campaign campaign id must read as missing: %v", err)
	}
	if _, err := s.SetAwareness(ctx, cid, "ghost", fx.FactMinesOwned, StanceKnows, 1, "", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown knower: %v", err)
	}
	if _, err := s.SetAwareness(ctx, cid, fx.Bran, "ghost-fact", StanceKnows, 1, "", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown fact: %v", err)
	}
	if _, err := s.SetAwareness(ctx, cid, fx.Bran, fx.FactMinesOwned, StanceKnows, 1, "ghost-event", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown event: %v", err)
	}
}

func TestAwarenessListingScopesToKnowers(t *testing.T) {
	s, fx, _ := seeded(t)
	ctx := context.Background()
	cid := fx.Campaign.ID

	party, err := s.Awareness(ctx, ScopeParty, cid, "", "")
	if err != nil {
		t.Fatalf("party awareness: %v", err)
	}
	for _, a := range party {
		if a.Knower != campaign.PartyKnower {
			t.Fatalf("party scope must not read %s's rows", a.Knower)
		}
	}
	thalia, err := s.Awareness(ctx, ScopeCharacter(fx.Thalia), cid, "", "")
	if err != nil {
		t.Fatalf("thalia awareness: %v", err)
	}
	if len(thalia) == 0 {
		t.Fatal("character scope sees own rows plus party rows; got none")
	}
	for _, a := range thalia {
		if a.Knower != fx.Thalia && a.Knower != campaign.PartyKnower {
			t.Fatalf("character scope must not read %s's rows", a.Knower)
		}
	}
	// One character may not read another's mind.
	if _, err := s.Awareness(ctx, ScopeCharacter(fx.Thalia), cid, fx.Mira, ""); !errors.Is(err, ErrScope) {
		t.Fatalf("reading mira's awareness as thalia must be ErrScope, got %v", err)
	}
	// The DM reads everything.
	dm, err := s.Awareness(ctx, ScopeDM, cid, "", "")
	if err != nil || len(dm) < 6 {
		t.Fatalf("dm awareness should see every row: %d %v", len(dm), err)
	}
}
