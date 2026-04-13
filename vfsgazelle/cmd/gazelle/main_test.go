package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/uhvesta/bazel-gazelle/vfsgazelle/internal/vfs"
)

func TestSaveLoadSnapshot(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "BUILD.bazel"), []byte("# gazelle:prefix example.com/test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "foo.go"), []byte("package foo\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	registry := vfs.NewRegistry()
	parser := testParser{key: "test/model", version: "v1"}
	if err := registry.Register(parser, vfs.MatchExtension(".go")); err != nil {
		t.Fatal(err)
	}
	bs, err := vfs.Build(root, vfs.BuildOptions{Registry: registry})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bs.GetModel("foo.go", parser.Key()); err != nil {
		t.Fatal(err)
	}
	snapshot := bs.Freeze()

	for _, tc := range []struct {
		name   string
		format vfs.StateFormat
	}{
		{name: "gob", format: vfs.StateFormatGob},
		{name: "json", format: vfs.StateFormatJSON},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			base := filepath.Join(t.TempDir(), "state")
			if err := saveSnapshot(base, snapshot, tc.format); err != nil {
				t.Fatal(err)
			}
			metaPath, _, _ := statePaths(base, tc.format)
			if _, err := os.Stat(parserCachePath(base, parser.Key(), tc.format)); err != nil {
				t.Fatal(err)
			}

			loaded, _, _, err := loadSnapshot(base, registry)
			if err != nil {
				t.Fatal(err)
			}

			got, err := loaded.ReadFile("BUILD.bazel")
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != "# gazelle:prefix example.com/test\n" {
				t.Fatalf("loaded BUILD.bazel mismatch: %q", got)
			}
			if loaded.ParserVersions()[parser.Key()] != parser.CacheVersion() {
				t.Fatalf("parser version = %q, want %q", loaded.ParserVersions()[parser.Key()], parser.CacheVersion())
			}
			if _, err := os.Stat(metaPath); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLoadSnapshotSkipsStaleParserCacheFile(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "foo.go"), []byte("package foo\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	registry := vfs.NewRegistry()
	parserV1 := testParser{key: "test/model", version: "v1"}
	if err := registry.Register(parserV1, vfs.MatchExtension(".go")); err != nil {
		t.Fatal(err)
	}
	bs, err := vfs.Build(root, vfs.BuildOptions{Registry: registry})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bs.GetModel("foo.go", parserV1.Key()); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(t.TempDir(), "state")
	if err := saveSnapshot(base, bs.Freeze(), vfs.StateFormatGob); err != nil {
		t.Fatal(err)
	}

	registryV2 := vfs.NewRegistry()
	parserV2 := testParser{key: "test/model", version: "v2"}
	if err := registryV2.Register(parserV2, vfs.MatchExtension(".go")); err != nil {
		t.Fatal(err)
	}
	loaded, _, _, err := loadSnapshot(base, registryV2)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.ParserVersions()[parserV2.Key()]; got != "v1" {
		t.Fatalf("loaded manifest version = %q, want %q", got, "v1")
	}
	if _, err := loaded.GetModel("foo.go", parserV2.Key()); err == nil {
		t.Fatal("expected stale parser cache to miss")
	}
}

func TestStateBasePathUsesExplicitStateDir(t *testing.T) {
	t.Parallel()

	repoRoot := filepath.Join(string(os.PathSeparator), "repo")

	got := stateBasePath(repoRoot, "test-cache")
	want := filepath.Join(repoRoot, "test-cache", "vfsgazelle-state")
	if got != want {
		t.Fatalf("stateBasePath(relative) = %q, want %q", got, want)
	}

	got = stateBasePath(repoRoot, filepath.Join(string(os.PathSeparator), "tmp", "cache"))
	want = filepath.Join(string(os.PathSeparator), "tmp", "cache", "vfsgazelle-state")
	if got != want {
		t.Fatalf("stateBasePath(abs) = %q, want %q", got, want)
	}
}

type testParser struct {
	key     string
	version string
}

func (p testParser) Key() string          { return p.key }
func (p testParser) CacheVersion() string { return p.version }
func (p testParser) Parse(path string, data []byte) (any, error) {
	return string(data), nil
}
func (p testParser) Encode(model any) ([]byte, error) { return json.Marshal(model) }
func (p testParser) Decode(data []byte) (any, error) {
	var s string
	err := json.Unmarshal(data, &s)
	return s, err
}
