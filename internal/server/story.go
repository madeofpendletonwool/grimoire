package server

// The story planner surface (MAD-360): REST under /api/campaigns/... for
// acts, scenes, cast, secrets, outcomes and session plans, plus the
// deterministic planning helpers (/api/story/shapes, /api/story/pace) that
// need no model and no key. The spine is DM material end to end — every
// campaign-scoped route here resolves access and requires the DM's
// perspective before touching the store; there is no player-readable form.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/story"
)

// WithStory wires the narrative spine. Without it the story endpoints report
// unavailable, the same additive pattern the other campaign stores follow.
func (s *Server) WithStory(stories *story.Store) *Server {
	s.stories = stories
	return s
}

// storyEnabled reports whether the spine is wired, writing the error
// response when it is not.
func (s *Server) storyEnabled(w http.ResponseWriter) bool {
	if s.stories == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("the story planner is not available"))
		return false
	}
	return true
}

/* ---------- views ---------- */

func toActView(a story.Act) map[string]any {
	return map[string]any{
		"id": a.ID, "ordinal": a.Ordinal, "name": a.Name, "premise": a.Premise,
		"level_start": a.LevelStart, "level_end": a.LevelEnd, "status": a.Status,
		"created_at": a.CreatedAt.Format(http.TimeFormat),
		"updated_at": a.UpdatedAt.Format(http.TimeFormat),
	}
}

func toSceneView(sc story.Scene) map[string]any {
	return map[string]any{
		"id": sc.ID, "act_id": sc.ActID, "session_id": sc.SessionID,
		"ordinal": sc.Ordinal, "kind": sc.Kind, "name": sc.Name,
		"purpose": sc.Purpose, "setting_entity": sc.SettingEntity,
		"status": sc.Status,
		"cast":   sc.Cast, "secrets": sc.Secrets, "outcomes": sc.Outcomes,
		"created_at": sc.CreatedAt.Format(http.TimeFormat),
		"updated_at": sc.UpdatedAt.Format(http.TimeFormat),
	}
}

func toPlanView(p story.SessionPlan) map[string]any {
	return map[string]any{
		"session_id": p.SessionID, "act_id": p.ActID, "goal": p.Goal,
		"prep_notes": p.PrepNotes, "status": p.Status,
		"created_at": p.CreatedAt.Format(http.TimeFormat),
		"updated_at": p.UpdatedAt.Format(http.TimeFormat),
	}
}

// writeStoryError maps the store vocabulary onto HTTP statuses (the spine
// reuses the campaign sentinels).
func writeStoryError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, campaign.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, campaign.ErrInvalid), errors.Is(err, campaign.ErrScope):
		writeError(w, http.StatusBadRequest, err)
	default:
		writeError(w, http.StatusInternalServerError, err)
	}
}

/* ---------- acts ---------- */

func (s *Server) handleListActs(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	if !s.storyEnabled(w) {
		return
	}
	acts, err := s.stories.ListActs(r.Context(), campaign.ScopeDM, a.campaign.ID)
	if err != nil {
		writeStoryError(w, err)
		return
	}
	out := make([]map[string]any, 0, len(acts))
	for _, act := range acts {
		out = append(out, toActView(act))
	}
	writeJSON(w, http.StatusOK, map[string]any{"acts": out})
}

func (s *Server) handleCreateAct(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	if !s.storyEnabled(w) {
		return
	}
	var req struct {
		Name       string `json:"name"`
		Premise    string `json:"premise"`
		LevelStart int    `json:"level_start"`
		LevelEnd   int    `json:"level_end"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	act, err := s.stories.CreateAct(r.Context(), a.campaign.ID, req.Name, req.Premise, req.LevelStart, req.LevelEnd)
	if err != nil {
		writeStoryError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"act": toActView(*act)})
}

func (s *Server) handleGetAct(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	if !s.storyEnabled(w) {
		return
	}
	act, err := s.stories.GetAct(r.Context(), campaign.ScopeDM, a.campaign.ID, r.PathValue("aid"))
	if err != nil {
		writeStoryError(w, err)
		return
	}
	scenes, err := s.stories.ListScenes(r.Context(), campaign.ScopeDM, a.campaign.ID, act.ID)
	if err != nil {
		writeStoryError(w, err)
		return
	}
	out := make([]map[string]any, 0, len(scenes))
	for _, sc := range scenes {
		out = append(out, toSceneView(sc))
	}
	writeJSON(w, http.StatusOK, map[string]any{"act": toActView(*act), "scenes": out})
}

func (s *Server) handleUpdateAct(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	if !s.storyEnabled(w) {
		return
	}
	var req struct {
		Name       *string `json:"name"`
		Premise    *string `json:"premise"`
		LevelStart *int    `json:"level_start"`
		LevelEnd   *int    `json:"level_end"`
		Status     *string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	act, err := s.stories.UpdateAct(r.Context(), a.campaign.ID, r.PathValue("aid"),
		req.Name, req.Premise, req.LevelStart, req.LevelEnd, req.Status)
	if err != nil {
		writeStoryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"act": toActView(*act)})
}

func (s *Server) handleDeleteAct(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	if !s.storyEnabled(w) {
		return
	}
	if err := s.stories.DeleteAct(r.Context(), a.campaign.ID, r.PathValue("aid")); err != nil {
		writeStoryError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

/* ---------- scenes ---------- */

func (s *Server) handleListScenes(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	if !s.storyEnabled(w) {
		return
	}
	scenes, err := s.stories.ListScenes(r.Context(), campaign.ScopeDM, a.campaign.ID, r.URL.Query().Get("act"))
	if err != nil {
		writeStoryError(w, err)
		return
	}
	out := make([]map[string]any, 0, len(scenes))
	for _, sc := range scenes {
		out = append(out, toSceneView(sc))
	}
	writeJSON(w, http.StatusOK, map[string]any{"scenes": out})
}

func (s *Server) handleCreateScene(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	if !s.storyEnabled(w) {
		return
	}
	var req struct {
		ActID         string `json:"act_id"`
		SessionID     string `json:"session_id"`
		Kind          string `json:"kind"`
		Name          string `json:"name"`
		Purpose       string `json:"purpose"`
		SettingEntity string `json:"setting_entity"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	sc, err := s.stories.CreateScene(r.Context(), a.campaign.ID, req.ActID, req.SessionID,
		req.Kind, req.Name, req.Purpose, req.SettingEntity)
	if err != nil {
		writeStoryError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"scene": toSceneView(*sc)})
}

func (s *Server) handleGetScene(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	if !s.storyEnabled(w) {
		return
	}
	sc, err := s.stories.GetScene(r.Context(), campaign.ScopeDM, a.campaign.ID, r.PathValue("sid"))
	if err != nil {
		writeStoryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"scene": toSceneView(*sc)})
}

func (s *Server) handleUpdateScene(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	if !s.storyEnabled(w) {
		return
	}
	var req struct {
		SessionID     *string `json:"session_id"`
		Kind          *string `json:"kind"`
		Name          *string `json:"name"`
		Purpose       *string `json:"purpose"`
		SettingEntity *string `json:"setting_entity"`
		Status        *string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	sc, err := s.stories.UpdateScene(r.Context(), a.campaign.ID, r.PathValue("sid"),
		req.SessionID, req.Kind, req.Name, req.Purpose, req.SettingEntity, req.Status)
	if err != nil {
		writeStoryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"scene": toSceneView(*sc)})
}

func (s *Server) handleDeleteScene(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	if !s.storyEnabled(w) {
		return
	}
	if err := s.stories.DeleteScene(r.Context(), a.campaign.ID, r.PathValue("sid")); err != nil {
		writeStoryError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

/* ---------- cast, secrets, outcomes ---------- */

func (s *Server) handleAddSceneCast(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	if !s.storyEnabled(w) {
		return
	}
	var req struct {
		EntityID string `json:"entity_id"`
		Role     string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	cast, err := s.stories.AddCast(r.Context(), a.campaign.ID, r.PathValue("sid"), req.EntityID, req.Role)
	if err != nil {
		writeStoryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cast": cast})
}

func (s *Server) handleRemoveSceneCast(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	if !s.storyEnabled(w) {
		return
	}
	if err := s.stories.RemoveCast(r.Context(), a.campaign.ID, r.PathValue("sid"), r.PathValue("eid")); err != nil {
		writeStoryError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAddSceneSecret(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	if !s.storyEnabled(w) {
		return
	}
	var req struct {
		FactID      string `json:"fact_id"`
		Disposition string `json:"disposition"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	secrets, err := s.stories.SetSecret(r.Context(), a.campaign.ID, r.PathValue("sid"), req.FactID, req.Disposition)
	if err != nil {
		writeStoryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"secrets": secrets})
}

func (s *Server) handleRemoveSceneSecret(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	if !s.storyEnabled(w) {
		return
	}
	if err := s.stories.RemoveSecret(r.Context(), a.campaign.ID, r.PathValue("sid"), r.PathValue("fid")); err != nil {
		writeStoryError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAddSceneOutcome(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	if !s.storyEnabled(w) {
		return
	}
	var req struct {
		Label           string                 `json:"label"`
		Summary         string                 `json:"summary"`
		LeadsToScene    string                 `json:"leads_to_scene"`
		QuestTransition *story.QuestTransition `json:"quest_transition"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	outcomes, err := s.stories.AddOutcome(r.Context(), a.campaign.ID, r.PathValue("sid"),
		req.Label, req.Summary, req.LeadsToScene, req.QuestTransition)
	if err != nil {
		writeStoryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"outcomes": outcomes})
}

func (s *Server) handleRemoveSceneOutcome(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	if !s.storyEnabled(w) {
		return
	}
	if err := s.stories.RemoveOutcome(r.Context(), a.campaign.ID, r.PathValue("sid"), r.PathValue("label")); err != nil {
		writeStoryError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

/* ---------- session plans ---------- */

func (s *Server) handleGetSessionPlan(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("cid"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	if !s.storyEnabled(w) {
		return
	}
	if _, ok := s.sessionInCampaign(w, r, r.PathValue("cid"), r.PathValue("sid")); !ok {
		return
	}
	p, err := s.stories.GetPlan(r.Context(), campaign.ScopeDM, r.PathValue("cid"), r.PathValue("sid"))
	if err != nil {
		writeStoryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"plan": toPlanView(*p)})
}

func (s *Server) handlePutSessionPlan(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("cid"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	if !s.storyEnabled(w) {
		return
	}
	if _, ok := s.sessionInCampaign(w, r, r.PathValue("cid"), r.PathValue("sid")); !ok {
		return
	}
	var req struct {
		ActID     string  `json:"act_id"`
		Goal      string  `json:"goal"`
		PrepNotes string  `json:"prep_notes"`
		Status    *string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	p, err := s.stories.PutPlan(r.Context(), r.PathValue("cid"), r.PathValue("sid"),
		req.ActID, req.Goal, req.PrepNotes, req.Status)
	if err != nil {
		writeStoryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"plan": toPlanView(*p)})
}

func (s *Server) handleDeleteSessionPlan(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("cid"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	if !s.storyEnabled(w) {
		return
	}
	if _, ok := s.sessionInCampaign(w, r, r.PathValue("cid"), r.PathValue("sid")); !ok {
		return
	}
	if err := s.stories.DeletePlan(r.Context(), r.PathValue("cid"), r.PathValue("sid")); err != nil {
		writeStoryError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

/* ---------- the whole spine, and the deterministic helpers ---------- */

// handleStory loads the whole spine in one payload — acts with their scenes
// (cast, secrets, outcomes attached) and the session plans — the planner
// view's single read. DM-only: this is the whole plan, secrets included.
func (s *Server) handleStory(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	if !s.storyEnabled(w) {
		return
	}
	ctx := r.Context()
	sp, err := story.LoadSpine(ctx, s.stories.DB(), a.campaign.ID)
	if err != nil {
		writeStoryError(w, err)
		return
	}
	scenesByAct := map[string][]map[string]any{}
	for i := range sp.Scenes {
		sc := &sp.Scenes[i]
		scenesByAct[sc.ActID] = append(scenesByAct[sc.ActID], toSceneView(*sc))
	}
	acts := make([]map[string]any, 0, len(sp.Acts))
	for _, act := range sp.Acts {
		v := toActView(act)
		sc, _ := scenesByAct[act.ID]
		if sc == nil {
			sc = []map[string]any{}
		}
		v["scenes"] = sc
		acts = append(acts, v)
	}
	plans := make([]map[string]any, 0, len(sp.Plans))
	for _, p := range sp.Plans {
		plans = append(plans, toPlanView(p))
	}
	writeJSON(w, http.StatusOK, map[string]any{"acts": acts, "plans": plans})
}

// handleStoryShapes lists the legal act structures and what each act is
// for. Pure math over no data — nothing to scope.
func (s *Server) handleStoryShapes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"shapes": story.Shapes()})
}

// handleStoryPace prices a level band across acts off the encounter
// package's XP tables. Pure math; the parameters come from the query string.
func (s *Server) handleStoryPace(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from := parsePaceLevel(q.Get("from"), 1)
	to := parsePaceLevel(q.Get("to"), from)
	acts := 4
	if v := q.Get("acts"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 12 {
			acts = n
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"pace": story.Pace(from, to, acts)})
}

// parsePaceLevel reads one level, clamped to the 1-20 the game has.
func parsePaceLevel(v string, def int) int {
	if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 20 {
		return n
	}
	return def
}
