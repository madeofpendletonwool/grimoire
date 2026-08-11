package cards

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const singleFaceJSON = `{
	"name": "Lightning Bolt",
	"mana_cost": "{R}",
	"type_line": "Instant",
	"oracle_text": "Lightning Bolt deals 3 damage to any target.",
	"set": "lea",
	"set_name": "Limited Edition Alpha",
	"scryfall_uri": "https://scryfall.com/card/lea/161/lightning-bolt",
	"image_uris": {"normal": "https://img.scryfall.com/bolt.jpg"}
}`

const doubleFaceJSON = `{
	"name": "Delver of Secrets // Insectile Aberration",
	"mana_cost": "{U}",
	"type_line": "Creature — Human // Creature — Human Insect",
	"oracle_text": "",
	"set": "isd",
	"scryfall_uri": "https://scryfall.com/card/isd/51",
	"card_faces": [
		{
			"name": "Delver of Secrets",
			"mana_cost": "{U}",
			"type_line": "Creature — Human",
			"oracle_text": "At the beginning of your upkeep, look at the top card of your library. You may reveal that card. If an instant or sorcery card is revealed this way, transform Delver of Secrets.",
			"power": "1",
			"toughness": "1",
			"image_uris": {"normal": "https://img.scryfall.com/delver-front.jpg"}
		},
		{
			"name": "Insectile Aberration",
			"type_line": "Creature — Human Insect",
			"oracle_text": "Flying",
			"power": "3",
			"toughness": "2",
			"image_uris": {"normal": "https://img.scryfall.com/delver-back.jpg"}
		}
	]
}`

func TestNormalizeCard_SingleFace(t *testing.T) {
	var raw scryfallCard
	if err := json.Unmarshal([]byte(singleFaceJSON), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	c := normalizeCard(&raw)
	if c.Name != "Lightning Bolt" {
		t.Errorf("name = %q", c.Name)
	}
	if c.OracleText != "Lightning Bolt deals 3 damage to any target." {
		t.Errorf("oracle = %q", c.OracleText)
	}
	if c.ImageURL != "https://img.scryfall.com/bolt.jpg" {
		t.Errorf("image = %q", c.ImageURL)
	}
	if len(c.Faces) != 0 {
		t.Errorf("expected no faces, got %d", len(c.Faces))
	}
}

func TestNormalizeCard_DoubleFace(t *testing.T) {
	var raw scryfallCard
	if err := json.Unmarshal([]byte(doubleFaceJSON), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	c := normalizeCard(&raw)
	if len(c.Faces) != 2 {
		t.Fatalf("faces = %d, want 2", len(c.Faces))
	}
	if c.Faces[0].Name != "Delver of Secrets" || c.Faces[1].Name != "Insectile Aberration" {
		t.Errorf("face names = %q / %q", c.Faces[0].Name, c.Faces[1].Name)
	}
	if !strings.Contains(c.OracleText, "transform Delver of Secrets") || !strings.Contains(c.OracleText, "Flying") {
		t.Errorf("flattened oracle missing face text: %q", c.OracleText)
	}
	if c.Faces[1].Power != "3" || c.Faces[1].Toughness != "2" {
		t.Errorf("back face p/t = %s/%s", c.Faces[1].Power, c.Faces[1].Toughness)
	}
	if c.ImageURL != "https://img.scryfall.com/delver-front.jpg" {
		t.Errorf("top image should fall back to first face: %q", c.ImageURL)
	}
}

func TestImageURL_PreferenceOrder(t *testing.T) {
	if got := imageURL(map[string]string{"small": "s", "normal": "n", "large": "l"}); got != "n" {
		t.Errorf("prefer normal-sized; got %q", got)
	}
	if got := imageURL(map[string]string{"png": "p"}); got != "p" {
		t.Errorf("got %q", got)
	}
	if got := imageURL(nil); got != "" {
		t.Errorf("nil should give empty, got %q", got)
	}
}

func newTestService(t *testing.T, fn func(w http.ResponseWriter, r *http.Request)) *Service {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(fn))
	t.Cleanup(srv.Close)
	s := NewWithBase(srv.URL)
	// Make throttle a no-op so tests don't sleep for the rate-limit interval.
	s.lastReq = time.Now().Add(-time.Hour)
	return s
}

func TestLookup_Success(t *testing.T) {
	s := newTestService(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cards/named" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.URL.Query().Get("fuzzy") != "Lightning Bolt" {
			t.Errorf("fuzzy param = %q", r.URL.Query().Get("fuzzy"))
		}
		if ua := r.Header.Get("user-agent"); !strings.Contains(ua, "grimoire") {
			t.Errorf("user-agent = %q", ua)
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(singleFaceJSON))
	})

	c, err := s.Lookup(context.Background(), "Lightning Bolt")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if c.Name != "Lightning Bolt" {
		t.Errorf("name = %q", c.Name)
	}
	// second call is served from cache (server would 500 on a second hit)
	c2, err := s.Lookup(context.Background(), "lightning bolt")
	if err != nil {
		t.Fatalf("cached lookup: %v", err)
	}
	if c2.Name != c.Name {
		t.Errorf("cached name = %q", c2.Name)
	}
}

func TestLookup_NotFound(t *testing.T) {
	s := newTestService(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"details":"No card found"}`))
	})
	if _, err := s.Lookup(context.Background(), "zzz not a card"); err != ErrNotFound {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestLookup_Ambiguous(t *testing.T) {
	// Scryfall signals ambiguity with a 400 + details.
	s := newTestService(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"details":"Too many cards match ambiguous"}`))
	})
	if _, err := s.Lookup(context.Background(), "bolt"); err != ErrNotFound {
		t.Errorf("ambiguous should map to ErrNotFound, got %v", err)
	}
}

func TestSearch_Results(t *testing.T) {
	s := newTestService(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cards/search" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"data":[` + singleFaceJSON + `]}`))
	})
	out, err := s.Search(context.Background(), "lightning", 5)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(out) != 1 || out[0].Name != "Lightning Bolt" {
		t.Errorf("search results = %+v", out)
	}
}

func TestSearch_NoMatches(t *testing.T) {
	s := newTestService(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"details":"No cards found"}`))
	})
	if _, err := s.Search(context.Background(), "zzzz", 5); err != ErrNotFound {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}
