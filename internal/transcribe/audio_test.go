package transcribe

import (
	"encoding/binary"
	"strings"
	"testing"
)

// pcmWAV builds a minimal valid WAV: 16-bit mono 16kHz PCM, so tests can
// control the data-region size exactly.
func pcmWAV(dataBytes int) []byte {
	head := synthWAVHeader(wavLayout{
		audioFormat: 1, channels: 1, sampleRate: 16000,
		blockAlign: 2, bitsPerSample: 16,
	}, int64(dataBytes))
	return append(head, make([]byte, dataBytes)...)
}

func TestFormatByFilename(t *testing.T) {
	for name, want := range map[string]string{
		"session.MP3": "mp3", "a.wav": "wav", "b.OGG": "ogg", "c.m4a": "m4a",
		"d.webm": "webm", "e.opus": "opus", "f.flac": "flac", "g.aac": "aac",
	} {
		got, err := FormatByFilename(name)
		if err != nil || got != want {
			t.Errorf("FormatByFilename(%q) = %q, %v; want %q", name, got, err, want)
		}
	}
	for _, name := range []string{"notes.txt", "recording", "song.xyz", "track.ogg.jpg"} {
		if _, err := FormatByFilename(name); err == nil {
			t.Errorf("FormatByFilename(%q) accepted a non-audio file", name)
		} else if !strings.Contains(err.Error(), "accepted") {
			t.Errorf("error should name the accepted set: %v", err)
		}
	}
}

func TestPlanChunks_SmallFileIsOneChunk(t *testing.T) {
	for _, format := range AcceptedFormats {
		chunks, err := PlanChunks(format, nil, 1000, 24<<20)
		if err != nil || len(chunks) != 1 || chunks[0].Start != 0 || chunks[0].End != 1000 {
			t.Errorf("%s: chunks = %+v err = %v, want one whole-file chunk", format, chunks, err)
		}
	}
}

func TestPlanChunks_MP3SplitsAtBoundaries(t *testing.T) {
	chunks, err := PlanChunks("mp3", nil, 100, 30)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(chunks) != 4 {
		t.Fatalf("chunks = %d, want 4", len(chunks))
	}
	want := [][2]int{{0, 30}, {30, 60}, {60, 90}, {90, 100}}
	for i, w := range want {
		if chunks[i].Start != int64(w[0]) || chunks[i].End != int64(w[1]) {
			t.Errorf("chunk[%d] = [%d,%d), want %v", i, chunks[i].Start, chunks[i].End, w)
		}
		if chunks[i].Prefix != nil {
			t.Errorf("chunk[%d] should carry no prefix", i)
		}
	}
}

func TestPlanChunks_ContainersThatCannotSplitGoWhole(t *testing.T) {
	for _, format := range []string{"m4a", "mp4", "webm"} {
		chunks, err := PlanChunks(format, nil, 1000, 10)
		if err != nil {
			t.Fatalf("%s: %v", format, err)
		}
		if len(chunks) != 1 || chunks[0].End != 1000 {
			t.Errorf("%s: %d chunks — an index-based container must go whole", format, len(chunks))
		}
	}
}

func TestPlanChunks_WAVChunksAreCompleteFiles(t *testing.T) {
	wav := pcmWAV(200)
	chunks, err := PlanChunks("wav", wav, int64(len(wav)), 44+90)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(chunks) != 3 {
		t.Fatalf("chunks = %d, want 3 (90+90+20 bytes of data)", len(chunks))
	}
	var dataSeen int
	for i, ch := range chunks {
		// Assemble what the backend would receive — prefix + range — and
		// prove it parses as a complete, standalone WAV file.
		full := append(append([]byte{}, ch.Prefix...), wav[ch.Start:ch.End]...)
		lay, ok := parseWAV(full)
		if !ok || !lay.splittable {
			t.Fatalf("chunk %d is not a parseable PCM/float WAV file", i)
		}
		if lay.sampleRate != 16000 || lay.channels != 1 || lay.bitsPerSample != 16 {
			t.Errorf("chunk %d header lost the format: %+v", i, lay)
		}
		declared := int64(binary.LittleEndian.Uint32(ch.Prefix[40:44]))
		if declared != ch.End-ch.Start || lay.dataBytes != declared {
			t.Errorf("chunk %d declares %d data bytes (parsed %d), range is %d", i, declared, lay.dataBytes, ch.End-ch.Start)
		}
		dataSeen += int(ch.End - ch.Start)
	}
	if want := 200; dataSeen != want {
		t.Errorf("chunks cover %d data bytes, want %d", dataSeen, want)
	}
	// Boundaries sit on sample-frame (2-byte) alignment by construction.
	if chunks[0].End%2 != 0 {
		t.Errorf("first boundary %d not block-aligned", chunks[0].End)
	}
}

func TestPlanChunks_WAVDataSizeLieIsClamped(t *testing.T) {
	wav := pcmWAV(100)
	// A streaming-style header claiming 4GiB of data in a 144-byte file.
	binary.LittleEndian.PutUint32(wav[40:44], 0xFFFFFFFF)
	chunks, err := PlanChunks("wav", wav, int64(len(wav)), 44+40)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	var covered int
	for _, ch := range chunks {
		if ch.End > int64(len(wav)) {
			t.Fatalf("chunk end %d beyond the real file", ch.End)
		}
		covered += int(ch.End - ch.Start)
	}
	if covered != 100 {
		t.Errorf("covered %d data bytes, want 100", covered)
	}
}

func TestPlanChunks_NonPCMWAVGoesWhole(t *testing.T) {
	// ADPCM (format 6): splittable-by-header only covers PCM and float.
	head := synthWAVHeader(wavLayout{
		audioFormat: 6, channels: 1, sampleRate: 8000,
		blockAlign: 2, bitsPerSample: 4,
	}, 100)
	wav := append(head, make([]byte, 100)...)
	chunks, err := PlanChunks("wav", wav, int64(len(wav)), 64)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(chunks) != 1 {
		t.Errorf("non-PCM wav split into %d chunks; want whole-file", len(chunks))
	}
}

func TestPlanChunks_EmptyRecordingErrors(t *testing.T) {
	if _, err := PlanChunks("mp3", nil, 0, 1024); err == nil {
		t.Error("empty recording should error")
	}
}
