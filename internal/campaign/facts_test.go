package campaign

import (
	"context"
	"errors"
	"testing"
)

// factCtx is the common setup for fact tests: a campaign, a duke and a mine.
func factCtx(t *testing.T) (*Store, context.Context, *Campaign, string, string) {
	t.Helper()
	s := newStore(t)
	ctx := context.Background()
	c := seedCampaign(t, s)
	duke, err := s.CreateEntity(ctx, c.ID, KindNPC, "The Duke", "", nil)
	if err != nil {
		t.Fatalf("create duke: %v", err)
	}
	mines, err := s.CreateEntity(ctx, c.ID, KindLocation, "The Mines", "", nil)
	if err != nil {
		t.Fatalf("create mines: %v", err)
	}
	return s, ctx, c, duke.ID, mines.ID
}

func TestCreateFactShapeTable(t *testing.T) {
	s, ctx, c, duke, mines := factCtx(t)

	cases := []struct {
		name          string
		objectEntity  string
		objectLiteral string
		provenance    []ProvenanceInput
		wantErr       error
	}{
		{
			name:         "entity object",
			objectEntity: mines,
			provenance:   []ProvenanceInput{{Method: MethodDMAuthored}},
		},
		{
			name:          "literal object",
			objectLiteral: "in the winter",
			provenance:    []ProvenanceInput{{Method: MethodDMAuthored}},
		},
		{
			name:         "both objects",
			objectEntity: mines, objectLiteral: "twice",
			provenance: []ProvenanceInput{{Method: MethodDMAuthored}},
			wantErr:    ErrInvalid,
		},
		{
			name:       "no object",
			provenance: []ProvenanceInput{{Method: MethodDMAuthored}},
			wantErr:    ErrInvalid,
		},
		{
			name:          "no provenance",
			objectLiteral: "somewhere",
			wantErr:       ErrInvalid,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.CreateFact(ctx, c.ID, duke, "visited", tc.objectEntity, tc.objectLiteral,
				"The Duke visited.", ConfidenceCanon, VisibilityPublic, "keeper", tc.provenance)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("want %v, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestCreateFactSpanRuleTable(t *testing.T) {
	s, ctx, c, duke, _ := factCtx(t)

	cases := []struct {
		name       string
		method     string
		confidence string
		prov       ProvenanceInput
		wantErr    bool
	}{
		{
			name:       "dm-authored needs no span",
			method:     MethodDMAuthored,
			confidence: ConfidenceCanon,
			prov:       ProvenanceInput{Method: MethodDMAuthored},
		},
		{
			name:       "extracted with full span",
			method:     MethodExtracted,
			confidence: ConfidenceProposed,
			prov:       ProvenanceInput{Method: MethodExtracted, SessionID: "s1", SourceID: "src1", SpanStart: 12, SpanEnd: 44, Quote: "he said"},
		},
		{
			name:       "extracted without session",
			method:     MethodExtracted,
			confidence: ConfidenceProposed,
			prov:       ProvenanceInput{Method: MethodExtracted, SourceID: "src1", SpanStart: 12, SpanEnd: 44, Quote: "he said"},
			wantErr:    true,
		},
		{
			name:       "extracted without quote",
			method:     MethodExtracted,
			confidence: ConfidenceProposed,
			prov:       ProvenanceInput{Method: MethodExtracted, SessionID: "s1", SourceID: "src1", SpanStart: 12, SpanEnd: 44},
			wantErr:    true,
		},
		{
			name:       "extracted with inverted span",
			method:     MethodExtracted,
			confidence: ConfidenceProposed,
			prov:       ProvenanceInput{Method: MethodExtracted, SessionID: "s1", SourceID: "src1", SpanStart: 44, SpanEnd: 12, Quote: "he said"},
			wantErr:    true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.CreateFact(ctx, c.ID, duke, "visited", "", "somewhere",
				"A statement.", tc.confidence, VisibilityPublic, "keeper", []ProvenanceInput{tc.prov})
			if tc.wantErr && !errors.Is(err, ErrInvalid) {
				t.Fatalf("want ErrInvalid, got %v", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestCreateFactConfidenceByMethodTable(t *testing.T) {
	s, ctx, c, duke, _ := factCtx(t)

	cases := []struct {
		name       string
		method     string
		confidence string
		wantErr    error
	}{
		// Machine methods may only stage proposals (ADR 3).
		{"extracted stages", MethodExtracted, ConfidenceProposed, nil},
		{"extracted cannot canon", MethodExtracted, ConfidenceCanon, ErrInvalid},
		{"ai-proposed stages", MethodAIProposed, ConfidenceProposed, nil},
		{"ai-proposed cannot derived", MethodAIProposed, ConfidenceDerived, ErrInvalid},
		// Human methods assert, they do not stage.
		{"dm writes canon", MethodDMAuthored, ConfidenceCanon, nil},
		{"dm writes derived", MethodDMAuthored, ConfidenceDerived, nil},
		{"dm cannot stage", MethodDMAuthored, ConfidenceProposed, ErrInvalid},
		{"imported canon", MethodImported, ConfidenceCanon, nil},
		{"bad confidence", MethodDMAuthored, "apocryphal", ErrInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A full span quadruple is harmless for human methods and
			// mandatory for extracted ones.
			prov := ProvenanceInput{Method: tc.method, SessionID: "s1", SourceID: "src1",
				SpanStart: 0, SpanEnd: 10, Quote: "q"}
			_, err := s.CreateFact(ctx, c.ID, duke, "visited", "", "somewhere",
				"A statement.", tc.confidence, VisibilityPublic, "keeper", []ProvenanceInput{prov})
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("want %v, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestFactProvenanceRoundTrip(t *testing.T) {
	s, ctx, c, duke, _ := factCtx(t)

	f, err := s.CreateFact(ctx, c.ID, duke, "owns", "", "a ledger",
		"The Duke keeps a ledger.", ConfidenceCanon, VisibilitySecret, "keeper",
		[]ProvenanceInput{
			{Method: MethodDMAuthored, SessionID: "s1"},
			{Method: MethodImported, Quote: "imported from the old wiki"},
		})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	prov, err := s.FactProvenance(ctx, ScopeDM, c.ID, f.ID)
	if err != nil || len(prov) != 2 {
		t.Fatalf("want two provenance rows: %v %v", prov, err)
	}
	if prov[0].Method != MethodDMAuthored || prov[0].SessionID != "s1" {
		t.Fatalf("first provenance mismatch: %+v", prov[0])
	}

	// Visibility and confidence survive the round trip.
	got, err := s.GetFact(ctx, ScopeDM, c.ID, f.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Visibility != VisibilitySecret || got.Confidence != ConfidenceCanon {
		t.Fatalf("round-trip mismatch: %+v", got)
	}

	// A foreign fact reads as missing.
	if _, err := s.GetFact(ctx, ScopeDM, "other-campaign", f.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-campaign fact read must be ErrNotFound, got %v", err)
	}
}

func TestSupersedeFactRetconsInPlace(t *testing.T) {
	s, ctx, c, duke, _ := factCtx(t)

	old, err := s.CreateFact(ctx, c.ID, duke, "resides", "", "the keep",
		"The Duke resides in the keep.", ConfidenceCanon, VisibilityPublic, "keeper",
		[]ProvenanceInput{{Method: MethodDMAuthored}})
	if err != nil {
		t.Fatalf("create old: %v", err)
	}
	replacement, err := s.CreateFact(ctx, c.ID, duke, "resides", "", "the mines",
		"The Duke resides in the mines.", ConfidenceCanon, VisibilityPublic, "keeper",
		[]ProvenanceInput{{Method: MethodDMAuthored}})
	if err != nil {
		t.Fatalf("create replacement: %v", err)
	}
	if err := s.SupersedeFact(ctx, c.ID, old.ID, replacement.ID); err != nil {
		t.Fatalf("supersede: %v", err)
	}
	got, _ := s.GetFact(ctx, ScopeDM, c.ID, old.ID)
	if got.Confidence != ConfidenceRetconned || got.SupersededBy != replacement.ID {
		t.Fatalf("superseded fact must stay, retconned and pointing forward: %+v", got)
	}
	// Superseding twice reads as not found: the fact already moved on.
	if err := s.SupersedeFact(ctx, c.ID, old.ID, replacement.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("re-supersede must be ErrNotFound, got %v", err)
	}

	// NotSuperseded filters retconned history out of listings.
	live, err := s.ListFacts(ctx, ScopeDM, c.ID, FactFilter{NotSuperseded: true})
	if err != nil || len(live) != 1 || live[0].ID != replacement.ID {
		t.Fatalf("live facts should exclude retconned: %v %v", live, err)
	}
}

func TestRegisterContradictionDowngradesMonotonically(t *testing.T) {
	s, ctx, c, duke, _ := factCtx(t)

	mk := func(literal, conf string) *Fact {
		f, err := s.CreateFact(ctx, c.ID, duke, "visited", "", literal,
			"The Duke visited "+literal+".", conf, VisibilityPublic, "keeper",
			[]ProvenanceInput{{Method: MethodDMAuthored}})
		if err != nil {
			t.Fatalf("create fact: %v", err)
		}
		return f
	}

	cases := []struct {
		name       string
		confidence string
		wantAfter  string
	}{
		{"canon downgrades to contested", ConfidenceCanon, ConfidenceContested},
		{"derived downgrades to contested", ConfidenceDerived, ConfidenceContested},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := mk("the mines", ConfidenceCanon)
			b := mk("nowhere", tc.confidence)
			if _, err := s.RegisterContradiction(ctx, c.ID, duke, "visited",
				[]FactVersionSide{{FactID: a.ID, Label: "ledger"}, {FactID: b.ID, Label: "testimony"}}, ""); err != nil {
				t.Fatalf("register: %v", err)
			}
			got, _ := s.GetFact(ctx, ScopeDM, c.ID, b.ID)
			if got.Confidence != tc.wantAfter {
				t.Fatalf("want %s, got %s", tc.wantAfter, got.Confidence)
			}
		})
	}

	// A proposed fact in conflict stays proposed: it is already below
	// contested and invisible to every retrieval path.
	a := mk("the keep", ConfidenceCanon)
	proposed, err := s.CreateFact(ctx, c.ID, duke, "visited", "", "the coast",
		"The Duke visited the coast.", ConfidenceProposed, VisibilityPublic, "keeper",
		[]ProvenanceInput{{Method: MethodAIProposed}})
	if err != nil {
		t.Fatalf("create proposed: %v", err)
	}
	if _, err := s.RegisterContradiction(ctx, c.ID, duke, "visited",
		[]FactVersionSide{{FactID: a.ID, Label: "ledger"}, {FactID: proposed.ID, Label: "the model"}}, ""); err != nil {
		t.Fatalf("register with proposed side: %v", err)
	}
	got, _ := s.GetFact(ctx, ScopeDM, c.ID, proposed.ID)
	if got.Confidence != ConfidenceProposed {
		t.Fatalf("proposed must not be touched: %s", got.Confidence)
	}
}

func TestRegisterContradictionRules(t *testing.T) {
	s, ctx, c, duke, _ := factCtx(t)

	mk := func(literal string) *Fact {
		f, err := s.CreateFact(ctx, c.ID, duke, "visited", "", literal,
			"The Duke visited "+literal+".", ConfidenceCanon, VisibilityPublic, "keeper",
			[]ProvenanceInput{{Method: MethodDMAuthored}})
		if err != nil {
			t.Fatalf("create fact: %v", err)
		}
		return f
	}
	a, b := mk("the mines"), mk("nowhere")

	// Needs two sides.
	if _, err := s.RegisterContradiction(ctx, c.ID, duke, "visited",
		[]FactVersionSide{{FactID: a.ID, Label: "ledger"}}, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("one side must be ErrInvalid, got %v", err)
	}
	// Labels are required.
	if _, err := s.RegisterContradiction(ctx, c.ID, duke, "visited",
		[]FactVersionSide{{FactID: a.ID}, {FactID: b.ID, Label: "testimony"}}, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unlabeled side must be ErrInvalid, got %v", err)
	}

	con, err := s.RegisterContradiction(ctx, c.ID, duke, "visited",
		[]FactVersionSide{{FactID: a.ID, Label: "ledger"}, {FactID: b.ID, Label: "testimony"}}, "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if con.Status != ContradictionOpen {
		t.Fatalf("new contradiction is open: %+v", con)
	}
	// The same pair cannot be registered twice while open.
	if _, err := s.RegisterContradiction(ctx, c.ID, duke, "visited",
		[]FactVersionSide{{FactID: a.ID, Label: "ledger"}, {FactID: b.ID, Label: "testimony"}}, ""); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("double register must be ErrAlreadyExists, got %v", err)
	}

	versions, err := s.FactVersions(ctx, ScopeDM, c.ID, con.ID)
	if err != nil || len(versions) != 2 {
		t.Fatalf("want two versions: %v %v", versions, err)
	}
	if versions[0].FactID != a.ID || versions[0].Label != "ledger" {
		t.Fatalf("version mismatch: %+v", versions[0])
	}

	// The predicate must actually match the facts.
	other := mk("the coast")
	if _, err := s.RegisterContradiction(ctx, c.ID, duke, "fled",
		[]FactVersionSide{{FactID: a.ID, Label: "x"}, {FactID: other.ID, Label: "y"}}, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("mismatched predicate must be ErrInvalid, got %v", err)
	}

	// Resolving closes the register entry; the facts stay contested.
	if err := s.ResolveContradiction(ctx, c.ID, con.ID, "the ledger wins"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	cons, err := s.Contradictions(ctx, ScopeDM, c.ID)
	if err != nil || len(cons) != 1 || cons[0].Status != ContradictionResolvedByReview {
		t.Fatalf("resolved register: %v %v", cons, err)
	}
	got, _ := s.GetFact(ctx, ScopeDM, c.ID, a.ID)
	if got.Confidence != ConfidenceContested {
		t.Fatalf("resolving the register does not pick a fact winner: %s", got.Confidence)
	}
	// Once resolved, a fresh register entry over the same predicate is a new
	// contradiction, not a duplicate.
	if _, err := s.RegisterContradiction(ctx, c.ID, duke, "visited",
		[]FactVersionSide{{FactID: a.ID, Label: "ledger"}, {FactID: other.ID, Label: "coast rumor"}}, ""); err != nil {
		t.Fatalf("register after resolve: %v", err)
	}
}
