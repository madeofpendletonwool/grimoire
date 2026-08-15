package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/data"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/study"
)

// mtgKeywordChapter is the Comprehensive Rules chapter that lists every keyword
// ability (Deathtouch in 702.2, Ward in 702.21, ...). One concept per section
// root makes a natural flashcard deck: the keyword name is the prompt, the
// section's sub-rules are the answer.
const mtgKeywordChapter = "702"

// deckGenerator adapts the index store to the study.Generator interface. It
// owns the per-corpus, per-topic translation of "rules text" into "flashcard
// content", so the study package never has to reach into the FTS5 layer.
type deckGenerator struct {
	store *index.Store
}

// Concepts builds the deck for a corpus + topic. An unknown corpus/topic yields
// no concepts, which the study store surfaces as ErrDeckEmpty — the UI shows
// "nothing to study" rather than a 500.
func (g deckGenerator) Concepts(ctx context.Context, corpus, topic string) ([]study.Concept, error) {
	topic = study.TopicFor(corpus, topic)
	switch {
	case strings.EqualFold(corpus, string(data.CorpusMTG)) && topic == study.TopicKeywordAbilities:
		return g.mtgKeywords(ctx)
	case strings.EqualFold(corpus, string(data.CorpusDND)) && topic == study.TopicConditions:
		return g.dndConditions(ctx)
	case strings.EqualFold(corpus, string(data.CorpusDND)) && topic == study.TopicSpells:
		return g.dndSpells(ctx)
	}
	return nil, nil
}

// mtgKeywords turns chapter 702 into one concept per keyword ability. The parent
// rule (e.g. 702.2, whose body is just "Deathtouch") names the keyword; its
// lettered sub-rules are the reminder text the reader is asked to recall.
func (g deckGenerator) mtgKeywords(ctx context.Context) ([]study.Concept, error) {
	rules, err := g.store.Chapter(ctx, data.CorpusMTG, mtgKeywordChapter)
	if err != nil {
		return nil, fmt.Errorf("load keyword chapter: %w", err)
	}
	groups := map[string][]index.Result{}
	var order []string
	for _, r := range rules {
		root := sectionRootOf(r.Number)
		if root == "" {
			continue
		}
		if _, ok := groups[root]; !ok {
			order = append(order, root)
		}
		groups[root] = append(groups[root], r)
	}
	sort.SliceStable(order, func(i, j int) bool { return lessRuleNum(order[i], order[j]) })

	out := make([]study.Concept, 0, len(order))
	for _, root := range order {
		group := groups[root]
		parent, children := splitKeyword(group, root)
		if parent == nil {
			continue // a section with no anchor rule is not a usable card
		}
		name := keywordName(parent, children)
		out = append(out, study.Concept{
			Key:    parent.Number,
			Corpus: string(data.CorpusMTG),
			Topic:  study.TopicKeywordAbilities,
			Number: parent.Number,
			Title:  name,
			Front:  fmt.Sprintf("%s — keyword ability (Rule %s). Recall how it works.", name, parent.Number),
			Back:   keywordBack(parent, children),
			Source: parent.Source,
		})
	}
	return out, nil
}

// splitKeyword separates a section's parent rule from its lettered sub-rules.
// The parent is the rule whose number carries no trailing letter (702.2 vs
// 702.2a); if no parent exists the first child stands in so the section is
// still deck-able.
func splitKeyword(group []index.Result, root string) (parent *index.Result, children []index.Result) {
	for i := range group {
		if sectionRootOf(group[i].Number) == group[i].Number {
			parent = &group[i]
			continue
		}
		children = append(children, group[i])
	}
	sort.SliceStable(children, func(i, j int) bool { return lessRuleNum(children[i].Number, children[j].Number) })
	return parent, children
}

// keywordName recovers the keyword's display name. The parent's body is usually
// just the name ("Deathtouch"); when it is empty or a sentence, fall back to
// title-casing the root's trailing segment so the card still has a label.
func keywordName(parent *index.Result, children []index.Result) string {
	if name := strings.TrimSpace(parent.Body); name != "" && !strings.Contains(name, ".") {
		return name
	}
	parts := strings.Split(parent.Number, ".")
	if len(parts) > 0 {
		return "Rule " + parent.Number
	}
	return parent.Number
}

// keywordBack assembles the answer face: the parent rule followed by each
// sub-rule in order, each on its own line with its number for citation.
func keywordBack(parent *index.Result, children []index.Result) string {
	var b strings.Builder
	if body := strings.TrimSpace(parent.Body); body != "" {
		fmt.Fprintf(&b, "%s. %s", parent.Number, body)
	}
	for _, c := range children {
		body := strings.TrimSpace(c.Body)
		if body == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%s. %s", c.Number, body)
	}
	return b.String()
}

// dndConditions builds a deck of the SRD's condition entries. The SRD indexes
// conditions as titled chunks; a focused search plus a title filter keeps the
// deck to the canonical condition list rather than every mention of the word.
func (g deckGenerator) dndConditions(ctx context.Context) ([]study.Concept, error) {
	hits, err := g.store.Retrieve(ctx, data.CorpusDND, "condition", 40)
	if err != nil {
		return nil, fmt.Errorf("load conditions: %w", err)
	}
	seen := map[string]bool{}
	var out []study.Concept
	for _, h := range hits {
		name := conditionName(h.Title)
		if name == "" || seen[name] {
			continue
		}
		// Keep only entries that look like a condition definition: a short,
		// capitalized title (e.g. "Conditions — Blinded" or just "Blinded"),
		// not a passing mention inside a spell block.
		if !looksLikeCondition(h.Title) {
			continue
		}
		seen[name] = true
		out = append(out, study.Concept{
			Key:    name,
			Corpus: string(data.CorpusDND),
			Topic:  study.TopicConditions,
			Title:  name,
			Front:  fmt.Sprintf("%s — condition. Recall its effect.", name),
			Back:   strings.TrimSpace(h.Body),
			Source: h.Source,
		})
	}
	return out, nil
}

// dndSpells builds a deck over the SRD's spell entries. The spells chapter
// lists each spell as a depth-4 heading directly under its depth-2 chapter,
// skipping depth 3 — so an entry's path number carries the skip as a literal
// 0000 segment (spells/NNNN/0000/NNNN) while structural subsections nest
// without one (spells/NNNN/NNNN). That shape is the entry filter.
func (g deckGenerator) dndSpells(ctx context.Context) ([]study.Concept, error) {
	docs, err := g.store.DNDChildren(ctx, data.CorpusDND, "spells")
	if err != nil {
		return nil, fmt.Errorf("load spells: %w", err)
	}
	seen := map[string]bool{}
	var out []study.Concept
	for _, d := range docs {
		key := data.DNDSectionKey(d.Number)
		parts := strings.Split(key, "/")
		// Entry shape: spells/<chapter>/0000/<ordinal>, first chunk only.
		if len(parts) != 4 || parts[2] != "0000" || !strings.HasSuffix(d.Number, ".0") {
			continue
		}
		name := conditionName(d.Title)
		if name == "" || len(name) > 60 || seen[key] {
			continue
		}
		// Skip structural strays that share the shape but carry sentence-like
		// titles (e.g. table-of-contents fragments from book imports).
		if strings.ContainsAny(name, ".,;:") {
			continue
		}
		seen[key] = true
		out = append(out, study.Concept{
			Key:    key,
			Corpus: string(data.CorpusDND),
			Topic:  study.TopicSpells,
			Title:  name,
			Front:  fmt.Sprintf("%s — spell. Recall what it does.", name),
			Back:   strings.TrimSpace(d.Body),
			Source: d.Source,
		})
	}
	return out, nil
}

// conditionName pulls the trailing segment of a D&D chunk title: the SRD's
// ancestor-joined titles read "Conditions — Blinded", and the condition is the
// last segment. A bare "Blinded" is returned unchanged.
func conditionName(title string) string {
	title = strings.TrimSpace(title)
	if i := strings.LastIndex(title, "—"); i >= 0 {
		return strings.TrimSpace(title[i+len("—"):])
	}
	return title
}

// looksLikeCondition keeps canonical, short condition titles and drops the long
// prose titles of spells and features that happen to mention "condition".
func looksLikeCondition(title string) bool {
	name := conditionName(title)
	if name == "" || len(name) > 40 {
		return false
	}
	// A condition name is a single capitalized word (or two, e.g. "Level
	// exhaustion" is not, but "Blinded", "Unconscious" are).
	for _, r := range name {
		if r == '.' || r == ',' {
			return false
		}
	}
	return name[0] >= 'A' && name[0] <= 'Z'
}

// sectionRootOf strips a numbered rule's trailing letters, returning its
// section root: "702.2a" -> "702.2". Mirrors index.sectionKey without reaching
// into the unexported helper.
func sectionRootOf(number string) string {
	return strings.TrimRight(number, "abcdefghijklmnopqrstuvwxyz")
}

// lessRuleNum is a small comparator for rule numbers used to order a deck. It
// orders dotted segments numerically (702.2 before 702.10) so the deck reads in
// rulebook order.
func lessRuleNum(a, b string) bool {
	sa, _ := splitNum(a)
	sb, _ := splitNum(b)
	for i := 0; i < len(sa) && i < len(sb); i++ {
		if sa[i] != sb[i] {
			return sa[i] < sb[i]
		}
	}
	return len(sa) < len(sb)
}

func splitNum(number string) ([]int, string) {
	num := strings.TrimRight(number, "abcdefghijklmnopqrstuvwxyz")
	letters := number[len(num):]
	var segs []int
	for _, part := range strings.Split(num, ".") {
		n, err := parseInt(part)
		if err != nil {
			return nil, number
		}
		segs = append(segs, n)
	}
	return segs, letters
}

func parseInt(s string) (int, error) {
	var n int
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("not a number: %q", s)
		}
		n = n*10 + int(r-'0')
	}
	if s == "" {
		return 0, fmt.Errorf("empty number")
	}
	return n, nil
}

/* ---------- HTTP handlers ---------- */

// studyCardView is the JSON shape of a study card. The SR schedule travels with
// the card so the UI can render the next-due date and the grade buttons without
// a second round-trip.
type studyCardView struct {
	Key          string  `json:"key"`
	Corpus       string  `json:"corpus"`
	Topic        string  `json:"topic"`
	Number       string  `json:"number,omitempty"`
	Title        string  `json:"title"`
	Front        string  `json:"front"`
	Back         string  `json:"back"`
	Source       string  `json:"source,omitempty"`
	New          bool    `json:"new"`
	Reps         int     `json:"reps"`
	Lapses       int     `json:"lapses"`
	IntervalDays float64 `json:"interval_days"`
	Ease         float64 `json:"ease"`
	DueAt        string  `json:"due_at"`
}

func toStudyCardView(c study.Card) studyCardView {
	return studyCardView{
		Key: c.Key, Corpus: c.Corpus, Topic: c.Topic, Number: c.Number, Title: c.Title,
		Front: c.Front, Back: c.Back, Source: c.Source, New: c.New,
		Reps: c.Reps, Lapses: c.Lapses, IntervalDays: c.IntervalDays, Ease: c.Ease,
		DueAt: c.DueAt.Format(time.RFC3339),
	}
}

// studyEnabled reports whether the study store is wired, writing the error
// response when it is not.
func (s *Server) studyEnabled(w http.ResponseWriter) bool {
	if s.study == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("study mode is not available"))
		return false
	}
	return true
}

func (s *Server) handleStudyQueue(w http.ResponseWriter, r *http.Request) {
	if !s.studyEnabled(w) {
		return
	}
	corpus := parseCorpus(r.URL.Query().Get("corpus"))
	topic := study.TopicFor(string(corpus), r.URL.Query().Get("topic"))
	limit := parseLimit(r.URL.Query().Get("limit"), 20)

	cards, err := s.study.Queue(r.Context(), userID(r), string(corpus), topic, limit, s.deckGen())
	if err != nil {
		if errors.Is(err, study.ErrDeckEmpty) {
			writeJSON(w, http.StatusOK, map[string]any{"cards": []studyCardView{}, "stats": study.Stats{}})
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	stats, err := s.study.Stats(r.Context(), userID(r), string(corpus), topic, s.deckGen())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	views := make([]studyCardView, 0, len(cards))
	for _, c := range cards {
		views = append(views, toStudyCardView(c))
	}
	writeJSON(w, http.StatusOK, map[string]any{"cards": views, "stats": stats})
}

func (s *Server) handleStudyGrade(w http.ResponseWriter, r *http.Request) {
	if !s.studyEnabled(w) {
		return
	}
	var req struct {
		Key    string `json:"key"`
		Corpus string `json:"corpus"`
		Topic  string `json:"topic"`
		Grade  string `json:"grade"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	req.Key = strings.TrimSpace(req.Key)
	if req.Key == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("key is required"))
		return
	}
	grade, ok := study.ParseGrade(req.Grade)
	if !ok {
		writeError(w, http.StatusBadRequest, fmt.Errorf("grade must be again, hard, good, or easy"))
		return
	}
	corpus := parseCorpus(req.Corpus)
	topic := study.TopicFor(string(corpus), req.Topic)

	// Resolve the concept content from the index so the store's write path
	// doesn't have to. A key the deck doesn't recognize is a 404: grading a
	// concept that no longer exists (a reindex dropped it) should not plant a
	// schedule row for it.
	concept, ok := s.lookupConcept(r.Context(), string(corpus), topic, req.Key)
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Errorf("unknown study card"))
		return
	}
	card, err := s.study.Grade(r.Context(), userID(r), concept, grade)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"card": toStudyCardView(card)})
}

// lookupConcept finds a single concept in a deck by key. Used on the grade path
// to recover the card content without re-running the whole queue.
func (s *Server) lookupConcept(ctx context.Context, corpus, topic, key string) (study.Concept, bool) {
	concepts, err := s.deckGen().Concepts(ctx, corpus, topic)
	if err != nil {
		return study.Concept{}, false
	}
	for _, c := range concepts {
		if c.Key == key {
			return c, true
		}
	}
	return study.Concept{}, false
}

// deckGen lazily builds the deck generator over the index store.
func (s *Server) deckGen() study.Generator {
	return deckGenerator{store: s.store}
}
