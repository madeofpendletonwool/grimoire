package encounter

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/index"
)

func timeZero() time.Time { return time.Time{} }

// catalogFixture serves two pages of SRD creatures plus one community entry
// and one duplicate from an older SRD document, so the sync's scoping and
// preference rules are both exercised.
func catalogFixture(t *testing.T) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/creatures/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("content-type", "application/json")
		if r.URL.Query().Get("page") == "2" {
			fmt.Fprint(w, `{"next":"","results":[
				{"key":"srd_goblin","name":"Goblin","document":{"key":"srd-2014"},
				 "challenge_rating":0.25,"type":{"name":"Humanoid"},"armor_class":15,"hit_points":7},
				{"key":"cc_gribbly","name":"Gribbly","document":{"key":"cc"},
				 "challenge_rating":2,"type":{"name":"Aberration"}}
			]}`)
			return
		}
		fmt.Fprintf(w, `{"next":%q,"results":[
			{"key":"srd-2024_goblin","name":"Goblin","document":{"key":"srd-2024"},
			 "challenge_rating":0.25,"type":{"name":"Humanoid"},"size":{"name":"Small"},
			 "armor_class":15,"hit_points":7,"hit_dice":"2d6",
			 "speed_all":{"walk":30,"unit":"feet","hover":false},
			 "darkvision_range":60,"passive_perception":9,
			 "actions":[{"name":"Shortbow","desc":"Ranged Weapon Attack: +4 to hit. Hit: 5 (1d6+2) piercing damage.","action_type":"ACTION"}],
			 "traits":[{"name":"Nimble Escape","desc":"The goblin takes the Disengage or Hide action as a bonus action."}]},
			{"key":"srd-2024_young-black-dragon","name":"Young Black Dragon","document":{"key":"srd-2024"},
			 "challenge_rating":7,"type":{"name":"Dragon"},"size":{"name":"Large"},
			 "armor_class":18,"hit_points":127,
			 "speed_all":{"walk":40,"fly":80,"swim":40,"unit":"feet"},
			 "blindsight_range":30,"darkvision_range":120,
			 "resistances_and_immunities":{"damage_immunities_display":"acid"},
			 "actions":[{"name":"Acid Breath","desc":"The dragon exhales acid in a 30-foot line. Each creature in a line takes 49 (11d8) acid damage.","action_type":"ACTION"}]},
			{"key":"srd-2024_lich","name":"Lich","document":{"key":"srd-2024"},
			 "challenge_rating":21,"type":{"name":"Undead"},"size":{"name":"Medium"},
			 "armor_class":17,"hit_points":135,"speed_all":{"walk":30,"unit":"feet"},
			 "traits":[{"name":"Spellcasting","desc":"The lich casts the following spells."}],
			 "actions":[{"name":"Frightening Gaze","desc":"The target has the Frightened condition.","action_type":"ACTION"},
			            {"name":"Cantrip","desc":"The lich casts a cantrip.","action_type":"LEGENDARY_ACTION"}]}
		]}`, srv.URL+"/v2/creatures/?page=2")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newCatalog(t *testing.T, baseURL string) (*Catalog, *index.Store) {
	t.Helper()
	store, err := index.Open(filepath.Join(t.TempDir(), "bestiary.db"))
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cat, err := NewCatalog(store.DB(), baseURL)
	if err != nil {
		t.Fatalf("new catalog: %v", err)
	}
	return cat, store
}

func TestCatalogSyncScopesAndPages(t *testing.T) {
	srv := catalogFixture(t)
	cat, store := newCatalog(t, srv.URL)

	if !cat.Stale() {
		t.Fatal("an empty catalog must report stale")
	}
	if err := cat.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if cat.Count() != 3 {
		t.Fatalf("count = %d, want 3 SRD creatures (community entries dropped)", cat.Count())
	}
	if _, ok := cat.Lookup("Gribbly"); ok {
		t.Error("a community-document creature reached the catalog")
	}
	if cat.Stale() {
		t.Error("a freshly synced catalog must not be stale")
	}

	// The duplicate Goblin must resolve to the preferred SRD document.
	g, ok := cat.Lookup("goblin")
	if !ok {
		t.Fatal("goblin missing after sync")
	}
	if g.Doc != "srd-2024" {
		t.Errorf("goblin came from %q, want the preferred srd-2024", g.Doc)
	}
	if g.XP != 50 {
		t.Errorf("goblin XP = %d, want the Monster Manual's 50 for CR 1/4", g.XP)
	}

	// The mirror survives a restart without touching the network.
	reloaded, err := NewCatalog(store.DB(), srv.URL)
	if err != nil {
		t.Fatalf("reopen catalog: %v", err)
	}
	if err := reloaded.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if reloaded.Count() != 3 {
		t.Fatalf("reloaded count = %d, want 3", reloaded.Count())
	}
}

// Tags are the vocabulary the filter and the prompt share, and every one of
// them has to come from something the statblock actually says.
func TestCatalogDerivesTags(t *testing.T) {
	srv := catalogFixture(t)
	cat, _ := newCatalog(t, srv.URL)
	if err := cat.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}

	cases := []struct {
		name string
		want []string
		not  []string
	}{
		{"Young Black Dragon", []string{"flying", "aquatic", "acid", "aoe", "blindsight", "darkvision", "damage-immune", "elite"}, []string{"legendary"}},
		{"Lich", []string{"spellcaster", "frightening", "legendary", "solo", "boss"}, []string{"flying", "aquatic"}},
		{"Goblin", []string{"ranged", "ambusher", "darkvision", "minion"}, []string{"legendary", "flying"}},
	}
	for _, tc := range cases {
		c, ok := cat.Lookup(tc.name)
		if !ok {
			t.Fatalf("%s missing", tc.name)
		}
		for _, want := range tc.want {
			if !hasTag(c.Tags, want) {
				t.Errorf("%s tags %v, want %q", tc.name, c.Tags, want)
			}
		}
		for _, no := range tc.not {
			if hasTag(c.Tags, no) {
				t.Errorf("%s tags %v, must not carry %q", tc.name, c.Tags, no)
			}
		}
	}
}

func TestCatalogFilter(t *testing.T) {
	srv := catalogFixture(t)
	cat, _ := newCatalog(t, srv.URL)
	if err := cat.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}

	// A challenge-rating window is a hard bound: nothing outside it may ever
	// reach the model, because the budget cannot pay for it.
	for _, c := range cat.Filter(Filter{MinCR: 1, MaxCR: 10}) {
		if c.CRNum < 1 || c.CRNum > 10 {
			t.Errorf("%s (CR %s) escaped the window", c.Name, c.CR)
		}
	}

	// A type is a gate, not a nudge.
	got := cat.Filter(Filter{MaxCR: 30, Types: []string{"dragon"}})
	if len(got) != 1 || got[0].Name != "Young Black Dragon" {
		t.Errorf("type gate = %+v, want only the dragon", names(got))
	}

	// The DM's own words outrank everything else.
	ranked := cat.Filter(Filter{MaxCR: 30, Terms: []string{"lich"}})
	if len(ranked) == 0 || ranked[0].Name != "Lich" {
		t.Errorf("term ranking = %v, want Lich first", names(ranked))
	}

	// Exclusions keep a revision from re-proposing what is already fielded.
	excluded := cat.Filter(Filter{MaxCR: 30, Exclude: map[string]bool{"lich": true}})
	for _, c := range excluded {
		if c.Name == "Lich" {
			t.Error("an excluded creature was offered again")
		}
	}
}

func TestCatalogSearchAndLookupTolerateNoise(t *testing.T) {
	srv := catalogFixture(t)
	cat, _ := newCatalog(t, srv.URL)
	if err := cat.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}

	if hits := cat.Search("gob", 0); len(hits) == 0 || hits[0].Name != "Goblin" {
		t.Errorf("search = %+v, want Goblin", hits)
	}
	// A model writing a plural or odd punctuation must still resolve.
	for _, written := range []string{"Goblins", "goblin", "GOBLIN"} {
		if _, ok := cat.Lookup(written); !ok {
			t.Errorf("lookup(%q) missed", written)
		}
	}
	if _, ok := cat.Lookup("Shadow Wyrm of Nowhere"); ok {
		t.Error("an invented monster resolved")
	}
}

// A nil catalog is the state of an install whose mirror never ran; every read
// path has to survive it rather than panic inside a request.
func TestNilCatalogIsSafe(t *testing.T) {
	var cat *Catalog
	if cat.Count() != 0 || cat.Stale() || len(cat.Filter(Filter{})) != 0 || len(cat.Search("goblin", 0)) != 0 {
		t.Fatal("nil catalog did not degrade quietly")
	}
	if _, ok := cat.Lookup("Goblin"); ok {
		t.Fatal("nil catalog resolved a name")
	}
	if err := cat.EnsureFresh(context.Background()); err != nil {
		t.Fatalf("nil EnsureFresh: %v", err)
	}
}

func TestBuildPoolStaysInsideTheBudget(t *testing.T) {
	srv := catalogFixture(t)
	cat, _ := newCatalog(t, srv.URL)
	if err := cat.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}

	b := Plan([]int{3, 3, 3, 3}, BandMedium)
	pool := BuildPool(cat, b, ReadIdea(""), nil)
	soloCap := crValue(b.MaxSoloCR)
	for _, c := range pool.All() {
		if c.CRNum > soloCap {
			t.Errorf("%s (CR %s) is past the solo cap CR %s", c.Name, c.CR, b.MaxSoloCR)
		}
	}
	// Each statblock appears once across the tiers.
	seen := map[string]bool{}
	for _, c := range pool.All() {
		if seen[c.Name] {
			t.Errorf("%s offered in two tiers", c.Name)
		}
		seen[c.Name] = true
	}
}

// The flavour tier exists to honour the DM's words even when the budget would
// not: asking for a lich at 3rd level should still surface the lich, so the
// model can say why it is a bad idea rather than silently ignoring the brief.
func TestBuildPoolKeepsTheFlavourTheDMAskedFor(t *testing.T) {
	srv := catalogFixture(t)
	cat, _ := newCatalog(t, srv.URL)
	if err := cat.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	pool := BuildPool(cat, Plan([]int{3, 3, 3, 3}, BandMedium), ReadIdea("a lich in its tomb"), nil)
	found := false
	for _, c := range pool.Flavour {
		if c.Name == "Lich" {
			found = true
		}
	}
	if !found {
		t.Errorf("flavour tier = %v, want the Lich the DM named", names(pool.Flavour))
	}
}

func names(list []Creature) []string {
	out := make([]string, 0, len(list))
	for _, c := range list {
		out = append(out, c.Name)
	}
	return out
}
