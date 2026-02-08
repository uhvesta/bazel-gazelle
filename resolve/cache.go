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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

// PackageIndexCache represents the cached index data for a single package.
type PackageIndexCache struct {
	// Metadata for cache invalidation
	BuildFileHash     string            `json:"build_file_hash"`      // SHA-256 of BUILD file content
	SourceFileHashes  map[string]string `json:"source_file_hashes"`   // filename -> SHA-256
	ConfigFingerprint string            `json:"config_fingerprint"`   // Hash of indexing-relevant config
	GazelleVersion    string            `json:"gazelle_version"`      // For compatibility
	Timestamp         int64             `json:"timestamp"`            // Unix timestamp (for debugging)

	// Cached rule records
	Records []*CachedRuleRecord `json:"records"`
}

// CachedRuleRecord is a serializable version of ruleRecord.
type CachedRuleRecord struct {
	Kind       string        `json:"kind"`
	Label      label.Label   `json:"label"`
	Pkg        string        `json:"pkg"`
	ImportedAs []ImportSpec  `json:"imported_as"`
	Embeds     []label.Label `json:"embeds"`
	Lang       string        `json:"lang"`
}

// IndexCacheManager manages the disk cache for rule indexing.
type IndexCacheManager struct {
	enabled        bool
	cacheDir       string // e.g., /repo/root/.gazelle-cache/index/
	configHash     string // Fingerprint of current config
	gazelleVersion string // Current gazelle version

	// In-memory cache of loaded package caches (for performance)
	packageCaches map[string]*PackageIndexCache
	mu            sync.RWMutex
}

// NewIndexCacheManager creates a new cache manager.
func NewIndexCacheManager(repoRoot, cacheDirName, configHash string) *IndexCacheManager {
	cacheDir := filepath.Join(repoRoot, cacheDirName, "index")
	return &IndexCacheManager{
		enabled:        true,
		cacheDir:       cacheDir,
		configHash:     configHash,
		gazelleVersion: "0.40.0", // TODO: extract from build info
		packageCaches:  make(map[string]*PackageIndexCache),
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

	// Check in-memory cache first
	cm.mu.RLock()
	cached, ok := cm.packageCaches[pkgRel]
	cm.mu.RUnlock()
	if ok && cm.validateCache(c, cached, buildFile, sourceFiles) {
		return cached.Records, true
	}

	// Load from disk
	cacheFilePath := cm.getCacheFilePath(pkgRel)
	data, err := os.ReadFile(cacheFilePath)
	if err != nil {
		return nil, false
	}

	var cache PackageIndexCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return nil, false
	}

	// Validate cache
	if !cm.validateCache(c, &cache, buildFile, sourceFiles) {
		return nil, false
	}

	// Store in memory for future use
	cm.mu.Lock()
	cm.packageCaches[pkgRel] = &cache
	cm.mu.Unlock()

	return cache.Records, true
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

	// Serialize to JSON
	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return err
	}

	// Ensure cache directory exists
	if err := os.MkdirAll(cm.cacheDir, 0755); err != nil {
		return err
	}

	// Write cache file
	cacheFilePath := cm.getCacheFilePath(pkgRel)
	return os.WriteFile(cacheFilePath, data, 0644)
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

// computeFileHash computes the SHA-256 hash of a file.
func (cm *IndexCacheManager) computeFileHash(filePath string) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
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
	return filepath.Join(cm.cacheDir, pkgHash+".json")
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
