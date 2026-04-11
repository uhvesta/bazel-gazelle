package walk

import (
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/rule"
	"github.com/bazelbuild/bazel-gazelle/v3/internal/vfs"
)

type testConfigurer struct{}

func (*testConfigurer) RegisterFlags(fs *flag.FlagSet, cmd string, c *config.Config) {}
func (*testConfigurer) CheckFlags(fs *flag.FlagSet, c *config.Config) error          { return nil }
func (*testConfigurer) KnownDirectives() []string                                    { return []string{"set_name"} }
func (*testConfigurer) Configure(c *config.Config, rel string, f *rule.File) {
	if f == nil {
		return
	}
	for _, d := range f.Directives {
		if d.Key == "set_name" {
			c.RepoName = d.Value
		}
	}
}

func TestWalkVisitsInPostOrderAndPropagatesConfig(t *testing.T) {
	root := t.TempDir()
	writeWalkFile(t, filepath.Join(root, "BUILD.bazel"), "# gazelle:set_name root\n")
	writeWalkFile(t, filepath.Join(root, "child", "BUILD.bazel"), "")
	writeWalkFile(t, filepath.Join(root, "child", "x.proto"), "syntax = \"proto3\";")

	repo, err := buildFrozenSnapshot(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.New()
	cfg.RepoRoot = root
	cfg.ValidBuildFileNames = []string{"BUILD.bazel", "BUILD"}

	var visited []string
	var repoNames []string
	err = Walk(repo, cfg, []config.Configurer{&testConfigurer{}}, func(args FuncArgs) error {
		visited = append(visited, args.Rel)
		repoNames = append(repoNames, args.Config.RepoName)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(visited, []string{"child", ""}) {
		t.Fatalf("visited = %#v, want %#v", visited, []string{"child", ""})
	}
	if !reflect.DeepEqual(repoNames, []string{"root", "root"}) {
		t.Fatalf("repoNames = %#v, want %#v", repoNames, []string{"root", "root"})
	}
}

func TestWalkLoadsBuildFileAndGenFiles(t *testing.T) {
	root := t.TempDir()
	writeWalkFile(t, filepath.Join(root, "BUILD.bazel"), "genrule(name = \"g\", outs = [\"a.pb.go\", \"b.pb.go\"], cmd = \"\")\n")

	repo, err := buildFrozenSnapshot(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.New()
	cfg.RepoRoot = root
	cfg.ValidBuildFileNames = []string{"BUILD.bazel", "BUILD"}

	err = Walk(repo, cfg, nil, func(args FuncArgs) error {
		if args.File == nil {
			t.Fatal("expected build file to be loaded")
		}
		if !reflect.DeepEqual(args.GenFiles, []string{"a.pb.go", "b.pb.go"}) {
			t.Fatalf("GenFiles = %#v, want %#v", args.GenFiles, []string{"a.pb.go", "b.pb.go"})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func buildFrozenSnapshot(root string) (*vfs.Snapshot, error) {
	build, err := vfs.Build(root, vfs.BuildOptions{})
	if err != nil {
		return nil, err
	}
	return build.Freeze(), nil
}

func writeWalkFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
