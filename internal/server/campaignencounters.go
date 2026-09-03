package server

// Campaign-scoped encounters and the party block (MAD-378).
//
// Two surfaces live here. `/api/campaigns/{id}/encounters` lists and creates
// the encounters that belong to a campaign — DM only, per ADR 2, because a
// roster is DM material by definition and there is no filtered version of a
// monster list that would be safe to hand a player. And `campaign_id` on the
// builder's budget and design calls, which is what lets the party boxes
// prefill from the campaign's own party instead of the DM retyping four
// levels they have already written down.
//
// The non-campaign builder is untouched by all of it. Without a campaign_id
// every one of these paths is exactly the surface MAD-299 shipped: two number
// boxes and a default table. That is the fallback for a DM who has no
// campaign, and it must not regress.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/encounter"
)

/* ---------- the party block ---------- */

// partyView is the party block as the builder sees it: the whole declared
// table, the levels the encounter math runs on, and the line the surface
// prints above the party boxes.
type partyView struct {
	CampaignID string                  `json:"campaign_id"`
	Members    []campaign.PartyMember  `json:"members"`
	Levels     []int                   `json:"levels"`
	Label      string                  `json:"label,omitempty"`
	Problems   []campaign.PartyProblem `json:"problems,omitempty"`
}

func toPartyView(t *campaign.PartyTable) partyView {
	if t == nil {
		return partyView{}
	}
	return partyView{
		CampaignID: t.CampaignID, Members: t.Members, Levels: t.Levels(),
		Label: partyLabel(t.Levels()), Problems: t.Problems,
	}
}

// partyLabel renders the one line the builder shows when the party came from
// a campaign: "from your campaign — 4 characters, level 5", or the mixed-level
// spelling when the table is not uniform. Empty when nobody declares a level,
// because then nothing was prefilled and there is nothing to explain.
func partyLabel(levels []int) string {
	if len(levels) == 0 {
		return ""
	}
	noun := "characters"
	if len(levels) == 1 {
		noun = "character"
	}
	uniform := true
	for _, l := range levels[1:] {
		if l != levels[0] {
			uniform = false
			break
		}
	}
	if uniform {
		return fmt.Sprintf("from your campaign — %d %s, level %d", len(levels), noun, levels[0])
	}
	return fmt.Sprintf("from your campaign — %d %s, levels %s", len(levels), noun, levelList(levels))
}

// campaignParty resolves the caller's standing in a campaign and reads its
// party block. It writes the HTTP error itself and returns nil when the caller
// should not proceed — 404 for a campaign they have no standing in, 403 for a
// player, which is ADR 6's rule applied to a new surface: a narrower
// perspective gets a different read path, never a filtered version of this one.
func (s *Server) campaignParty(w http.ResponseWriter, r *http.Request, campaignID string) *campaign.PartyTable {
	a := s.resolveCampaignAccess(w, r, campaignID)
	if a == nil {
		return nil
	}
	if !a.requireDM(w) {
		return nil
	}
	table, err := campaign.PartySnapshot(r.Context(), campaign.ScopeDM, s.campaigns.DB(), a.campaign.ID)
	if err != nil {
		writeStoreError(w, err)
		return nil
	}
	return table
}

// handleCampaignParty serves one campaign's party block. The builder reads it
// to prefill, and the DM reads it to see what Grimoire thinks their table is —
// including the block keys it could not parse, which are reported rather than
// silently dropped.
func (s *Server) handleCampaignParty(w http.ResponseWriter, r *http.Request) {
	table := s.campaignParty(w, r, r.PathValue("id"))
	if table == nil {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"party": toPartyView(table)})
}

/* ---------- campaign-scoped encounters ---------- */

// handleCampaignEncounters lists (GET) and creates (POST) the encounters that
// belong to a campaign.
//
// A create with no party falls back to the campaign's declared levels, which
// is the whole point of the surface: the DM who wrote their party down once
// should not type it again to save a fight against it.
//
// session_event_id and scene_id are NOT writable here. Migration 0026 reserves
// them and this package round-trips them, but the layers that own those ids —
// session prep for the event, the spine for the scene — are what link a record
// to one, through encounter.CreateIn. Accepting either from a request body
// would mean writing a cross-campaign id into a column with no foreign key to
// catch it.
func (s *Server) handleCampaignEncounters(w http.ResponseWriter, r *http.Request) {
	if !s.encountersEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	if r.Method == http.MethodGet {
		list, err := s.encounters.ListCampaign(r.Context(), a.campaign.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		views := make([]encounterView, 0, len(list))
		for _, e := range list {
			views = append(views, toEncounterView(e))
		}
		writeJSON(w, http.StatusOK, map[string]any{"encounters": views})
		return
	}

	var req encounterRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	if req.Name == nil || strings.TrimSpace(*req.Name) == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("name is required"))
		return
	}
	if err := validateParty(req.Party); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	monsters, err := normalizeMonsters(req.Monsters)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := validateObjective(req.Objective); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := validateTerrain(req.Terrain); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	party := req.Party
	if len(party) == 0 {
		table, err := campaign.PartySnapshot(r.Context(), campaign.ScopeDM, s.campaigns.DB(), a.campaign.ID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		party = table.Levels()
	}
	notes := ""
	if req.Notes != nil {
		notes = *req.Notes
	}
	scope := encounter.Scope{CampaignID: a.campaign.ID, Status: encounter.StatusPlanned}
	e, err := s.encounters.CreateIn(r.Context(), userID(r), scope, *req.Name, party, monsters, notes,
		contentOptions(req)...)
	if err != nil {
		writeEncounterStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"encounter": toEncounterView(e)})
}

// writeEncounterStoreError maps the encounter store's sentinels onto HTTP
// statuses the way writeStoreError does for the campaign stores.
func writeEncounterStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, encounter.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, encounter.ErrInvalid):
		writeError(w, http.StatusBadRequest, err)
	default:
		writeError(w, http.StatusInternalServerError, err)
	}
}
