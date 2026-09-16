package server

// The deck-aware play surface's handler tests (MAD-336): exact odds
// over a driven game, outs from the actual remaining library, the
// order-dependent refusal over HTTP, the mulligan opt-in gate, and the
// account scope. Assertions are on the HTTP response; the odds
// package's own suite covers the maths.

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/auth"
	"github.com/madeofpendletonwool/grimoire/internal/carddb"
	"github.com/madeofpendletonwool/grimoire/internal/deck"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
	"github.com/madeofpendletonwool/grimoire/internal/migrate"
	"github.com/madeofpendletonwool/grimoire/internal/table/engine"
	"github.com/madeofpendletonwool/grimoire/internal/table/universe"
)

// oddsFixtureGzip builds the small AtomicCards corpus the odds surface
// reads: lands, a wipe, targeted answers, ramp, draw, Rhystic.
func oddsFixtureGzip(t *testing.T) []byte {
	t.Helper()
	payload := map[string]any{"data": map[string][]map[string]any{
		"Forest": {{"type": "Basic Land — Forest", "text": "{T}: Add {G}.",
			"colorIdentity": []string{"G"}, "legalities": map[string]string{"commander": "Legal"}}},
		"Island": {{"type": "Basic Land — Island", "text": "{T}: Add {U}.",
			"colorIdentity": []string{"U"}, "legalities": map[string]string{"commander": "Legal"}}},
		"Wrath of God": {{"manaCost": "{2}{W}{W}", "manaValue": 4, "type": "Sorcery",
			"text": "Destroy all creatures.", "colorIdentity": []string{"W"},
			"legalities": map[string]string{"commander": "Legal"}}},
		"Counterspell": {{"manaCost": "{U}{U}", "manaValue": 2, "type": "Instant",
			"text": "Counter target spell.", "colorIdentity": []string{"U"},
			"legalities": map[string]string{"commander": "Legal"}}},
		"Naturalize": {{"manaCost": "{1}{G}", "manaValue": 2, "type": "Instant",
			"text": "Destroy target artifact or enchantment.", "colorIdentity": []string{"G"},
			"legalities": map[string]string{"commander": "Legal"}}},
		"Beast Within": {{"manaCost": "{2}{G}", "manaValue": 3, "type": "Instant",
			"text":          "Destroy target permanent. Its controller creates a 3/3 green Beast creature token.",
			"colorIdentity": []string{"G"}, "legalities": map[string]string{"commander": "Legal"}}},
		"Sol Ring": {{"manaCost": "{1}", "manaValue": 1, "type": "Artifact",
			"text": "{T}: Add {C}{C}.", "colorIdentity": []string{},
			"legalities": map[string]string{"commander": "Legal"}}},
		"Cultivate": {{"manaCost": "{2}{G}", "manaValue": 3, "type": "Sorcery",
			"text":          "Search your library for up to two basic land cards. Put one onto the battlefield tapped and the other into your hand.",
			"colorIdentity": []string{"G"}, "legalities": map[string]string{"commander": "Legal"}}},
		"Harmonize": {{"manaCost": "{2}{G}", "manaValue": 3, "type": "Sorcery",
			"text": "Draw three cards.", "colorIdentity": []string{"G"},
			"legalities": map[string]string{"commander": "Legal"}}},
		"Rhystic Study": {{"manaCost": "{1}{U}{U}", "manaValue": 3, "type": "Enchantment",
			"text":          "Whenever an opponent casts a spell, you may draw a card unless they pay {1}.",
			"colorIdentity": []string{"U"}, "legalities": map[string]string{"commander": "Legal"}}},
		"Atraxa, Praetors' Voice": {{"manaCost": "{2}{W}{U}{B}{G}", "manaValue": 4,
			"type": "Legendary Creature — Phyrexian Angel", "text": "Flying, vigilance...",
			"colorIdentity": []string{"W", "U", "B", "G"}, "edhrecRank": 20,
			"leadershipSkills": map[string]any{"Commander": true},
			"legalities":       map[string]string{"commander": "Legal"}}},
	}}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return buf.Bytes()
}

// newOddsServer wires games + universe + the card index, the way
// runServe does when the card store exists.
func newOddsServer(t *testing.T) (*Server, *engine.Store) {
	t.Helper()
	store, err := index.Open(filepath.Join(t.TempDir(), "odds.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := migrate.Up(store.DB()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	mtgjson := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/gzip")
		_, _ = w.Write(oddsFixtureGzip(t))
	}))
	t.Cleanup(mtgjson.Close)
	if _, err := carddb.Populate(context.Background(), store.DB(), mtgjson.URL); err != nil {
		t.Fatalf("seed cards: %v", err)
	}
	cdb, err := carddb.New(store.DB())
	if err != nil {
		t.Fatalf("carddb: %v", err)
	}
	users, err := auth.New(store.DB(), 0, 0)
	if err != nil {
		t.Fatalf("open auth store: %v", err)
	}
	games, err := engine.New(store.DB())
	if err != nil {
		t.Fatalf("open engine store: %v", err)
	}
	universeStore, err := universe.NewStore(store.DB(), cdb)
	if err != nil {
		t.Fatalf("open universe store: %v", err)
	}
	decks, err := deck.New(store.DB())
	if err != nil {
		t.Fatalf("open deck store: %v", err)
	}
	s, err := New(store, llm.New(llm.Config{}), nil, nil, nil, nil, nil, nil, Auth{Users: users}, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	s = s.WithGames(games).WithUniverse(universeStore).WithDeckBuilder(cdb, decks, nil)
	return s, games
}

// seedOddsDeck inserts the decks row a seat attaches: the counts each
// test names, plus the commander on her board.
func seedOddsDeck(t *testing.T, games *engine.Store, id string, cards map[string]int) {
	t.Helper()
	rows := []struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
		Board string `json:"board"`
	}{
		{Name: "Atraxa, Praetors' Voice", Count: 1, Board: "commander"},
	}
	for name, count := range cards {
		rows = append(rows, struct {
			Name  string `json:"name"`
			Count int    `json:"count"`
			Board string `json:"board"`
		}{Name: name, Count: count})
	}
	enc, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := games.DB().Exec(`INSERT INTO decks (id, owner_id, name, commander, cards, notes, created_at, updated_at)
		VALUES (?, 'seed', 'Test Deck', 'Atraxa, Praetors'' Voice', ?, '', ?, ?)`,
		id, string(enc), time.Now().UnixMilli(), time.Now().UnixMilli()); err != nil {
		t.Fatalf("seed deck: %v", err)
	}
}

func TestGameOddsOverHTTP(t *testing.T) {
	s, games := newOddsServer(t)
	admin := adminSession(t, s)
	seedOddsDeck(t, games, "odeck1", map[string]int{
		"Forest": 30, "Wrath of God": 2, "Naturalize": 1, "Beast Within": 2, "Rhystic Study": 1,
	})

	game := gameCreate(t, s, admin)
	for _, seat := range []string{
		`{"position":1,"name":"Collin","deck_id":"odeck1","commander":"Atraxa, Praetors' Voice"}`,
		`{"position":2,"name":"Bob"}`,
	} {
		rec := hit(t, s, http.MethodPost, "/api/games/"+game+"/seats", seat, admin)
		if rec.Code != http.StatusCreated {
			t.Fatalf("seat: status %d, body %s", rec.Code, rec.Body)
		}
	}
	if rec := hit(t, s, http.MethodPost, "/api/games/"+game+"/start", `{}`, admin); rec.Code != http.StatusOK {
		t.Fatalf("start: status %d, body %s", rec.Code, rec.Body)
	}
	// An identified draw: Forest, the Wrath and the Rhystic leave the
	// library, and the composition folds down.
	gameAction(t, s, admin, game, `{"kind":"DRAW","seat":1,"count":3,"cards":["Forest","Wrath of God","Rhystic Study"]}`)

	// The derived library: 36 began, 3 left, composition says so.
	rec := hit(t, s, http.MethodGet, "/api/games/"+game+"/library?seat=1", "", admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("library: status %d, body %s", rec.Code, rec.Body)
	}
	var lib struct {
		Library struct {
			Known  bool           `json:"known"`
			Exact  bool           `json:"exact"`
			N      int            `json:"n"`
			Counts map[string]int `json:"counts"`
		} `json:"library"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &lib); err != nil {
		t.Fatalf("library body: %v", err)
	}
	if !lib.Library.Known || !lib.Library.Exact || lib.Library.N != 33 {
		t.Fatalf("library = %+v, want exact 33", lib.Library)
	}
	if lib.Library.Counts["Wrath of God"] != 1 || lib.Library.Counts["Rhystic Study"] != 0 {
		t.Fatalf("composition = %v, want the drawn cards gone", lib.Library.Counts)
	}

	/* ---- exact odds, hand-checked ---- */

	// 29 Forests remain in 33: a land in the next three is
	// 1 − C(4,3)/C(33,3) = 1 − 4/5456 = 1363/1364.
	rec = hit(t, s, http.MethodPost, "/api/games/"+game+"/odds",
		`{"seat":1,"question":"chance of a land in the next three"}`, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("odds: status %d, body %s", rec.Code, rec.Body)
	}
	var ans struct {
		Answer struct {
			N        int     `json:"n"`
			K        int     `json:"k_hits"`
			Rational string  `json:"rational"`
			P        float64 `json:"probability"`
		} `json:"answer"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ans); err != nil {
		t.Fatalf("odds body: %v (%s)", err, rec.Body)
	}
	if ans.Answer.N != 33 || ans.Answer.K != 29 {
		t.Fatalf("N=%d K=%d, want 33/29", ans.Answer.N, ans.Answer.K)
	}
	if ans.Answer.Rational != "1363/1364" {
		t.Fatalf("rational = %s, want 1363/1364", ans.Answer.Rational)
	}

	// Reproducible: the same question answers identically.
	rec2 := hit(t, s, http.MethodPost, "/api/games/"+game+"/odds",
		`{"seat":1,"question":"chance of a land in the next three"}`, admin)
	if rec2.Body.String() != rec.Body.String() {
		t.Fatalf("answers differ between runs:\n%s\n%s", rec.Body, rec2.Body)
	}

	// By turn: from turn 1, "by turn nine" is nine draws, and the
	// answer carries its one-draw-per-turn assumption.
	rec = hit(t, s, http.MethodPost, "/api/games/"+game+"/odds",
		`{"seat":1,"question":"chance of finding a board wipe by turn nine"}`, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("by-turn odds: status %d, body %s", rec.Code, rec.Body)
	}
	var wipe struct {
		Answer struct {
			K    int    `json:"k_hits"`
			Note string `json:"note"`
		} `json:"answer"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &wipe); err != nil {
		t.Fatalf("by-turn body: %v", err)
	}
	if wipe.Answer.K != 1 {
		t.Fatalf("K = %d, want 1 (one Wrath remains)", wipe.Answer.K)
	}
	if wipe.Answer.Note == "" {
		t.Fatal("a by-turn answer must carry its one-draw-per-turn assumption")
	}

	/* ---- the order-dependent refusal, over HTTP ---- */

	for _, q := range []string{
		"what's my next card?",
		"when will I draw a Wrath of God",
		"is the top card of my library a land",
	} {
		body, _ := json.Marshal(map[string]any{"seat": 1, "question": q})
		rec := hit(t, s, http.MethodPost, "/api/games/"+game+"/odds", string(body), admin)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%q: status %d, want 400", q, rec.Code)
		}
		if !bytes.Contains(rec.Body.Bytes(), []byte("library order is never modelled")) {
			t.Fatalf("%q: body must carry the refusal: %s", q, rec.Body)
		}
	}

	// A no-parse is honest, not a guess.
	if rec := hit(t, s, http.MethodPost, "/api/games/"+game+"/odds",
		`{"seat":1,"question":"what should I do"}`, admin); rec.Code != http.StatusBadRequest {
		t.Fatalf("no-parse: status %d, want 400", rec.Code)
	}

	// A deckless seat answers unknown, not zero.
	rec = hit(t, s, http.MethodPost, "/api/games/"+game+"/odds",
		`{"seat":2,"question":"chance of a land in the next three"}`, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("deckless: status %d, body %s", rec.Code, rec.Body)
	}
	var unknown struct {
		Known bool `json:"known"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &unknown)
	if unknown.Known {
		t.Fatalf("deckless seat must answer unknown: %s", rec.Body)
	}

	// The account scope holds.
	friend := gameFriend(t, s, admin)
	if rec := hit(t, s, http.MethodPost, "/api/games/"+game+"/odds",
		`{"seat":1,"question":"chance of a land in the next three"}`, friend); rec.Code != http.StatusNotFound {
		t.Fatalf("friend: status %d, want 404", rec.Code)
	}
}

func TestGameOutsOverHTTP(t *testing.T) {
	s, games := newOddsServer(t)
	admin := adminSession(t, s)
	seedOddsDeck(t, games, "odeck2", map[string]int{
		"Forest": 30, "Wrath of God": 2, "Naturalize": 1, "Beast Within": 2,
	})

	game := gameCreate(t, s, admin)
	for _, seat := range []string{
		`{"position":1,"name":"Collin","deck_id":"odeck2","commander":"Atraxa, Praetors' Voice"}`,
		`{"position":2,"name":"Bob"}`,
	} {
		if rec := hit(t, s, http.MethodPost, "/api/games/"+game+"/seats", seat, admin); rec.Code != http.StatusCreated {
			t.Fatalf("seat: %d %s", rec.Code, rec.Body)
		}
	}
	if rec := hit(t, s, http.MethodPost, "/api/games/"+game+"/start", `{}`, admin); rec.Code != http.StatusOK {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	// Out of the untap step and into priority, the gameDrive path.
	gameAction(t, s, admin, game, `{"kind":"ADVANCE","seat":1}`)
	gameAction(t, s, admin, game, `{"kind":"ADVANCE","seat":1}`)

	// Free-text target: an enchantment. Naturalize and Beast Within
	// answer it, most copies first; Wrath only sweeps creatures.
	rec := hit(t, s, http.MethodPost, "/api/games/"+game+"/outs",
		`{"seat":1,"target":"enchantment","draws":3}`, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("outs: status %d, body %s", rec.Code, rec.Body)
	}
	var out struct {
		Outs struct {
			Target struct {
				Types []string `json:"types"`
			} `json:"target"`
			Outs []struct {
				Name    string   `json:"name"`
				Count   int      `json:"count"`
				Reasons []string `json:"reasons"`
			} `json:"outs"`
			K           int     `json:"k_hits"`
			Probability float64 `json:"probability"`
		} `json:"outs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("outs body: %v", err)
	}
	if len(out.Outs.Outs) != 2 || out.Outs.Outs[0].Name != "Beast Within" || out.Outs.Outs[1].Name != "Naturalize" {
		t.Fatalf("outs = %+v", out.Outs.Outs)
	}
	if out.Outs.K != 3 {
		t.Fatalf("K = %d, want 3", out.Outs.K)
	}
	for _, o := range out.Outs.Outs {
		if len(o.Reasons) == 0 {
			t.Fatalf("%s carries no reasons", o.Name)
		}
	}

	// The live-object path: cast an enchantment, let it resolve, then
	// answer the permanent by its object id. Seat 1 passes so seat 2
	// may cast; both pass to resolve.
	gameAction(t, s, admin, game, `{"kind":"PASS_PRIORITY","seat":1}`)
	gameAction(t, s, admin, game, `{"kind":"CAST","seat":2,"card":"Rhystic Study","base":{"types":["Enchantment"]}}`)
	gameAction(t, s, admin, game, `{"kind":"PASS_PRIORITY","seat":2}`)
	gameAction(t, s, admin, game, `{"kind":"PASS_PRIORITY","seat":1}`)
	st := gameState(t, s, admin, game)
	var rhystic int64
	for id, o := range st.Objects {
		if o.Identity.Card == "Rhystic Study" {
			rhystic = id
		}
	}
	if rhystic == 0 {
		t.Fatal("the Rhystic never landed on the stack")
	}
	body, _ := json.Marshal(map[string]any{"seat": 1, "object": rhystic, "draws": 1})
	rec = hit(t, s, http.MethodPost, "/api/games/"+game+"/outs", string(body), admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("outs by object: status %d, body %s", rec.Code, rec.Body)
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Outs.K != 3 {
		t.Fatalf("object outs K = %d, want 3", out.Outs.K)
	}

	// A missing object is 404, never invented.
	if rec := hit(t, s, http.MethodPost, "/api/games/"+game+"/outs",
		`{"seat":1,"object":9999}`, admin); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown object: status %d, want 404", rec.Code)
	}
}

func TestGameMulliganGateAndAdvice(t *testing.T) {
	s, games := newOddsServer(t)
	admin := adminSession(t, s)
	seedOddsDeck(t, games, "odeck3", map[string]int{
		"Forest": 30, "Cultivate": 4, "Harmonize": 4, "Beast Within": 2,
		"Sol Ring": 2, "Naturalize": 2,
	})

	game := gameCreate(t, s, admin)
	if rec := hit(t, s, http.MethodPost, "/api/games/"+game+"/seats",
		`{"position":1,"name":"Collin","deck_id":"odeck3","commander":"Atraxa, Praetors' Voice"}`, admin); rec.Code != http.StatusCreated {
		t.Fatalf("seat: %d %s", rec.Code, rec.Body)
	}
	hit(t, s, http.MethodPost, "/api/games/"+game+"/seats", `{"position":2,"name":"Bob"}`, admin)
	hit(t, s, http.MethodPost, "/api/games/"+game+"/start", `{}`, admin)

	hand := `{"seat":1,"hand":["Forest","Forest","Forest","Cultivate","Harmonize","Beast Within","Sol Ring"]}`

	// Off by default: the gate refuses — advice is opt-in per game.
	rec := hit(t, s, http.MethodPost, "/api/games/"+game+"/mulligan", hand, admin)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("ungated advice: status %d, want 403", rec.Code)
	}

	// Opt in through the settings surface; the game view carries it.
	if rec := hit(t, s, http.MethodPut, "/api/games/"+game+"/settings",
		`{"mulligan_advice":true}`, admin); rec.Code != http.StatusOK {
		t.Fatalf("settings: status %d, body %s", rec.Code, rec.Body)
	}
	rec = hit(t, s, http.MethodGet, "/api/games/"+game, "", admin)
	var view struct {
		Game struct {
			Settings map[string]any `json:"settings"`
		} `json:"game"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &view)
	if view.Game.Settings["mulligan_advice"] != true {
		t.Fatalf("settings did not round-trip: %v", view.Game.Settings)
	}

	// With the opt-in, the advice answers with its arithmetic.
	rec = hit(t, s, http.MethodPost, "/api/games/"+game+"/mulligan", hand, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("advice: status %d, body %s", rec.Code, rec.Body)
	}
	var advice struct {
		Advice struct {
			Verdict string   `json:"verdict"`
			Lands   int      `json:"lands"`
			Reasons []string `json:"reasons"`
		} `json:"advice"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &advice); err != nil {
		t.Fatalf("advice body: %v", err)
	}
	if advice.Advice.Verdict != "keep" || advice.Advice.Lands != 3 || len(advice.Advice.Reasons) == 0 {
		t.Fatalf("advice = %+v", advice.Advice)
	}

	// Strict settings: a non-boolean value errors rather than riding
	// along.
	if rec := hit(t, s, http.MethodPut, "/api/games/"+game+"/settings",
		`{"mulligan_advice":"yes"}`, admin); rec.Code != http.StatusBadRequest {
		t.Fatalf("loose settings: status %d, want 400", rec.Code)
	}

	// The account scope holds on the settings write too.
	friend := gameFriend(t, s, admin)
	if rec := hit(t, s, http.MethodPut, "/api/games/"+game+"/settings",
		`{"mulligan_advice":true}`, friend); rec.Code != http.StatusNotFound {
		t.Fatalf("friend settings: status %d, want 404", rec.Code)
	}
}

func TestGameOddsWithoutCardIndex(t *testing.T) {
	// The games-only install: named-card odds still answer from the
	// composition, category odds and outs say what is missing — honest
	// degradation, the universe pattern.
	s, games := newGamesServer(t)
	admin := adminSession(t, s)
	seedGameDeck(t, games, "gdeck1", "Atraxa, Praetors' Voice")

	game := gameCreate(t, s, admin)
	hit(t, s, http.MethodPost, "/api/games/"+game+"/seats",
		`{"position":1,"name":"Collin","deck_id":"gdeck1"}`, admin)
	hit(t, s, http.MethodPost, "/api/games/"+game+"/seats", `{"position":2,"name":"Bob"}`, admin)
	hit(t, s, http.MethodPost, "/api/games/"+game+"/start", `{}`, admin)

	// Named-card odds: composition only, no card data needed.
	rec := hit(t, s, http.MethodPost, "/api/games/"+game+"/odds",
		`{"seat":1,"card":"Rhystic Study","draws":3}`, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("named odds: status %d, body %s", rec.Code, rec.Body)
	}
	var ans struct {
		Answer struct {
			Rational string `json:"rational"`
		} `json:"answer"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &ans)
	if ans.Answer.Rational == "" {
		t.Fatal("named-card odds must answer from the composition alone")
	}

	// Category odds name the gap.
	if rec := hit(t, s, http.MethodPost, "/api/games/"+game+"/odds",
		`{"seat":1,"category":"lands","draws":3}`, admin); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("category odds without the index: status %d, want 503", rec.Code)
	}
	// Outs likewise.
	if rec := hit(t, s, http.MethodPost, "/api/games/"+game+"/outs",
		`{"seat":1,"target":"enchantment"}`, admin); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("outs without the index: status %d, want 503", rec.Code)
	}
}
