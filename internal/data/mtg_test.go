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

func TestParseMTG_ReaderNodes(t *testing.T) {
	ds, err := ParseMTG(strings.NewReader(mtgSample))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	nodes := ds.Reader
	if len(nodes) == 0 {
		t.Fatal("no reader nodes parsed")
	}

	byNumber := map[string]ReaderNode{}
	for _, n := range nodes {
		if n.Guide != "rules" || n.GuideKind != "rules" {
			t.Errorf("node %s guide = %s/%s", n.Number, n.Guide, n.GuideKind)
		}
		byNumber[n.Number] = n
	}

	// Chapter 1 carries its title and its sections hang beneath it.
	ch1, ok := byNumber["1"]
	if !ok || ch1.Title != "Game Concepts" || ch1.Level != 1 {
		t.Errorf("chapter 1 = %+v", ch1)
	}
	if byNumber["101"].Level != 2 || byNumber["101"].Title != "The Magic Golden Rules" {
		t.Errorf("section 101 = %+v", byNumber["101"])
	}
	if byNumber["102"].Level != 2 || byNumber["102"].Title != "Players" {
		t.Errorf("section 102 = %+v", byNumber["102"])
	}

	// A section's reading body carries its rules with numbers, in order.
	body := byNumber["101"].Body
	if !strings.Contains(body, "101.1. Whenever a card's text directly contradicts") {
		t.Errorf("101 body missing rule text: %q", body)
	}
	if !strings.Contains(body, "101.2. When a rule or effect allows") {
		t.Errorf("101 body missing second rule: %q", body)
	}

	// The glossary is a chapter of term entries, not one giant blob.
	gloss := byNumber["glossary"]
	if !ok || gloss.Level != 1 {
		t.Errorf("glossary chapter = %+v", gloss)
	}
	var vigilance string
	for _, n := range nodes {
		if n.Title == "Vigilance" && n.Guide == "rules" {
			vigilance = n.Body
		}
	}
	if !strings.Contains(vigilance, "attack without tapping") {
		t.Errorf("glossary term Vigilance missing: %q", vigilance)
	}

	// Positions are strictly increasing in book order.
	for i := 1; i < len(nodes); i++ {
		if nodes[i].Position <= nodes[i-1].Position {
			t.Fatalf("positions not increasing at %d", i)
		}
	}
}
