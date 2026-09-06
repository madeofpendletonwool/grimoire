package server

// The combat tracker's surface (MAD-422, stage 5 of MAD-417): the whole
// battle as one state machine.
//
//	GET  /api/campaigns/{id}/combat                                  the active battle (DM)
//	GET  /api/campaigns/{id}/combats                                 the campaign's battles (DM)
//	POST /api/campaigns/{id}/combat                                  start a battle (DM)
//	GET  /api/campaigns/{id}/combats/{cid}                           one battle + order + journal (DM)
//	POST /api/campaigns/{id}/combats/{cid}/next                      advance one turn (DM)
//	POST /api/campaigns/{id}/combats/{cid}/end                       end the battle (DM)
//	POST /api/campaigns/{id}/combats/{cid}/combatants/{ctid}/damage  apply damage (DM)
//	POST /api/campaigns/{id}/combats/{cid}/combatants/{ctid}/heal    apply healing (DM)
//	POST /api/campaigns/{id}/combats/{cid}/combatants/{ctid}/temp-hp grant temp hp (DM)
//	POST /api/campaigns/{id}/combats/{cid}/combatants/{ctid}/death-save  record a death save (DM)
//	POST /api/campaigns/{id}/combats/{cid}/combatants/{ctid}/reaction    record the reaction (DM)
//	POST /api/campaigns/{id}/combats/{cid}/combatants/{ctid}/legendary  spend a legendary action (DM)
//	POST /api/campaigns/{id}/combats/{cid}/combatants/{ctid}/conditions  apply a condition (DM)
//	POST /api/campaigns/{id}/combats/{cid}/combatants/{ctid}/conditions/{condid}/end  end one (DM)
//
// Permissions: the tracker is the DM's screen — every route here is the
// DM perspective. The table's shared view is a later surface's question
// (the MAD-318 merge); the journal and the session log are where
// players read the battle today. Initiative rolls are public dice like
// any other: they land in the shared feed the moment combat starts.

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/madeofpendletonwool/grimoire/internal/combat"
)

// combatEnabled reports the tracker's availability.
func (s *Server) combatEnabled(w http.ResponseWriter) bool {
	if s.combats == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("the combat tracker is not configured on this install"))
		return false
	}
	return true
}

// WithCombat wires the combat tracker (MAD-422). Without it the combat
// endpoints answer 503.
func (s *Server) WithCombat(store *combat.Store) *Server {
	s.combats = store
	return s
}

/* ---------- views ---------- */

type combatView struct {
	ID          string         `json:"id"`
	EncounterID string         `json:"encounter_id,omitempty"`
	SessionID   string         `json:"session_id,omitempty"`
	Name        string         `json:"name"`
	Status      string         `json:"status"`
	EndReason   string         `json:"end_reason,omitempty"`
	Round       int            `json:"round"`
	TurnIndex   int            `json:"turn_index"`
	StartedAt   string         `json:"started_at"`
	EndedAt     string         `json:"ended_at,omitempty"`
	Alive       map[string]int `json:"alive,omitempty"`
}

func toCombatView(c combat.Combat, order []combat.Combatant) combatView {
	v := combatView{
		ID: c.ID, EncounterID: c.EncounterID, SessionID: c.SessionID,
		Name: c.Name, Status: c.Status, EndReason: c.EndReason,
		Round: c.Round, TurnIndex: c.TurnIndex,
		StartedAt: c.StartedAt.Format(http.TimeFormat),
	}
	if !c.EndedAt.IsZero() {
		v.EndedAt = c.EndedAt.Format(http.TimeFormat)
	}
	if order != nil {
		v.Alive = map[string]int{"party": 0, "foe": 0}
		for _, ct := range order {
			if !ct.Dead {
				v.Alive[ct.Side]++
			}
		}
	}
	return v
}

type combatantView struct {
	ID             string             `json:"id"`
	EntityID       string             `json:"entity_id,omitempty"`
	Name           string             `json:"name"`
	Side           string             `json:"side"`
	Kind           string             `json:"kind"`
	Statblock      combat.Snapshot    `json:"statblock"`
	Initiative     int                `json:"initiative"`
	InitFormula    string             `json:"init_formula,omitempty"`
	AC             int                `json:"ac"`
	MaxHP          int                `json:"max_hp"`
	EffectiveMaxHP int                `json:"effective_max_hp"`
	HP             int                `json:"hp"`
	TempHP         int                `json:"temp_hp"`
	HPDisplay      string             `json:"hp_display"`
	Downed         bool               `json:"downed,omitempty"`
	Stable         bool               `json:"stable,omitempty"`
	Dead           bool               `json:"dead,omitempty"`
	DeathSuccesses int                `json:"death_successes,omitempty"`
	DeathFailures  int                `json:"death_failures,omitempty"`
	ReactionSpent  bool               `json:"reaction_spent,omitempty"`
	LegendaryUsed  int                `json:"legendary_used,omitempty"`
	LegendaryMax   int                `json:"legendary_max,omitempty"`
	Conditions     []combat.Condition `json:"conditions,omitempty"`
	Position       int                `json:"position"`
	IsTurn         bool               `json:"is_turn"`
}

func toCombatantView(c combat.Combatant, turnIndex int) combatantView {
	return combatantView{
		ID: c.ID, EntityID: c.EntityID, Name: c.Name, Side: c.Side, Kind: c.Kind,
		Statblock: c.Snapshot, Initiative: c.Initiative, InitFormula: c.InitFormula,
		AC: c.AC, MaxHP: c.MaxHP, EffectiveMaxHP: c.EffectiveMax(),
		HP: c.HP, TempHP: c.TempHP, HPDisplay: fmt.Sprintf("%d/%d", c.HP, c.EffectiveMax()),
		Downed: c.Downed, Stable: c.Stable, Dead: c.Dead,
		DeathSuccesses: c.DeathSuccesses, DeathFailures: c.DeathFailures,
		ReactionSpent: c.ReactionSpent,
		LegendaryUsed: c.LegendaryUsed, LegendaryMax: c.Snapshot.LegendaryMax,
		Conditions: c.Conditions, Position: c.Position, IsTurn: c.Position == turnIndex,
	}
}

func toCombatantViews(order []combat.Combatant, turnIndex int) []combatantView {
	out := make([]combatantView, 0, len(order))
	for _, c := range order {
		out = append(out, toCombatantView(c, turnIndex))
	}
	return out
}

type expiredEffectView struct {
	Name   string `json:"name"`
	Target string `json:"target_name"`
}

/* ---------- reads ---------- */

func (s *Server) handleActiveCombat(w http.ResponseWriter, r *http.Request) {
	if !s.combatEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	c, order, err := s.combats.Active(r.Context(), a.campaign.ID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if c == nil {
		writeJSON(w, http.StatusOK, map[string]any{"combat": nil})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"combat": toCombatView(*c, order),
		"order":  toCombatantViews(order, c.TurnIndex),
	})
}

func (s *Server) handleListCombats(w http.ResponseWriter, r *http.Request) {
	if !s.combatEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	list, err := s.combats.List(r.Context(), a.campaign.ID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	out := make([]combatView, 0, len(list))
	for _, c := range list {
		out = append(out, toCombatView(c, nil))
	}
	writeJSON(w, http.StatusOK, map[string]any{"combats": out})
}

func (s *Server) handleGetCombat(w http.ResponseWriter, r *http.Request) {
	if !s.combatEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	c, order, err := s.combats.Get(r.Context(), a.campaign.ID, r.PathValue("cid"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	log, err := s.combats.Log(r.Context(), a.campaign.ID, c.ID, 0, 200)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"combat": toCombatView(*c, order),
		"order":  toCombatantViews(order, c.TurnIndex),
		"log":    log,
	})
}

/* ---------- start ---------- */

func (s *Server) handleStartCombat(w http.ResponseWriter, r *http.Request) {
	if !s.combatEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	var req combat.StartInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %v", err))
		return
	}
	out, err := s.combats.Start(r.Context(), a.campaign.ID, req, userID(r))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	body := map[string]any{
		"combat": toCombatView(out.Combat, out.Order),
		"order":  toCombatantViews(out.Order, out.Combat.TurnIndex),
	}
	if len(out.Warnings) > 0 {
		body["warnings"] = out.Warnings
	}
	writeJSON(w, http.StatusCreated, body)
}

/* ---------- the turn engine ---------- */

func (s *Server) handleCombatNextTurn(w http.ResponseWriter, r *http.Request) {
	if !s.combatEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	out, err := s.combats.NextTurn(r.Context(), a.campaign.ID, r.PathValue("cid"), userID(r))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	body := map[string]any{
		"combat":        toCombatView(out.Combat, out.Order),
		"order":         toCombatantViews(out.Order, out.Combat.TurnIndex),
		"summary":       out.Summary,
		"round_wrapped": out.RoundWrapped,
		"lair_reminder": out.LairReminder,
		"alive":         out.Alive,
	}
	if len(out.Prompts) > 0 {
		body["prompts"] = out.Prompts
	}
	if len(out.ExpiredConditions) > 0 {
		body["expired_conditions"] = out.ExpiredConditions
	}
	if len(out.ExpiredEffects) > 0 {
		expired := make([]expiredEffectView, 0, len(out.ExpiredEffects))
		for _, row := range out.ExpiredEffects {
			expired = append(expired, expiredEffectView{Name: row.Name, Target: row.TargetName})
		}
		body["expired_effects"] = expired
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) handleEndCombat(w http.ResponseWriter, r *http.Request) {
	if !s.combatEnabled(w) {
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
	out, err := s.combats.End(r.Context(), a.campaign.ID, r.PathValue("cid"), req.Reason, userID(r))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"combat":  toCombatView(out.Combat, out.Order),
		"summary": out.Summary,
		"alive":   out.Alive,
	})
}

/* ---------- mid-play writes ---------- */

// combatantAction runs one mid-play combatant write behind the shared
// gate: DM perspective, active combat, one request body.
func (s *Server) combatantAction(w http.ResponseWriter, r *http.Request, run func(store *combat.Store) (any, error)) {
	if !s.combatEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	body, err := run(s.combats)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) handleCombatDamage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Amount    int    `json:"amount"`
		Type      string `json:"damage_type"`
		Note      string `json:"note"`
		ReduceMax bool   `json:"reduce_max"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %v", err))
		return
	}
	s.combatantAction(w, r, func(store *combat.Store) (any, error) {
		out, err := store.Damage(r.Context(), r.PathValue("id"), r.PathValue("cid"), r.PathValue("ctid"),
			req.Amount, req.Type, req.Note, req.ReduceMax, userID(r))
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"combatant":            toCombatantView(out.Combatant, -2),
			"outcome":              out.Outcome,
			"summary":              out.Summary,
			"concentration_checks": out.Concentration,
		}, nil
	})
}

func (s *Server) handleCombatHeal(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Amount int    `json:"amount"`
		Note   string `json:"note"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %v", err))
		return
	}
	s.combatantAction(w, r, func(store *combat.Store) (any, error) {
		out, err := store.Heal(r.Context(), r.PathValue("id"), r.PathValue("cid"), r.PathValue("ctid"),
			req.Amount, req.Note, userID(r))
		if err != nil {
			return nil, err
		}
		return map[string]any{"combatant": toCombatantView(out.Combatant, -2), "summary": out.Summary}, nil
	})
}

func (s *Server) handleCombatTempHP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Amount int    `json:"amount"`
		Note   string `json:"note"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %v", err))
		return
	}
	s.combatantAction(w, r, func(store *combat.Store) (any, error) {
		out, err := store.TempHP(r.Context(), r.PathValue("id"), r.PathValue("cid"), r.PathValue("ctid"),
			req.Amount, req.Note, userID(r))
		if err != nil {
			return nil, err
		}
		return map[string]any{"combatant": toCombatantView(out.Combatant, -2), "summary": out.Summary}, nil
	})
}

func (s *Server) handleCombatDeathSave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Result string `json:"result"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %v", err))
		return
	}
	s.combatantAction(w, r, func(store *combat.Store) (any, error) {
		out, err := store.DeathSave(r.Context(), r.PathValue("id"), r.PathValue("cid"), r.PathValue("ctid"),
			req.Result, userID(r))
		if err != nil {
			return nil, err
		}
		return map[string]any{"combatant": toCombatantView(out.Combatant, -2), "outcome": out.Outcome, "summary": out.Summary}, nil
	})
}

func (s *Server) handleCombatReaction(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Spent *bool `json:"spent"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %v", err))
		return
	}
	s.combatantAction(w, r, func(store *combat.Store) (any, error) {
		// Absent the flag the call toggles: spent when ready, ready
		// when spent.
		out, err := store.Reaction(r.Context(), r.PathValue("id"), r.PathValue("cid"), r.PathValue("ctid"),
			req.Spent == nil || *req.Spent, userID(r))
		if err != nil {
			return nil, err
		}
		return map[string]any{"combatant": toCombatantView(out.Combatant, -2), "summary": out.Summary}, nil
	})
}

func (s *Server) handleCombatLegendary(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Ability string `json:"ability"`
		Cost    int    `json:"cost"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %v", err))
		return
	}
	s.combatantAction(w, r, func(store *combat.Store) (any, error) {
		out, err := store.Legendary(r.Context(), r.PathValue("id"), r.PathValue("cid"), r.PathValue("ctid"),
			req.Ability, req.Cost, userID(r))
		if err != nil {
			return nil, err
		}
		return map[string]any{"combatant": toCombatantView(out.Combatant, -2), "summary": out.Summary}, nil
	})
}

func (s *Server) handleCombatApplyCondition(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name   string `json:"name"`
		Rounds int    `json:"rounds"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %v", err))
		return
	}
	s.combatantAction(w, r, func(store *combat.Store) (any, error) {
		out, err := store.ApplyCondition(r.Context(), r.PathValue("id"), r.PathValue("cid"), r.PathValue("ctid"),
			req.Name, req.Rounds, userID(r))
		if err != nil {
			return nil, err
		}
		return map[string]any{"combatant": toCombatantView(out.Combatant, -2), "condition": out.Condition, "summary": out.Summary}, nil
	})
}

func (s *Server) handleCombatEndCondition(w http.ResponseWriter, r *http.Request) {
	s.combatantAction(w, r, func(store *combat.Store) (any, error) {
		out, err := store.EndCondition(r.Context(), r.PathValue("id"), r.PathValue("cid"), r.PathValue("ctid"),
			r.PathValue("condid"), userID(r))
		if err != nil {
			return nil, err
		}
		return map[string]any{"combatant": toCombatantView(out.Combatant, -2), "summary": out.Summary}, nil
	})
}
