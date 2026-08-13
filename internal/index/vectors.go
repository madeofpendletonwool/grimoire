package index

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"reflect"
	"sort"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/data"
)

// vectorSchema creates the embeddings side-table. Each row mirrors a doc in
// the FTS5 table plus its embedding stored as a little-endian float32 BLOB.
// It is created alongside the docs schema and cleared whenever docs are.
const vectorSchema = `
CREATE TABLE IF NOT EXISTS doc_vectors (
	corpus    TEXT NOT NULL,
	number    TEXT NOT NULL,
	title     TEXT NOT NULL,
	body      TEXT NOT NULL,
	source    TEXT NOT NULL,
	embedding BLOB NOT NULL
);
CREATE INDEX IF NOT EXISTS doc_vectors_corpus ON doc_vectors(corpus);
`

// Embedder turns text into a vector. The concrete implementation lives in the
// embeddings package; the store holds it as an interface so retrieval and
// indexing can be exercised with a fake in tests without an HTTP mock. A nil
// embedder means semantic retrieval is off and the store is FTS5-only.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// SetEmbedder installs (or clears, with nil) the embeddings client used at
// index time and at query time. It must be set before indexing for vectors to
// be populated, and before serving for Retrieve to use them.
//
// A nil *pointer* passed as an Embedder is stored as no embedder at all. Go
// wraps a nil pointer in a non-nil interface, so `SetEmbedder(clientOrNil())`
// — the natural way to write the caller — would otherwise leave `s.embedder`
// non-nil while holding nothing, defeating every `s.embedder == nil` guard in
// this package and panicking on the first call. Normalising here keeps that
// guard meaning what it says, wherever the value came from.
func (s *Store) SetEmbedder(e Embedder) {
	if isNilValue(e) {
		s.embedder = nil
		return
	}
	s.embedder = e
}

// isNilValue reports whether an interface is nil or holds a nil pointer-like
// value. Only reflection can see the second case.
func isNilValue(e Embedder) bool {
	if e == nil {
		return true
	}
	v := reflect.ValueOf(e)
	switch v.Kind() {
	case reflect.Ptr, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.Interface:
		return v.IsNil()
	default:
		return false
	}
}

// IndexEmbeddings embeds every stored doc once and writes the vectors to
// doc_vectors, replacing any prior vectors. It is a no-op when no embedder is
// configured. Reading from the already-indexed docs table (rather than the
// source Dataset) means this also works from `grimoire serve` when a rules
// index already exists but vectors do not.
func (s *Store) IndexEmbeddings(ctx context.Context) error {
	if s.embedder == nil {
		return nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT corpus, number, title, body, source FROM docs`)
	if err != nil {
		return fmt.Errorf("read docs for embeddings: %w", err)
	}
	type doc struct {
		corpus                      data.Corpus
		number, title, body, source string
	}
	var docs []doc
	for rows.Next() {
		var d doc
		if err := rows.Scan(&d.corpus, &d.number, &d.title, &d.body, &d.source); err != nil {
			rows.Close()
			return err
		}
		if strings.TrimSpace(d.body) == "" && strings.TrimSpace(d.title) == "" {
			continue
		}
		docs = append(docs, d)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(docs) == 0 {
		return nil
	}

	texts := make([]string, len(docs))
	for i, d := range docs {
		texts[i] = embedText(d.number, d.title, d.body)
	}
	vecs, err := s.embedder.Embed(ctx, texts)
	if err != nil {
		return fmt.Errorf("embed docs: %w", err)
	}
	if len(vecs) != len(docs) {
		return fmt.Errorf("embeddings returned %d vectors for %d docs", len(vecs), len(docs))
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM doc_vectors`); err != nil {
		return fmt.Errorf("clear doc_vectors: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO doc_vectors(corpus, number, title, body, source, embedding) VALUES(?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare vector insert: %w", err)
	}
	defer stmt.Close()
	for i, d := range docs {
		if _, err := stmt.ExecContext(ctx, string(d.corpus), d.number, d.title, d.body, d.source, encodeFloat32(vecs[i])); err != nil {
			return fmt.Errorf("insert vector: %w", err)
		}
	}
	return tx.Commit()
}

// Embedded reports whether any vectors are stored.
func (s *Store) Embedded(ctx context.Context) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM doc_vectors`).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// vectorRetrieve returns the top-`limit` docs by cosine similarity to the
// embedded query. A dimension mismatch (model changed without reindex) is
// reported once and the call falls back to no results rather than mixing
// incompatible vectors.
func (s *Store) vectorRetrieve(ctx context.Context, corpus data.Corpus, query string, limit int) ([]Result, error) {
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	vecs, err := s.embedder.Embed(ctx, []string{query})
	if err != nil {
		return nil, err
	}
	if len(vecs) == 0 || len(vecs[0]) == 0 {
		return nil, nil
	}
	q := vecs[0]

	rows, err := s.db.QueryContext(ctx,
		`SELECT number, title, body, source, embedding FROM doc_vectors WHERE corpus = ?`, corpus)
	if err != nil {
		return nil, fmt.Errorf("vector query: %w", err)
	}
	defer rows.Close()

	type scored struct {
		r Result
		s float32
	}
	var hits []scored
	dimMismatch := false
	for rows.Next() {
		var r Result
		var blob []byte
		if err := rows.Scan(&r.Number, &r.Title, &r.Body, &r.Source, &blob); err != nil {
			return nil, err
		}
		v := decodeFloat32(blob)
		if len(v) != len(q) {
			dimMismatch = true
			continue
		}
		sc := cosine(q, v)
		if sc <= 0 {
			continue // orthogonal/opposite: not a semantic match
		}
		hits = append(hits, scored{r, sc})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if dimMismatch {
		log.Printf("embeddings: stored vector dimensions differ from the query (model changed?); re-run `grimoire index` with embeddings configured")
		return nil, nil
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].s > hits[j].s })
	if limit > 0 && len(hits) > limit {
		hits = hits[:limit]
	}
	out := make([]Result, len(hits))
	for i, h := range hits {
		h.r.Score = float64(h.s)
		out[i] = h.r
	}
	return out, nil
}

// mergeResults unions FTS5 and vector hits, keeping FTS5 first in its rank order
// (the backbone) and appending vector-only matches by cosine. Dedup is by the
// same key Expand uses: rule number for numbered corpora, title+body otherwise.
func mergeResults(fts, vec []Result) []Result {
	seen := map[string]bool{}
	out := make([]Result, 0, len(fts)+len(vec))
	add := func(r Result) {
		k := resultKey(r)
		if seen[k] {
			return
		}
		seen[k] = true
		out = append(out, r)
	}
	for _, r := range fts {
		add(r)
	}
	for _, r := range vec {
		add(r)
	}
	return out
}

// resultKey is the dedup identity for a doc across retrieval sources.
func resultKey(r Result) string {
	if r.Number != "" {
		return r.Number
	}
	return r.Title + "\x00" + r.Body
}

// embedText builds the text sent to the embedding model for one doc: the rule
// number and title lead (so "702.11 Hexproof" is visible to the model), then
// the body.
func embedText(number, title, body string) string {
	var b strings.Builder
	if number != "" {
		b.WriteString(number)
		if title != "" {
			b.WriteByte(' ')
		}
	}
	if title != "" {
		b.WriteString(title)
		b.WriteByte('\n')
	}
	b.WriteString(body)
	return strings.TrimSpace(b.String())
}

// cosine is the cosine similarity of two equal-length vectors. Mismatched or
// empty inputs return 0, which naturally drops out of ranking.
func cosine(a, b []float32) float32 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float32
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (f32sqrt(na) * f32sqrt(nb))
}

// f32sqrt is float32 square root via the math package.
func f32sqrt(x float32) float32 { return float32(math.Sqrt(float64(x))) }

// encodeFloat32 serializes a vector as a little-endian float32 BLOB.
func encodeFloat32(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(x))
	}
	return b
}

// decodeFloat32 deserializes a little-endian float32 BLOB. A length that is
// not a whole multiple of 4 yields nil.
func decodeFloat32(b []byte) []float32 {
	if len(b)%4 != 0 {
		return nil
	}
	out := make([]float32, len(b)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return out
}
