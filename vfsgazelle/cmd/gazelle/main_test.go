package main

import (
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
	bs, err := vfs.Build(root, vfs.BuildOptions{Registry: registry})
	if err != nil {
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
		})
	}
}
