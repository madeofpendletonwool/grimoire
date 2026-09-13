package server

// The push-to-talk transcription surface over HTTP (MAD-332): a held
// button's clip in, its transcript out, in one request. These tests
// assert the HTTP half — the configured/unconfigured contract, the
// account scope, and the upload guards. The endpoint client itself is
// covered by internal/transcribe's own suite; the fixture here only
// needs to speak the same wire shape.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/transcribe"
)

// newTalkServer wires the games surface with the transcription seam the
// way an install with TRANSCRIBE_MODEL set has it: one fake OpenAI-shaped
// endpoint, one clip-sized request.
func newTalkServer(t *testing.T) (*Server, *fakeTranscriber) {
	t.Helper()
	fake := &fakeTranscriber{}
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)
	s, _ := newIntentServer(t, nil)
	s = s.WithTranscriber(transcribe.New(transcribe.Config{
		BaseURL: srv.URL + "/v1", Model: "whisper-fixture",
	}), TranscribeOptions{Dir: t.TempDir()})
	return s, fake
}

// postClip uploads a talk clip the way the hold-to-talk button does.
func postClip(t *testing.T, s *Server, cookie *http.Cookie, game, filename string, audio []byte) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if filename != "" {
		fw, _ := mw.CreateFormFile("file", filename)
		_, _ = fw.Write(audio)
	}
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/games/"+game+"/transcribe", &buf)
	req.Header.Set("content-type", mw.FormDataContentType())
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func TestGameTranscribeClip(t *testing.T) {
	s, fake := newTalkServer(t)
	admin := adminSession(t, s)
	game := gameCreate(t, s, admin)

	clip := []byte("fake webm bytes a held button might produce")
	rec, body := postClip(t, s, admin, game, "clip.webm", clip)
	if rec.Code != http.StatusOK {
		t.Fatalf("transcribe: status %d body %s", rec.Code, rec.Body)
	}
	if body["text"] != "DM: The door grinds open. Mira: I check for traps." {
		t.Errorf("text = %v, want the fixture transcript", body["text"])
	}
	calls, bodies := fake.snapshot()
	if calls != 1 {
		t.Fatalf("endpoint calls = %d, want 1", calls)
	}
	if !bytes.Equal(bodies[0], clip) {
		t.Errorf("endpoint received %d bytes, want the clip verbatim (%d)", len(bodies[0]), len(clip))
	}
}

func TestGameTranscribeNotConfigured(t *testing.T) {
	// The transcriber unwired: the route answers 503, the same
	// "affordance absent" contract the campaign hook keeps.
	s, _ := newIntentServer(t, nil)
	admin := adminSession(t, s)
	game := gameCreate(t, s, admin)
	rec, _ := postClip(t, s, admin, game, "clip.webm", []byte("x"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("unconfigured: status %d, want 503", rec.Code)
	}
}

func TestGameTranscribeScope(t *testing.T) {
	s, _ := newTalkServer(t)
	admin := adminSession(t, s)
	game := gameCreate(t, s, admin)
	friend := gameFriend(t, s, admin)
	rec, _ := postClip(t, s, friend, game, "clip.webm", []byte("x"))
	if rec.Code != http.StatusNotFound {
		t.Errorf("friend clip: status %d, want 404", rec.Code)
	}
	rec, _ = postClip(t, s, admin, "no-such-game", "clip.webm", []byte("x"))
	if rec.Code != http.StatusNotFound {
		t.Errorf("missing game: status %d, want 404", rec.Code)
	}
}

func TestGameTranscribeUploadGuards(t *testing.T) {
	s, _ := newTalkServer(t)
	admin := adminSession(t, s)
	game := gameCreate(t, s, admin)

	// No file field at all.
	rec, _ := postClip(t, s, admin, game, "", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("no file: status %d, want 400", rec.Code)
	}
	// An extension no backend understands — guessing would only move
	// the failure downstream.
	rec, _ = postClip(t, s, admin, game, "clip.exe", []byte("x"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad extension: status %d, want 400", rec.Code)
	}
	// A present-but-empty clip is a client bug, not silence to bill.
	rec, body := postClip(t, s, admin, game, "clip.webm", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty clip: status %d, want 400", rec.Code)
	}
	if fmt.Sprint(body["error"]) == "" {
		t.Errorf("empty clip: want an error saying so, got %v", body)
	}
	// Over the clip cap: no endpoint call, a clear 413.
	over := make([]byte, talkClipCap+1)
	rec, _ = postClip(t, s, admin, game, "clip.webm", over)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized: status %d, want 413", rec.Code)
	}
}
