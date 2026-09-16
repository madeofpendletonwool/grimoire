package server

// The judge and spectator gate over HTTP (MAD-338): the observer joins
// a live game through the same code a seat is, reads the public game
// through the same SQL WHERE clause, asks the rules judge as the table
// itself, and records rulings anchored to ordinals. The hidden-zone
// gate is 6a's, applied unchanged: every surface an observer can reach
// is swept by the hidden_zone_leak check, and the write side answers
// 403 for everything an observer is not.
//
//   - TestObserverJoinsLiveGameByCode: the front door stays open while
//     play runs; the role rides the join and the viewer block.
//   - TestJudgeSpectatorSurfacesLeakNothing: every read surface and the
//     judge's ask prompt, scanned at the public scope. Zero findings.
//   - TestJudgeRulingFlow: the acceptance — a judge joins a live game,
//     inspects public state, records a ruling anchored to an ordinal,
//     and the ruling survives the rewind past it.
//   - TestJudgeWritesAreRefused: the observer's pen writes rulings and
//     nothing else.
//   - TestJudgeAskAsTheTable: seat 0 is the judge's chair to ask from.
//   - TestGameStreamCarriesRulings: a recorded ruling lands on every
//     attached stream as its own frame.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/table/engine"
)

/* ---------- the fixture ---------- */

// joinObserver registers a named account and redeems the pod's code for
// an observer role on the live game — the dispute arrival.
func joinObserver(t *testing.T, f *podFixture, name, role string) *http.Cookie {
	t.Helper()
	cookie, _ := podFriend(t, f.s, f.owner, name)
	code := observerCode(t, f)
	body, _ := json.Marshal(map[string]any{"code": code, "role": role})
	rec := hit(t, f.s, http.MethodPost, "/api/games/join", string(body), cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s join as %s: %d %s", name, role, rec.Code, rec.Body)
	}
	var joined struct {
		Game struct {
			ID string `json:"id"`
		} `json:"game"`
		Seat int    `json:"seat"`
		Role string `json:"role"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &joined)
	if joined.Game.ID != f.game || joined.Seat != 0 || joined.Role != role {
		t.Fatalf("%s join → game %s seat %d role %q", name, joined.Game.ID, joined.Seat, joined.Role)
	}
	return cookie
}

// observerCode reads the join code the owner's copy carries.
func observerCode(t *testing.T, f *podFixture) string {
	t.Helper()
	rec := hit(t, f.s, http.MethodGet, "/api/games/"+f.game, "", f.owner)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner read: %d %s", rec.Code, rec.Body)
	}
	var view struct {
		Game struct {
			JoinCode string `json:"join_code"`
		} `json:"game"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &view)
	if view.Game.JoinCode == "" {
		t.Fatal("owner view carries no join code")
	}
	return view.Game.JoinCode
}

/* ---------- the front door ---------- */

// TestObserverJoinsLiveGameByCode: seats close when play begins;
// observers do not. The role rides the join answer and the get-game
// viewer block, the game joins the observer's list, and the join code
// itself stays the host's to share.
func TestObserverJoinsLiveGameByCode(t *testing.T) {
	f := buildPod(t)
	judge := joinObserver(t, f, "podmercer", "judge")

	// The game is in the judge's list.
	rec := hit(t, f.s, http.MethodGet, "/api/games", "", judge)
	if !strings.Contains(rec.Body.String(), f.game) {
		t.Fatalf("observed game absent from judge's list: %s", rec.Body)
	}
	// The viewer block names the role; the state is the public fold.
	rec = hit(t, f.s, http.MethodGet, "/api/games/"+f.game, "", judge)
	if rec.Code != http.StatusOK {
		t.Fatalf("judge read: %d %s", rec.Code, rec.Body)
	}
	var view struct {
		Viewer struct {
			Owner bool   `json:"owner"`
			Role  string `json:"role"`
		} `json:"viewer"`
		Game struct {
			JoinCode string `json:"join_code"`
		} `json:"game"`
		Observers []engine.Observer `json:"observers"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &view)
	if view.Viewer.Owner || view.Viewer.Role != "judge" {
		t.Fatalf("judge viewer block = %+v", view.Viewer)
	}
	if view.Game.JoinCode != "" {
		t.Fatal("the observer's copy carries the join code")
	}
	if len(view.Observers) != 1 || view.Observers[0].Role != "judge" {
		t.Fatalf("observers roster = %+v", view.Observers)
	}
	// Redeeming again is the same role, not a second row.
	code := observerCode(t, f)
	rec = hit(t, f.s, http.MethodPost, "/api/games/join",
		fmt.Sprintf(`{"code":%q,"role":"judge"}`, code), judge)
	if rec.Code != http.StatusOK {
		t.Fatalf("re-observe: %d %s", rec.Code, rec.Body)
	}
	// A stranger who never joined still sees nothing.
	stranger := gameFriend(t, f.s, f.owner)
	rec = hit(t, f.s, http.MethodGet, "/api/games/"+f.game, "", stranger)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("stranger read the game: %d", rec.Code)
	}
	// A seated player cannot become an observer.
	rec = hit(t, f.s, http.MethodPost, "/api/games/join",
		fmt.Sprintf(`{"code":%q,"role":"judge"}`, code), f.seats[2])
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("seated player observed: %d %s", rec.Code, rec.Body)
	}
}

/* ---------- the leak gate ---------- */

// TestJudgeSpectatorSurfacesLeakNothing: every read surface a judge or
// spectator can reach, plus the judge's ask prompt, scanned by the
// hidden_zone_leak check at the public scope. The 6a gate, applied to
// the 6b viewer unchanged.
func TestJudgeSpectatorSurfacesLeakNothing(t *testing.T) {
	f := buildPod(t)
	judge := joinObserver(t, f, "podmercer", "judge")
	spec := joinObserver(t, f, "podwatch", "spectator")
	x := f.leakIndex(t)

	for _, who := range []struct{ label string; cookie *http.Cookie }{
		{"judge", judge}, {"spectator", spec},
	} {
		get := func(path string) string {
			rec := hit(t, f.s, http.MethodGet, path, "", who.cookie)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s GET %s: %d %s", who.label, path, rec.Code, rec.Body)
			}
			return rec.Body.String()
		}
		surfaces := map[string]string{
			"game":      get("/api/games/" + f.game),
			"events":    get("/api/games/" + f.game + "/events?after=0"),
			"pending":   get("/api/games/" + f.game + "/pending"),
			"nudges":    get("/api/games/" + f.game + "/nudges"),
			"turns":     get("/api/games/" + f.game + "/turns/1"),
			"rulings":   get("/api/games/" + f.game + "/rulings"),
			"game-list": get("/api/games"),
		}
		scanned := x.Scanned()
		for name, body := range surfaces {
			findings := x.Scan(engine.PublicViewer(), who.label, body)
			if len(findings) > 0 {
				t.Fatalf("%s leaked on %s: %+v", who.label, name, findings)
			}
			assertNoNoteMarkers(t, 0, name, body, f)
		}
		if x.Scanned() == scanned {
			t.Fatalf("%s sweep was vacuous — no string leaf visited", who.label)
		}
		// The observer folds the public game: same life, same hand
		// counts, same battlefield the seats hold.
		var view struct {
			State *engine.State `json:"state"`
		}
		_ = json.Unmarshal([]byte(surfaces["game"]), &view)
		if view.State == nil || len(view.State.Seats) != 4 {
			t.Fatalf("%s folded %v seats", who.label, view.State)
		}
		// The rules judge's prompt, asked as the table (seat 0): the
		// captured request body is a rendered surface like any other.
		before := len(f.rec.all())
		rec := hit(t, f.s, http.MethodPost, "/api/games/"+f.game+"/ask",
			`{"seat":0,"question":"what resolves next?"}`, who.cookie)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s ask: %d %s", who.label, rec.Code, rec.Body)
		}
		for _, body := range f.rec.all()[before:] {
			findings := x.Scan(engine.PublicViewer(), who.label, body)
			if len(findings) > 0 {
				t.Fatalf("%s's ask prompt leaked: %+v", who.label, findings)
			}
			assertNoNoteMarkers(t, 0, "ask-prompt", body, f)
		}
	}
	// The gate has teeth at the public scope too: the owner's full
	// payload scanned there fires.
	rec := hit(t, f.s, http.MethodGet, "/api/games/"+f.game, "", f.owner)
	if findings := x.Scan(engine.PublicViewer(), "judge", rec.Body.String()); len(findings) == 0 {
		t.Fatal("the owner's full payload scanned at the public scope did not fire — the observer gate has no teeth")
	}
}

/* ---------- the ruling log ---------- */

// TestJudgeRulingFlow is the acceptance: a judge joins a live game,
// inspects public state and traces, and records a ruling anchored to an
// ordinal — which survives the rewind past it, because a human record
// is never clobbered by a truncate.
func TestJudgeRulingFlow(t *testing.T) {
	f := buildPod(t)
	judge := joinObserver(t, f, "podmercer", "judge")
	spec := joinObserver(t, f, "podwatch", "spectator")

	// Public inspection: the trace surfaces answer the judge.
	_, latest := gameEvents(t, f.s, f.owner, f.game)
	if latest < 1 {
		t.Fatal("no log to anchor to")
	}
	var firstObj int64
	st := gameState(t, f.s, f.owner, f.game)
	for id := range st.Objects {
		if firstObj == 0 || id < firstObj {
			firstObj = id
		}
	}
	if firstObj > 0 {
		rec := hit(t, f.s, http.MethodGet,
			fmt.Sprintf("/api/games/%s/objects/%d/trace", f.game, firstObj), "", judge)
		if rec.Code != http.StatusOK {
			t.Fatalf("judge trace: %d %s", rec.Code, rec.Body)
		}
	}
	rec := hit(t, f.s, http.MethodGet, "/api/games/"+f.game+"/turns/1", "", judge)
	if rec.Code != http.StatusOK {
		t.Fatalf("judge turn slice: %d %s", rec.Code, rec.Body)
	}

	// The ruling: anchored to the head ordinal, recorded by the judge.
	note := "the Rhystic trigger was announced — it resolves before the draw"
	rec = hit(t, f.s, http.MethodPost, "/api/games/"+f.game+"/rulings",
		fmt.Sprintf(`{"ord":%d,"note":%q}`, latest, note), judge)
	if rec.Code != http.StatusCreated {
		t.Fatalf("judge ruling: %d %s", rec.Code, rec.Body)
	}
	var posted struct {
		Ruling engine.Ruling `json:"ruling"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &posted)
	if posted.Ruling.Ord != latest || posted.Ruling.Note != note {
		t.Fatalf("ruling = %+v", posted.Ruling)
	}
	if posted.Ruling.Ruler != "podmercer" {
		t.Fatalf("ruler = %q", posted.Ruling.Ruler)
	}
	// Everyone entitled reads the log — owner, a seat, the spectator.
	for _, cookie := range []*http.Cookie{f.owner, f.seats[2], spec} {
		rec := hit(t, f.s, http.MethodGet, "/api/games/"+f.game+"/rulings", "", cookie)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), note) {
			t.Fatalf("ruling read: %d %s", rec.Code, rec.Body)
		}
	}
	// Anchors must exist.
	rec = hit(t, f.s, http.MethodPost, "/api/games/"+f.game+"/rulings",
		fmt.Sprintf(`{"ord":%d,"note":"x"}`, latest+50), judge)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("past-head anchor: %d %s", rec.Code, rec.Body)
	}
	// The host may rule too — a solo table's owner is its judge.
	rec = hit(t, f.s, http.MethodPost, "/api/games/"+f.game+"/rulings",
		fmt.Sprintf(`{"ord":%d,"note":"clean up the stack"}`, latest), f.owner)
	if rec.Code != http.StatusCreated {
		t.Fatalf("host ruling: %d %s", rec.Code, rec.Body)
	}
	// The rewind past the anchor leaves the ruling standing.
	if rec := hit(t, f.s, http.MethodPost, "/api/games/"+f.game+"/rewind",
		fmt.Sprintf(`{"to":%d}`, latest-1), f.owner); rec.Code != http.StatusOK {
		t.Fatalf("rewind: %d %s", rec.Code, rec.Body)
	}
	rec = hit(t, f.s, http.MethodGet, "/api/games/"+f.game+"/rulings", "", spec)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), note) {
		t.Fatalf("ruling did not survive the rewind: %s", rec.Body)
	}
}

// TestJudgeWritesAreRefused: the observer's pen writes rulings and
// nothing else — no actions, no host controls, no zone reads, no pads.
func TestJudgeWritesAreRefused(t *testing.T) {
	f := buildPod(t)
	judge := joinObserver(t, f, "podmercer", "judge")
	spec := joinObserver(t, f, "podwatch", "spectator")

	// A judge cannot act as any seat — they hold none, seat 0 included.
	for _, seat := range []int{0, 1, 2} {
		r := hit(t, f.s, http.MethodPost, "/api/games/"+f.game+"/actions",
			fmt.Sprintf(`{"kind":"CHANGE_LIFE","seat":%d,"target_seat":2,"delta":-3}`, seat), judge)
		if r.Code != http.StatusForbidden {
			t.Fatalf("judge acted as seat %d: %d %s", seat, r.Code, r.Body)
		}
	}
	// Host controls stay the host's.
	for _, path := range []string{"/rewind", "/amend", "/start", "/seats", "/settings"} {
		rec := hit(t, f.s, http.MethodPost, "/api/games/"+f.game+path, `{}`, judge)
		if rec.Code == http.StatusOK {
			t.Fatalf("judge reached host control %s", path)
		}
	}
	// Zone reads another seat owns: the library, the odds over it, the
	// pads. 403, not silent.
	rec := hit(t, f.s, http.MethodGet, "/api/games/"+f.game+"/library?seat=2", "", judge)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("judge read seat 2's library: %d %s", rec.Code, rec.Body)
	}
	rec = hit(t, f.s, http.MethodPost, "/api/games/"+f.game+"/odds",
		`{"seat":2,"card":"Forest","draws":1}`, judge)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("judge asked odds for seat 2: %d %s", rec.Code, rec.Body)
	}
	rec = hit(t, f.s, http.MethodGet, "/api/games/"+f.game+"/notes", "", judge)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("judge read a pad: %d %s", rec.Code, rec.Body)
	}
	// The spectator's pen is read-only outright.
	rec = hit(t, f.s, http.MethodPost, "/api/games/"+f.game+"/rulings",
		`{"ord":1,"note":"I saw it differently"}`, spec)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("spectator ruled: %d %s", rec.Code, rec.Body)
	}
	// A seated player asks the judge rather than ruling on their own game.
	rec = hit(t, f.s, http.MethodPost, "/api/games/"+f.game+"/rulings",
		`{"ord":1,"note":"self-serving"}`, f.seats[2])
	if rec.Code != http.StatusForbidden {
		t.Fatalf("seated player ruled: %d %s", rec.Code, rec.Body)
	}
}

/* ---------- the ask ---------- */

// TestJudgeAskAsTheTable: seat 0 is the observer's chair — the ask
// renders the public fold. A named seat is not theirs.
func TestJudgeAskAsTheTable(t *testing.T) {
	f := buildPod(t)
	judge := joinObserver(t, f, "podmercer", "judge")

	rec := hit(t, f.s, http.MethodPost, "/api/games/"+f.game+"/ask",
		`{"seat":2,"question":"what resolves next?"}`, judge)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("judge asked as seat 2: %d %s", rec.Code, rec.Body)
	}
	rec = hit(t, f.s, http.MethodPost, "/api/games/"+f.game+"/ask",
		`{"seat":0,"question":"what resolves next?"}`, judge)
	if rec.Code != http.StatusOK {
		t.Fatalf("judge asked as the table: %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "event: done") {
		t.Fatalf("the table's ask never answered:\n%s", rec.Body)
	}
	// A seated player still cannot borrow seat 0 — their chair is
	// their own, and the guard is unchanged for them.
	rec = hit(t, f.s, http.MethodPost, "/api/games/"+f.game+"/ask",
		`{"seat":0,"question":"what resolves next?"}`, f.seats[2])
	if rec.Code != http.StatusForbidden {
		t.Fatalf("seated player asked as the table: %d %s", rec.Code, rec.Body)
	}
}

/* ---------- the stream ---------- */

// TestGameStreamCarriesRulings: a recorded ruling lands on every
// attached stream as its own frame — the table sees the judge's pen
// move live.
func TestGameStreamCarriesRulings(t *testing.T) {
	f := buildPod(t)
	judge := joinObserver(t, f, "podmercer", "judge")

	rec, end, done := openGameStream(t, f.s, f.seats[3], "/api/games/"+f.game+"/stream")
	time.Sleep(150 * time.Millisecond)
	note := "state-based actions were checked — the token is gone"
	_, latest := gameEvents(t, f.s, f.owner, f.game)
	if r := hit(t, f.s, http.MethodPost, "/api/games/"+f.game+"/rulings",
		fmt.Sprintf(`{"ord":%d,"note":%q}`, latest, note), judge); r.Code != http.StatusCreated {
		t.Fatalf("ruling: %d %s", r.Code, r.Body)
	}
	time.Sleep(600 * time.Millisecond)
	end()
	cancelAndWait(t, done)

	body := rec.Body.String()
	if !strings.Contains(body, "event: ruling") {
		t.Fatalf("stream never carried the ruling:\n%s", body)
	}
	saw := false
	for _, frame := range sseEvents(t, body) {
		if frame.Event != "ruling" {
			continue
		}
		var wrap struct {
			Ruling engine.Ruling `json:"ruling"`
		}
		if err := json.Unmarshal([]byte(frame.Data), &wrap); err != nil {
			t.Fatalf("decode ruling frame: %v (%s)", err, frame.Data)
		}
		if wrap.Ruling.Note == note {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("the ruling frame lost its note:\n%s", body)
	}
}
