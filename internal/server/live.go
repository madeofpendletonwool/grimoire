package server

// The DM screen's live context (MAD-485, stage 2 of MAD-318): one read that
// answers "where are we?" — the live session the clock runs on, and the
// campaign's active scenes with their cast. The screen strip composes this
// beside the tracker and the board rather than duplicating either.
//
//	GET /api/campaigns/{id}/live
//
// DM-only, like every spine read: the screen is the DM's surface and the
// scene card is planning material. The payloads are explicit structs
// because they are new: a scene view with no secrets field cannot leak
// one, and the test asserts the absence on the raw body.
//
// The board snapshot stays what it was — party vitals over realtime push.
// Scenes change at planning speed, not combat speed, so they ride this
// quiet read (the screen re-polls it on a slow beat) rather than every
// board frame.

import (
	"net/http"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/gamesession"
	"github.com/madeofpendletonwool/grimoire/internal/story"
)

// liveSessionView is the clock's raw material: the session's own identity
// and the moment it went live. The ticking display is client-side.
type liveSessionView struct {
	ID        string `json:"id"`
	Ordinal   int64  `json:"ordinal"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	StartedAt string `json:"started_at,omitempty"`
}

// liveSceneView is the scene card: kind, name, purpose, setting, cast —
// and nothing else. Secrets and outcomes stay in the spine's own reads.
type liveSceneView struct {
	ID            string             `json:"id"`
	ActID         string             `json:"act_id"`
	SessionID     string             `json:"session_id,omitempty"`
	Ordinal       int64              `json:"ordinal"`
	Kind          string             `json:"kind"`
	Name          string             `json:"name"`
	Purpose       string             `json:"purpose"`
	SettingEntity string             `json:"setting_entity,omitempty"`
	Status        string             `json:"status"`
	Cast          []story.CastMember `json:"cast"`
}

// handleCampaignLive serves the live context. A campaign with no session
// live answers session: null — the screen offers to seat one; active
// scenes are returned either way, because a scene can run mid-session
// without the planner having seated it.
func (s *Server) handleCampaignLive(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	if !s.storyEnabled(w) || !s.sessionsEnabled(w) {
		return
	}

	var session *liveSessionView
	list, err := s.sessions.ListSessions(r.Context(), a.campaign.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	for i := range list {
		if list[i].Status != gamesession.StatusLive {
			continue
		}
		ses := list[i]
		v := liveSessionView{ID: ses.ID, Ordinal: ses.Ordinal, Name: ses.Name, Status: ses.Status}
		if !ses.StartedAt.IsZero() {
			v.StartedAt = ses.StartedAt.Format(time.RFC3339)
		}
		session = &v
		break
	}

	scenes, err := s.stories.ActiveScenes(r.Context(), campaign.ScopeDM, a.campaign.ID)
	if err != nil {
		writeStoryError(w, err)
		return
	}
	out := make([]liveSceneView, 0, len(scenes))
	for _, sc := range scenes {
		out = append(out, liveSceneView{
			ID: sc.ID, ActID: sc.ActID, SessionID: sc.SessionID, Ordinal: sc.Ordinal,
			Kind: sc.Kind, Name: sc.Name, Purpose: sc.Purpose,
			SettingEntity: sc.SettingEntity, Status: sc.Status, Cast: sc.Cast,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"live": map[string]any{
		"session": session,
		"scenes":  out,
	}})
}
