package server

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"
)

// reindexState is the shared, mutex-guarded status of an index rebuild. One
// rebuild runs at a time; the status lets the settings UI show progress and
// lets a second click be refused rather than queued.
type reindexState struct {
	mu       sync.Mutex
	running  bool
	started  time.Time
	finished time.Time
	err      error
}

// status is the JSON shape of GET /api/admin/reindex.
type reindexStatus struct {
	Running    bool   `json:"running"`
	StartedAt  string `json:"started_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
	Error      string `json:"error,omitempty"`
}

func (s *Server) reindexStatusView() reindexStatus {
	s.reindex.mu.Lock()
	defer s.reindex.mu.Unlock()
	out := reindexStatus{Running: s.reindex.running}
	if !s.reindex.started.IsZero() {
		out.StartedAt = s.reindex.started.Format(time.RFC3339)
	}
	if !s.reindex.finished.IsZero() {
		out.FinishedAt = s.reindex.finished.Format(time.RFC3339)
	}
	if s.reindex.err != nil {
		out.Error = s.reindex.err.Error()
	}
	return out
}

// startReindex launches the rebuild in the background when none is running.
// It returns false when a rebuild is already in flight.
func (s *Server) startReindex() bool {
	s.reindex.mu.Lock()
	if s.reindex.running {
		s.reindex.mu.Unlock()
		return false
	}
	s.reindex.running = true
	s.reindex.started = time.Now()
	s.reindex.finished = time.Time{}
	s.reindex.err = nil
	rebuild := s.rebuild
	s.reindex.mu.Unlock()

	go func() {
		// The rebuild is a fresh context on purpose: it must outlive the
		// request that started it, and it should complete even if the client
		// disconnects mid-poll.
		err := rebuild(context.Background())
		s.reindex.mu.Lock()
		s.reindex.running = false
		s.reindex.finished = time.Now()
		s.reindex.err = err
		s.reindex.mu.Unlock()
	}()
	return true
}

// handleReindexStart is POST /api/admin/reindex: begin a rebuild (admin only).
// The response is 202 and the rebuild continues in the background; poll
// handleReindexStatus for completion. 409 when one is already running, 503
// when the server has no rebuild wired (tests, embedded front-ends).
func (s *Server) handleReindexStart(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	if s.rebuild == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("index rebuilding is not available"))
		return
	}
	if !s.startReindex() {
		writeError(w, http.StatusConflict, errors.New("a rebuild is already running"))
		return
	}
	writeJSON(w, http.StatusAccepted, s.reindexStatusView())
}

// handleReindexStatus is GET /api/admin/reindex: how the last/running rebuild
// is doing (admin only).
func (s *Server) handleReindexStatus(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.reindexStatusView())
}
