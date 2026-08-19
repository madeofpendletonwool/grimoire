package deck

import "testing"

func TestCleanCardName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Command Tower [40K]", "Command Tower"},
		{"Sol Ring (LTC) 123", "Sol Ring"},
		{"Sol Ring (LTC) 123 *F*", "Sol Ring"},
		{"Arachnogenesis <C15>", "Arachnogenesis"},
		{"Cultivate (C15) 210 [Ramp]", "Cultivate"},
		{"Farseek {noDeck} {noPrice}", "Farseek"},
		{"Shelob, Dread Weaver [LTC]", "Shelob, Dread Weaver"},
		{"Blex, Vexing Pest // Search for Blex [PRM]", "Blex, Vexing Pest // Search for Blex"},
		{"Lightning Bolt #141", "Lightning Bolt"},
		// Nothing to strip.
		{"Sol Ring", "Sol Ring"},
		{"Kodama's Reach", "Kodama's Reach"},
		// A parenthesised group that is part of the name survives, because it
		// is too long and too spaced to be a set code.
		{"B.F.M. (Big Furry Monster)", "B.F.M. (Big Furry Monster)"},
	}
	for _, c := range cases {
		if got := CleanCardName(c.in); got != c.want {
			t.Errorf("CleanCardName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The bug this whole path exists for: a real Archidekt export of a 100-card
// deck used to resolve as 47 cards, because every name carried its set code.
func TestParseDecklistRealExport(t *testing.T) {
	text := `1 Command Tower [40K]
19 Forest [10E]
1 Golgari Rot Farm [2X2]
10 Swamp [10E]
1 Shelob's Ambush [LTR]

Sideboard
1 Access Tunnel [PW26]
2 Farseek [40K]`

	got := ParseDecklist(text)
	want := []Entry{
		{Name: "Command Tower", Count: 1, Board: "main"},
		{Name: "Forest", Count: 19, Board: "main"},
		{Name: "Golgari Rot Farm", Count: 1, Board: "main"},
		{Name: "Swamp", Count: 10, Board: "main"},
		{Name: "Shelob's Ambush", Count: 1, Board: "main"},
		{Name: "Access Tunnel", Count: 1, Board: "sideboard"},
		{Name: "Farseek", Count: 2, Board: "sideboard"},
	}
	if len(got) != len(want) {
		t.Fatalf("parsed %d entries, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if n := Total(got, "main"); n != 32 {
		t.Errorf("maindeck total = %d, want 32", n)
	}
}

func TestParseDecklistHeaders(t *testing.T) {
	text := `Commander (1)
1 Shelob, Dread Weaver [LTC]

Creatures (2)
1 Nyx Weaver
1 Arasta of the Endless Web

Lands (1)
1 Swarmyard

Sideboard (1)
SB: 1 Bone Splinters`

	got := ParseDecklist(text)
	byBoard := map[string]int{}
	for _, e := range got {
		byBoard[e.Board] += e.Count
	}
	if byBoard["commander"] != 1 || byBoard["main"] != 3 || byBoard["sideboard"] != 1 {
		t.Fatalf("boards = %v, want commander 1 / main 3 / sideboard 1 (%+v)", byBoard, got)
	}
	for _, e := range got {
		if e.Name == "Creatures" || e.Name == "Lands" || e.Name == "Commander" {
			t.Errorf("section header parsed as a card: %+v", e)
		}
	}
}
