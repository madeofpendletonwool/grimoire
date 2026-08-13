package data

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

// FetchOptions controls which corpora are fetched and from where. The
// corpus-specific override fields are read by the matching corpus Fetcher.
type FetchOptions struct {
	MTGURL  string // override MTG comp rules URL
	DNDRepo string // override D&D SRD repo "owner/name"
	DNDRef  string // override D&D SRD git ref
	// Include restricts the build to a subset of registered corpora. A nil/empty
	// map means "all registered corpora", so adding a corpus by registering it
	// is enough for it to be indexed — no literal to update.
	Include map[Corpus]bool
}

// DefaultFetchOptions enables the registered corpora with canonical sources.
func DefaultFetchOptions() FetchOptions {
	return FetchOptions{
		MTGURL:  mtgDefaultURL,
		DNDRepo: dndDefaultRepo,
		DNDRef:  dndDefaultRef,
	}
}

// include reports whether a corpus should be built. An empty Include set means
// "all registered corpora"; otherwise only the listed corpora are included.
func (o FetchOptions) include(c Corpus) bool {
	if len(o.Include) == 0 {
		return true
	}
	return o.Include[c]
}

// BuildDataset fetches and parses the registered corpora and merges them. It is
// driven entirely by the registry: each Definition's Fetcher builds its own
// Dataset, so adding a corpus is a Register call, not an edit here.
func BuildDataset(ctx context.Context, opts FetchOptions) (*Dataset, error) {
	merged := &Dataset{Meta: map[Corpus]CorpusMeta{}}
	for _, d := range Registered() {
		if d.Fetcher == nil || !opts.include(d.Corpus) {
			continue
		}
		ds, err := d.Fetcher(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", d.Corpus, err)
		}
		merge(merged, ds)
	}
	return merged, nil
}

// fetchMTGDataset adapts the MTG parser to the registry Fetcher signature.
func fetchMTGDataset(ctx context.Context, opts FetchOptions) (*Dataset, error) {
	return fetchMTG(ctx, opts.MTGURL)
}

// fetchDNDDataset adapts the D&D parser to the registry Fetcher signature.
func fetchDNDDataset(ctx context.Context, opts FetchOptions) (*Dataset, error) {
	return fetchDND(ctx, opts.DNDRepo, opts.DNDRef)
}

func fetchMTG(ctx context.Context, url string) (*Dataset, error) {
	if url == "" {
		url = mtgDefaultURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch mtg rules: %s", resp.Status)
	}
	return ParseMTG(resp.Body)
}

func fetchDND(ctx context.Context, repo, ref string) (*Dataset, error) {
	_ = ctx
	gz, err := FetchDND(repo, ref)
	if err != nil {
		return nil, err
	}
	files, err := ExtractDNDMarkdown(gz)
	if err != nil {
		return nil, err
	}
	if ref == "" {
		ref = dndDefaultRef
	}
	return ParseDND(files, ref), nil
}

func merge(dst, src *Dataset) {
	dst.Records = append(dst.Records, src.Records...)
	for k, v := range src.Meta {
		dst.Meta[k] = v
	}
}

// ParseMTGReader is a convenience for tests that already have the text.
func ParseMTGReader(r io.Reader) (*Dataset, error) { return ParseMTG(r) }
