package campaign

import "testing"

func TestParseScope(t *testing.T) {
	for _, ok := range []string{"dm", "party", "character:" + "e1", "npc:" + "e2"} {
		s, err := ParseScope(ok)
		if err != nil {
			t.Fatalf("ParseScope(%q): %v", ok, err)
		}
		if s.String() != ok {
			t.Fatalf("round trip: %q -> %q", ok, s.String())
		}
	}
	for _, bad := range []string{"", "player", "dm:x", "party:x", "character:", "npc:", "character:party", " CHARACTER:e1"} {
		if _, err := ParseScope(bad); err == nil {
			t.Fatalf("ParseScope(%q) should fail", bad)
		}
	}

	dm, _ := ParseScope("dm")
	if !dm.IsDM() || dm.Knower() != "" || dm.EntityID() != "" {
		t.Fatalf("dm scope: %+v", dm)
	}
	party, _ := ParseScope("party")
	if party.IsDM() || party.Knower() != PartyKnower {
		t.Fatalf("party scope: %+v", party)
	}
	ch, _ := ParseScope("character:e9")
	if ch.Kind() != ScopeKindCharacter || ch.EntityID() != "e9" || ch.Knower() != "e9" {
		t.Fatalf("character scope: %+v", ch)
	}
	npc, _ := ParseScope("npc:e7")
	if npc.Kind() != ScopeKindNPC || npc.Knower() != "e7" {
		t.Fatalf("npc scope: %+v", npc)
	}
	// The scopes are values; the DM one is the zero-ish singleton.
	if (ScopeDM == Scope{}) {
		t.Fatal("ScopeDM should not be the zero Scope")
	}
	if err := ScopeDM.requireDM(); err != nil {
		t.Fatalf("dm requireDM: %v", err)
	}
	if err := ScopeParty.requireDM(); err == nil {
		t.Fatal("party requireDM must fail")
	}
}
