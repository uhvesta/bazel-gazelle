package vfs

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestBuildSnapshotListsDirsAndFiles(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "pkg", "foo.txt"), "hello")
	writeTestFile(t, filepath.Join(root, "pkg", "sub", "bar.proto"), "syntax = \"proto3\";")

	snapshot, err := Build(root, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}

	rootEntries, err := snapshot.ListDir("")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rootEntries, []string{"pkg"}) {
		t.Fatalf("root entries = %#v, want %#v", rootEntries, []string{"pkg"})
	}

	pkgEntries, err := snapshot.ListDir("pkg")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pkgEntries, []string{"foo.txt", "sub"}) {
		t.Fatalf("pkg entries = %#v, want %#v", pkgEntries, []string{"foo.txt", "sub"})
	}

	got, err := snapshot.ReadFile("pkg/foo.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Fatalf("ReadFile = %q, want %q", got, "hello")
	}
}

func TestBuildSnapshotGetModelUsesRegisteredParsersAndCache(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "pkg", "foo.txt"), "a\nb")

	registry := NewRegistry()
	parser := &countingParser{key: "test/model", version: "v1"}
	if err := registry.Register(parser, MatchExtension(".txt")); err != nil {
		t.Fatal(err)
	}

	snapshot, err := Build(root, BuildOptions{Registry: registry})
	if err != nil {
		t.Fatal(err)
	}

	matching := snapshot.MatchingParsers("pkg/foo.txt")
	if len(matching) != 1 || matching[0].Key() != parser.Key() {
		t.Fatalf("MatchingParsers = %#v, want [%q]", matching, parser.Key())
	}

	first, err := snapshot.GetModel("pkg/foo.txt", parser.Key())
	if err != nil {
		t.Fatal(err)
	}
	second, err := snapshot.GetModel("pkg/foo.txt", parser.Key())
	if err != nil {
		t.Fatal(err)
	}

	if parser.parses != 1 {
		t.Fatalf("parser called %d times, want 1", parser.parses)
	}
	if first.CacheHit {
		t.Fatal("first GetModel should not be a cache hit")
	}
	if !second.CacheHit {
		t.Fatal("second GetModel should be a cache hit")
	}
}

func TestBuildSnapshotCanSeedFromPreviousFrozenCache(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "pkg", "foo.txt")
	writeTestFile(t, path, "same")

	registry := NewRegistry()
	parser := &countingParser{key: "test/model", version: "v1"}
	if err := registry.Register(parser, MatchExtension(".txt")); err != nil {
		t.Fatal(err)
	}

	firstBuild, err := Build(root, BuildOptions{Registry: registry})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := firstBuild.GetModel("pkg/foo.txt", parser.Key()); err != nil {
		t.Fatal(err)
	}
	cache := firstBuild.Freeze().cache

	secondBuild, err := Build(root, BuildOptions{
		Cache:    cache,
		Registry: registry,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := secondBuild.GetModel("pkg/foo.txt", parser.Key())
	if err != nil {
		t.Fatal(err)
	}

	if parser.parses != 1 {
		t.Fatalf("parser called %d times, want 1", parser.parses)
	}
	if !got.CacheHit {
		t.Fatal("seeded build should reuse parsed model")
	}
}

func TestBuildSnapshotInvalidatesParsedModelWhenFileChanges(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "pkg", "foo.txt")
	writeTestFile(t, path, "old")

	registry := NewRegistry()
	parser := &countingParser{key: "test/model", version: "v1"}
	if err := registry.Register(parser, MatchExtension(".txt")); err != nil {
		t.Fatal(err)
	}

	firstBuild, err := Build(root, BuildOptions{Registry: registry})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := firstBuild.GetModel("pkg/foo.txt", parser.Key()); err != nil {
		t.Fatal(err)
	}
	cache := firstBuild.Freeze().cache

	writeTestFile(t, path, "new")

	secondBuild, err := Build(root, BuildOptions{
		Cache:    cache,
		Registry: registry,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := secondBuild.GetModel("pkg/foo.txt", parser.Key())
	if err != nil {
		t.Fatal(err)
	}

	if parser.parses != 2 {
		t.Fatalf("parser called %d times, want 2", parser.parses)
	}
	if got.CacheHit {
		t.Fatal("changed file should not hit parsed-model cache")
	}
}

func TestFrozenSnapshotServesReadOnlyModels(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "pkg", "foo.txt"), "same")

	registry := NewRegistry()
	parser := &countingParser{key: "test/model", version: "v1"}
	if err := registry.Register(parser, MatchExtension(".txt")); err != nil {
		t.Fatal(err)
	}

	buildSnapshot, err := Build(root, BuildOptions{Registry: registry})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := buildSnapshot.GetModel("pkg/foo.txt", parser.Key()); err != nil {
		t.Fatal(err)
	}

	frozen := buildSnapshot.Freeze()
	got, err := frozen.GetModel("pkg/foo.txt", parser.Key())
	if err != nil {
		t.Fatal(err)
	}
	if !got.CacheHit {
		t.Fatal("frozen snapshot should return cache hit")
	}
}

func TestFreezeConsumesBuildSnapshotState(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "pkg", "foo.txt"), "same")

	buildSnapshot, err := Build(root, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}

	frozen := buildSnapshot.Freeze()
	if _, err := frozen.ReadFile("pkg/foo.txt"); err != nil {
		t.Fatal(err)
	}
	if buildSnapshot.builder != nil {
		t.Fatal("expected builder to be cleared after freeze")
	}
	if buildSnapshot.files != nil {
		t.Fatal("expected file map to be cleared after freeze")
	}
	if buildSnapshot.dirs != nil {
		t.Fatal("expected dir map to be cleared after freeze")
	}
}

func TestSnapshotRoundTripPersistence(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "pkg", "foo.txt"), "hello")

	registry := NewRegistry()
	parser := &countingParser{key: "test/model", version: "v1"}
	if err := registry.Register(parser, MatchExtension(".txt")); err != nil {
		t.Fatal(err)
	}

	buildSnapshot, err := Build(root, BuildOptions{Registry: registry})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := buildSnapshot.GetModel("pkg/foo.txt", parser.Key()); err != nil {
		t.Fatal(err)
	}
	frozen := buildSnapshot.Freeze()

	var buf bytes.Buffer
	if err := frozen.Save(&buf); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadSnapshot(&buf, registry)
	if err != nil {
		t.Fatal(err)
	}
	got, err := loaded.GetModel("pkg/foo.txt", parser.Key())
	if err != nil {
		t.Fatal(err)
	}
	if !got.CacheHit {
		t.Fatal("loaded snapshot should serve cached model")
	}
}

func TestPatchUpdatesOnlyChangedPaths(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "pkg", "foo.txt"), "old")
	writeTestFile(t, filepath.Join(root, "pkg", "bar.txt"), "same")

	registry := NewRegistry()
	parser := &countingParser{key: "test/model", version: "v1"}
	if err := registry.Register(parser, MatchExtension(".txt")); err != nil {
		t.Fatal(err)
	}

	buildSnapshot, err := Build(root, BuildOptions{Registry: registry})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := buildSnapshot.GetModel("pkg/foo.txt", parser.Key()); err != nil {
		t.Fatal(err)
	}
	if _, err := buildSnapshot.GetModel("pkg/bar.txt", parser.Key()); err != nil {
		t.Fatal(err)
	}
	frozen := buildSnapshot.Freeze()

	writeTestFile(t, filepath.Join(root, "pkg", "foo.txt"), "new")
	patched, err := Patch(root, frozen, BuildOptions{Registry: registry}, []Change{{Path: "pkg/foo.txt", Kind: ChangeModify}})
	if err != nil {
		t.Fatal(err)
	}

	foo, err := patched.GetModel("pkg/foo.txt", parser.Key())
	if err != nil {
		t.Fatal(err)
	}
	bar, err := patched.GetModel("pkg/bar.txt", parser.Key())
	if err != nil {
		t.Fatal(err)
	}
	if parser.parses != 3 {
		t.Fatalf("parser called %d times, want 3", parser.parses)
	}
	if foo.CacheHit {
		t.Fatal("changed file should not hit parsed-model cache")
	}
	if !bar.CacheHit {
		t.Fatal("unchanged file should reuse parsed-model cache")
	}
}

func TestPatchHandlesDeletion(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "pkg", "foo.txt"), "hello")

	buildSnapshot, err := Build(root, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	frozen := buildSnapshot.Freeze()

	if err := os.Remove(filepath.Join(root, "pkg", "foo.txt")); err != nil {
		t.Fatal(err)
	}
	patched, err := Patch(root, frozen, BuildOptions{}, []Change{{Path: "pkg/foo.txt", Kind: ChangeDelete}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := patched.ReadFile("pkg/foo.txt"); !os.IsNotExist(err) {
		t.Fatalf("ReadFile error = %v, want not exist", err)
	}
}

func TestPatchSkipsUnchangedFileContent(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "pkg", "foo.txt"), "same")

	buildSnapshot, err := Build(root, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	frozen := buildSnapshot.Freeze()

	patched, err := Patch(root, frozen, BuildOptions{}, []Change{{Path: "pkg/foo.txt", Kind: ChangeModify}})
	if err != nil {
		t.Fatal(err)
	}
	if patched.Changed() {
		t.Fatal("expected unchanged file patch to be skipped")
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
