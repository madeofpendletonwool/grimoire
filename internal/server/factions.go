package server

// The faction surface (MAD-366): the dossier and the plans.
//
// The dossier is mostly a read, not a write. Territory, leaders, members,
// allies, enemies and puppets are live graph edges — categorized by
// campaign.FactionEdgesOf and never copied into a payload — and the facts
// section is the scope's fact read over the faction as subject. The only
// stored interior is the payload's agent block, and the player scope sees
// exactly two fields of it: the public face and the reputation. Plans and
// PrivateTruth are DM material by construction (ADR 2): the player dossier
// does not omit them reluctantly, it cannot reach them — the player reads
// run through PlayerView, which has no method that returns a plan at all.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/faction"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
)

// WithFactions wires the faction plan store. Without it the plan endpoints
// report unavailable and dossiers render without plans.
func (s *Server) WithFactions(store *faction.Store) *Server {
	s.factions = store
	return s
}

// factionsEnabled reports whether the plan surface is wired, writing the
// error response when it is not.
func (s *Server) factionsEnabled(w http.ResponseWriter) bool {
	if s.factions == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("faction plans are not available"))
		return false
	}
	return true
}

/* ---------- views ---------- */

// factionEdgesView is the categorized edge read, as entity ids; the roster
// beside it names the ones the caller's scope can read.
type factionEdgesView struct {
	Territory []string `json:"territory"`
	Leaders   []string `json:"leaders"`
	Members   []string `json:"members"`
	Allies    []string `json:"allies"`
	Enemies   []string `json:"enemies"`
	Puppets   []string `json:"puppets"`
}

func toFactionEdgesView(e campaign.FactionEdges) factionEdgesView {
	v := factionEdgesView{
		Territory: e.Territory, Leaders: e.Leaders, Members: e.Members,
		Allies: e.Allies, Enemies: e.Enemies, Puppets: e.Puppets,
	}
	if v.Territory == nil {
		v.Territory = []string{}
	}
	if v.Leaders == nil {
		v.Leaders = []string{}
	}
	if v.Members == nil {
		v.Members = []string{}
	}
	if v.Allies == nil {
		v.Allies = []string{}
	}
	if v.Enemies == nil {
		v.Enemies = []string{}
	}
	if v.Puppets == nil {
		v.Puppets = []string{}
	}
	return v
}

// factionDossierView is the faction page's whole payload. The DM shape
// carries the full agent block and plans; the player shape carries the
// public face and reputation as top-level strings and no plans key at all.
type factionDossierView struct {
	campaignEntityView
	PublicFace string                 `json:"public_face,omitempty"`
	Reputation string                 `json:"reputation,omitempty"`
	Agent      *campaign.FactionAgent `json:"agent,omitempty"` // DM reads only
	Edges      factionEdgesView       `json:"edges"`
	Roster     map[string]rosterEntry `json:"roster"` // the ids Edges names, when the scope can read them
	Facts      []factView             `json:"facts"`
	Plans      []factionPlanView      `json:"plans,omitempty"` // DM reads only, by construction
}

// rosterEntry names one entity the dossier's edges reference.
type rosterEntry struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Status string `json:"status"`
}

// factionPlanStepView is one step of the plan's checklist with its live
// precondition status.
type factionPlanStepView struct {
	State    string                      `json:"state"`
	Name     string                      `json:"name"`
	Detail   string                      `json:"detail"`
	Cost     float64                     `json:"cost"`
	Done     bool                        `json:"done"`
	Requires []faction.RequirementStatus `json:"requires,omitempty"`
}

// factionPlanView is one plan for the DM's page: the machine, the state, the
// progress bar numbers, the steps with their preconditions, and the legal
// next states so the UI's buttons can never offer an illegal move.
type factionPlanView struct {
	ID            string                `json:"id"`
	FactionEntity string                `json:"faction_entity"`
	Name          string                `json:"name"`
	Machine       campaign.StateMachine `json:"state_machine"`
	CurrentState  string                `json:"current_state"`
	Status        string                `json:"status"`
	Visibility    string                `json:"visibility"`
	RatePerDay    float64               `json:"rate_per_day"`
	Progress      float64               `json:"progress"`
	Percent       float64               `json:"percent"`
	Steps         []factionPlanStepView `json:"steps"`
	NextStates    []string              `json:"next_states"`
	StartedDay    *int64                `json:"started_day,omitempty"`
	LastAdvanced  *int64                `json:"last_advanced_day,omitempty"`
	CreatedAt     string                `json:"created_at"`
	UpdatedAt     string                `json:"updated_at"`
}

func (s *Server) toFactionPlanView(ctx context.Context, p *faction.Plan) factionPlanView {
	entered := map[string]bool{}
	for _, st := range p.Reached {
		entered[st] = true
	}
	v := factionPlanView{
		ID: p.ID, FactionEntity: p.FactionEntity, Name: p.Name,
		Machine: p.Machine, CurrentState: p.CurrentState, Status: p.Status,
		Visibility: p.Visibility, RatePerDay: p.RatePerDay,
		Progress: p.Progress, Percent: p.PercentDone(),
		StartedDay: p.StartedDay, LastAdvanced: p.LastAdvanced,
	}
	for _, step := range p.Steps {
		sv := factionPlanStepView{
			State: step.State, Name: step.Name, Detail: step.Detail,
			Cost: step.Cost, Done: entered[step.State],
		}
		if !sv.Done && len(step.Requires) > 0 {
			if statuses, err := s.factions.RequirementStatuses(ctx, p); err == nil {
				sv.Requires = statuses
			}
		}
		v.Steps = append(v.Steps, sv)
	}
	for _, e := range p.Machine.Edges {
		if e.From == p.CurrentState {
			v.NextStates = append(v.NextStates, e.To)
		}
	}
	return v
}

/* ---------- the dossier ---------- */

// handleCampaignFactions lists a campaign's factions at the caller's scope.
func (s *Server) handleCampaignFactions(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	ctx := r.Context()
	var factions []campaign.Entity
	if a.isDM() {
		list, err := s.campaigns.ListEntities(ctx, campaign.ScopeDM, a.campaign.ID, campaign.KindFaction)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		factions = list
	} else {
		list, err := a.view.Entities(ctx, a.campaign.ID, campaign.KindFaction)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		factions = list
	}
	views := make([]campaignEntityView, 0, len(factions))
	for i := range factions {
		views = append(views, toCampaignEntityView(&factions[i], false))
	}
	writeJSON(w, http.StatusOK, map[string]any{"factions": views})
}

// handleCampaignFaction serves the scope-filtered dossier: the entity, the
// live edge categories, the facts the scope holds, and — DM only — the full
// agent block and the plans.
func (s *Server) handleCampaignFaction(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	eid := r.PathValue("eid")
	ctx := r.Context()
	var (
		entity *campaign.Entity
		rels   []campaign.Relationship
		facts  []campaign.Fact
	)
	if a.isDM() {
		e, err := s.campaigns.GetEntity(ctx, campaign.ScopeDM, a.campaign.ID, eid)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		entity = e
		list, err := s.campaigns.RelationshipsOf(ctx, campaign.ScopeDM, a.campaign.ID, eid)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		rels = list
		subject, err := s.campaigns.ListFacts(ctx, campaign.ScopeDM, a.campaign.ID, campaign.FactFilter{SubjectEntity: eid})
		if err != nil {
			writeStoreError(w, err)
			return
		}
		facts = subject
	} else {
		e, err := a.view.Entity(ctx, a.campaign.ID, eid)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		entity = e
		list, err := a.view.Relationships(ctx, a.campaign.ID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		for i := range list {
			if list[i].FromEntity == eid || list[i].ToEntity == eid {
				rels = append(rels, list[i])
			}
		}
		subject, err := a.view.Facts(ctx, a.campaign.ID, knowledge.FactFilter{SubjectEntity: eid})
		if err != nil {
			writeStoreError(w, err)
			return
		}
		facts = subject
	}
	if entity.Kind != campaign.KindFaction {
		writeError(w, http.StatusBadRequest, fmt.Errorf("%s is a %s, not a faction", entity.Name, entity.Kind))
		return
	}

	view := factionDossierView{
		campaignEntityView: toCampaignEntityView(entity, a.isDM()),
		Edges:              toFactionEdgesView(campaign.FactionEdgesOf(eid, rels)),
		Roster:             map[string]rosterEntry{},
	}
	for i := range facts {
		view.Facts = append(view.Facts, toFactView(&facts[i]))
	}
	if view.Facts == nil {
		view.Facts = []factView{}
	}

	// The roster names every entity the edge categories reference, read
	// through the caller's scope: an id the scope cannot resolve stays an
	// id — the edge is aware, the entity is not yet met.
	for _, id := range append(append(append(append(append(append([]string{},
		view.Edges.Territory...), view.Edges.Leaders...), view.Edges.Members...),
		view.Edges.Allies...), view.Edges.Enemies...), view.Edges.Puppets...) {
		if _, done := view.Roster[id]; done {
			continue
		}
		var (
			e   *campaign.Entity
			err error
		)
		if a.isDM() {
			e, err = s.campaigns.GetEntity(ctx, campaign.ScopeDM, a.campaign.ID, id)
		} else {
			e, err = a.view.Entity(ctx, a.campaign.ID, id)
		}
		if err != nil {
			continue
		}
		view.Roster[id] = rosterEntry{Name: e.Name, Kind: e.Kind, Status: e.Status}
	}

	if a.isDM() {
		agent := campaign.FactionAgentOf(entity)
		view.Agent = &agent
		if s.factions != nil {
			plans, err := s.factions.PlansOfFaction(ctx, campaign.ScopeDM, a.campaign.ID, eid)
			if err != nil {
				writeStoreError(w, err)
				return
			}
			for i := range plans {
				view.Plans = append(view.Plans, s.toFactionPlanView(ctx, &plans[i]))
			}
		}
	} else {
		// The player reads the faction's face — the two payload fields any
		// non-DM scope may see — through the knowledge layer's whitelisted
		// facade read. The payload itself never crosses the scope line.
		face, reputation, err := a.view.FactionFacade(ctx, a.campaign.ID, eid)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		view.PublicFace = face
		view.Reputation = reputation
	}
	writeJSON(w, http.StatusOK, map[string]any{"faction": view})
}

/* ---------- plans ---------- */

// handleFactionPlans lists one faction's plans. DM-only: a faction's plans
// are its private machinery.
func (s *Server) handleFactionPlans(w http.ResponseWriter, r *http.Request) {
	if !s.factionsEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	plans, err := s.factions.PlansOfFaction(r.Context(), campaign.ScopeDM, a.campaign.ID, r.PathValue("eid"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	views := make([]factionPlanView, 0, len(plans))
	for i := range plans {
		views = append(views, s.toFactionPlanView(r.Context(), &plans[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"plans": views})
}

// handleCreateFactionPlan adds a plan to a faction. DM-only.
func (s *Server) handleCreateFactionPlan(w http.ResponseWriter, r *http.Request) {
	if !s.factionsEnabled(w) {
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
		Name       string                `json:"name"`
		Machine    campaign.StateMachine `json:"state_machine"`
		Steps      []faction.Step        `json:"steps"`
		RatePerDay float64               `json:"rate_per_day"`
		Visibility string                `json:"visibility"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	if req.RatePerDay == 0 {
		req.RatePerDay = 1
	}
	p, err := s.factions.CreatePlan(r.Context(), a.campaign.ID, r.PathValue("eid"), faction.PlanInput{
		Name: req.Name, Machine: req.Machine, Steps: req.Steps,
		RatePerDay: req.RatePerDay, Visibility: req.Visibility,
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"plan": s.toFactionPlanView(r.Context(), p)})
}

// resolveFactionPlan loads a plan path value and its owning access, writing
// errors itself; nil when the caller should not proceed.
func (s *Server) resolveFactionPlan(w http.ResponseWriter, r *http.Request) (*campAccess, *faction.Plan) {
	if !s.factionsEnabled(w) {
		return nil, nil
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return nil, nil
	}
	if !a.requireDM(w) {
		return nil, nil
	}
	p, err := s.factions.GetPlan(r.Context(), campaign.ScopeDM, a.campaign.ID, r.PathValue("pid"))
	if err != nil {
		writeStoreError(w, err)
		return nil, nil
	}
	return a, p
}

// handleUpdateFactionPlan patches a plan's editable fields. DM-only.
func (s *Server) handleUpdateFactionPlan(w http.ResponseWriter, r *http.Request) {
	a, p := s.resolveFactionPlan(w, r)
	if a == nil {
		return
	}
	var req struct {
		Name       *string  `json:"name"`
		RatePerDay *float64 `json:"rate_per_day"`
		Status     *string  `json:"status"`
		Visibility *string  `json:"visibility"`
		Progress   *float64 `json:"progress"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	updated, err := s.factions.UpdatePlan(r.Context(), a.campaign.ID, p.ID, faction.UpdatePlanInput{
		Name: req.Name, RatePerDay: req.RatePerDay, Status: req.Status,
		Visibility: req.Visibility, Progress: req.Progress,
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"plan": s.toFactionPlanView(r.Context(), updated)})
}

// handleFactionPlanTransition moves a plan along a declared edge — the same
// rule quests follow. DM-only.
func (s *Server) handleFactionPlanTransition(w http.ResponseWriter, r *http.Request) {
	a, p := s.resolveFactionPlan(w, r)
	if a == nil {
		return
	}
	var req struct {
		To      string `json:"to"`
		EventID string `json:"event_id"`
		Reason  string `json:"reason"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	if req.Reason == "" {
		req.Reason = "moved by hand"
	}
	moved, err := s.factions.TransitionPlan(r.Context(), a.campaign.ID, p.ID, req.To, req.EventID, req.Reason)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"plan": s.toFactionPlanView(r.Context(), moved)})
}
