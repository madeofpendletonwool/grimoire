package server

// The party board's handler tests (MAD-423): the snapshot's shapes and
// its leak posture (asserted on the raw JSON's keys — absence, not
// zeros), the SSE stream under live writes and concurrent clients, and
// the visibility config's round trip. The dice stream's harness shape
// (TestRollStreamPushesLive) is the pattern throughout.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/auth"
	"github.com/madeofpendletonwool/grimoire/internal/board"
	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/canon"
	"github.com/madeofpendletonwool/grimoire/internal/combat"
	"github.com/madeofpendletonwool/grimoire/internal/dice"
	"github.com/madeofpendletonwool/grimoire/internal/effects"
	"github.com/madeofpendletonwool/grimoire/internal/gamesession"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
	"github.com/madeofpendletonwool/grimoire/internal/ledger"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
	"github.com/madeofpendletonwool/grimoire/internal/migrate"
	"github.com/madeofpendletonwool/grimoire/internal/pubsub"
)

// newBoardServer wires the full mechanical stack the way runServe does:
// one broker shared by every store, the ledger holding the hp bridge,
// the board over all of them.
func newBoardServer(t *testing.T) (*Server, *fixture) {
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
	sessions, err := gamesession.New(store.DB())
	if err != nil {
		t.Fatalf("open session store: %v", err)
	}
	offlineCanon, err := canon.NewOffline(store.DB())
	if err != nil {
		t.Fatalf("open canon store: %v", err)
	}
	diceStore, err := dice.New(store.DB(), campaigns, sessions)
	if err != nil {
		t.Fatalf("open dice store: %v", err)
	}
	effectEngine, err := effects.New(store.DB(), campaigns, nil)
	if err != nil {
		t.Fatalf("open effects store: %v", err)
	}
	ledgerEngine, err := ledger.New(store.DB(), campaigns, offlineCanon)
	if err != nil {
		t.Fatalf("open ledger store: %v", err)
	}
	combatEngine, err := combat.New(store.DB(), campaigns, sessions, diceStore)
	if err != nil {
		t.Fatalf("open combat store: %v", err)
	}
	combatEngine = combatEngine.WithEffects(effectEngine).WithHitPoints(ledgerEngine)

	broker := pubsub.New()
	campaigns.WithBroker(broker)
	diceStore.WithBroker(broker)
	effectEngine.WithBroker(broker)
	ledgerEngine.WithBroker(broker)
	combatEngine.WithBroker(broker)

	boardStore, err := board.New(campaigns, ledgerEngine, effectEngine, combatEngine, users)
	if err != nil {
		t.Fatalf("open board store: %v", err)
	}
	boardStore.WithBroker(broker)

	s, err := New(store, llm.New(llm.Config{}), nil, nil, nil, nil, nil, nil, Auth{Users: users}, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	s = s.WithCampaign(campaigns, sessions).
		WithCampaigns(campaigns, knowledgeStore).
		WithDice(diceStore).WithEffects(effectEngine).
		WithLedger(ledgerEngine).WithCombat(combatEngine).
		WithBoard(boardStore)
	f := buildFixture(t, s)

	// A typed sheet on the fixture pc, through the real surface — the
	// ledger's pools (hp included) seed from it on the write.
	dm := dmSession(t, s)
	rec := hit(t, s, http.MethodPut, "/api/campaigns/"+f.campaignID+"/characters/"+f.pcID+"/sheet",
		`{"classes":[{"class":"wizard","level":5}],"ac":13,"max_hp":32,`+
			`"spellcasting":{"slots":{"1":4,"2":3,"3":2}}}`, dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("put sheet: status %d, body %s", rec.Code, rec.Body)
	}
	return s, &f
}

// hpNumber spots an hp number on the wire: `"hp":<digit>` — the config
// echo's string value does not match.
var hpNumber = regexp.MustCompile(`"hp":\s*[0-9]`)

// boardBody fetches one snapshot as generic maps — the shape the wire
// carries, which is what the leak assertions read.
func boardBody(t *testing.T, s *Server, f fixture, cookie *http.Cookie) map[string]any {
	t.Helper()
	rec := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/board", "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("board: status %d, body %s", rec.Code, rec.Body)
	}
	var body struct {
		Board map[string]any `json:"board"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("board body: %v", err)
	}
	return body.Board
}

// stripOf reads one member strip out of a generic board body.
func stripOf(t *testing.T, b map[string]any, characterID string) map[string]any {
	t.Helper()
	members, ok := b["members"].([]any)
	if !ok {
		t.Fatalf("board carries no members: %v", b["members"])
	}
	for _, m := range members {
		strip, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if strip["character_id"] == characterID {
			return strip
		}
	}
	t.Fatalf("no strip for %s", characterID)
	return nil
}

func TestBoardSnapshotShapes(t *testing.T) {
	s, f := newBoardServer(t)
	dm := dmSession(t, s)
	player := addPlayerMember(t, s, *f, "mira", true)

	// Default config: exact, visible. The player's own strip carries the
	// numbers the ledger derived.
	b := boardBody(t, s, *f, player)
	own := stripOf(t, b, f.pcID)
	if own["hp"] != float64(32) || own["max_hp"] != float64(32) {
		t.Fatalf("own strip hp = %v/%v, want 32/32", own["hp"], own["max_hp"])
	}
	slots, ok := own["slots"].([]any)
	if !ok || len(slots) != 3 {
		t.Fatalf("own strip slots = %v", own["slots"])
	}

	// The DM reads the same board with the dm flag and the exact strip.
	db := boardBody(t, s, *f, dm)
	if db["dm"] != true {
		t.Fatal("the dm's board does not say dm")
	}
}

func TestBoardSnapshotWordModeLeak(t *testing.T) {
	s, f := newBoardServer(t)
	dm := dmSession(t, s)
	player := addPlayerMember(t, s, *f, "mira", true)
	observer := addPlayerMember(t, s, *f, "watcher", false)

	// The table goes word + private.
	rec := hit(t, s, http.MethodPut, "/api/campaigns/"+f.campaignID+"/board/settings",
		`{"hp":"word","slots":"private"}`, dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("settings: status %d, body %s", rec.Code, rec.Body)
	}

	// The bound player: their own strip stays exact (5e players know
	// their own numbers), and their own slots ride even in private mode.
	b := boardBody(t, s, *f, player)
	own := stripOf(t, b, f.pcID)
	if own["hp"] != float64(32) {
		t.Fatalf("own strip hp = %v, want exact even in word mode", own["hp"])
	}
	if _, has := own["slots"]; !has {
		t.Fatal("own slots absent in private mode")
	}

	// The observer: nobody's strip may carry a number. Keys are checked
	// on the generic map — the shape the server serialized — so a hidden
	// field that shipped as a zero still fails here.
	ob := boardBody(t, s, *f, observer)
	members := ob["members"].([]any)
	if len(members) == 0 {
		t.Fatal("the observer sees no strips")
	}
	for _, m := range members {
		strip := m.(map[string]any)
		for _, leak := range []string{"hp", "max_hp", "temp_hp", "slots", "warnings"} {
			if _, has := strip[leak]; has {
				t.Fatalf("observer strip for %v leaked %q — a leak", strip["name"], leak)
			}
		}
	}

	// And the raw wire body itself: word mode never spells an hp number
	// (the config echo may spell the key — with a string value).
	raw := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/board", "", observer)
	if hpNumber.MatchString(raw.Body.String()) {
		t.Fatalf("word-mode wire body carries an hp number: %s", raw.Body)
	}
}

func TestBoardSettingsValidationAndPermissions(t *testing.T) {
	s, f := newBoardServer(t)
	dm := dmSession(t, s)
	player := addPlayerMember(t, s, *f, "mira", true)

	if rec := hit(t, s, http.MethodPut, "/api/campaigns/"+f.campaignID+"/board/settings",
		`{"hp":"whenever"}`, dm); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad config accepted: status %d", rec.Code)
	}
	if rec := hit(t, s, http.MethodPut, "/api/campaigns/"+f.campaignID+"/board/settings",
		`{"hp":"word"}`, player); rec.Code != http.StatusForbidden {
		t.Fatalf("a player set the board config: status %d", rec.Code)
	}

	// A partial update changes what it names and keeps the rest.
	if rec := hit(t, s, http.MethodPut, "/api/campaigns/"+f.campaignID+"/board/settings",
		`{"hp":"word"}`, dm); rec.Code != http.StatusOK {
		t.Fatalf("set word mode: status %d, body %s", rec.Code, rec.Body)
	}
	b := boardBody(t, s, *f, dm)
	cfg, ok := b["config"].(map[string]any)
	if !ok || cfg["hp"] != "word" || cfg["slots"] != "visible" {
		t.Fatalf("config after partial update = %v", b["config"])
	}
}

func TestBoardStreamPushesLiveAndLeaksNothing(t *testing.T) {
	s, f := newBoardServer(t)
	dm := dmSession(t, s)
	addPlayerMember(t, s, *f, "mira", true) // the wizard's player
	observer := addPlayerMember(t, s, *f, "watcher", false)

	// Word mode first: whatever the stream pushes, it must not push a
	// number the table hid.
	if rec := hit(t, s, http.MethodPut, "/api/campaigns/"+f.campaignID+"/board/settings",
		`{"hp":"word","slots":"private"}`, dm); rec.Code != http.StatusOK {
		t.Fatalf("settings: %d", rec.Code)
	}

	// The hp pool's id, through the player's own surface.
	poolID := hpPoolID(t, s, *f, dm)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/campaigns/"+f.campaignID+"/board/stream", nil)
	req = req.WithContext(ctx)
	req.AddCookie(observer)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Handler().ServeHTTP(rec, req)
	}()

	time.Sleep(150 * time.Millisecond)
	// The DM hurts the wizard — an out-of-combat wound through the
	// ledger, the exact path the hp pool exists for.
	if r := hit(t, s, http.MethodPost,
		"/api/campaigns/"+f.campaignID+"/characters/"+f.pcID+"/resources/"+poolID+"/transactions",
		`{"kind":"spend","amount":22,"note":"the trap"}`, dm); r.Code != http.StatusCreated {
		t.Fatalf("hurt the wizard: status %d, body %s", r.Code, r.Body)
	}
	time.Sleep(500 * time.Millisecond)
	cancel()
	<-done

	body := rec.Body.String()
	if !strings.Contains(rec.Header().Get("content-type"), "text/event-stream") {
		t.Fatalf("content-type %q", rec.Header().Get("content-type"))
	}
	if !strings.Contains(body, "event: open") {
		t.Fatal("the stream never opened")
	}
	if !strings.Contains(body, "event: board") {
		t.Fatal("a ledger write never re-rendered the board")
	}
	if !strings.Contains(body, "bloodied") {
		t.Fatal("the word-mode change (10/32) never arrived as a word")
	}
	// The config echo legitimately spells hp/slots as string values; the
	// leaks are numbers and arrays, and those are what we hunt.
	for _, leak := range []string{`"max_hp":`, `"temp_hp":`, `"slots":[`, `"warnings":`} {
		if strings.Contains(body, leak) {
			t.Fatalf("word+private stream leaked %q — a leak:\n%s", leak, body)
		}
	}
	if hpNumber.MatchString(body) {
		t.Fatalf("word+private stream leaked an hp number — a leak:\n%s", body)
	}
}

func TestBoardStreamConcurrentClients(t *testing.T) {
	s, f := newBoardServer(t)
	dm := dmSession(t, s)
	watcher := addPlayerMember(t, s, *f, "watcher", true)

	poolID := hpPoolID(t, s, *f, dm)

	open := func(cookie *http.Cookie) (*httptest.ResponseRecorder, context.CancelFunc, <-chan struct{}) {
		ctx, cancel := context.WithCancel(context.Background())
		req := httptest.NewRequest(http.MethodGet, "/api/campaigns/"+f.campaignID+"/board/stream", nil)
		req = req.WithContext(ctx)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		done := make(chan struct{})
		go func() {
			defer close(done)
			s.Handler().ServeHTTP(rec, req)
		}()
		return rec, cancel, done
	}
	recA, cancelA, doneA := open(dm)
	recB, cancelB, doneB := open(watcher)
	defer func() { cancelA(); cancelB(); <-doneA; <-doneB }()

	time.Sleep(150 * time.Millisecond)
	if r := hit(t, s, http.MethodPost,
		"/api/campaigns/"+f.campaignID+"/characters/"+f.pcID+"/resources/"+poolID+"/transactions",
		`{"kind":"spend","amount":4,"note":"a scratch"}`, dm); r.Code != http.StatusCreated {
		t.Fatalf("spend: status %d, body %s", r.Code, r.Body)
	}
	time.Sleep(600 * time.Millisecond)
	cancelA()
	cancelB()
	<-doneA
	<-doneB

	for name, rec := range map[string]*httptest.ResponseRecorder{"dm": recA, "player": recB} {
		body := rec.Body.String()
		if !strings.Contains(body, "event: open") {
			t.Fatalf("%s stream never opened", name)
		}
		if !strings.Contains(body, "event: board") {
			t.Fatalf("%s stream missed the live update — updates must land under concurrent clients", name)
		}
	}

	// Presence: both streams were open; the last snapshot the dm's
	// stream sent carries both users... the open frame raced the other
	// subscriber's join, so assert at most on the keeper-dm session and
	// the player's presence in any frame the player's own stream sent.
	if !strings.Contains(recB.Body.String(), `"present":true`) {
		t.Fatal("a connected player never showed present on the board")
	}
}

// hpPoolID finds the hp pool through the DM's resources read.
func hpPoolID(t *testing.T, s *Server, f fixture, dm *http.Cookie) string {
	t.Helper()
	rec := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/characters/"+f.pcID+"/resources", "", dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("resources: status %d, body %s", rec.Code, rec.Body)
	}
	var body struct {
		Balances []struct {
			Pool struct {
				ID   string `json:"id"`
				Kind string `json:"kind"`
				Name string `json:"name"`
			} `json:"pool"`
		} `json:"balances"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("resources body: %v", err)
	}
	for _, b := range body.Balances {
		if b.Pool.Kind == "hp" {
			return b.Pool.ID
		}
	}
	t.Fatalf("no hp pool in %+v", body.Balances)
	return ""
}
