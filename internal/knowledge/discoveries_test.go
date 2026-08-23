package knowledge

import (
	"context"
	"errors"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
)

func TestRecordDiscoveryGrantsAwareness(t *testing.T) {
	s, fx, kx := seeded(t)
	ctx := context.Background()
	cid := fx.Campaign.ID

	d, err := s.GetDiscovery(ctx, ScopeDM, cid, kx.DiscoveryCharterID)
	if err != nil {
		t.Fatalf("get discovery: %v", err)
	}
	if d.FactID != fx.FactMinesOwned || d.DiscoveredBy != campaign.PartyKnower {
		t.Fatalf("charter discovery is wrong: %+v", d)
	}
	if d.Method != "Mira read the holding charter at the assay office" {
		t.Fatalf("method prose must round-trip: %q", d.Method)
	}
	if d.SpanStart != 14 || d.SpanEnd != 61 {
		t.Fatalf("span must round-trip: %d..%d", d.SpanStart, d.SpanEnd)
	}
	if d.AcceptedBy != "keeper" || d.AcceptedAt.IsZero() {
		t.Fatalf("recording a discovery accepts it: %+v", d)
	}

	// The awareness row the discovery granted.
	rows, err := s.Awareness(ctx, ScopeDM, cid, campaign.PartyKnower, fx.FactMinesOwned)
	if err != nil || len(rows) != 1 {
		t.Fatalf("charter discovery must grant party awareness: %v %v", rows, err)
	}
	if rows[0].Stance != StanceKnows || rows[0].DiscoveryID != d.ID {
		t.Fatalf("grant points back at its discovery: %+v", rows[0])
	}

	// Mira's own discovery upgraded nothing she did not already hold: the
	// deed discovery left her at knows via her own row.
	mira, err := s.Awareness(ctx, ScopeCharacter(fx.Mira), cid, fx.Mira, fx.FactMinesOwned)
	if err != nil || len(mira) != 1 || mira[0].Stance != StanceKnows {
		t.Fatalf("mira knows via her own discovery: %v %v", mira, err)
	}
	if mira[0].DiscoveryID != kx.DiscoveryDeedID {
		t.Fatalf("mira's row should point at the deed, got %q", mira[0].DiscoveryID)
	}
	if mira[0].SinceEvent != fx.EventAmbush {
		t.Fatalf("since event must round-trip: %q", mira[0].SinceEvent)
	}
}

func TestRecordDiscoveryValidation(t *testing.T) {
	s, fx, _ := seeded(t)
	ctx := context.Background()
	cid := fx.Campaign.ID

	// A discovery always grants.
	if _, err := s.RecordDiscovery(ctx, RecordDiscoveryInput{
		CampaignID: cid, FactID: fx.FactMinesOwned, DiscoveredBy: campaign.PartyKnower,
		Stance: StanceUnaware, Confidence: 1,
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unaware discovery must be invalid: %v", err)
	}
	// Bad span.
	if _, err := s.RecordDiscovery(ctx, RecordDiscoveryInput{
		CampaignID: cid, FactID: fx.FactMinesOwned, DiscoveredBy: campaign.PartyKnower,
		Stance: StanceKnows, Confidence: 1, SpanStart: 90, SpanEnd: 10,
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("inverted span must be invalid: %v", err)
	}
	// Bad confidence.
	if _, err := s.RecordDiscovery(ctx, RecordDiscoveryInput{
		CampaignID: cid, FactID: fx.FactMinesOwned, DiscoveredBy: campaign.PartyKnower,
		Stance: StanceKnows, Confidence: 2,
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("confidence 2 must be invalid: %v", err)
	}
	// Unaccepted discoveries leave the acceptance trail empty.
	d, err := s.RecordDiscovery(ctx, RecordDiscoveryInput{
		CampaignID: cid, FactID: fx.FactDukeVisited, DiscoveredBy: fx.Keth,
		Stance: StanceSuspects, Confidence: 0.4, Method: "overheard the steward",
	})
	if err != nil {
		t.Fatalf("record unaccepted: %v", err)
	}
	if d.AcceptedBy != "" || !d.AcceptedAt.IsZero() {
		t.Fatalf("unaccepted discovery must carry no acceptance: %+v", d)
	}
}

func TestDiscoveryVisibilityIsScoped(t *testing.T) {
	s, fx, kx := seeded(t)
	ctx := context.Background()
	cid := fx.Campaign.ID

	// Mira sees both the party's charter discovery and her own deed.
	mira, err := s.Discoveries(ctx, ScopeCharacter(fx.Mira), cid, "")
	if err != nil {
		t.Fatalf("mira discoveries: %v", err)
	}
	if len(mira) != 2 {
		t.Fatalf("mira sees the party grant and her own: %d", len(mira))
	}
	// Thalia sees only the party's — Mira's reading is Mira's trail.
	thalia, err := s.Discoveries(ctx, ScopeCharacter(fx.Thalia), cid, "")
	if err != nil {
		t.Fatalf("thalia discoveries: %v", err)
	}
	if len(thalia) != 1 || thalia[0].ID != kx.DiscoveryCharterID {
		t.Fatalf("thalia sees only the shared discovery: %+v", thalia)
	}
	// The DM sees both (the charter and Mira's deed).
	dm, err := s.Discoveries(ctx, ScopeDM, cid, "")
	if err != nil || len(dm) != 2 {
		t.Fatalf("dm sees every discovery: %d %v", len(dm), err)
	}
	// One discovery by id, scoped.
	if _, err := s.GetDiscovery(ctx, ScopeCharacter(fx.Thalia), cid, kx.DiscoveryDeedID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("thalia must not read mira's discovery by id: %v", err)
	}
}
