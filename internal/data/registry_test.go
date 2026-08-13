package data

import (
	"context"
	"testing"
)

// TestRegistry_Builtins proves the two built-in corpora register at init and
// are visible through the registry API exactly as /api/meta and the index build
// consume them.
func TestRegistry_Builtins(t *testing.T) {
	got := Registered()
	if len(got) < 2 {
		t.Fatalf("Registered() = %d corpora, want at least 2 (mtg, dnd)", len(got))
	}
	if d, ok := Lookup(CorpusMTG); !ok || d.Name != "Magic: The Gathering" || d.Fetcher == nil {
		t.Errorf("mtg lookup = %+v ok=%v", d, ok)
	}
	if d, ok := Lookup(CorpusDND); !ok || d.Name != "D&D 5e SRD" || d.Fetcher == nil {
		t.Errorf("dnd lookup = %+v ok=%v", d, ok)
	}
	// MTG is registered first, so it is the default for unknown corpus values.
	if Default().Corpus != CorpusMTG {
		t.Errorf("default corpus = %q, want %q", Default().Corpus, CorpusMTG)
	}
}

// TestRegistry_UnknownLookupMisses confirms a slug that is not registered does
// not resolve — parseCorpus falls back to Default rather than inventing a value.
func TestRegistry_UnknownLookupMisses(t *testing.T) {
	if _, ok := Lookup(Corpus("nope")); ok {
		t.Fatal("lookup of unregistered corpus should miss")
	}
}

// TestRegistry_ThirdCorpusByRegistrationAlone is the acceptance test for this
// issue: a hypothetical third rule system is added purely by registering it,
// with no edit to any parseCorpus/handleMeta switch or the [mtg, dnd] literal.
// BuildDataset then fetches it through the registry alongside the built-ins.
func TestRegistry_ThirdCorpusByRegistrationAlone(t *testing.T) {
	const fake Corpus = "fakegame"
	fetched := false
	Register(Definition{
		Corpus: fake,
		Name:   "Fake Game",
		Accent: "test-green",
		Fetcher: func(_ context.Context, _ FetchOptions) (*Dataset, error) {
			fetched = true
			return &Dataset{
				Records: []Record{{Corpus: fake, Number: "1.1", Title: "Basics", Body: "A fake rule.", Source: "Fake"}},
				Meta:    map[Corpus]CorpusMeta{fake: {Name: "Fake Game", Version: "1", RecordCount: 1}},
			}, nil
		},
	})
	t.Cleanup(func() { deregister(fake) })

	// The new corpus is valid the moment it is registered — no switch to update.
	d, ok := Lookup(fake)
	if !ok {
		t.Fatalf("registered %q not found by Lookup", fake)
	}
	if d.Name != "Fake Game" {
		t.Errorf("name = %q", d.Name)
	}

	// BuildDataset iterates the registry and calls the new fetcher too.
	ds, err := BuildDataset(context.Background(), FetchOptions{Include: map[Corpus]bool{fake: true}})
	if err != nil {
		t.Fatalf("BuildDataset: %v", err)
	}
	if !fetched {
		t.Fatal("BuildDataset did not invoke the registered corpus's fetcher")
	}
	var sawFake bool
	for _, r := range ds.Records {
		if r.Corpus == fake {
			sawFake = true
		}
	}
	if !sawFake {
		t.Error("BuildDataset did not merge the registered corpus's records")
	}
}

// TestFetchOptions_Include confirms the Include semantics: empty means all,
// non-empty means only the listed corpora.
func TestFetchOptions_Include(t *testing.T) {
	opts := FetchOptions{}
	if !opts.include(CorpusMTG) || !opts.include(CorpusDND) {
		t.Error("empty Include should permit all registered corpora")
	}
	subset := FetchOptions{Include: map[Corpus]bool{CorpusDND: true}}
	if !subset.include(CorpusDND) {
		t.Error("listed corpus should be included")
	}
	if subset.include(CorpusMTG) {
		t.Error("unlisted corpus should be excluded when Include is non-empty")
	}
}
