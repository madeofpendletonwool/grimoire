package knowledge

import (
	"context"
	"errors"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
)

// The handout store (MAD-490): the lifecycle writes and the scoped reads.
// The contract under test is the one the portal runs on — status is
// authorization in the SQL, so a draft or retired row is unreachable from
// any non-DM scope the way a secret fact is unreachable without its
// awareness grant.
func TestHandoutLifecycleAndScopedReads(t *testing.T) {
	s, fx, _ := seeded(t)
	ctx := context.Background()
	cid := fx.Campaign.ID

	// A map without its image cannot be published; a text handout can.
	letters, err := s.CreateHandout(ctx, cid, HandoutInput{
		Kind: campaign.HandoutKindHandout, Title: "The steward's letter",
		Body: "The Duke holds the Eastern Mines through a steward.",
	})
	if err != nil {
		t.Fatalf("create handout: %v", err)
	}
	if letters.Status != campaign.HandoutStatusDraft {
		t.Fatalf("a fresh handout is a draft, got %s", letters.Status)
	}
	if !letters.PublishedAt.IsZero() {
		t.Fatal("a draft carries no hand-out stamp")
	}

	// Publishing an imageless map is refused with the shape named.
	valley, err := s.CreateHandout(ctx, cid, HandoutInput{Kind: campaign.HandoutKindMap, Title: "The valley"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetHandoutStatus(ctx, cid, valley.ID, campaign.HandoutStatusPublished); !errors.Is(err, ErrInvalid) {
		t.Fatalf("publishing an imageless map: %v", err)
	}
	if _, err := s.SetHandoutImage(ctx, cid, valley.ID, "map.png", "image/png", 2048); err != nil {
		t.Fatal(err)
	}
	valley, err = s.SetHandoutStatus(ctx, cid, valley.ID, campaign.HandoutStatusPublished)
	if err != nil {
		t.Fatalf("publish map: %v", err)
	}
	if valley.Status != campaign.HandoutStatusPublished || valley.PublishedAt.IsZero() {
		t.Fatalf("published map = %+v", valley)
	}

	// The scoped reads: the DM sees drafts and published rows; a party
	// scope sees the published one alone — list and single both.
	published, err := s.CreateHandout(ctx, cid, HandoutInput{
		Kind: campaign.HandoutKindHandout, Title: "The charter", Body: "Be it known…",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetHandoutStatus(ctx, cid, published.ID, campaign.HandoutStatusPublished); err != nil {
		t.Fatal(err)
	}
	dmList, err := s.Handouts(ctx, ScopeDM, cid, HandoutFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(dmList) != 3 {
		t.Fatalf("dm sees %d handouts, want the draft, the map and the charter", len(dmList))
	}
	partyList, err := s.Handouts(ctx, ScopeParty, cid, HandoutFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(partyList) != 2 {
		t.Fatalf("party sees %d handouts, want the map and the charter", len(partyList))
	}
	for _, h := range partyList {
		if h.Status != campaign.HandoutStatusPublished {
			t.Fatalf("party list carried a %s row: %+v", h.Status, h)
		}
	}
	if _, err := s.Handout(ctx, ScopeParty, cid, letters.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a draft by id at party scope must be missing, got %v", err)
	}
	got, err := s.Handout(ctx, ScopeParty, cid, published.ID)
	if err != nil || got.Title != "The charter" {
		t.Fatalf("published by id at party scope: %v %+v", err, got)
	}

	// The status filter is the DM's question alone.
	if _, err := s.Handouts(ctx, ScopeParty, cid, HandoutFilter{Status: campaign.HandoutStatusDraft}); !errors.Is(err, ErrScope) {
		t.Fatalf("a player scope cannot filter by status, got %v", err)
	}
	dmDrafts, err := s.Handouts(ctx, ScopeDM, cid, HandoutFilter{Status: campaign.HandoutStatusDraft})
	if err != nil {
		t.Fatal(err)
	}
	if len(dmDrafts) != 1 || dmDrafts[0].ID != letters.ID {
		t.Fatalf("dm draft filter = %+v", dmDrafts)
	}

	// Unpublish takes it back to draft: the party's read loses it, the
	// stamp clears.
	if _, err := s.SetHandoutStatus(ctx, cid, published.ID, campaign.HandoutStatusDraft); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Handout(ctx, ScopeParty, cid, published.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unpublished by id at party scope must be missing, got %v", err)
	}

	// Retire closes it for the party and keeps it for the DM's history.
	retired, err := s.SetHandoutStatus(ctx, cid, valley.ID, campaign.HandoutStatusRetired)
	if err != nil {
		t.Fatal(err)
	}
	if retired.Status != campaign.HandoutStatusRetired || !retired.PublishedAt.IsZero() {
		t.Fatalf("retired map = %+v", retired)
	}
	if _, err := s.Handout(ctx, ScopeParty, cid, valley.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("retired by id at party scope must be missing, got %v", err)
	}
	dmAll, err := s.Handouts(ctx, ScopeDM, cid, HandoutFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(dmAll) != 3 {
		t.Fatalf("the dm keeps the history: %d rows, want 3", len(dmAll))
	}

	// Character scopes read the published set too; npc scopes resolve
	// against the campaign and read the same party material.
	charList, err := s.Handouts(ctx, ScopeCharacter(fx.Thalia), cid, HandoutFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(charList) != 0 {
		t.Fatalf("everything is draft or retired now; thalia sees %d rows", len(charList))
	}

	// Edits validate shape: a blank title is refused, a bad kind is
	// refused, and a kind flip that would invalidate a published shape is
	// caught at the edit.
	if _, err := s.UpdateHandout(ctx, cid, letters.ID, HandoutUpdate{Title: strp(" ")}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("blank title: %v", err)
	}
	if _, err := s.UpdateHandout(ctx, cid, letters.ID, HandoutUpdate{Kind: strp("prophecy")}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad kind: %v", err)
	}
	letters, err = s.UpdateHandout(ctx, cid, letters.ID, HandoutUpdate{Title: strp("The steward's letter, second draft")})
	if err != nil || letters.Title != "The steward's letter, second draft" {
		t.Fatalf("edit: %v %+v", err, letters)
	}

	// A foreign campaign's handout is a plain 404.
	if _, err := s.Handout(ctx, ScopeDM, "no-such-campaign", letters.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign campaign read: %v", err)
	}
	if err := s.DeleteHandout(ctx, cid, letters.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := s.DeleteHandout(ctx, cid, letters.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double delete must be not-found, got %v", err)
	}
}

func strp(s string) *string { return &s }
