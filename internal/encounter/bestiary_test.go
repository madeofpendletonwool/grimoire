package encounter

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// bestiaryServer stubs /v2/creatures/ against fixtures keyed by the raw
// search term, so the search client is exercised end to end with no network.
func bestiaryServer(t *testing.T, pages map[string]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/creatures/", func(w http.ResponseWriter, r *http.Request) {
		body, ok := pages[strings.ToLower(r.URL.Query().Get("search"))]
		if !ok {
			body = `{"count":0,"results":[]}`
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func creaturesFixture(entries ...string) string {
	return `{"count":` + fmt.Sprint(len(entries)) + `,"results":[` + strings.Join(entries, ",") + `]}`
}

func creature(name, doc, cr, typ string) string {
	return fmt.Sprintf(`{"name":%q,"document":{"key":%q},"challenge_rating":%s,"type":{"name":%q}}`,
		name, doc, cr, typ)
}

const goblinsPage = `{"count":3,"results":[
  {"name":"Goblin","document":{"key":"srd-2024"},"challenge_rating":0.25,"type":{"name":"Humanoid"}},
  {"name":"Goblin Boss","document":{"key":"srd-2024"},"challenge_rating":1,"type":{"name":"Humanoid"}},
  {"name":"Goblin Minion","document":{"key":"a5e-ag"},"challenge_rating":0.125,"type":{"name":"Humanoid"}}
]}`

func TestBestiarySearchScopesToSRDAndRanks(t *testing.T) {
	srv := bestiaryServer(t, map[string]string{"goblin": goblinsPage})
	b := NewBestiaryWithBase(srv.URL)

	hits, err := b.Search(context.Background(), "goblin")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("got %d hits, want 2 (community doc dropped): %+v", len(hits), hits)
	}
	if hits[0].Name != "Goblin" || hits[0].CR != "1/4" || hits[0].XP != 50 || hits[0].Type != "Humanoid" {
		t.Errorf("first hit = %+v, want Goblin CR 1/4 XP 50", hits[0])
	}
	if hits[1].Name != "Goblin Boss" || hits[1].XP != 200 {
		t.Errorf("second hit = %+v, want Goblin Boss XP 200", hits[1])
	}
}

// "goblin boss" and "goblinboss" must both work: the squashed-name tier makes
// the run-on form an exact match even though icontains would miss it.
func TestBestiarySearchSquashedAndSpacedForms(t *testing.T) {
	srv := bestiaryServer(t, map[string]string{
		"goblin boss": goblinsPage,
		// The prefix fallback ("gobli") serves the run-on query.
		"gobli": goblinsPage,
	})
	b := NewBestiaryWithBase(srv.URL)

	for _, q := range []string{"goblin boss", "goblinboss", "Goblin Boss"} {
		hits, err := b.Search(context.Background(), q)
		if err != nil {
			t.Fatalf("search %q: %v", q, err)
		}
		if len(hits) == 0 || hits[0].Name != "Goblin Boss" {
			t.Errorf("search %q: top hit = %+v, want Goblin Boss", q, hits)
		}
	}
}

func TestBestiarySearchNoResults(t *testing.T) {
	srv := bestiaryServer(t, map[string]string{})
	b := NewBestiaryWithBase(srv.URL)
	hits, err := b.Search(context.Background(), "beholder tyranid")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("got %+v, want no hits", hits)
	}
}

// An unreachable API surfaces as an error the handler can degrade with —
// never a panic, never a fabricated result list.
func TestBestiarySearchUnreachable(t *testing.T) {
	srv := bestiaryServer(t, nil)
	b := NewBestiaryWithBase(srv.URL)
	srv.Close() // unreachable from here on
	if _, err := b.Search(context.Background(), "goblin"); err == nil {
		t.Fatal("search against a dead endpoint should error")
	}
}

// The cache means a repeated query does not re-hit the API.
func TestBestiarySearchCaches(t *testing.T) {
	hits := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/creatures/", func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(creaturesFixture(creature("Goblin", "srd-2024", "0.25", "Humanoid"))))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	b := NewBestiaryWithBase(srv.URL)

	for i := 0; i < 3; i++ {
		if _, err := b.Search(context.Background(), "goblin"); err != nil {
			t.Fatalf("search %d: %v", i, err)
		}
	}
	if hits != 1 {
		t.Errorf("API hit %d times, want 1 (cached)", hits)
	}
}

// A server that rejects the document filter still answers: the client
// retries unfiltered and scopes client-side.
func TestBestiarySearchFilterFallback(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/creatures/", func(w http.ResponseWriter, r *http.Request) {
		if _, filtered := r.URL.Query()["document__key__in"]; !filtered {
			w.Header().Set("content-type", "application/json")
			_, _ = w.Write([]byte(goblinsPage))
			return
		}
		http.Error(w, "unsupported filter", http.StatusBadRequest)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	b := NewBestiaryWithBase(srv.URL)

	hits, err := b.Search(context.Background(), "goblin")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("got %d hits, want 2: %+v", len(hits), hits)
	}
}
