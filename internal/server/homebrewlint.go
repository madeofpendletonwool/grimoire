package server

// The homebrew linter's HTTP surface (MAD-385): lint a saved homebrew
// monster or item. The structural and computed checks run with no model
// and no network; retrieval reads the local index when it is wired; and
// the model, when configured, writes the comparison up from what the
// engine produced — never a finding of its own. The response carries
// findings with their bases, neighbours with their deep links, and no
// field anywhere by which a legal/illegal verdict could be expressed.

import (
	"context"
	"net/http"

	"github.com/madeofpendletonwool/grimoire/internal/canon"
	"github.com/madeofpendletonwool/grimoire/internal/data"
	"github.com/madeofpendletonwool/grimoire/internal/homebrew"
	"github.com/madeofpendletonwool/grimoire/internal/items"
)

// lintModel adapts the canon engine's model client to the linter's, so
// both model passes share one production adapter and the tests keep
// faking at their own seams.
type lintModel struct{ m canon.ModelClient }

func (l lintModel) ModelName() string { return l.m.ModelName() }

func (l lintModel) Complete(ctx context.Context, system, user string) (homebrew.Completion, error) {
	c, err := l.m.Complete(ctx, system, user)
	return homebrew.Completion(c), err
}

// WithLintModel pins the linter's model client. The default is the
// shared llm client through the canon adapter, when configured; the
// explicit builder exists so a test can script the model pass the same
// way the canon engine's tests do.
func (s *Server) WithLintModel(m homebrew.ModelClient) *Server {
	s.lintModelOverride = m
	return s
}

// lintEngine builds the engine for one request. The index and the model
// are both optional at the engine's own seams; the deterministic checks
// answer without either.
func (s *Server) lintEngine() *homebrew.Engine {
	e := &homebrew.Engine{Index: s.store, Corpus: data.CorpusDND}
	if s.lintModelOverride != nil {
		e.Model = s.lintModelOverride
	} else if s.llm != nil && s.llm.Configured() {
		e.Model = lintModel{canon.NewLLMModel(s.llm)}
	}
	return e
}

// handleMonsterLint lints one of the caller's homebrew monsters — the
// same owner-scoped read the shelf serves, plus the engine.
func (s *Server) handleMonsterLint(w http.ResponseWriter, r *http.Request) {
	if !s.homebrewEnabled(w) {
		return
	}
	m, err := s.homebrew.Get(r.Context(), userID(r), r.PathValue("id"))
	if err != nil {
		writeHomebrewError(w, err)
		return
	}
	rep := s.lintEngine().LintMonster(r.Context(), homebrew.MonsterInput{
		Statblock:   m.Statblock,
		RequestedCR: m.RequestedCR,
	})
	writeJSON(w, http.StatusOK, map[string]any{"report": rep})
}

// handleItemLint lints one of the caller's homebrew items against the
// SRD shelf. A missing mirror degrades into the report's notices — the
// structural checks answer regardless.
func (s *Server) handleItemLint(w http.ResponseWriter, r *http.Request) {
	if !s.homebrewItemsEnabled(w) {
		return
	}
	m, err := s.itemHomebrew.Get(r.Context(), userID(r), r.PathValue("id"))
	if err != nil {
		writeItemError(w, err)
		return
	}
	var corpus []items.Item
	if s.itemCatalog != nil {
		corpus = s.itemCatalog.All()
	}
	rep := s.lintEngine().LintItem(r.Context(), homebrew.ItemInput{
		Design: m.Design,
		Corpus: corpus,
	})
	writeJSON(w, http.StatusOK, map[string]any{"report": rep})
}
