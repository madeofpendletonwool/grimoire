// Package canon is the canon engine: the port of The History of Arda's
// fact-validation pipeline, retargeted at campaign sessions.
//
// The design principle is mechanical, not aspirational (Arda's
// docs/extraction.md, ported): the model extracts and cites; it never decides
// what is true. Every rule in this package exists to make that checkable.
//
// This file's stage (MAD-307) is extraction: one work unit is one
// session_source, chunked with overlap so a span never straddles a chunk
// boundary silently; the model is given the chunk, the campaign's entity
// list, the relationship vocabulary and the party roster; and its output
// lands as candidates under the span rule:
//
//	Every extracted candidate must cite the game session it came from and a
//	verbatim span — source_id plus byte offsets — of the transcript, note or
//	journal that supports it. A candidate that cannot is dropped and logged
//	with its reason, never staged.
//
// Staged candidates are written ONLY to canon_candidates (ADR 3: stage, never
// write-then-downgrade). Nothing extraction produces touches the campaign
// graph, so a candidate is invisible to every scoped retrieval path in
// internal/knowledge and internal/campaign — including the DM's chat — until
// a human accepts it through the review queue (MAD-310). That is a deliberate
// divergence from Arda, which writes claims into the graph and downgrades
// later: Arda's reader sees the book months later, Grimoire's DM is running a
// game on Thursday, and a wrong fact in canon poisons play immediately and
// invisibly.
//
// The schema is owned by migration 0006; this package creates no tables,
// following the pattern internal/campaign set.
package canon

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
)

// Errors. The canon engine reuses the campaign vocabulary, the same way
// internal/knowledge and internal/gamesession do: callers branch on one set
// of sentinels across every campaign package.
var (
	ErrNotFound = campaign.ErrNotFound
	ErrInvalid  = campaign.ErrInvalid
)

// vocabularies for candidate kinds.
const (
	KindFact         = "fact"
	KindEvent        = "event"
	KindDiscovery    = "discovery"
	KindRelationship = "relationship"
	KindEntity       = "entity"
)

// Drop reasons — the vocabulary logged per dropped candidate and counted in
// run stats. Distinguish uncited (the span rule itself) from unparseable
// (the wire) from unreferencable (graph integrity).
const (
	DropUncited           = "uncited"             // no quote at all — the span rule's hard floor
	DropQuoteNotInSource  = "quote_not_in_source" // quote is not verbatim in the source
	DropSpanOutsideChunk  = "span_outside_chunk"  // quote is in the source but not in the chunk the model saw
	DropInvalidID         = "invalid_local_id"    // local id is not a lowercase slug
	DropDuplicateID       = "duplicate_local_id"
	DropDuplicateCand     = "duplicate_candidate"
	DropUnknownEntity     = "unknown_entity" // entity ref resolves nowhere (campaign nor payload)
	DropInvalidKind       = "invalid_entity_kind"
	DropInvalidRelType    = "invalid_rel_type"
	DropInvalidStance     = "invalid_stance"
	DropInvalidVisibility = "invalid_visibility"
	DropInvalidConfidence = "invalid_confidence"
	DropInvalidObject     = "invalid_object" // fact object must be exactly one of entity / literal
	DropDanglingFactRef   = "dangling_fact_ref"
	DropEmptyStatement    = "empty_statement"
	DropMaxCandidates     = "max_candidates"       // run reached CANON_MAX_CANDIDATES mid-payload
	DropUnparseable       = "unparseable_response" // whole response was not JSON
)

// Run status values.
const (
	RunRunning   = "running"
	RunCompleted = "completed" // every unit done
	RunStopped   = "stopped"   // a guard stopped it; remainder deferred, resumable
	RunFailed    = "failed"    // an error aborted it; done units stay committed
)

// Stop reasons.
const (
	StopBudget     = "budget"
	StopCandidates = "candidates"
	StopUnits      = "units"
	StopError      = "error"
)

// Ledger status values.
const (
	LedgerDone  = "done"
	LedgerError = "error"
)

// Completion is one model response with its token accounting.
type Completion struct {
	Text         string
	InputTokens  int
	OutputTokens int
}

// ModelClient is the slice of the LLM surface extraction needs: one
// non-streaming prompt exchange with usage. The production adapter is the
// shared internal/llm client; tests replay fixture responses through it.
type ModelClient interface {
	// ModelName names the model for the run record.
	ModelName() string
	// Complete answers one system+user exchange.
	Complete(ctx context.Context, system, user string) (Completion, error)
}

// llmModel adapts the shared client to ModelClient.
type llmModel struct{ c *llm.Client }

// NewLLMModel adapts the shared internal/llm client to the canon engine's
// ModelClient. The client's own provider failover applies underneath.
func NewLLMModel(c *llm.Client) ModelClient { return llmModel{c: c} }

// NewLLMValidator adapts the shared client for the adversarial pass with the
// model CANON_VALIDATE_MODEL names (cfg.ValidateModel). An empty model
// returns the same adapter as NewLLMModel — the two passes then share a
// model, which the configuration docs warn is not adversarial validation.
func NewLLMValidator(c *llm.Client, model string) ModelClient {
	if strings.TrimSpace(model) == "" {
		return NewLLMModel(c)
	}
	return NewLLMModel(c.WithModel(model))
}

func (m llmModel) ModelName() string { return m.c.Model() }

func (m llmModel) Complete(ctx context.Context, system, user string) (Completion, error) {
	text, usage, err := m.c.AnswerPromptUsage(ctx, system, user)
	if err != nil {
		return Completion{}, err
	}
	return Completion{Text: text, InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens}, nil
}

/* ---------- configuration ---------- */

// DefaultAgreementThreshold is the score at or above which the adversarial
// pass counts as agreement (CANON_AGREEMENT_THRESHOLD). Arda's default,
// kept: below it the checker could not decide, and the candidate is flagged
// for review rather than confirmed or downgraded.
const DefaultAgreementThreshold = 0.8

// Config carries the budget guards (epistemics.md § "Cost"). Every knob has a
// default that errs small: self-hosters pay for their own tokens, and a
// runaway extraction over a four-hour transcript is a real way to make
// someone hate this feature.
type Config struct {
	// BudgetUSD is the soft USD spend ceiling per run. Before each model
	// call, the estimated spend so far is compared against it; at or over,
	// the run stops (remainder deferred, resumable). Zero disables the USD
	// guard; with prices unset the budget falls back to the candidate cap
	// and cost is tracked in tokens only — Arda's rule, kept.
	BudgetUSD float64
	// MaxCandidates is the hard per-run cap on staged candidates. At or
	// over, the run stops. Guards the review queue's size as much as cost.
	MaxCandidates int
	// BatchSize bounds how many units (sources) one run processes; the
	// remainder is deferred and a later run picks them up.
	BatchSize int
	// Interval is the minimum spacing between model calls — one global
	// politeness gate, so a run cannot hammer an endpoint.
	Interval time.Duration
	// PriceInMTok / PriceOutMTok price a million input / output tokens in
	// USD, for the budget guard. Zero means prices are unknown and the USD
	// budget cannot be enforced.
	PriceInMTok  float64
	PriceOutMTok float64
	// AgreementThreshold is the score at or above which a verdict counts as
	// agreement; below it the checker could not decide and the candidate is
	// flagged for review. Read by the adversarial pass only.
	AgreementThreshold float64
	// ValidateModel names the model the adversarial pass should run
	// (CANON_VALIDATE_MODEL). Empty means the shared client's own model —
	// the degenerate configuration where both passes are the same model,
	// which is not adversarial validation. Carried for the wiring layer:
	// the store takes its clients by constructor, and this is the knob that
	// makes the two passes genuinely two different models.
	ValidateModel string
}

// DefaultConfig is the conservative built-in configuration: no USD budget
// (prices unknown), 500 candidates, 8 sources per run, one call per second,
// agreement at 0.8.
func DefaultConfig() Config {
	return Config{
		MaxCandidates:      500,
		BatchSize:          8,
		Interval:           time.Second,
		AgreementThreshold: DefaultAgreementThreshold,
	}
}

// Chunking defaults. ChunkTarget is the byte size the chunker aims for; the
// cut lands on the last paragraph break before it, so quotes are never split
// mid-sentence by construction, and ChunkOverlap carries the tail forward so
// a span straddling the cut appears whole in the next chunk too. A candidate
// whose quote resolves outside the chunk the model saw is dropped with
// span_outside_chunk — the boundary is enforced, never trusted.
const (
	ChunkTarget  = 12000
	ChunkOverlap = 800
)

// Store is the canon engine on the shared database handle. The schema must
// already be applied (migrate.Up runs before anything serves).
type Store struct {
	db        *sql.DB
	model     ModelClient
	validator ModelClient
	cfg       Config
	now       func() time.Time
	// The graph and knowledge stores the review queue (MAD-310) writes into
	// on accept. Wired with WithGraphStores; nil keeps the deterministic and
	// queue-read surfaces working while accepting is refused.
	campaigns *campaign.Store
	knowledge *knowledge.Store
}

// New builds a canon store on an open, migrated database handle with the
// given model client and configuration. The adversarial pass falls back to
// the same client — allowed, but two passes of the same model is not
// adversarial validation; see NewWithValidator.
func New(db *sql.DB, model ModelClient, cfg Config) (*Store, error) {
	return NewWithValidator(db, model, nil, cfg)
}

// NewWithValidator builds a canon store whose adversarial pass runs on its
// own model client, so extraction and validation can genuinely be two
// different models (CANON_VALIDATE_MODEL). A nil validator falls back to the
// extractor's client.
func NewWithValidator(db *sql.DB, model, validator ModelClient, cfg Config) (*Store, error) {
	if db == nil {
		return nil, errors.New("canon: nil database handle")
	}
	if model == nil {
		return nil, errors.New("canon: nil model client")
	}
	if validator == nil {
		validator = model
	}
	if cfg.MaxCandidates <= 0 {
		cfg.MaxCandidates = 500
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 8
	}
	if cfg.Interval < 0 {
		cfg.Interval = 0
	}
	if cfg.AgreementThreshold <= 0 || cfg.AgreementThreshold > 1 {
		cfg.AgreementThreshold = DefaultAgreementThreshold
	}
	return &Store{db: db, model: model, validator: validator, cfg: cfg, now: time.Now().UTC}, nil
}

// NewOffline builds a canon store with no model at all: the deterministic
// engine and its flag ledger only. This is the constructor behind
// `grimoire canon check` and the check API — the consistency engine needs no
// key configured, and saying so at construction (rather than crashing in a
// model call) is what makes the offline guarantee real. The extraction and
// adversarial entry points refuse with a clear error on this store.
func NewOffline(db *sql.DB) (*Store, error) {
	if db == nil {
		return nil, errors.New("canon: nil database handle")
	}
	return &Store{db: db, cfg: DefaultConfig(), now: time.Now().UTC}, nil
}

// errOffline is the error the model-driven stages return on a store built by
// NewOffline.
var errOffline = errors.New("canon: no model configured (offline store; extraction and validation need a model client)")

// IsOffline reports whether err is the offline store's refusal — the HTTP
// layer maps it to 503 rather than a store error.
func IsOffline(err error) bool { return errors.Is(err, errOffline) }

// ConfigFromEnv reads the CANON_* budget guards from the environment, the
// same one-key-one-knob shape the rest of the configuration uses. Empty or
// absent values fall back to DefaultConfig's value for that knob.
func ConfigFromEnv(getenv func(string) string) Config {
	cfg := DefaultConfig()
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	if v, ok := envFloat(getenv, "CANON_BUDGET_USD"); ok {
		cfg.BudgetUSD = v
	}
	if v, ok := envInt(getenv, "CANON_MAX_CANDIDATES"); ok {
		cfg.MaxCandidates = int(v)
	}
	if v, ok := envInt(getenv, "CANON_BATCH_SIZE"); ok {
		cfg.BatchSize = int(v)
	}
	if raw := strings.TrimSpace(getenv("CANON_REQUEST_INTERVAL")); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d >= 0 {
			cfg.Interval = d
		}
	}
	if v, ok := envFloat(getenv, "CANON_PRICE_IN_MTOK"); ok {
		cfg.PriceInMTok = v
	}
	if v, ok := envFloat(getenv, "CANON_PRICE_OUT_MTOK"); ok {
		cfg.PriceOutMTok = v
	}
	if v, ok := envFloat(getenv, "CANON_AGREEMENT_THRESHOLD"); ok && v > 0 && v <= 1 {
		cfg.AgreementThreshold = v
	}
	if v := strings.TrimSpace(getenv("CANON_VALIDATE_MODEL")); v != "" {
		cfg.ValidateModel = v
	}
	return cfg
}

func envFloat(getenv func(string) string, key string) (float64, bool) {
	raw := strings.TrimSpace(getenv(key))
	if raw == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v < 0 {
		return 0, false
	}
	return v, true
}

func envInt(getenv func(string) string, key string) (int64, bool) {
	raw := strings.TrimSpace(getenv(key))
	if raw == "" {
		return 0, false
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < 0 {
		return 0, false
	}
	return v, true
}

// costUSD prices one exchange with the configured prices. Zero prices mean
// unknown cost, and unknown cost is never billed against a budget.
func (c Config) costUSD(in, out int) float64 {
	if c.PriceInMTok <= 0 && c.PriceOutMTok <= 0 {
		return 0
	}
	return float64(in)/1e6*c.PriceInMTok + float64(out)/1e6*c.PriceOutMTok
}

// Config reports the store's effective configuration.
func (s *Store) Config() Config { return s.cfg }

func nullString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func nullInt64(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}
