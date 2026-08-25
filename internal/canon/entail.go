// The entailment pass (MAD-312): Arda's fact-check stage, ported. Arda
// fact-checks generated prose against the reference records that were
// supposed to support it; this is the same mechanism over AI-generated
// campaign prose, run before the DM sees it.
//
// The rule set, straight from the issue: connective tissue, atmosphere and
// pacing are legitimate; new names, dates, deeds, motives or causal links
// are not. Two layers enforce that:
//
//   - a deterministic name sweep: every proper name in the prose must appear
//     in the records the prose was supposed to rest on. New names are the
//     one class of invention a join can catch with no model at all;
//   - a model pass in the checker stance for everything else — dates, deeds,
//     motives, causal links — where every verdict must quote the prose
//     verbatim and, when it finds a claim entailed, quote the record that
//     entails it. An "entailed" verdict whose support quote is not in the
//     records is downgraded to unsupported: cited support that does not
//     exist is exactly the failure mode this pass exists to catch.
//
// Reports are ephemeral and advisory: the caller gates the prose on them,
// nothing lands in the ledger, nothing is mutated.

package canon

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
)

// Check codes for the entailment pass. Ephemeral: they appear in reports,
// never in the flag ledger.
const (
	CheckUnbackedName    = "unbacked_name"
	CheckUnentailedClaim = "unentailed_claim"
	EntailPromptVersion  = "canon-entail-001"

	// entailRecordCap bounds the records one pass judges against, so a
	// campaign with thousands of facts cannot silently become a giant
	// prompt. Auto-selection prefers facts whose entities the prose names.
	entailRecordCap = 60
)

// EntailInput is one prose document and, optionally, the records it was
// generated from. Empty FactIDs/EventIDs mean auto-select: live facts whose
// subject or object entity is named in the prose, plus events whose summary
// names one.
type EntailInput struct {
	Prose    string
	FactIDs  []string
	EventIDs []string
}

// EntailRecord is one record the prose was judged against, echoed in the
// report so the DM can see what the checker actually read.
type EntailRecord struct {
	Kind string `json:"kind"` // fact | event
	ID   string `json:"id"`
	Text string `json:"text"`
}

// EntailClaim is the model's verdict on one factual claim in the prose.
type EntailClaim struct {
	Claim        string `json:"claim"`
	Quote        string `json:"quote"` // verbatim from the prose
	Verdict      string `json:"verdict"`
	SupportQuote string `json:"support_quote,omitempty"` // verbatim from the records
	Reason       string `json:"reason,omitempty"`
}

// EntailReport is the result of one entailment pass.
type EntailReport struct {
	CampaignID    string             `json:"campaign_id"`
	Offline       bool               `json:"offline"`
	PromptVersion string             `json:"prompt_version"`
	Records       []EntailRecord     `json:"records"`
	Findings      []campaign.Finding `json:"findings"`
	Claims        []EntailClaim      `json:"claims,omitempty"`
	Problems      []string           `json:"problems,omitempty"`
	InputTokens   int                `json:"input_tokens"`
	OutputTokens  int                `json:"output_tokens"`
	CostUSD       float64            `json:"cost_usd"`
}

// CheckEntailment runs the entailment pass over one piece of prose:
// deterministic name sweep always, model pass only when a model client is
// wired. An offline store returns the deterministic report with Offline=true.
func (s *Store) CheckEntailment(ctx context.Context, campaignID string, in EntailInput) (*EntailReport, error) {
	snap, err := LoadSnapshot(ctx, s.db, campaignID)
	if err != nil {
		return nil, err
	}
	records, problems := selectEntailRecords(snap, in)
	rep := &EntailReport{
		CampaignID: campaignID, Offline: true,
		PromptVersion: EntailPromptVersion,
		Records:       records, Problems: problems,
	}
	rep.Findings = append(rep.Findings, checkUnbackedNames(in.Prose, records)...)
	if s.model == nil {
		return rep, nil
	}
	claims, inTok, outTok, cost, modelProblems, err := s.entailModelPass(ctx, in.Prose, records)
	if err != nil {
		return nil, err
	}
	rep.Offline = false
	rep.Claims = claims
	rep.InputTokens, rep.OutputTokens = inTok, outTok
	rep.CostUSD = cost
	rep.Problems = append(rep.Problems, modelProblems...)
	for i, c := range rep.Claims {
		if c.Verdict != "unsupported" {
			continue
		}
		rep.Findings = append(rep.Findings, campaign.Finding{
			Check: CheckUnentailedClaim, Severity: campaign.SeverityReview,
			RecordKind: "prose", RecordID: fmt.Sprintf("claim-%d", i+1),
			Message: fmt.Sprintf("the prose asserts %q, which no provided record entails — new names, dates, deeds, motives or causal links are not the model's to invent (quoted: %q)",
				c.Claim, c.Quote),
		})
	}
	return rep, nil
}

/* ---------- record selection ---------- */

// selectEntailRecords picks the records the prose is judged against.
// Explicit ids are honored as given; auto-selection gathers live facts whose
// subject or object entity (by canonical name or alias) is named in the
// prose, plus events whose summary names one, newest first, capped.
func selectEntailRecords(snap *Snapshot, in EntailInput) ([]EntailRecord, []string) {
	var problems []string
	if len(in.FactIDs) > 0 || len(in.EventIDs) > 0 {
		var out []EntailRecord
		for _, id := range in.FactIDs {
			f, ok := factByID(snap, id)
			if !ok {
				problems = append(problems, fmt.Sprintf("fact %s not found in campaign", id))
				continue
			}
			out = append(out, EntailRecord{Kind: "fact", ID: f.ID, Text: f.Statement})
		}
		for _, id := range in.EventIDs {
			ev, ok := eventByID(snap, id)
			if !ok {
				problems = append(problems, fmt.Sprintf("event %s not found in campaign", id))
				continue
			}
			out = append(out, EntailRecord{Kind: "event", ID: ev.ID, Text: ev.Summary})
		}
		return out, problems
	}
	if strings.TrimSpace(in.Prose) == "" {
		return nil, nil
	}
	// Word-level name matching: the prose saying "the Duke" must pull in
	// "Duke Aldric Vane", which substring matching would miss in both
	// directions. Words of at least three letters, from canonical names
	// and aliases both.
	proseWords := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(in.Prose), func(r rune) bool {
		return r < 'a' || r > 'z'
	}) {
		if len(w) >= 3 {
			proseWords[w] = true
		}
	}
	entityNamed := func(entityID string) bool {
		for _, e := range snap.Entities {
			if e.ID != entityID || e.Status == campaign.StatusDeleted {
				continue
			}
			for _, w := range strings.FieldsFunc(strings.ToLower(e.Name), func(r rune) bool {
				return r < 'a' || r > 'z'
			}) {
				if len(w) >= 3 && proseWords[w] {
					return true
				}
			}
			for _, n := range snap.Names {
				if n.EntityID != entityID {
					continue
				}
				for _, w := range strings.FieldsFunc(strings.ToLower(n.Name), func(r rune) bool {
					return r < 'a' || r > 'z'
				}) {
					if len(w) >= 3 && proseWords[w] {
						return true
					}
				}
			}
		}
		return false
	}
	type dated struct {
		rec  EntailRecord
		when time.Time
	}
	var picked []dated
	for _, f := range snap.Facts {
		if f.SupersededBy != "" || f.Confidence == campaign.ConfidenceProposed {
			continue
		}
		if entityNamed(f.SubjectEntity) || (f.ObjectEntity != "" && entityNamed(f.ObjectEntity)) {
			picked = append(picked, dated{EntailRecord{Kind: "fact", ID: f.ID, Text: f.Statement}, f.CreatedAt})
		}
	}
	for _, ev := range snap.Events {
		if entityNamed(ev.LocationEntity) {
			picked = append(picked, dated{EntailRecord{Kind: "event", ID: ev.ID, Text: ev.Summary}, ev.CreatedAt})
			continue
		}
		for _, p := range ev.Participants {
			if entityNamed(p.EntityID) {
				picked = append(picked, dated{EntailRecord{Kind: "event", ID: ev.ID, Text: ev.Summary}, ev.CreatedAt})
				break
			}
		}
	}
	sort.Slice(picked, func(i, j int) bool { return picked[i].when.After(picked[j].when) })
	var out []EntailRecord
	for _, p := range picked {
		if len(out) >= entailRecordCap {
			problems = append(problems, fmt.Sprintf("record cap %d reached; older records not judged against", entailRecordCap))
			break
		}
		out = append(out, p.rec)
	}
	return out, nil
}

func eventByID(snap *Snapshot, id string) (campaign.Event, bool) {
	for _, e := range snap.Events {
		if e.ID == id {
			return e, true
		}
	}
	return campaign.Event{}, false
}

/* ---------- the deterministic name sweep ---------- */

// nameStoplist holds capitalized words that begin sentences and clauses but
// are never proper names. Deliberately small and dumb: the sweep trades
// recall for never flagging grammar.
var nameStoplist = map[string]bool{
	"i": true, "i'm": true, "i've": true, "i'll": true, "the": true, "a": true,
	"an": true, "and": true, "but": true, "or": true, "nor": true, "so": true,
	"yet": true, "he": true, "she": true, "it": true, "they": true, "we": true,
	"you": true, "his": true, "her": true, "its": true, "their": true,
	"our": true, "my": true, "your": true, "then": true, "when": true,
	"while": true, "with": true, "after": true, "before": true, "as": true,
	"at": true, "in": true, "on": true, "for": true, "to": true, "if": true,
	"no": true, "not": true, "there": true, "here": true, "this": true,
	"that": true, "these": true, "those": true, "now": true, "soon": true,
	"later": true, "meanwhile": true, "suddenly": true, "finally": true,
	"somewhere": true, "beneath": true, "beyond": true, "above": true,
	"inside": true, "outside": true, "chapter": true, "scene": true,
	"session": true, "part": true, "act": true, "dm": true,
}

// nameConnectors join the pieces of multi-word names ("Cult of the Root",
// "Court of the Ashen Vale") and are kept when they sit between capitals.
var nameConnectors = map[string]bool{
	"of": true, "the": true, "in": true, "at": true, "on": true, "a": true,
	"an": true, "de": true, "von": true, "van": true, "la": true, "le": true,
	"du": true, "del": true, "der": true, "den": true, "di": true,
}

// properNameRuns extracts maximal capitalized runs from prose: "Lord Vane",
// "the Ashen Court" style proper names, with connectors swallowed when more
// capitals follow. Possessives ('s) are stripped, and leading sentence
// adverbs ("Meanwhile", "Suddenly") — stoplist words that are not connectors
// — are trimmed so the run starts at the name itself.
func properNameRuns(prose string) []string {
	// Words are runs of letters and apostrophes: spaces and punctuation
	// both separate, and "Vane's" stays one word.
	words := strings.FieldsFunc(prose, func(r rune) bool {
		return !unicode.IsLetter(r) && r != '\''
	})
	trimWord := func(w string) string {
		w = strings.Trim(w, "'")
		if len(w) > 2 && strings.HasSuffix(strings.ToLower(w), "'s") {
			w = w[:len(w)-2]
		}
		return w
	}
	var runs []string
	var cur []string
	flush := func() {
		// trim leading sentence mechanics: stoplist words that are not
		// connectors ("Meanwhile the Duke" -> "the Duke")
		for len(cur) > 0 {
			lower := strings.ToLower(cur[0])
			if nameStoplist[lower] && !nameConnectors[lower] {
				cur = cur[1:]
				continue
			}
			break
		}
		// trim trailing connectors: "Duke of the" -> "Duke"
		for len(cur) > 0 && nameConnectors[strings.ToLower(cur[len(cur)-1])] {
			cur = cur[:len(cur)-1]
		}
		if len(cur) > 0 {
			runs = append(runs, strings.Join(cur, " "))
		}
		cur = nil
	}
	for _, raw := range words {
		w := trimWord(raw)
		if w == "" {
			flush()
			continue
		}
		lower := strings.ToLower(w)
		cap := unicode.IsUpper(firstRune(w))
		switch {
		case cap:
			cur = append(cur, w)
		case len(cur) > 0 && nameConnectors[lower]:
			cur = append(cur, w) // provisional; trimmed on flush
		default:
			flush()
		}
	}
	flush()
	return runs
}

func firstRune(s string) rune {
	for _, r := range s {
		return r
	}
	return 0
}

// checkUnbackedNames is the deterministic half of the entailment rule: a
// proper name in the prose that no record contains (squashed substring) is
// an invention, flagged with no model involved. A run survives if ANY of its
// non-connector words appears in the records — partial overlap reads as the
// same name, not a new one.
func checkUnbackedNames(prose string, records []EntailRecord) []campaign.Finding {
	if strings.TrimSpace(prose) == "" || len(records) == 0 {
		return nil
	}
	var known strings.Builder
	for _, r := range records {
		known.WriteString(r.Text)
		known.WriteString("\n")
	}
	knownSquashed := squashName(known.String())
	var out []campaign.Finding
	seen := map[string]bool{}
	for _, run := range properNameRuns(prose) {
		words := strings.Fields(run)
		meaningful := 0
		backed := 0
		for _, w := range words {
			if nameConnectors[strings.ToLower(w)] {
				continue
			}
			meaningful++
			if strings.Contains(knownSquashed, squashName(w)) {
				backed++
			}
		}
		if meaningful == 0 || backed > 0 {
			continue
		}
		if allStop(words) {
			continue
		}
		if seen[run] {
			continue
		}
		seen[run] = true
		out = append(out, campaign.Finding{
			Check: CheckUnbackedName, Severity: campaign.SeverityReview,
			RecordKind: "prose", RecordID: squashName(run),
			Message: fmt.Sprintf("the prose names %q, which no provided record mentions — a generated name is an invention, not canon", run),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RecordID < out[j].RecordID })
	return out
}

// allStop reports whether every word in the run is a stoplist word — the
// run is sentence mechanics, not a name.
func allStop(words []string) bool {
	for _, w := range words {
		if !nameStoplist[strings.ToLower(w)] && !nameConnectors[strings.ToLower(w)] {
			return false
		}
	}
	return true
}

/* ---------- the model pass ---------- */

// entailModelPass sends the prose and the records to the model in one
// exchange and returns the validated claims. Quote discipline on both sides:
// a claim whose quote is not verbatim in the prose is dropped; an "entailed"
// verdict whose support quote is not verbatim in the records is downgraded
// to unsupported — the cited-support failure the pass exists to catch.
func (s *Store) entailModelPass(ctx context.Context, prose string, records []EntailRecord) ([]EntailClaim, int, int, float64, []string, error) {
	if strings.TrimSpace(prose) == "" || len(records) == 0 {
		return nil, 0, 0, 0, nil, nil
	}
	var lastCall time.Time
	if err := s.waitInterval(ctx, &lastCall); err != nil {
		return nil, 0, 0, 0, nil, err
	}
	var rb strings.Builder
	for _, r := range records {
		fmt.Fprintf(&rb, "- [%s %s] %s\n", r.Kind, r.ID, r.Text)
	}
	completion, err := s.model.Complete(ctx, entailSystemPrompt(), entailUserPrompt(prose, rb.String()))
	if err != nil {
		return nil, 0, 0, 0, nil, fmt.Errorf("entailment model pass: %w", err)
	}
	cost := s.cfg.costUSD(completion.InputTokens, completion.OutputTokens)
	if s.cfg.BudgetUSD > 0 && cost > s.cfg.BudgetUSD {
		return nil, completion.InputTokens, completion.OutputTokens, cost,
			[]string{fmt.Sprintf("entailment pass cost %.4f USD exceeds the run budget %.2f USD; prose unchecked by model", cost, s.cfg.BudgetUSD)},
			nil
	}
	claims, problems := parseEntailVerdicts(completion.Text, prose, rb.String())
	return claims, completion.InputTokens, completion.OutputTokens, cost, problems, nil
}

// parseEntailVerdicts decodes and validates the model's claim list.
func parseEntailVerdicts(text, prose, recordsText string) ([]EntailClaim, []string) {
	var problems []string
	block, err := jsonBlock(text)
	if err != nil {
		return nil, []string{err.Error()}
	}
	var wire struct {
		Claims []struct {
			Claim   string `json:"claim"`
			Quote   string `json:"quote"`
			Verdict string `json:"verdict"`
			Support string `json:"support"`
			Reason  string `json:"reason"`
		} `json:"claims"`
	}
	if err := json.Unmarshal([]byte(block), &wire); err != nil {
		return nil, []string{fmt.Sprintf("invalid JSON: %v", err)}
	}
	var out []EntailClaim
	for i, c := range wire.Claims {
		if strings.TrimSpace(c.Claim) == "" || strings.TrimSpace(c.Quote) == "" {
			problems = append(problems, fmt.Sprintf("claims[%d]: missing claim or quote", i))
			continue
		}
		if !strings.Contains(prose, c.Quote) {
			problems = append(problems, fmt.Sprintf("claims[%d]: quote not verbatim in the prose", i))
			continue
		}
		verdict := strings.ToLower(strings.TrimSpace(c.Verdict))
		if verdict != "entailed" && verdict != "unsupported" {
			problems = append(problems, fmt.Sprintf("claims[%d]: verdict %q outside the vocabulary", i, c.Verdict))
			continue
		}
		claim := EntailClaim{
			Claim: c.Claim, Quote: c.Quote, Verdict: verdict,
			SupportQuote: strings.TrimSpace(c.Support), Reason: c.Reason,
		}
		if verdict == "entailed" {
			if claim.SupportQuote == "" {
				claim.Verdict = "unsupported"
				claim.Reason = strings.TrimSpace("cited no support; downgraded — " + claim.Reason)
				problems = append(problems, fmt.Sprintf("claims[%d]: entailed verdict with no support quote; downgraded", i))
			} else if !strings.Contains(recordsText, claim.SupportQuote) {
				claim.Verdict = "unsupported"
				claim.Reason = strings.TrimSpace("cited support not found in the records; downgraded — " + claim.Reason)
				problems = append(problems, fmt.Sprintf("claims[%d]: support quote not verbatim in the records; downgraded", i))
			}
		}
		out = append(out, claim)
	}
	return out, problems
}

// entailSystemPrompt is the standing instruction: the Arda checker stance,
// pointed at campaign prose. Every factual statement must be entailed by the
// records; connective tissue is legitimate; invention is not.
func entailSystemPrompt() string {
	return `You are the fact-checker for a D&D campaign. You are given PROSE (draft campaign material, possibly AI-generated) and RECORDS (the campaign facts and events the prose was supposed to rest on). Your job: determine which factual statements in the prose are ENTAILED by the records.

THE RULE: connective tissue, atmosphere and pacing are legitimate — the prose may dramatize, summarize and connect what the records establish. But NEW names, dates, deeds, motives or causal links are NOT. A statement is entailed only when the records state it or directly imply it.

VERDICTS:
- "entailed": the records state or directly imply the claim. You MUST quote the supporting record words verbatim in "support".
- "unsupported": the records do not establish the claim — an invented name, date, deed, motive or causal link. "support" stays empty.

PRIME DIRECTIVE — QUOTE OR STAY SILENT. Every claim must carry a "quote" copied VERBATIM from the prose. An "entailed" verdict must additionally carry a "support" quote copied VERBATIM from the records. Do not use your knowledge of D&D lore or tabletop tropes to rescue a claim the records do not support; outside knowledge may deepen suspicion, never confirmation.

OUTPUT FORMAT:
Return ONE JSON object and nothing else — no prose, no markdown fences:
{"claims": [{"claim": "the factual assertion in your own words", "quote": "verbatim words from the prose", "verdict": "entailed|unsupported", "support": "verbatim words from the records, or empty", "reason": "one sentence"}]}
Check every factual statement you can find. Return only the JSON object.`
}

// entailUserPrompt renders the entailment task: prose and records, clearly
// delimited so the quote discipline is checkable.
func entailUserPrompt(prose, recordsText string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "TASK: Check the PROSE below against the RECORDS. Every factual statement must be entailed by the records.\n\n")
	fmt.Fprintf(&b, "PROSE (quote claims verbatim from this text):\n%s\n\n", prose)
	fmt.Fprintf(&b, "RECORDS (quote support verbatim from this text):\n%s\n\n", recordsText)
	b.WriteString("Return the claim list following every rule in the system message. Return only the JSON object.")
	return b.String()
}
