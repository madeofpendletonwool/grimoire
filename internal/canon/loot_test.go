package canon

// The loot generator's placement tests (MAD-384): the generated items and
// the hand-out stage as one batch behind the review gate, the hand-out
// depends on the items it carries, and nothing touches the graph until
// the DM decides the batch.

import (
	"context"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
)

// A hoard placement stages every generated item plus the hand-out event
// that depends on them, and writes nothing until the batch is accepted.
func TestPlaceHoard_StagesAndWritesNothing(t *testing.T) {
	s, campaignID, fx := batchStore(t)
	ctx := context.Background()

	count := func(table string) int {
		var n int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM `+table+` WHERE campaign_id = ?`, campaignID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	beforeEntities, beforeEvents := count("entities"), count("events")

	batch, err := s.PlaceHoard(ctx, HoardPlaceInput{
		CampaignID: campaignID,
		Summary:    "The party pries open the crypt hoard.",
		Items: []HoardPlaceItem{
			{Key: "item-1", Slug: "flame-tongue", Name: "Flame Tongue", Rarity: "Rare",
				Reason: "Magic Item Table C rides Uncommon-to-Rare; Flame Tongue is Rare."},
			{Key: "item-2", Slug: "hauberk", Name: "Hauberk of Warding", Rarity: "Uncommon",
				Reason: "Magic Item Table B rides Uncommon; sits with Bran, who holds nothing."},
		},
		ParticipantIDs: []string{fx.Thalia, fx.Bran, fx.Keth, fx.Mira},
		CreatedBy:      "dm",
	})
	if err != nil {
		t.Fatalf("PlaceHoard: %v", err)
	}
	if batch.Status != BatchOpen || batch.ItemCount != 3 {
		t.Fatalf("batch = %s with %d items, want open with two entities and the hand-out",
			batch.Status, batch.ItemCount)
	}
	if batch.Source != BatchSourceLoot {
		t.Errorf("source = %q, want %q", batch.Source, BatchSourceLoot)
	}
	if count("entities") != beforeEntities || count("events") != beforeEvents {
		t.Fatal("staging a hoard wrote to the graph")
	}

	// Accepting lands two item entities and one event, in dependency
	// order, with the pcs on the hand-out.
	if _, err := s.DecideBatch(ctx, campaignID, batch.ID, DecisionAccept, nil, "dm"); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if got := count("entities"); got != beforeEntities+2 {
		t.Fatalf("entities = %d, want %d", got, beforeEntities+2)
	}
	if got := count("events"); got != beforeEvents+1 {
		t.Fatalf("events = %d, want %d", got, beforeEvents+1)
	}
	var kind string
	if err := s.db.QueryRowContext(ctx,
		`SELECT kind FROM entities WHERE campaign_id = ? AND name = ?`, campaignID, "Flame Tongue").Scan(&kind); err != nil {
		t.Fatal(err)
	}
	if kind != campaign.KindItem {
		t.Errorf("placed kind = %s, want an item", kind)
	}
	var participants int
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM event_participants p
		 JOIN events e ON e.id = p.event_id
		 WHERE e.campaign_id = ? AND e.summary = ?`, campaignID,
		"The party pries open the crypt hoard.").Scan(&participants); err != nil {
		t.Fatal(err)
	}
	if participants != 4 {
		t.Errorf("the hand-out has %d participants, want the party of 4", participants)
	}
}

// The act's own item is already an entity: it rides the summary and is
// never staged twice. An unexplained line is not stageable at all.
func TestPlaceHoard_ValidatesTheAsk(t *testing.T) {
	s, campaignID, fx := batchStore(t)
	ctx := context.Background()

	// Two lines where one is the act's own item — only one entity stages,
	// and the batch still carries the hand-out.
	batch, err := s.PlaceHoard(ctx, HoardPlaceInput{
		CampaignID: campaignID,
		Summary:    "The act closes; the hoard carries the Silver Key out of the crypt.",
		Items: []HoardPlaceItem{
			{Key: "item-1", Name: "A Rolled Item", Rarity: "Rare", Reason: "rolled"},
			{Key: "narrative", EntityID: fx.Key, Name: "The Silver Key", Reason: "carries the act's item"},
		},
		CreatedBy: "dm",
	})
	if err != nil {
		t.Fatalf("PlaceHoard: %v", err)
	}
	if batch.ItemCount != 2 {
		t.Fatalf("batch has %d items, want the one entity and the hand-out", batch.ItemCount)
	}

	if _, err := s.PlaceHoard(ctx, HoardPlaceInput{
		CampaignID: campaignID, Summary: "no reasons anywhere",
		Items: []HoardPlaceItem{{Key: "item-1", Name: "Unexplained"}},
	}); err == nil {
		t.Fatal("an item with no reason staged anyway")
	}
	if _, err := s.PlaceHoard(ctx, HoardPlaceInput{
		CampaignID: campaignID,
		Items:      []HoardPlaceItem{{Key: "item-1", Name: "X", Reason: "r"}},
	}); err == nil {
		t.Fatal("a placement with no summary staged anyway")
	}
	offline, err := NewOffline(s.db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := offline.PlaceHoard(ctx, HoardPlaceInput{
		CampaignID: campaignID, Summary: "s",
		Items: []HoardPlaceItem{{Key: "item-1", Name: "X", Reason: "r"}},
	}); err == nil {
		t.Fatal("an offline store cannot stage a hoard")
	}
}
