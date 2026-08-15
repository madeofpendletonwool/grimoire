package index

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/data"
)

// rebuildDataset builds n numbered rules whose bodies share no vocabulary with
// sampleDataset, so old-index vs new-index content is observable mid-rebuild.
func rebuildDataset(n int) *data.Dataset {
	recs := make([]data.Record, 0, n)
	for i := 0; i < n; i++ {
		recs = append(recs, data.Record{
			Corpus: data.CorpusMTG,
			Number: fmt.Sprintf("701.%d", i+1),
			Title:  "Rebuilt Rules",
			Body:   fmt.Sprintf("Rebuilt rule %d about regeneration shields.", i+1),
			Source: "MTG Comp Rules",
		})
	}
	return &data.Dataset{
		Records: recs,
		Meta: map[data.Corpus]data.CorpusMeta{
			data.CorpusMTG: {Name: "Magic: The Gathering", Version: "2027", SourceURL: "x", RecordCount: n},
		},
	}
}

// TestIndexKeepsServingWhileRebuilding is the regression for MAD-146: the
// admin's reindex button froze the whole app because Index wrote its whole
// dataset in one transaction on the store's single connection, queueing every
// other request — login included — behind the rebuild. The rebuild now stages
// in short chunked transactions, so a query issued while the rebuild is parked
// between chunks must go through, and it must still see the old index because
// the swap is atomic.
func TestIndexKeepsServingWhileRebuilding(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if err := s.Index(ctx, sampleDataset()); err != nil {
		t.Fatalf("seed index: %v", err)
	}

	oldChunk := docsChunkSize
	docsChunkSize = 1
	fired := make(chan struct{})
	hold := make(chan struct{})
	var once sync.Once
	afterDocsChunk = func() {
		once.Do(func() {
			close(fired)
			<-hold
		})
	}
	release := sync.OnceFunc(func() { close(hold) })
	defer func() {
		docsChunkSize = oldChunk
		afterDocsChunk = nil
		release() // unblock the rebuild if the test fails while parked
	}()

	done := make(chan error, 1)
	go func() { done <- s.Index(ctx, rebuildDataset(5)) }()

	select {
	case <-fired:
	case <-time.After(5 * time.Second):
		t.Fatal("rebuild never reached a chunk boundary — is Index one transaction again?")
	}

	// The rebuild is parked between chunks: the connection must be free and
	// the old index still intact.
	resCh := make(chan []Result, 1)
	errCh := make(chan error, 1)
	go func() {
		res, err := s.Search(ctx, data.CorpusMTG, "ward", 10)
		resCh <- res
		errCh <- err
	}()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("search during rebuild: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("search blocked while rebuild was parked between chunks")
	}
	if res := <-resCh; len(res) == 0 || res[0].Number != "702.21" {
		t.Errorf("mid-rebuild search should see the old index, got %+v", numbersOf(res))
	}

	release()
	if err := <-done; err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	// After the swap the new dataset is live and the old one is gone.
	got, err := s.Search(ctx, data.CorpusMTG, "regeneration", 10)
	if err != nil {
		t.Fatalf("search after rebuild: %v", err)
	}
	if len(got) == 0 || got[0].Number != "701.1" {
		t.Errorf("post-rebuild search should see the new index, got %+v", numbersOf(got))
	}
	if stale, _ := s.Search(ctx, data.CorpusMTG, "ward", 10); len(stale) != 0 {
		t.Errorf("old index rows survived the swap: %+v", numbersOf(stale))
	}
}

// TestIndexFailureKeepsOldIndex proves the chunked rebuild is still atomic on
// failure: a rebuild cancelled mid-staging leaves the old index searchable and
// drops its staging table.
func TestIndexFailureKeepsOldIndex(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if err := s.Index(ctx, sampleDataset()); err != nil {
		t.Fatalf("seed index: %v", err)
	}

	oldChunk := docsChunkSize
	docsChunkSize = 1
	defer func() { docsChunkSize = oldChunk; afterDocsChunk = nil }()

	cancelCtx, cancel := context.WithCancel(ctx)
	afterDocsChunk = cancel // cancel right after the first chunk commits
	if err := s.Index(cancelCtx, rebuildDataset(5)); err == nil {
		t.Fatal("cancelled rebuild should fail")
	}

	res, err := s.Search(ctx, data.CorpusMTG, "ward", 10)
	if err != nil {
		t.Fatalf("search after failed rebuild: %v", err)
	}
	if len(res) == 0 || res[0].Number != "702.21" {
		t.Errorf("old index must survive a failed rebuild, got %+v", numbersOf(res))
	}

	var staged int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE name = 'docs_build'`).Scan(&staged); err != nil {
		t.Fatalf("sqlite_master: %v", err)
	}
	if staged != 0 {
		t.Error("failed rebuild left its staging table behind")
	}
}

// TestIndexEmbeddingsKeepsServingWhileRebuilding: the vector rewrite is staged
// in chunks too, so retrieval must go through while it is parked between
// chunks and keep seeing the old vectors until the swap.
func TestIndexEmbeddingsKeepsServingWhileRebuilding(t *testing.T) {
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
		t.Fatalf("seed embeddings: %v", err)
	}

	oldChunk := vectorsChunkSize
	vectorsChunkSize = 1
	fired := make(chan struct{})
	hold := make(chan struct{})
	var once sync.Once
	afterVectorsChunk = func() {
		once.Do(func() {
			close(fired)
			<-hold
		})
	}
	release := sync.OnceFunc(func() { close(hold) })
	defer func() {
		vectorsChunkSize = oldChunk
		afterVectorsChunk = nil
		release()
	}()

	done := make(chan error, 1)
	go func() { done <- s.IndexEmbeddings(ctx) }()

	select {
	case <-fired:
	case <-time.After(5 * time.Second):
		t.Fatal("vector rebuild never reached a chunk boundary — is it one transaction again?")
	}

	// Parked between vector chunks: retrieval (FTS5 + vector scan) must work
	// against the still-live old vectors.
	resCh := make(chan []Result, 1)
	errCh := make(chan error, 1)
	go func() {
		res, err := s.Retrieve(ctx, data.CorpusMTG, "zzq flumph blarg", 5)
		resCh <- res
		errCh <- err
	}()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("retrieve during vector rebuild: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("retrieve blocked while vector rebuild was parked between chunks")
	}
	if res := <-resCh; len(res) != 1 || res[0].Number != "702.11" {
		t.Errorf("mid-rebuild retrieve should see the old vectors, got %+v", numbersOf(res))
	}

	release()
	if err := <-done; err != nil {
		t.Fatalf("vector rebuild: %v", err)
	}
}
