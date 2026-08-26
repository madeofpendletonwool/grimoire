package canon

// The entailment pass (MAD-312): the deterministic name sweep unit-tested,
// the wire validation tested against fabricated quotes (the span rule,
// pointed at prose), and the store pass end-to-end over a seeded campaign
// with a fake client replaying a fixture response.

import (
	"context"
	"strings"
	"testing"
)

/* ---------- proper-name extraction ---------- */

func TestProperNameRuns(t *testing.T) {
	cases := []struct {
		prose string
		want  []string
	}{
		{"Lord Vane walks into the inn.", []string{"Lord Vane"}},
		{"The Cult of the Root gathers.", []string{"The Cult of the Root"}},
		{"Thalia takes the ledger and reads it.", []string{"Thalia"}},
		{"Vane's sigil marks the door.", []string{"Vane"}},
		{"Bran and Keth argue while Mira watches.", []string{"Bran", "Keth", "Mira"}},
		{"Meanwhile the Ashen Court meets beneath Greyfall.", []string{"the Ashen Court", "Greyfall"}},
		{"He said nothing. The Duke smiled.", []string{"The Duke"}},
		{"no capitals at all here", nil},
	}
	for _, c := range cases {
		got := properNameRuns(c.prose)
		if len(got) != len(c.want) {
			t.Fatalf("properNameRuns(%q) = %v, want %v", c.prose, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("properNameRuns(%q) = %v, want %v", c.prose, got, c.want)
			}
		}
	}
}

/* ---------- the deterministic name sweep ---------- */

func records(texts ...string) []EntailRecord {
	var out []EntailRecord
	for i, txt := range texts {
		out = append(out, EntailRecord{Kind: "fact", ID: string(rune('a' + i)), Text: txt})
	}
	return out
}

func TestCheckUnbackedNames(t *testing.T) {
	recs := records(
		"Duke Aldric Vane died at the falls.",
		"Thalia carries the Silver Key.",
		"The Cult of the Root worships the Verdant God.",
	)

	// Every name appears in the records: clean.
	prose := "Duke Aldric Vane died, and Thalia took the Silver Key from the Cult of the Root."
	if n := countCheck(checkUnbackedNames(prose, recs), CheckUnbackedName); n != 0 {
		t.Fatal("fully backed prose must not flag")
	}

	// A new name the records never mention.
	prose = "Thalia meets the Ashen Court in the hills."
	findings := checkUnbackedNames(prose, recs)
	if n := countCheck(findings, CheckUnbackedName); n != 1 {
		t.Fatalf("unbacked name: got %d findings, want 1 (%v)", n, findings)
	}
	if !strings.Contains(findings[0].Message, "Ashen Court") {
		t.Fatalf("the finding must name the invention: %q", findings[0].Message)
	}

	// Partial overlap reads as the same name: Vane alone is backed by
	// "Duke Aldric Vane".
	prose = "Vane keeps his appointments."
	if n := countCheck(checkUnbackedNames(prose, recs), CheckUnbackedName); n != 0 {
		t.Fatal("partial overlap must not flag")
	}

	// Stoplist-only runs are sentence mechanics, not names.
	prose = "Meanwhile, the Duke's men came twice."
	if n := countCheck(checkUnbackedNames(prose, recs), CheckUnbackedName); n != 0 {
		t.Fatalf("stoplist run flagged: %v", checkUnbackedNames(prose, recs))
	}

	// No records means no basis to judge — the sweep stays silent rather
	// than flagging every noun.
	if n := countCheck(checkUnbackedNames("Thalia meets the Ashen Court.", nil), CheckUnbackedName); n != 0 {
		t.Fatal("no records must mean no findings")
	}
}

/* ---------- wire validation ---------- */

func TestParseEntailVerdicts(t *testing.T) {
	prose := "Duke Aldric Vane died at the falls. The Ashen Court claims the Key."
	recs := "Duke Aldric Vane died at the falls.\nThalia carries the Silver Key.\n"

	// A valid pair: entailed with verbatim support, unsupported with none.
	text := `{"claims":[
		{"claim":"The Duke is dead","quote":"Duke Aldric Vane died at the falls.","verdict":"entailed","support":"Duke Aldric Vane died at the falls.","reason":"stated outright"},
		{"claim":"The Ashen Court claims the key","quote":"The Ashen Court claims the Key.","verdict":"unsupported","reason":"no record mentions the Court"}
	]}`
	claims, problems := parseEntailVerdicts(text, prose, recs)
	if len(problems) != 0 {
		t.Fatalf("valid wire must produce no problems: %v", problems)
	}
	if len(claims) != 2 || claims[0].Verdict != "entailed" || claims[1].Verdict != "unsupported" {
		t.Fatalf("claims: %+v", claims)
	}

	// An entailed verdict whose support quote is not in the records is
	// downgraded to unsupported — cited support that does not exist is the
	// exact failure this pass exists to catch.
	text = `{"claims":[{"claim":"The Duke is dead","quote":"Duke Aldric Vane died at the falls.","verdict":"entailed","support":"words that appear in no record","reason":""}]}`
	claims, problems = parseEntailVerdicts(text, prose, recs)
	if len(claims) != 1 || claims[0].Verdict != "unsupported" {
		t.Fatalf("fabricated support must downgrade: %+v", claims)
	}
	if len(problems) == 0 {
		t.Fatal("the downgrade must be logged")
	}

	// An entailed verdict with no support quote at all: same downgrade.
	text = `{"claims":[{"claim":"The Duke is dead","quote":"Duke Aldric Vane died at the falls.","verdict":"entailed"}]}`
	claims, problems = parseEntailVerdicts(text, prose, recs)
	if len(claims) != 1 || claims[0].Verdict != "unsupported" {
		t.Fatalf("missing support must downgrade: %+v", claims)
	}
	if len(problems) == 0 {
		t.Fatal("the downgrade must be logged")
	}

	// A quote that is not verbatim in the prose is dropped, never surfaced.
	text = `{"claims":[{"claim":"invented","quote":"words not in the prose","verdict":"unsupported"}]}`
	claims, problems = parseEntailVerdicts(text, prose, recs)
	if len(claims) != 0 || len(problems) == 0 {
		t.Fatalf("fabricated quote must drop with a problem: %+v %v", claims, problems)
	}

	// A verdict outside the vocabulary is dropped.
	text = `{"claims":[{"claim":"x","quote":"Duke Aldric Vane died at the falls.","verdict":"maybe"}]}`
	claims, problems = parseEntailVerdicts(text, prose, recs)
	if len(claims) != 0 || len(problems) == 0 {
		t.Fatalf("bogus verdict must drop with a problem: %+v %v", claims, problems)
	}
}

/* ---------- store pass over the seeded campaign ---------- */

func TestEntailOfflineDeterministicOnly(t *testing.T) {
	db, fx, _ := seeded(t)
	s, err := NewOffline(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Auto-selection: prose naming the Duke pulls his live facts in, and an
	// invented name is caught with no model at all.
	prose := "Duke Aldric Vane owns the Eastern Mines, and the Ashen Court gathers beneath it."
	rep, err := s.CheckEntailment(ctx, fx.Campaign.ID, EntailInput{Prose: prose})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Offline {
		t.Fatal("offline store must report Offline=true")
	}
	if n := countCheck(rep.Findings, CheckUnbackedName); n != 1 {
		t.Fatalf("unbacked name: got %d, want 1 (findings %+v)", n, rep.Findings)
	}
	if len(rep.Records) == 0 {
		t.Fatal("auto-selection must pull the Duke's facts as records")
	}
	var sawDukeFact bool
	for _, r := range rep.Records {
		if strings.Contains(r.Text, "Eastern Mines") {
			sawDukeFact = true
		}
	}
	if !sawDukeFact {
		t.Fatalf("records must include the Duke's ownership fact: %+v", rep.Records)
	}
}

func TestEntailWithFakeModel(t *testing.T) {
	db, fx, _ := seeded(t)
	ctx := context.Background()
	store := newStore(t, db, &fakeModel{responses: []string{
		`{"claims":[` +
			`{"claim":"The Duke owns the mines","quote":"The Duke owns the Eastern Mines through a holding charter.","verdict":"entailed","support":"The Duke owns the Eastern Mines through a holding charter.","reason":"stated"},` +
			`{"claim":"The steward met the Court","quote":"the Ashen Court received the Duke's steward","verdict":"unsupported","reason":"no record mentions the Court"}` +
			`]}`,
		`{"claims":[]}`,
	}}, testConfig())

	prose := "The Duke owns the Eastern Mines through a holding charter. Last winter, the Ashen Court received the Duke's steward."
	rep, err := store.CheckEntailment(ctx, fx.Campaign.ID, EntailInput{Prose: prose})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Offline || len(rep.Claims) != 2 {
		t.Fatalf("claims: %+v (problems %v)", rep.Claims, rep.Problems)
	}
	// The unsupported claim becomes a finding; the entailed one does not.
	if n := countCheck(rep.Findings, CheckUnentailedClaim); n != 1 {
		t.Fatalf("unentailed claims: got %d, want 1 (findings %+v)", n, rep.Findings)
	}
	if rep.InputTokens == 0 || rep.CostUSD < 0 {
		t.Fatalf("accounting missing: %+v", rep)
	}

	// Explicit record ids are honored, unknown ones logged.
	rep, err = store.CheckEntailment(ctx, fx.Campaign.ID, EntailInput{
		Prose:   "A fact and a phantom.",
		FactIDs: []string{fx.FactMinesOwned, "no-such-fact"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Records) != 1 || rep.Records[0].ID != fx.FactMinesOwned {
		t.Fatalf("explicit selection: %+v", rep.Records)
	}
	if len(rep.Problems) != 1 {
		t.Fatalf("unknown id must be logged once: %v", rep.Problems)
	}
}
