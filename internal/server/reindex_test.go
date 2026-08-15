package server

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"
)

// reindexServer builds a gated server whose rebuild blocks until release is
// invoked, so a test can observe the running state deterministically. release
// is safe to call more than once (the test's own call and this cleanup may
// both fire).
func reindexServer(t *testing.T) (*Server, *http.Cookie, func()) {
	t.Helper()
	s, _, _ := newGatedServer(t)
	releaseCh := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(releaseCh) }) }
	s.rebuild = func(ctx context.Context) error {
		<-releaseCh
		return nil
	}
	t.Cleanup(release)
	return s, adminSession(t, s), release
}

func fetchReindexStatus(t *testing.T, s *Server, cookie *http.Cookie) reindexStatus {
	t.Helper()
	rec := call(s, http.MethodGet, "/api/admin/reindex", "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("reindex status: %d %s", rec.Code, rec.Body)
	}
	var st reindexStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("reindex status body: %v (%s)", err, rec.Body)
	}
	return st
}

// waitFor polls cond until it holds or the deadline passes, failing the test
// on the timeout. A rebuild runs in a goroutine, so "finished" is eventual.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition did not hold before the deadline")
}

func TestReindexStartsAndReportsRunning(t *testing.T) {
	s, cookie, _ := reindexServer(t)

	rec := call(s, http.MethodPost, "/api/admin/reindex", "", cookie)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start: status %d, body %s", rec.Code, rec.Body)
	}
	waitFor(t, func() bool { return fetchReindexStatus(t, s, cookie).Running })
}

func TestReindexRefusesConcurrentRun(t *testing.T) {
	s, cookie, _ := reindexServer(t)

	if rec := call(s, http.MethodPost, "/api/admin/reindex", "", cookie); rec.Code != http.StatusAccepted {
		t.Fatalf("first start: status %d, body %s", rec.Code, rec.Body)
	}
	waitFor(t, func() bool { return fetchReindexStatus(t, s, cookie).Running })
	rec := call(s, http.MethodPost, "/api/admin/reindex", "", cookie)
	if rec.Code != http.StatusConflict {
		t.Errorf("second start: status %d, want 409", rec.Code)
	}
}

func TestReindexFinishesAndRecordsOutcome(t *testing.T) {
	s, cookie, release := reindexServer(t)
	if rec := call(s, http.MethodPost, "/api/admin/reindex", "", cookie); rec.Code != http.StatusAccepted {
		t.Fatalf("start: status %d, body %s", rec.Code, rec.Body)
	}
	release()
	waitFor(t, func() bool { return !fetchReindexStatus(t, s, cookie).Running })
	st := fetchReindexStatus(t, s, cookie)
	if st.Running || st.Error != "" || st.FinishedAt == "" {
		t.Errorf("finished status = %+v, want a clean finish", st)
	}
}

func TestReindexRequiresAdmin(t *testing.T) {
	s, admin, _ := reindexServer(t)

	// Unauthenticated: 401.
	if rec := call(s, http.MethodPost, "/api/admin/reindex", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous start: status %d, want 401", rec.Code)
	}
	// Authenticated non-admin: 403.
	inv := createInvite(t, s, admin, "")
	code, _ := inv["code"].(string)
	rec := call(s, http.MethodPost, "/api/auth/register", registerJSON("friend", "a-fine-passphrase", code))
	if rec.Code != http.StatusCreated {
		t.Fatalf("register friend: %d %s", rec.Code, rec.Body)
	}
	friend := sessionFrom(t, rec)
	if rec := call(s, http.MethodPost, "/api/admin/reindex", "", friend); rec.Code != http.StatusForbidden {
		t.Errorf("non-admin start: status %d, want 403", rec.Code)
	}
}

func TestReindexUnavailableWithoutRebuild(t *testing.T) {
	// A server wired without a rebuild function declines politely.
	s, _, _ := newGatedServer(t) // rebuild stays nil
	cookie := adminSession(t, s)
	rec := call(s, http.MethodPost, "/api/admin/reindex", "", cookie)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("start without rebuild: status %d, want 503", rec.Code)
	}
}
