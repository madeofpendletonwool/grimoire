package entities

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/cards"
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
	  "ability_scores":{"strength":20,"dexterity":12,"constitution":17,"intelligence":8,"wisdom":12,"charisma":10},
	  "saving_throws":{"wisdom":3},
	  "skill_bonuses":{"perception":5},
	  "passive_perception":15,"darkvision_range":60,
	  "languages":{"as_string":"None (understands Common and Elvish but can't speak)"},
	  "resistances_and_immunities":{"damage_immunities_display":"","damage_resistances_display":"","damage_vulnerabilities_display":"","condition_immunities_display":""},
	  "traits":[{"name":"Keen Sight and Smell","desc":"The owlbear has Advantage on Wisdom (Perception) checks that rely on sight or hearing."}],
	  "actions":[
	    {"name":"Rend","desc":"Melee Attack Roll: +7, reach 5 ft. 14 (2d8 + 5) Slashing damage.","action_type":"ACTION"},
	    {"name":"Redirect Attack","desc":"When a creature the owlbear can see attacks a target other than it, the owlbear makes a Rend attack against the attacker.","action_type":"REACTION"}
	  ]
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
	for _, want := range []string{
		"Large Monstrosity", "CR 3", "HP 59", "AC 13",
		"Abilities: Str 20 (+5)",
		"Saving Throws: Wis +3",
		"Skills: Perception +5",
		"Senses: darkvision 60 ft., passive Perception 15",
		"Languages: None (understands Common and Elvish",
		"Keen Sight and Smell",
		"Actions:", "Rend", "Slashing damage",
		"Reactions:", "Redirect Attack",
	} {
		if !strings.Contains(e.Body, want) {
			t.Errorf("creature body missing %q: %q", want, e.Body)
		}
	}
}

// TestOpen5e_LegendaryActions confirms legendary actions render under their
// own header with their action cost, so legendary-action questions ground in
// the full option list.
func TestOpen5e_LegendaryActions(t *testing.T) {
	search := `{"count":1,"results":[
	  {"document":{"key":"srd-2024","name":"SRD"},"object_pk":"srd-2024_adult-black-dragon","object_name":"Adult Black Dragon","object_model":"Creature","route":"v2/creatures/","text":"Adult Black Dragon","match_type":"exact","match_score":1.0}
	]}`
	obj := `{
	  "name":"Adult Black Dragon","document":{"key":"srd-2024","name":"SRD"},
	  "size":{"name":"Large","key":"large"},"type":{"name":"Dragon","key":"dragon"},
	  "hit_points":195,"armor_class":19,"challenge_rating":14.0,
	  "resistances_and_immunities":{"damage_immunities_display":"acid","damage_resistances_display":"","damage_vulnerabilities_display":"","condition_immunities_display":""},
	  "traits":[{"name":"Legendary Resistance (3/Day)","desc":"If the dragon fails a saving throw, it can choose to succeed instead."}],
	  "actions":[
	    {"name":"Rend","desc":"Melee Attack Roll: +11. 13 (2d6 + 6) Slashing damage plus 4 (1d8) Acid damage.","action_type":"ACTION"},
	    {"name":"Cloud of Insects","desc":"Dexterity Saving Throw: DC 17, one creature. Failure: 22 (4d10) Poison damage.","action_type":"LEGENDARY_ACTION","legendary_action_cost":1},
	    {"name":"Pounce","desc":"The dragon moves up to half its Speed and makes one Rend attack.","action_type":"LEGENDARY_ACTION","legendary_action_cost":2}
	  ]
	}`
	srv := open5eServer(t,
		map[string]string{"Adult Black Dragon": search},
		map[string]string{"v2/creatures/srd-2024_adult-black-dragon": obj},
	)
	r := NewWithBase(srv.URL)
	entities, _, err := r.Resolve(context.Background(), `Tell me about the Adult Black Dragon.`)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(entities) != 1 {
		t.Fatalf("resolved %d, want 1", len(entities))
	}
	body := entities[0].Body
	for _, want := range []string{
		"Damage Immunities: acid.",
		"Legendary Actions:",
		"Cloud of Insects",
		"Pounce (costs 2 actions)",
		"Legendary Resistance (3/Day)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("legendary body missing %q: %q", want, body)
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

// TestOpen5e_DictionaryDetectsLowercaseMentions confirms the SRD name
// dictionary tier: "hunter's mark" in lowercase, unquoted prose is detected
// only through the dictionary, the counterpart of MTG's MTGJSON tier.
func TestOpen5e_DictionaryDetectsLowercaseMentions(t *testing.T) {
	search := `{"count":1,"results":[
	  {"document":{"key":"srd-2024","name":"SRD"},"object_pk":"srd-2024_hunters-mark","object_name":"Hunter's Mark","object_model":"Spell","route":"v2/spells/","text":"Hunter's Mark","match_type":"exact","match_score":1.0}
	]}`
	obj := `{
	  "name":"Hunter's Mark","document":{"key":"srd-2024","name":"SRD"},
	  "level":1,"school":{"name":"Divination","key":"divination"},
	  "desc":"You mark a creature as your quarry.",
	  "concentration":true,
	  "range_text":"90 feet","casting_time":"bonus action","duration":"1 hour",
	  "classes":[{"name":"Ranger","key":"srd-2024_ranger"}]
	}`
	srv := open5eServer(t,
		map[string]string{"Hunter's Mark": search},
		map[string]string{"v2/spells/srd-2024_hunters-mark": obj},
	)

	t.Run("without dictionary", func(t *testing.T) {
		r := NewWithBase(srv.URL)
		entities, _, err := r.Resolve(context.Background(), `does hunter's mark stack with itself?`)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if len(entities) != 0 {
			t.Errorf("lowercase mention resolved without a dictionary: %+v", entities)
		}
	})

	t.Run("with dictionary", func(t *testing.T) {
		r := NewWithBase(srv.URL)
		r.SetDictionary(cards.NewDictionary([]string{"Hunter's Mark", "Fireball"}))
		entities, _, err := r.Resolve(context.Background(), `does hunter's mark stack with itself?`)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if len(entities) != 1 || entities[0].Name != "Hunter's Mark" {
			t.Fatalf("dictionary tier should resolve the lowercase mention, got %+v", entities)
		}
		if !strings.Contains(entities[0].Body, "Concentration: yes") {
			t.Errorf("concentration tag missing: %q", entities[0].Body)
		}
	})
}

func TestOpen5e_NilSafe(t *testing.T) {
	var r *Open5e
	entities, unresolved, err := r.Resolve(context.Background(), `What does "Fireball" do?`)
	if err != nil || len(entities) != 0 || len(unresolved) != 0 {
		t.Errorf("nil resolver should resolve nothing, got entities=%v unresolved=%v err=%v", entities, unresolved, err)
	}
}

// TestOpen5e_ResolvesCreatorPrefix confirms the Open5e search retry that
// drops a creator prefix. The 2024 SRD dropped the creator names from many
// spells ("Tenser's Floating Disk" -> "Floating Disk", "Leomund's Tiny Hut"
// -> "Tiny Hut"), and Open5e's search returns zero results for the prefixed
// name, so the resolver must strip the prefix and re-search to ground the
// canonical entry. Regression for MAD-141.
func TestOpen5e_ResolvesCreatorPrefix(t *testing.T) {
	cases := []struct {
		name      string
		question  string
		searchKey string // the prefix-stripped form the SRD match is filed under
		objectKey string
		spell     string
		search    string
		obj       string
	}{
		{
			name:      "tensers floating disk",
			question:  `What does Tenser's Floating Disk do?`,
			searchKey: "Floating Disk",
			objectKey: "v2/spells/srd-2024_floating-disk",
			spell:     "Floating Disk",
			search:    floatingDiskSearch,
			obj:       floatingDiskObject,
		},
		{
			name:      "leomunds tiny hut",
			question:  `What does "Leomund's Tiny Hut" do?`,
			searchKey: "Tiny Hut",
			objectKey: "v2/spells/srd-2024_tiny-hut",
			spell:     "Tiny Hut",
			search:    tinyHutSearch,
			obj:       tinyHutObject,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := open5eServer(t,
				map[string]string{c.searchKey: c.search},
				map[string]string{c.objectKey: c.obj},
			)
			r := NewWithBase(srv.URL)
			entities, unresolved, err := r.Resolve(context.Background(), c.question)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if len(unresolved) != 0 {
				t.Errorf("unresolved = %v, want none (prefix should strip to %q)", unresolved, c.spell)
			}
			if len(entities) != 1 || entities[0].Name != c.spell {
				t.Fatalf("resolved %+v, want %q", entities, c.spell)
			}
		})
	}
}

// TestOpen5e_StripsOnlyThePrefixThatYieldsAHit confirms the resolver does not
// over-strip: "Melf's Acid Arrow" must resolve via "Acid Arrow" (the
// two-word suffix), and a single trailing word is never searched on its own.
// The object is keyed on the stripped form's object_pk so the fetch path is
// exercised too.
func TestOpen5e_StripsOnlyThePrefixThatYieldsAHit(t *testing.T) {
	queries := map[string]bool{}
	srv := open5eServer(t,
		map[string]string{"Acid Arrow": acidArrowSearch},
		map[string]string{"v2/spells/srd-2024_acid-arrow": acidArrowObject},
	)
	orig := srv.Config.Handler
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/search/" {
			queries[r.URL.Query().Get("query")] = true
		}
		orig.ServeHTTP(w, r)
	})
	r := NewWithBase(srv.URL)
	entities, unresolved, err := r.Resolve(context.Background(), `What does "Melf's Acid Arrow" do?`)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(unresolved) != 0 || len(entities) != 1 || entities[0].Name != "Acid Arrow" {
		t.Fatalf("resolved entities=%v unresolved=%v, want Acid Arrow", entities, unresolved)
	}
	if !queries["Melf's Acid Arrow"] {
		t.Errorf("verbatim name was not searched; queries=%v", queries)
	}
	if !queries["Acid Arrow"] {
		t.Errorf("prefix-stripped form was not searched; queries=%v", queries)
	}
	if queries["Arrow"] {
		t.Errorf("single-word suffix should not be searched (too noisy); queries=%v", queries)
	}
}

// TestOpen5e_FuzzyTypo confirms a pure typo with no exact match falls back to
// the strongest credible near-match. "Firebal" must ground as "Fireball", and
// "Delayed Blast Fireball" — also returned by the search — must be rejected so
// a fuzzy search cannot attach the wrong entity. Regression for MAD-141.
func TestOpen5e_FuzzyTypo(t *testing.T) {
	srv := open5eServer(t,
		map[string]string{"Firebal": fireballFuzzySearch},
		map[string]string{"v2/spells/srd-2024_fireball": fireballObject},
	)
	r := NewWithBase(srv.URL)
	entities, unresolved, err := r.Resolve(context.Background(), `What does "Firebal" do?`)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(unresolved) != 0 {
		t.Errorf("unresolved = %v, want none (Firebal should fuzzy-match Fireball)", unresolved)
	}
	if len(entities) != 1 || entities[0].Name != "Fireball" {
		t.Fatalf("resolved %+v, want Fireball (not Delayed Blast Fireball)", entities)
	}
}

const floatingDiskSearch = `{"count":1,"results":[
  {"document":{"key":"srd-2024","name":"System Reference Document 5.2"},"object_pk":"srd-2024_floating-disk","object_name":"Floating Disk","object_model":"Spell","route":"v2/spells/","text":"Floating Disk\n\nCreates a circular plane of force.","match_type":"exact","match_score":1.0}
]}`

const floatingDiskObject = `{
  "name":"Floating Disk","document":{"key":"srd-2024","name":"System Reference Document 5.2"},
  "level":1,"school":{"name":"Conjuration","key":"conjuration"},
  "desc":"This spell creates a 3-foot-diameter circular plane of force that floats 3 feet above the ground.",
  "range_text":"30 feet","casting_time":"1 minute","duration":"1 hour",
  "classes":[{"name":"Wizard","key":"srd-2024_wizard"}]
}`

const tinyHutSearch = `{"count":1,"results":[
  {"document":{"key":"srd-2024","name":"System Reference Document 5.2"},"object_pk":"srd-2024_tiny-hut","object_name":"Tiny Hut","object_model":"Spell","route":"v2/spells/","text":"Tiny Hut\n\nA 10-foot Emanation springs into existence.","match_type":"exact","match_score":1.0}
]}`

const tinyHutObject = `{
  "name":"Tiny Hut","document":{"key":"srd-2024","name":"System Reference Document 5.2"},
  "level":3,"school":{"name":"Evocation","key":"evocation"},
  "desc":"A 10-foot Emanation springs into existence around you. Creatures and objects inside when you cast it can pass through freely.",
  "range_text":"self","casting_time":"1 minute","duration":"8 hours",
  "classes":[{"name":"Bard","key":"srd-2024_bard"},{"name":"Wizard","key":"srd-2024_wizard"}]
}`

const acidArrowSearch = `{"count":1,"results":[
  {"document":{"key":"srd-2024","name":"System Reference Document 5.2"},"object_pk":"srd-2024_acid-arrow","object_name":"Acid Arrow","object_model":"Spell","route":"v2/spells/","text":"Acid Arrow\n\nA shimmering arrow.","match_type":"exact","match_score":1.0}
]}`

const acidArrowObject = `{
  "name":"Acid Arrow","document":{"key":"srd-2024","name":"System Reference Document 5.2"},
  "level":2,"school":{"name":"Evocation","key":"evocation"},
  "desc":"A shimmering arrow of acid leaps from your hand.",
  "range_text":"90 feet","casting_time":"action","duration":"instantaneous",
  "classes":[{"name":"Wizard","key":"srd-2024_wizard"}]
}`

// fireballFuzzySearch models Open5e's response to a typo: "Firebal" returns
// both the intended "Fireball" and the unrelated "Delayed Blast Fireball" as
// fuzzy hits with equal scores. Only the credible match may be accepted.
const fireballFuzzySearch = `{"count":2,"results":[
  {"document":{"key":"srd-2024","name":"System Reference Document 5.2"},"object_pk":"srd-2024_fireball","object_name":"Fireball","object_model":"Spell","route":"v2/spells/","text":"Fireball","match_type":"fuzzy","match_score":0.9333},
  {"document":{"key":"srd-2014","name":"SRD 5.1"},"object_pk":"srd-2014_delayed-blast-fireball","object_name":"Delayed Blast Fireball","object_model":"Spell","route":"v2/spells/","text":"Delayed Blast Fireball","match_type":"fuzzy","match_score":0.9333}
]}`
