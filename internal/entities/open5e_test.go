package entities

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// open5eServer stubs the Open5e cross-endpoint search and the per-object fetch
// endpoints against in-memory fixtures, so the resolver is exercised end to end
// without hitting the public API.
func open5eServer(t *testing.T, searchResults map[string]string, objects map[string]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/search/", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		body, ok := searchResults[q]
		if !ok {
			body = `{"count":0,"results":[]}`
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Object fetch: path like /v2/spells/srd-2024_fireball/
		body, ok := objects[strings.Trim(r.URL.Path, "/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

const fireballSearch = `{"count":2,"results":[
  {"document":{"key":"a5e-ag","name":"Adventurer's Guide"},"object_pk":"a5e-ag_fireball","object_name":"Fireball","object":{"school":"Evocation","level":3},"object_model":"Spell","route":"v2/spells/","text":"Fireball\n\nA community-source fireball.","match_type":"exact","match_score":1.0},
  {"document":{"key":"srd-2024","name":"System Reference Document 5.2"},"object_pk":"srd-2024_fireball","object_name":"Fireball","object":{"school":"Evocation","level":3},"object_model":"Spell","route":"v2/spells/","text":"Fireball\n\nA bright streak flashes from you to a point within range.","match_type":"exact","match_score":1.0}
]}`

const fireballObject = `{
  "name":"Fireball","document":{"key":"srd-2024","name":"System Reference Document 5.2"},
  "level":3,"school":{"name":"Evocation","key":"evocation"},
  "desc":"A bright streak flashes from you to a point you choose within range and then blossoms with a fiery explosion. Each creature in a 20-foot-radius Sphere makes a Dexterity saving throw, taking 8d6 Fire damage on a failed save.",
  "higher_level":"The damage increases by 1d6 for each spell slot level above 3.",
  "range_text":"150 feet","casting_time":"action","duration":"instantaneous",
  "classes":[{"name":"Sorcerer","key":"srd-2024_sorcerer"},{"name":"Wizard","key":"srd-2024_wizard"}]
}`

func TestOpen5e_ResolveSpell(t *testing.T) {
	srv := open5eServer(t,
		map[string]string{"Fireball": fireballSearch},
		map[string]string{"v2/spells/srd-2024_fireball": fireballObject},
	)
	r := NewWithBase(srv.URL)

	entities, unresolved, err := r.Resolve(context.Background(), `What does "Fireball" do?`)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(unresolved) != 0 {
		t.Errorf("unresolved = %v, want none", unresolved)
	}
	if len(entities) != 1 {
		t.Fatalf("resolved %d entities, want 1: %+v", len(entities), entities)
	}
	e := entities[0]
	if e.Name != "Fireball" {
		t.Errorf("name = %q, want Fireball", e.Name)
	}
	if e.Kind != "spell" {
		t.Errorf("kind = %q, want spell", e.Kind)
	}
	for _, want := range []string{"Level 3", "Evocation", "150 feet", "8d6 Fire damage", "At Higher Levels"} {
		if !strings.Contains(e.Body, want) {
			t.Errorf("body missing %q: %q", want, e.Body)
		}
	}
}

// TestOpen5e_PrefersSRDOverCommunity confirms the cross-endpoint search, which
// spans community documents, is filtered to the SRD — the 2024 SRD wins over an
// exact-match community hit with the same score.
func TestOpen5e_PrefersSRDOverCommunity(t *testing.T) {
	var fetchedPath string
	srv := open5eServer(t,
		map[string]string{"Fireball": fireballSearch},
		map[string]string{"v2/spells/srd-2024_fireball": fireballObject},
	)
	// Wrap to observe which object the resolver fetched.
	orig := srv.Config.Handler
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v2/spells/") {
			fetchedPath = strings.Trim(r.URL.Path, "/")
		}
		orig.ServeHTTP(w, r)
	})

	r := NewWithBase(srv.URL)
	entities, _, err := r.Resolve(context.Background(), `What does "Fireball" do?`)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(entities) != 1 || entities[0].Name != "Fireball" {
		t.Fatalf("resolved %+v", entities)
	}
	if fetchedPath != "v2/spells/srd-2024_fireball" {
		t.Errorf("fetched %q, want the SRD object v2/spells/srd-2024_fireball", fetchedPath)
	}
}

func TestOpen5e_ReportsUnresolved(t *testing.T) {
	srv := open5eServer(t, map[string]string{}, map[string]string{})
	r := NewWithBase(srv.URL)

	entities, unresolved, err := r.Resolve(context.Background(), `What does "Not A Real Spell" do?`)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(entities) != 0 {
		t.Errorf("resolved %v, want none for a miss", entities)
	}
	if len(unresolved) == 0 {
		t.Errorf("a multi-word miss must be reported unresolved, got %v", unresolved)
	}
}

// TestOpen5e_FallsBackToSearchText confirms that when the richer object fetch
// is unavailable, the resolver still grounds on the search text (authoritative
// SRD content) instead of reporting a miss.
func TestOpen5e_FallsBackToSearchText(t *testing.T) {
	srv := open5eServer(t,
		map[string]string{"Fireball": fireballSearch},
		map[string]string{}, // no object fixtures -> object fetch 404s
	)
	r := NewWithBase(srv.URL)

	entities, unresolved, err := r.Resolve(context.Background(), `What does "Fireball" do?`)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(unresolved) != 0 {
		t.Errorf("unresolved = %v, want none (search text should ground it)", unresolved)
	}
	if len(entities) != 1 || entities[0].Name != "Fireball" {
		t.Fatalf("resolved %+v, want Fireball from search text", entities)
	}
	if !strings.Contains(entities[0].Body, "bright streak flashes from you") {
		t.Errorf("fallback body should be the SRD search text: %q", entities[0].Body)
	}
}

func TestOpen5e_CreatureFormatting(t *testing.T) {
	search := `{"count":1,"results":[
	  {"document":{"key":"srd-2024","name":"SRD"},"object_pk":"srd-2024_owlbear","object_name":"Owlbear","object_model":"Creature","route":"v2/creatures/","text":"Owlbear","match_type":"exact","match_score":1.0}
	]}`
	obj := `{
	  "name":"Owlbear","document":{"key":"srd-2024","name":"SRD"},
	  "size":{"name":"Large","key":"large"},"type":{"name":"Monstrosity","key":"monstrosity"},
	  "hit_points":59,"armor_class":13,"challenge_rating":3.0,
	  "speed":{"walk":40,"climb":40,"unit":"feet"},
	  "actions":[{"name":"Rend","desc":"Melee Attack Roll: +7, reach 5 ft. 14 (2d8 + 5) Slashing damage."}]
	}`
	srv := open5eServer(t,
		map[string]string{"Owlbear": search},
		map[string]string{"v2/creatures/srd-2024_owlbear": obj},
	)
	r := NewWithBase(srv.URL)

	entities, _, err := r.Resolve(context.Background(), `What does an Owlbear do?`)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(entities) != 1 {
		t.Fatalf("resolved %d, want 1", len(entities))
	}
	e := entities[0]
	if e.Kind != "creature" {
		t.Errorf("kind = %q, want creature", e.Kind)
	}
	for _, want := range []string{"Large Monstrosity", "CR 3", "HP 59", "AC 13", "Rend", "Slashing damage"} {
		if !strings.Contains(e.Body, want) {
			t.Errorf("creature body missing %q: %q", want, e.Body)
		}
	}
}

// TestOpen5e_CachesAfterFirstResolve confirms a repeat mention is served from
// cache (no second HTTP round-trip), matching the cards/rulings discipline.
func TestOpen5e_CachesAfterFirstResolve(t *testing.T) {
	hits := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/search/", func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(fireballSearch))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(fireballObject))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	r := NewWithBase(srv.URL)
	if _, _, err := r.Resolve(context.Background(), `What does "Fireball" do?`); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	firstHits := hits
	if _, _, err := r.Resolve(context.Background(), `What does "Fireball" do?`); err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if hits != firstHits {
		t.Errorf("second resolve hit the search endpoint %d times, want 0 (cached)", hits-firstHits)
	}
}

func TestOpen5e_NilSafe(t *testing.T) {
	var r *Open5e
	entities, unresolved, err := r.Resolve(context.Background(), `What does "Fireball" do?`)
	if err != nil || len(entities) != 0 || len(unresolved) != 0 {
		t.Errorf("nil resolver should resolve nothing, got entities=%v unresolved=%v err=%v", entities, unresolved, err)
	}
}
