package data

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchDNDNames(t *testing.T) {
	pages := map[string]string{
		// Page 1 of spells: two SRD names plus a community name to filter.
		"/v2/spells/?fields=name,document&document__key__in=srd-2024,srd,srd-2014&limit=500&page=1": `{
			"next":"page2","results":[
				{"name":"Fireball","document":{"key":"srd-2024"}},
				{"name":"fireball","document":{"key":"srd-2014"}},
				{"name":"Definitely Homebrew","document":{"key":"a5e-ag"}}
			]}`,
		// Page 2 ends the listing.
		"/v2/spells/?fields=name,document&document__key__in=srd-2024,srd,srd-2014&limit=500&page=2": `{
			"next":null,"results":[{"name":"Magic Missile","document":{"key":"srd"}}]}`,
		"/v2/creatures/?fields=name,document&document__key__in=srd-2024,srd,srd-2014&limit=500&page=1": `{
			"next":null,"results":[{"name":"Owlbear","document":{"key":"srd-2024"}}]}`,
	}
	// Remaining endpoints return empty listings.
	for _, ep := range []string{"magicitems", "feats", "conditions", "weapons"} {
		pages["/v2/"+ep+"/?fields=name,document&document__key__in=srd-2024,srd,srd-2014&limit=500&page=1"] = `{"next":null,"results":[]}`
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		body, ok := pages[r.URL.Path+"?"+r.URL.RawQuery]
		if !ok {
			t.Errorf("unexpected request: %s?%s", r.URL.Path, r.URL.RawQuery)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	names, err := FetchDNDNames(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("FetchDNDNames: %v", err)
	}
	joined := "\n" + strings.Join(names, "\n") + "\n"
	for _, want := range []string{"Fireball", "Magic Missile", "Owlbear"} {
		if !strings.Contains(joined, "\n"+want+"\n") {
			t.Errorf("names missing %q: %v", want, names)
		}
	}
	if strings.Contains(joined, "Definitely Homebrew") {
		t.Errorf("community name leaked into the dictionary: %v", names)
	}
	// Case-insensitive dedup: "fireball" (srd-2014) collapses into "Fireball".
	count := 0
	for _, n := range names {
		if strings.EqualFold(n, "fireball") {
			count++
		}
	}
	if count != 1 {
		t.Errorf("fireball appears %d times, want 1 (case-insensitive dedup)", count)
	}
}
