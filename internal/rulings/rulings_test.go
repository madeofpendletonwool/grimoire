package rulings

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const namedJSON = `{
	"id": "3a1d0dad-18a8-489e-ac11-08f64b72fda4",
	"oracle_id": "afa49a09-146f-4439-850e-dd1938c93cef",
	"name": "Derevi, Empyrial Tactician"
}`

const rulingsJSON = `{
	"object": "list",
	"has_more": false,
	"data": [
		{
			"object": "ruling",
			"oracle_id": "afa49a09-146f-4439-850e-dd1938c93cef",
			"source": "wotc",
			"published_at": "2020-11-10",
			"comment": "You can activate Derevi's last ability only when it is in the command zone."
		},
		{
			"object": "ruling",
			"oracle_id": "afa49a09-146f-4439-850e-dd1938c93cef",
			"source": "scryfall",
			"published_at": "2015-01-19",
			"comment": "Derevi is banned as a commander in Duel Commander."
		}
	]
}`

// newTestService wires a Service at a stub server. The throttle is made a
// no-op so tests don't sleep for the rate-limit interval (mirrors cards tests).
func newTestService(t *testing.T, fn func(w http.ResponseWriter, r *http.Request)) *Service {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(fn))
	t.Cleanup(srv.Close)
	s := NewWithBase(srv.URL)
	s.lastReq = time.Now().Add(-time.Hour)
	return s
}

func TestFetch_Success(t *testing.T) {
	var namedHits, rulingsHits int
	s := newTestService(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/cards/named" && r.URL.Query().Get("fuzzy") == "Derevi":
			namedHits++
			w.Header().Set("content-type", "application/json")
			_, _ = w.Write([]byte(namedJSON))
		case r.URL.Path == "/cards/3a1d0dad-18a8-489e-ac11-08f64b72fda4/rulings":
			rulingsHits++
			w.Header().Set("content-type", "application/json")
			_, _ = w.Write([]byte(rulingsJSON))
		default:
			http.NotFound(w, r)
		}
	})

	out, err := s.Fetch(context.Background(), "Derevi")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("rulings = %d, want 2", len(out))
	}
	if out[0].Source != "wotc" || out[0].PublishedAt != "2020-11-10" {
		t.Errorf("first ruling = %+v", out[0])
	}
	if out[0].Comment == "" {
		t.Errorf("ruling comment should not be empty")
	}

	// Second call is served entirely from cache — neither endpoint is hit again.
	out2, err := s.Fetch(context.Background(), "derevi")
	if err != nil {
		t.Fatalf("cached fetch: %v", err)
	}
	if len(out2) != len(out) {
		t.Errorf("cached rulings = %d, want %d", len(out2), len(out))
	}
	if namedHits != 1 || rulingsHits != 1 {
		t.Errorf("expected one hit per endpoint, got named=%d rulings=%d", namedHits, rulingsHits)
	}
}

func TestFetch_EmptyName(t *testing.T) {
	s := newTestService(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream should not be hit on empty name")
	})
	if _, err := s.Fetch(context.Background(), "   "); err != ErrNotFound {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestFetch_CardNotFound(t *testing.T) {
	s := newTestService(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"details":"No card found"}`))
	})
	if _, err := s.Fetch(context.Background(), "zzz not a card"); err != ErrNotFound {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestFetch_AmbiguousName(t *testing.T) {
	// Scryfall signals ambiguity with a 400 + details — same as cards lookup.
	s := newTestService(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"details":"Too many cards match ambiguous"}`))
	})
	if _, err := s.Fetch(context.Background(), "bolt"); err != ErrNotFound {
		t.Errorf("ambiguous should map to ErrNotFound, got %v", err)
	}
}

func TestFetch_NoRulings(t *testing.T) {
	// A real card with zero rulings is a normal empty outcome, not an error:
	// the model and UI should see "no rulings" rather than a lookup failure.
	hits := 0
	s := newTestService(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cards/named":
			hits++
			w.Header().Set("content-type", "application/json")
			_, _ = w.Write([]byte(`{"id":"abc-123","name":"Vanilla Creature"}`))
		case "/cards/abc-123/rulings":
			hits++
			w.Header().Set("content-type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","has_more":false,"data":[]}`))
		default:
			http.NotFound(w, r)
		}
	})
	out, err := s.Fetch(context.Background(), "Vanilla Creature")
	if err != nil {
		t.Fatalf("no-rulings card: err = %v, want nil", err)
	}
	if len(out) != 0 {
		t.Errorf("rulings = %d, want 0", len(out))
	}

	// The empty result is cached: a second fetch hits no endpoint.
	out2, err := s.Fetch(context.Background(), "vanilla creature")
	if err != nil {
		t.Fatalf("cached fetch: %v", err)
	}
	if len(out2) != 0 {
		t.Errorf("cached rulings = %d, want 0", len(out2))
	}
	if hits != 2 {
		t.Errorf("expected 2 upstream hits then cache, got %d", hits)
	}
}

func TestFetch_RulingsEndpointNotFound(t *testing.T) {
	// The card resolves but the rulings endpoint 404s — treat as no rulings
	// rather than surfacing a hard error to the reader.
	s := newTestService(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cards/named":
			w.Header().Set("content-type", "application/json")
			_, _ = w.Write([]byte(`{"id":"abc-123","name":"Some Card"}`))
		case "/cards/abc-123/rulings":
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	})
	out, err := s.Fetch(context.Background(), "Some Card")
	if err != nil {
		t.Fatalf("rulings 404 should yield nil err, got %v", err)
	}
	if out != nil {
		t.Errorf("rulings = %v, want nil", out)
	}
}
