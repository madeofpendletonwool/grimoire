package server

// The table meta surface (MAD-428, stage 11 of MAD-417): the campaign's
// numbers, derived — plus the one write the 2014 rules name for
// everyone, inspiration.
//
//	GET  /api/campaigns/{id}/stats[?session=]  the fold — endcap or sitting
//	POST /api/campaigns/{id}/characters/{eid}/inspiration   award (DM)
//
// Stats are read by any member of the campaign: the party shot is the
// party's own. The DM's fold sees secret rolls; a player's fold runs
// the same queries the feed would — a secret roll is absent, not
// unprinted. Inspiration is the DM's word: awarded, never stacked, and
// spent through the roll flow (dice.go's spend_inspiration), which is
// where the advantage the 2014 rules promise is applied.

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/madeofpendletonwool/grimoire/internal/stats"
)

// statsEnabled reports the fold's availability.
func (s *Server) statsEnabled(w http.ResponseWriter) bool {
	if s.stats == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("campaign stats are not configured on this install"))
		return false
	}
	return true
}

// WithStats wires the campaign stats fold (MAD-428). Without it the
// stats endpoint answers 503.
func (s *Server) WithStats(store *stats.Store) *Server {
	s.stats = store
	return s
}

func (s *Server) handleCampaignStats(w http.ResponseWriter, r *http.Request) {
	if !s.statsEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	var (
		st  *stats.Stats
		err error
	)
	if sid := r.URL.Query().Get("session"); sid != "" {
		// The sitting must belong to this campaign — a session from
		// another table is not this fold's to read.
		var owner string
		if err = s.store.DB().QueryRowContext(r.Context(),
			`SELECT campaign_id FROM game_sessions WHERE id = ?`, sid).Scan(&owner); err != nil || owner != a.campaign.ID {
			writeError(w, http.StatusNotFound, fmt.Errorf("session %s", sid))
			return
		}
		st, err = s.stats.Session(r.Context(), sid, a.isDM())
	} else {
		st, err = s.stats.Campaign(r.Context(), a.campaign.ID, a.isDM())
	}
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"stats":    st,
		"markdown": st.Markdown(),
		"scope":    map[string]any{"dm": a.isDM()},
	})
}

/* ---------- inspiration ---------- */

func (s *Server) handleAwardInspiration(w http.ResponseWriter, r *http.Request) {
	if !s.ledgerEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	var req struct {
		Note string `json:"note"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %v", err))
		return
	}
	txn, balances, err := s.ledgers.AwardInspiration(r.Context(), a.campaign.ID, r.PathValue("eid"),
		req.Note, userID(r))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	views := make([]balanceView, 0, len(balances))
	for _, b := range balances {
		views = append(views, toBalanceView(b))
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"transaction": toTxnView(*txn),
		"balances":    views,
	})
}
