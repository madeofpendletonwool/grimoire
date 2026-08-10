package data

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

// FetchOptions controls which corpora are fetched and from where.
type FetchOptions struct {
	MTGURL  string // override MTG comp rules URL
	DNDRepo string // override D&D SRD repo "owner/name"
	DNDRef  string // override D&D SRD git ref
	Include map[Corpus]bool
}

// DefaultFetchOptions enables both corpora with canonical sources.
func DefaultFetchOptions() FetchOptions {
	return FetchOptions{
		MTGURL:  mtgDefaultURL,
		DNDRepo: dndDefaultRepo,
		DNDRef:  dndDefaultRef,
		Include: map[Corpus]bool{CorpusMTG: true, CorpusDND: true},
	}
}

// BuildDataset fetches and parses the requested corpora and merges them.
func BuildDataset(ctx context.Context, opts FetchOptions) (*Dataset, error) {
	merged := &Dataset{Meta: map[Corpus]CorpusMeta{}}

	if opts.Include[CorpusMTG] {
		ds, err := fetchMTG(ctx, opts.MTGURL)
		if err != nil {
			return nil, fmt.Errorf("mtg: %w", err)
		}
		merge(merged, ds)
	}
	if opts.Include[CorpusDND] {
		ds, err := fetchDND(ctx, opts.DNDRepo, opts.DNDRef)
		if err != nil {
			return nil, fmt.Errorf("dnd: %w", err)
		}
		merge(merged, ds)
	}
	return merged, nil
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
