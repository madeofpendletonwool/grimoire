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
//	ANTHROPIC_BASE_URL  LLM endpoint (default https://api.anthropic.com; z.ai: https://api.z.ai/api/anthropic)
//	ANTHROPIC_API_KEY   LLM secret key (enables the Q&A chat)
//	ANTHROPIC_MODEL     model name (e.g. glm-4.6, claude-3-5-sonnet-20241022)
//	EMBEDDINGS_BASE_URL OpenAI-compatible embeddings endpoint (default https://api.openai.com/v1)
//	EMBEDDINGS_API_KEY  embeddings secret key (enables semantic retrieval when set with EMBEDDINGS_MODEL)
//	EMBEDDINGS_MODEL    embeddings model name (e.g. text-embedding-3-small)
//	SCRYFALL_BASE_URL   MTG card lookup endpoint (default https://api.scryfall.com; no key needed)
//	MTG_RULES_URL       override MTG comp rules source
//	MTGJSON_URL         override MTGJSON AtomicCards source (card-name dictionary)
//	DND_REPO            override D&D SRD repo "owner/name"
//	DND_REF             override D&D SRD git ref
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
	"github.com/madeofpendletonwool/grimoire/internal/cards"
	"github.com/madeofpendletonwool/grimoire/internal/chat"
	"github.com/madeofpendletonwool/grimoire/internal/data"
	"github.com/madeofpendletonwool/grimoire/internal/embeddings"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
	"github.com/madeofpendletonwool/grimoire/internal/server"
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
  ANTHROPIC_BASE_URL, ANTHROPIC_API_KEY, ANTHROPIC_MODEL,
  EMBEDDINGS_BASE_URL, EMBEDDINGS_API_KEY, EMBEDDINGS_MODEL,
  SCRYFALL_BASE_URL, MTG_RULES_URL, MTGJSON_URL, DND_REPO, DND_REF`)
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

func llmConfig() llm.Config {
	return llm.Config{
		BaseURL: env("ANTHROPIC_BASE_URL", llm.DefaultBaseURL),
		APIKey:  os.Getenv("ANTHROPIC_API_KEY"),
		Model:   env("ANTHROPIC_MODEL", "glm-4.6"),
	}
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

// mtgjsonURL returns the AtomicCards endpoint used to build the card-name
// dictionary.
func mtgjsonURL() string { return env("MTGJSON_URL", data.DefaultMTGJSONURL) }

func fetchOpts() data.FetchOptions {
	return data.FetchOptions{
		MTGURL:  os.Getenv("MTG_RULES_URL"),
		DNDRepo: os.Getenv("DND_REPO"),
		DNDRef:  os.Getenv("DND_REF"),
		Include: map[data.Corpus]bool{data.CorpusMTG: true, data.CorpusDND: true},
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

	// Chat history shares the index's SQLite file and connection pool.
	// Rebuilding the rules index does not touch these tables.
	chats, err := chat.New(store.DB())
	if err != nil {
		return err
	}

	// Accounts and sessions share that file too, for the same reason.
	users, err := auth.New(store.DB(), sessionTTL())
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

	srv, err := server.New(store, llm.New(llmConfig()), cardsService(), cardDict, chats,
		server.Auth{Users: users, OpenRegistration: openRegistration()})
	if err != nil {
		return err
	}

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
	log.Printf("Grimoire listening on %s (chat configured: %t, embeddings: %s)", addr(), llm.New(llmConfig()).Configured(), embedStatus)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	log.Println("shutdown complete")
	return nil
}
