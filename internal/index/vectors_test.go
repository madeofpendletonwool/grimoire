package index

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/data"
)

// fakeEmbedder maps each text to a fixed vector via a test-supplied function,
// so vector ranking is deterministic without an HTTP mock.
type fakeEmbedder struct {
	vec   func(text string) []float32
	calls int
}

func (f *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = f.vec(t)
		f.calls++
	}
	return out, nil
}

type errEmbedder struct{ err error }

func (e *errEmbedder) Embed(context.Context, []string) ([][]float32, error) { return nil, e.err }

// semanticDataset carries two MTG docs with no shared vocabulary, so a
// nonsense FTS5 query can only reach one via vector similarity.
func semanticDataset() *data.Dataset {
	return &data.Dataset{
		Records: []data.Record{
			{Corpus: data.CorpusMTG, Number: "702.11", Title: "Hexproof", Body: "A permanent with hexproof can't be the target of spells or abilities your opponents control.", Source: "MTG Comp Rules"},
			{Corpus: data.CorpusMTG, Number: "702.20", Title: "Vigilance", Body: "Vigilance lets a creature attack without tapping.", Source: "MTG Comp Rules"},
		},
		Meta: map[data.Corpus]data.CorpusMeta{
			data.CorpusMTG: {Name: "Magic: The Gathering", Version: "2026", SourceURL: "x", RecordCount: 2},
		},
	}
}

func TestRetrieve_VectorAddsSemanticRecall(t *testing.T) {
	// The fake embedder puts the Hexproof doc and the query on the same axis;
	// Vigilance on an orthogonal one. A query that shares no tokens with
	// either doc (FTS5 returns nothing) must still surface 702.11 via vectors.
	embed := &fakeEmbedder{vec: func(text string) []float32 {
		switch {
		case strings.Contains(strings.ToLower(text), "hexproof"):
			return []float32{1, 0}
		case strings.Contains(strings.ToLower(text), "vigilance"):
			return []float32{0, 1}
		default: // the query lands near hexproof
			return []float32{1, 0}
		}
	}}

	ctx := context.Background()
	s := newTestStore(t)
	s.SetEmbedder(embed)
	if err := s.Index(ctx, semanticDataset()); err != nil {
		t.Fatalf("index: %v", err)
	}
	if err := s.IndexEmbeddings(ctx); err != nil {
		t.Fatalf("index embeddings: %v", err)
	}
	embedded, err := s.Embedded(ctx)
	if err != nil {
		t.Fatalf("embedded: %v", err)
	}
	if !embedded {
		t.Fatal("expected vectors to be stored")
	}

	// "zzq flumph" matches no doc tokens; only the vector scan can find it.
	res, err := s.Retrieve(ctx, data.CorpusMTG, "zzq flumph blarg", 5)
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(res) != 1 || res[0].Number != "702.11" {
		t.Errorf("expected semantic recall of 702.11, got %+v", res)
	}
}

func TestRetrieve_FTSOnlyWhenNoEmbedder(t *testing.T) {
	// No embedder set: behavior matches the pre-semantic path. The same
	// nonsense query that vectors would catch returns nothing.
	ctx := context.Background()
	s := newTestStore(t)
	if err := s.Index(ctx, semanticDataset()); err != nil {
		t.Fatalf("index: %v", err)
	}
	res, err := s.Retrieve(ctx, data.CorpusMTG, "zzq flumph blarg", 5)
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(res) != 0 {
		t.Errorf("expected no FTS hits for nonsense query, got %+v", res)
	}
}

func TestRetrieve_MergesAndDedups(t *testing.T) {
	// A query FTS5 can see in the Vigilance doc ("attack without tapping")
	// but that the fake embedder routes near Hexproof. FTS5 supplies 702.20;
	// vectors supply 702.11; the union keeps FTS first with no duplicate.
	embed := &fakeEmbedder{vec: func(text string) []float32 {
		switch {
		case strings.Contains(strings.ToLower(text), "hexproof"):
			return []float32{1, 0}
		case strings.Contains(strings.ToLower(text), "vigilance"):
			return []float32{0, 1}
		default: // the query lands near hexproof
			return []float32{1, 0}
		}
	}}
	ctx := context.Background()
	s := newTestStore(t)
	s.SetEmbedder(embed)
	if err := s.Index(ctx, semanticDataset()); err != nil {
		t.Fatalf("index: %v", err)
	}
	if err := s.IndexEmbeddings(ctx); err != nil {
		t.Fatalf("index embeddings: %v", err)
	}

	res, err := s.Retrieve(ctx, data.CorpusMTG, "attack without tapping", 5)
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	nums := numbersOf(res)
	// Both docs present, no duplicates, exactly two entries.
	if len(res) != 2 {
		t.Fatalf("expected merged set of 2, got %d: %+v", len(res), res)
	}
	// FTS5 hit (702.20 vigilance) stays first as the backbone.
	if res[0].Number != "702.20" {
		t.Errorf("expected FTS hit 702.20 first, got %s: %v", res[0].Number, nums)
	}
	seen := map[string]int{}
	for _, n := range nums {
		seen[n]++
	}
	for n, c := range seen {
		if c != 1 {
			t.Errorf("number %s appeared %d times (dedup failed): %v", n, c, nums)
		}
	}
}

func TestRetrieve_VectorErrorFallsBackToFTS(t *testing.T) {
	// A failing embedder must not break retrieval: FTS5 results still return.
	ctx := context.Background()
	s := newTestStore(t)
	s.SetEmbedder(&errEmbedder{err: errors.New("upstream down")})
	if err := s.Index(ctx, semanticDataset()); err != nil {
		t.Fatalf("index: %v", err)
	}

	res, err := s.Retrieve(ctx, data.CorpusMTG, "vigilance", 5)
	if err != nil {
		t.Fatalf("retrieve should fall back, got err: %v", err)
	}
	if len(res) != 1 || res[0].Number != "702.20" {
		t.Errorf("expected FTS fallback to 702.20, got %+v", res)
	}
}

func TestRetrieve_EmbedderSetButNoVectors(t *testing.T) {
	// Embedder configured but IndexEmbeddings never ran (e.g. added after a
	// pre-embeddings index). Retrieval should still succeed via FTS5.
	embed := &fakeEmbedder{vec: func(string) []float32 { return []float32{1, 0} }}
	ctx := context.Background()
	s := newTestStore(t)
	s.SetEmbedder(embed)
	if err := s.Index(ctx, semanticDataset()); err != nil {
		t.Fatalf("index: %v", err)
	}
	// Deliberately do NOT call IndexEmbeddings.
	embedded, err := s.Embedded(ctx)
	if err != nil {
		t.Fatalf("embedded: %v", err)
	}
	if embedded {
		t.Fatal("expected no vectors stored")
	}
	res, err := s.Retrieve(ctx, data.CorpusMTG, "vigilance", 5)
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(res) != 1 || res[0].Number != "702.20" {
		t.Errorf("expected FTS result, got %+v", res)
	}
}

func TestIndexEmbeddings_NoOpWithoutEmbedder(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	// No SetEmbedder call.
	if err := s.Index(ctx, semanticDataset()); err != nil {
		t.Fatalf("index: %v", err)
	}
	if err := s.IndexEmbeddings(ctx); err != nil {
		t.Fatalf("IndexEmbeddings should be a no-op without an embedder: %v", err)
	}
	embedded, _ := s.Embedded(ctx)
	if embedded {
		t.Error("expected no vectors when no embedder is set")
	}
}

func TestIndexEmbeddings_ReplacesOnReindex(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	s.SetEmbedder(&fakeEmbedder{vec: func(string) []float32 { return []float32{1, 0, 0} }})
	if err := s.Index(ctx, semanticDataset()); err != nil {
		t.Fatalf("index: %v", err)
	}
	if err := s.IndexEmbeddings(ctx); err != nil {
		t.Fatalf("index embeddings: %v", err)
	}
	// Re-run: a fresh embed should clear and rewrite, not accumulate.
	if err := s.IndexEmbeddings(ctx); err != nil {
		t.Fatalf("re-index embeddings: %v", err)
	}
	// Count rows in doc_vectors; should equal the two docs, not four.
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM doc_vectors`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Errorf("expected 2 vectors after reindex, got %d", n)
	}
}

func TestCosine(t *testing.T) {
	cases := []struct {
		name string
		a, b []float32
		want float32
	}{
		{"identical", []float32{1, 0}, []float32{1, 0}, 1},
		{"orthogonal", []float32{1, 0}, []float32{0, 1}, 0},
		{"opposite", []float32{1, 0}, []float32{-1, 0}, -1},
		{"mismatched length", []float32{1, 0}, []float32{1}, 0},
		{"empty", nil, []float32{1}, 0},
		{"zero vector", []float32{0, 0}, []float32{1, 1}, 0},
	}
	for _, c := range cases {
		if got := cosine(c.a, c.b); absf(got-c.want) > 1e-5 {
			t.Errorf("%s: cosine = %v, want %v", c.name, got, c.want)
		}
	}
}

func absf(x float32) float32 {
	if x < 0 {
		return -x
	}
	return x
}

func TestEncodeDecodeFloat32(t *testing.T) {
	in := []float32{0, 1, -1, 1.5, -2.25, 3.14159}
	out := decodeFloat32(encodeFloat32(in))
	if len(out) != len(in) {
		t.Fatalf("roundtrip length: got %d, want %d", len(out), len(in))
	}
	for i := range in {
		if out[i] != in[i] {
			t.Errorf("roundtrip[%d] = %v, want %v", i, out[i], in[i])
		}
	}
	if decodeFloat32([]byte{0, 1, 2}) != nil {
		t.Error("expected nil for non-multiple-of-4 blob")
	}
}

func TestMergeResults_DedupsAndPreservesFTSOrder(t *testing.T) {
	fts := []Result{{Number: "613.1"}, {Number: "613.2"}}
	vec := []Result{{Number: "613.2"}, {Number: "702.11"}}
	got := mergeResults(fts, vec)
	want := []string{"613.1", "613.2", "702.11"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", numbersOf(got), want)
	}
	for i, n := range want {
		if got[i].Number != n {
			t.Errorf("got[%d] = %s, want %s", i, got[i].Number, n)
		}
	}
}

func TestMergeResults_UnnumberedByKey(t *testing.T) {
	// D&D docs have no number; dedup falls back to title+body.
	d := Result{Title: "Fireball", Body: "streak"}
	fts := []Result{d}
	vec := []Result{d, {Title: "Cure Wounds", Body: "heals"}}
	got := mergeResults(fts, vec)
	if len(got) != 2 {
		t.Errorf("expected 2 after dedup, got %d: %+v", len(got), got)
	}
}
