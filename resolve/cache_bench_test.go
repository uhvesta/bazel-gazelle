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
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/label"
	"github.com/bazelbuild/bazel-gazelle/rule"
)

func BenchmarkCacheSave(b *testing.B) {
	tmpDir := b.TempDir()
	repoRoot := filepath.Join(tmpDir, "repo")
	os.MkdirAll(repoRoot, 0755)

	c := &config.Config{
		RepoRoot:      repoRoot,
		RepoName:      "bench_repo",
		IndexCache:    true,
		IndexCacheDir: ".gazelle-cache",
	}
	configHash := ComputeConfigFingerprint(c)
	cm := NewIndexCacheManager(repoRoot, ".gazelle-cache", configHash)

	// Create test records (simulating a medium-sized package)
	testRecords := make([]*CachedRuleRecord, 20)
	for i := 0; i < 20; i++ {
		testRecords[i] = &CachedRuleRecord{
			Kind:  "go_library",
			Label: label.Label{Repo: "bench_repo", Pkg: "pkg", Name: fmt.Sprintf("lib%d", i)},
			Pkg:   "pkg",
			ImportedAs: []ImportSpec{
				{Lang: "go", Imp: fmt.Sprintf("example.com/bench/pkg/lib%d", i)},
			},
			Lang: "go",
		}
	}

	buildFilePath := filepath.Join(repoRoot, "pkg", "BUILD.bazel")
	os.MkdirAll(filepath.Dir(buildFilePath), 0755)
	os.WriteFile(buildFilePath, []byte("# benchmark\n"), 0644)

	f := &rule.File{Path: buildFilePath, Pkg: "pkg"}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		pkgRel := fmt.Sprintf("pkg%d", i)
		cm.SavePackageCache(c, pkgRel, f, []string{}, testRecords)
	}
}

func BenchmarkCacheLoad(b *testing.B) {
	tmpDir := b.TempDir()
	repoRoot := filepath.Join(tmpDir, "repo")
	os.MkdirAll(repoRoot, 0755)

	c := &config.Config{
		RepoRoot:      repoRoot,
		RepoName:      "bench_repo",
		IndexCache:    true,
		IndexCacheDir: ".gazelle-cache",
	}
	configHash := ComputeConfigFingerprint(c)
	cm := NewIndexCacheManager(repoRoot, ".gazelle-cache", configHash)

	// Pre-populate cache
	testRecords := []*CachedRuleRecord{
		{
			Kind:  "go_library",
			Label: label.Label{Repo: "bench_repo", Pkg: "pkg", Name: "test"},
			Pkg:   "pkg",
			ImportedAs: []ImportSpec{
				{Lang: "go", Imp: "example.com/bench/pkg"},
			},
			Lang: "go",
		},
	}

	buildFilePath := filepath.Join(repoRoot, "pkg", "BUILD.bazel")
	os.MkdirAll(filepath.Dir(buildFilePath), 0755)
	os.WriteFile(buildFilePath, []byte("# benchmark\n"), 0644)

	f := &rule.File{Path: buildFilePath, Pkg: "pkg"}
	cm.SavePackageCache(c, "pkg", f, []string{}, testRecords)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cm.LoadPackageCache(c, "pkg", f, []string{})
	}
}

func BenchmarkCacheLoadConcurrent(b *testing.B) {
	tmpDir := b.TempDir()
	repoRoot := filepath.Join(tmpDir, "repo")
	os.MkdirAll(repoRoot, 0755)

	c := &config.Config{
		RepoRoot:      repoRoot,
		RepoName:      "bench_repo",
		IndexCache:    true,
		IndexCacheDir: ".gazelle-cache",
	}
	configHash := ComputeConfigFingerprint(c)
	cm := NewIndexCacheManager(repoRoot, ".gazelle-cache", configHash)

	// Pre-populate caches for multiple packages
	numPackages := 100
	for i := 0; i < numPackages; i++ {
		pkgRel := fmt.Sprintf("pkg%d", i)
		buildFilePath := filepath.Join(repoRoot, pkgRel, "BUILD.bazel")
		os.MkdirAll(filepath.Dir(buildFilePath), 0755)
		os.WriteFile(buildFilePath, []byte("# benchmark\n"), 0644)

		testRecords := []*CachedRuleRecord{
			{
				Kind:  "go_library",
				Label: label.Label{Repo: "bench_repo", Pkg: pkgRel, Name: "test"},
				Pkg:   pkgRel,
			},
		}

		f := &rule.File{Path: buildFilePath, Pkg: pkgRel}
		cm.SavePackageCache(c, pkgRel, f, []string{}, testRecords)
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			pkgRel := fmt.Sprintf("pkg%d", i%numPackages)
			buildFilePath := filepath.Join(repoRoot, pkgRel, "BUILD.bazel")
			f := &rule.File{Path: buildFilePath, Pkg: pkgRel}
			cm.LoadPackageCache(c, pkgRel, f, []string{})
			i++
		}
	})
}

func BenchmarkConfigFingerprint(b *testing.B) {
	c := &config.Config{
		RepoName: "test_repo",
		Langs:    []string{"go", "proto", "java"},
		KindMap: map[string]config.MappedKind{
			"go_library": {FromKind: "go_library", KindName: "custom_go_library"},
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ComputeConfigFingerprint(c)
	}
}

func BenchmarkFileHashing(b *testing.B) {
	tmpDir := b.TempDir()
	testFile := filepath.Join(tmpDir, "test.go")

	// Create a test file with realistic size (10KB)
	content := make([]byte, 10*1024)
	for i := range content {
		content[i] = byte(i % 256)
	}
	os.WriteFile(testFile, content, 0644)

	cm := NewIndexCacheManager(tmpDir, ".gazelle-cache", "test")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cm.computeFileHash(testFile)
	}
}
