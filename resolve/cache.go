/* Copyright 2026 The Bazel Authors. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

   http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package resolve

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/label"
	"github.com/bazelbuild/bazel-gazelle/rule"
)

func init() {
	// Pre-register types for gob encoding to improve performance
	gob.Register(&PackageIndexCache{})
	gob.Register(&CachedRuleRecord{})
	gob.Register(label.Label{})
	gob.Register(ImportSpec{})
}

// PackageIndexCache represents the cached index data for a single package.
// Note: All fields must be exported for gob encoding.
type PackageIndexCache struct {
	// Metadata for cache invalidation
	BuildFileHash     string            // SHA-256 of BUILD file content
	SourceFileHashes  map[string]string // filename -> SHA-256
	ConfigFingerprint string            // Hash of indexing-relevant config
	GazelleVersion    string            // For compatibility
	Timestamp         int64             // Unix timestamp (for debugging)

	// Cached rule records
	Records []*CachedRuleRecord
}

// CachedRuleRecord is a serializable version of ruleRecord.
// Note: All fields must be exported for gob encoding.
type CachedRuleRecord struct {
	Kind       string
	Label      label.Label
	Pkg        string
	ImportedAs []ImportSpec
	Embeds     []label.Label
	Lang       string
}

// PackageGenerationCache represents cached rule generation results for a package.
// This allows skipping the entire GenerateRules() + Merge + Resolve phases for unchanged packages.
type PackageGenerationCache struct {
	// Validation metadata
	BuildFileHash     string            // SHA-256 of BUILD file content
	SourceFileHashes  map[string]string // filename -> SHA-256
	SubdirHashes      map[string]string // subdir -> hash of listing
	GenFileHashes     map[string]string // generated files
	ConfigFingerprint string            // Hash of config
	GazelleVersion    string            // Version compatibility
	Timestamp         int64             // For debugging

	// Cached generation results (serialized as BUILD file text)
	GeneratedRulesText string              // BUILD syntax for generated rules
	EmptyRulesText     string              // BUILD syntax for empty rules
	ImportsData        [][]byte            // Serialized imports (opaque, language-specific)
	MappedKinds        []config.MappedKind // Mapped kinds used

	// Cached final BUILD file (after merge and resolve)
	// This allows skipping Merge and Resolve phases entirely
	FinalBuildFile []byte // Final BUILD file content
}

// SerializedRule represents a rule serialized as BUILD file text.
type SerializedRule struct {
	Text string // BUILD file syntax
	Name string // Rule name for identification
	Kind string // Rule kind
}

// DeserializeGeneratedRules parses cached generated rules.
func (c *PackageGenerationCache) DeserializeGeneratedRules(pkg string) ([]*rule.Rule, error) {
	if c.GeneratedRulesText == "" {
		return nil, nil
	}

	f, err := rule.LoadData("BUILD.bazel", pkg, []byte(c.GeneratedRulesText))
	if err != nil {
		return nil, err
	}

	return f.Rules, nil
}

// DeserializeEmptyRules parses cached empty rules.
func (c *PackageGenerationCache) DeserializeEmptyRules(pkg string) ([]*rule.Rule, error) {
	if c.EmptyRulesText == "" {
		return nil, nil
	}

	f, err := rule.LoadData("BUILD.bazel", pkg, []byte(c.EmptyRulesText))
	if err != nil {
		return nil, err
	}

	return f.Rules, nil
}

// DeserializeImports returns the cached imports data as interface{} slices.
func (c *PackageGenerationCache) DeserializeImports() []interface{} {
	result := make([]interface{}, len(c.ImportsData))
	for i, data := range c.ImportsData {
		if len(data) == 0 {
			result[i] = nil
			continue
		}
		// Imports are stored as raw bytes - language extensions will decode them
		// For now, we return them as-is
		result[i] = data
	}
	return result
}

// IndexCacheManager manages the disk cache for rule indexing.
//
// Performance optimization: Uses sync.Map for lock-free concurrent reads.
// During the walk phase, multiple goroutines may try to load cache entries
// simultaneously. sync.Map provides better performance than map+RWMutex for
// read-heavy workloads with occasional writes.
//
// The cache workflow:
// 1. First access: Load from disk, validate, store in sync.Map
// 2. Subsequent accesses: Lock-free reads from sync.Map
// 3. Cache invalidation: Remove stale entries from sync.Map
//
// This optimization is especially beneficial when:
// - Using lazy indexing (only index directories being updated)
// - Large repositories with many packages
// - Concurrent walk operations
type IndexCacheManager struct {
	enabled        bool
	cacheDir       string // e.g., /repo/root/.gazelle-cache/index/
	configHash     string // Fingerprint of current config
	gazelleVersion string // Current gazelle version

	// In-memory cache of loaded package caches (for performance)
	// Using sync.Map for lock-free concurrent reads
	packageCaches sync.Map // map[string]*PackageIndexCache
}

// NewIndexCacheManager creates a new cache manager.
func NewIndexCacheManager(repoRoot, cacheDirName, configHash string) *IndexCacheManager {
	cacheDir := filepath.Join(repoRoot, cacheDirName, "index")
	return &IndexCacheManager{
		enabled:        true,
		cacheDir:       cacheDir,
		configHash:     configHash,
		gazelleVersion: "0.40.0", // TODO: extract from build info
	}
}

// IsEnabled returns whether caching is enabled.
func (cm *IndexCacheManager) IsEnabled() bool {
	return cm != nil && cm.enabled
}

// LoadPackageCache loads the cached index for a package if valid.
// Returns the cached records and a boolean indicating if the cache is valid.
func (cm *IndexCacheManager) LoadPackageCache(
	c *config.Config,
	pkgRel string,
	buildFile *rule.File,
	sourceFiles []string,
) ([]*CachedRuleRecord, bool) {
	if !cm.IsEnabled() {
		return nil, false
	}

	// Check in-memory cache first (lock-free read with sync.Map)
	if value, ok := cm.packageCaches.Load(pkgRel); ok {
		cached := value.(*PackageIndexCache)
		if cm.validateCache(c, cached, buildFile, sourceFiles) {
			return cached.Records, true
		}
		// Cache is stale, remove it
		cm.packageCaches.Delete(pkgRel)
	}

	// Load from disk
	cache, err := cm.loadCacheFromDisk(pkgRel)
	if err != nil {
		return nil, false
	}

	// Validate cache
	if !cm.validateCache(c, cache, buildFile, sourceFiles) {
		return nil, false
	}

	// Store in memory for future use (lock-free with sync.Map)
	cm.packageCaches.Store(pkgRel, cache)

	return cache.Records, true
}

// loadCacheFromDisk loads a cache entry from disk using gob encoding.
func (cm *IndexCacheManager) loadCacheFromDisk(pkgRel string) (*PackageIndexCache, error) {
	cacheFilePath := cm.getCacheFilePath(pkgRel)
	f, err := os.Open(cacheFilePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Use buffered reader for better I/O performance
	reader := bufio.NewReader(f)
	decoder := gob.NewDecoder(reader)

	var cache PackageIndexCache
	if err := decoder.Decode(&cache); err != nil {
		return nil, err
	}

	return &cache, nil
}

// SavePackageCache saves the cached index for a package.
func (cm *IndexCacheManager) SavePackageCache(
	c *config.Config,
	pkgRel string,
	buildFile *rule.File,
	sourceFiles []string,
	records []*CachedRuleRecord,
) error {
	if !cm.IsEnabled() {
		return nil
	}

	// Compute BUILD file hash
	buildFileHash, err := cm.computeFileHash(buildFile.Path)
	if err != nil {
		return err
	}

	// Compute source file hashes
	sourceFileHashes := make(map[string]string)
	for _, sf := range sourceFiles {
		filePath := filepath.Join(filepath.Dir(buildFile.Path), sf)
		hash, err := cm.computeFileHash(filePath)
		if err != nil {
			// Skip files that can't be hashed (e.g., missing)
			continue
		}
		sourceFileHashes[sf] = hash
	}

	cache := &PackageIndexCache{
		BuildFileHash:     buildFileHash,
		SourceFileHashes:  sourceFileHashes,
		ConfigFingerprint: cm.configHash,
		GazelleVersion:    cm.gazelleVersion,
		Timestamp:         time.Now().Unix(),
		Records:           records,
	}

	// Ensure cache directory exists
	if err := os.MkdirAll(cm.cacheDir, 0755); err != nil {
		return err
	}

	// Serialize using gob encoding (much faster than JSON)
	cacheFilePath := cm.getCacheFilePath(pkgRel)
	f, err := os.Create(cacheFilePath)
	if err != nil {
		return err
	}
	defer f.Close()

	// Use buffered writer for better I/O performance
	writer := bufio.NewWriter(f)
	encoder := gob.NewEncoder(writer)

	if err := encoder.Encode(cache); err != nil {
		return err
	}

	return writer.Flush()
}

// validateCache checks if a cached package is still valid.
func (cm *IndexCacheManager) validateCache(
	c *config.Config,
	cache *PackageIndexCache,
	buildFile *rule.File,
	sourceFiles []string,
) bool {
	// Check gazelle version
	if cache.GazelleVersion != cm.gazelleVersion {
		return false
	}

	// Check config fingerprint
	if cache.ConfigFingerprint != cm.configHash {
		return false
	}

	// Check BUILD file hash
	buildFileHash, err := cm.computeFileHash(buildFile.Path)
	if err != nil || buildFileHash != cache.BuildFileHash {
		return false
	}

	// Check source file hashes
	for _, sf := range sourceFiles {
		filePath := filepath.Join(filepath.Dir(buildFile.Path), sf)
		hash, err := cm.computeFileHash(filePath)
		if err != nil {
			return false
		}
		cachedHash, ok := cache.SourceFileHashes[sf]
		if !ok || cachedHash != hash {
			return false
		}
	}

	// Check for removed source files
	if len(cache.SourceFileHashes) != len(sourceFiles) {
		return false
	}

	return true
}

// computeFileHash computes the SHA-256 hash of a file with buffered I/O.
func (cm *IndexCacheManager) computeFileHash(filePath string) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	// Use buffered reader for better I/O performance (32KB buffer)
	reader := bufio.NewReaderSize(f, 32*1024)

	h := sha256.New()
	if _, err := io.Copy(h, reader); err != nil {
		return "", err
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

// getCacheFilePath returns the path to the cache file for a package.
func (cm *IndexCacheManager) getCacheFilePath(pkgRel string) string {
	// Use hash of package path to handle special characters and long paths
	h := sha256.New()
	h.Write([]byte(pkgRel))
	pkgHash := hex.EncodeToString(h.Sum(nil))
	return filepath.Join(cm.cacheDir, pkgHash+".gob")
}

// PreloadCache attempts to load a cache entry into memory.
// This is useful for pre-populating the cache before the walk phase.
// Returns true if the cache was successfully loaded and is valid.
func (cm *IndexCacheManager) PreloadCache(
	c *config.Config,
	pkgRel string,
	buildFilePath string,
	sourceFiles []string,
) bool {
	if !cm.IsEnabled() {
		return false
	}

	// Check if already loaded
	if _, ok := cm.packageCaches.Load(pkgRel); ok {
		return true
	}

	// Load from disk
	cache, err := cm.loadCacheFromDisk(pkgRel)
	if err != nil {
		return false
	}

	// For pre-loading, we do a lightweight validation (just version and config)
	// Full validation with file hashes happens in LoadPackageCache
	if cache.GazelleVersion != cm.gazelleVersion {
		return false
	}
	if cache.ConfigFingerprint != cm.configHash {
		return false
	}

	// Store in memory
	cm.packageCaches.Store(pkgRel, cache)
	return true
}

// GetCacheStats returns statistics about the in-memory cache.
func (cm *IndexCacheManager) GetCacheStats() (loaded int) {
	cm.packageCaches.Range(func(key, value interface{}) bool {
		loaded++
		return true
	})
	return loaded
}

// ClearInMemoryCache clears all in-memory cached entries.
// This is useful for testing or forcing a fresh load.
func (cm *IndexCacheManager) ClearInMemoryCache() {
	cm.packageCaches = sync.Map{}
}

// SaveGenerationCache saves the rule generation results to cache.
func (cm *IndexCacheManager) SaveGenerationCache(
	c *config.Config,
	pkgRel string,
	buildFilePath string,
	sourceFiles, subdirs, genFiles []string,
	generatedRules, emptyRules []*rule.Rule,
	importsData []interface{},
	mappedKinds []config.MappedKind,
) error {
	if !cm.IsEnabled() {
		return nil
	}

	// Compute BUILD file hash
	buildFileHash, err := cm.computeFileHash(buildFilePath)
	if err != nil {
		return err
	}

	// Compute source file hashes
	sourceFileHashes := make(map[string]string)
	for _, sf := range sourceFiles {
		filePath := filepath.Join(filepath.Dir(buildFilePath), sf)
		hash, err := cm.computeFileHash(filePath)
		if err != nil {
			continue // Skip files that can't be hashed
		}
		sourceFileHashes[sf] = hash
	}

	// Compute subdir hashes (recursive hash to detect changes in testdata/** etc.)
	subdirHashes := make(map[string]string)
	for _, subdir := range subdirs {
		subdirPath := filepath.Join(filepath.Dir(buildFilePath), subdir)
		// Hash recursively up to 3 levels deep to catch glob patterns like testdata/**
		hash, err := cm.computeDirectoryHashRecursive(subdirPath, 3)
		if err != nil {
			continue
		}
		subdirHashes[subdir] = hash
	}

	// Compute generated file hashes
	genFileHashes := make(map[string]string)
	for _, gf := range genFiles {
		filePath := filepath.Join(filepath.Dir(buildFilePath), gf)
		hash, err := cm.computeFileHash(filePath)
		if err != nil {
			continue
		}
		genFileHashes[gf] = hash
	}

	// Serialize rules as BUILD file text
	generatedText := cm.serializeRules(generatedRules)
	emptyText := cm.serializeRules(emptyRules)

	// Serialize imports data using gob
	var serializedImports [][]byte
	for _, imp := range importsData {
		if imp == nil {
			serializedImports = append(serializedImports, nil)
			continue
		}
		var buf []byte
		buf, err := cm.serializeImports(imp)
		if err != nil {
			// If we can't serialize imports, skip this cache entry
			return err
		}
		serializedImports = append(serializedImports, buf)
	}

	cache := &PackageGenerationCache{
		BuildFileHash:      buildFileHash,
		SourceFileHashes:   sourceFileHashes,
		SubdirHashes:       subdirHashes,
		GenFileHashes:      genFileHashes,
		ConfigFingerprint:  cm.configHash,
		GazelleVersion:     cm.gazelleVersion,
		Timestamp:          time.Now().Unix(),
		GeneratedRulesText: generatedText,
		EmptyRulesText:     emptyText,
		ImportsData:        serializedImports,
		MappedKinds:        mappedKinds,
	}

	// Write to disk
	cacheFilePath := cm.getGenerationCacheFilePath(pkgRel)
	if err := os.MkdirAll(filepath.Dir(cacheFilePath), 0755); err != nil {
		return err
	}

	f, err := os.Create(cacheFilePath)
	if err != nil {
		return err
	}
	defer f.Close()

	writer := bufio.NewWriter(f)
	encoder := gob.NewEncoder(writer)

	if err := encoder.Encode(cache); err != nil {
		return err
	}

	return writer.Flush()
}

// LoadGenerationCache loads cached rule generation results.
func (cm *IndexCacheManager) LoadGenerationCache(
	c *config.Config,
	pkgRel string,
	buildFilePath string,
	sourceFiles, subdirs, genFiles []string,
) (*PackageGenerationCache, bool) {
	if !cm.IsEnabled() {
		return nil, false
	}

	// Load from disk
	cacheFilePath := cm.getGenerationCacheFilePath(pkgRel)
	f, err := os.Open(cacheFilePath)
	if err != nil {
		return nil, false
	}
	defer f.Close()

	reader := bufio.NewReader(f)
	decoder := gob.NewDecoder(reader)

	var cache PackageGenerationCache
	if err := decoder.Decode(&cache); err != nil {
		return nil, false
	}

	// Validate cache
	if !cm.validateGenerationCache(c, &cache, buildFilePath, sourceFiles, subdirs, genFiles) {
		return nil, false
	}

	return &cache, true
}

// validateGenerationCache checks if a generation cache is still valid.
func (cm *IndexCacheManager) validateGenerationCache(
	c *config.Config,
	cache *PackageGenerationCache,
	buildFilePath string,
	sourceFiles, subdirs, genFiles []string,
) bool {
	// Check version
	if cache.GazelleVersion != cm.gazelleVersion {
		return false
	}

	// Check config fingerprint
	if cache.ConfigFingerprint != cm.configHash {
		return false
	}

	// Check BUILD file hash
	buildFileHash, err := cm.computeFileHash(buildFilePath)
	if err != nil || buildFileHash != cache.BuildFileHash {
		return false
	}

	// Check source file hashes
	if len(cache.SourceFileHashes) != len(sourceFiles) {
		return false
	}
	for _, sf := range sourceFiles {
		filePath := filepath.Join(filepath.Dir(buildFilePath), sf)
		hash, err := cm.computeFileHash(filePath)
		if err != nil {
			return false
		}
		cachedHash, ok := cache.SourceFileHashes[sf]
		if !ok || cachedHash != hash {
			return false
		}
	}

	// Check subdir hashes (recursive)
	if len(cache.SubdirHashes) != len(subdirs) {
		return false
	}
	for _, subdir := range subdirs {
		subdirPath := filepath.Join(filepath.Dir(buildFilePath), subdir)
		// Hash recursively up to 3 levels deep (same as when saving)
		hash, err := cm.computeDirectoryHashRecursive(subdirPath, 3)
		if err != nil {
			return false
		}
		cachedHash, ok := cache.SubdirHashes[subdir]
		if !ok || cachedHash != hash {
			return false
		}
	}

	// Check generated file hashes
	if len(cache.GenFileHashes) != len(genFiles) {
		return false
	}
	for _, gf := range genFiles {
		filePath := filepath.Join(filepath.Dir(buildFilePath), gf)
		hash, err := cm.computeFileHash(filePath)
		if err != nil {
			return false
		}
		cachedHash, ok := cache.GenFileHashes[gf]
		if !ok || cachedHash != hash {
			return false
		}
	}

	return true
}

// serializeRules converts rules to BUILD file text.
func (cm *IndexCacheManager) serializeRules(rules []*rule.Rule) string {
	if len(rules) == 0 {
		return ""
	}

	// Create a temporary file to hold the rules
	f := rule.EmptyFile("", "")
	for _, r := range rules {
		r.Insert(f)
	}

	return string(f.Format())
}

// deserializeRules parses BUILD file text back into rules.
func (cm *IndexCacheManager) deserializeRules(text, pkg string) ([]*rule.Rule, error) {
	if text == "" {
		return nil, nil
	}

	// Parse the BUILD file text
	f, err := rule.LoadData("BUILD.bazel", pkg, []byte(text))
	if err != nil {
		return nil, err
	}

	return f.Rules, nil
}

// serializeImports serializes the imports data using gob.
func (cm *IndexCacheManager) serializeImports(imports interface{}) ([]byte, error) {
	var buf []byte
	// Use a bytes.Buffer with gob encoder
	writer := new(bytes.Buffer)
	encoder := gob.NewEncoder(writer)
	if err := encoder.Encode(imports); err != nil {
		return nil, err
	}
	buf = writer.Bytes()
	return buf, nil
}

// deserializeImports deserializes imports data.
func (cm *IndexCacheManager) deserializeImports(data []byte, target interface{}) error {
	if len(data) == 0 {
		return nil
	}
	reader := bytes.NewReader(data)
	decoder := gob.NewDecoder(reader)
	return decoder.Decode(target)
}

// computeDirectoryListingHash computes a hash of directory entries recursively.
// This ensures that changes to files deep in subdirectories (like testdata/) are detected.
func (cm *IndexCacheManager) computeDirectoryListingHash(dirPath string) (string, error) {
	return cm.computeDirectoryHashRecursive(dirPath, 0)
}

// computeDirectoryHashRecursive recursively hashes a directory tree.
// maxDepth limits recursion (0 = current dir only, -1 = unlimited)
func (cm *IndexCacheManager) computeDirectoryHashRecursive(dirPath string, maxDepth int) (string, error) {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return "", err
	}

	h := sha256.New()
	for _, entry := range entries {
		// Hash entry name and type
		h.Write([]byte(entry.Name()))
		h.Write([]byte{0})

		if entry.IsDir() {
			h.Write([]byte("d"))
			h.Write([]byte{0})

			// Recursively hash subdirectories (up to 3 levels deep to catch testdata/**/*)
			if maxDepth != 0 {
				subPath := filepath.Join(dirPath, entry.Name())
				nextDepth := maxDepth - 1
				if maxDepth < 0 {
					nextDepth = -1
				}
				subHash, err := cm.computeDirectoryHashRecursive(subPath, nextDepth)
				if err != nil {
					// Skip directories we can't read
					continue
				}
				h.Write([]byte(subHash))
				h.Write([]byte{0})
			}
		} else {
			h.Write([]byte("f"))
			h.Write([]byte{0})

			// Hash file modification time and size for efficiency
			info, err := entry.Info()
			if err == nil {
				fmt.Fprintf(h, "%d:%d", info.ModTime().Unix(), info.Size())
				h.Write([]byte{0})
			}
		}
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

// UpdateGenerationCacheWithFinalFile updates the cache with the final BUILD file.
// This allows skipping Merge and Resolve on subsequent runs.
func (cm *IndexCacheManager) UpdateGenerationCacheWithFinalFile(
	pkgRel string,
	finalBuildFile []byte,
) error {
	if !cm.IsEnabled() {
		return nil
	}

	// Load existing cache
	cacheFilePath := cm.getGenerationCacheFilePath(pkgRel)
	f, err := os.Open(cacheFilePath)
	if err != nil {
		return err // Cache doesn't exist yet
	}

	reader := bufio.NewReader(f)
	decoder := gob.NewDecoder(reader)

	var cache PackageGenerationCache
	if err := decoder.Decode(&cache); err != nil {
		f.Close()
		return err
	}
	f.Close()

	// Update with final file
	cache.FinalBuildFile = finalBuildFile

	// Write back
	outFile, err := os.Create(cacheFilePath)
	if err != nil {
		return err
	}
	defer outFile.Close()

	writer := bufio.NewWriter(outFile)
	encoder := gob.NewEncoder(writer)

	if err := encoder.Encode(&cache); err != nil {
		return err
	}

	return writer.Flush()
}

// getGenerationCacheFilePath returns the path to the generation cache file.
func (cm *IndexCacheManager) getGenerationCacheFilePath(pkgRel string) string {
	h := sha256.New()
	h.Write([]byte("gen:"))
	h.Write([]byte(pkgRel))
	pkgHash := hex.EncodeToString(h.Sum(nil))
	return filepath.Join(cm.cacheDir, "gen", pkgHash+".gob")
}

// ComputeConfigFingerprint computes a fingerprint of indexing-relevant config.
func ComputeConfigFingerprint(c *config.Config) string {
	h := sha256.New()

	// Include repo name
	h.Write([]byte(c.RepoName))
	h.Write([]byte{0})

	// Include language filter
	for _, lang := range c.Langs {
		h.Write([]byte(lang))
		h.Write([]byte{0})
	}

	// Include kind map keys (rule type mappings affect indexing)
	for fromKind, mappedKind := range c.KindMap {
		h.Write([]byte(fromKind))
		h.Write([]byte(mappedKind.KindName))
		h.Write([]byte{0})
	}

	// Include alias map keys
	for alias, underlying := range c.AliasMap {
		h.Write([]byte(alias))
		h.Write([]byte(underlying))
		h.Write([]byte{0})
	}

	return fmt.Sprintf("%x", h.Sum(nil))
}
