package items

// The homebrew item shelf's tests (MAD-383): the save contract — no
// path past the structural rules, the DM's rarity echoed never judged —
// the scoping, the overlay shape, and the guarantee that a catalog sync
// can never destroy or absorb a designed item.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/testdb"
)

func newHomebrewStore(t *testing.T) *HomebrewStore {
	t.Helper()
	return NewHomebrewStore(testdb.Open(t))
}

func emberInput() HomebrewInput {
	d := flamebrand()
	return HomebrewInput{Name: d.Name, Design: &d, Notes: "Forged for the siege of Blackwater."}
}

func TestHomebrewSaveValidatesAndTags(t *testing.T) {
	s := newHomebrewStore(t)
	m, err := s.Save(context.Background(), "dm", "", emberInput())
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if m.Slug != "emberbrand" || m.Source != ItemDesigned {
		t.Errorf("saved = %q/%q, want the squashed slug and the designed source", m.Slug, m.Source)
	}
	if m.RequestedRarity != "" {
		t.Errorf("requested rarity = %q, want the DM's label untouched (empty here)", m.RequestedRarity)
	}
	// The tags are the server's derivation of what the design says.
	for _, want := range []string{"homebrew", "weapon", "damage-rider"} {
		if !hasTag(m.Tags, want) {
			t.Errorf("tags %v, want %q", m.Tags, want)
		}
	}

	// The rendered read shape is always distinguishable from SRD.
	it := m.Item()
	if !it.Homebrew || it.Doc != "homebrew" || !hasTag(it.Tags, "homebrew") {
		t.Errorf("rendered item = %+v, want the homebrew marking at every hop", it)
	}

	// A malformed design has no save path — bad attunement, an
	// ungrammatical recharge, and an effect with no game vocabulary are
	// all refused with the specific rule named.
	bad := emberInput()
	bad.Design.Attunement = Attunement{Required: false, Condition: "by a cleric"}
	if _, err := s.Save(context.Background(), "dm", "", bad); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "attunement condition") {
		t.Errorf("bad attunement err = %v, want ErrInvalid naming the rule", err)
	}
	bad = emberInput()
	bad.Design.Charges, bad.Design.Recharge = 7, "whenever"
	if _, err := s.Save(context.Background(), "dm", "", bad); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "charge recovery") {
		t.Errorf("ungrammatical recharge err = %v, want ErrInvalid naming the grammar", err)
	}
	bad = emberInput()
	bad.Design.Effects = []Effect{{Text: "The wielder simply knows things."}}
	if _, err := s.Save(context.Background(), "dm", "", bad); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "no game vocabulary") {
		t.Errorf("vocabulary-less effect err = %v, want ErrInvalid naming the vocabulary rule", err)
	}

	// A hand-entered design rides the same contract; a bogus source is a
	// 400 at the surface and an error here.
	hand := emberInput()
	hand.Name = "The Warden's Flint"
	hand.Design.Name = "The Warden's Flint"
	hand.Source = ItemHand
	if _, err := s.Save(context.Background(), "dm", "", hand); err != nil {
		t.Fatalf("hand save: %v", err)
	}
	bogus := emberInput()
	bogus.Source = "stolen"
	if _, err := s.Save(context.Background(), "dm", "", bogus); !errors.Is(err, ErrInvalid) {
		t.Errorf("bogus source err = %v, want ErrInvalid", err)
	}
}

func TestHomebrewUpdateReplacesAndRescopes(t *testing.T) {
	s := newHomebrewStore(t)
	ctx := context.Background()
	m, err := s.Save(ctx, "dm", "", emberInput())
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	upd := emberInput()
	upd.Design.Rarity = "Rare"
	upd.Notes = "Reforged after the siege."
	saved, err := s.Save(ctx, "dm", m.ID, upd)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if saved.ID != m.ID || saved.RequestedRarity != "Rare" || saved.Notes != "Reforged after the siege." {
		t.Errorf("update = %+v, want the replace applied to the same row", saved)
	}

	// A foreign owner's id is a 404 at the surface and ErrNotFound here.
	if _, err := s.Save(ctx, "someone-else", m.ID, emberInput()); !errors.Is(err, ErrNotFound) {
		t.Errorf("foreign update err = %v, want ErrNotFound", err)
	}
}

func TestHomebrewListScopesAndTheOverlayShape(t *testing.T) {
	s := newHomebrewStore(t)
	ctx := context.Background()

	campaign := emberInput()
	campaign.CampaignID = "camp-1"
	campaign.Name = "The Ashen Crown"
	if _, err := s.Save(ctx, "dm", "", campaign); err != nil {
		t.Fatalf("save campaign item: %v", err)
	}
	if _, err := s.Save(ctx, "dm", "", emberInput()); err != nil {
		t.Fatalf("save unscoped item: %v", err)
	}
	other := emberInput()
	other.Name = "Someone Else's Ring"
	if _, err := s.Save(ctx, "not-the-dm", "", other); err != nil {
		t.Fatalf("save foreign item: %v", err)
	}

	// The campaign view leads with the campaign's own, then the owner's
	// unscoped designs. A foreign owner's shelf never leaks in.
	list, err := s.List(ctx, "dm", "camp-1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 || list[0].Name != "The Ashen Crown" {
		names := make([]string, 0, len(list))
		for i := range list {
			names = append(names, list[i].Name)
		}
		t.Fatalf("list = %v, want the campaign's own first", names)
	}

	// The overlay renders every row in the catalog's read shape, marked.
	overlay, err := s.Overlay(ctx, "dm", "camp-1")
	if err != nil {
		t.Fatalf("overlay: %v", err)
	}
	if len(overlay) != 2 {
		t.Fatalf("overlay = %d, want 2", len(overlay))
	}
	for _, it := range overlay {
		if !it.Homebrew || it.Doc != "homebrew" || !hasTag(it.Tags, "homebrew") {
			t.Fatalf("overlay item = %+v, unmarked", it)
		}
	}
	o := NewOverlay(overlay)
	if _, ok := o.Lookup("the ashen crown"); !ok {
		t.Fatal("the overlay's lookup missed its own")
	}

	names, err := s.CampaignNames(ctx, "camp-1")
	if err != nil || len(names) != 1 || names[0] != "The Ashen Crown" {
		t.Errorf("campaign names = %v (%v), want the campaign's own", names, err)
	}
}

// A catalog sync replaces the SRD mirror wholesale — and the homebrew
// shelf survives it untouched, distinguishable everywhere it appears.
func TestHomebrewSurvivesCatalogSync(t *testing.T) {
	srv := catalogFixture(t)
	db := testdb.Open(t)
	cat, err := NewCatalog(db, srv.URL)
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	s := NewHomebrewStore(db)
	ctx := context.Background()

	if err := cat.Sync(ctx); err != nil {
		t.Fatalf("sync: %v", err)
	}
	// A homebrew item that deliberately reuses an SRD name: the DM's own
	// design is the more specific answer about their table, in their
	// scope only.
	healing := emberInput()
	healing.Name = "Potion of Healing"
	healing.Design.Name = "Potion of Healing"
	healing.Design.Type = "potion"
	healing.Design.Base = ""
	healing.Design.Bonus = 0
	healing.Design.Attunement = Attunement{}
	healing.Design.Effects = []Effect{{Text: "The drinker heals.", Spell: "cure wounds"}}
	if _, err := s.Save(ctx, "dm", "", healing); err != nil {
		t.Fatalf("save collision: %v", err)
	}
	if _, err := s.Save(ctx, "dm", "", emberInput()); err != nil {
		t.Fatalf("save emberbrand: %v", err)
	}

	overlayList, err := s.Overlay(ctx, "dm", "")
	if err != nil {
		t.Fatalf("overlay: %v", err)
	}
	overlay := NewOverlay(overlayList)

	// The sync replaces the mirror — the overlay survives it.
	if err := cat.Sync(ctx); err != nil {
		t.Fatalf("re-sync: %v", err)
	}
	if cat.Count() != 7 {
		t.Fatalf("mirror count = %d, want the same 7 SRD items", cat.Count())
	}
	h, ok := cat.Lookup("Emberbrand", overlay)
	if !ok || !h.Homebrew {
		t.Fatalf("the sync destroyed a homebrew item: %+v", h)
	}
	collide, ok := cat.Lookup("Potion of Healing", overlay)
	if !ok || !collide.Homebrew {
		t.Fatalf("a homebrew name must win over the SRD in its own scope: %+v", collide)
	}
	plain, ok := cat.Lookup("Potion of Healing", nil)
	if !ok || plain.Homebrew || plain.Doc != "srd-2024" {
		t.Fatalf("the SRD entry must stay itself for everyone else: %+v", plain)
	}
	// Search leads with the owner's own, flagged.
	hits := cat.Search("ember", 0, overlay)
	if len(hits) != 1 || !hits[0].Homebrew {
		t.Fatalf("search = %v, want the flagged homebrew hit", names(hits))
	}
}
