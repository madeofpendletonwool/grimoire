package cards

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestQuery_ReportsTotalAndLink(t *testing.T) {
	s := newTestService(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cards/search" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("unique"); got != "art" {
			t.Errorf("unique = %q, want art", got)
		}
		if got := r.URL.Query().Get("order"); got != "released" {
			t.Errorf("order = %q, want released", got)
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"total_cards":412,"has_more":true,"warnings":["Ignored unknown keyword"],"data":[` + singleFaceJSON + `]}`))
	})
	res, err := s.Query(context.Background(), "art:ship", QueryOptions{Unique: "art", Order: "released"})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if res.Total != 412 || !res.HasMore {
		t.Errorf("total/has_more = %d/%v, want 412/true", res.Total, res.HasMore)
	}
	if len(res.Cards) != 1 || res.Cards[0].Name != "Lightning Bolt" {
		t.Errorf("cards = %+v", res.Cards)
	}
	if len(res.Warnings) != 1 {
		t.Errorf("warnings = %v", res.Warnings)
	}
	if !strings.HasPrefix(res.URL, SearchSiteURL+"?") || !strings.Contains(res.URL, "q=art%3Aship") || !strings.Contains(res.URL, "unique=art") {
		t.Errorf("url = %q", res.URL)
	}
}

// A page of 175 truncated to the caller's limit is still "more than shown",
// even when Scryfall itself had no further page.
func TestQuery_LimitMarksHasMore(t *testing.T) {
	s := newTestService(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"total_cards":2,"has_more":false,"data":[` + singleFaceJSON + `,` + singleFaceJSON + `]}`))
	})
	res, err := s.Query(context.Background(), "bolt", QueryOptions{Limit: 1})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(res.Cards) != 1 || !res.HasMore || res.Total != 2 {
		t.Errorf("cards=%d has_more=%v total=%d", len(res.Cards), res.HasMore, res.Total)
	}
}

// Regression: Search maps a 400 to ErrNotFound, which discards Scryfall's
// explanation of what was wrong with the query. The model-facing search must
// keep it — it is the correction the next attempt is built from.
func TestQuery_BadSyntaxKeepsScryfallsWords(t *testing.T) {
	s := newTestService(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"details":"All of your terms were ignored.","warnings":["Invalid expression “c:4” was ignored. Checking if cards are “4” is not supported"]}`))
	})
	_, err := s.Query(context.Background(), "c:4", QueryOptions{})
	var qe *QueryError
	if !errors.As(err, &qe) {
		t.Fatalf("err = %v, want *QueryError", err)
	}
	if !strings.Contains(err.Error(), "c:4") || !strings.Contains(err.Error(), "All of your terms were ignored") {
		t.Errorf("error lost Scryfall's explanation: %v", err)
	}
}

// No matches is an answer ("zero cards"), not a failure, and the warnings
// Scryfall attaches to it usually say why (an art tag that does not exist).
func TestQuery_NoMatchesIsZeroWithWarnings(t *testing.T) {
	s := newTestService(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"details":"Your query didn’t match any cards.","warnings":["Invalid expression “art:zeppelin” was ignored."]}`))
	})
	res, err := s.Query(context.Background(), "art:zeppelin", QueryOptions{})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if res.Total != 0 || len(res.Cards) != 0 || len(res.Warnings) != 1 {
		t.Errorf("res = %+v", res)
	}
}
