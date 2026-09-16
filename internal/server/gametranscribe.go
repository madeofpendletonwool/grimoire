package server

// The push-to-talk transcription half (MAD-332, stage 4 of MAD-321): one
// short spoken clip in, its transcript out — the server-side voice path
// for browsers without the Web Speech API (Firefox, Safari). The client
// pairs it with the same intent endpoint the typed say strip uses; this
// surface never touches game state, it only turns audio into words.
//
//	POST /api/games/{id}/transcribe  multipart file=clip.webm → {text}
//
// The ADR 5 seam serves two consumers from one configured endpoint: the
// campaign audio hook (long recordings, chunked, a resumable job) and
// this (a held button's worth of audio, transcribed in-request). The
// difference in shape is the whole point — a push-to-talk clip is
// seconds, so there is no job to resume, no ledger, and no disk: the
// bytes go from the request to the endpoint and stop existing.
//
// Unconfigured means the affordance is not there: the route answers 503
// and /api/meta already reports transcribe_configured, which is what the
// client keys on to hide the button entirely.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/transcribe"
)

// talkClipCap bounds one push-to-talk upload. A held button's worth of
// audio is seconds — even uncompressed WAV runs ~100 KB/s — so 16 MiB is
// generous while still refusing anything a hold could not produce.
const talkClipCap = 16 << 20

// talkClipTimeout bounds one clip's transcription. The transcriber's own
// timeout is sized for tens of minutes of session audio; a talk clip
// wants an answer in seconds, and a table that has released the button
// should not wait on a wedged backend longer than it would wait on a
// slow player.
const talkClipTimeout = 90 * time.Second

// handleGameTranscribe is the hold-to-talk release: the clip the client
// recorded goes to the configured endpoint and the transcript comes
// back, all in one request. The clip is never persisted — unlike the
// session hook there is no retry to serve, and a spoken clip is exactly
// as sensitive as it is short-lived.
func (s *Server) handleGameTranscribe(w http.ResponseWriter, r *http.Request) {
	if !s.gamesEnabled(w) {
		return
	}
	g, _ := s.resolveGameAny(w, r)
	if g == nil {
		return
	}
	if s.transcribe == nil || !s.transcribe.Configured() {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("audio transcription is not configured (set TRANSCRIBE_BASE_URL and TRANSCRIBE_MODEL)"))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, talkClipCap+(1<<20))
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		writeError(w, http.StatusRequestEntityTooLarge,
			fmt.Errorf("clip too large or malformed (max %d MB)", talkClipCap>>20))
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("file field is required"))
		return
	}
	defer file.Close()
	if _, err := transcribe.FormatByFilename(header.Filename); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	// Buffer the clip rather than streaming the part: the multipart
	// reader is single-pass, the cap needs enforcing before the endpoint
	// call, and nothing here is large enough for streaming to matter.
	audio, err := io.ReadAll(io.LimitReader(file, talkClipCap+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("read clip: %v", err))
		return
	}
	if len(audio) > talkClipCap {
		writeError(w, http.StatusRequestEntityTooLarge,
			fmt.Errorf("clip exceeds the %d MB push-to-talk cap", talkClipCap>>20))
		return
	}
	if len(audio) == 0 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("the clip is empty"))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), talkClipTimeout)
	defer cancel()
	res, err := s.transcribe.Transcribe(ctx, bytes.NewReader(audio), header.Filename, "")
	if err != nil {
		writeGameError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"text": res.Text})
}
