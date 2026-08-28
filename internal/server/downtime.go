package server

// The downtime surface (MAD-368): "I spend three weeks researching the cult."
// The pure three-stage resolution is internal/downtime; this file is the
// HTTP shape. Unlike every other campaign write, a PLAYER may call the
// request endpoint — for their own character only — because the request is
// the player's own time being spent. The result, though, is a proposal the
// DM decides, and the result the request computes names secrets the
// character has no path to yet: a player response carries the request's
// identity and never the computed outcome. Staging is the DM's, like every
// other write.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/downtime"
)

// WithDowntime wires downtime resolution (MAD-368). Without it the downtime
// endpoints answer 503.
func (s *Server) WithDowntime(store *downtime.Store) *Server {
	s.downtime = store
	return s
}

// downtimeEnabled reports the surface's availability: the store is wired
// only when the canon engine, the faction plans and the campaign store all
// are.
func (s *Server) downtimeEnabled(w http.ResponseWriter) bool {
	if s.downtime == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("downtime is not configured on this install"))
		return false
	}
	return true
}

// requestView is one downtime_requests row as the API renders it — the
// whole player-facing answer: the request was recorded, over this window,
// and the DM will decide it. It deliberately carries no findings.
type requestView struct {
	ID          string `json:"id"`
	CharacterID string `json:"character_id"`
	Activity    string `json:"activity"`
	Subject     string `json:"subject,omitempty"`
	Days        int    `json:"days"`
	FromDay     int64  `json:"from_day"`
	ToDay       int64  `json:"to_day"`
	Seed        int64  `json:"seed"`
	BatchID     string `json:"batch_id,omitempty"`
	Status      string `json:"status"`
	CreatedBy   string `json:"created_by,omitempty"`
	CreatedAt   int64  `json:"created_at"`
}

func toRequestView(r *downtime.RequestRow) requestView {
	return requestView{
		ID: r.ID, CharacterID: r.CharacterID, Activity: r.Activity, Subject: r.Subject,
		Days: r.Days, FromDay: r.FromDay, ToDay: r.ToDay, Seed: r.Seed,
		BatchID: r.BatchID, Status: r.Status, CreatedBy: r.CreatedBy,
		CreatedAt: r.CreatedAt.UnixMilli(),
	}
}

// handleCampaignDowntime records one downtime request:
// {"activity": "I research the cult", "subject": "<entity-id>", "days": 21,
// "seed": 7, "character": "<pc-id>"}.
//
// The DM may name any character of the campaign and gets the full
// deterministic result back. A player gets their own bound character — the
// character field is refused when it names anyone else — and gets the
// request back, never the result: what they find out is the proposal the DM
// decides. A player's subject must be an entity their own perspective can
// see; anything else is the same 404 a missing entity produces, so the
// endpoint cannot be probed for hidden entities. An unmappable activity is
// a clarifying question (400) with the candidates it pointed at.
func (s *Server) handleCampaignDowntime(w http.ResponseWriter, r *http.Request) {
	if !s.downtimeEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	var req struct {
		Activity  string `json:"activity"`
		Subject   string `json:"subject"`
		Character string `json:"character"`
		Days      int    `json:"days"`
		Seed      *int64 `json:"seed"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	character := req.Character
	if !a.isDM() {
		if a.member == nil || a.member.CharacterID == "" {
			writeError(w, http.StatusForbidden, fmt.Errorf("no character is bound to your membership; ask the DM to bind one first"))
			return
		}
		if character != "" && character != a.member.CharacterID {
			writeError(w, http.StatusForbidden, fmt.Errorf("a player may request downtime for their own character only"))
			return
		}
		character = a.member.CharacterID
		// The subject must be an entity the player's own perspective can
		// see: whether a name resolves at all is knowledge.
		if req.Subject != "" {
			if _, err := s.knowledge.Entity(r.Context(), a.playerScope, a.campaign.ID, req.Subject); err != nil {
				if errors.Is(err, campaign.ErrNotFound) || errors.Is(err, campaign.ErrInvalid) {
					writeError(w, http.StatusNotFound, fmt.Errorf("not found"))
					return
				}
				writeError(w, http.StatusInternalServerError, err)
				return
			}
		}
	}
	row, res, err := s.downtime.Request(r.Context(), a.campaign.ID, character,
		req.Activity, req.Subject, req.Days, req.Seed, userID(r))
	if err != nil {
		var clarify *downtime.ClarifyError
		if errors.As(err, &clarify) {
			writeError(w, http.StatusBadRequest, clarify)
			return
		}
		writeStoreError(w, err)
		return
	}
	out := map[string]any{"request": toRequestView(row)}
	if a.isDM() {
		out["result"] = res
	} else {
		// The player asked; the world will answer through the review queue.
		out["message"] = "your downtime request is recorded; the DM will decide it"
	}
	writeJSON(w, http.StatusCreated, out)
}

// handleCampaignDowntimeStage turns one recorded request into a proposal
// batch. DM-only, like every staging: deciding is the DM's work. The batch
// renders and decides on the ordinary review surface.
func (s *Server) handleCampaignDowntimeStage(w http.ResponseWriter, r *http.Request) {
	if !s.downtimeEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	batch, _, err := s.downtime.Stage(r.Context(), a.campaign.ID, r.PathValue("did"), userID(r))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"batch": toBatchView(*batch, true),
		"request": requestView{
			ID: r.PathValue("did"), Status: "staged", BatchID: batch.ID,
		},
	})
}
