// The optional audio→transcript hook (MAD-320, ADR 5): upload a session
// recording, a background job walks it to the configured OpenAI-compatible
// transcription endpoint chunk by chunk, and the finished product is an
// ordinary 'transcript' source — identical in shape to a pasted one, so
// spans, extraction and the canon engine are unchanged.
//
// Unconfigured means the affordance is not there: routes answer 503, /api/meta
// reports it, and the UI shows no audio button. Long jobs never hold an HTTP
// request open — the upload returns 202 and the job is polled. The job
// ledger (migration 0010) persists each chunk's transcript as it returns, so
// a restart resumes at the first unfinished chunk, and the raw audio is
// deleted the moment the transcript source exists unless the operator opted
// in to keeping it.

package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/madeofpendletonwool/grimoire/internal/gamesession"
	"github.com/madeofpendletonwool/grimoire/internal/transcribe"
)

// Caps and defaults for the transcription path. A four-hour session is the
// normal case, not the edge case, so the upload cap is generous while still
// refusing the accidentally-dropped disk image, and the chunk target stays
// under OpenAI's 25 MB per-request limit.
const (
	defaultTranscribeMaxUpload = 1 << 30 // 1 GiB
	defaultTranscribeMaxDur    = 8 * time.Hour
)

// TranscribeOptions carries the operator-tunable knobs beyond the endpoint
// itself. Zero values fall back to the defaults.
type TranscribeOptions struct {
	// Dir is where uploaded recordings wait while their job runs — beside
	// the database file by default, never /tmp (a reboot must not eat a
	// resumable job's audio).
	Dir string
	// KeepAudio opts into keeping the recording after the transcript lands
	// (failed jobs keep theirs regardless, for a retry). Off by default:
	// session recordings of real people are the most sensitive data this
	// app will ever touch.
	KeepAudio bool
	// MaxUploadBytes caps one uploaded recording.
	MaxUploadBytes int64
	// MaxDuration caps the transcript's measured length, guarding a
	// mislabeled 40-hour file from quietly burning a weekend of GPU.
	MaxDuration time.Duration
	// ChunkBytes is the chunk target for long recordings.
	ChunkBytes int64
}

// WithTranscriber wires the optional transcription hook. A nil or
// unconfigured client leaves the affordance absent: routes report
// unconfigured rather than degrading.
func (s *Server) WithTranscriber(client *transcribe.Client, opts TranscribeOptions) *Server {
	s.transcribe = client
	if opts.MaxUploadBytes <= 0 {
		opts.MaxUploadBytes = defaultTranscribeMaxUpload
	}
	if opts.MaxDuration <= 0 {
		opts.MaxDuration = defaultTranscribeMaxDur
	}
	if opts.ChunkBytes <= 0 {
		opts.ChunkBytes = transcribe.DefaultChunkBytes
	}
	s.transcribeOpts = opts
	return s
}

// ResumeTranscriptions runs at boot: jobs caught mid-flight by a shutdown go
// back to pending, and the worker starts on whatever the ledger holds. Each
// chunk's transcript was persisted before the interruption, so a resumed job
// continues rather than restarts.
func (s *Server) ResumeTranscriptions() {
	if s.transcribe == nil || s.sessions == nil {
		return
	}
	if n, err := s.sessions.ResetInterruptedTranscriptions(context.Background()); err != nil {
		log.Printf("transcription resume: %v", err)
	} else if n > 0 {
		log.Printf("resuming %d interrupted transcription job(s)", n)
	}
	s.kickTranscriber()
}

/* ---------- the worker ---------- */

// transcribeWorkState is the single-flight guard: one transcription job runs
// at a time, deliberately — the realistic backends are single-model boxes,
// and sequential keeps memory and GPU honest.
type transcribeWorkState struct {
	mu      sync.Mutex
	running bool
}

func (s *Server) kickTranscriber() {
	if s.transcribe == nil || s.sessions == nil {
		return
	}
	s.transcribeWork.mu.Lock()
	if s.transcribeWork.running {
		s.transcribeWork.mu.Unlock()
		return
	}
	s.transcribeWork.running = true
	s.transcribeWork.mu.Unlock()
	go s.transcriberWorker()
}

func (s *Server) transcriberWorker() {
	defer func() {
		s.transcribeWork.mu.Lock()
		s.transcribeWork.running = false
		s.transcribeWork.mu.Unlock()
	}()
	for {
		job, err := s.sessions.NextTranscription(context.Background())
		if errors.Is(err, gamesession.ErrNotFound) {
			return // queue empty
		}
		if err != nil {
			log.Printf("transcription queue: %v", err)
			return
		}
		if err := s.runTranscription(context.Background(), job.ID); err != nil {
			log.Printf("transcription %s: %v", job.ID, err)
		}
	}
}

// runTranscription drives one job to completion. It reloads the job from the
// ledger around every chunk, so a DELETE (session gone) or a cancel between
// chunks is honored without a race, and each chunk's result is saved before
// the next request — the resume point.
func (s *Server) runTranscription(ctx context.Context, jobID string) error {
	for {
		job, err := s.sessions.GetTranscription(ctx, jobID)
		if errors.Is(err, gamesession.ErrNotFound) {
			// The session (and with it the job) was deleted mid-flight;
			// the audio has no owner left.
			s.removeTranscriptionAudio(jobID)
			return nil
		}
		if err != nil {
			return err
		}
		if job.Status == gamesession.TranscriptionCancelled {
			s.finishCancelled(job)
			return nil
		}
		next := firstPendingChunk(job)
		if next < 0 {
			return s.completeTranscription(ctx, job)
		}
		if job.Status != gamesession.TranscriptionRunning {
			job.Status = gamesession.TranscriptionRunning
			if err := s.sessions.UpdateTranscription(ctx, job); err != nil {
				return err
			}
		}

		ch := &job.Chunks[next]
		// The chunk's offset in the whole recording is only knowable once
		// the previous chunk's duration was measured.
		if next > 0 && job.Chunks[next-1].Done {
			ch.OffsetMS = job.Chunks[next-1].OffsetMS + chunkDurationMS(&job.Chunks[next-1])
		}

		f, err := os.Open(job.AudioPath)
		if err != nil {
			return s.failTranscription(ctx, job, fmt.Errorf("recording file is missing (%v) — delete the job and upload again", err))
		}
		var body io.Reader = io.NewSectionReader(f, ch.Start, ch.End-ch.Start)
		if len(ch.Prefix) > 0 {
			body = io.MultiReader(strings.NewReader(string(ch.Prefix)), body)
		}
		res, terr := s.transcribe.Transcribe(ctx, body, job.Filename, job.Language)
		f.Close()
		if terr != nil {
			// The audio stays: a failed job is retryable, and the retry
			// resumes at this chunk.
			return s.failTranscription(ctx, job, terr)
		}

		ch.Text = res.Text
		ch.Segments = make([]gamesession.Cue, 0, len(res.Segments))
		for _, seg := range res.Segments {
			ch.Segments = append(ch.Segments, gamesession.Cue{
				StartMS: seg.StartMS, EndMS: seg.EndMS, Text: seg.Text,
			})
		}
		ch.Done = true
		job.ChunksDone++
		if end := ch.OffsetMS + chunkDurationMS(ch); s.transcribeOpts.MaxDuration > 0 && end > s.transcribeOpts.MaxDuration.Milliseconds() {
			s.removeTranscriptionAudio(job.ID) // no retry for a too-long recording
			job.Status = gamesession.TranscriptionFailed
			job.Error = fmt.Sprintf("recording exceeds the %s duration cap (TRANSCRIBE_MAX_DURATION)", s.transcribeOpts.MaxDuration)
			job.FinishedAt = time.Now().UTC()
			_, _ = s.sessions.UpdateTranscriptionProgress(ctx, job)
			return nil
		}
		ok, err := s.sessions.UpdateTranscriptionProgress(ctx, job)
		if err != nil {
			return err
		}
		if !ok {
			// Cancelled while the request was in flight — the cancel wins.
			if fresh, ferr := s.sessions.GetTranscription(ctx, job.ID); ferr == nil {
				s.finishCancelled(fresh)
			}
			return nil
		}
	}
}

// completeTranscription assembles the chunk ledger into the transcript
// source. Timed output is built exactly the way subtitle ingest builds it —
// text is the cue texts joined with blank lines, timing the cues — so the
// source is indistinguishable from an .srt upload. If any chunk came back
// without timings the whole source is untimed, like a paste.
func (s *Server) completeTranscription(ctx context.Context, job *gamesession.TranscriptionJob) error {
	text, timing := assembleTranscript(job)
	src, err := s.sessions.AddSource(ctx, job.SessionID, gamesession.SourceTranscript,
		job.Author, job.Title, text, timing)
	if err != nil {
		return s.failTranscription(ctx, job, err)
	}
	job.Status = gamesession.TranscriptionCompleted
	job.SourceID = src.ID
	job.FinishedAt = time.Now().UTC()
	if ok, err := s.sessions.UpdateTranscriptionProgress(ctx, job); err != nil {
		return err
	} else if !ok {
		// Cancelled in the closing microseconds; the transcript source is
		// written and valid, so it stays — but the audio goes, per the
		// cancel the DM asked for.
		s.removeTranscriptionAudio(job.ID)
		return nil
	}
	if !s.transcribeOpts.KeepAudio {
		s.removeTranscriptionAudio(job.ID)
	}
	log.Printf("transcribed %s into source %s (%d chunks)", job.Filename, src.ID, job.ChunksTotal)
	return nil
}

func (s *Server) failTranscription(ctx context.Context, job *gamesession.TranscriptionJob, cause error) error {
	job.Status = gamesession.TranscriptionFailed
	job.Error = cause.Error()
	job.FinishedAt = time.Now().UTC()
	if ok, err := s.sessions.UpdateTranscriptionProgress(ctx, job); err != nil {
		return err
	} else if !ok {
		// A cancel beat the failure to the row; honor the cancel instead.
		if fresh, ferr := s.sessions.GetTranscription(ctx, job.ID); ferr == nil {
			s.finishCancelled(fresh)
		}
		return nil
	}
	return cause
}

func (s *Server) finishCancelled(job *gamesession.TranscriptionJob) {
	s.removeTranscriptionAudio(job.ID)
	job.FinishedAt = time.Now().UTC()
	_ = s.sessions.UpdateTranscription(context.Background(), job)
}

// removeTranscriptionAudio deletes the recording the job's audio path points
// at — the whole point of the retention rule. Best-effort: an already-gone
// file is fine. The path is derived from the job row, never the request.
func (s *Server) removeTranscriptionAudio(jobID string) {
	job, err := s.sessions.GetTranscription(context.Background(), jobID)
	if err != nil {
		return
	}
	if s.transcribeOpts.KeepAudio {
		return
	}
	_ = os.Remove(job.AudioPath)
}

// firstPendingChunk returns the index of the first not-done chunk, or -1.
func firstPendingChunk(job *gamesession.TranscriptionJob) int {
	for i := range job.Chunks {
		if !job.Chunks[i].Done {
			return i
		}
	}
	return -1
}

// chunkDurationMS is the measured length of a finished chunk: its last
// segment's end, or 0 when the backend returned no timings.
func chunkDurationMS(ch *gamesession.TranscriptionChunk) int64 {
	if len(ch.Segments) == 0 {
		return 0
	}
	return ch.Segments[len(ch.Segments)-1].EndMS
}

func assembleTranscript(job *gamesession.TranscriptionJob) (string, []gamesession.Cue) {
	timed := true
	for i := range job.Chunks {
		if len(job.Chunks[i].Segments) == 0 {
			timed = false
			break
		}
	}
	var parts []string
	if timed {
		var cues []gamesession.Cue
		for i := range job.Chunks {
			for _, seg := range job.Chunks[i].Segments {
				cues = append(cues, gamesession.Cue{
					StartMS: job.Chunks[i].OffsetMS + seg.StartMS,
					EndMS:   job.Chunks[i].OffsetMS + seg.EndMS,
					Text:    seg.Text,
				})
				parts = append(parts, seg.Text)
			}
		}
		return strings.Join(parts, "\n\n"), cues
	}
	for i := range job.Chunks {
		if job.Chunks[i].Text != "" {
			parts = append(parts, job.Chunks[i].Text)
		}
	}
	return strings.Join(parts, "\n\n"), nil
}

/* ---------- the HTTP surface ---------- */

type transcriptionView struct {
	ID          string `json:"id"`
	SessionID   string `json:"session_id"`
	Status      string `json:"status"`
	Title       string `json:"title"`
	Author      string `json:"author"`
	Filename    string `json:"filename"`
	AudioBytes  int64  `json:"audio_bytes"`
	ChunksDone  int    `json:"chunks_done"`
	ChunksTotal int    `json:"chunks_total"`
	SourceID    string `json:"source_id,omitempty"`
	Error       string `json:"error,omitempty"`
	CreatedAt   string `json:"created_at"`
	FinishedAt  string `json:"finished_at,omitempty"`
}

func toTranscriptionView(j gamesession.TranscriptionJob) transcriptionView {
	v := transcriptionView{
		ID: j.ID, SessionID: j.SessionID, Status: j.Status, Title: j.Title,
		Author: j.Author, Filename: j.Filename, AudioBytes: j.AudioBytes,
		ChunksDone: j.ChunksDone, ChunksTotal: j.ChunksTotal, SourceID: j.SourceID,
		CreatedAt: j.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
	if j.Error != "" {
		v.Error = j.Error
	}
	if !j.FinishedAt.IsZero() {
		v.FinishedAt = j.FinishedAt.Format("2006-01-02T15:04:05Z07:00")
	}
	return v
}

// handleStartTranscription is POST .../transcriptions: the audio upload. DM
// only, multipart, 202 — the job runs in the background and is polled. The
// recording is written straight to disk under the cap; nothing audio-shaped
// ever enters SQLite.
func (s *Server) handleStartTranscription(w http.ResponseWriter, r *http.Request) {
	if !s.requireDM(w, r, r.PathValue("cid")) {
		return
	}
	if _, ok := s.sessionInCampaign(w, r, r.PathValue("cid"), r.PathValue("sid")); !ok {
		return
	}
	if s.transcribe == nil || !s.transcribe.Configured() {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("audio transcription is not configured (set TRANSCRIBE_BASE_URL and TRANSCRIBE_MODEL)"))
		return
	}
	opts := s.transcribeOpts
	r.Body = http.MaxBytesReader(w, r.Body, opts.MaxUploadBytes+(2<<20))
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		writeError(w, http.StatusRequestEntityTooLarge,
			fmt.Errorf("upload too large (max %d MB)", opts.MaxUploadBytes>>20))
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("file field is required"))
		return
	}
	defer file.Close()
	format, err := transcribe.FormatByFilename(header.Filename)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("create transcription dir"))
		return
	}
	audioPath := filepath.Join(opts.Dir, uuid.NewString()+"."+format)
	if err := writeCapped(file, audioPath, opts.MaxUploadBytes); err != nil {
		_ = os.Remove(audioPath)
		writeError(w, http.StatusRequestEntityTooLarge, err)
		return
	}

	job, err := s.planTranscription(r.PathValue("sid"), header.Filename, format, audioPath)
	if err != nil {
		_ = os.Remove(audioPath)
		writeError(w, http.StatusInternalServerError, fmt.Errorf("plan transcription: %v", err))
		return
	}
	job.Author = strings.TrimSpace(r.FormValue("author"))
	job.Title = strings.TrimSpace(r.FormValue("title"))
	job.Language = strings.TrimSpace(r.FormValue("language"))
	if job.Title == "" {
		job.Title = header.Filename
	}
	if err := s.sessions.CreateTranscription(r.Context(), job); err != nil {
		_ = os.Remove(audioPath)
		writeCampaignError(w, err)
		return
	}
	s.kickTranscriber()
	writeJSON(w, http.StatusAccepted, map[string]any{"transcription": toTranscriptionView(*job)})
}

// planTranscription reads just enough of the recording to plan its chunks
// (the WAV header, when it is one) and builds the pending job.
func (s *Server) planTranscription(sessionID, filename, format, audioPath string) (*gamesession.TranscriptionJob, error) {
	f, err := os.Open(audioPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	head := make([]byte, 64<<10)
	n, _ := io.ReadFull(f, head)
	planned, err := transcribe.PlanChunks(format, head[:n], info.Size(), s.transcribeOpts.ChunkBytes)
	if err != nil {
		return nil, err
	}
	chunks := make([]gamesession.TranscriptionChunk, len(planned))
	for i, c := range planned {
		chunks[i] = gamesession.TranscriptionChunk{Start: c.Start, End: c.End, Prefix: c.Prefix}
	}
	return &gamesession.TranscriptionJob{
		SessionID: sessionID, Filename: filename, Format: format,
		AudioPath: audioPath, AudioBytes: info.Size(),
		ChunksTotal: len(chunks), Chunks: chunks,
	}, nil
}

// writeCapped streams the upload to dst, refusing more than cap bytes with a
// clear error naming the cap.
func writeCapped(src io.Reader, dst string, cap int64) error {
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	n, err := io.Copy(f, io.LimitReader(src, cap+1))
	if err != nil {
		return err
	}
	if n > cap {
		return fmt.Errorf("recording exceeds the %d MB transcription upload cap (TRANSCRIBE_MAX_UPLOAD_MB)", cap>>20)
	}
	return nil
}

// transcriptionInSession loads a job and confirms it belongs to the session
// in the path — a job from another session is a plain 404, like every other
// cross-ownership read here.
func (s *Server) transcriptionInSession(w http.ResponseWriter, r *http.Request) (*gamesession.TranscriptionJob, bool) {
	job, err := s.sessions.GetTranscription(r.Context(), r.PathValue("tid"))
	if err != nil || job.SessionID != r.PathValue("sid") {
		writeError(w, http.StatusNotFound, fmt.Errorf("transcription %s", r.PathValue("tid")))
		return nil, false
	}
	return job, true
}

func (s *Server) handleListTranscriptions(w http.ResponseWriter, r *http.Request) {
	if !s.requireDM(w, r, r.PathValue("cid")) {
		return
	}
	if _, ok := s.sessionInCampaign(w, r, r.PathValue("cid"), r.PathValue("sid")); !ok {
		return
	}
	jobs, err := s.sessions.ListTranscriptions(r.Context(), r.PathValue("sid"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	views := make([]transcriptionView, 0, len(jobs))
	for i := range jobs {
		views = append(views, toTranscriptionView(jobs[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"transcriptions": views})
}

// handleGetTranscription is the progress poll: status, chunk counts, the
// finished source id.
func (s *Server) handleGetTranscription(w http.ResponseWriter, r *http.Request) {
	if !s.requireDM(w, r, r.PathValue("cid")) {
		return
	}
	if _, ok := s.sessionInCampaign(w, r, r.PathValue("cid"), r.PathValue("sid")); !ok {
		return
	}
	job, ok := s.transcriptionInSession(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"transcription": toTranscriptionView(*job)})
}

// handleDeleteTranscription removes a job. A running one is cancelled — the
// worker honors it at the next chunk boundary — anything else goes straight
// out, audio included.
func (s *Server) handleDeleteTranscription(w http.ResponseWriter, r *http.Request) {
	if !s.requireDM(w, r, r.PathValue("cid")) {
		return
	}
	if _, ok := s.sessionInCampaign(w, r, r.PathValue("cid"), r.PathValue("sid")); !ok {
		return
	}
	job, ok := s.transcriptionInSession(w, r)
	if !ok {
		return
	}
	if job.Status == gamesession.TranscriptionRunning {
		job.Status = gamesession.TranscriptionCancelled
		if err := s.sessions.UpdateTranscription(r.Context(), job); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	} else {
		if err := s.sessions.DeleteTranscription(r.Context(), job.ID); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		_ = os.Remove(job.AudioPath)
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleRetryTranscription re-enqueues a failed job. Done chunks are not
// redone — the ledger resumes at the failure.
func (s *Server) handleRetryTranscription(w http.ResponseWriter, r *http.Request) {
	if !s.requireDM(w, r, r.PathValue("cid")) {
		return
	}
	if _, ok := s.sessionInCampaign(w, r, r.PathValue("cid"), r.PathValue("sid")); !ok {
		return
	}
	job, ok := s.transcriptionInSession(w, r)
	if !ok {
		return
	}
	if job.Status != gamesession.TranscriptionFailed {
		writeError(w, http.StatusConflict, fmt.Errorf("only a failed transcription can be retried"))
		return
	}
	// The audio a retry needs must still exist.
	if _, err := os.Stat(job.AudioPath); err != nil {
		writeError(w, http.StatusGone, fmt.Errorf("the recording is gone — upload it again"))
		return
	}
	job.Status = gamesession.TranscriptionPending
	job.Error = ""
	job.FinishedAt = time.Time{}
	if err := s.sessions.UpdateTranscription(r.Context(), job); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.kickTranscriber()
	writeJSON(w, http.StatusAccepted, map[string]any{"transcription": toTranscriptionView(*job)})
}
