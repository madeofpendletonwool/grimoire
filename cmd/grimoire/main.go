// Command grimoire runs the MTG + D&D rules reference server.
//
// Usage:
//
//	grimoire serve          serve the UI + API (builds the index on first run)
//	grimoire index          (re)build the search index and exit
//	grimoire [-h]           help
//
// Configuration is via environment variables (see .env.example):
//
//	GRIMOIRE_ADDR       listen address for serve (default :8080)
//	GRIMOIRE_DB         SQLite index path (default data/grimoire.db)
//	GRIMOIRE_SESSION_TTL       session lifetime, Go duration (default 720h)
//	GRIMOIRE_OPEN_REGISTRATION keep account creation open after the first
//	                           account exists (default false)
//	GRIMOIRE_INVITE_TTL        how long admin invite links stay usable, Go
//	                           duration (default 168h / 7d; 0 = never expire)
//	GRIMOIRE_ANSWER_CACHE_TTL  how long cached Q&A answers stay fresh, Go
//	                           duration (default 168h / 7d)
//	ANTHROPIC_BASE_URL  LLM endpoint (default https://api.anthropic.com; z.ai: https://api.z.ai/api/anthropic)
//	ANTHROPIC_API_KEY   LLM secret key (enables the Q&A chat)
//	ANTHROPIC_MODEL     model name (e.g. glm-4.6, claude-3-5-sonnet-20241022)
//	ANTHROPIC_FALLBACK_BASE_URL, ANTHROPIC_FALLBACK_API_KEY, ANTHROPIC_FALLBACK_MODEL
//	                    standby provider used when the primary fails (out of
//	                    quota, bad key, overloaded, unreachable). Base URL
//	                    defaults to Anthropic's, model to ANTHROPIC_MODEL.
//	                    Further rungs: ANTHROPIC_FALLBACK_2_*, _3_, ...
//	EMBEDDINGS_BASE_URL OpenAI-compatible embeddings endpoint (default https://api.openai.com/v1)
//	EMBEDDINGS_API_KEY  embeddings secret key (enables semantic retrieval when set with EMBEDDINGS_MODEL)
//	EMBEDDINGS_MODEL    embeddings model name (e.g. text-embedding-3-small)
//	SCRYFALL_BASE_URL   MTG card lookup endpoint (default https://api.scryfall.com; no key needed)
//	MTG_RULES_URL       override MTG comp rules source
//	MTGJSON_URL         override MTGJSON AtomicCards source (card-name dictionary)
//	OPEN5E_BASE_URL     D&D entity lookup endpoint (default https://api.open5e.com; no key needed)
//	DND_REPO            override D&D SRD repo "owner/name"
//	DND_REF             override D&D SRD git ref
//	DND_DOCS_DIR        local D&D documents (markdown/text) imported alongside the SRD
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/auth"
	"github.com/madeofpendletonwool/grimoire/internal/cache"
	"github.com/madeofpendletonwool/grimoire/internal/cards"
	"github.com/madeofpendletonwool/grimoire/internal/chat"
	"github.com/madeofpendletonwool/grimoire/internal/data"
	"github.com/madeofpendletonwool/grimoire/internal/embeddings"
	"github.com/madeofpendletonwool/grimoire/internal/encounter"
	"github.com/madeofpendletonwool/grimoire/internal/entities"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
	"github.com/madeofpendletonwool/grimoire/internal/rulings"
	"github.com/madeofpendletonwool/grimoire/internal/server"
	"github.com/madeofpendletonwool/grimoire/internal/study"
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "-h" || os.Args[1] == "--help" || os.Args[1] == "help") {
		usage()
		return
	}

	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	switch cmd {
	case "serve":
		if err := runServe(); err != nil {
			log.Fatalf("serve: %v", err)
		}
	case "index":
		if err := runIndex(); err != nil {
			log.Fatalf("index: %v", err)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `grimoire — MTG + D&D rules reference

Usage:
  grimoire serve     serve the UI + API (builds the index on first run)
  grimoire index     (re)build the search index and exit

Env (see .env.example):
  GRIMOIRE_ADDR, GRIMOIRE_DB, GRIMOIRE_SESSION_TTL, GRIMOIRE_OPEN_REGISTRATION,
  GRIMOIRE_INVITE_TTL, GRIMOIRE_ANSWER_CACHE_TTL, ANTHROPIC_BASE_URL, ANTHROPIC_API_KEY, ANTHROPIC_MODEL,
  ANTHROPIC_FALLBACK_BASE_URL, ANTHROPIC_FALLBACK_API_KEY, ANTHROPIC_FALLBACK_MODEL (and _2_, _3_, ...),
  EMBEDDINGS_BASE_URL, EMBEDDINGS_API_KEY, EMBEDDINGS_MODEL,
  SCRYFALL_BASE_URL, MTG_RULES_URL, MTGJSON_URL, OPEN5E_BASE_URL, DND_REPO, DND_REF, DND_DOCS_DIR`)
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func dbPath() string { return env("GRIMOIRE_DB", "data/grimoire.db") }
func addr() string   { return env("GRIMOIRE_ADDR", ":8080") }

// sessionTTL reads the session lifetime. An unparseable value falls back to the
// default rather than failing the boot — being logged out is a nuisance, a
// server that refuses to start over a typo is worse.
func sessionTTL() time.Duration {
	raw := os.Getenv("GRIMOIRE_SESSION_TTL")
	if raw == "" {
		return auth.DefaultSessionTTL
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		log.Printf("GRIMOIRE_SESSION_TTL=%q is not a valid duration — using %s", raw, auth.DefaultSessionTTL)
		return auth.DefaultSessionTTL
	}
	return d
}

func openRegistration() bool {
	switch strings.ToLower(os.Getenv("GRIMOIRE_OPEN_REGISTRATION")) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// inviteTTL reads how long a freshly minted invite link stays usable. An empty
// value picks the default (7d); a value of zero ("0s") means invites never
// expire. An unparseable value falls back to the default — a burned invite that
// a friend has to ask about again is a nuisance, a server that refuses to start
// over a typo is worse.
func inviteTTL() time.Duration {
	raw := os.Getenv("GRIMOIRE_INVITE_TTL")
	if raw == "" {
		return auth.DefaultInviteTTL
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		log.Printf("GRIMOIRE_INVITE_TTL=%q is not a valid duration — using %s", raw, auth.DefaultInviteTTL)
		return auth.DefaultInviteTTL
	}
	if d < 0 {
		return 0 // negative is treated as "never expire", same as zero
	}
	return d
}

// answerCacheTTL reads how long a cached Q&A answer stays fresh. An unparseable
// or non-positive value falls back to the default — a stale cache that misses
// is a nuisance, a server that refuses to start over a typo is worse.
func answerCacheTTL() time.Duration {
	raw := os.Getenv("GRIMOIRE_ANSWER_CACHE_TTL")
	if raw == "" {
		return cache.DefaultTTL
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		log.Printf("GRIMOIRE_ANSWER_CACHE_TTL=%q is not a valid duration — using %s", raw, cache.DefaultTTL)
		return cache.DefaultTTL
	}
	return d
}

func llmConfig() llm.Config {
	return llm.Config{
		BaseURL: env("ANTHROPIC_BASE_URL", llm.DefaultBaseURL),
		APIKey:  os.Getenv("ANTHROPIC_API_KEY"),
		Model:   env("ANTHROPIC_MODEL", "glm-4.6"),
	}
}

// llmFallbacks reads the standby providers the chat falls back to when the
// primary fails — an exhausted balance, a dead key, an endpoint having a bad
// day. They are read from ANTHROPIC_FALLBACK_* and then numbered
// ANTHROPIC_FALLBACK_2_*, _3_, ... and are tried in that order. The chain stops
// at the first gap, so a missing key ends it rather than skipping a rung.
//
// A fallback needs only a key: its base URL defaults to Anthropic's (the usual
// destination when a cheaper gateway runs dry) and its model to the primary's
// (right when the same model is served from a second account).
func llmFallbacks(primary llm.Config) []llm.Config {
	var out []llm.Config
	for i := 1; ; i++ {
		prefix := "ANTHROPIC_FALLBACK_"
		if i > 1 {
			prefix = fmt.Sprintf("ANTHROPIC_FALLBACK_%d_", i)
		}
		key := os.Getenv(prefix + "API_KEY")
		if strings.TrimSpace(key) == "" {
			return out
		}
		out = append(out, llm.Config{
			BaseURL: env(prefix+"BASE_URL", llm.DefaultBaseURL),
			APIKey:  key,
			Model:   env(prefix+"MODEL", primary.Model),
		})
	}
}

// llmClient builds the chat client over the primary provider and its fallbacks.
func llmClient() *llm.Client {
	primary := llmConfig()
	return llm.New(primary, llmFallbacks(primary)...)
}

// embeddingsConfig reads the OpenAI-compatible embeddings endpoint settings.
// Default base URL is OpenAI's; z.ai and other compatible gateways work by
// overriding EMBEDDINGS_BASE_URL (e.g. https://api.z.ai/v1).
func embeddingsConfig() embeddings.Config {
	return embeddings.Config{
		BaseURL: env("EMBEDDINGS_BASE_URL", embeddings.DefaultBaseURL),
		APIKey:  os.Getenv("EMBEDDINGS_API_KEY"),
		Model:   env("EMBEDDINGS_MODEL", ""),
	}
}

// embeddingsClient returns a configured embeddings client, or nil when
// embeddings are not configured (the default — retrieval is then FTS5-only).
func embeddingsClient() *embeddings.Client {
	c := embeddings.New(embeddingsConfig())
	if !c.Configured() {
		return nil
	}
	return c
}

func cardsService() *cards.Service {
	return cards.NewWithBase(env("SCRYFALL_BASE_URL", cards.DefaultBaseURL))
}

// rulingsService shares the Scryfall endpoint with cardsService: the rulings
// layer resolves card names to ids via the same Scryfall API, so a single
// SCRYFALL_BASE_URL override retargets both.
func rulingsService() *rulings.Service {
	return rulings.NewWithBase(env("SCRYFALL_BASE_URL", rulings.DefaultBaseURL))
}

// mtgjsonURL returns the AtomicCards endpoint used to build the card-name
// dictionary.
func mtgjsonURL() string { return env("MTGJSON_URL", data.DefaultMTGJSONURL) }

// open5eBaseURL returns the Open5e endpoint used for both the D&D entity
// resolver and the entity-name dictionary build, so one override retargets
// both.
func open5eBaseURL() string { return env("OPEN5E_BASE_URL", entities.DefaultBaseURL) }

func fetchOpts() data.FetchOptions {
	return data.FetchOptions{
		MTGURL:     os.Getenv("MTG_RULES_URL"),
		DNDRepo:    os.Getenv("DND_REPO"),
		DNDRef:     os.Getenv("DND_REF"),
		DNDDocsDir: os.Getenv("DND_DOCS_DIR"),
	}
}

func openStore() (*index.Store, error) {
	db := dbPath()
	if dir := filepath.Dir(db); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create db dir: %w", err)
		}
	}
	return index.Open(db)
}

// ensureIndexed builds the index if the store is empty.
func ensureIndexed(ctx context.Context, store *index.Store) error {
	ok, err := store.Indexed(ctx)
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	log.Println("index empty — building (this fetches MTG + D&D rules; first run takes a moment)...")
	return buildIndex(ctx, store)
}

func buildIndex(ctx context.Context, store *index.Store) error {
	ds, err := data.BuildDataset(ctx, fetchOpts())
	if err != nil {
		return err
	}
	total := len(ds.Records)
	for c, m := range ds.Meta {
		log.Printf("  indexed %-3s %s — %d records (version %s)", c, m.Name, m.RecordCount, m.Version)
	}
	log.Printf("indexing %d total records into SQLite + FTS5...", total)
	if err := store.Index(ctx, ds); err != nil {
		return err
	}
	log.Printf("index built: %d records", total)

	// Semantic vectors are best-effort, like the card dictionary: without them
	// retrieval is FTS5-only; with them the Q&A step gains semantic recall for
	// questions that share no keywords with the rule they need.
	if err := store.IndexEmbeddings(ctx); err != nil {
		log.Printf("embeddings skipped: %v (retrieval will be FTS5-only)", err)
	} else if embedded, _ := store.Embedded(ctx); embedded {
		log.Printf("indexed semantic vectors")
	}

	// The card dictionary is best-effort: the chat still works without it,
	// just with weaker card detection (text heuristics only).
	if err := buildCardDictionary(ctx, store); err != nil {
		log.Printf("card dictionary skipped: %v (chat will detect cards by text heuristics only)", err)
	}

	// The D&D entity-name dictionary is equally best-effort: without it the
	// D&D chat detects entity mentions by text heuristics only.
	if err := buildDNDNameDictionary(ctx, store); err != nil {
		log.Printf("d&d name dictionary skipped: %v (chat will detect entities by text heuristics only)", err)
	}

	// Record the local-books fingerprint the index was built against, so the
	// next boot can tell whether the library changed and rebuild on its own.
	if err := store.SetMeta(ctx, "dnd_docs_fingerprint", data.DNDDocsFingerprint(fetchOpts().DNDDocsDir)); err != nil {
		log.Printf("record books fingerprint: %v", err)
	}
	return nil
}

// buildCardDictionary fetches MTGJSON AtomicCards and stores the card-name
// set so the chat can detect card mentions the text heuristics miss.
func buildCardDictionary(ctx context.Context, store *index.Store) error {
	names, err := data.FetchCardNames(ctx, mtgjsonURL())
	if err != nil {
		return fmt.Errorf("fetch mtgjson: %w", err)
	}
	if err := store.IndexCards(ctx, names); err != nil {
		return fmt.Errorf("index card names: %w", err)
	}
	log.Printf("indexed %d card names from MTGJSON", len(names))
	return nil
}

// buildDNDNameDictionary lists the SRD entity names from Open5e (spells,
// creatures, magic items, feats, conditions, weapons) and stores them so the
// D&D chat can detect entity mentions the text heuristics miss — the
// counterpart of the MTG card-name dictionary.
func buildDNDNameDictionary(ctx context.Context, store *index.Store) error {
	names, err := data.FetchDNDNames(ctx, open5eBaseURL())
	if err != nil {
		return fmt.Errorf("fetch open5e names: %w", err)
	}
	if err := store.IndexEntityNames(ctx, names); err != nil {
		return fmt.Errorf("index entity names: %w", err)
	}
	log.Printf("indexed %d D&D entity names from Open5e", len(names))
	return nil
}

// loadCardDictionary builds the in-memory card dictionary from the store. If
// the table is empty (a fresh serve that did not run a full index build, or
// an install upgraded from before the dictionary existed) it fetches MTGJSON
// once. A nil dictionary is returned when no names are available; the chat
// then falls back to text heuristics.
func loadCardDictionary(ctx context.Context, store *index.Store) *cards.Dictionary {
	names, err := store.LoadCardNames(ctx)
	if err != nil {
		log.Printf("load card names: %v", err)
		return nil
	}
	if len(names) == 0 {
		if err := buildCardDictionary(ctx, store); err != nil {
			log.Printf("card dictionary unavailable: %v", err)
			return nil
		}
		names, err = store.LoadCardNames(ctx)
		if err != nil {
			log.Printf("load card names: %v", err)
			return nil
		}
	}
	if len(names) == 0 {
		return nil
	}
	d := cards.NewDictionary(names)
	log.Printf("loaded card dictionary: %d names", d.Size())
	return d
}

// loadDNDNameDictionary builds the in-memory D&D entity dictionary from the
// store, fetching Open5e's name listings once if the table is empty (a fresh
// serve without a full index build). A nil dictionary degrades detection to
// text heuristics.
func loadDNDNameDictionary(ctx context.Context, store *index.Store) *cards.Dictionary {
	names, err := store.LoadEntityNames(ctx)
	if err != nil {
		log.Printf("load entity names: %v", err)
		return nil
	}
	if len(names) == 0 {
		if err := buildDNDNameDictionary(ctx, store); err != nil {
			log.Printf("d&d name dictionary unavailable: %v", err)
			return nil
		}
		names, err = store.LoadEntityNames(ctx)
		if err != nil {
			log.Printf("load entity names: %v", err)
			return nil
		}
	}
	if len(names) == 0 {
		return nil
	}
	d := cards.NewDictionary(names)
	log.Printf("loaded D&D name dictionary: %d names", d.Size())
	return d
}

func runIndex() error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	store, err := openStore()
	if err != nil {
		return err
	}
	defer store.Close()
	store.SetEmbedder(embeddingsClient())

	if err := store.Reset(); err != nil {
		return fmt.Errorf("reset: %w", err)
	}
	return buildIndex(ctx, store)
}

func runServe() error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	store, err := openStore()
	if err != nil {
		return err
	}
	defer store.Close()
	embedClient := embeddingsClient()
	store.SetEmbedder(embedClient)

	if err := ensureIndexed(ctx, store); err != nil {
		return fmt.Errorf("ensure index: %w", err)
	}

	// A changed local library reindexes on its own: fingerprint the books
	// directory and compare against the one the index was built with. Nobody
	// should need a terminal because a PDF was added.
	if dir := fetchOpts().DNDDocsDir; dir != "" {
		now := data.DNDDocsFingerprint(dir)
		built, err := store.GetMeta(ctx, "dnd_docs_fingerprint")
		if err != nil {
			log.Printf("books fingerprint check: %v", err)
		} else if now != built {
			log.Println("local D&D books changed since the last build — reindexing...")
			if err := buildIndex(ctx, store); err != nil {
				log.Printf("books reindex failed (serving the existing index): %v", err)
			}
		}
	}

	// Populate semantic vectors when embeddings are configured but a rules
	// index already exists without them (e.g. enabled after first boot). A
	// failure is best-effort: retrieval falls back to FTS5-only.
	if embedClient != nil {
		embedded, embedErr := store.Embedded(ctx)
		if embedErr != nil {
			log.Printf("embeddings check: %v", embedErr)
		} else if !embedded {
			log.Printf("indexing semantic vectors (model %s)...", embedClient.Model())
			if err := store.IndexEmbeddings(ctx); err != nil {
				log.Printf("embeddings skipped: %v (retrieval will be FTS5-only)", err)
			} else {
				log.Printf("indexed semantic vectors (model %s)", embedClient.Model())
			}
		}
	}

	cardDict := loadCardDictionary(ctx, store)
	dndDict := loadDNDNameDictionary(ctx, store)

	// Wire each corpus's entity resolver onto its registered definition. MTG's
	// Scryfall resolver shares the card service + dictionary the chat already
	// uses; D&D's Open5e resolver grounds spells/creatures/items/feats from the
	// SRD, with its own name dictionary for the mentions the heuristics miss.
	// Done after the dictionaries load so a best-effort dictionary miss
	// (text-heuristics-only detection) still leaves each corpus a working
	// resolver.
	data.SetResolver(data.CorpusMTG, entities.NewScryfall(cardsService(), cardDict))
	open5e := entities.NewWithBase(open5eBaseURL())
	open5e.SetDictionary(dndDict)
	data.SetResolver(data.CorpusDND, open5e)

	// Chat history shares the index's SQLite file and connection pool.
	// Rebuilding the rules index does not touch these tables.
	chats, err := chat.New(store.DB())
	if err != nil {
		return err
	}

	// Cached Q&A answers share that file too, for the same reason: a rules
	// reindex only clears the docs tables, so cached entries survive the
	// rebuild (they stop hitting on their own once the grounding shifts).
	answers, err := cache.New(store.DB(), answerCacheTTL())
	if err != nil {
		return err
	}

	// Accounts and sessions share that file too, for the same reason.
	users, err := auth.New(store.DB(), sessionTTL(), inviteTTL())
	if err != nil {
		return err
	}

	// Spaced-repetition review schedules share that file too: the concept keys
	// are stable rule numbers, so a reindex never strands a user's progress.
	studies, err := study.New(store.DB())
	if err != nil {
		return err
	}

	// Saved encounters share that file as well; difficulty verdicts are never
	// stored, only the party and roster they are recomputed from.
	encounters, err := encounter.New(store.DB())
	if err != nil {
		return err
	}
	// Upgrade path: an install that predates accounts has history filed under
	// the anonymous owner. Hand it to the first keeper on the way up, so it is
	// already theirs by the time they sign in.
	if adopted, err := server.AdoptAnonymousChats(ctx, users, chats); err != nil {
		return fmt.Errorf("adopt anonymous chats: %w", err)
	} else if adopted > 0 {
		log.Printf("adopted %d pre-authentication conversations", adopted)
	}

	chatClient := llmClient()
	srv, err := server.New(store, chatClient, cardsService(), rulingsService(), cardDict, chats, answers, studies,
		server.Auth{Users: users, OpenRegistration: openRegistration()},
		func(ctx context.Context) error { return buildIndex(ctx, store) })
	if err != nil {
		return err
	}
	srv = srv.WithEncounters(encounters, encounter.NewBestiaryWithBase(open5eBaseURL()))

	httpSrv := &http.Server{
		Addr:              addr(),
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdown, sc := context.WithTimeout(context.Background(), 10*time.Second)
		defer sc()
		_ = httpSrv.Shutdown(shutdown)
	}()

	embedStatus := "off"
	if embedClient != nil {
		embedStatus = embedClient.Model()
	}
	chatStatus := fmt.Sprintf("%t", chatClient.Configured())
	if fb := chatClient.FallbackModels(); len(fb) > 0 {
		chatStatus += fmt.Sprintf(", falling back to %s", strings.Join(fb, " then "))
	}
	log.Printf("Grimoire listening on %s (chat configured: %s, embeddings: %s)", addr(), chatStatus, embedStatus)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	log.Println("shutdown complete")
	return nil
}
