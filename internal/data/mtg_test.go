package data

import (
	"strings"
	"testing"
)

const mtgSample = `Magic: The Gathering Comprehensive Rules

These rules are effective as of August 7, 2026.

Introduction

Contents

100. General
101. The Magic Golden Rules

Glossary

Credits

1. Game Concepts

100. General

101. The Magic Golden Rules

101.1. Whenever a card's text directly contradicts these rules, the card wins.

101.2. When a rule or effect allows or directs something to happen, and another effect states that it can't happen, the "can't" effect wins.

102. Players

102.1. A player is one of the people in the game.

102.2. Some effects refer to the "active player."

Glossary

Active Player
See rule 102.2.

Vigilance
A keyword ability that lets a creature attack without tapping. See rule 702.20.

Ward
A triggered ability that can counter spells or abilities that target the permanent with ward. See rule 702.21.

Credits

Magic: The Gathering Original Game Design: Richard Garfield
`

func TestParseMTG(t *testing.T) {
	ds, err := ParseMTG(strings.NewReader(mtgSample))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if got := ds.Meta[CorpusMTG].Version; got != "August 7, 2026" {
		t.Errorf("version = %q, want August 7, 2026", got)
	}

	wantRules := map[string]string{
		"101.1": "Whenever a card's text directly contradicts these rules, the card wins.",
		"102.2": "Some effects refer to the \"active player.\"",
	}
	got := map[string]string{}
	for _, r := range ds.Records {
		if r.Corpus == CorpusMTG && r.Number != "" {
			got[r.Number] = r.Body
		}
	}
	for num, body := range wantRules {
		if got[num] != body {
			t.Errorf("rule %s body = %q, want %q", num, got[num], body)
		}
	}

	// section titles attach to rules
	for _, r := range ds.Records {
		if r.Number == "101.1" && r.Title != "The Magic Golden Rules" {
			t.Errorf("rule 101.1 title = %q, want The Magic Golden Rules", r.Title)
		}
	}

	// glossary terms become records with no number
	var vigilance, ward, active *Record
	for i := range ds.Records {
		r := ds.Records[i]
		switch r.Title {
		case "Vigilance":
			vigilance = &ds.Records[i]
		case "Ward":
			ward = &ds.Records[i]
		case "Active Player":
			active = &ds.Records[i]
		}
	}
	if vigilance == nil || !strings.Contains(vigilance.Body, "attack without tapping") {
		t.Errorf("glossary Vigilance missing or wrong: %+v", vigilance)
	}
	if ward == nil || !strings.Contains(ward.Body, "triggered ability") {
		t.Errorf("glossary Ward missing or wrong: %+v", ward)
	}
	if active == nil || !strings.Contains(active.Body, "102.2") {
		t.Errorf("glossary Active Player missing or wrong: %+v", active)
	}
}

func TestParseMTG_TopLevelRuleHasTrailingPeriod(t *testing.T) {
	// Regression: top-level rules like "205.1." carry a trailing period before the text.
	// Structure mirrors the real file: rules, then Glossary, then Credits.
	in := strings.Join([]string{
		"100. General",
		"",
		"100.1. The Magic rules apply to any Magic game.",
		"",
		"Glossary",
		"",
		"Term",
		"A definition here.",
		"",
		"Credits",
		"",
		"Design: someone",
		"",
	}, "\n")
	ds, err := ParseMTG(strings.NewReader(in))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var found string
	for _, r := range ds.Records {
		if r.Number == "100.1" {
			found = r.Body
		}
	}
	if found == "" {
		t.Fatal("rule 100.1 not parsed")
	}
	if !strings.HasPrefix(found, "The Magic rules apply") {
		t.Errorf("rule 100.1 body = %q", found)
	}
}
