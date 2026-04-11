package vfs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Parser describes how a file should be parsed into a serializable model.
//
// Implementations should ensure that Encode/Decode are stable across process
// boundaries for a fixed Version, since encoded models may be persisted.
type Parser interface {
	Key() string
	Version() string
	Parse(path string, data []byte) (any, error)
	Encode(model any) ([]byte, error)
	Decode(data []byte) (any, error)
}

// LookupResult describes the result of a cache-backed parse.
type LookupResult struct {
	Model       any
	ContentHash string
	ModelHash   string
	CacheHit    bool
}

type cacheKey struct {
	Path      string
	ParserKey string
}

// Entry is the serialized form stored by the parsed-model cache.
type Entry struct {
	Path          string `json:"path"`
	ParserKey     string `json:"parserKey"`
	ParserVersion string `json:"parserVersion"`
	ContentHash   string `json:"contentHash"`
	ModelHash     string `json:"modelHash"`
	EncodedModel  []byte `json:"encodedModel"`
}

// Persisted is the on-disk representation for a parsed-model cache.
type Persisted struct {
	Entries []Entry `json:"entries"`
}

// CacheBuilder is the mutable single-writer form of the parsed-model cache.
//
// The intended ownership model is:
//  1. worker goroutines parse and send results to a coordinator
//  2. the coordinator alone calls Parse and mutates the builder
//  3. Freeze is called when the build phase ends, transferring ownership of
//     the entries into an immutable Cache
type CacheBuilder struct {
	entries map[cacheKey]Entry
}

// Cache is the frozen read-only form of the parsed-model cache.
type Cache struct {
	entries map[cacheKey]Entry
}

func NewCacheBuilder(seed *Cache) *CacheBuilder {
	builder := &CacheBuilder{
		entries: make(map[cacheKey]Entry),
	}
	if seed == nil {
		return builder
	}
	for key, entry := range seed.entries {
		builder.entries[key] = cloneEntry(entry)
	}
	return builder
}

// Parse returns a parsed model for path and parser, reusing a cached encoded
// model when the content hash and parser version match.
//
// This method is intended to be called only by the single build-phase owner.
func (b *CacheBuilder) Parse(path string, data []byte, parser Parser) (LookupResult, error) {
	if parser == nil {
		return LookupResult{}, fmt.Errorf("nil parser")
	}
	if parser.Key() == "" {
		return LookupResult{}, fmt.Errorf("parser key must not be empty")
	}
	if parser.Version() == "" {
		return LookupResult{}, fmt.Errorf("parser version must not be empty")
	}

	contentHash := digest(data)
	key := cacheKey{Path: path, ParserKey: parser.Key()}

	if entry, ok := b.entries[key]; ok && entry.ParserVersion == parser.Version() && entry.ContentHash == contentHash {
		model, err := parser.Decode(entry.EncodedModel)
		if err != nil {
			return LookupResult{}, fmt.Errorf("decode cached model for %s with parser %s: %w", path, parser.Key(), err)
		}
		return LookupResult{
			Model:       model,
			ContentHash: entry.ContentHash,
			ModelHash:   entry.ModelHash,
			CacheHit:    true,
		}, nil
	}

	model, err := parser.Parse(path, data)
	if err != nil {
		return LookupResult{}, err
	}
	encoded, err := parser.Encode(model)
	if err != nil {
		return LookupResult{}, fmt.Errorf("encode parsed model for %s with parser %s: %w", path, parser.Key(), err)
	}

	result := LookupResult{
		Model:       model,
		ContentHash: contentHash,
		ModelHash:   digest(encoded),
		CacheHit:    false,
	}
	b.entries[key] = Entry{
		Path:          path,
		ParserKey:     parser.Key(),
		ParserVersion: parser.Version(),
		ContentHash:   result.ContentHash,
		ModelHash:     result.ModelHash,
		EncodedModel:  append([]byte(nil), encoded...),
	}
	return result, nil
}

// Check reports whether an entry can be reused without mutating the builder.
// This is intended for the coordinator to decide whether worker parsing is
// needed before dispatching jobs.
func (b *CacheBuilder) Check(path string, data []byte, parser Parser) (LookupResult, bool, error) {
	if parser == nil {
		return LookupResult{}, false, fmt.Errorf("nil parser")
	}
	if parser.Key() == "" {
		return LookupResult{}, false, fmt.Errorf("parser key must not be empty")
	}
	if parser.Version() == "" {
		return LookupResult{}, false, fmt.Errorf("parser version must not be empty")
	}

	contentHash := digest(data)
	key := cacheKey{Path: path, ParserKey: parser.Key()}
	entry, ok := b.entries[key]
	if !ok || entry.ParserVersion != parser.Version() || entry.ContentHash != contentHash {
		return LookupResult{}, false, nil
	}

	model, err := parser.Decode(entry.EncodedModel)
	if err != nil {
		return LookupResult{}, false, fmt.Errorf("decode cached model for %s with parser %s: %w", path, parser.Key(), err)
	}
	return LookupResult{
		Model:       model,
		ContentHash: entry.ContentHash,
		ModelHash:   entry.ModelHash,
		CacheHit:    true,
	}, true, nil
}

func (b *CacheBuilder) CheckHash(path, contentHash string, parser Parser) (LookupResult, bool, error) {
	if parser == nil {
		return LookupResult{}, false, fmt.Errorf("nil parser")
	}
	if parser.Key() == "" {
		return LookupResult{}, false, fmt.Errorf("parser key must not be empty")
	}
	if parser.Version() == "" {
		return LookupResult{}, false, fmt.Errorf("parser version must not be empty")
	}
	key := cacheKey{Path: path, ParserKey: parser.Key()}
	entry, ok := b.entries[key]
	if !ok || entry.ParserVersion != parser.Version() || entry.ContentHash != contentHash {
		return LookupResult{}, false, nil
	}
	model, err := parser.Decode(entry.EncodedModel)
	if err != nil {
		return LookupResult{}, false, fmt.Errorf("decode cached model for %s with parser %s: %w", path, parser.Key(), err)
	}
	return LookupResult{
		Model:       model,
		ContentHash: entry.ContentHash,
		ModelHash:   entry.ModelHash,
		CacheHit:    true,
	}, true, nil
}

// StoreEntry records a parsed entry in the builder. This is intended for the
// single coordinator goroutine after worker parsing completes.
func (b *CacheBuilder) StoreEntry(entry Entry) {
	key := cacheKey{Path: entry.Path, ParserKey: entry.ParserKey}
	b.entries[key] = cloneEntry(entry)
}

func (b *CacheBuilder) DeletePath(path string) {
	if b == nil {
		return
	}
	path = cleanRepoPath(path)
	for key := range b.entries {
		if key.Path == path {
			delete(b.entries, key)
		}
	}
}

func (b *CacheBuilder) DeleteSubtree(prefix string) {
	if b == nil {
		return
	}
	prefix = cleanRepoPath(prefix)
	for key := range b.entries {
		if key.Path == prefix || strings.HasPrefix(key.Path, prefix+"/") {
			delete(b.entries, key)
		}
	}
}

func (b *CacheBuilder) Freeze() *Cache {
	if b == nil {
		return &Cache{entries: make(map[cacheKey]Entry)}
	}
	entries := b.entries
	b.entries = nil
	return &Cache{entries: entries}
}

// Get returns a parsed model from a frozen cache.
//
// The bool result is false when the entry is absent or stale for the supplied
// content hash / parser version. Frozen caches never parse or mutate.
func (c *Cache) Get(path string, data []byte, parser Parser) (LookupResult, bool, error) {
	if parser == nil {
		return LookupResult{}, false, fmt.Errorf("nil parser")
	}
	if parser.Key() == "" {
		return LookupResult{}, false, fmt.Errorf("parser key must not be empty")
	}
	if parser.Version() == "" {
		return LookupResult{}, false, fmt.Errorf("parser version must not be empty")
	}

	contentHash := digest(data)
	key := cacheKey{Path: path, ParserKey: parser.Key()}
	entry, ok := c.entries[key]
	if !ok || entry.ParserVersion != parser.Version() || entry.ContentHash != contentHash {
		return LookupResult{}, false, nil
	}

	model, err := parser.Decode(entry.EncodedModel)
	if err != nil {
		return LookupResult{}, false, fmt.Errorf("decode cached model for %s with parser %s: %w", path, parser.Key(), err)
	}
	return LookupResult{
		Model:       model,
		ContentHash: entry.ContentHash,
		ModelHash:   entry.ModelHash,
		CacheHit:    true,
	}, true, nil
}

func (c *Cache) GetHash(path, contentHash string, parser Parser) (LookupResult, bool, error) {
	if parser == nil {
		return LookupResult{}, false, fmt.Errorf("nil parser")
	}
	if parser.Key() == "" {
		return LookupResult{}, false, fmt.Errorf("parser key must not be empty")
	}
	if parser.Version() == "" {
		return LookupResult{}, false, fmt.Errorf("parser version must not be empty")
	}
	key := cacheKey{Path: path, ParserKey: parser.Key()}
	entry, ok := c.entries[key]
	if !ok || entry.ParserVersion != parser.Version() || entry.ContentHash != contentHash {
		return LookupResult{}, false, nil
	}
	model, err := parser.Decode(entry.EncodedModel)
	if err != nil {
		return LookupResult{}, false, fmt.Errorf("decode cached model for %s with parser %s: %w", path, parser.Key(), err)
	}
	return LookupResult{
		Model:       model,
		ContentHash: entry.ContentHash,
		ModelHash:   entry.ModelHash,
		CacheHit:    true,
	}, true, nil
}

// Snapshot returns a stable persisted view of the cache suitable for
// serialization.
func (c *Cache) Snapshot() Persisted {
	entries := make([]Entry, 0, len(c.entries))
	for _, entry := range c.entries {
		entries = append(entries, cloneEntry(entry))
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Path != entries[j].Path {
			return entries[i].Path < entries[j].Path
		}
		return entries[i].ParserKey < entries[j].ParserKey
	})
	return Persisted{Entries: entries}
}

func (c *Cache) Save(w io.Writer) error {
	return json.NewEncoder(w).Encode(c.Snapshot())
}

func Load(r io.Reader) (*Cache, error) {
	var persisted Persisted
	if err := json.NewDecoder(r).Decode(&persisted); err != nil {
		return nil, err
	}
	entries := make(map[cacheKey]Entry, len(persisted.Entries))
	for _, entry := range persisted.Entries {
		key := cacheKey{Path: entry.Path, ParserKey: entry.ParserKey}
		entries[key] = cloneEntry(entry)
	}
	return &Cache{entries: entries}, nil
}

func cloneEntry(entry Entry) Entry {
	entry.EncodedModel = append([]byte(nil), entry.EncodedModel...)
	return entry
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
