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
// boundaries for a fixed CacheVersion, since encoded models may be persisted.
// Parser authors are responsible for manually bumping CacheVersion whenever
// old cached parser results should be invalidated.
type Parser interface {
	Key() string
	CacheVersion() string
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
	base            *Cache
	entries         map[cacheKey]Entry
	deletedPaths    map[string]struct{}
	deletedSubtrees []string
}

// Cache is the frozen read-only form of the parsed-model cache.
type Cache struct {
	base            *Cache
	future          *cacheFuture
	entries         map[cacheKey]Entry
	deletedPaths    map[string]struct{}
	deletedSubtrees []string
}

type cacheFuture struct {
	done  chan struct{}
	cache *Cache
	err   error
}

// NewCacheBuilder creates a mutable cache builder, optionally seeded from a
// previously frozen cache.
func NewCacheBuilder(seed *Cache) *CacheBuilder {
	builder := &CacheBuilder{
		base:         seed,
		entries:      make(map[cacheKey]Entry),
		deletedPaths: make(map[string]struct{}),
	}
	return builder
}

// Parse returns a parsed model for path and parser, reusing a cached encoded
// model when the content hash and parser cache version match.
//
// This method is intended to be called only by the single build-phase owner.
func (b *CacheBuilder) Parse(path string, data []byte, parser Parser) (LookupResult, error) {
	if parser == nil {
		return LookupResult{}, fmt.Errorf("nil parser")
	}
	if parser.Key() == "" {
		return LookupResult{}, fmt.Errorf("parser key must not be empty")
	}
	if parser.CacheVersion() == "" {
		return LookupResult{}, fmt.Errorf("parser cache version must not be empty")
	}

	contentHash := digest(data)
	key := cacheKey{Path: path, ParserKey: parser.Key()}

	if entry, ok, err := b.lookupEntry(key); err != nil {
		return LookupResult{}, err
	} else if ok && entry.ParserVersion == parser.CacheVersion() && entry.ContentHash == contentHash {
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
		ParserVersion: parser.CacheVersion(),
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
	if parser.CacheVersion() == "" {
		return LookupResult{}, false, fmt.Errorf("parser cache version must not be empty")
	}

	contentHash := digest(data)
	key := cacheKey{Path: path, ParserKey: parser.Key()}
	entry, ok, err := b.lookupEntry(key)
	if err != nil {
		return LookupResult{}, false, err
	}
	if !ok || entry.ParserVersion != parser.CacheVersion() || entry.ContentHash != contentHash {
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

// CheckHash reports whether an entry can be reused for a known content hash
// without reading the file bytes again.
func (b *CacheBuilder) CheckHash(path, contentHash string, parser Parser) (LookupResult, bool, error) {
	if parser == nil {
		return LookupResult{}, false, fmt.Errorf("nil parser")
	}
	if parser.Key() == "" {
		return LookupResult{}, false, fmt.Errorf("parser key must not be empty")
	}
	if parser.CacheVersion() == "" {
		return LookupResult{}, false, fmt.Errorf("parser cache version must not be empty")
	}
	key := cacheKey{Path: path, ParserKey: parser.Key()}
	entry, ok, err := b.lookupEntry(key)
	if err != nil {
		return LookupResult{}, false, err
	}
	if !ok || entry.ParserVersion != parser.CacheVersion() || entry.ContentHash != contentHash {
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
	delete(b.deletedPaths, entry.Path)
	b.entries[key] = cloneEntry(entry)
}

// DeletePath removes all cached parser entries for path.
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
	b.deletedPaths[path] = struct{}{}
}

// DeleteSubtree removes all cached parser entries under prefix.
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
	b.deletedSubtrees = append(b.deletedSubtrees, prefix)
}

// Freeze consumes the builder and returns an immutable cache.
func (b *CacheBuilder) Freeze() *Cache {
	if b == nil {
		return &Cache{entries: make(map[cacheKey]Entry)}
	}
	entries := b.entries
	deletedPaths := b.deletedPaths
	deletedSubtrees := append([]string(nil), b.deletedSubtrees...)
	b.entries = nil
	b.deletedPaths = nil
	b.deletedSubtrees = nil
	return &Cache{
		base:            b.base,
		entries:         entries,
		deletedPaths:    deletedPaths,
		deletedSubtrees: deletedSubtrees,
	}
}

// Get returns a parsed model from a frozen cache.
//
// The bool result is false when the entry is absent or stale for the supplied
// content hash / parser cache version. Frozen caches never parse or mutate.
func (c *Cache) Get(path string, data []byte, parser Parser) (LookupResult, bool, error) {
	if parser == nil {
		return LookupResult{}, false, fmt.Errorf("nil parser")
	}
	if parser.Key() == "" {
		return LookupResult{}, false, fmt.Errorf("parser key must not be empty")
	}
	if parser.CacheVersion() == "" {
		return LookupResult{}, false, fmt.Errorf("parser cache version must not be empty")
	}

	contentHash := digest(data)
	key := cacheKey{Path: path, ParserKey: parser.Key()}
	entry, ok, err := c.lookupEntry(key)
	if err != nil {
		return LookupResult{}, false, err
	}
	if !ok || entry.ParserVersion != parser.CacheVersion() || entry.ContentHash != contentHash {
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

// GetHash looks up a frozen cache entry using a known content hash.
func (c *Cache) GetHash(path, contentHash string, parser Parser) (LookupResult, bool, error) {
	if parser == nil {
		return LookupResult{}, false, fmt.Errorf("nil parser")
	}
	if parser.Key() == "" {
		return LookupResult{}, false, fmt.Errorf("parser key must not be empty")
	}
	if parser.CacheVersion() == "" {
		return LookupResult{}, false, fmt.Errorf("parser cache version must not be empty")
	}
	key := cacheKey{Path: path, ParserKey: parser.Key()}
	entry, ok, err := c.lookupEntry(key)
	if err != nil {
		return LookupResult{}, false, err
	}
	if !ok || entry.ParserVersion != parser.CacheVersion() || entry.ContentHash != contentHash {
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
// Snapshot returns the persisted representation of the cache.
func (c *Cache) Snapshot() Persisted {
	persisted, _ := c.snapshotPersisted()
	return persisted
}

func (c *Cache) snapshotPersisted() (Persisted, error) {
	flattened, err := c.flattenEntries()
	if err != nil {
		return Persisted{}, err
	}
	entries := make([]Entry, 0, len(flattened))
	for _, entry := range flattened {
		entries = append(entries, cloneEntry(entry))
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Path != entries[j].Path {
			return entries[i].Path < entries[j].Path
		}
		return entries[i].ParserKey < entries[j].ParserKey
	})
	return Persisted{Entries: entries}, nil
}

func (b *CacheBuilder) lookupEntry(key cacheKey) (Entry, bool, error) {
	if b == nil {
		return Entry{}, false, nil
	}
	if entry, ok := b.entries[key]; ok {
		return cloneEntry(entry), true, nil
	}
	if b.isDeleted(key.Path) {
		return Entry{}, false, nil
	}
	if b.base == nil {
		return Entry{}, false, nil
	}
	return b.base.lookupEntry(key)
}

func (b *CacheBuilder) isDeleted(path string) bool {
	if _, ok := b.deletedPaths[path]; ok {
		return true
	}
	for _, prefix := range b.deletedSubtrees {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}

func (c *Cache) lookupEntry(key cacheKey) (Entry, bool, error) {
	if c == nil {
		return Entry{}, false, nil
	}
	if entry, ok := c.entries[key]; ok {
		return cloneEntry(entry), true, nil
	}
	if c.isDeleted(key.Path) {
		return Entry{}, false, nil
	}
	if c.base != nil {
		return c.base.lookupEntry(key)
	}
	if c.future == nil {
		return Entry{}, false, nil
	}
	base, err := c.future.wait()
	if err != nil {
		return Entry{}, false, err
	}
	if base == nil {
		return Entry{}, false, nil
	}
	return base.lookupEntry(key)
}

func (c *Cache) isDeleted(path string) bool {
	if _, ok := c.deletedPaths[path]; ok {
		return true
	}
	for _, prefix := range c.deletedSubtrees {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}

func (c *Cache) flattenEntries() (map[cacheKey]Entry, error) {
	if c == nil {
		return map[cacheKey]Entry{}, nil
	}
	entries := make(map[cacheKey]Entry)
	if c.base != nil {
		baseEntries, err := c.base.flattenEntries()
		if err != nil {
			return nil, err
		}
		for key, entry := range baseEntries {
			entries[key] = cloneEntry(entry)
		}
	} else if c.future != nil {
		base, err := c.future.wait()
		if err != nil {
			return nil, err
		}
		if base != nil {
			baseEntries, err := base.flattenEntries()
			if err != nil {
				return nil, err
			}
			for key, entry := range baseEntries {
				entries[key] = cloneEntry(entry)
			}
		}
	}
	for key := range entries {
		if c.isDeleted(key.Path) {
			delete(entries, key)
		}
	}
	for key, entry := range c.entries {
		entries[key] = cloneEntry(entry)
	}
	return entries, nil
}

func newPendingCache(fn func() (*Cache, error)) *Cache {
	future := &cacheFuture{done: make(chan struct{})}
	go func() {
		future.cache, future.err = fn()
		close(future.done)
	}()
	return &Cache{
		future:       future,
		entries:      make(map[cacheKey]Entry),
		deletedPaths: make(map[string]struct{}),
	}
}

func (f *cacheFuture) wait() (*Cache, error) {
	if f == nil {
		return nil, nil
	}
	<-f.done
	return f.cache, f.err
}

func (c *Cache) Save(w io.Writer) error {
	return json.NewEncoder(w).Encode(c.Snapshot())
}

// Load decodes a persisted parsed-model cache.
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
