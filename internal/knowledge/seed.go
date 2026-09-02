package knowledge

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
)

// KnowledgeFixture extends campaign.Fixture with the knowledge-layer rows the
// shared test substrate plants. Built on top of the campaign fixture, never
// inside it: the campaign seed stays epistemically neutral (no awareness, no
// discoveries) so Stage-3 tests can construct their own knowledge states.
//
// Deliberately, no non-DM knower is granted a secret-visibility fact here.
// That is what keeps the reflection leak test honest: it proves ungranted
// secrets never surface at any scope, while a separate unit test
// (playerview_test.go) proves granted secrets flow to their knower — and
// only their knower — on the wide store.
type KnowledgeFixture struct {
	// The discovery that put the mines in the party's hands.
	DiscoveryCharterID string
	// A second discovery, Mira's alone, exercising the per-character trail.
	DiscoveryDeedID string
	// A proposed, secret fact the extraction pipeline might produce: never
	// retrievable by any scope until a human accepts it.
	FactDukeVampireID string
}

// Seed plants the knowledge layer over the campaign fixture: what the party
// knows, what Thalia suspects, what the party is confidently wrong about,
// one deliberate unaware row, one NPC's knowledge, and one staged proposal.
//
// The resulting state, in one breath: the party knows the Duke owns the
// Eastern Mines because Mira read the holding charter; Mira additionally
// read the deed herself; Thalia suspects the Duke's ledger visit is real;
// the party is confidently wrong about the Duke never traveling — the
// account is true, their disbelief in it is the error; the party
// walked past the Silver Key's crypt without asking what it opens; Elara
// knows the mine ownership like any chancellor would; and something in the
// pipeline has proposed that the Duke is a vampire, which nobody may read.
func Seed(ctx context.Context, db *sql.DB, fx *campaign.Fixture, owner string) (*KnowledgeFixture, error) {
	s, err := New(db)
	if err != nil {
		return nil, err
	}
	kx := &KnowledgeFixture{}
	cid := fx.Campaign.ID

	charter, err := s.RecordDiscovery(ctx, RecordDiscoveryInput{
		CampaignID: cid, FactID: fx.FactMinesOwned, DiscoveredBy: campaign.PartyKnower,
		SessionID: "seed", Method: "Mira read the holding charter at the assay office",
		SourceID: "seed-charter", SpanStart: 14, SpanEnd: 61,
		Quote:      "the charter names the holder of the Eastern Mines",
		Confidence: 0.95, AcceptedBy: owner, Stance: StanceKnows,
	})
	if err != nil {
		return nil, fmt.Errorf("seed charter discovery: %w", err)
	}
	kx.DiscoveryCharterID = charter.ID

	deed, err := s.RecordDiscovery(ctx, RecordDiscoveryInput{
		CampaignID: cid, FactID: fx.FactMinesOwned, DiscoveredBy: fx.Mira,
		Method: "read the deed of the Eastern Mines", Confidence: 0.9,
		AcceptedBy: owner, Stance: StanceKnows, SinceEvent: fx.EventAmbush,
	})
	if err != nil {
		return nil, fmt.Errorf("seed deed discovery: %w", err)
	}
	kx.DiscoveryDeedID = deed.ID

	// Thalia, reading the same ledger the steward keeps, suspects the visit
	// happened. The party as a whole has not taken a position on it.
	if _, err := s.SetAwareness(ctx, cid, fx.Thalia, fx.FactDukeVisited,
		StanceSuspects, 0.6, fx.EventAmbush, ""); err != nil {
		return nil, fmt.Errorf("seed thalia suspicion: %w", err)
	}

	// The housekeeper says the Duke never travels — and the housekeeper is
	// right. A believes_false row means the knower is wrong about this fact
	// (the Summarize reading, settled in MAD-374): the fact is true, and
	// what is false is the party's disbelief in it. Having seen the charter
	// trail, the party confidently believes the account is false. The wrong
	// content lives in the belief, not in the fact.
	if _, err := s.SetAwareness(ctx, cid, campaign.PartyKnower, fx.FactDukeNever,
		StanceBelievesFalse, 0.8, "", ""); err != nil {
		return nil, fmt.Errorf("seed party wrong belief: %w", err)
	}

	// The deliberate blank: they walked past the crypt door and did not ask.
	if _, err := s.SetAwareness(ctx, cid, campaign.PartyKnower, fx.FactKeyOpensCrypt,
		StanceUnaware, 1, fx.EventAmbush, ""); err != nil {
		return nil, fmt.Errorf("seed party unaware: %w", err)
	}

	// Elara, chancellor: of course she knows who holds the mines.
	if _, err := s.SetAwareness(ctx, cid, fx.Elara, fx.FactMinesOwned,
		StanceKnows, 1, "", ""); err != nil {
		return nil, fmt.Errorf("seed elara knowledge: %w", err)
	}

	// The staged proposal: an extraction pass, reading session audio,
	// suggests the obvious. It is secret AND proposed — doubly invisible.
	cs, err := campaign.New(db)
	if err != nil {
		return nil, err
	}
	vampire, err := cs.CreateFact(ctx, cid, fx.Duke, "is", "", "a vampire",
		"The Duke is a vampire.", campaign.ConfidenceProposed, campaign.VisibilitySecret, owner,
		[]campaign.ProvenanceInput{{
			SessionID: "seed", SourceID: "seed-transcript", SpanStart: 88, SpanEnd: 130,
			Quote:  "he did not eat, and the mirrors in the keep are all black lead",
			Method: campaign.MethodExtracted,
		}})
	if err != nil {
		return nil, fmt.Errorf("seed proposed fact: %w", err)
	}
	kx.FactDukeVampireID = vampire.ID

	return kx, nil
}
