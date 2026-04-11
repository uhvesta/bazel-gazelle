package vfs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"sync"
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

// Cache stores parsed models keyed by path and parser identity.
//
// The cache stores only encoded parsed models and hashes, not raw file content.
// A parser is called only when the file content hash or parser version changes,
// or when the entry was not already present.
type Cache struct {
	mu      sync.RWMutex
	entries map[cacheKey]Entry
}

type cacheKey struct {
	Path      string
	ParserKey string
}

// Entry is the serialized form stored by Cache.
type Entry struct {
	Path          string `json:"path"`
	ParserKey     string `json:"parserKey"`
	ParserVersion string `json:"parserVersion"`
	ContentHash   string `json:"contentHash"`
	ModelHash     string `json:"modelHash"`
	EncodedModel  []byte `json:"encodedModel"`
}

// Persisted is the on-disk representation for a Cache.
type Persisted struct {
	Entries []Entry `json:"entries"`
}

func NewCache() *Cache {
	return &Cache{
		entries: make(map[cacheKey]Entry),
	}
}

// Lookup returns a parsed model for path and parser, reusing a cached encoded
// model when the content hash and parser version match.
func (c *Cache) Lookup(path string, data []byte, parser Parser) (LookupResult, error) {
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

	c.mu.RLock()
	entry, ok := c.entries[key]
	c.mu.RUnlock()
	if ok && entry.ParserVersion == parser.Version() && entry.ContentHash == contentHash {
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

	c.mu.Lock()
	c.entries[key] = Entry{
		Path:          path,
		ParserKey:     parser.Key(),
		ParserVersion: parser.Version(),
		ContentHash:   result.ContentHash,
		ModelHash:     result.ModelHash,
		EncodedModel:  append([]byte(nil), encoded...),
	}
	c.mu.Unlock()

	return result, nil
}

// Snapshot returns a stable persisted view of the cache suitable for
// serialization.
func (c *Cache) Snapshot() Persisted {
	c.mu.RLock()
	defer c.mu.RUnlock()

	entries := make([]Entry, 0, len(c.entries))
	for _, entry := range c.entries {
		clone := entry
		clone.EncodedModel = append([]byte(nil), entry.EncodedModel...)
		entries = append(entries, clone)
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
	cache := NewCache()
	for _, entry := range persisted.Entries {
		key := cacheKey{Path: entry.Path, ParserKey: entry.ParserKey}
		cache.entries[key] = Entry{
			Path:          entry.Path,
			ParserKey:     entry.ParserKey,
			ParserVersion: entry.ParserVersion,
			ContentHash:   entry.ContentHash,
			ModelHash:     entry.ModelHash,
			EncodedModel:  append([]byte(nil), entry.EncodedModel...),
		}
	}
	return cache, nil
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
