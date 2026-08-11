package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/cards"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
)

const singleFaceJSON = `{
	"name": "Lightning Bolt",
	"mana_cost": "{R}",
	"type_line": "Instant",
	"oracle_text": "Lightning Bolt deals 3 damage to any target.",
	"set": "lea",
	"scryfall_uri": "https://scryfall.com/card/lea/161/lightning-bolt",
	"image_uris": {"normal": "https://img.scryfall.com/bolt.jpg"}
}`

func newTestServer(t *testing.T, scryfallHandler http.HandlerFunc) (*Server, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(scryfallHandler))
	t.Cleanup(srv.Close)

	store, err := index.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	s, err := New(store, llm.New(llm.Config{}), cards.NewWithBase(srv.URL))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return s, srv
}

func doGet(t *testing.T, s *Server, target string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

func TestHandleCard_DirectMatch(t *testing.T) {
	s, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cards/named" && r.URL.Query().Get("fuzzy") == "Lightning Bolt" {
			w.Header().Set("content-type", "application/json")
			_, _ = w.Write([]byte(singleFaceJSON))
			return
		}
		http.NotFound(w, r)
	})

	code, body := doGet(t, s, "/api/card?q=Lightning+Bolt")
	if code != http.StatusOK {
		t.Fatalf("code = %d", code)
	}
	card, ok := body["card"].(map[string]any)
	if !ok || card == nil {
		t.Fatalf("no card in response: %v", body)
	}
	if card["name"] != "Lightning Bolt" {
		t.Errorf("name = %v", card["name"])
	}
	if card["oracle_text"] != "Lightning Bolt deals 3 damage to any target." {
		t.Errorf("oracle = %v", card["oracle_text"])
	}
}

func TestHandleCard_FallsBackToSearch(t *testing.T) {
	s, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		// named lookup misses
		if r.URL.Path == "/cards/named" {
			http.NotFound(w, r)
			return
		}
		// search returns a couple of candidates
		if r.URL.Path == "/cards/search" {
			w.Header().Set("content-type", "application/json")
			_, _ = w.Write([]byte(`{"data":[` + singleFaceJSON + `]}`))
			return
		}
		http.NotFound(w, r)
	})

	code, body := doGet(t, s, "/api/card?q=bolt")
	if code != http.StatusOK {
		t.Fatalf("code = %d", code)
	}
	if body["card"] != nil {
		t.Errorf("card should be nil on no direct match, got %v", body["card"])
	}
	matches, ok := body["matches"].([]any)
	if !ok || len(matches) != 1 {
		t.Fatalf("matches = %v", body["matches"])
	}
}

func TestHandleCard_RequiresQuery(t *testing.T) {
	s, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream should not be hit on empty query")
	})
	code, body := doGet(t, s, "/api/card?q=")
	if code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400", code)
	}
	if !strings.Contains(body["error"].(string), "required") {
		t.Errorf("error = %v", body["error"])
	}
}

func TestLookupQuestionCards_MTGOnly(t *testing.T) {
	// D&D corpus must never trigger Scryfall lookups.
	s, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("scryfall should not be called for D&D")
	})
	docs, hits := s.lookupQuestionCards(context.Background(), "dnd", `what does "Fireball" do`)
	if docs != nil || hits != nil {
		t.Errorf("expected nil for D&D, got docs=%v hits=%v", docs, hits)
	}
}

func TestLookupQuestionCards_NilService(t *testing.T) {
	// A nil card service is graceful (no panic, no lookups).
	store, err := index.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s, err := New(store, llm.New(llm.Config{}), nil)
	if err != nil {
		t.Fatal(err)
	}
	docs, hits := s.lookupQuestionCards(context.Background(), "mtg", `what does "Lightning Bolt" do`)
	if docs != nil || hits != nil {
		t.Errorf("expected nil with no card service, got %v / %v", docs, hits)
	}
}
