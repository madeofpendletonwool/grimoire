package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/gamesession"
	"github.com/madeofpendletonwool/grimoire/internal/transcribe"
)

// fixtureSegments is what a verbose_json backend returns: two timed segments
// covering the session's iconic line.
const fixtureSegments = `[
	{"id":0,"start":1.0,"end":4.0,"text":" DM: The door grinds open. "},
	{"id":1,"start":5.5,"end":9.25,"text":" Mira: I check for traps. "}
]`

// fakeTranscriber is an httptest endpoint speaking the OpenAI transcription
// shape, recording what it received. failFrom, when nonzero, makes that call
// and every one after it answer 503 — a deterministic mid-job outage. The
// mutex matters: the handler runs on the server's goroutine while the test
// flips failFrom and reads calls (the suite runs -race).
type fakeTranscriber struct {
	mu       sync.Mutex
	calls    int
	bodies   [][]byte
	failFrom int
}

func (f *fakeTranscriber) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeTranscriber) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/audio/transcriptions", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls++
		fail := f.failFrom > 0 && f.calls >= f.failFrom
		f.mu.Unlock()
		if fail {
			http.Error(w, "endpoint down", http.StatusServiceUnavailable)
			return
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "no file", http.StatusBadRequest)
			return
		}
		defer file.Close()
		body, _ := io.ReadAll(file)
		f.mu.Lock()
		f.bodies = append(f.bodies, body)
		f.mu.Unlock()
		w.Header().Set("content-type", "application/json")
		fmt.Fprintf(w, `{"text":"DM: The door grinds open. Mira: I check for traps.","segments":%s}`, fixtureSegments)
	})
	return mux
}

// snapshot copies the counters under the lock.
func (f *fakeTranscriber) snapshot() (int, [][]byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	bodies := make([][]byte, len(f.bodies))
	copy(bodies, f.bodies)
	return f.calls, bodies
}

// newTranscribeServer extends newSessionServer with the transcription hook
// pointed at a fake endpoint and an audio dir under t.TempDir().
func newTranscribeServer(t *testing.T, opts TranscribeOptions) (*Server, *fakeTranscriber) {
	t.Helper()
	fake := &fakeTranscriber{}
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)
	s := newSessionServer(t)
	if opts.Dir == "" {
		opts.Dir = t.TempDir()
	}
	s = s.WithTranscriber(transcribe.New(transcribe.Config{
		BaseURL: srv.URL + "/v1", Model: "whisper-fixture",
	}), opts)
	return s, fake
}

// uploadAudio posts a recording the way the UI's audio button does.
func uploadAudio(t *testing.T, s *Server, cid, sid, filename string, audio []byte) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", filename)
	_, _ = fw.Write(audio)
	_ = mw.WriteField("author", "DM")
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/campaigns/%s/sessions/%s/transcriptions", cid, sid), &buf)
	req.Header.Set("content-type", mw.FormDataContentType())
	req.AddCookie(sessions[s])
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// waitJob polls a job until it reaches a terminal status or the deadline.
func waitJob(t *testing.T, s *Server, cid, sid, tid string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		code, body := do(t, s, http.MethodGet,
			fmt.Sprintf("/api/campaigns/%s/sessions/%s/transcriptions/%s", cid, sid, tid), "")
		if code != http.StatusOK {
			t.Fatalf("poll job: status %d body %v", code, body)
		}
		job, _ := body["transcription"].(map[string]any)
		switch job["status"] {
		case "completed", "failed", "cancelled":
			return job
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("job did not finish within the deadline")
	return nil
}

// The acceptance walk: upload → job → transcript source → a span resolves
// back to its quoted text, with timings kept on the source row.
func TestTranscriptionLandsTimedTranscriptSource(t *testing.T) {
	s, fake := newTranscribeServer(t, TranscribeOptions{})
	cid := newCampaignAPI(t, s, "one")
	sid := newSessionAPI(t, s, cid, "The Ambush")

	audio := bytes.Repeat([]byte("not-really-mp3-"), 64)
	code, body := uploadAudio(t, s, cid, sid, "session-12.mp3", audio)
	if code != http.StatusAccepted {
		t.Fatalf("upload: status %d body %v", code, body)
	}
	job, _ := body["transcription"].(map[string]any)
	tid, _ := job["id"].(string)
	if tid == "" || job["chunks_total"] != float64(1) {
		t.Fatalf("job view = %v", job)
	}

	final := waitJob(t, s, cid, sid, tid)
	if final["status"] != "completed" {
		t.Fatalf("job = %v", final)
	}
	sourceID, _ := final["source_id"].(string)
	if sourceID == "" {
		t.Fatal("completed job carries no source_id")
	}
	if calls, _ := fake.snapshot(); calls != 1 {
		t.Errorf("endpoint calls = %d, want 1 for a single-chunk file", calls)
	}

	// The source is an ordinary timed transcript, shaped like an .srt
	// upload — which is the whole invariant of the hook.
	code, body = do(t, s, http.MethodGet,
		fmt.Sprintf("/api/campaigns/%s/sessions/%s/sources/%s", cid, sid, sourceID), "")
	if code != http.StatusOK {
		t.Fatalf("get source: %d %v", code, body)
	}
	src, _ := body["source"].(map[string]any)
	if src["kind"] != "transcript" || src["timed"] != true {
		t.Fatalf("source view = %v", src)
	}
	timing, _ := body["timing"].([]any)
	if len(timing) != 2 {
		t.Fatalf("timing = %v, want 2 cues", timing)
	}
	content, _ := src["content"].(string)
	if !strings.Contains(content, "I check for traps") {
		t.Fatalf("content = %q", content)
	}

	// A span resolves back to its quoted text, and the cue offsets put it
	// at a real timestamp in the recording.
	code, body = do(t, s, http.MethodGet,
		fmt.Sprintf("/api/campaigns/%s/sessions/%s/span?source_id=%s&quote=%s",
			cid, sid, sourceID, url.QueryEscape("door grinds open")), "")
	if code != http.StatusOK {
		t.Fatalf("locate span: %d %v", code, body)
	}
	span, _ := body["span"].(map[string]any)
	if span["quote"] != "door grinds open" {
		t.Fatalf("span = %v", span)
	}
	firstCue, _ := timing[0].(map[string]any)
	if firstCue["start_ms"] != float64(1000) || firstCue["text"] != "DM: The door grinds open." {
		t.Fatalf("first cue = %v", firstCue)
	}
}

func TestTranscriptionAudioDeletedAfterSuccess(t *testing.T) {
	dir := t.TempDir()
	s, _ := newTranscribeServer(t, TranscribeOptions{Dir: dir})
	cid := newCampaignAPI(t, s, "one")
	sid := newSessionAPI(t, s, cid, "one")

	_, body := uploadAudio(t, s, cid, sid, "a.mp3", []byte("mp3data"))
	job, _ := body["transcription"].(map[string]any)
	final := waitJob(t, s, cid, sid, job["id"].(string))
	if final["status"] != "completed" {
		t.Fatalf("job = %v", final)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("audio dir still holds %d file(s) after success; the retention rule is off by default", len(entries))
	}

	// Opt-in keeps it.
	s2, _ := newTranscribeServer(t, TranscribeOptions{Dir: dir, KeepAudio: true})
	cid2 := newCampaignAPI(t, s2, "two")
	sid2 := newSessionAPI(t, s2, cid2, "two")
	_, body = uploadAudio(t, s2, cid2, sid2, "a.mp3", []byte("mp3data"))
	job, _ = body["transcription"].(map[string]any)
	final = waitJob(t, s2, cid2, sid2, job["id"].(string))
	if final["status"] != "completed" {
		t.Fatalf("keep-audio job = %v", final)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("KeepAudio lost the recording: %d files", len(entries))
	}
}

func TestTranscriptionChunkedAndResumable(t *testing.T) {
	s, fake := newTranscribeServer(t, TranscribeOptions{ChunkBytes: 100})
	cid := newCampaignAPI(t, s, "one")
	sid := newSessionAPI(t, s, cid, "one")

	audio := bytes.Repeat([]byte("x"), 250) // three chunks at 100 bytes
	code, body := uploadAudio(t, s, cid, sid, "long.mp3", audio)
	if code != http.StatusAccepted {
		t.Fatalf("upload: %d %v", code, body)
	}
	job, _ := body["transcription"].(map[string]any)
	if job["chunks_total"] != float64(3) {
		t.Fatalf("chunks_total = %v, want 3", job["chunks_total"])
	}
	final := waitJob(t, s, cid, sid, job["id"].(string))
	if final["status"] != "completed" {
		t.Fatalf("job = %v", final)
	}
	if calls, bodies := fake.snapshot(); calls != 3 {
		t.Fatalf("endpoint calls = %d, want 3 (one per chunk)", calls)
	} else {
		for i, b := range bodies {
			want := 100
			if i == 2 {
				want = 50 // the remainder
			}
			if len(b) != want {
				t.Errorf("chunk %d body = %d bytes, want %d", i, len(b), want)
			}
		}
	}
	// Three chunks × two cues each, with the second chunk's cues offset by
	// the first chunk's measured duration (9250ms).
	sourceID, _ := final["source_id"].(string)
	_, body = do(t, s, http.MethodGet,
		fmt.Sprintf("/api/campaigns/%s/sessions/%s/sources/%s", cid, sid, sourceID), "")
	timing, _ := body["timing"].([]any)
	if len(timing) != 6 {
		t.Fatalf("cues = %d, want 6", len(timing))
	}
	third, _ := timing[2].(map[string]any)
	if third["start_ms"] != float64(9250+1000) {
		t.Fatalf("chunk 2 cue start = %v, want offset 9250ms applied", third["start_ms"])
	}

	// The resume half of the contract: a job whose first chunk is already
	// done only asks the endpoint for the rest. Simulate the interruption
	// by building the ledger directly, the way a crash mid-job would leave
	// it.
	audioPath := filepath.Join(s.transcribeOpts.Dir, "resume.mp3")
	if err := os.WriteFile(audioPath, audio, 0o600); err != nil {
		t.Fatal(err)
	}
	sid2 := newSessionAPI(t, s, cid, "second")
	resumed := &gamesession.TranscriptionJob{
		SessionID: sid2, Filename: "long.mp3", Format: "mp3",
		AudioPath: audioPath, AudioBytes: int64(len(audio)),
		ChunksTotal: 3, ChunksDone: 1,
		Chunks: []gamesession.TranscriptionChunk{
			{Start: 0, End: 100, OffsetMS: 9250, Done: true,
				Text:     "DM: The door grinds open. Mira: I check for traps.",
				Segments: []gamesession.Cue{{StartMS: 1000, EndMS: 4000, Text: "DM: The door grinds open."}, {StartMS: 5500, EndMS: 9250, Text: "Mira: I check for traps."}}},
			{Start: 100, End: 200}, {Start: 200, End: 250},
		},
	}
	if err := s.sessions.CreateTranscription(t.Context(), resumed); err != nil {
		t.Fatal(err)
	}
	before := fake.count()
	s.kickTranscriber()
	tid := resumed.ID
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		job2, err := s.sessions.GetTranscription(t.Context(), tid)
		if err == nil && job2.Status == gamesession.TranscriptionCompleted {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := fake.count() - before; got != 2 {
		t.Fatalf("resume asked the endpoint %d times, want 2 (chunk 0 is already done)", got)
	}
}

func TestTranscriptionDurationCapFailsClearly(t *testing.T) {
	dir := t.TempDir()
	s, _ := newTranscribeServer(t, TranscribeOptions{Dir: dir, MaxDuration: 5 * time.Second})
	cid := newCampaignAPI(t, s, "one")
	sid := newSessionAPI(t, s, cid, "one")

	// The fixture's second cue ends at 9250ms > the 5s cap.
	_, body := uploadAudio(t, s, cid, sid, "a.mp3", []byte("mp3data"))
	job, _ := body["transcription"].(map[string]any)
	final := waitJob(t, s, cid, sid, job["id"].(string))
	if final["status"] != "failed" {
		t.Fatalf("job = %v, want failed", final)
	}
	if msg, _ := final["error"].(string); !strings.Contains(msg, "duration cap") {
		t.Fatalf("error = %q, want the duration-cap message", msg)
	}
	if final["source_id"] != nil {
		t.Error("a too-long recording must not produce a source")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("audio kept after a duration-cap failure: %d files", len(entries))
	}
}

func TestTranscriptionUploadCapIs413(t *testing.T) {
	s, _ := newTranscribeServer(t, TranscribeOptions{MaxUploadBytes: 1024})
	cid := newCampaignAPI(t, s, "one")
	sid := newSessionAPI(t, s, cid, "one")

	code, body := uploadAudio(t, s, cid, sid, "big.mp3", bytes.Repeat([]byte("x"), 4096))
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized upload: %d %v", code, body)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "upload cap") {
		t.Errorf("error = %q, want the cap message", msg)
	}
}

func TestTranscriptionRejectsNonAudio(t *testing.T) {
	s, _ := newTranscribeServer(t, TranscribeOptions{})
	cid := newCampaignAPI(t, s, "one")
	sid := newSessionAPI(t, s, cid, "one")

	code, body := uploadAudio(t, s, cid, sid, "notes.txt", []byte("text"))
	if code != http.StatusBadRequest {
		t.Fatalf("txt upload: %d %v", code, body)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "accepted") {
		t.Errorf("error = %q, want the accepted-formats message", msg)
	}
}

func TestTranscriptionFailureIsRetryableAndSkipsDoneChunks(t *testing.T) {
	s, fake := newTranscribeServer(t, TranscribeOptions{ChunkBytes: 100, Dir: t.TempDir()})
	cid := newCampaignAPI(t, s, "one")
	sid := newSessionAPI(t, s, cid, "one")

	_, body := uploadAudio(t, s, cid, sid, "long.mp3", bytes.Repeat([]byte("x"), 150))
	job, _ := body["transcription"].(map[string]any)
	tid, _ := job["id"].(string)

	// Chunk 0 lands; call 2 answers 503, so the job fails with its first
	// chunk already in the ledger.
	fake.failFrom = 2
	final := waitJob(t, s, cid, sid, tid)
	if final["status"] != "failed" {
		t.Fatalf("job = %v, want failed after the endpoint died", final)
	}

	// Retry resumes at the failed chunk rather than from zero.
	fake.failFrom = 0
	before := fake.count()
	code, body := do(t, s, http.MethodPost,
		fmt.Sprintf("/api/campaigns/%s/sessions/%s/transcriptions/%s/retry", cid, sid, tid), "")
	if code != http.StatusAccepted {
		t.Fatalf("retry: %d %v", code, body)
	}
	final = waitJob(t, s, cid, sid, tid)
	if final["status"] != "completed" {
		t.Fatalf("retried job = %v", final)
	}
	if got := fake.count() - before; got != 1 {
		t.Fatalf("retry asked the endpoint %d times, want 1 (chunk 0 was done)", got)
	}
	if _, ok := final["source_id"]; !ok {
		t.Fatal("retried job produced no source")
	}
}

func TestTranscriptionDeleteCancelsRunningJob(t *testing.T) {
	dir := t.TempDir()
	s, _ := newTranscribeServer(t, TranscribeOptions{Dir: dir})
	cid := newCampaignAPI(t, s, "one")
	sid := newSessionAPI(t, s, cid, "one")

	// A slow endpoint keeps the job running long enough to cancel.
	block := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
		_, _ = w.Write([]byte(`{"text":"late"}`))
	}))
	defer slow.Close()
	s.transcribe = transcribe.New(transcribe.Config{BaseURL: slow.URL + "/v1", Model: "m"})

	_, body := uploadAudio(t, s, cid, sid, "a.mp3", []byte("mp3data"))
	job, _ := body["transcription"].(map[string]any)
	tid, _ := job["id"].(string)

	// Wait until the job is running, then cancel it.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got, err := s.sessions.GetTranscription(t.Context(), tid)
		if err == nil && got.Status == gamesession.TranscriptionRunning {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	code, _ := do(t, s, http.MethodDelete,
		fmt.Sprintf("/api/campaigns/%s/sessions/%s/transcriptions/%s", cid, sid, tid), "")
	if code != http.StatusNoContent {
		t.Fatalf("cancel: %d", code)
	}
	close(block) // let the in-flight request finish; the cancel takes effect after it
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got, err := s.sessions.GetTranscription(t.Context(), tid)
		if err == nil && got.Status == gamesession.TranscriptionCancelled && !got.FinishedAt.IsZero() {
			return // cancelled and cleaned up
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("job never settled as cancelled")
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("cancelled job left %d audio file(s)", len(entries))
	}
}

func TestTranscriptionUnconfiguredIsAbsent(t *testing.T) {
	s := newSessionServer(t) // no WithTranscriber
	cid := newCampaignAPI(t, s, "one")
	sid := newSessionAPI(t, s, cid, "one")

	// The route answers 503, not a degraded path.
	code, body := uploadAudio(t, s, cid, sid, "a.mp3", []byte("x"))
	if code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured upload: %d %v", code, body)
	}

	// And the affordance is reported absent to the UI.
	code, body = do(t, s, http.MethodGet, "/api/meta", "")
	if code != http.StatusOK {
		t.Fatalf("meta: %d", code)
	}
	if body["transcribe_configured"] != false || body["transcribe_model"] != "" {
		t.Fatalf("meta = %v", body)
	}
}

func TestTranscriptionGatedToDM(t *testing.T) {
	s, _ := newTranscribeServer(t, TranscribeOptions{})
	cid := newCampaignAPI(t, s, "one")
	sid := newSessionAPI(t, s, cid, "one")

	// A real player member (the TestPlayerSeesLessThanTheDM registration
	// walk) is refused the DM's ingest mechanics.
	admin := adminSession2(t, s)
	inv := createInvite(t, s, admin, "")
	code0, _ := inv["code"].(string)
	rec := call(s, http.MethodPost, "/api/auth/register", registerJSON("mira", "a-fine-passphrase", code0))
	if rec.Code != http.StatusCreated {
		t.Fatalf("register player: %d %s", rec.Code, rec.Body)
	}
	player := sessionFrom(t, rec)
	u, err := s.users.Authenticate(t.Context(), "mira", "a-fine-passphrase")
	if err != nil {
		t.Fatalf("authenticate player: %v", err)
	}
	if err := s.campaigns.AddMember(t.Context(), cid, u.ID, campaign.RolePlayer, ""); err != nil {
		t.Fatalf("add member: %v", err)
	}

	for _, tc := range []struct{ method, target string }{
		{http.MethodGet, fmt.Sprintf("/api/campaigns/%s/sessions/%s/transcriptions", cid, sid)},
		{http.MethodPost, fmt.Sprintf("/api/campaigns/%s/sessions/%s/transcriptions", cid, sid)},
	} {
		req := httptest.NewRequest(tc.method, tc.target, nil)
		req.AddCookie(player)
		r2 := httptest.NewRecorder()
		s.Handler().ServeHTTP(r2, req)
		if r2.Code != http.StatusForbidden {
			t.Errorf("%s transcriptions: status %d, want 403 (the DM's seat only)", tc.method, r2.Code)
		}
	}
}
