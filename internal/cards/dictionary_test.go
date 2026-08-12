package cards

import (
	"reflect"
	"testing"
)

func TestDictionary_Mentions(t *testing.T) {
	d := NewDictionary([]string{
		"Lightning Bolt",
		"Prizefight",
		"Giant Growth",
		"Kokusho, the Evening Star",
	})

	cases := []struct {
		name string
		text string
		want []string
	}{
		// The MAD-113 case: lowercase, unquoted, wrong spacing.
		{"lowercase wrong spacing", "a fog effect would not stop prize fight right?", []string{"Prizefight"}},
		{"spacing variant two words", "does lightning bolt stack", []string{"Lightning Bolt"}},
		{"multi-word lowercase", "cast giant growth", []string{"Giant Growth"}},
		{"punctuation in canonical name", "sacrifice kokusho the evening star", []string{"Kokusho, the Evening Star"}},
		{"two mentions in order", "giant growth then prizefight", []string{"Giant Growth", "Prizefight"}},
		{"no card present", "how does priority work in combat", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := d.Mentions(c.text)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("Mentions(%q) = %v, want %v", c.text, got, c.want)
			}
		})
	}
}

// Names that normalize to fewer than three characters are noise and are
// dropped at index time.
func TestDictionary_DropsShortNames(t *testing.T) {
	d := NewDictionary([]string{"Bo", "A", "Sun", "Giant Growth"})
	if d.Size() != 2 {
		t.Errorf("Size = %d, want 2 (Sun, Giant Growth)", d.Size())
	}
	if got := d.Mentions("the sun is bright"); !reflect.DeepEqual(got, []string{"Sun"}) {
		t.Errorf("got %v, want [Sun]", got)
	}
}

func TestDictionary_NilSafe(t *testing.T) {
	var d *Dictionary
	if got := d.Mentions("anything"); got != nil {
		t.Errorf("nil Mentions = %v, want nil", got)
	}
	if d.Size() != 0 {
		t.Errorf("nil Size = %d, want 0", d.Size())
	}
}

func TestExtractCandidatesWithDict(t *testing.T) {
	d := NewDictionary([]string{"Prizefight", "Lightning Bolt"})

	// The dictionary catches the lowercase mention the heuristics miss and
	// returns the canonical spelling, which Resolve then looks up on Scryfall.
	got := ExtractCandidatesWithDict("what does prize fight do", d)
	if !containsStr(got, "Prizefight") {
		t.Errorf("got %v, want it to include Prizefight", got)
	}

	// A nil dictionary degrades to exactly the heuristic extractor, so callers
	// without a dictionary are unaffected.
	want := ExtractCandidates(`what does "Lightning Bolt" do`)
	gotNil := ExtractCandidatesWithDict(`what does "Lightning Bolt" do`, nil)
	if !reflect.DeepEqual(gotNil, want) {
		t.Errorf("nil dict: got %v, want %v", gotNil, want)
	}

	// A dictionary hit and a heuristic hit for the same name are not doubled.
	merged := ExtractCandidatesWithDict("Lightning Bolt", d)
	count := 0
	for _, c := range merged {
		if c == "Lightning Bolt" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("Lightning Bolt appeared %d times in %v, want exactly 1", count, merged)
	}
}

func containsStr(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
