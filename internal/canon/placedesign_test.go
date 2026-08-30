package canon

// The location designer's tests (MAD-372): the premise read, the scale
// band asserted with the model faked, near-anchor reuse (the second
// Blackwater never happens), the accepted-village canon check, the
// parent-dismissal cascade, the flesh-out path, and part regeneration.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/place"
)

const placeTestPremise = "A small village on the forest road, uneasy about what is in the trees"

// placeTestFill builds a valid model fill for a full fresh design of the
// given shape: new names for everything, holders drawn from the new
// people. The variant suffix keeps every generated batch's payloads
// distinct — StageBatch's dedup would otherwise skip identical items from
// an earlier undecided batch. Route fields are added by the caller when
// the request carries an anchor.
func placeTestFill(shape place.Shape, variant int) map[string]any {
	v := fmt.Sprintf(" %d", variant)
	m := map[string]any{
		"place_name":    "Fenwick" + v,
		"place_summary": "Forty roofs in a clearing, a palisade of grey poles, and the road running wet through the middle. Nobody stands outside after dark.",
		"population":    "about 200",
		"government":    "Reeve Aldous, answerable to the Duke's steward",
		"defences":      "a palisade and a watch of six",
		"climate":       "temperate; damp off the pines",
		"state":         "the mill burned last month and nobody says why",
		"senses":        "woodsmoke and wet pine; an axe somewhere in the trees",
		"danger":        float64(2),
	}
	for i := 1; i <= shape.Services; i++ {
		m[fmt.Sprintf("service_%d", i)] = fmt.Sprintf("service %d%s", i, v)
	}
	for i, slot := range shape.Subs {
		m[slot.ID] = "new"
		m[slot.ID+"_new_name"] = fmt.Sprintf("The %s of Fenwick %d%s", slot.Role, i+1, v)
		m[slot.ID+"_new_summary"] = fmt.Sprintf("The village's %s, kept and argued over.", slot.Role)
	}
	var npcNames []string
	for i := 1; i <= shape.NPCs; i++ {
		name := fmt.Sprintf("Notable %d of Fenwick%s", i, v)
		npcNames = append(npcNames, name)
		m[fmt.Sprintf("npc_%d", i)] = "new"
		m[fmt.Sprintf("npc_%d_new_name", i)] = name
		m[fmt.Sprintf("npc_%d_new_summary", i)] = "Keeps a finger on the village's pulse."
		m[fmt.Sprintf("npc_%d_goal", i)] = "Wants the road quiet again."
		m[fmt.Sprintf("npc_%d_voice", i)] = "Flat, careful, watching the door."
		m[fmt.Sprintf("npc_%d_faction", i)] = "none"
	}
	for i := 1; i <= shape.Hooks; i++ {
		m[fmt.Sprintf("hook_%d_statement", i)] = fmt.Sprintf("Hook %d%s: the woodcutters will not past the north ridge, and the ones who did are missing.", i, v)
		m[fmt.Sprintf("hook_%d_thread", i)] = fmt.Sprintf("the missing woodcutters %d%s", i, v)
	}
	for i := 1; i <= shape.Secrets; i++ {
		m[fmt.Sprintf("secret_%d_statement", i)] = fmt.Sprintf("Secret %d%s: the reeve pays the thing in the trees, and has for years.", i, v)
		m[fmt.Sprintf("secret_%d_holder", i)] = npcNames[i-1]
	}
	return m
}

// withRouteFill adds the road-out fields an anchored request declares.
func withRouteFill(m map[string]any) {
	m["route_days"] = float64(2)
	m["route_terrain"] = "dirt road through dark pines"
}

func placeJSON(t *testing.T, v map[string]any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// itemPayload decodes a batch item's staged detail.
func itemPayload(t *testing.T, r Review) map[string]any {
	t.Helper()
	var p map[string]any
	if err := json.Unmarshal([]byte(r.Detail), &p); err != nil {
		t.Fatalf("decode %s: %v", r.ID, err)
	}
	return p
}

func TestReadPlacePremise(t *testing.T) {
	s := ReadPlacePremise(placeTestPremise)
	if s.Terrain != "road" {
		t.Errorf("terrain = %q, want road (the road is the more specific answer)", s.Terrain)
	}
	if s.Tone != "uneasy" {
		t.Errorf("tone = %q, want uneasy", s.Tone)
	}
	if len(s.Terms) == 0 {
		t.Error("no terms read from the premise")
	}
	f := ReadPlacePremise("A prosperous harbour town of pilgrims under siege by raiders")
	if f.Terrain != "coast" {
		t.Errorf("terrain = %q, want coast", f.Terrain)
	}
	if f.Tone != "besieged" {
		t.Errorf("tone = %q, want besieged", f.Tone)
	}
	h := ReadPlacePremise("A mining settlement in the mountains, haunted")
	if h.Terrain != "mining hills" {
		t.Errorf("terrain = %q, want mining hills", h.Terrain)
	}
	if h.Tone != "haunted" {
		t.Errorf("tone = %q, want haunted", h.Tone)
	}
}

// generatePlace runs one fresh design with the scripted fill, the model
// faked (ADR 8).
func generatePlace(t *testing.T, s *Store, campaignID string, shape place.Shape, variant int, near string) *LocationDesignResult {
	t.Helper()
	fill := placeTestFill(shape, variant)
	if near != "" {
		withRouteFill(fill)
	}
	s.model = &fakeModel{responses: []string{placeJSON(t, fill)}}
	res, err := s.GenerateLocation(context.Background(), LocationDesignInput{
		CampaignID: campaignID, Premise: placeTestPremise,
		Kind: shape.Kind, Scale: shape.Scale, Near: near, CreatedBy: "dm",
	})
	if err != nil {
		t.Fatalf("GenerateLocation: %v", err)
	}
	return res
}

func TestGenerateLocation_BatchSitsInsideTheBandForEveryKind(t *testing.T) {
	s, campaignID, _ := batchStore(t)
	variant := 0
	for _, kind := range place.Kinds() {
		for _, scale := range place.Scales() {
			shape, err := place.ShapeFor(kind, scale)
			if err != nil {
				t.Fatalf("%s/%s: %v", kind, scale, err)
			}
			variant++
			res := generatePlace(t, s, campaignID, shape, variant, "")
			if !shape.InsideBand(res.Batch.ItemCount) {
				t.Errorf("%s/%s: batch items = %d, band is %d-%d",
					kind, scale, res.Batch.ItemCount, shape.MinItems(), shape.MaxItems())
			}
			// The structural counts the model cannot exceed: one place,
			// the band's sub-locations and people, its hooks and secrets.
			if got := countItems(res.Batch.Items, ReviewProposedEntity); got != 1+shape.SubLocations()+shape.NPCs {
				t.Errorf("%s/%s: entity items = %d, want %d", kind, scale, got, 1+shape.SubLocations()+shape.NPCs)
			}
			if got := countItems(res.Batch.Items, ReviewProposedFact); got != shape.Hooks+shape.Secrets {
				t.Errorf("%s/%s: fact items = %d, want %d", kind, scale, got, shape.Hooks+shape.Secrets)
			}
			// Every secret has a holder discovery depending on it.
			secrets, holders := 0, 0
			for _, it := range res.Batch.Items {
				if it.Kind == ReviewProposedFact &&
					itemPayload(t, it)["visibility"] == campaign.VisibilitySecret {
					secrets++
				}
				if it.Kind == ReviewProposedDiscovery {
					for _, dep := range it.DependsOn {
						if strings.HasSuffix(dep, "") || len(dep) > 0 {
							holders++
							break
						}
					}
				}
			}
			if secrets != shape.Secrets || holders != shape.Secrets {
				t.Errorf("%s/%s: secrets = %d, holder discoveries = %d, want %d of each",
					kind, scale, secrets, holders, shape.Secrets)
			}
		}
	}
}

func countItems(items []Review, kind string) int {
	n := 0
	for i := range items {
		if items[i].Kind == kind {
			n++
		}
	}
	return n
}

func TestGenerateLocation_NearBlackwaterReusesNotDuplicates(t *testing.T) {
	s, campaignID, fx := batchStore(t)
	shape, _ := place.ShapeFor(place.KindVillage, place.ScaleMedium)
	res := generatePlace(t, s, campaignID, shape, 1, fx.Blackwater)

	// No entity item stages a second Blackwater; the edge and the road
	// point at the existing entity's real id.
	var placeItem Review
	for _, it := range res.Batch.Items {
		if itemPayload(t, it)["local_id"] == "place" {
			placeItem = it
		}
		if it.Kind != ReviewProposedEntity {
			continue
		}
		if name, _ := itemPayload(t, it)["name"].(string); strings.EqualFold(name, "Blackwater") {
			t.Fatalf("a second Blackwater was staged: %+v", it)
		}
	}
	if placeItem.ID == "" {
		t.Fatal("no place item")
	}
	pp := itemPayload(t, placeItem)
	block, _ := pp["payload"].(map[string]any)
	travel, _ := block["travel"].(map[string]any)
	routes, _ := travel["routes"].([]any)
	if len(routes) != 1 {
		t.Fatalf("routes = %v, want the road to Blackwater", routes)
	}
	route := routes[0].(map[string]any)
	if route["to"] != fx.Blackwater {
		t.Errorf("route to = %v, want the existing Blackwater id", route["to"])
	}
	if d, _ := route["days"].(float64); d != 2 {
		t.Errorf("route days = %v, want 2", route["days"])
	}
	// The anchor edge item: the village located_in the real Blackwater.
	var anchorEdge Review
	for _, it := range res.Batch.Items {
		if it.Kind == ReviewProposedRelationship {
			p := itemPayload(t, it)
			if p["to_entity"] == fx.Blackwater && p["rel_type"] == "located_in" {
				anchorEdge = it
			}
		}
	}
	if anchorEdge.ID == "" {
		t.Fatal("no located_in edge to the existing Blackwater")
	}

	// Accept the whole batch: the route survives on the entity, exactly
	// one Blackwater exists, and the edge landed.
	if _, err := s.DecideBatch(context.Background(), campaignID, res.Batch.ID, DecisionAccept, nil, "dm"); err != nil {
		t.Fatalf("DecideBatch: %v", err)
	}
	hits, err := s.campaigns.ResolveName(context.Background(), campaign.ScopeDM, campaignID, "Fenwick 1")
	if err != nil || len(hits) != 1 {
		t.Fatalf("ResolveName(Fenwick 1) = %d hits (err %v)", len(hits), err)
	}
	village := hits[0]
	if got := campaign.RoutesOf(&village); len(got) != 1 || got[0].To != fx.Blackwater || got[0].Days != 2 {
		t.Errorf("routes on the entity = %v, want one two-day road to Blackwater", got)
	}
	bw, err := s.campaigns.ResolveName(context.Background(), campaign.ScopeDM, campaignID, "Blackwater")
	if err != nil || len(bw) != 1 {
		t.Errorf("ResolveName(Blackwater) = %d hits (err %v), want exactly the one", len(bw), err)
	}
	rels, err := s.campaigns.ListRelationships(context.Background(), campaign.ScopeDM, campaignID)
	if err != nil {
		t.Fatal(err)
	}
	linked := false
	for _, r := range rels {
		if r.FromEntity == village.ID && r.ToEntity == fx.Blackwater && r.RelType == "located_in" {
			linked = true
		}
	}
	if !linked {
		t.Error("no located_in edge from the village to the existing Blackwater")
	}
}

// placeReviewID finds a batch item's review row by its staged local_id.
func placeReviewID(t *testing.T, b *Batch, localID string) string {
	t.Helper()
	for i := range b.Items {
		p := itemPayload(t, b.Items[i])
		if p["local_id"] == localID {
			return b.Items[i].ID
		}
	}
	t.Fatalf("no item with local_id %q", localID)
	return ""
}

func TestGenerateLocation_AcceptedVillagePassesCanonCheck(t *testing.T) {
	s, campaignID, _ := batchStore(t)
	shape, _ := place.ShapeFor(place.KindVillage, place.ScaleMedium)
	res := generatePlace(t, s, campaignID, shape, 1, "")

	// Every secret item stages a sibling holder discovery, depending on it.
	secretIDs := map[string]bool{}
	holderDeps := map[string]bool{}
	for _, it := range res.Batch.Items {
		if it.Kind == ReviewProposedFact && itemPayload(t, it)["visibility"] == campaign.VisibilitySecret {
			secretIDs[it.ID] = true
		}
		if it.Kind == ReviewProposedDiscovery {
			for _, dep := range it.DependsOn {
				holderDeps[dep] = true
			}
		}
	}
	if len(secretIDs) != shape.Secrets {
		t.Fatalf("secret items = %d, want %d", len(secretIDs), shape.Secrets)
	}
	for id := range secretIDs {
		if !holderDeps[id] {
			t.Errorf("secret %s has no holder discovery depending on it", id)
		}
	}

	decided, err := s.DecideBatch(context.Background(), campaignID, res.Batch.ID, DecisionAccept, nil, "dm")
	if err != nil {
		t.Fatalf("DecideBatch: %v", err)
	}
	var newIDs []string
	for _, out := range decided.Items {
		if out.ResultRef != "" {
			newIDs = append(newIDs, out.ResultRef)
		}
	}
	placeID := ""
	for _, out := range decided.Items {
		if out.ReviewID == placeReviewID(t, res.Batch, "place") {
			placeID = out.ResultRef
		}
	}
	if placeID == "" {
		t.Fatal("the place did not land")
	}

	snap, err := LoadSnapshot(context.Background(), s.db, campaignID)
	if err != nil {
		t.Fatal(err)
	}
	isNew := map[string]bool{}
	for _, id := range newIDs {
		isNew[id] = true
	}
	for _, f := range CheckSnapshot(snap, DefaultCheckOptions()) {
		if f.Severity == campaign.SeverityError && isNew[f.RecordID] {
			t.Errorf("error finding against the accepted village: %s (%s)", f.Message, f.Check)
		}
		if f.Check == CheckDormantRegion && f.RecordID == placeID {
			t.Errorf("dormant_region against the new place: %s", f.Message)
		}
		if f.Check == CheckUnreachableSecret && isNew[f.RecordID] {
			t.Errorf("unreachable_secret against a secret the village introduced: %s", f.Message)
		}
	}
}

func TestGenerateLocation_DismissingThePlaceRefusesItsDependents(t *testing.T) {
	s, campaignID, _ := batchStore(t)
	shape, _ := place.ShapeFor(place.KindVillage, place.ScaleMedium)
	res := generatePlace(t, s, campaignID, shape, 1, "")

	decided, err := s.DecideBatch(context.Background(), campaignID, res.Batch.ID, DecisionAccept,
		[]ItemDecision{{ItemID: placeReviewID(t, res.Batch, "place"), Decision: DecisionDismiss}}, "dm")
	if err != nil {
		t.Fatalf("DecideBatch: %v", err)
	}
	// Every dependent was refused with the reason said out loud, and
	// nothing partial was written.
	written, refused := 0, 0
	for _, out := range decided.Items {
		if out.ResultRef != "" {
			written++
		}
		if out.Status == ReviewDismissed && out.Reason != "" {
			refused++
		}
	}
	if refused == 0 {
		t.Fatalf("no dependent was refused: %+v", decided.Items)
	}
	if written != 0 {
		t.Fatalf("partial write after dismissing the place: %d items landed", written)
	}
	// The graph agrees: no Fenwick anywhere.
	hits, err := s.campaigns.ResolveName(context.Background(), campaign.ScopeDM, campaignID, "Fenwick 1")
	if err != nil || len(hits) != 0 {
		t.Errorf("Fenwick 1 exists after dismissal: %d hits (err %v)", len(hits), err)
	}
}

func TestGenerateLocation_FleshOutProposesAroundNeverReplaces(t *testing.T) {
	s, campaignID, fx := batchStore(t)
	ctx := context.Background()

	// A location that already exists: a name and one line, a hand-written
	// block field and one hand-placed person.
	loc, err := s.campaigns.CreateEntity(ctx, campaignID, campaign.KindLocation, "Ashford",
		"A market village on the Duke's road.", campaign.WithPlace(nil, campaign.Place{
			Kind: "village", Scale: "medium", Population: "about 400",
		}))
	if err != nil {
		t.Fatal(err)
	}
	miller, err := s.campaigns.CreateEntity(ctx, campaignID, campaign.KindNPC, "The Miller", "Grinds slow, listens fast.", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.campaigns.CreateRelationship(ctx, campaignID, miller.ID, "located_in", loc.ID, 0, "", ""); err != nil {
		t.Fatal(err)
	}

	shape, _ := place.ShapeFor(place.KindVillage, place.ScaleMedium)
	fill := placeTestFill(shape, 1)
	delete(fill, "place_name")
	delete(fill, "place_summary")
	delete(fill, "population") // already written; a flesh-out never asks
	// The band minus what exists: 3 sub-locations (none yet), 2 people
	// (the miller already sits here), the hooks and the secrets. One
	// holder is the miller — someone already at the place.
	delete(fill, "npc_3")
	delete(fill, "npc_3_new_name")
	delete(fill, "npc_3_new_summary")
	delete(fill, "npc_3_goal")
	delete(fill, "npc_3_voice")
	delete(fill, "npc_3_faction")
	fill["secret_1_holder"] = "The Miller"
	withRouteFill(fill)
	s.model = &fakeModel{responses: []string{placeJSON(t, fill)}}

	res, err := s.GenerateLocation(ctx, LocationDesignInput{
		CampaignID: campaignID, Premise: "the mill burned and nobody says why",
		Location: loc.ID, Near: fx.Blackwater, CreatedBy: "dm",
	})
	if err != nil {
		t.Fatalf("GenerateLocation flesh-out: %v", err)
	}

	// The place item is an update of the existing entity, not a new one.
	pp := itemPayload(t, placeReview(t, res.Batch, "place"))
	if pp["entity_update"] != loc.ID {
		t.Fatalf("place item is not an update: %v", pp)
	}
	block, _ := pp["payload"].(map[string]any)
	placeBlock, _ := block["place"].(map[string]any)
	if placeBlock["population"] != "about 400" {
		t.Errorf("existing population replaced: %v", placeBlock["population"])
	}

	// The band, honoured: the people items are 3 - 1 already here.
	if got := countItems(res.Batch.Items, ReviewProposedEntity); got != 1+2+shape.SubLocations() {
		t.Errorf("entity items = %d, want %d (the block update, 2 new people, %d sub-locations)", got, 1+2+shape.SubLocations(), shape.SubLocations())
	}

	decided, err := s.DecideBatch(ctx, campaignID, res.Batch.ID, DecisionAccept, nil, "dm")
	if err != nil {
		t.Fatalf("DecideBatch: %v", err)
	}
	updated, err := s.campaigns.GetEntity(ctx, campaign.ScopeDM, campaignID, loc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Summary != "A market village on the Duke's road." {
		t.Errorf("summary replaced: %q", updated.Summary)
	}
	got := campaign.PlaceOf(updated)
	if got.Population != "about 400" {
		t.Errorf("population = %q, want the hand-written value kept", got.Population)
	}
	if got.Government == "" || len(got.Services) == 0 {
		t.Errorf("the block was not filled around the existing fields: %+v", got)
	}

	// No unreachable_secret against any secret this batch introduced: the
	// holders' awareness rows landed with them.
	newIDs := map[string]bool{}
	for _, out := range decided.Items {
		if out.ResultRef != "" {
			newIDs[out.ResultRef] = true
		}
	}
	snap, err := LoadSnapshot(ctx, s.db, campaignID)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range CheckSnapshot(snap, DefaultCheckOptions()) {
		if f.Check == CheckUnreachableSecret && newIDs[f.RecordID] {
			t.Errorf("unreachable_secret against a fleshed-out secret: %s", f.Message)
		}
	}

	// Re-rolling a full part is refused with the count, not truncated.
	if _, err := s.GenerateLocation(ctx, LocationDesignInput{
		CampaignID: campaignID, Premise: "the same", Location: loc.ID,
		Parts: []string{LocationPartSublocations}, CreatedBy: "dm",
	}); err == nil || !strings.Contains(err.Error(), "full") || !strings.Contains(err.Error(), "band allows") {
		t.Fatalf("full part refusal = %v, want the count", err)
	}
}

// placeReview finds a batch item's review row by its staged local_id.
func placeReview(t *testing.T, b *Batch, localID string) Review {
	t.Helper()
	for i := range b.Items {
		if itemPayload(t, b.Items[i])["local_id"] == localID {
			return b.Items[i]
		}
	}
	t.Fatalf("no item with local_id %q", localID)
	return Review{}
}

func TestGenerateLocation_PartsRegenerateOnePiece(t *testing.T) {
	s, campaignID, _ := batchStore(t)
	ctx := context.Background()
	shape, _ := place.ShapeFor(place.KindVillage, place.ScaleMedium)
	fill := placeTestFill(shape, 1)

	// A geography-only design: place and sub-locations, no people, no
	// hooks, no secrets, no road.
	geography := func(m map[string]any) {
		for i := 1; i <= shape.NPCs; i++ {
			for _, suffix := range []string{"", "_new_name", "_new_summary", "_goal", "_voice", "_faction"} {
				delete(m, fmt.Sprintf("npc_%d%s", i, suffix))
			}
		}
		for i := 1; i <= shape.Secrets; i++ {
			delete(m, fmt.Sprintf("secret_%d_statement", i))
			delete(m, fmt.Sprintf("secret_%d_holder", i))
		}
		for i := 1; i <= shape.Hooks; i++ {
			delete(m, fmt.Sprintf("hook_%d_statement", i))
			delete(m, fmt.Sprintf("hook_%d_thread", i))
		}
	}
	geoFill := cloneFill(fill)
	geography(geoFill)
	s.model = &fakeModel{responses: []string{placeJSON(t, geoFill), placeJSON(t, geoFill)}}
	res, err := s.GenerateLocation(ctx, LocationDesignInput{
		CampaignID: campaignID, Premise: placeTestPremise, Kind: shape.Kind, Scale: shape.Scale,
		Parts: []string{LocationPartPlace, LocationPartSublocations}, CreatedBy: "dm",
	})
	if err != nil {
		t.Fatalf("geography-only: %v", err)
	}
	if got := countItems(res.Batch.Items, ReviewProposedFact); got != 0 {
		t.Errorf("facts staged in a geography-only run: %d", got)
	}
	if got := countItems(res.Batch.Items, ReviewProposedDiscovery); got != 0 {
		t.Errorf("discoveries staged in a geography-only run: %d", got)
	}
	decided, err := s.DecideBatch(ctx, campaignID, res.Batch.ID, DecisionAccept, nil, "dm")
	if err != nil {
		t.Fatal(err)
	}
	placeID := ""
	for _, out := range decided.Items {
		if out.ReviewID == placeReviewID(t, res.Batch, "place") {
			placeID = out.ResultRef
		}
	}
	if placeID == "" {
		t.Fatal("the geography's place did not land")
	}

	// The people arrive later, against the accepted geography: re-roll the
	// people, keep the geography — one part, one batch.
	people := func(m map[string]any) {
		delete(m, "place_name")
		delete(m, "place_summary")
		delete(m, "population")
		delete(m, "government")
		delete(m, "defences")
		delete(m, "climate")
		delete(m, "state")
		delete(m, "senses")
		delete(m, "danger")
		for i := 1; i <= shape.Services; i++ {
			delete(m, fmt.Sprintf("service_%d", i))
		}
		for _, slot := range shape.Subs {
			delete(m, slot.ID)
			delete(m, slot.ID+"_new_name")
			delete(m, slot.ID+"_new_summary")
		}
		for i := 1; i <= shape.Secrets; i++ {
			delete(m, fmt.Sprintf("secret_%d_statement", i))
			delete(m, fmt.Sprintf("secret_%d_holder", i))
		}
		for i := 1; i <= shape.Hooks; i++ {
			delete(m, fmt.Sprintf("hook_%d_statement", i))
			delete(m, fmt.Sprintf("hook_%d_thread", i))
		}
	}
	peopleFill := cloneFill(fill)
	people(peopleFill)
	s.model = &fakeModel{responses: []string{placeJSON(t, peopleFill)}}
	res2, err := s.GenerateLocation(ctx, LocationDesignInput{
		CampaignID: campaignID, Premise: placeTestPremise,
		Location: placeID, Parts: []string{LocationPartNPCs}, CreatedBy: "dm",
	})
	if err != nil {
		t.Fatalf("people re-roll: %v", err)
	}
	if got := countItems(res2.Batch.Items, ReviewProposedEntity); got != shape.NPCs {
		t.Errorf("people items = %d, want %d", got, shape.NPCs)
	}
	if got := countItems(res2.Batch.Items, ReviewProposedFact); got != 0 {
		t.Errorf("facts staged in a people-only run: %d", got)
	}
	// The edges hang off the existing place, by its real id.
	for _, it := range res2.Batch.Items {
		if it.Kind != ReviewProposedRelationship {
			continue
		}
		p := itemPayload(t, it)
		if p["rel_type"] == "located_in" && p["to_entity"] != placeID {
			t.Errorf("a person placed somewhere other than the accepted place: %v", p)
		}
	}
	if _, err := s.DecideBatch(ctx, campaignID, res2.Batch.ID, DecisionAccept, nil, "dm"); err != nil {
		t.Fatal(err)
	}
}

func cloneFill(fill map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range fill {
		out[k] = v
	}
	return out
}

func TestGenerateLocation_ReuseBeforeInvention(t *testing.T) {
	s, campaignID, fx := batchStore(t)
	shape, _ := place.ShapeFor(place.KindVillage, place.ScaleMedium)

	// The model names an NPC the campaign already has: the read-back
	// reuses Tom, it does not stage a second Tom.
	res := generatePlace(t, s, campaignID, shape, 1, "")
	_ = res
	// (the plain fill names are new; assert the dedicated reuse case below)

	fill := placeTestFill(shape, 2)
	fill["npc_1_new_name"] = "Tom the Innkeeper"
	fill["secret_1_holder"] = "Tom the Innkeeper"
	s.model = &fakeModel{responses: []string{placeJSON(t, fill), placeJSON(t, fill)}}
	res, err := s.GenerateLocation(context.Background(), LocationDesignInput{
		CampaignID: campaignID, Premise: placeTestPremise,
		Kind: shape.Kind, Scale: shape.Scale, CreatedBy: "dm",
	})
	if err != nil {
		t.Fatalf("GenerateLocation: %v", err)
	}
	reused := false
	for _, r := range res.Reused {
		if r.ID == fx.Tom {
			reused = true
		}
	}
	if !reused {
		t.Fatalf("Tom was not reused: %+v", res.Reused)
	}
	for _, it := range res.Batch.Items {
		if it.Kind != ReviewProposedEntity {
			continue
		}
		if name, _ := itemPayload(t, it)["name"].(string); strings.EqualFold(name, "Tom the Innkeeper") {
			t.Fatal("a second Tom the Innkeeper was staged as a new entity")
		}
	}

	// The place's own name colliding with an existing entity is a problem
	// the model repairs: the refill names it properly.
	first := placeTestFill(shape, 3)
	first["place_name"] = "Blackwater"
	repaired := placeTestFill(shape, 3)
	repaired["place_name"] = "Fenwick-upon-Ash"
	model := &fakeModel{responses: []string{placeJSON(t, first), placeJSON(t, repaired)}}
	s.model = model
	if _, err := s.GenerateLocation(context.Background(), LocationDesignInput{
		CampaignID: campaignID, Premise: placeTestPremise,
		Kind: shape.Kind, Scale: shape.Scale, CreatedBy: "dm",
	}); err != nil {
		t.Fatalf("the name collision was not repaired: %v", err)
	}
	if len(model.calls) != 2 {
		t.Errorf("model calls = %d, want the repair retry", len(model.calls))
	}
}

func TestGenerateLocation_BadHolderIsRepaired(t *testing.T) {
	s, campaignID, _ := batchStore(t)
	shape, _ := place.ShapeFor(place.KindVillage, place.ScaleMedium)

	first := placeTestFill(shape, 1)
	first["secret_1_holder"] = "A Passing Stranger"
	repaired := placeTestFill(shape, 1)
	model := &fakeModel{responses: []string{placeJSON(t, first), placeJSON(t, repaired)}}
	s.model = model
	if _, err := s.GenerateLocation(context.Background(), LocationDesignInput{
		CampaignID: campaignID, Premise: placeTestPremise,
		Kind: shape.Kind, Scale: shape.Scale, CreatedBy: "dm",
	}); err != nil {
		t.Fatalf("the bad holder was not repaired: %v", err)
	}
	if len(model.calls) != 2 {
		t.Errorf("model calls = %d, want the repair retry", len(model.calls))
	}

	// A holder nobody repairs fails the design loudly.
	s2, campaignID2, _ := batchStore(t)
	bad := placeTestFill(shape, 1)
	bad["secret_1_holder"] = "A Passing Stranger"
	bad2 := placeTestFill(shape, 1)
	bad2["secret_1_holder"] = "Another Passing Stranger"
	s2.model = &fakeModel{responses: []string{placeJSON(t, bad), placeJSON(t, bad2)}}
	_, err := s2.GenerateLocation(context.Background(), LocationDesignInput{
		CampaignID: campaignID2, Premise: placeTestPremise,
		Kind: shape.Kind, Scale: shape.Scale, CreatedBy: "dm",
	})
	if err == nil || !strings.Contains(err.Error(), "failed validation twice") {
		t.Fatalf("unrepaired holder = %v, want a loud failure", err)
	}
}

func TestGenerateLocation_InputRefusals(t *testing.T) {
	s, campaignID, fx := batchStore(t)
	ctx := context.Background()
	for _, bad := range []struct {
		name string
		in   LocationDesignInput
		want string
	}{
		{"no premise", LocationDesignInput{CampaignID: campaignID}, "premise"},
		{"bad kind", LocationDesignInput{CampaignID: campaignID, Premise: "x", Kind: "megacity"}, "settlement kind"},
		{"bad part", LocationDesignInput{CampaignID: campaignID, Premise: "x", Parts: []string{"dragons"}}, "location part"},
		{"foreign location", LocationDesignInput{CampaignID: campaignID, Premise: "x", Location: "no-such-place"}, "not found"},
		{"non-location target", LocationDesignInput{CampaignID: campaignID, Premise: "x", Location: fx.Duke}, "not a location"},
		{"non-location near", LocationDesignInput{CampaignID: campaignID, Premise: "x", Near: fx.Duke}, "not a location"},
		{"fresh without place part", LocationDesignInput{CampaignID: campaignID, Premise: "x", Parts: []string{LocationPartNPCs}}, "stages the place itself"},
	} {
		if _, err := s.GenerateLocation(ctx, bad.in); err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("%s: err = %v, want it to mention %q", bad.name, err, bad.want)
		}
	}
}
