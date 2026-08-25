// Transcription jobs (MAD-320): the resumable ledger behind the optional
// audio→transcript hook. A job row is mutable state, deliberately unlike the
// append-only sources: each chunk's transcript is persisted the moment it
// returns, so a restart resumes at the first unfinished chunk instead of
// re-uploading a four-hour recording. The finished product is an ordinary
// 'transcript' source row written through AddSource once every chunk is done.

package gamesession

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Job statuses. pending waits for the worker; running is mid-flight (reset to
// pending at boot); completed wrote its source; failed keeps its audio for a
// retry; cancelled is on its way out.
const (
	TranscriptionPending   = "pending"
	TranscriptionRunning   = "running"
	TranscriptionCompleted = "completed"
	TranscriptionFailed    = "failed"
	TranscriptionCancelled = "cancelled"
)

// TranscriptionChunk is one sequentially-transcribable piece of a recording:
// a byte range of the original file, plus the transcript that came back.
// OffsetMS is where the chunk sits in the whole recording — assigned from
// the measured durations of the chunks before it, which only a sequential
// walk can know. Segments are relative to the chunk's own start. Prefix is
// the synthesized WAV header a split WAV chunk carries in front of its bytes
// (nil for stream formats).
type TranscriptionChunk struct {
	Start    int64  `json:"start"`
	End      int64  `json:"end"`
	OffsetMS int64  `json:"offset_ms"`
	Prefix   []byte `json:"prefix,omitempty"`
	Text     string `json:"text,omitempty"`
	Segments []Cue  `json:"segments,omitempty"`
	Done     bool   `json:"done"`
}

// TranscriptionJob is one uploaded recording working its way to a transcript
// source. AudioPath points at the file waiting beside the database; it is
// the job's to delete when it finishes (unless the operator opted in).
type TranscriptionJob struct {
	ID          string
	SessionID   string
	Title       string
	Author      string
	Filename    string
	Format      string
	Language    string
	AudioPath   string
	AudioBytes  int64
	Status      string
	ChunksTotal int
	ChunksDone  int
	Chunks      []TranscriptionChunk
	SourceID    string
	Error       string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	FinishedAt  time.Time
}

const transcriptionCols = `id, session_id, title, author, filename, format, language,
	audio_path, audio_bytes, status, chunks_total, chunks_done, chunks, source_id, error,
	created_at, updated_at, finished_at`

func scanTranscription(row interface{ Scan(...any) error }) (*TranscriptionJob, error) {
	var (
		job       TranscriptionJob
		chunks    string
		sourceID  sql.NullString
		errMsg    sql.NullString
		finished  sql.NullInt64
		createdMs int64
		updatedMs int64
	)
	if err := row.Scan(&job.ID, &job.SessionID, &job.Title, &job.Author, &job.Filename,
		&job.Format, &job.Language, &job.AudioPath, &job.AudioBytes, &job.Status,
		&job.ChunksTotal, &job.ChunksDone, &chunks, &sourceID, &errMsg,
		&createdMs, &updatedMs, &finished); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: transcription job", ErrNotFound)
		}
		return nil, err
	}
	job.SourceID = sourceID.String
	job.Error = errMsg.String
	job.CreatedAt = time.UnixMilli(createdMs).UTC()
	job.UpdatedAt = time.UnixMilli(updatedMs).UTC()
	if finished.Valid {
		job.FinishedAt = time.UnixMilli(finished.Int64).UTC()
	}
	if chunks != "" {
		if err := json.Unmarshal([]byte(chunks), &job.Chunks); err != nil {
			return nil, fmt.Errorf("decode transcription chunks: %w", err)
		}
	}
	return &job, nil
}

// CreateTranscription records a freshly uploaded recording. The chunks must
// already be planned (byte ranges carry no server state yet) and the audio
// already written to its final path.
func (s *Store) CreateTranscription(ctx context.Context, job *TranscriptionJob) error {
	if job.SessionID == "" {
		return fmt.Errorf("%w: transcription job needs a session", ErrInvalid)
	}
	if _, err := s.sessionCampaign(ctx, job.SessionID); err != nil {
		return err
	}
	if job.Status == "" {
		job.Status = TranscriptionPending
	}
	now := time.Now().UTC()
	job.ID = uuid.NewString()
	job.CreatedAt, job.UpdatedAt = now, now
	if job.Chunks == nil {
		job.Chunks = []TranscriptionChunk{}
	}
	chunks, err := json.Marshal(job.Chunks)
	if err != nil {
		return fmt.Errorf("encode chunks: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO transcription_jobs (id, session_id, title, author, filename, format, language,
			audio_path, audio_bytes, status, chunks_total, chunks_done, chunks, source_id, error,
			created_at, updated_at, finished_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL, ?, ?, NULL)`,
		job.ID, job.SessionID, job.Title, job.Author, job.Filename, job.Format, job.Language,
		job.AudioPath, job.AudioBytes, job.Status, job.ChunksTotal, job.ChunksDone, string(chunks),
		job.CreatedAt.UnixMilli(), job.UpdatedAt.UnixMilli())
	if err != nil {
		return fmt.Errorf("insert transcription job: %w", err)
	}
	return nil
}

// GetTranscription loads one job with its chunk ledger.
func (s *Store) GetTranscription(ctx context.Context, id string) (*TranscriptionJob, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+transcriptionCols+` FROM transcription_jobs WHERE id = ?`, id)
	return scanTranscription(row)
}

// ListTranscriptions returns a session's jobs, oldest first.
func (s *Store) ListTranscriptions(ctx context.Context, sessionID string) ([]TranscriptionJob, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+transcriptionCols+` FROM transcription_jobs
		WHERE session_id = ? ORDER BY created_at, id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list transcription jobs: %w", err)
	}
	defer rows.Close()
	var out []TranscriptionJob
	for rows.Next() {
		job, err := scanTranscription(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *job)
	}
	return out, rows.Err()
}

// NextTranscription returns the oldest pending job, or ErrNotFound when the
// queue is empty — the worker's only scheduling call.
func (s *Store) NextTranscription(ctx context.Context) (*TranscriptionJob, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+transcriptionCols+` FROM transcription_jobs
		WHERE status = ? ORDER BY created_at, id LIMIT 1`, TranscriptionPending)
	return scanTranscription(row)
}

// UpdateTranscription persists the mutable ledger state: status, chunk
// results, the finished source id, the failure message, the timestamps.
func (s *Store) UpdateTranscription(ctx context.Context, job *TranscriptionJob) error {
	_, err := s.updateTranscription(ctx, job, "")
	return err
}

// UpdateTranscriptionProgress is the worker's save, and it refuses to touch
// a cancelled job: a cancel that lands while a chunk request is in flight
// wins the race with the save that follows. It reports false when the row
// was cancelled (or vanished) in between.
func (s *Store) UpdateTranscriptionProgress(ctx context.Context, job *TranscriptionJob) (bool, error) {
	return s.updateTranscription(ctx, job, "AND status != '"+TranscriptionCancelled+"'")
}

func (s *Store) updateTranscription(ctx context.Context, job *TranscriptionJob, guard string) (bool, error) {
	chunks, err := json.Marshal(job.Chunks)
	if err != nil {
		return false, fmt.Errorf("encode chunks: %w", err)
	}
	job.UpdatedAt = time.Now().UTC()
	var finished any
	if !job.FinishedAt.IsZero() {
		finished = job.FinishedAt.UnixMilli()
	}
	var sourceID, errMsg any
	if job.SourceID != "" {
		sourceID = job.SourceID
	}
	if job.Error != "" {
		errMsg = job.Error
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE transcription_jobs SET status = ?, chunks_total = ?, chunks_done = ?, chunks = ?,
			source_id = ?, error = ?, updated_at = ?, finished_at = ?
		WHERE id = ? `+guard,
		job.Status, job.ChunksTotal, job.ChunksDone, string(chunks),
		sourceID, errMsg, job.UpdatedAt.UnixMilli(), finished, job.ID)
	if err != nil {
		return false, fmt.Errorf("update transcription job: %w", err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ResetInterruptedTranscriptions returns running jobs to pending — the boot
// resume: a job mid-flight when the server stopped continues where its ledger
// left off. It reports how many were re-enqueued.
func (s *Store) ResetInterruptedTranscriptions(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE transcription_jobs SET status = ?, updated_at = ?
		WHERE status = ?`, TranscriptionPending, time.Now().UTC().UnixMilli(), TranscriptionRunning)
	if err != nil {
		return 0, fmt.Errorf("reset interrupted transcription jobs: %w", err)
	}
	return res.RowsAffected()
}

// DeleteTranscription removes a job row (the caller owns the audio file).
func (s *Store) DeleteTranscription(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM transcription_jobs WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete transcription job: %w", err)
	}
	return nil
}
