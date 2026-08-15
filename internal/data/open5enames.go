package data

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// open5eNameEndpoints lists the Open5e v2 endpoints whose SRD names make up
// the D&D entity dictionary: the kinds a question can mention by name and
// expect the sage to ground in real text.
var open5eNameEndpoints = []string{"spells", "creatures", "magicitems", "feats", "conditions", "weapons"}

// open5eNamesPage is the page size used while listing endpoint names. 500 is
// the API's maximum and keeps the whole fetch to a handful of round-trips
// (~5,700 SRD names across all six endpoints).
const open5eNamesPage = 500

// open5eNamesTimeout bounds the dictionary fetch during an index build. The
// payload is small (names only); two minutes covers a slow link.
const open5eNamesTimeout = 2 * time.Minute

// open5eSrdKeys are the document keys whose names are admitted to the
// dictionary, in preference order — the same SRD gating the question-time
// search uses, so the dictionary can only ever surface names the resolver
// can actually find.
var open5eSrdKeys = map[string]bool{"srd-2024": true, "srd": true, "srd-2014": true}

// FetchDNDNames lists every SRD entity name from Open5e — spells, creatures,
// magic items, feats, conditions, weapons — across the 2024, 5.1, and 5.0 SRD
// documents. Grimoire stores them as a dictionary so the chat can detect
// entity mentions the text heuristics miss (lowercase, unquoted, no Title
// Case), the exact counterpart of MTGJSON's card names on the Magic side.
// Open5e is only consulted during an index build, never per question.
func FetchDNDNames(ctx context.Context, baseURL string) ([]string, error) {
	if baseURL == "" {
		baseURL = "https://api.open5e.com"
	}
	inner, cancel := context.WithTimeout(ctx, open5eNamesTimeout)
	defer cancel()

	seen := map[string]bool{}
	var names []string
	for _, ep := range open5eNameEndpoints {
		if err := fetchEndpointNames(inner, baseURL, ep, func(name string) {
			key := nameKeyOf(name)
			if key == "" || seen[key] {
				return
			}
			seen[key] = true
			names = append(names, name)
		}); err != nil {
			return nil, fmt.Errorf("open5e %s: %w", ep, err)
		}
	}
	return names, nil
}

// fetchEndpointNames pages through one Open5e v2 list endpoint, restricted to
// the SRD documents, invoking add for each name.
func fetchEndpointNames(ctx context.Context, baseURL, endpoint string, add func(string)) error {
	page := 1
	for {
		u := fmt.Sprintf("%s/v2/%s/?fields=name,document&document__key__in=srd-2024,srd,srd-2014&limit=%d&page=%d",
			baseURL, endpoint, open5eNamesPage, page)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return err
		}
		req.Header.Set("accept", "application/json")
		req.Header.Set("user-agent", "grimoire/1.0 (+https://github.com/madeofpendletonwool/grimoire)")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return fmt.Errorf("list %s: %s", endpoint, resp.Status)
		}
		var payload struct {
			Next    *string `json:"next"`
			Results []struct {
				Name     string `json:"name"`
				Document struct {
					Key string `json:"key"`
				} `json:"document"`
			} `json:"results"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&payload)
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("decode %s page %d: %w", endpoint, page, err)
		}
		for _, r := range payload.Results {
			if r.Name == "" || !open5eSrdKeys[r.Document.Key] {
				continue
			}
			add(r.Name)
		}
		if payload.Next == nil || len(payload.Results) == 0 {
			return nil
		}
		page++
		if page > 50 { // safety ceiling: ~25k names per endpoint
			return nil
		}
	}
}

// nameKeyOf normalizes a name for dedup: lowercase letters and digits only.
func nameKeyOf(s string) string {
	var b []byte
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b = append(b, byte(r))
		} else if r >= 'A' && r <= 'Z' {
			b = append(b, byte(r-'A'+'a'))
		}
	}
	return string(b)
}
