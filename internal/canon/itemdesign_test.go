package canon

// The item designer's placement tests (MAD-383): staging a designed item
// behind the review gate, and the decision that is the only thing that
// writes to the graph.

import (
	"context"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
)

// The placement stages one item entity behind the review gate and writes
// nothing until the batch is decided.
func TestPlaceItem_StagesAndWritesNothing(t *testing.T) {
	s, campaignID, _ := batchStore(t)
	ctx := context.Background()

	countEntities := func() int {
		var n int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM entities WHERE campaign_id = ?`, campaignID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := countEntities()

	batch, err := s.PlaceItem(ctx, ItemPlaceInput{
		CampaignID: campaignID, HomebrewID: "hb-item-1", Name: "Emberbrand",
		Summary: "Forged for the siege of Blackwater.", Rarity: "Rare",
		Notes: "Forged for the siege of Blackwater. It remembers the forge.", CreatedBy: "dm",
	})
	if err != nil {
		t.Fatalf("PlaceItem: %v", err)
	}
	if batch.Status != BatchOpen || batch.ItemCount != 1 {
		t.Fatalf("batch = %s with %d items, want open with the one entity", batch.Status, batch.ItemCount)
	}
	if batch.Source != BatchSourceItem {
		t.Errorf("source = %q, want %q", batch.Source, BatchSourceItem)
	}
	if countEntities() != before {
		t.Fatal("staging a placement wrote to the graph")
	}

	// Deciding the batch lands the item entity.
	if _, err := s.DecideBatch(ctx, campaignID, batch.ID, DecisionAccept, nil, "dm"); err != nil {
		t.Fatalf("accept: %v", err)
	}
	after := countEntities()
	if after != before+1 {
		t.Fatalf("entities = %d, want %d", after, before+1)
	}
	var name, kind string
	if err := s.db.QueryRowContext(ctx,
		`SELECT name, kind FROM entities WHERE campaign_id = ? AND name = ?`, campaignID, "Emberbrand").Scan(&name, &kind); err != nil {
		t.Fatal(err)
	}
	if kind != campaign.KindItem {
		t.Fatalf("placed kind = %s, want an item", kind)
	}
}

// A placement needs a name; an offline store refuses everything.
func TestPlaceItem_ValidatesTheAsk(t *testing.T) {
	s, campaignID, _ := batchStore(t)
	if _, err := s.PlaceItem(context.Background(), ItemPlaceInput{
		CampaignID: campaignID,
	}); err == nil {
		t.Fatal("a placement needs the item's name")
	}
	offline, err := NewOffline(s.db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := offline.PlaceItem(context.Background(), ItemPlaceInput{
		CampaignID: campaignID, Name: "Emberbrand",
	}); err == nil {
		t.Fatal("an offline store cannot stage a placement")
	}
}
