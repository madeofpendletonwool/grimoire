package gamesession

import (
	"context"
	"errors"
	"testing"
	"time"
)

// newJob builds a two-chunk job the way the server does after an upload.
func newJob(sessionID string) *TranscriptionJob {
	return &TranscriptionJob{
		SessionID: sessionID, Title: "session-12.mp3", Filename: "session-12.mp3",
		Format: "mp3", AudioPath: "/data/transcribe/x.mp3", AudioBytes: 100,
		ChunksTotal: 2, Chunks: []TranscriptionChunk{
			{Start: 0, End: 50}, {Start: 50, End: 100},
		},
	}
}

func TestTranscriptionJobRoundTrip(t *testing.T) {
	s, cid := seeded(t)
	ses := addSession(t, s, cid, "The Ambush")

	job := newJob(ses.ID)
	if err := s.CreateTranscription(context.Background(), job); err != nil {
		t.Fatalf("create: %v", err)
	}
	if job.ID == "" || job.Status != TranscriptionPending {
		t.Fatalf("create filled = %+v", job)
	}

	got, err := s.GetTranscription(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Chunks) != 2 || got.Chunks[1].Start != 50 || got.ChunksTotal != 2 {
		t.Fatalf("chunk ledger lost: %+v", got.Chunks)
	}
	if got.CreatedAt.UnixMilli() != job.CreatedAt.UnixMilli() {
		t.Errorf("created_at drift: %v vs %v", got.CreatedAt, job.CreatedAt)
	}
}

func TestTranscriptionJobChunkLedgerPersists(t *testing.T) {
	s, cid := seeded(t)
	ses := addSession(t, s, cid, "The Ambush")
	ctx := context.Background()

	job := newJob(ses.ID)
	if err := s.CreateTranscription(ctx, job); err != nil {
		t.Fatalf("create: %v", err)
	}
	// The worker's resume point: chunk 0 done, chunk 1 not.
	job.Chunks[0].Done = true
	job.Chunks[0].Text = "DM: The door grinds open."
	job.Chunks[0].Segments = []Cue{{StartMS: 0, EndMS: 4000, Text: "DM: The door grinds open."}}
	job.Chunks[0].OffsetMS = 0
	job.Chunks[1].OffsetMS = 4000
	job.ChunksDone = 1
	job.Status = TranscriptionRunning
	if err := s.UpdateTranscription(ctx, job); err != nil {
		t.Fatalf("update: %v", err)
	}

	got, err := s.GetTranscription(ctx, job.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.Chunks[0].Done || got.Chunks[0].Text == "" || len(got.Chunks[0].Segments) != 1 {
		t.Fatalf("done chunk lost state: %+v", got.Chunks[0])
	}
	if got.ChunksDone != 1 || got.Status != TranscriptionRunning {
		t.Fatalf("progress lost: done=%d status=%s", got.ChunksDone, got.Status)
	}

	// Completing the job stamps the finished time and the source id.
	job.Status = TranscriptionCompleted
	job.SourceID = "src-1"
	job.FinishedAt = time.Now().UTC()
	if err := s.UpdateTranscription(ctx, job); err != nil {
		t.Fatalf("complete: %v", err)
	}
	got, _ = s.GetTranscription(ctx, job.ID)
	if got.SourceID != "src-1" || got.FinishedAt.IsZero() {
		t.Fatalf("completion lost: source=%q finished=%v", got.SourceID, got.FinishedAt)
	}
}

func TestNextTranscriptionIsOldestPending(t *testing.T) {
	s, cid := seeded(t)
	first := addSession(t, s, cid, "one")
	second := addSession(t, s, cid, "two")
	ctx := context.Background()

	j1, j2 := newJob(first.ID), newJob(second.ID)
	if err := s.CreateTranscription(ctx, j1); err != nil {
		t.Fatalf("create 1: %v", err)
	}
	time.Sleep(2 * time.Millisecond) // distinct created_at
	if err := s.CreateTranscription(ctx, j2); err != nil {
		t.Fatalf("create 2: %v", err)
	}
	got, err := s.NextTranscription(ctx)
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if got.ID != j1.ID {
		t.Fatalf("next = %s, want the oldest %s", got.ID, j1.ID)
	}

	// Nothing pending once both are claimed.
	j1.Status = TranscriptionRunning
	_ = s.UpdateTranscription(ctx, j1)
	j2.Status = TranscriptionRunning
	_ = s.UpdateTranscription(ctx, j2)
	if _, err := s.NextTranscription(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty queue err = %v, want ErrNotFound", err)
	}
}

func TestResetInterruptedTranscriptions(t *testing.T) {
	s, cid := seeded(t)
	ses := addSession(t, s, cid, "one")
	ctx := context.Background()

	running := newJob(ses.ID)
	running.Status = TranscriptionRunning
	if err := s.CreateTranscription(ctx, running); err != nil {
		t.Fatalf("create running: %v", err)
	}
	pending := newJob(ses.ID)
	if err := s.CreateTranscription(ctx, pending); err != nil {
		t.Fatalf("create pending: %v", err)
	}

	n, err := s.ResetInterruptedTranscriptions(ctx)
	if err != nil || n != 1 {
		t.Fatalf("reset = %d, %v; want 1 reset", n, err)
	}
	got, _ := s.GetTranscription(ctx, running.ID)
	if got.Status != TranscriptionPending {
		t.Fatalf("interrupted job status = %s, want pending", got.Status)
	}
}

func TestListAndDeleteTranscriptions(t *testing.T) {
	s, cid := seeded(t)
	ses := addSession(t, s, cid, "one")
	other := addSession(t, s, cid, "two")
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if err := s.CreateTranscription(ctx, newJob(ses.ID)); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	jobs, err := s.ListTranscriptions(ctx, ses.ID)
	if err != nil || len(jobs) != 2 {
		t.Fatalf("list = %d jobs, %v; want 2", len(jobs), err)
	}
	if otherJobs, err := s.ListTranscriptions(ctx, other.ID); err != nil || len(otherJobs) != 0 {
		t.Fatalf("other session list = %d jobs, %v; want 0", len(otherJobs), err)
	}

	if err := s.DeleteTranscription(ctx, jobs[0].ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetTranscription(ctx, jobs[0].ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted job err = %v, want ErrNotFound", err)
	}
}

func TestTranscriptionJobsCascadeWithSession(t *testing.T) {
	s, cid := seeded(t)
	ses := addSession(t, s, cid, "one")
	ctx := context.Background()
	job := newJob(ses.ID)
	if err := s.CreateTranscription(ctx, job); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.DB().ExecContext(ctx, `DELETE FROM game_sessions WHERE id = ?`, ses.ID); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	if _, err := s.GetTranscription(ctx, job.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("job outlived its session: err = %v, want ErrNotFound", err)
	}
}

func TestCreateTranscriptionRejectsUnknownSession(t *testing.T) {
	s, _ := seeded(t)
	job := newJob("no-such-session")
	if err := s.CreateTranscription(context.Background(), job); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
