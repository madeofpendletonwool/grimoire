// Ingestion: turning uploaded files and pasted text into sources. Plain text
// and Markdown pass through verbatim; the subtitle formats (.srt, .vtt) are
// parsed to text plus a cue list, and the timing is preserved on the source
// row so a span can resolve back to a timestamp in the recording.

package gamesession

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Cue is one timed block of a subtitle-format source.
type Cue struct {
	StartMS int64  `json:"start_ms"`
	EndMS   int64  `json:"end_ms"`
	Text    string `json:"text"`
}

// Ingest formats. FormatText is also what a paste produces unless the caller
// says otherwise; FormatAuto sniffs.
const (
	FormatText = "text"
	FormatMD   = "md"
	FormatSRT  = "srt"
	FormatVTT  = "vtt"
	FormatAuto = ""
)

// maxCues caps a subtitle parse so a corrupt "file of one giant cue" cannot
// build an unbounded timing payload. Real session transcripts are thousands
// of cues at most.
const maxCues = 100_000

// Ingested is the result of parsing an upload: the verbatim-preserved text
// (what spans address) and, for subtitle formats, the cue list (what maps a
// span back to a timestamp).
type Ingested struct {
	Format string
	Text   string
	Timing []Cue // nil for plain text / markdown
}

// Ingest parses raw bytes by explicit format, sniffing when the format is
// empty or unknown. The extension helpers cover the upload path; the sniff
// covers a paste whose declared format does not match its body.
func Ingest(format string, data []byte) (Ingested, error) {
	text := normalizeNewlines(string(data))
	switch strings.ToLower(strings.TrimSpace(format)) {
	case FormatSRT:
		return parseSubtitles(text, false)
	case FormatVTT:
		return parseSubtitles(text, true)
	case FormatText, FormatMD:
		return Ingested{Format: FormatText, Text: text}, nil
	case FormatAuto:
		if sniffVTT(text) {
			return parseSubtitles(text, true)
		}
		if sniffSRT(text) {
			return parseSubtitles(text, false)
		}
		return Ingested{Format: FormatText, Text: text}, nil
	default:
		return Ingested{}, fmt.Errorf("%w: unknown ingest format %q", ErrInvalid, format)
	}
}

// IngestByFilename is the upload path: the extension decides. Unknown
// extensions fall through to the sniffer, so a mislabeled file still has a
// chance to land as what it actually is.
func IngestByFilename(name string, data []byte) (Ingested, error) {
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, ".srt"):
		return Ingest(FormatSRT, data)
	case strings.HasSuffix(lower, ".vtt"):
		return Ingest(FormatVTT, data)
	case strings.HasSuffix(lower, ".txt"), strings.HasSuffix(lower, ".md"):
		return Ingest(FormatText, data)
	default:
		return Ingest(FormatAuto, data)
	}
}

// normalizeNewlines strips a BOM and maps CRLF (and lone CR) to LF, so span
// offsets are against one canonical form of the text.
func normalizeNewlines(s string) string {
	s = strings.TrimPrefix(s, "\ufeff")
	if !strings.ContainsRune(s, '\r') {
		return s
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

/* ---------- the shared subtitle parser ---------- */

// tsRe matches an SRT or VTT timestamp: HH:MM:SS with , or . before the
// millis, or the VTT short MM:SS form. Captures h, m, s, ms.
var tsRe = regexp.MustCompile(`^(?:(\d{1,3}):)?(\d{1,2}):(\d{1,2})[,.](\d{1,3})$`)

// arrowRe matches a cue timing line: "<ts> --> <ts>" plus optional VTT cue
// settings (position:50%, line:0, align:start, ...) which are ignored.
var arrowRe = regexp.MustCompile(`^(\S+)\s+-->\s+(\S+)(?:\s.*)?$`)

// parseMS parses one timestamp; ok is false when it does not match.
func parseMS(v string) (int64, bool) {
	m := tsRe.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return 0, false
	}
	var ms int64
	if m[1] != "" {
		h, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			return 0, false
		}
		ms += h * 3600_000
	}
	mm, err1 := strconv.ParseInt(m[2], 10, 64)
	ss, err2 := strconv.ParseInt(m[3], 10, 64)
	frac, err3 := strconv.ParseInt(m[4], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return 0, false
	}
	// ".5" means 500ms, not 5ms: scale the fraction to three digits.
	for i := len(m[4]); i < 3; i++ {
		frac *= 10
	}
	ms += mm*60_000 + ss*1000 + frac
	return ms, true
}

// inlineTagRe strips VTT inline markup from cue text: <c.className>, <i>,
// <b>, <00:00:01.000> inline timestamps, and the like.
var inlineTagRe = regexp.MustCompile(`<[^>]*>`)

// parseSubtitles walks the cue blocks of an SRT or VTT body. The two formats
// share the timing line; the differences (WEBVTT header, NOTE blocks, cue
// ids, MM:SS timestamps) are all handled by the same lenient walk: any block
// containing a "-->" line is a cue, anything else (an SRT index line, a VTT
// cue id) is skipped. Malformed blocks are dropped, not fatal — a transcript
// with one hand-mangled cue in the middle is still a transcript.
func parseSubtitles(text string, vtt bool) (Ingested, error) {
	format := FormatSRT
	if vtt {
		format = FormatVTT
	}
	// A VTT header can carry metadata lines (Kind:, Language:) before the
	// first blank line; drop everything through it.
	body := text
	if vtt {
		body = strings.TrimPrefix(body, "WEBVTT")
		if i := strings.Index(body, "\n\n"); i >= 0 && !strings.Contains(body[:i], "-->") {
			body = body[i+2:]
		}
	}
	var (
		cues   []Cue
		blocks []string
		cur    []string
	)
	for _, line := range strings.Split(body, "\n") {
		if strings.TrimSpace(line) == "" {
			if len(cur) > 0 {
				blocks = append(blocks, strings.Join(cur, "\n"))
				cur = nil
			}
			continue
		}
		cur = append(cur, line)
	}
	if len(cur) > 0 {
		blocks = append(blocks, strings.Join(cur, "\n"))
	}

	var texts []string
	for _, block := range blocks {
		lines := strings.Split(block, "\n")
		// Find the timing line; the text is everything after it.
		ti := -1
		var m []string
		for i, ln := range lines {
			if mm := arrowRe.FindStringSubmatch(ln); mm != nil {
				ti, m = i, mm
				break
			}
		}
		if ti < 0 {
			continue // an SRT index block, a VTT NOTE, a stray id — not a cue
		}
		start, ok1 := parseMS(m[1])
		end, ok2 := parseMS(m[2])
		if !ok1 || !ok2 || end < start {
			continue
		}
		var tl []string
		for _, ln := range lines[ti+1:] {
			if strings.TrimSpace(ln) == "" {
				break
			}
			tl = append(tl, inlineTagRe.ReplaceAllString(ln, ""))
		}
		cueText := strings.Trim(strings.Join(tl, "\n"), " ")
		if cueText == "" {
			continue
		}
		if len(cues) >= maxCues {
			return Ingested{}, fmt.Errorf("%w: subtitle source exceeds %d cues", ErrInvalid, maxCues)
		}
		cues = append(cues, Cue{StartMS: start, EndMS: end, Text: cueText})
		texts = append(texts, cueText)
	}
	if len(cues) == 0 {
		return Ingested{}, fmt.Errorf("%w: no subtitle cues found — is this really an %s file?", ErrInvalid, format)
	}
	return Ingested{Format: format, Text: strings.Join(texts, "\n\n"), Timing: cues}, nil
}

/* ---------- sniffing ---------- */

// vttHeaderRe recognizes a WEBVTT file: the magic word on the first line,
// optionally followed by header text on the same line.
var vttHeaderRe = regexp.MustCompile(`^WEBVTT.*`)

func sniffVTT(text string) bool { return vttHeaderRe.MatchString(text) }

// srtArrowRe recognizes the load-bearing part of an SRT: a full
// HH:MM:SS,mmm --> HH:MM:SS,mmm timing line.
var srtArrowRe = regexp.MustCompile(`\d{1,3}:\d{2}:\d{2}[,.]\d{1,3}\s+-->\s+\d{1,3}:\d{2}:\d{2}[,.]\d{1,3}`)

func sniffSRT(text string) bool { return srtArrowRe.MatchString(text) }
