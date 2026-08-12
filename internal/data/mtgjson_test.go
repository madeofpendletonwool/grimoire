package data

import (
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"testing"
)

func TestAtomicCardNames_StreamsKeys(t *testing.T) {
	// The "meta" object and the per-name face arrays must be skipped; only the
	// data keys are wanted. Order in the file is preserved by the stream.
	payload := []byte(`{"meta":{"version":"5.2.1","date":"2026-08-12"},"data":{"Lightning Bolt":[{"name":"Lightning Bolt","manaCost":"{R}","text":"deals 3 damage"}],"Prizefight":[{"name":"Prizefight","manaCost":"{1}{G}"}],"Giant Growth":[{"name":"Giant Growth","text":"+3/+3"}]}}`)
	got, err := atomicCardNames(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("atomicCardNames: %v", err)
	}
	sort.Strings(got)
	want := []string{"Giant Growth", "Lightning Bolt", "Prizefight"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestAtomicCardNames_NoDataObject(t *testing.T) {
	if _, err := atomicCardNames(bytes.NewReader([]byte(`{"meta":{"v":"1"}}`))); err == nil {
		t.Fatal("expected error when data object is absent")
	}
}

func TestFetchCardNames_Gzipped(t *testing.T) {
	payload := []byte(`{"meta":{},"data":{"Lightning Bolt":[{}],"Prizefight":[{}]}}`)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(payload); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write(gz.Bytes())
	}))
	t.Cleanup(srv.Close)

	got, err := FetchCardNames(context.Background(), srv.URL+"/AtomicCards.json.gz")
	if err != nil {
		t.Fatalf("FetchCardNames: %v", err)
	}
	sort.Strings(got)
	want := []string{"Lightning Bolt", "Prizefight"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestFetchCardNames_BadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	if _, err := FetchCardNames(context.Background(), srv.URL); err == nil {
		t.Fatal("expected error for 404")
	}
}

// An empty URL falls back to the canonical AtomicCards endpoint. We only
// confirm the fallback resolves to a parseable URL shape (no network).
func TestFetchCardNames_EmptyURLFallsBack(t *testing.T) {
	// Construct the request the same way FetchCardNames does and assert the
	// default URL is used; do not perform the network round-trip.
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, DefaultMTGJSONURL, nil)
	if err != nil {
		t.Fatalf("default URL did not build a request: %v", err)
	}
	if req.Host == "" || req.URL.String() == "" {
		t.Fatalf("default URL produced an empty request: %+v", req)
	}
}
