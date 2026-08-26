package story

import "testing"

// Shape tests: the legal structures are three, four and five acts; anything
// else has no named structure, and the catalogue says what each act is for.

func TestShapeLegalCounts(t *testing.T) {
	for _, n := range []int{3, 4, 5} {
		sh, ok := Shape(n)
		if !ok {
			t.Fatalf("Shape(%d): a legal count must resolve", n)
		}
		if len(sh.Acts) != n {
			t.Errorf("Shape(%d) returned %d acts", n, len(sh.Acts))
		}
		for _, r := range sh.Acts {
			if r.Key == "" || r.Label == "" || r.Purpose == "" {
				t.Errorf("Shape(%d): act role %+v is missing key, label or purpose", n, r)
			}
		}
	}
}

func TestShapeRejectsEverythingElse(t *testing.T) {
	for _, n := range []int{0, 1, 2, 6, 7, 12} {
		if _, ok := Shape(n); ok {
			t.Errorf("Shape(%d): %d acts is not a named structure; the planner must say so, not invent one", n, n)
		}
	}
}

func TestFiveActHasAMidTurn(t *testing.T) {
	sh, ok := Shape(5)
	if !ok {
		t.Fatal("Shape(5) must resolve")
	}
	found := false
	for _, r := range sh.Acts {
		if r.Key == "mid_turn" {
			found = true
		}
	}
	if !found {
		t.Errorf("the five-act structure must carry the mid turn, got %+v", sh.Acts)
	}
}

func TestShapesCatalogue(t *testing.T) {
	all := Shapes()
	if len(all) != 3 {
		t.Errorf("Shapes() lists %d structures, want the three (three-, four-, five-act)", len(all))
	}
	// The returned catalogue is a copy: a caller mutating it must not
	// corrupt the package's shapes.
	all[0].Acts[0].Purpose = "corrupted"
	if _, ok := Shape(3); !ok {
		t.Fatal("Shape(3) must still resolve")
	}
	fresh, _ := Shape(3)
	if fresh.Acts[0].Purpose == "corrupted" {
		t.Error("Shapes() handed out the package's own structures")
	}
}
