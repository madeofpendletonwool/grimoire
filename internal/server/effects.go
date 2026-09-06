package server

// The duration and condition engine's surface (MAD-421, stage 4 of
// MAD-417): ongoing effects that count themselves down.
//
//	GET  /api/campaigns/{id}/effects                        the campaign's effects (DM)
//	GET  /api/campaigns/{id}/effects/concentrations         who is concentrating on what (DM)
//	POST /api/campaigns/{id}/effects                        apply an effect (DM)
//	POST /api/campaigns/{id}/effects/advance                the combat clock ticks N rounds (DM)
//	POST /api/campaigns/{id}/effects/{fxid}/end             dispel or end by hand (DM)
//	GET  /api/campaigns/{id}/characters/{eid}/effects       one character's effects (player: own)
//
// Permissions: applying, ending and ticking are the DM's — the table's
// referee decides what is on whom. The player surface is the read of
// their own bound character's effects, exactly and only: the scope check
// is the same requireOwnCharacter gate the resource ledger uses, and a
// player cannot reach another character's rows (the leak test asserts
// the absence, not the hiding). Conditions surface the indexed SRD text
// beside the row — applying poisoned hands the table the real rules.
//
// The world clock needs no endpoint: every read derives against the
// campaign clock and the applied rests, so travel and sleep expire
// things whether or not anyone was watching.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/madeofpendletonwool/grimoire/internal/effects"
)

// effectsEnabled reports the effect engine's availability.
func (s *Server) effectsEnabled(w http.ResponseWriter) bool {
	if s.effectEngine == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("the effect engine is not configured on this install"))
		return false
	}
	return true
}

// WithEffects wires the duration and condition engine (MAD-421). Without
// it the effect endpoints answer 503.
func (s *Server) WithEffects(store *effects.Store) *Server {
	s.effectEngine = store
	return s
}

/* ---------- views ---------- */

type effectView struct {
	ID            string           `json:"id"`
	TargetID      string           `json:"target_id"`
	TargetName    string           `json:"target_name"`
	Kind          string           `json:"kind"`
	Name          string           `json:"name"`
	Ref           string           `json:"ref,omitempty"`
	SourceID      string           `json:"source_id,omitempty"`
	SourceName    string           `json:"source_name,omitempty"`
	Concentration bool             `json:"concentration,omitempty"`
	Duration      effects.Duration `json:"duration"`
	Declared      string           `json:"declared"`
	Remaining     int64            `json:"remaining_seconds"`
	Display       string           `json:"display"`
	Status        string           `json:"status"`
	EndReason     string           `json:"end_reason,omitempty"`
	EndedBy       string           `json:"ended_by,omitempty"`
	Actor         string           `json:"actor,omitempty"`
	SessionID     string           `json:"session_id,omitempty"`
	EventID       string           `json:"session_event_id,omitempty"`
	Note          string           `json:"note,omitempty"`
	AppliedAt     string           `json:"applied_at"`
	EndedAt       string           `json:"ended_at,omitempty"`
	SRD           *effects.SRDText `json:"srd,omitempty"`
}

// toEffectView renders one row with its derived running state. Conditions
// carry the indexed SRD text beside them — the real rules, not a
// paraphrase — resolved once per name per request.
func (s *Server) toEffectView(ctx context.Context, r effects.Row, grounded map[string]effects.SRDText) effectView {
	v := effectView{
		ID: r.ID, TargetID: r.TargetID, TargetName: r.TargetName,
		Kind: r.Kind, Name: r.Name, Ref: r.Ref,
		SourceID: r.SourceID, SourceName: r.SourceName, Concentration: r.Concentration,
		Duration:  effects.Duration{Amount: r.Amount, Unit: r.Unit},
		Declared:  r.Duration.String(),
		Remaining: r.Running.Remaining, Display: r.Running.Display(),
		Status: r.Status, EndReason: r.EndReason, EndedBy: r.EndedBy, Actor: r.Actor,
		SessionID: r.SessionID, EventID: r.EventID, Note: r.Note,
		AppliedAt: r.AppliedAt.Format(http.TimeFormat),
	}
	if !r.EndedAt.IsZero() {
		v.EndedAt = r.EndedAt.Format(http.TimeFormat)
	}
	if r.Running.Ended && r.Status == effects.StatusActive {
		// Time wore the row away between writes; the read says so.
		v.Status = effects.StatusEnded
		v.EndReason = r.Running.EndReason
	}
	if r.Kind == effects.KindCondition {
		srd, ok := grounded[r.Name]
		if !ok && s.effectEngine != nil {
			srd = s.effectEngine.GroundCondition(ctx, r.Name)
			grounded[r.Name] = srd
		}
		if srd.Ref != "" || srd.Body != "" {
			v.SRD = &srd
		}
	}
	return v
}

func (s *Server) toEffectViews(ctx context.Context, rows []effects.Row) []effectView {
	out := make([]effectView, 0, len(rows))
	grounded := map[string]effects.SRDText{}
	for _, r := range rows {
		out = append(out, s.toEffectView(ctx, r, grounded))
	}
	return out
}

/* ---------- reads ---------- */

func (s *Server) handleCampaignEffects(w http.ResponseWriter, r *http.Request) {
	if !s.effectsEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	target := r.URL.Query().Get("target")
	includeEnded := r.URL.Query().Get("ended") == "1"
	rows, err := s.effectEngine.List(r.Context(), a.campaign.ID, target, includeEnded)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"effects": s.toEffectViews(r.Context(), rows)})
}

func (s *Server) handleCharacterEffects(w http.ResponseWriter, r *http.Request) {
	if !s.effectsEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	eid := r.PathValue("eid")
	if !s.requireOwnCharacter(w, a, eid) {
		return
	}
	includeEnded := r.URL.Query().Get("ended") == "1"
	rows, err := s.effectEngine.List(r.Context(), a.campaign.ID, eid, includeEnded)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"effects": s.toEffectViews(r.Context(), rows)})
}

func (s *Server) handleEffectConcentrations(w http.ResponseWriter, r *http.Request) {
	if !s.effectsEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	rows, err := s.effectEngine.Concentrations(r.Context(), a.campaign.ID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"concentrations": s.toEffectViews(r.Context(), rows)})
}

/* ---------- apply ---------- */

type effectApplyRequest struct {
	TargetID      string           `json:"target_id"`
	Kind          string           `json:"kind"`
	Name          string           `json:"name"`
	Ref           string           `json:"ref"`
	SourceID      string           `json:"source_id"`
	Concentration bool             `json:"concentration"`
	Duration      effects.Duration `json:"duration"`
	Note          string           `json:"note"`
	SessionID     string           `json:"session_id"`
	EventID       string           `json:"session_event_id"`
}

func (s *Server) handleApplyEffect(w http.ResponseWriter, r *http.Request) {
	if !s.effectsEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	var req effectApplyRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %v", err))
		return
	}
	out, err := s.effectEngine.Apply(r.Context(), a.campaign.ID, effects.ApplyInput{
		TargetID: req.TargetID, Kind: req.Kind, Name: req.Name, Ref: req.Ref,
		SourceID: req.SourceID, Concentration: req.Concentration, Duration: req.Duration,
		Note: req.Note, SessionID: req.SessionID, EventID: req.EventID,
	}, userID(r))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	// The applied row's derived state, grounded for conditions.
	applied, err := s.effectEngine.Get(r.Context(), a.campaign.ID, out.Applied.ID, false)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"effect":              s.toEffectView(r.Context(), *applied, map[string]effects.SRDText{}),
		"broke_concentration": s.toEffectViews(r.Context(), out.BrokeConcentration),
		"superseded":          s.toEffectViews(r.Context(), out.Superseded),
	})
}

/* ---------- the combat clock ---------- */

func (s *Server) handleEffectsAdvance(w http.ResponseWriter, r *http.Request) {
	if !s.effectsEnabled(w) {
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
		Rounds int64 `json:"rounds"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %v", err))
		return
	}
	active, expired, err := s.effectEngine.AdvanceRounds(r.Context(), a.campaign.ID, req.Rounds)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"rounds_ticked": req.Rounds,
		"active":        s.toEffectViews(r.Context(), active),
		"expired":       s.toEffectViews(r.Context(), expired),
	})
}

/* ---------- ending ---------- */

func (s *Server) handleEndEffect(w http.ResponseWriter, r *http.Request) {
	if !s.effectsEnabled(w) {
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
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %v", err))
		return
	}
	ended, err := s.effectEngine.End(r.Context(), a.campaign.ID, r.PathValue("fxid"), req.Reason, userID(r))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"effect": s.toEffectView(r.Context(), *ended, map[string]effects.SRDText{})})
}

/* ---------- the vocabulary ---------- */

// handleEffectVocabulary serves the declared vocabularies — the effect
// kinds, the duration units, the game's fifteen conditions — so surfaces
// offer the grammar instead of guessing at it.
func (s *Server) handleEffectVocabulary(w http.ResponseWriter, r *http.Request) {
	if !s.effectsEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	units := []string{
		effects.UnitRound, effects.UnitMinute, effects.UnitHour, effects.UnitDay,
		effects.UnitUntilDispelled, effects.UnitUntilRest,
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"kinds":      []string{effects.KindSpell, effects.KindCondition, effects.KindFeature, effects.KindOther},
		"units":      units,
		"conditions": effects.Conditions(),
	})
}
