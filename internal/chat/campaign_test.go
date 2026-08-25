package chat

import (
	"context"
	"encoding/json"
	"testing"
)

// Campaign conversations (MAD-311): pinned to a campaign and a scope, listed
// separately from the rules sidebar, and carrying the campaign citation
// payload on their messages.

func TestCreateInCampaignPinsScope(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if _, err := s.CreateInCampaign(ctx, "u1", "dnd", "", "party", ""); err == nil {
		t.Fatal("an empty campaign id must be refused")
	}
	if _, err := s.CreateInCampaign(ctx, "u1", "dnd", "camp1", "  ", ""); err == nil {
		t.Fatal("an empty scope must be refused")
	}

	c, err := s.CreateInCampaign(ctx, "u1", "dnd", "camp1", "party", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if c.CampaignID != "camp1" || c.Scope != "party" {
		t.Fatalf("pin not recorded: %+v", c)
	}
	got, err := s.Get(ctx, "u1", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CampaignID != "camp1" || got.Scope != "party" {
		t.Fatalf("pin not persisted: %+v", got)
	}
}

func TestListSeparatesRulesAndCampaignThreads(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	rules, _ := s.Create(ctx, "u1", "mtg", "")
	mine, _ := s.CreateInCampaign(ctx, "u1", "dnd", "camp1", "dm", "")
	// Another campaign's thread, same user.
	other, _ := s.CreateInCampaign(ctx, "u1", "dnd", "camp2", "party", "")
	// Another user's thread in the same campaign — invisible.
	s.CreateInCampaign(ctx, "u2", "dnd", "camp1", "dm", "")

	listed, err := s.List(ctx, "u1", 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != rules.ID {
		t.Fatalf("rules sidebar must list only the rules thread: %+v", listed)
	}

	inCamp, err := s.ListInCampaign(ctx, "u1", "camp1", 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(inCamp) != 1 || inCamp[0].ID != mine.ID {
		t.Fatalf("campaign list must be scoped to campaign and user: %+v", inCamp)
	}

	inOther, err := s.ListInCampaign(ctx, "u1", "camp2", 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(inOther) != 1 || inOther[0].ID != other.ID || inOther[0].Scope != "party" {
		t.Fatalf("second campaign thread missing: %+v", inOther)
	}
}

func TestCampaignCitationsRoundTrip(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	c, _ := s.CreateInCampaign(ctx, "u1", "dnd", "camp1", "character:abc", "")
	if _, err := s.AddMessage(ctx, c.ID, RoleUser, "What do we know of the Duke?", nil, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	payload := json.RawMessage(`{"facts":[{"id":"f1","statement":"The Duke rules the northern marches."}]}`)
	if _, err := s.AddMessage(ctx, c.ID, RoleAssistant, "He rules the marches.", nil, nil, nil, nil, payload); err != nil {
		t.Fatal(err)
	}
	msgs, err := s.Messages(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || string(msgs[1].Campaign) != string(payload) {
		t.Fatalf("campaign payload did not round-trip: %+v", msgs)
	}

	// History drops the payload like every other citation block.
	hist, err := s.History(ctx, c.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 2 || hist[1].Campaign != nil {
		t.Fatalf("history must strip the campaign payload: %+v", hist)
	}
}
