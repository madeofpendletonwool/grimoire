package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/cards"
	"github.com/madeofpendletonwool/grimoire/internal/data"
)

func scryfallStub(t *testing.T, fn http.HandlerFunc) *cards.Service {
	t.Helper()
	srv := httptest.NewServer(fn)
	t.Cleanup(srv.Close)
	return cards.NewWithBase(srv.URL)
}

// A search result tells the model the whole shape of the answer — the total,
// the link for the rest, the warnings — before the cards, and leaves a
// citation that links out to that list.
func TestSearchCardsReportsTotalLinkAndChip(t *testing.T) {
	svc := scryfallStub(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"total_cards":412,"has_more":true,"warnings":["Invalid expression “is:boat” was ignored."],"data":[
			{"name":"Pirate Ship","mana_cost":"{5}{U}","type_line":"Creature — Human Pirate","set":"lea","scryfall_uri":"https://scryfall.com/x"}
		]}`))
	})
	f := &ruleFetcher{corpus: data.CorpusMTG, cards: svc}

	out, err := f.SearchCards(context.Background(), "art:ship is:boat", "art", "")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	for _, want := range []string{"412 cards match", "showing the first 1", "Full list: " + cards.SearchSiteURL, "is:boat", "Pirate Ship — {5}{U} — Creature — Human Pirate (LEA)"} {
		if !strings.Contains(out, want) {
			t.Errorf("result lacks %q:\n%s", want, out)
		}
	}
	// The model's "art" rollup must reach Scryfall and the reader's link alike.
	if !strings.Contains(out, "unique=art") {
		t.Errorf("link does not carry the rollup: %s", out)
	}

	srcs := f.sources()
	if len(srcs) != 1 || srcs[0].URL == "" || !strings.Contains(srcs[0].Title, "art:ship") {
		t.Fatalf("sources = %+v, want one linked Scryfall chip", srcs)
	}
	// The same query again is one chip, not two.
	if _, err := f.SearchCards(context.Background(), "art:ship is:boat", "art", ""); err != nil {
		t.Fatal(err)
	}
	if got := len(f.sources()); got != 1 {
		t.Errorf("repeat search made %d chips, want 1", got)
	}
}

// Regression: the palette's Search turns a 400 into "not found". For the
// model that is the wrong outcome twice over — it hides the correction and it
// invites "no such cards" as the answer. A rejected query must come back as a
// result the model reads and retries from, not as a failure.
func TestSearchCardsHandsBackScryfallsRejection(t *testing.T) {
	svc := scryfallStub(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"details":"All of your terms were ignored.","warnings":["Invalid expression “c:4” was ignored. Checking if cards are “4” is not supported"]}`))
	})
	f := &ruleFetcher{corpus: data.CorpusMTG, cards: svc}

	out, err := f.SearchCards(context.Background(), "c:4", "", "")
	if err != nil {
		t.Fatalf("a rejected query must not be an error: %v", err)
	}
	for _, want := range []string{"rejected", "All of your terms were ignored", "c:4", "search again"} {
		if !strings.Contains(out, want) {
			t.Errorf("result lacks %q:\n%s", want, out)
		}
	}
	if len(f.sources()) != 0 {
		t.Error("a rejected query must not leave a citation")
	}
}

// Zero matches is an answer, and the warning that explains it travels with it.
func TestSearchCardsZeroMatchesKeepsWarnings(t *testing.T) {
	svc := scryfallStub(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"details":"Your query didn’t match any cards.","warnings":["Invalid expression “art:zeppelin” was ignored."]}`))
	})
	f := &ruleFetcher{corpus: data.CorpusMTG, cards: svc}
	out, err := f.SearchCards(context.Background(), "art:zeppelin", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "No cards match") || !strings.Contains(out, "art:zeppelin” was ignored") {
		t.Errorf("out = %q", out)
	}
}

// The card search is offered to Magic questions and only Magic questions.
func TestCardSearchIsMagicOnly(t *testing.T) {
	svc := cards.New()
	mtg := grounded{fetcher: &ruleFetcher{corpus: data.CorpusMTG, cards: svc}}
	if mtg.request(data.CorpusMTG, "q", nil).CardSearch == nil {
		t.Error("MTG request has no card search")
	}
	dnd := grounded{fetcher: &ruleFetcher{corpus: data.CorpusDND}}
	if dnd.request(data.CorpusDND, "q", nil).CardSearch != nil {
		t.Error("D&D request offers a card search")
	}
}
