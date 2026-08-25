// Audio container knowledge: which formats the transcription hook accepts,
// how a long recording splits into sequentially-transcribable chunks, and the
// one container (WAV) whose chunks need a synthesized header to stay valid.
//
// Chunking is byte-wise, which is safe exactly where it is used: mp3, ADTS
// aac, ogg/opus and flac are frame/page streams whose decoders resynchronize
// after an arbitrary cut (losing at most one frame at each seam). WAV chunks
// get a fresh canonical header so each part is a complete file. The
// chunked-index containers (m4a/mp4, webm) cannot be split this way, so they
// are sent whole — fine for a local backend, and the upload cap rejects the
// unreasonable.

package transcribe

import (
	"encoding/binary"
	"fmt"
	"math"
	"path/filepath"
	"strings"
)

// The accepted container formats, keyed by extension.
const (
	FormatMP3  = "mp3"
	FormatWAV  = "wav"
	FormatOGG  = "ogg"
	FormatOGA  = "oga"
	FormatOPUS = "opus"
	FormatFLAC = "flac"
	FormatAAC  = "aac"
	FormatM4A  = "m4a"
	FormatMP4  = "mp4"
	FormatWEBM = "webm"
)

// AcceptedFormats is the whole list, for error messages and UI accept lists.
var AcceptedFormats = []string{
	FormatMP3, FormatWAV, FormatOGG, FormatOGA, FormatOPUS,
	FormatFLAC, FormatAAC, FormatM4A, FormatMP4, FormatWEBM,
}

// splittable formats survive a byte-wise cut: decoders resync on the next
// frame or page boundary.
var splittable = map[string]bool{
	FormatMP3: true, FormatOGG: true, FormatOGA: true,
	FormatOPUS: true, FormatFLAC: true, FormatAAC: true,
}

// FormatByFilename resolves an upload's container format from its extension.
// A missing or unaccepted extension is an error naming the accepted set —
// backends detect the container by extension, so guessing would only move
// the failure downstream.
func FormatByFilename(name string) (string, error) {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
	for _, f := range AcceptedFormats {
		if ext == f {
			return f, nil
		}
	}
	return "", fmt.Errorf("unsupported audio format %q — accepted: %s", ext, strings.Join(AcceptedFormats, ", "))
}

// mimeByFormat maps a container to the content type the multipart part
// should carry.
var mimeByFormat = map[string]string{
	FormatMP3: "audio/mpeg", FormatWAV: "audio/wav", FormatOGG: "audio/ogg",
	FormatOGA: "audio/ogg", FormatOPUS: "audio/ogg", FormatFLAC: "audio/flac",
	FormatAAC: "audio/aac", FormatM4A: "audio/mp4", FormatMP4: "audio/mp4",
	FormatWEBM: "audio/webm",
}

// MIMEByFilename returns the content type for an upload's extension,
// application/octet-stream when unknown (backends fall back to sniffing).
func MIMEByFilename(name string) string {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
	if mt, ok := mimeByFormat[ext]; ok {
		return mt
	}
	return "application/octet-stream"
}

// Chunk is one sequentially-transcribable piece of a recording: a byte range
// of the original file, plus a prefix to prepend when building the request
// (the synthesized WAV header; nil for stream formats).
type Chunk struct {
	Start  int64  `json:"start"` // byte offset into the recording
	End    int64  `json:"end"`   // end offset, exclusive
	Prefix []byte `json:"prefix,omitempty"`
}

// DefaultChunkBytes is the chunk target: under OpenAI's 25 MB per-request
// cap, big enough that a four-hour mp3 is a handful of requests, small
// enough that one request's transcript lands in a bounded save.
const DefaultChunkBytes = 24 << 20

// PlanChunks decides how a recording of the given format and size splits.
// head is the first bytes of the file (enough to carry a WAV header — a few
// KiB is plenty) and is only read for WAV. Anything at or under chunkBytes
// is one chunk; unsplittable formats are always one chunk.
func PlanChunks(format string, head []byte, size, chunkBytes int64) ([]Chunk, error) {
	if size <= 0 {
		return nil, fmt.Errorf("empty recording")
	}
	if chunkBytes <= 0 {
		chunkBytes = DefaultChunkBytes
	}
	one := []Chunk{{Start: 0, End: size}}
	if size <= chunkBytes {
		return one, nil
	}
	if !splittable[format] && format != FormatWAV {
		return one, nil // a chunked container sent whole; the local backends take it
	}
	if format == FormatWAV {
		layout, ok := parseWAV(head)
		if !ok || !layout.splittable {
			return one, nil // not PCM/float or unparseable: whole file, no seam surgery
		}
		return planWAVChunks(layout, size, chunkBytes), nil
	}
	var chunks []Chunk
	for off := int64(0); off < size; off += chunkBytes {
		end := off + chunkBytes
		if end > size {
			end = size
		}
		chunks = append(chunks, Chunk{Start: off, End: end})
	}
	return chunks, nil
}

/* ---------- WAV ---------- */

// wavLayout is what a chunked WAV split needs from the original header.
type wavLayout struct {
	audioFormat   uint16 // 1 = PCM, 3 = IEEE float — both splittable
	channels      uint16
	sampleRate    uint32
	bitsPerSample uint16
	blockAlign    uint16
	dataOffset    int64
	dataBytes     int64
	splittable    bool
}

// parseWAV walks the RIFF chunk list for "fmt " and "data". Lenient about a
// data size that runs past the actual file (some writers put 0xFFFFFFFF
// there for streaming): it is clamped by the caller's real size.
func parseWAV(b []byte) (wavLayout, bool) {
	var lay wavLayout
	if len(b) < 12 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return lay, false
	}
	pos := int64(12)
	for pos+8 <= int64(len(b)) {
		id := string(b[pos : pos+4])
		size := int64(binary.LittleEndian.Uint32(b[pos+4 : pos+8]))
		body := pos + 8
		if body+size > int64(len(b)) {
			size = int64(len(b)) - body // truncated final chunk
		}
		switch id {
		case "fmt ":
			if size < 16 {
				return lay, false
			}
			lay.audioFormat = binary.LittleEndian.Uint16(b[body:])
			lay.channels = binary.LittleEndian.Uint16(b[body+2:])
			lay.sampleRate = binary.LittleEndian.Uint32(b[body+4:])
			lay.blockAlign = binary.LittleEndian.Uint16(b[body+12:])
			lay.bitsPerSample = binary.LittleEndian.Uint16(b[body+14:])
		case "data":
			lay.dataOffset = body
			lay.dataBytes = size
		}
		pos = body + size
		if size%2 == 1 {
			pos++ // chunks are word-aligned
		}
	}
	if lay.dataOffset == 0 || lay.audioFormat == 0 {
		return lay, false
	}
	lay.splittable = lay.audioFormat == 1 || lay.audioFormat == 3
	return lay, lay.splittable
}

// planWAVChunks splits the data region on blockAlign boundaries (no partial
// sample frames) and gives each chunk its own canonical header.
func planWAVChunks(lay wavLayout, size, chunkBytes int64) []Chunk {
	dataBytes := lay.dataBytes
	if lay.dataOffset+dataBytes > size {
		dataBytes = size - lay.dataOffset // the header lied; trust the file
	}
	if dataBytes <= 0 || lay.blockAlign == 0 {
		return []Chunk{{Start: 0, End: size}}
	}
	// Split the sample data, not the header bytes: max data per chunk is the
	// chunk budget minus the 44-byte header each part carries.
	maxData := chunkBytes - 44
	maxData -= maxData % int64(lay.blockAlign)
	if maxData < int64(lay.blockAlign) {
		return []Chunk{{Start: 0, End: size}}
	}
	var chunks []Chunk
	for off := int64(0); off < dataBytes; off += maxData {
		end := off + maxData
		if end > dataBytes {
			end = dataBytes
		}
		chunks = append(chunks, Chunk{
			Start:  lay.dataOffset + off,
			End:    lay.dataOffset + end,
			Prefix: synthWAVHeader(lay, end-off),
		})
	}
	return chunks
}

// synthWAVHeader writes the canonical 44-byte header for the layout's format
// with a data payload of dataLen bytes. Only PCM and IEEE float layouts
// reach here (parseWAV's splittable gate).
func synthWAVHeader(lay wavLayout, dataLen int64) []byte {
	byteRate := lay.sampleRate * uint32(lay.blockAlign)
	if dataLen > math.MaxUint32 {
		dataLen = math.MaxUint32 // unreachable with bounded chunks; keep the header honest anyway
	}
	b := make([]byte, 44)
	copy(b[0:4], "RIFF")
	binary.LittleEndian.PutUint32(b[4:8], uint32(36+dataLen))
	copy(b[8:12], "WAVE")
	copy(b[12:16], "fmt ")
	binary.LittleEndian.PutUint32(b[16:20], 16)
	binary.LittleEndian.PutUint16(b[20:22], lay.audioFormat)
	binary.LittleEndian.PutUint16(b[22:24], lay.channels)
	binary.LittleEndian.PutUint32(b[24:28], lay.sampleRate)
	binary.LittleEndian.PutUint32(b[28:32], byteRate)
	binary.LittleEndian.PutUint16(b[32:34], lay.blockAlign)
	binary.LittleEndian.PutUint16(b[34:36], lay.bitsPerSample)
	copy(b[36:40], "data")
	binary.LittleEndian.PutUint32(b[40:44], uint32(dataLen))
	return b
}

// fileHeader builds the multipart part header for the audio file, carrying a
// real content type instead of CreateFormFile's octet-stream — backends that
// dispatch on content type rather than extension need it.
func fileHeader(filename, contentType string) map[string][]string {
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	return map[string][]string{
		"content-disposition": {fmt.Sprintf(`form-data; name="file"; filename=%q`, filename)},
		"content-type":        {contentType},
	}
}
