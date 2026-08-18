package edhrec

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// newStubEDHREC stands up a fake EDHREC serving the Next.js data routes from
// canned payloads shaped like the live ones verified on 2026-08-18. It counts
// network hits so tests can prove caching.
func newStubEDHREC(t *testing.T, hits *int32) *Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt32(hits, 1)
		fmt.Fprint(w, `<!doctype html><script id="__NEXT_DATA__" type="application/json">{"buildId":"stub-build-1"}</script>`)
	})
	mux.HandleFunc("/_next/data/stub-build-1/commanders/krenko-tin-street-kingpin.json", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{"pageProps":{"data":{"container":{"json_dict":{"cardlists":[
			{"tag":"highsynergycards","header":"High Synergy Cards","cardviews":[
				{"name":"Krenko, Mob Boss","num_decks":1235,"synergy":0.6009},
				{"name":"Skirk Prospector","num_decks":1404,"synergy":0.5998}]},
			{"tag":"topcards","header":"Top Cards","cardviews":[
				{"name":"Sol Ring","num_decks":9000,"synergy":0.0}]}
		]}}}}}`)
	})
	mux.HandleFunc("/_next/data/stub-build-1/combos/hawkeyes-bow.json", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{"pageProps":{"data":{"container":{"json_dict":{"cardlists":[
			{"header":"Hawkeye's Bow + Seeker of Skybreak","cardviews":[{"name":"Hawkeye's Bow"},{"name":"Seeker of Skybreak"}]},
			{"header":"Hawkeye's Bow + Freed from the Real","cardviews":[{"name":"Hawkeye's Bow"},{"name":"Freed from the Real"}]}
		]}}}}}`)
	})
	mux.HandleFunc("/_next/data/stub-build-1/average-decks/krenko-tin-street-kingpin.json", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{"pageProps":{"data":{"deck":{"commander":["Krenko, Tin Street Kingpin"],"cards":{
			"Artifact":[["Sol Ring",1],["Arcane Signet",1]],
			"Creature":[["Krenko, Baron of Tin Street",1]],
			"Land":[["Mountain",36]]
		}}}}}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return New(Options{
		BaseURL:     srv.URL,
		CacheDir:    filepath.Join(t.TempDir(), "edhrec-cache"),
		MinInterval: time.Millisecond,
		Enabled:     true,
	})
}

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"Hawkeye's Bow":              "hawkeyes-bow",
		"Miirym, Sentinel Wyrm":      "miirym-sentinel-wyrm",
		"Krenko, Tin Street Kingpin": "krenko-tin-street-kingpin",
		"Sol Ring":                   "sol-ring",
	}
	for in, want := range cases {
		if got := Slugify(in); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCommanderData(t *testing.T) {
	var hits int32
	c := newStubEDHREC(t, &hits)
	data, err := c.CommanderData(context.Background(), "Krenko, Tin Street Kingpin")
	if err != nil {
		t.Fatalf("commander data: %v", err)
	}
	hs := data.HighSynergy()
	if len(hs) != 2 || hs[0].Name != "Krenko, Mob Boss" || hs[0].Synergy < 0.6 {
		t.Fatalf("high synergy = %+v", hs)
	}
	if data.TopCards()[0].Name != "Sol Ring" {
		t.Fatalf("top cards = %+v", data.TopCards())
	}
}

func TestCombos(t *testing.T) {
	var hits int32
	c := newStubEDHREC(t, &hits)
	combos, err := c.Combos(context.Background(), "Hawkeye's Bow")
	if err != nil {
		t.Fatalf("combos: %v", err)
	}
	if len(combos) != 2 {
		t.Fatalf("combos = %+v", combos)
	}
	if combos[0].Title != "Hawkeye's Bow + Seeker of Skybreak" {
		t.Fatalf("first combo = %+v", combos[0])
	}
	if len(combos[0].Cards) != 2 || combos[0].Cards[1] != "Seeker of Skybreak" {
		t.Fatalf("combo cards = %+v", combos[0].Cards)
	}
}

func TestAverageDeck(t *testing.T) {
	var hits int32
	c := newStubEDHREC(t, &hits)
	deck, err := c.AverageDeck(context.Background(), "Krenko, Tin Street Kingpin")
	if err != nil {
		t.Fatalf("average deck: %v", err)
	}
	byName := map[string]DeckEntry{}
	for _, e := range deck {
		byName[e.Name] = e
	}
	if e, ok := byName["Mountain"]; !ok || e.Count != 36 || e.Board != "Land" {
		t.Fatalf("mountain entry = %+v ok=%v", e, ok)
	}
	if _, ok := byName["Arcane Signet"]; !ok {
		t.Fatal("arcane signet missing from average deck")
	}
}

func TestCacheAvoidsRefetch(t *testing.T) {
	var hits int32
	c := newStubEDHREC(t, &hits)
	for i := 0; i < 3; i++ {
		if _, err := c.CommanderData(context.Background(), "Krenko, Tin Street Kingpin"); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	// One homepage fetch (build id) + one commander fetch, cached after.
	if n := atomic.LoadInt32(&hits); n > 2 {
		t.Fatalf("network hits = %d, want <= 2 (cache should serve repeats)", n)
	}
}

func TestNotFound(t *testing.T) {
	var hits int32
	c := newStubEDHREC(t, &hits)
	if _, err := c.Combos(context.Background(), "Unknown Card"); err != ErrNotFound {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestDisabledClient(t *testing.T) {
	c := New(Options{Enabled: false})
	if c.Enabled() {
		t.Fatal("client should report disabled")
	}
	if _, err := c.CommanderData(context.Background(), "Krenko"); err != ErrDisabled {
		t.Fatalf("err = %v, want ErrDisabled", err)
	}
}

func TestStaleCacheServesOnOutage(t *testing.T) {
	var hits int32
	c := newStubEDHREC(t, &hits)
	if _, err := c.CommanderData(context.Background(), "Krenko, Tin Street Kingpin"); err != nil {
		t.Fatalf("prime: %v", err)
	}

	// Corrupt the cached file's freshness by pretending a long TTL passed:
	// instead, point the client at a dead server and shrink the TTL to zero
	// so the cache is considered stale — the outage path must still serve it.
	dead := New(Options{
		BaseURL:     "http://127.0.0.1:1",
		CacheDir:    c.cacheDir,
		CacheTTL:    -1, // always stale
		MinInterval: time.Millisecond,
		Enabled:     true,
	})
	data, err := dead.CommanderData(context.Background(), "Krenko, Tin Street Kingpin")
	if err != nil {
		t.Fatalf("stale-serve: %v", err)
	}
	if len(data.HighSynergy()) != 2 {
		t.Fatalf("stale data = %+v", data)
	}
}

func TestCacheFileWritten(t *testing.T) {
	var hits int32
	c := newStubEDHREC(t, &hits)
	if _, err := c.Combos(context.Background(), "Hawkeye's Bow"); err != nil {
		t.Fatalf("combos: %v", err)
	}
	entries, err := os.ReadDir(c.cacheDir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("cache dir: %v entries=%d", err, len(entries))
	}
}
