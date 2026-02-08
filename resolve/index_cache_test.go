package resolve

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/bazelbuild/bazel-gazelle/label"
	"github.com/bazelbuild/bazel-gazelle/rule"
	"github.com/google/go-cmp/cmp"
)

var cmpOpt = cmp.AllowUnexported(ruleRecord{})

func newTestIndex() *RuleIndex {
	return NewRuleIndex(func(r *rule.Rule, pkgRel string) Resolver { return nil })
}

func TestCacheRoundTrip(t *testing.T) {
	ix := newTestIndex()
	ix.rules = []*ruleRecord{
		{
			Kind:  "go_library",
			Label: label.New("", "pkg/foo", "foo"),
			Pkg:   "pkg/foo",
			ImportedAs: []ImportSpec{
				{Lang: "go", Imp: "example.com/pkg/foo"},
			},
			Embeds: []label.Label{label.New("", "pkg/foo", "embed")},
			Lang:   "go",
		},
		{
			Kind:  "go_library",
			Label: label.New("", "pkg/bar", "bar"),
			Pkg:   "pkg/bar",
			ImportedAs: []ImportSpec{
				{Lang: "go", Imp: "example.com/pkg/bar"},
			},
			Lang: "go",
		},
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "index.json")
	fp := "abc123"

	if err := ix.SaveCache(path, fp); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}

	ix2 := newTestIndex()
	if err := ix2.LoadCache(path, fp); err != nil {
		t.Fatalf("LoadCache: %v", err)
	}

	if diff := cmp.Diff(ix.rules, ix2.rules, cmpOpt); diff != "" {
		t.Errorf("round-trip mismatch (-want +got):\n%s", diff)
	}
}

func TestCacheFileNotFound(t *testing.T) {
	ix := newTestIndex()
	err := ix.LoadCache("/nonexistent/path/index.json", "fp")
	if err != nil {
		t.Fatalf("expected nil error for missing file, got: %v", err)
	}
	if len(ix.rules) != 0 {
		t.Fatalf("expected 0 rules, got %d", len(ix.rules))
	}
}

func TestCacheFingerprintMismatch(t *testing.T) {
	ix := newTestIndex()
	ix.rules = []*ruleRecord{
		{
			Kind:  "go_library",
			Label: label.New("", "pkg", "lib"),
			Pkg:   "pkg",
			Lang:  "go",
		},
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "index.json")

	if err := ix.SaveCache(path, "fingerprint_A"); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}

	ix2 := newTestIndex()
	if err := ix2.LoadCache(path, "fingerprint_B"); err != nil {
		t.Fatalf("LoadCache: %v", err)
	}
	if len(ix2.rules) != 0 {
		t.Errorf("expected 0 rules after fingerprint mismatch, got %d", len(ix2.rules))
	}
}

func TestCacheFormatVersionMismatch(t *testing.T) {
	c := struct {
		FormatVersion int           `json:"formatVersion"`
		Fingerprint   string        `json:"fingerprint"`
		Records       []*ruleRecord `json:"records"`
	}{
		FormatVersion: 9999,
		Fingerprint:   "fp",
		Records: []*ruleRecord{
			{Kind: "go_library", Label: label.New("", "pkg", "lib"), Pkg: "pkg", Lang: "go"},
		},
	}
	data, err := json.Marshal(&c)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "index.json")
	if err := os.WriteFile(path, data, 0o666); err != nil {
		t.Fatal(err)
	}

	ix := newTestIndex()
	if err := ix.LoadCache(path, "fp"); err != nil {
		t.Fatalf("LoadCache: %v", err)
	}
	if len(ix.rules) != 0 {
		t.Errorf("expected 0 rules after version mismatch, got %d", len(ix.rules))
	}
}

func TestCacheCorruptJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.json")
	if err := os.WriteFile(path, []byte("{corrupt"), 0o666); err != nil {
		t.Fatal(err)
	}

	ix := newTestIndex()
	err := ix.LoadCache(path, "fp")
	if err == nil {
		t.Fatal("expected error for corrupt JSON, got nil")
	}
}

func TestCacheLoadAfterFinish(t *testing.T) {
	ix := newTestIndex()
	ix.Finish()

	err := ix.LoadCache("/some/path", "fp")
	if err == nil {
		t.Fatal("expected error when LoadCache called after Finish, got nil")
	}
}

func TestCacheInvalidatePackage(t *testing.T) {
	ix := newTestIndex()
	ix.rules = []*ruleRecord{
		{Kind: "go_library", Label: label.New("", "a", "a"), Pkg: "a", Lang: "go"},
		{Kind: "go_library", Label: label.New("", "b", "b"), Pkg: "b", Lang: "go"},
		{Kind: "go_library", Label: label.New("", "a/sub", "sub"), Pkg: "a/sub", Lang: "go"},
		{Kind: "go_library", Label: label.New("", "a", "a2"), Pkg: "a", Lang: "go"},
	}

	ix.InvalidatePackage("a")

	want := []*ruleRecord{
		{Kind: "go_library", Label: label.New("", "b", "b"), Pkg: "b", Lang: "go"},
		{Kind: "go_library", Label: label.New("", "a/sub", "sub"), Pkg: "a/sub", Lang: "go"},
	}

	if diff := cmp.Diff(want, ix.rules, cmpOpt); diff != "" {
		t.Errorf("InvalidatePackage mismatch (-want +got):\n%s", diff)
	}
}

func TestCacheLabelSerialization(t *testing.T) {
	ix := newTestIndex()
	ix.rules = []*ruleRecord{
		{
			Kind:  "go_library",
			Label: label.New("repo", "some/pkg", "lib"),
			Pkg:   "some/pkg",
			ImportedAs: []ImportSpec{
				{Lang: "go", Imp: "example.com/some/pkg"},
				{Lang: "proto", Imp: "some/pkg/foo.proto"},
			},
			Embeds: []label.Label{
				label.New("repo", "some/pkg", "embed1"),
				label.New("", "other/pkg", "embed2"),
			},
			Lang: "go",
		},
		{
			Kind:       "proto_library",
			Label:      label.New("", "proto/pkg", "proto_lib"),
			Pkg:        "proto/pkg",
			ImportedAs: []ImportSpec{{Lang: "proto", Imp: "proto/pkg/foo.proto"}},
			Lang:       "proto",
		},
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "index.json")
	fp := "labelfp"

	if err := ix.SaveCache(path, fp); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}

	ix2 := newTestIndex()
	if err := ix2.LoadCache(path, fp); err != nil {
		t.Fatalf("LoadCache: %v", err)
	}

	if diff := cmp.Diff(ix.rules, ix2.rules, cmpOpt); diff != "" {
		t.Errorf("label serialization mismatch (-want +got):\n%s", diff)
	}
}

func TestCacheSameInputOutputPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.json")
	fp := "samepath"

	// Save initial records.
	ix := newTestIndex()
	ix.rules = []*ruleRecord{
		{Kind: "go_library", Label: label.New("", "pkg", "lib"), Pkg: "pkg", Lang: "go"},
	}
	if err := ix.SaveCache(path, fp); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}

	// Load from same path.
	ix2 := newTestIndex()
	if err := ix2.LoadCache(path, fp); err != nil {
		t.Fatalf("LoadCache: %v", err)
	}
	if len(ix2.rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(ix2.rules))
	}

	// Add more records, save to same path.
	ix2.rules = append(ix2.rules, &ruleRecord{
		Kind:  "go_library",
		Label: label.New("", "pkg2", "lib2"),
		Pkg:   "pkg2",
		Lang:  "go",
	})
	if err := ix2.SaveCache(path, fp); err != nil {
		t.Fatalf("SaveCache (second): %v", err)
	}

	// Load again and verify both records are present.
	ix3 := newTestIndex()
	if err := ix3.LoadCache(path, fp); err != nil {
		t.Fatalf("LoadCache (second): %v", err)
	}
	if len(ix3.rules) != 2 {
		t.Fatalf("expected 2 rules, got %d", len(ix3.rules))
	}
}

func TestBinaryFingerprint(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "fake_binary")
	if err := os.WriteFile(binPath, []byte("hello world"), 0o755); err != nil {
		t.Fatal(err)
	}

	fp1, err := BinaryFingerprint(binPath)
	if err != nil {
		t.Fatalf("BinaryFingerprint: %v", err)
	}
	if fp1 == "" {
		t.Fatal("expected non-empty fingerprint")
	}

	// Same content should produce same fingerprint.
	fp2, err := BinaryFingerprint(binPath)
	if err != nil {
		t.Fatalf("BinaryFingerprint: %v", err)
	}
	if fp1 != fp2 {
		t.Errorf("expected same fingerprint, got %q and %q", fp1, fp2)
	}

	// Different content should produce different fingerprint.
	binPath2 := filepath.Join(dir, "fake_binary2")
	if err := os.WriteFile(binPath2, []byte("different content"), 0o755); err != nil {
		t.Fatal(err)
	}
	fp3, err := BinaryFingerprint(binPath2)
	if err != nil {
		t.Fatalf("BinaryFingerprint: %v", err)
	}
	if fp1 == fp3 {
		t.Errorf("expected different fingerprints, both got %q", fp1)
	}
}

// generateRecords creates n ruleRecords spread across numPkgs packages.
func generateRecords(n, numPkgs int) []*ruleRecord {
	records := make([]*ruleRecord, n)
	for i := range n {
		pkg := fmt.Sprintf("pkg/%d", i%numPkgs)
		records[i] = &ruleRecord{
			Kind:  "go_library",
			Label: label.New("", pkg, fmt.Sprintf("lib_%d", i)),
			Pkg:   pkg,
			ImportedAs: []ImportSpec{
				{Lang: "go", Imp: fmt.Sprintf("example.com/%s/lib_%d", pkg, i)},
			},
			Lang: "go",
		}
	}
	return records
}

func BenchmarkSaveCache(b *testing.B) {
	for _, size := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprintf("records=%d", size), func(b *testing.B) {
			ix := newTestIndex()
			ix.rules = generateRecords(size, size/10)
			dir := b.TempDir()
			path := filepath.Join(dir, "index.json")

			b.ResetTimer()
			for range b.N {
				if err := ix.SaveCache(path, "bench-fp"); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkLoadCache(b *testing.B) {
	for _, size := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprintf("records=%d", size), func(b *testing.B) {
			ix := newTestIndex()
			ix.rules = generateRecords(size, size/10)
			dir := b.TempDir()
			path := filepath.Join(dir, "index.json")
			if err := ix.SaveCache(path, "bench-fp"); err != nil {
				b.Fatal(err)
			}

			b.ResetTimer()
			for range b.N {
				ix2 := newTestIndex()
				if err := ix2.LoadCache(path, "bench-fp"); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkInvalidatePackage(b *testing.B) {
	for _, size := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprintf("records=%d", size), func(b *testing.B) {
			base := generateRecords(size, size/10)

			b.ResetTimer()
			for range b.N {
				b.StopTimer()
				ix := newTestIndex()
				ix.rules = make([]*ruleRecord, len(base))
				copy(ix.rules, base)
				b.StartTimer()

				ix.InvalidatePackage("pkg/0")
			}
		})
	}
}

func BenchmarkBinaryFingerprint(b *testing.B) {
	// Create a ~1MB fake binary to benchmark hashing.
	dir := b.TempDir()
	binPath := filepath.Join(dir, "fake_binary")
	data := make([]byte, 1<<20)
	for i := range data {
		data[i] = byte(i)
	}
	if err := os.WriteFile(binPath, data, 0o755); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for range b.N {
		if _, err := BinaryFingerprint(binPath); err != nil {
			b.Fatal(err)
		}
	}
}
