package items

// The magic-item catalog's tests (MAD-383): the sync's scoping and
// preference rules, the restart, the derived tags asserted against
// hand-checked items at every rarity, the filter's gates and nudges, and
// the nil-catalog safety every read path owes.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/index"
)

// catalogFixture serves two pages of SRD magic items plus one community
// entry and one duplicate from an older SRD document, so the sync's
// scoping and preference rules are both exercised. The items span every
// rarity — Common to Legendary — because the tags and the rarity bands
// are asserted against hand-checked items across the shelf.
func catalogFixture(t *testing.T) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/magicitems/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("content-type", "application/json")
		if r.URL.Query().Get("page") == "2" {
			fmt.Fprint(w, `{"next":"","results":[
				{"key":"ring-of-evasion","name":"Ring of Evasion","document":{"key":"srd-2024"},
				 "category":{"name":"Ring","key":"ring"},"rarity":{"name":"Very Rare","rank":3},
				 "requires_attunement":true,
				 "desc":"While wearing this ring, you have advantage on Dexterity saving throws against effects that you can see."},
				{"key":"staff-of-power","name":"Staff of Power","document":{"key":"srd-2024"},
				 "category":{"name":"Staff","key":"staff"},"rarity":{"name":"Legendary","rank":4},
				 "requires_attunement":true,"attunement_detail":"by a wizard, sorcerer, or warlock",
				 "desc":"This staff can be wielded as a magic quarterstaff, granting a +2 bonus to attack and damage rolls made with it. You gain a +2 bonus to Armor Class, and a +2 bonus to saving throws. The staff has 20 charges. It regains 2d8 + 4 expended charges daily at dawn."},
				{"key":"weapon-plus-three","name":"Weapon +3","document":{"key":"srd-2024"},
				 "category":{"name":"Weapon","key":"weapon"},"rarity":{"name":"Legendary","rank":4},
				 "requires_attunement":true,
				 "desc":"You have a +3 bonus to attack and damage rolls made with this magic weapon."},
				{"key":"cloak-of-protection","name":"Cloak of Protection","document":{"key":"srd-2014"},
				 "category":{"name":"Wondrous Item","key":"wondrous-item"},"rarity":{"name":"Uncommon","rank":1},
				 "requires_attunement":true,
				 "desc":"You gain a +1 bonus to Armor Class and saving throws while you wear this cloak."},
				{"key":"cc-gribblys-gizmo","name":"Gribbly's Gizmo","document":{"key":"cc"},
				 "category":{"name":"Wondrous Item","key":"wondrous-item"},"rarity":{"name":"Rare","rank":2},
				 "desc":"A community oddity that must never reach the shelf."}
			]}`)
			return
		}
		fmt.Fprintf(w, `{"next":%q,"results":[
			{"key":"potion-of-healing","name":"Potion of Healing","document":{"key":"srd-2024"},
			 "category":{"name":"Potion","key":"potion"},"rarity":{"name":"Common","rank":0},
			 "desc":"You regain 2d4 + 2 hit points when you drink this potion. The potion's red liquid glimmers when agitated."},
			{"key":"cloak-of-protection","name":"Cloak of Protection","document":{"key":"srd-2024"},
			 "category":{"name":"Wondrous Item","key":"wondrous-item"},"rarity":{"name":"Uncommon","rank":1},
			 "requires_attunement":true,
			 "desc":"You gain a +1 bonus to Armor Class and saving throws while you wear this cloak."},
			{"key":"dagger-of-venom","name":"Dagger of Venom","document":{"key":"srd-2024"},
			 "category":{"name":"Weapon","key":"weapon"},"rarity":{"name":"Rare","rank":2},
			 "requires_attunement":true,
			 "desc":"You gain a +1 bonus to attack and damage rolls made with this magic weapon. You can use a bonus action to coat the blade. The next time you hit, the target must succeed on a DC 13 Constitution saving throw, taking 2d10 poison damage on a failed save."},
			{"key":"flame-tongue","name":"Flame Tongue","document":{"key":"srd-2024"},
			 "category":{"name":"Weapon","key":"weapon"},"rarity":{"name":"Rare","rank":2},
			 "requires_attunement":true,
			 "desc":"While holding this magic sword, you can use a bonus action to set it ablaze. When you hit with it, the target takes an extra 2d6 fire damage."}
		]}`, srv.URL+"/v2/magicitems/?page=2")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newCatalog(t *testing.T, baseURL string) (*Catalog, *index.Store) {
	t.Helper()
	store, err := index.Open(filepath.Join(t.TempDir(), "items.db"))
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
	if cat.Count() != 7 {
		t.Fatalf("count = %d, want 7 SRD items (the community oddity dropped)", cat.Count())
	}
	if _, ok := cat.Lookup("Gribbly's Gizmo", nil); ok {
		t.Error("a community-document item reached the catalog")
	}
	if cat.Stale() {
		t.Error("a freshly synced catalog must not be stale")
	}

	// The duplicate Cloak of Protection must resolve to the preferred SRD
	// document.
	c, ok := cat.Lookup("cloak of protection", nil)
	if !ok {
		t.Fatal("cloak missing after sync")
	}
	if c.Doc != "srd-2024" {
		t.Errorf("cloak came from %q, want the preferred srd-2024", c.Doc)
	}

	// Charges are parsed from the item's own text or not at all.
	s := cat.byKey[squash("Staff of Power")]
	staff := cat.list[s]
	if staff.Charges != 20 || staff.Recharge != "2d8+4 daily at dawn" {
		t.Errorf("staff charges = %d recharge = %q, want 20 and \"2d8+4 daily at dawn\"", staff.Charges, staff.Recharge)
	}
	if staff.AttunementCondition != "by a wizard, sorcerer, or warlock" {
		t.Errorf("staff attunement condition = %q", staff.AttunementCondition)
	}

	// The mirror survives a restart without touching the network.
	reloaded, err := NewCatalog(store.DB(), srv.URL)
	if err != nil {
		t.Fatalf("reopen catalog: %v", err)
	}
	if err := reloaded.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if reloaded.Count() != 7 {
		t.Fatalf("reloaded count = %d, want 7", reloaded.Count())
	}
}

// Tags are the vocabulary the filter, the rarity bands and the
// nearest-neighbour read share, and every one of them has to come from
// something the item's category or its own text says. These are hand
// checked against one item at every rarity.
func TestCatalogDerivesTags(t *testing.T) {
	srv := catalogFixture(t)
	cat, _ := newCatalog(t, srv.URL)
	if err := cat.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}

	cases := []struct {
		rarity string
		name   string
		want   []string
		not    []string
	}{
		{"Common", "Potion of Healing", []string{"consumable"}, []string{"weapon", "offensive", "defensive"}},
		{"Uncommon", "Cloak of Protection", []string{"defensive", "save-boost"}, []string{"weapon", "consumable", "offensive"}},
		{"Rare", "Dagger of Venom", []string{"weapon", "offensive"}, []string{"damage-rider", "consumable", "save-boost"}},
		{"Rare", "Flame Tongue", []string{"weapon", "damage-rider", "offensive"}, []string{"consumable", "defensive"}},
		{"Very Rare", "Ring of Evasion", []string{"save-boost"}, []string{"weapon", "armor", "consumable", "offensive"}},
		{"Legendary", "Staff of Power", []string{"defensive", "save-boost"}, []string{"consumable", "damage-rider"}},
		{"Legendary", "Weapon +3", []string{"weapon"}, []string{"armor", "consumable"}},
	}
	for _, tc := range cases {
		it, ok := cat.Lookup(tc.name, nil)
		if !ok {
			t.Fatalf("%s missing", tc.name)
		}
		if it.Rarity != tc.rarity {
			t.Errorf("%s rarity = %q, want %q (fixture drifted)", tc.name, it.Rarity, tc.rarity)
		}
		for _, want := range tc.want {
			if !hasTag(it.Tags, want) {
				t.Errorf("%s tags %v, want %q", tc.name, it.Tags, want)
			}
		}
		for _, no := range tc.not {
			if hasTag(it.Tags, no) {
				t.Errorf("%s tags %v, must not carry %q", tc.name, it.Tags, no)
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

	// A type is a gate, not a nudge.
	weapons := cat.Filter(Filter{Types: []string{"weapon"}}, nil)
	if len(weapons) != 3 {
		t.Errorf("weapon gate = %d items, want the 3 weapons", len(weapons))
	}
	for _, w := range weapons {
		if !strings.EqualFold(w.Type, "weapon") {
			t.Errorf("%s escaped the weapon gate", w.Name)
		}
	}

	// So is a rarity.
	legends := cat.Filter(Filter{Rarities: []string{"legendary"}}, nil)
	if len(legends) != 2 {
		t.Errorf("legendary gate = %d items, want 2", len(legends))
	}

	// A tag is a nudge, not a gate: every hit carries it, but things
	// without it still surface.
	nudged := cat.Filter(Filter{Tags: []string{"damage-rider"}}, nil)
	if len(nudged) == 0 || !hasTag(nudged[0].Tags, "damage-rider") {
		t.Errorf("tag nudge = %v, want the rider first", names(nudged))
	}

	// The DM's own words outrank everything else.
	ranked := cat.Filter(Filter{Terms: []string{"flame"}}, nil)
	if len(ranked) == 0 || ranked[0].Name != "Flame Tongue" {
		t.Errorf("term ranking = %v, want Flame Tongue first", names(ranked))
	}

	// Exclusions keep a loot pass from offering the same thing twice.
	excluded := cat.Filter(Filter{Types: []string{"weapon"}, Exclude: map[string]bool{squash("Flame Tongue"): true}}, nil)
	for _, w := range excluded {
		if w.Name == "Flame Tongue" {
			t.Error("an excluded item was offered again")
		}
	}
}

func TestCatalogSearchAndLookupTolerateNoise(t *testing.T) {
	srv := catalogFixture(t)
	cat, _ := newCatalog(t, srv.URL)
	if err := cat.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}

	if hits := cat.Search("flame", 0, nil); len(hits) == 0 || hits[0].Name != "Flame Tongue" {
		t.Errorf("search = %v, want Flame Tongue", names(hits))
	}
	// A brief writing plurals or odd punctuation must still resolve.
	for _, written := range []string{"Potions of Healing", "potion of healing", "POTION OF HEALING"} {
		if _, ok := cat.Lookup(written, nil); !ok {
			t.Errorf("lookup(%q) missed", written)
		}
	}
	if _, ok := cat.Lookup("Orb of Nowhere", nil); ok {
		t.Error("an invented item resolved")
	}
}

// A nil catalog is the state of an install whose mirror never ran; every
// read path has to survive it rather than panic inside a request.
func TestNilCatalogIsSafe(t *testing.T) {
	var cat *Catalog
	if cat.Count() != 0 || cat.Stale() || len(cat.Filter(Filter{}, nil)) != 0 ||
		len(cat.Search("potion", 0, nil)) != 0 || len(cat.All()) != 0 || len(cat.Types()) != 0 {
		t.Fatal("nil catalog did not degrade quietly")
	}
	if _, ok := cat.Lookup("Potion of Healing", nil); ok {
		t.Fatal("nil catalog resolved a name")
	}
	if err := cat.EnsureFresh(context.Background()); err != nil {
		t.Fatalf("nil EnsureFresh: %v", err)
	}
}

func names(list []Item) []string {
	out := make([]string, 0, len(list))
	for _, it := range list {
		out = append(out, it.Name)
	}
	return out
}
