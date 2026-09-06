package server

// The dice surface's HTTP contract (MAD-420): the round-trip, the
// permission lines (a player rolls public as their own character; the DM
// may roll secret), and the leak test — a secret roll is absent from the
// player's feed and stream because the query filtered it, not the
// handler. The SSE test drives the real stream end to end.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/auth"
	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/dice"
	"github.com/madeofpendletonwool/grimoire/internal/faction"
	"github.com/madeofpendletonwool/grimoire/internal/gamesession"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
	"github.com/madeofpendletonwool/grimoire/internal/migrate"
)

// newDiceServer boots the full stack the roller needs: the campaign
// graph, the session log (the roll mirror), and the dice store.
func newDiceServer(t *testing.T) (*Server, *fixture) {
	t.Helper()
	store, err := index.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := migrate.Up(store.DB()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	users, err := auth.New(store.DB(), 0, 0)
	if err != nil {
		t.Fatalf("open auth store: %v", err)
	}
	campaigns, err := campaign.New(store.DB())
	if err != nil {
		t.Fatalf("open campaign store: %v", err)
	}
	knowledgeStore, err := knowledge.New(store.DB())
	if err != nil {
		t.Fatalf("open knowledge store: %v", err)
	}
	factions, err := faction.New(store.DB())
	if err != nil {
		t.Fatalf("open faction store: %v", err)
	}
	sessions, err := gamesession.New(store.DB())
	if err != nil {
		t.Fatalf("open session store: %v", err)
	}
	diceStore, err := dice.New(store.DB(), campaigns, sessions)
	if err != nil {
		t.Fatalf("open dice store: %v", err)
	}
	s, err := New(store, llm.New(llm.Config{}), nil, nil, nil, nil, nil, nil, Auth{Users: users}, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	s = s.WithCampaign(campaigns, sessions).
		WithCampaigns(campaigns, knowledgeStore).WithFactions(factions).WithDice(diceStore)
	f := buildFixture(t, s)
	return s, &f
}

// rollResponse is the POST body's shape, for the tests' round-trips.
type rollResponse struct {
	Roll struct {
		ID         string          `json:"id"`
		Seq        int64           `json:"seq"`
		Formula    string          `json:"formula"`
		Mode       string          `json:"mode"`
		Notation   string          `json:"notation"`
		Dice       json.RawMessage `json:"dice"`
		Modifier   int             `json:"modifier"`
		Total      int             `json:"total"`
		Visibility string          `json:"visibility"`
		Character  string          `json:"character_name"`
		Actor      string          `json:"actor_name"`
	} `json:"roll"`
}

func rollFrom(t *testing.T, rec *recorder) rollResponse {
	t.Helper()
	if rec.Code != http.StatusCreated {
		t.Fatalf("roll: status %d, body %s", rec.Code, rec.Body)
	}
	var out rollResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("roll body: %v", err)
	}
	return out
}

func TestRollRoundTrip(t *testing.T) {
	s, f := newDiceServer(t)
	dm := dmSession(t, s)

	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/rolls",
		`{"formula":"2d6+3","context":"damage","detail":"Fireball","character_id":`+quote(f.pcID)+`}`, dm)
	roll := rollFrom(t, rec)

	// The total is the kept dice plus the flats — recomputed from the
	// natural dice the response carries, nothing trusted on faith.
	var terms []struct {
		Kind     string `json:"kind"`
		Sign     int    `json:"sign"`
		Subtotal int    `json:"subtotal"`
	}
	if err := json.Unmarshal(roll.Roll.Dice, &terms); err != nil {
		t.Fatalf("dice terms: %v", err)
	}
	sum := 0
	for _, term := range terms {
		sum += term.Sign * term.Subtotal
	}
	if sum != roll.Roll.Total {
		t.Fatalf("total %d does not match the terms %d", roll.Roll.Total, sum)
	}
	if roll.Roll.Formula != "2d6 + 3" {
		t.Fatalf("formula normalized to %q", roll.Roll.Formula)
	}
	if roll.Roll.Visibility != "public" {
		t.Fatalf("default visibility is public, got %q", roll.Roll.Visibility)
	}
	if roll.Roll.Actor != "keeper" {
		t.Fatalf("the roll's actor resolved to %q, want the roller's own name", roll.Roll.Actor)
	}

	// The feed carries it back, with the cursor to resume from.
	feed := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/rolls", "", dm)
	if feed.Code != http.StatusOK {
		t.Fatalf("feed: status %d, body %s", feed.Code, feed.Body)
	}
	var body struct {
		Rolls  []rollResponse_Roll `json:"rolls"`
		Latest int64               `json:"latest"`
	}
	if err := json.Unmarshal(feed.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Rolls) != 1 || body.Rolls[0].ID != roll.Roll.ID {
		t.Fatalf("feed does not round-trip the roll: %+v", body.Rolls)
	}
	if body.Latest != roll.Roll.Seq {
		t.Fatalf("feed latest %d, roll seq %d", body.Latest, roll.Roll.Seq)
	}
}

// rollResponse_Roll mirrors rollResponse's inner shape for list reads.
type rollResponse_Roll struct {
	ID string `json:"id"`
}

func TestRollVisibilityLeak(t *testing.T) {
	s, f := newDiceServer(t)
	dm := dmSession(t, s)
	player := addPlayerMember(t, s, *f, "roller", true)

	// The DM's secret roll — the goblin's save the table must not see.
	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/rolls",
		`{"formula":"1d20+2","visibility":"secret","context":"save","detail":"goblin vs spell"}`, dm)
	secret := rollFrom(t, rec)
	if secret.Roll.Visibility != "secret" {
		t.Fatalf("visibility round-trip: %q", secret.Roll.Visibility)
	}
	// A public roll the party shares.
	pub := rollFrom(t, hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/rolls",
		`{"formula":"1d20+5","context":"attack"}`, dm))

	// The DM sees both.
	dmFeed := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/rolls?limit=10", "", dm)
	var dmBody struct {
		Rolls []struct {
			ID         string `json:"id"`
			Visibility string `json:"visibility"`
		} `json:"rolls"`
	}
	if err := json.Unmarshal(dmFeed.Body.Bytes(), &dmBody); err != nil {
		t.Fatal(err)
	}
	if len(dmBody.Rolls) != 2 {
		t.Fatalf("DM feed has %d rolls, want 2", len(dmBody.Rolls))
	}

	// The player sees exactly the public one — the secret row is absent
	// from the payload, not blanked.
	playerFeed := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/rolls?limit=10", "", player)
	if playerFeed.Code != http.StatusOK {
		t.Fatalf("player feed: status %d", playerFeed.Code)
	}
	raw := playerFeed.Body.String()
	if strings.Contains(raw, secret.Roll.ID) {
		t.Fatal("the player's feed contains the secret roll's id — a leak")
	}
	if strings.Contains(raw, "goblin") || strings.Contains(raw, "secret") {
		t.Fatal("the player's feed contains the secret roll's content — a leak")
	}
	if !strings.Contains(raw, pub.Roll.ID) {
		t.Fatal("the player's feed lost the public roll")
	}

	// A player cannot roll secret, and cannot roll as another character.
	if rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/rolls",
		`{"formula":"1d20+5","visibility":"secret"}`, player); rec.Code != http.StatusForbidden {
		t.Fatalf("player secret roll: status %d, want 403", rec.Code)
	}
	if rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/rolls",
		`{"formula":"1d20+5","character_id":`+quote(f.dukeID)+`}`, player); rec.Code != http.StatusForbidden {
		t.Fatalf("player rolling as the duke: status %d, want 403", rec.Code)
	}

	// A player's own roll lands public, as their bound character, without
	// asking.
	mine := rollFrom(t, hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/rolls",
		`{"formula":"1d20+3","context":"check"}`, player))
	if mine.Roll.Visibility != "public" {
		t.Fatalf("player roll visibility %q", mine.Roll.Visibility)
	}
	if mine.Roll.Character != "Mira Thorn" {
		t.Fatalf("player rolled as %q, want their bound Mira Thorn", mine.Roll.Character)
	}
}

func TestRollMalformedIsError(t *testing.T) {
	s, f := newDiceServer(t)
	dm := dmSession(t, s)
	for _, formula := range []string{"", "banana", "2d6+", "1d20+5*2", "3d"} {
		rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/rolls",
			`{"formula":`+quote(formula)+`}`, dm)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("formula %q: status %d, want 400", formula, rec.Code)
		}
	}
	// Advantage on a formula without a d20 term is the same refusal.
	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/rolls",
		`{"formula":"2d6+3","mode":"advantage"}`, dm)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("advantage on damage: status %d, want 400", rec.Code)
	}
}

func TestRollAdvantageMode(t *testing.T) {
	s, f := newDiceServer(t)
	dm := dmSession(t, s)
	roll := rollFrom(t, hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/rolls",
		`{"formula":"1d20+5","mode":"advantage","context":"attack"}`, dm))
	if roll.Roll.Mode != "advantage" {
		t.Fatalf("mode %q", roll.Roll.Mode)
	}
	var terms []struct {
		Count int `json:"count"`
		Sides int `json:"sides"`
		Keep  int `json:"keep"`
		Dice  []struct {
			Value int  `json:"value"`
			Kept  bool `json:"kept"`
		} `json:"dice"`
		Subtotal int `json:"subtotal"`
		Sign     int `json:"sign"`
	}
	if err := json.Unmarshal(roll.Roll.Dice, &terms); err != nil {
		t.Fatal(err)
	}
	d20 := terms[0]
	if d20.Count != 2 || d20.Sides != 20 || d20.Keep != 1 || len(d20.Dice) != 2 {
		t.Fatalf("advantage term is %+v", d20)
	}
	kept := 0
	for _, d := range d20.Dice {
		if d.Kept {
			kept = d.Value
		}
	}
	if kept != d20.Subtotal {
		t.Fatalf("kept die %d != subtotal %d", kept, d20.Subtotal)
	}
}

func TestRollMirrorsIntoLiveSession(t *testing.T) {
	s, f := newDiceServer(t)
	dm := dmSession(t, s)

	// A live sitting for tonight.
	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/sessions", `{"name":"Night 12"}`, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create session: status %d, body %s", rec.Code, rec.Body)
	}
	var ses struct {
		Session struct {
			ID string `json:"id"`
		} `json:"session"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ses); err != nil {
		t.Fatal(err)
	}
	if r := hit(t, s, http.MethodPatch, "/api/campaigns/"+f.campaignID+"/sessions/"+ses.Session.ID,
		`{"status":"live"}`, dm); r.Code != http.StatusOK {
		t.Fatalf("go live: status %d, body %s", r.Code, r.Body)
	}

	roll := rollFrom(t, hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/rolls",
		`{"formula":"1d20+5","context":"attack","detail":"longsword"}`, dm))
	if roll.Roll.Seq == 0 {
		t.Fatal("no seq assigned")
	}

	// The session log carries the roll event, and the export prints the
	// roll's line instead of raw JSON.
	events := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/sessions/"+ses.Session.ID+"/events", "", dm)
	if events.Code != http.StatusOK {
		t.Fatalf("events: status %d", events.Code)
	}
	if !strings.Contains(events.Body.String(), `"kind":"roll"`) {
		t.Fatal("the roll did not mirror into the session log")
	}
	if !strings.Contains(events.Body.String(), "longsword") {
		t.Fatal("the roll event lost its detail")
	}

	export := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/sessions/"+ses.Session.ID+"/export", "", dm)
	if export.Code != http.StatusOK {
		t.Fatalf("export: status %d", export.Code)
	}
	if !strings.Contains(export.Body.String(), "→") {
		t.Fatal("the export lost the roll's total line")
	}
	if strings.Contains(export.Body.String(), `"map[string`) {
		t.Fatal("the export dumped the roll payload as raw JSON")
	}

	// A secret roll is marked in the DM's export.
	rollFrom(t, hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/rolls",
		`{"formula":"1d20","visibility":"secret"}`, dm))
	export2 := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/sessions/"+ses.Session.ID+"/export", "", dm)
	if !strings.Contains(export2.Body.String(), "secret") {
		t.Fatal("the export lost the secret marker")
	}
}

func TestRollFeedCursorPages(t *testing.T) {
	s, f := newDiceServer(t)
	dm := dmSession(t, s)
	var last int64
	for i := 0; i < 5; i++ {
		roll := rollFrom(t, hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/rolls",
			`{"formula":"1d4"}`, dm))
		last = roll.Roll.Seq
	}
	feed := hit(t, s, http.MethodGet, fmt.Sprintf("/api/campaigns/%s/rolls?after=%d", f.campaignID, last), "", dm)
	var body struct {
		Rolls []struct {
			Seq int64 `json:"seq"`
		} `json:"rolls"`
	}
	if err := json.Unmarshal(feed.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Rolls) != 0 {
		t.Fatalf("cursor after the last roll still returned %d rolls", len(body.Rolls))
	}
	// A cursor in the middle returns exactly the tail.
	mid := hit(t, s, http.MethodGet, fmt.Sprintf("/api/campaigns/%s/rolls?after=%d", f.campaignID, last-2), "", dm)
	if err := json.Unmarshal(mid.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Rolls) != 2 || body.Rolls[0].Seq != last-1 || body.Rolls[1].Seq != last {
		t.Fatalf("middle cursor returned %+v", body.Rolls)
	}
}

// TestRollStreamPushesLive drives the SSE stream end to end: a player
// holds the stream open, the DM rolls public and secret, and only the
// public roll arrives — ordered, framed, with nothing after it.
func TestRollStreamPushesLive(t *testing.T) {
	s, f := newDiceServer(t)
	dm := dmSession(t, s)
	player := addPlayerMember(t, s, *f, "watcher", true)

	pub := rollFrom(t, hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/rolls",
		`{"formula":"1d20+5"}`, dm)) // before the stream opens; the cursor skips it

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet,
		"/api/campaigns/"+f.campaignID+"/rolls/stream?after="+fmt.Sprint(pub.Roll.Seq), nil)
	req = req.WithContext(ctx)
	req.AddCookie(player)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Handler().ServeHTTP(rec, req)
	}()

	// The stream opens, then both rolls land; the broker wakes the
	// subscriber without waiting for the poll tick.
	time.Sleep(150 * time.Millisecond)
	next := rollFrom(t, hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/rolls",
		`{"formula":"2d6+1","context":"damage"}`, dm))
	rollFrom(t, hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/rolls",
		`{"formula":"1d20","visibility":"secret"}`, dm))
	time.Sleep(400 * time.Millisecond)
	cancel()
	<-done

	body := rec.Body.String()
	if !strings.Contains(body, "event: open") {
		t.Fatal("the stream never opened")
	}
	if !strings.Contains(rec.Header().Get("content-type"), "text/event-stream") {
		t.Fatalf("content-type %q", rec.Header().Get("content-type"))
	}
	if !strings.Contains(body, next.Roll.ID) {
		t.Fatal("the public roll never arrived on the stream")
	}
	if strings.Contains(body, "secret") {
		t.Fatal("the secret roll leaked through the stream — a leak")
	}
	// Ordered: the open frame precedes the roll frame.
	if strings.Index(body, "event: open") > strings.Index(body, next.Roll.ID) {
		t.Fatal("stream frames out of order")
	}
}

// TestRollDeterministicAtStore re-derives a stored roll from its seed and
// nonce — the replay contract, asserted at the store where both are
// readable.
func TestRollDeterministicAtStore(t *testing.T) {
	s, f := newDiceServer(t)
	dm := dmSession(t, s)
	first := rollFrom(t, hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/rolls",
		`{"formula":"3d6+2"}`, dm))

	rows, err := s.dice.Feed(t.Context(), f.campaignID, 0, 10, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != first.Roll.ID {
		t.Fatalf("feed disagrees with the POST: %+v", rows)
	}
	// seed + nonce + formula reproduce the stored dice exactly: parse
	// again, roll again, compare term for term.
	expr, err := dice.Parse("3d6+2")
	if err != nil {
		t.Fatal(err)
	}
	again := dice.Roll(rows[0].Seed, rows[0].Nonce, expr, "")
	if again.Total != rows[0].Result.Total {
		t.Fatalf("replay total %d, stored %d", again.Total, rows[0].Result.Total)
	}
	for i := range again.Terms {
		if len(again.Terms[i].Dice) != len(rows[0].Result.Terms[i].Dice) {
			t.Fatalf("replay term %d differs", i)
		}
		for j := range again.Terms[i].Dice {
			if again.Terms[i].Dice[j] != rows[0].Result.Terms[i].Dice[j] {
				t.Fatalf("replay die %d/%d: %v vs %v", i, j, again.Terms[i].Dice[j], rows[0].Result.Terms[i].Dice[j])
			}
		}
	}
}
