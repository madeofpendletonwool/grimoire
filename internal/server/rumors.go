package server

// The rumour mill's HTTP surface (MAD-374): CRUD over
// /api/campaigns/{id}/rumors, the heard path that writes stances, and the
// generate path behind the model key. Reads resolve the caller's scope —
// a player reads the statement, the spread and who said it, and never the
// truth column, because the store's SQL never loaded it; writes are DM-only
// like every canon write.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/canon"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
)

// rumorHolderView is who repeats a rumour, as the wire carries it.
type rumorHolderView struct {
	Entity  string `json:"entity"`
	Name    string `json:"name"`
	Variant string `json:"variant,omitempty"`
}

// rumorView is one rumour as the wire carries it. Truth and FactID ride in
// the struct and render for the DM; at a player scope the store selected
// them as empty strings, so the JSON keys render empty — the columns never
// crossed the scope line to be filtered here.
type rumorView struct {
	ID        string            `json:"id"`
	Statement string            `json:"statement"`
	Truth     string            `json:"truth,omitempty"`
	About     *entityChip       `json:"about,omitempty"`
	FactID    string            `json:"fact_id,omitempty"`
	Origin    string            `json:"origin,omitempty"`
	Spread    string            `json:"spread"`
	Status    string            `json:"status"`
	DMOnly    bool              `json:"dm_only,omitempty"`
	Holders   []rumorHolderView `json:"holders"`
	CreatedAt string            `json:"created_at"`
}

// entityNames resolves entity ids to names for one render: who said it,
// and what the rumour circles. Unknown ids render as their id — a holder
// name never blocks a rumour from rendering.
func (s *Server) entityNames(ctx context.Context, campaignID string, ids ...string) map[string]string {
	names := map[string]string{}
	for _, id := range ids {
		if id == "" {
			continue
		}
		if e, err := s.campaigns.GetEntity(ctx, campaign.ScopeDM, campaignID, id); err == nil {
			names[id] = e.Name
		}
	}
	return names
}

// toRumorView renders one rumour with its holders.
func toRumorView(r *campaign.Rumor, holders []campaign.RumorHolder, names map[string]string) rumorView {
	v := rumorView{
		ID: r.ID, Statement: r.Statement, Truth: r.Truth, FactID: r.FactID,
		Origin: r.Origin, Spread: r.Spread, Status: r.Status, DMOnly: r.DMOnly,
		Holders: []rumorHolderView{},
	}
	for _, h := range holders {
		hv := rumorHolderView{Entity: h.EntityID, Variant: h.Variant}
		if name, ok := names[h.EntityID]; ok {
			hv.Name = name
		} else {
			hv.Name = h.EntityID
		}
		v.Holders = append(v.Holders, hv)
	}
	return v
}

/* ---------- reads ---------- */

// handleCampaignRumors lists the mill at the caller's scope. Query params:
// about (an entity id) and status; truth is a DM-only filter and is refused
// at a player scope.
func (s *Server) handleCampaignRumors(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	ctx := r.Context()
	filter := knowledge.RumorFilter{
		About:  r.URL.Query().Get("about"),
		Status: r.URL.Query().Get("status"),
		Truth:  r.URL.Query().Get("truth"),
	}
	scope := campaign.ScopeDM
	if !a.isDM() {
		scope = a.playerScope
	}
	rumors, err := s.knowledge.Rumors(ctx, scope, a.campaign.ID, filter)
	if err != nil {
		writeStoreError(w, err)
		return
	}

	// Names for the about chips, one read.
	ids := map[string]bool{}
	for i := range rumors {
		if rumors[i].AboutEntity != "" {
			ids[rumors[i].AboutEntity] = true
		}
	}
	nameArgs := make([]string, 0, len(ids))
	for id := range ids {
		nameArgs = append(nameArgs, id)
	}
	names := s.entityNames(ctx, a.campaign.ID, nameArgs...)

	views := make([]rumorView, 0, len(rumors))
	for i := range rumors {
		holders, err := s.knowledge.Holders(ctx, scope, a.campaign.ID, rumors[i].ID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		v := toRumorView(&rumors[i], holders, names)
		if name, ok := names[rumors[i].AboutEntity]; ok && rumors[i].AboutEntity != "" {
			v.About = &entityChip{ID: rumors[i].AboutEntity, Name: name}
		}
		v.CreatedAt = rumors[i].CreatedAt.Format("2006-01-02")
		views = append(views, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"rumors": views})
}

// handleCampaignRumor serves one rumour at the caller's scope, holders
// attached — "what are people saying" from the NPC sheet and the location
// dossier both land here.
func (s *Server) handleCampaignRumor(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	ctx := r.Context()
	scope := campaign.ScopeDM
	if !a.isDM() {
		scope = a.playerScope
	}
	ru, err := s.knowledge.Rumor(ctx, scope, a.campaign.ID, r.PathValue("rid"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	holders, err := s.knowledge.Holders(ctx, scope, a.campaign.ID, ru.ID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	names := s.entityNames(ctx, a.campaign.ID, ru.AboutEntity)
	v := toRumorView(ru, holders, names)
	if name, ok := names[ru.AboutEntity]; ok && ru.AboutEntity != "" {
		v.About = &entityChip{ID: ru.AboutEntity, Name: name}
	}
	v.CreatedAt = ru.CreatedAt.Format("2006-01-02")
	writeJSON(w, http.StatusOK, map[string]any{"rumor": v})
}

/* ---------- writes ---------- */

// handleCreateCampaignRumor authors one rumour by hand: the statement, the
// truth value the DM holds, optional about / fact links, spread and
// origin. Holders attach through their own call.
func (s *Server) handleCreateCampaignRumor(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	var req struct {
		Statement string `json:"statement"`
		Truth     string `json:"truth"`
		About     string `json:"about"`
		Fact      string `json:"fact_id"`
		Origin    string `json:"origin"`
		Spread    string `json:"spread"`
		Status    string `json:"status"`
		DMOnly    bool   `json:"dm_only"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	created, err := s.knowledge.CreateRumor(r.Context(), a.campaign.ID, knowledge.RumorInput{
		Statement: req.Statement, Truth: req.Truth, AboutEntity: req.About, FactID: req.Fact,
		Origin: req.Origin, Spread: req.Spread, Status: req.Status, DMOnly: req.DMOnly,
		CreatedBy: userID(r),
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	names := s.entityNames(r.Context(), a.campaign.ID, created.AboutEntity)
	v := toRumorView(created, nil, names)
	if name, ok := names[created.AboutEntity]; ok && created.AboutEntity != "" {
		v.About = &entityChip{ID: created.AboutEntity, Name: name}
	}
	v.CreatedAt = created.CreatedAt.Format("2006-01-02")
	writeJSON(w, http.StatusCreated, map[string]any{"rumor": v})
}

// handleUpdateCampaignRumor patches one rumour — the DM settles what the
// rumour actually was (truth), where it has travelled (spread), and its
// life (status: debunked, confirmed, dormant).
func (s *Server) handleUpdateCampaignRumor(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	var req struct {
		Statement *string `json:"statement"`
		Truth     *string `json:"truth"`
		About     *string `json:"about"`
		Fact      *string `json:"fact_id"`
		Origin    *string `json:"origin"`
		Spread    *string `json:"spread"`
		Status    *string `json:"status"`
		DMOnly    *bool   `json:"dm_only"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	up := knowledge.RumorUpdate{
		Statement: req.Statement, Truth: req.Truth, AboutEntity: req.About, FactID: req.Fact,
		Origin: req.Origin, Spread: req.Spread, Status: req.Status, DMOnly: req.DMOnly,
	}
	updated, err := s.knowledge.UpdateRumor(r.Context(), a.campaign.ID, r.PathValue("rid"), up)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	holders, err := s.knowledge.Holders(r.Context(), campaign.ScopeDM, a.campaign.ID, updated.ID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	names := s.entityNames(r.Context(), a.campaign.ID, updated.AboutEntity)
	v := toRumorView(updated, holders, names)
	if name, ok := names[updated.AboutEntity]; ok && updated.AboutEntity != "" {
		v.About = &entityChip{ID: updated.AboutEntity, Name: name}
	}
	v.CreatedAt = updated.CreatedAt.Format("2006-01-02")
	writeJSON(w, http.StatusOK, map[string]any{"rumor": v})
}

// handleDeleteCampaignRumor takes a rumour out of the mill.
func (s *Server) handleDeleteCampaignRumor(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	if err := s.knowledge.DeleteRumor(r.Context(), a.campaign.ID, r.PathValue("rid")); err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// handleSetRumorHolder records who repeats a rumour, in their own words.
func (s *Server) handleSetRumorHolder(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	var req struct {
		Entity     string `json:"entity"`
		Variant    string `json:"variant"`
		SinceEvent string `json:"since_event"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	h, err := s.knowledge.SetRumorHolder(r.Context(), a.campaign.ID, r.PathValue("rid"),
		req.Entity, req.Variant, req.SinceEvent)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"holder": h})
}

// handleDeleteRumorHolder takes a holder out of the mill.
func (s *Server) handleDeleteRumorHolder(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	if err := s.knowledge.RemoveRumorHolder(r.Context(), a.campaign.ID,
		r.PathValue("rid"), r.PathValue("eid")); err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// handleRumorHeard records that a knower heard a rumour and writes the
// stance it earns — suspects on a true rumour's fact, believes_false on
// the fact a false or distorted one contradicts, a holding on the mill for
// one that names nothing. Gossip never downgrades knowledge.
func (s *Server) handleRumorHeard(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	var req struct {
		Knower     string `json:"knower"`
		SinceEvent string `json:"since_event"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	res, err := s.knowledge.RumorHeard(r.Context(), a.campaign.ID, r.PathValue("rid"),
		req.Knower, req.SinceEvent)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"heard": res})
}

/* ---------- generation ---------- */

// handleRumorGenerate runs one generation exchange: a subject, a truth mix,
// an optional premise. Returns the staged batch — nothing enters the mill
// until it is decided — plus the facts each slot drew.
func (s *Server) handleRumorGenerate(w http.ResponseWriter, r *http.Request) {
	if !s.canonEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	if !s.llm.Configured() {
		writeError(w, http.StatusServiceUnavailable, errors.New(
			"generating rumours needs the model. Set ANTHROPIC_API_KEY to enable it"))
		return
	}
	var req struct {
		About     string `json:"about"`
		True      int    `json:"true"`
		False     int    `json:"false"`
		Distorted int    `json:"distorted"`
		Premise   string `json:"premise"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	res, err := s.canon.GenerateRumors(r.Context(), canon.RumorDesignInput{
		CampaignID: a.campaign.ID, About: req.About,
		TrueCount: req.True, FalseCount: req.False, DistortedCount: req.Distorted,
		Premise: req.Premise, CreatedBy: userID(r),
	})
	if err != nil {
		if canon.IsOffline(err) {
			writeError(w, http.StatusServiceUnavailable, err)
			return
		}
		writeStoreError(w, err)
		return
	}
	body := map[string]any{
		"facts":   res.Facts,
		"holders": res.Holders,
	}
	if res.Batch != nil {
		body["batch"] = toBatchView(*res.Batch, true)
	}
	writeJSON(w, http.StatusCreated, body)
}
