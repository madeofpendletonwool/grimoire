package gamesession

import (
	"strings"
	"testing"
)

const srtFixture = "1\n" +
	"00:00:01,000 --> 00:00:04,000\n" +
	"DM: The door grinds open.\n" +
	"\n" +
	"2\n" +
	"00:00:05,500 --> 00:00:09,250\n" +
	"Mira: I check for traps.\n" +
	"\n" +
	"3\n" +
	"00:01:02,000 --> 00:01:05,000\n" +
	"DM: You find none — but the dust\n" +
	"on the floor is disturbed.\n"

func TestIngestSRT(t *testing.T) {
	got, err := Ingest(FormatSRT, []byte(srtFixture))
	if err != nil {
		t.Fatalf("srt: %v", err)
	}
	if got.Format != FormatSRT {
		t.Fatalf("format = %s", got.Format)
	}
	if len(got.Timing) != 3 {
		t.Fatalf("cues = %d; want 3", len(got.Timing))
	}
	if got.Timing[1].StartMS != 5500 || got.Timing[1].EndMS != 9250 {
		t.Fatalf("cue 2 timing = %+v", got.Timing[1])
	}
	// A cue's wrapped lines keep their inner newline; the cues themselves are
	// joined into the text with blank lines.
	if want := "DM: You find none — but the dust\non the floor is disturbed."; got.Timing[2].Text != want {
		t.Fatalf("cue 3 text = %q; want %q", got.Timing[2].Text, want)
	}
	if !strings.Contains(got.Text, "Mira: I check for traps.") {
		t.Fatalf("text missing a cue: %q", got.Text)
	}
	if !strings.Contains(got.Text, "the floor is disturbed.") {
		t.Fatalf("text missing a wrapped cue line: %q", got.Text)
	}
	// The parsed text is what spans address, so cue order and paragraph
	// breaks are deterministic: cues joined by blank lines.
	if strings.Count(got.Text, "\n\n") != 2 {
		t.Fatalf("cue separation = %q", got.Text)
	}
}

const vttFixture = "WEBVTT - session recording\n" +
	"Kind: captions\n" +
	"Language: en\n" +
	"\n" +
	"NOTE\n" +
	"This section is a comment.\n" +
	"\n" +
	"intro\n" +
	"00:01.000 --> 00:04.000 align:start position:50%\n" +
	"DM: Welcome back.\n" +
	"\n" +
	"00:00:10.000 --> 00:00:12.500\n" +
	"<c>Bran</c> rolls a <i>natural twenty</i> at <00:00:11.000>exactly the right moment.\n"

func TestIngestVTT(t *testing.T) {
	got, err := Ingest(FormatVTT, []byte(vttFixture))
	if err != nil {
		t.Fatalf("vtt: %v", err)
	}
	if got.Format != FormatVTT {
		t.Fatalf("format = %s", got.Format)
	}
	if len(got.Timing) != 2 {
		t.Fatalf("cues = %d; want 2 (NOTE skipped)", len(got.Timing))
	}
	if got.Timing[0].StartMS != 1000 || got.Timing[0].EndMS != 4000 {
		t.Fatalf("short-form timestamp = %+v", got.Timing[0])
	}
	if got.Timing[1].StartMS != 10000 {
		t.Fatalf("full timestamp = %+v", got.Timing[1])
	}
	for _, tag := range []string{"<c>", "</c>", "<i>", "</i>", "<00:00:11.000>"} {
		if strings.Contains(got.Text, tag) {
			t.Errorf("inline markup survived into the text: %q contains %s", got.Text, tag)
		}
	}
	if !strings.Contains(got.Text, "Bran rolls a natural twenty") {
		t.Fatalf("tag-stripped text wrong: %q", got.Text)
	}
}

func TestIngestTextAndMarkdownVerbatim(t *testing.T) {
	raw := "line one\r\nline two\r\n"
	for _, format := range []string{FormatText, FormatMD} {
		got, err := Ingest(format, []byte(raw))
		if err != nil {
			t.Fatalf("%s: %v", format, err)
		}
		if got.Text != "line one\nline two\n" {
			t.Fatalf("%s: text = %q; want CRLF normalized, otherwise verbatim", format, got.Text)
		}
		if got.Timing != nil {
			t.Fatalf("%s: plain text grew timing", format)
		}
	}
}

func TestIngestByFilename(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"session.srt", FormatSRT},
		{"session.VTT", FormatVTT},
		{"notes.txt", FormatText},
		{"notes.md", FormatText},
	}
	for _, tc := range cases {
		data := []byte(srtFixture)
		if tc.want == FormatVTT {
			data = []byte(vttFixture)
		}
		got, err := IngestByFilename(tc.name, data)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got.Format != tc.want {
			t.Errorf("%s: format = %s; want %s", tc.name, got.Format, tc.want)
		}
	}

	// Unknown extension falls through to the sniffer.
	got, err := IngestByFilename("session.srt.txt.bak", []byte(srtFixture))
	if err != nil || got.Format != FormatSRT {
		t.Fatalf("sniff: got %s err %v; want srt sniffed from the body", got.Format, err)
	}
}

func TestIngestSniffsAndNormalizes(t *testing.T) {
	// BOM + CRLF SRT, no declared format.
	bommed := "\ufeff" + strings.ReplaceAll(srtFixture, "\n", "\r\n")
	got, err := Ingest(FormatAuto, []byte(bommed))
	if err != nil {
		t.Fatalf("auto: %v", err)
	}
	if got.Format != FormatSRT || len(got.Timing) != 3 {
		t.Fatalf("sniff = %s, %d cues; want srt, 3", got.Format, len(got.Timing))
	}
}

func TestIngestRejectsNonSubtitleBody(t *testing.T) {
	if _, err := Ingest(FormatSRT, []byte("just some prose, no cues here")); err == nil {
		t.Fatal("srt parse of prose should fail loudly")
	}
	if _, err := Ingest("bogus", []byte("x")); err == nil {
		t.Fatal("unknown format should fail")
	}
	// The sniffer never misfiles prose as subtitles: it comes back text.
	got, err := Ingest(FormatAuto, []byte("just some prose, no cues here"))
	if err != nil || got.Format != FormatText {
		t.Fatalf("sniffed prose = %s, %v; want text", got.Format, err)
	}
}

func TestIngestSkipsMalformedCuesNotTheFile(t *testing.T) {
	mixed := "1\n00:00:01,000 --> 00:00:02,000\nGood cue\n\n" +
		"2\nnot a timing line\nBroken block\n\n" +
		"3\n00:00:03,000 --> 00:00:02,000\nBackwards cue\n\n" +
		"4\n00:00:04,000 --> 00:00:05,000\nAnother good cue\n"
	got, err := Ingest(FormatSRT, []byte(mixed))
	if err != nil {
		t.Fatalf("srt: %v", err)
	}
	if len(got.Timing) != 2 {
		t.Fatalf("cues = %d; want the 2 well-formed ones", len(got.Timing))
	}
}
