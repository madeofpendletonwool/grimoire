package server

// The loot surface (MAD-384): the three reads a DM wants over what the
// table already owns — the distribution, the power curve, the
// concentration warning — and the hoard generator whose amount is the
// DMG's and whose choices are the campaign's.
//
// Everything here is assembly. The arithmetic lives in internal/loot
// (pure, deterministic); this file folds the campaign's own records —
// the party block, the item entities, the relationships, the possession
// facts, the event log — into the shapes that arithmetic reads. The fold
// is deliberately ledger-free: every holding comes from a record another
// surface already wrote, so the distribution cannot disagree with the
// event log, only be silent about what the log never dated.

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/canon"
	"github.com/madeofpendletonwool/grimoire/internal/items"
	"github.com/madeofpendletonwool/grimoire/internal/loot"
	"github.com/madeofpendletonwool/grimoire/internal/story"
)

// lootEnabled reports whether the loot surface is wired: it generates from
// the item catalog and places through the canon engine.
func (s *Server) lootEnabled(w http.ResponseWriter) bool {
	if s.itemCatalog == nil || s.canon == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("the loot surface needs the item catalog and the canon engine wired"))
		return false
	}
	return true
}

/* ---------- the fold ---------- */

// holding sources in precedence order, most-dated first.
var holdingScore = map[string]int{
	loot.SourceRelation:   3,
	loot.SourceFact:       2,
	loot.SourcePartyBlock: 1,
}

// holdingPredicatesToPC are the possession predicates whose subject is the
// item ("the key is carried by the fighter"); holdingPredicatesFromPC are
// the ones whose subject is the holder ("the fighter holds the key"). Both
// subsets of campaign.PossessionPredicates, which the canon engine's
// continuity checks also read — the vocabulary one surface learns, every
// surface reads.
var (
	holdingPredicatesToPC = map[string]bool{
		"held_by": true, "carried_by": true, "possessed_by": true,
	}
	holdingPredicatesFromPC = map[string]bool{
		"bears": true, "holds": true, "has": true, "keeps": true,
	}
)

// foldHoldings assembles the loot reads' input: per pc, every item the
// campaign's own records say they hold, classified where the catalog can
// classify it, dated where the event log dates it.
//
// Records, in the order the fold prefers them: a dated relationship (pc
// owns item, since an event) is a hand-out with a play position; a
// possession fact (extracted at the table or authored by the DM) is a
// holding dated by the session it was recorded in; the party block's
// items list is the pc's declared sheet, undated. One (pc, item) pair
// yields one holding — the most-dated record wins, and the fold never
// double-counts the same sword.
func (s *Server) foldHoldings(r *http.Request, campaignID string, table *campaign.PartyTable) ([]loot.PC, error) {
	ctx := r.Context()
	isPC := map[string]bool{}
	for _, m := range table.Members {
		isPC[m.EntityID] = true
	}
	// Live item entities, by id. A deleted item is out of every read: it
	// is no longer in the campaign.
	itemEntities := map[string]string{}
	entities, err := s.campaigns.ListEntities(ctx, campaign.ScopeDM, campaignID, campaign.KindItem)
	if err != nil {
		return nil, err
	}
	for _, e := range entities {
		if e.Status != campaign.StatusDeleted {
			itemEntities[e.ID] = e.Name
		}
	}
	// The event log's play positions, for since_event dating.
	events := map[string]int64{}
	timeline, err := s.campaigns.ListEvents(ctx, campaign.ScopeDM, campaignID)
	if err != nil {
		return nil, err
	}
	for _, e := range timeline {
		events[e.ID] = e.RealOrdinal
	}
	// Session ordinals, for fact provenance dating.
	sessionOrd := map[string]int64{}
	if s.sessions != nil {
		sessions, err := s.sessions.ListSessions(ctx, campaignID)
		if err == nil {
			for _, ses := range sessions {
				sessionOrd[ses.ID] = ses.Ordinal
			}
		}
	}

	// Candidate holdings per pc, deduped by item with the most-dated
	// record winning.
	type cell struct {
		holding loot.Holding
		score   int
		ordinal int64
	}
	perPC := map[string]map[string]*cell{}
	add := func(pcID string, h loot.Holding) {
		if itemEntities[h.ItemID] == "" {
			return // not a live item entity of this campaign
		}
		if h.ItemName == "" {
			h.ItemName = itemEntities[h.ItemID]
		}
		m := perPC[pcID]
		if m == nil {
			m = map[string]*cell{}
			perPC[pcID] = m
		}
		score := holdingScore[h.Source]
		var ordinal int64
		if h.Ordinal != nil {
			ordinal = *h.Ordinal
		}
		if old, ok := m[h.ItemID]; ok {
			better := score > old.score
			if !better && score == old.score && h.Ordinal != nil && old.ordinal != 0 && ordinal < old.ordinal {
				better = true // same kind of record: the first acquisition wins
			}
			if !better {
				return
			}
		}
		m[h.ItemID] = &cell{holding: h, score: score, ordinal: ordinal}
	}

	// Dated relationships: pc --owns--> item, or item --owned_by--> pc.
	rels, err := s.campaigns.ListRelationships(ctx, campaign.ScopeDM, campaignID)
	if err != nil {
		return nil, err
	}
	for _, rel := range rels {
		var pcID, itemID string
		switch {
		case rel.RelType == "owns" && isPC[rel.FromEntity] && itemEntities[rel.ToEntity] != "":
			pcID, itemID = rel.FromEntity, rel.ToEntity
		case rel.RelType == "owned_by" && itemEntities[rel.FromEntity] != "" && isPC[rel.ToEntity]:
			pcID, itemID = rel.ToEntity, rel.FromEntity
		default:
			continue
		}
		h := loot.Holding{
			ItemID: itemID, Source: loot.SourceRelation,
		}
		if ord, ok := events[rel.SinceEvent]; ok {
			o := ord
			h.Ordinal = &o
		}
		add(pcID, h)
	}

	// Possession facts: the same predicates the continuity engine reads,
	// skipping superseded and proposed facts exactly as it does. A fact
	// extracted from a session is dated by that session's play position.
	facts, err := s.campaigns.ListFacts(ctx, campaign.ScopeDM, campaignID, campaign.FactFilter{NotSuperseded: true})
	if err != nil {
		return nil, err
	}
	for i := range facts {
		f := &facts[i]
		if f.SupersededBy != "" || f.Confidence == campaign.ConfidenceProposed {
			continue
		}
		if f.ObjectEntity == "" {
			continue
		}
		var pcID, itemID string
		switch {
		case holdingPredicatesToPC[campaign.NormalizePredicate(f.Predicate)] &&
			itemEntities[f.SubjectEntity] != "" && isPC[f.ObjectEntity]:
			itemID, pcID = f.SubjectEntity, f.ObjectEntity
		case holdingPredicatesFromPC[campaign.NormalizePredicate(f.Predicate)] &&
			isPC[f.SubjectEntity] && itemEntities[f.ObjectEntity] != "":
			itemID, pcID = f.ObjectEntity, f.SubjectEntity
		default:
			continue
		}
		h := loot.Holding{
			ItemID: itemID, Source: loot.SourceFact,
			Statement: f.Statement,
		}
		prov, err := s.campaigns.FactProvenance(ctx, campaign.ScopeDM, campaignID, f.ID)
		if err == nil {
			for _, p := range prov {
				if p.SessionID == "" {
					continue
				}
				if ord, ok := sessionOrd[p.SessionID]; ok {
					o := ord
					h.Session = &o
					break
				}
			}
		}
		add(pcID, h)
	}

	// The party block's declared items — the pc's own sheet, undated.
	for _, m := range table.Members {
		for _, id := range m.Block.Items {
			add(m.EntityID, loot.Holding{ItemID: id, Source: loot.SourcePartyBlock})
		}
	}

	// Classify: the catalog names the rarity and the tags a holding counts
	// under, SRD and homebrew alike. A holding the catalog cannot classify
	// still counts — it just counts as unclassified, and the curve says
	// so.
	overlay := s.itemHomebrewOverlay(r, campaignID)
	out := make([]loot.PC, 0, len(table.Members))
	for _, m := range table.Members {
		pc := loot.PC{EntityID: m.EntityID, Name: m.Name, Level: m.Block.Level, Class: m.Block.Class}
		for _, c := range perPC[m.EntityID] {
			h := c.holding
			if it, ok := s.itemCatalog.Lookup(h.ItemName, overlay); ok {
				h.Rarity = it.Rarity
				h.Tags = it.Tags
			}
			pc.Items = append(pc.Items, h)
		}
		out = append(out, pc)
	}
	return out, nil
}

/* ---------- the reads ---------- */

// handleLootDistribution serves the distribution view: who has received
// what, over time, folded from the campaign's own records.
func (s *Server) handleLootDistribution(w http.ResponseWriter, r *http.Request) {
	if !s.lootEnabled(w) {
		return
	}
	table := s.campaignParty(w, r, r.PathValue("id"))
	if table == nil {
		return
	}
	pcs, err := s.foldHoldings(r, table.CampaignID, table)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"distribution": loot.DistributionOf(pcs),
		"problems":     table.Problems,
	})
}

// handleLootPowerCurve serves the power curve: the party's items against
// the game's expectation for its tier, arithmetic attached.
func (s *Server) handleLootPowerCurve(w http.ResponseWriter, r *http.Request) {
	if !s.lootEnabled(w) {
		return
	}
	table := s.campaignParty(w, r, r.PathValue("id"))
	if table == nil {
		return
	}
	pcs, err := s.foldHoldings(r, table.CampaignID, table)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"power_curve": loot.PowerCurveOf(pcs)})
}

/* ---------- the generator ---------- */

// hoardRequest is one generate or reroll ask.
type hoardRequest struct {
	Tier  int    `json:"tier"`
	ActID string `json:"act_id"`
	// Seed and Rerolls drive RegenerateHoard: the seed is the generate
	// response's, a reroll entry names a line key and a fresh nonce. A
	// request with no seed rolls one.
	Seed    int64            `json:"seed"`
	Rerolls map[string]int64 `json:"rerolls"`
	HasSeed bool             `json:"has_seed"`
}

// resolveHoardRequest turns the ask into a loot.Request: the tier (given,
// or the party's), the party read, and the act context when the spine
// names one. It writes the HTTP error itself and returns nil when the
// caller should not proceed.
func (s *Server) resolveHoardRequest(w http.ResponseWriter, r *http.Request, a *campAccess, req hoardRequest) *loot.Request {
	ctx := r.Context()
	table, err := campaign.PartySnapshot(ctx, campaign.ScopeDM, s.campaigns.DB(), a.campaign.ID)
	if err != nil {
		writeStoreError(w, err)
		return nil
	}
	pcs, err := s.foldHoldings(r, table.CampaignID, table)
	if err != nil {
		writeStoreError(w, err)
		return nil
	}
	lr := &loot.Request{Levels: table.Levels(), Party: pcs}
	if req.Tier >= 1 && req.Tier <= 4 {
		lr.Tier = loot.Tier(req.Tier)
	}
	if req.ActID != "" {
		if s.stories == nil {
			writeError(w, http.StatusServiceUnavailable, errors.New("the narrative spine is not available on this install"))
			return nil
		}
		act, err := s.stories.GetAct(ctx, campaign.ScopeDM, a.campaign.ID, req.ActID)
		if err != nil {
			writeStoreError(w, err)
			return nil
		}
		actCtx := &loot.ActContext{Name: act.Name}
		if item := s.actItem(r, a.campaign.ID, act); item != nil {
			actCtx.Item = item
		}
		lr.Act = actCtx
	}
	return lr
}

// actItem finds the item the act names, if any: an item entity tied to one
// of the act's quests, else one on an act scene's cast. The first match in
// quest-then-scene order wins — the hoard carries one act item, and the
// reason says where it came from.
func (s *Server) actItem(r *http.Request, campaignID string, act *story.Act) *loot.NarrativeItem {
	ctx := r.Context()
	quests, err := s.campaigns.ListQuests(ctx, campaign.ScopeDM, campaignID)
	if err == nil {
		for _, q := range quests {
			if q.ActID != act.ID {
				continue
			}
			links, err := s.campaigns.QuestEntities(ctx, campaign.ScopeDM, campaignID, q.ID)
			if err != nil {
				continue
			}
			for _, l := range links {
				if e, err := s.campaigns.GetEntity(ctx, campaign.ScopeDM, campaignID, l.EntityID); err == nil &&
					e.Kind == campaign.KindItem && e.Status != campaign.StatusDeleted {
					return &loot.NarrativeItem{
						EntityID: e.ID, Name: e.Name, Summary: e.Summary,
						Where: fmt.Sprintf("the quest %q", q.Name),
					}
				}
			}
		}
	}
	scenes, err := s.stories.ListScenes(ctx, campaign.ScopeDM, campaignID, act.ID)
	if err != nil {
		return nil
	}
	for i := range scenes {
		sc, err := s.stories.GetScene(ctx, campaign.ScopeDM, campaignID, scenes[i].ID)
		if err != nil {
			continue
		}
		for _, cast := range sc.Cast {
			if e, err := s.campaigns.GetEntity(ctx, campaign.ScopeDM, campaignID, cast.EntityID); err == nil &&
				e.Kind == campaign.KindItem && e.Status != campaign.StatusDeleted {
				return &loot.NarrativeItem{
					EntityID: e.ID, Name: e.Name, Summary: e.Summary,
					Where: fmt.Sprintf("the scene %q", sc.Name),
				}
			}
		}
	}
	return nil
}

// hoardCatalog is the shelf a hoard draws from: the SRD mirror with the
// caller's and the campaign's homebrew riding over it, the same overlay
// the item picker serves.
func (s *Server) hoardCatalog(r *http.Request, campaignID string) []items.Item {
	overlay := s.itemHomebrewOverlay(r, campaignID)
	all := s.itemCatalog.All()
	if overlay != nil {
		all = append(all, overlay.Items()...)
	}
	return all
}

// handleLootGenerate rolls one hoard. Nothing is written; the response
// carries the seed, so the DM can reroll single lines later without
// moving the rest.
func (s *Server) handleLootGenerate(w http.ResponseWriter, r *http.Request) {
	s.handleLootRoll(w, r, false)
}

// handleLootReroll rebuilds the hoard with named lines rerolled. The seed
// and request must be the original generate's; every line whose key is
// not named comes back byte-identical.
func (s *Server) handleLootReroll(w http.ResponseWriter, r *http.Request) {
	s.handleLootRoll(w, r, true)
}

func (s *Server) handleLootRoll(w http.ResponseWriter, r *http.Request, reroll bool) {
	if !s.lootEnabled(w) {
		return
	}
	var req hoardRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, errors.New("invalid request body"))
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil || !a.requireDM(w) {
		return
	}
	// A tier that is not one of the game's four is a rejected ask, never a
	// silent fall back to the party's band.
	if req.Tier != 0 && (req.Tier < 1 || req.Tier > 4) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("tier %d is not one of the four tiers of play (1-4)", req.Tier))
		return
	}
	lr := s.resolveHoardRequest(w, r, a, req)
	if lr == nil {
		return // the error is already written
	}
	seed := req.Seed
	if !req.HasSeed {
		seed = rand.Int63()
	}
	var (
		h   *loot.Hoard
		err error
	)
	if reroll && req.HasSeed {
		h, err = loot.RegenerateHoard(*lr, s.hoardCatalog(r, a.campaign.ID), seed, req.Rerolls)
	} else {
		h, err = loot.GenerateHoard(*lr, s.hoardCatalog(r, a.campaign.ID), seed)
	}
	if err != nil {
		if errors.Is(err, loot.ErrNoTier) {
			writeError(w, http.StatusBadRequest, fmt.Errorf(
				"no tier in the request and no pc declares a level — say which tier of play this hoard is for"))
			return
		}
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"hoard": h})
}

/* ---------- the placement ---------- */

// handleLootPlace stages a hoard the DM is looking at: the generated items
// as item entities, the hand-out as an event, one proposal batch. Nothing
// is written until the DM approves the batch, and rerolling or walking
// away writes nothing at all.
func (s *Server) handleLootPlace(w http.ResponseWriter, r *http.Request) {
	if !s.lootEnabled(w) {
		return
	}
	var req struct {
		Summary string `json:"summary"`
		Items   []struct {
			Key      string `json:"key"`
			Slug     string `json:"slug"`
			Name     string `json:"name"`
			Summary  string `json:"summary"`
			Doc      string `json:"doc"`
			Rarity   string `json:"rarity"`
			Homebrew bool   `json:"homebrew"`
			Reason   string `json:"reason"`
			// EntityID marks the act's own item: already a campaign
			// entity, carried by the hand-out, never staged twice.
			EntityID string `json:"entity_id"`
		} `json:"items"`
		Participants []string `json:"participants"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, errors.New("invalid request body"))
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil || !a.requireDM(w) {
		return
	}
	in := canon.HoardPlaceInput{
		CampaignID: a.campaign.ID, Summary: req.Summary,
		ParticipantIDs: req.Participants, CreatedBy: userID(r),
	}
	for _, it := range req.Items {
		in.Items = append(in.Items, canon.HoardPlaceItem{
			Key: it.Key, Slug: it.Slug, Name: it.Name, Summary: it.Summary,
			Doc: it.Doc, Rarity: it.Rarity, Homebrew: it.Homebrew,
			Reason: it.Reason, EntityID: it.EntityID,
		})
	}
	batch, err := s.canon.PlaceHoard(r.Context(), in)
	if err != nil {
		if canon.IsOffline(err) {
			writeError(w, http.StatusServiceUnavailable, err)
			return
		}
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"batch": toBatchView(*batch, true)})
}
