package server

// The DM screen's live context (MAD-485): one DM-only read, and the leak
// discipline stated as a test — the payload is new, so its absence of
// secrets is asserted on the raw body, and a player learns nothing from
// the route at all.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// buildLiveStage seats the fixture's campaign mid-play: one active scene
// with the duke on stage and the secret fact in play, one planned scene
// beside it, and a session that has gone live.
func buildLiveStage(t *testing.T, s *Server, f fixture) (activeID, plannedID, sessionID string) {
	t.Helper()
	dm := dmSession(t, s)
	base := "/api/campaigns/" + f.campaignID

	var actOut struct {
		Act struct {
			ID string `json:"id"`
		} `json:"act"`
	}
	r := hit(t, s, http.MethodPost, base+"/acts", `{"name":"The Letter","level_start":1,"level_end":3}`, dm)
	if r.Code != http.StatusCreated {
		t.Fatalf("create act: status %d, body %s", r.Code, r.Body)
	}
	if err := json.Unmarshal(r.Body.Bytes(), &actOut); err != nil {
		t.Fatalf("decode act: %v", err)
	}

	mkScene := func(name string) string {
		body := `{"act_id":` + quote(actOut.Act.ID) + `,"kind":"social","name":` + quote(name) + `,"purpose":"Put the question on the table."}`
		r := hit(t, s, http.MethodPost, base+"/scenes", body, dm)
		if r.Code != http.StatusCreated {
			t.Fatalf("create scene %s: status %d, body %s", name, r.Code, r.Body)
		}
		var out struct {
			Scene struct {
				ID string `json:"id"`
			} `json:"scene"`
		}
		if err := json.Unmarshal(r.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode scene: %v", err)
		}
		return out.Scene.ID
	}
	activeID = mkScene("The Waystone at midnight")
	plannedID = mkScene("The auction at dawn")

	for _, patch := range []string{
		`{"entity_id":` + quote(f.dukeID) + `,"role":"focus"}`,
	} {
		if r := hit(t, s, http.MethodPost, base+"/scenes/"+activeID+"/cast", patch, dm); r.Code != http.StatusOK {
			t.Fatalf("add cast: status %d, body %s", r.Code, r.Body)
		}
	}
	if r := hit(t, s, http.MethodPost, base+"/scenes/"+activeID+"/secrets",
		`{"fact_id":`+quote(f.secretID)+`,"disposition":"in_play"}`, dm); r.Code != http.StatusOK {
		t.Fatalf("set secret: status %d, body %s", r.Code, r.Body)
	}
	if r := hit(t, s, http.MethodPatch, base+"/scenes/"+activeID, `{"status":"active"}`, dm); r.Code != http.StatusOK {
		t.Fatalf("activate scene: status %d, body %s", r.Code, r.Body)
	}

	r = hit(t, s, http.MethodPost, base+"/sessions", `{"name":"Session 1"}`, dm)
	if r.Code != http.StatusCreated {
		t.Fatalf("create session: status %d, body %s", r.Code, r.Body)
	}
	var sesOut struct {
		Session struct {
			ID string `json:"id"`
		} `json:"session"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &sesOut); err != nil {
		t.Fatalf("decode session: %v", err)
	}
	sessionID = sesOut.Session.ID
	if r := hit(t, s, http.MethodPatch, base+"/sessions/"+sessionID, `{"status":"live"}`, dm); r.Code != http.StatusOK {
		t.Fatalf("go live: status %d, body %s", r.Code, r.Body)
	}
	return activeID, plannedID, sessionID
}

// THE ACCEPTANCE TEST: the live read carries the live session with its
// started_at, the active scenes with cast — and no secrets, by
// construction: the payload has no field one could ride in, asserted on
// the raw body of a scene that has a secret attached.
func TestLiveContextServesSceneAndClock(t *testing.T) {
	s := newStoryServer(t)
	f := buildFixture(t, s)
	activeID, plannedID, sessionID := buildLiveStage(t, s, f)
	dm := dmSession(t, s)

	r := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/live", "", dm)
	if r.Code != http.StatusOK {
		t.Fatalf("live read: status %d, body %s", r.Code, r.Body)
	}
	var out struct {
		Live struct {
			Session *struct {
				ID        string `json:"id"`
				Name      string `json:"name"`
				Status    string `json:"status"`
				StartedAt string `json:"started_at"`
			} `json:"session"`
			Scenes []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
				Kind string `json:"kind"`
				Cast []struct {
					EntityID string `json:"entity_id"`
					Role     string `json:"role"`
				} `json:"cast"`
			} `json:"scenes"`
		} `json:"live"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode live: %v", err)
	}
	if out.Live.Session == nil || out.Live.Session.ID != sessionID {
		t.Fatalf("the live session did not arrive: %+v", out.Live.Session)
	}
	if out.Live.Session.Status != "live" || out.Live.Session.StartedAt == "" {
		t.Errorf("the clock's raw material is incomplete: %+v", out.Live.Session)
	}
	if len(out.Live.Scenes) != 1 || out.Live.Scenes[0].ID != activeID {
		t.Fatalf("only the active scene belongs: %+v", out.Live.Scenes)
	}
	sc := out.Live.Scenes[0]
	if len(sc.Cast) != 1 || sc.Cast[0].EntityID != f.dukeID || sc.Cast[0].Role != "focus" {
		t.Errorf("the cast did not arrive: %+v", sc.Cast)
	}

	body := r.Body.String()
	for _, leak := range []string{"secrets", "vampire", f.secretID, plannedID} {
		if strings.Contains(body, leak) {
			t.Errorf("the live payload carries %q — the screen's card never needs it:\n%s", leak, body)
		}
	}
}

// Before anyone goes live the session is null and the active scenes still
// arrive: a scene can run mid-session without being seated.
func TestLiveContextWithoutALiveSession(t *testing.T) {
	s := newStoryServer(t)
	f := buildFixture(t, s)
	activeID, _, _ := buildLiveStage(t, s, f)
	dm := dmSession(t, s)

	// Close the session; the read must not pretend a clock is running.
	if r := hit(t, s, http.MethodPatch, "/api/campaigns/"+f.campaignID+"/sessions/"+
		extractSessionID(t, s, f), `{"status":"done"}`, dm); r.Code != http.StatusOK {
		t.Fatalf("end session: status %d, body %s", r.Code, r.Body)
	}
	r := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/live", "", dm)
	if r.Code != http.StatusOK {
		t.Fatalf("live read: status %d, body %s", r.Code, r.Body)
	}
	var out struct {
		Live struct {
			Session any `json:"session"`
			Scenes  []struct {
				ID string `json:"id"`
			} `json:"scenes"`
		} `json:"live"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode live: %v", err)
	}
	if out.Live.Session != nil {
		t.Errorf("a done session is not a live one: %v", out.Live.Session)
	}
	if len(out.Live.Scenes) != 1 || out.Live.Scenes[0].ID != activeID {
		t.Errorf("the active scene left with the session: %+v", out.Live.Scenes)
	}
}

func extractSessionID(t *testing.T, s *Server, f fixture) string {
	t.Helper()
	dm := dmSession(t, s)
	r := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/sessions", "", dm)
	var list struct {
		Sessions []struct {
			ID string `json:"id"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &list); err != nil || len(list.Sessions) == 0 {
		t.Fatalf("list sessions: %v (%s)", err, r.Body)
	}
	return list.Sessions[0].ID
}

// THE SCOPE TESTS: the screen is the DM's surface — a player member is
// refused outright, and a stranger learns the campaign does not exist.
func TestLiveContextIsDMOnly(t *testing.T) {
	s := newStoryServer(t)
	f := buildFixture(t, s)
	buildLiveStage(t, s, f)

	player := addPlayerMember(t, s, f, "mira", true)
	r := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/live", "", player)
	if r.Code != http.StatusForbidden {
		t.Fatalf("player read: status %d, body %s", r.Code, r.Body)
	}
	if strings.Contains(r.Body.String(), "Waystone") {
		t.Error("the refusal carried scene material")
	}

	stranger := registerOutsider(t, s, "live-stranger")
	r = hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/live", "", stranger)
	if r.Code != http.StatusNotFound {
		t.Fatalf("stranger read: status %d, body %s", r.Code, r.Body)
	}
}
