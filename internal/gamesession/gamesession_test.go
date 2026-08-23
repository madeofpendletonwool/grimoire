package gamesession

import (
	"context"
	"errors"
	"testing"
)

func TestCreateSessionNumbersItself(t *testing.T) {
	s, cid := seeded(t)
	first := addSession(t, s, cid, "The Ambush")
	second := addSession(t, s, cid, "")
	if first.Ordinal != 1 || second.Ordinal != 2 {
		t.Fatalf("ordinals = %d, %d; want 1, 2", first.Ordinal, second.Ordinal)
	}
	if second.Name != "Session 2" {
		t.Fatalf("default name = %q; want %q", second.Name, "Session 2")
	}
	if first.Status != StatusPlanned {
		t.Fatalf("fresh status = %q; want planned", first.Status)
	}
}

func TestCreateSessionForeignCampaign(t *testing.T) {
	s, _ := seeded(t)
	if _, err := s.CreateSession(context.Background(), "no-such-campaign", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v; want ErrNotFound", err)
	}
}

func TestSessionStatusTransitions(t *testing.T) {
	s, cid := seeded(t)
	ses := addSession(t, s, cid, "one")

	live := StatusLive
	updated, err := s.UpdateSession(context.Background(), ses.ID, nil, &live)
	if err != nil {
		t.Fatalf("go live: %v", err)
	}
	if updated.Status != StatusLive || updated.StartedAt.IsZero() {
		t.Fatalf("live session: status %q started %v", updated.Status, updated.StartedAt)
	}
	done := StatusDone
	updated, err = s.UpdateSession(context.Background(), ses.ID, nil, &done)
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	if updated.EndedAt.IsZero() {
		t.Fatal("done session has no ended_at")
	}

	// A done session is a record, not a draft: it does not reopen.
	reopen := StatusLive
	if _, err := s.UpdateSession(context.Background(), ses.ID, nil, &reopen); !errors.Is(err, ErrInvalid) {
		t.Fatalf("reopen err = %v; want ErrInvalid", err)
	}
	bogus := "paused"
	if _, err := s.UpdateSession(context.Background(), ses.ID, nil, &bogus); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bogus status err = %v; want ErrInvalid", err)
	}
}

func TestListSessionsInPlayOrder(t *testing.T) {
	s, cid := seeded(t)
	addSession(t, s, cid, "a")
	addSession(t, s, cid, "b")
	addSession(t, s, cid, "c")
	got, err := s.ListSessions(context.Background(), cid)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 3 || got[0].Name != "a" || got[2].Name != "c" {
		t.Fatalf("order = %v", got)
	}
}

func TestGetSessionMissing(t *testing.T) {
	s, _ := seeded(t)
	if _, err := s.GetSession(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v; want ErrNotFound", err)
	}
}
