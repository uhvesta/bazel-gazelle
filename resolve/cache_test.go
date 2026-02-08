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
	"os"
	"path/filepath"
	"testing"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/label"
	"github.com/bazelbuild/bazel-gazelle/rule"
)

func TestCacheSaveAndLoad(t *testing.T) {
	// Create a temporary directory for cache
	tmpDir := t.TempDir()
	repoRoot := filepath.Join(tmpDir, "repo")
	if err := os.MkdirAll(repoRoot, 0755); err != nil {
		t.Fatal(err)
	}

	// Create a test BUILD file
	buildFilePath := filepath.Join(repoRoot, "pkg", "BUILD.bazel")
	if err := os.MkdirAll(filepath.Dir(buildFilePath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(buildFilePath, []byte("# test build file\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create a test source file
	sourceFilePath := filepath.Join(repoRoot, "pkg", "test.go")
	if err := os.WriteFile(sourceFilePath, []byte("package test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create config
	c := &config.Config{
		RepoRoot:  repoRoot,
		RepoName:  "test_repo",
		IndexCache: true,
		IndexCacheDir: ".gazelle-cache",
	}
	configHash := ComputeConfigFingerprint(c)

	// Create cache manager
	cm := NewIndexCacheManager(repoRoot, ".gazelle-cache", configHash)

	// Create test records
	testRecords := []*CachedRuleRecord{
		{
			Kind:  "go_library",
			Label: label.Label{Repo: "test_repo", Pkg: "pkg", Name: "test"},
			Pkg:   "pkg",
			ImportedAs: []ImportSpec{
				{Lang: "go", Imp: "example.com/test/pkg"},
			},
			Embeds: []label.Label{},
			Lang:   "go",
		},
	}

	// Create a mock build file
	f := &rule.File{
		Path: buildFilePath,
		Pkg:  "pkg",
	}

	// Save to cache
	err := cm.SavePackageCache(c, "pkg", f, []string{"test.go"}, testRecords)
	if err != nil {
		t.Fatalf("SavePackageCache failed: %v", err)
	}

	// Verify cache file was created
	cacheFilePath := cm.getCacheFilePath("pkg")
	if _, err := os.Stat(cacheFilePath); os.IsNotExist(err) {
		t.Fatal("Cache file was not created")
	}

	// Load from cache
	loaded, valid := cm.LoadPackageCache(c, "pkg", f, []string{"test.go"})
	if !valid {
		t.Fatal("Cache should be valid")
	}
	if len(loaded) != len(testRecords) {
		t.Fatalf("Expected %d records, got %d", len(testRecords), len(loaded))
	}
	if loaded[0].Kind != testRecords[0].Kind {
		t.Errorf("Expected kind %s, got %s", testRecords[0].Kind, loaded[0].Kind)
	}
	if loaded[0].Label.String() != testRecords[0].Label.String() {
		t.Errorf("Expected label %s, got %s", testRecords[0].Label, loaded[0].Label)
	}
}

func TestCacheInvalidationOnFileChange(t *testing.T) {
	// Create a temporary directory for cache
	tmpDir := t.TempDir()
	repoRoot := filepath.Join(tmpDir, "repo")
	if err := os.MkdirAll(repoRoot, 0755); err != nil {
		t.Fatal(err)
	}

	// Create a test BUILD file
	buildFilePath := filepath.Join(repoRoot, "pkg", "BUILD.bazel")
	if err := os.MkdirAll(filepath.Dir(buildFilePath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(buildFilePath, []byte("# test build file\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create a test source file
	sourceFilePath := filepath.Join(repoRoot, "pkg", "test.go")
	if err := os.WriteFile(sourceFilePath, []byte("package test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create config
	c := &config.Config{
		RepoRoot:      repoRoot,
		RepoName:      "test_repo",
		IndexCache:    true,
		IndexCacheDir: ".gazelle-cache",
	}
	configHash := ComputeConfigFingerprint(c)

	// Create cache manager
	cm := NewIndexCacheManager(repoRoot, ".gazelle-cache", configHash)

	// Create test records
	testRecords := []*CachedRuleRecord{
		{
			Kind:  "go_library",
			Label: label.Label{Repo: "test_repo", Pkg: "pkg", Name: "test"},
			Pkg:   "pkg",
		},
	}

	// Create a mock build file
	f := &rule.File{
		Path: buildFilePath,
		Pkg:  "pkg",
	}

	// Save to cache
	err := cm.SavePackageCache(c, "pkg", f, []string{"test.go"}, testRecords)
	if err != nil {
		t.Fatalf("SavePackageCache failed: %v", err)
	}

	// Verify cache is valid
	_, valid := cm.LoadPackageCache(c, "pkg", f, []string{"test.go"})
	if !valid {
		t.Fatal("Cache should be valid initially")
	}

	// Modify the source file
	if err := os.WriteFile(sourceFilePath, []byte("package test\n\nfunc Foo() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Cache should now be invalid
	_, valid = cm.LoadPackageCache(c, "pkg", f, []string{"test.go"})
	if valid {
		t.Fatal("Cache should be invalid after source file change")
	}
}

func TestConcurrentCacheAccess(t *testing.T) {
	// Create a temporary directory for cache
	tmpDir := t.TempDir()
	repoRoot := filepath.Join(tmpDir, "repo")
	if err := os.MkdirAll(repoRoot, 0755); err != nil {
		t.Fatal(err)
	}

	// Create test packages
	for i := 0; i < 10; i++ {
		pkgDir := filepath.Join(repoRoot, "pkg", string(rune('a'+i)))
		if err := os.MkdirAll(pkgDir, 0755); err != nil {
			t.Fatal(err)
		}
		buildFilePath := filepath.Join(pkgDir, "BUILD.bazel")
		if err := os.WriteFile(buildFilePath, []byte("# test\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	c := &config.Config{
		RepoRoot:      repoRoot,
		RepoName:      "test_repo",
		IndexCache:    true,
		IndexCacheDir: ".gazelle-cache",
	}
	configHash := ComputeConfigFingerprint(c)
	cm := NewIndexCacheManager(repoRoot, ".gazelle-cache", configHash)

	// Concurrent access test
	done := make(chan bool)
	for i := 0; i < 10; i++ {
		go func(idx int) {
			pkgRel := filepath.Join("pkg", string(rune('a'+idx)))
			buildFilePath := filepath.Join(repoRoot, pkgRel, "BUILD.bazel")
			f := &rule.File{Path: buildFilePath, Pkg: pkgRel}

			// Try to load (will miss)
			_, _ = cm.LoadPackageCache(c, pkgRel, f, []string{})

			// Save
			testRecords := []*CachedRuleRecord{
				{Kind: "test", Pkg: pkgRel},
			}
			_ = cm.SavePackageCache(c, pkgRel, f, []string{}, testRecords)

			// Load again (should hit)
			_, _ = cm.LoadPackageCache(c, pkgRel, f, []string{})

			done <- true
		}(i)
	}

	// Wait for all goroutines
	for i := 0; i < 10; i++ {
		<-done
	}

	// Verify cache stats
	stats := cm.GetCacheStats()
	if stats == 0 {
		t.Error("Expected some cache entries to be loaded")
	}
}
