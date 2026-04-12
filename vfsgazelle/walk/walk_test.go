package walk

import (
	"flag"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/uhvesta/bazel-gazelle/config"
	"github.com/uhvesta/bazel-gazelle/rule"
	"github.com/uhvesta/bazel-gazelle/vfsgazelle/internal/vfs"
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
		if !args.Update {
			t.Fatalf("expected updates enabled for %q", args.Rel)
		}
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

func TestWalkHonorsBazelIgnoreAndExcludeDirective(t *testing.T) {
	root := t.TempDir()
	writeWalkFile(t, filepath.Join(root, ".bazelignore"), "ignored\n")
	writeWalkFile(t, filepath.Join(root, "BUILD.bazel"), "# gazelle:exclude skipped\n")
	writeWalkFile(t, filepath.Join(root, "ignored", "BUILD.bazel"), "")
	writeWalkFile(t, filepath.Join(root, "skipped", "BUILD.bazel"), "")
	writeWalkFile(t, filepath.Join(root, "kept", "BUILD.bazel"), "")

	repo, err := buildFrozenSnapshot(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.New()
	cfg.RepoRoot = root
	cfg.ValidBuildFileNames = []string{"BUILD.bazel", "BUILD"}

	wc := &Configurer{cliBuildFileNames: "BUILD.bazel,BUILD"}
	if err := wc.CheckFlags(nil, cfg); err != nil {
		t.Fatal(err)
	}

	var visited []string
	err = Walk(repo, cfg, []config.Configurer{wc}, func(args FuncArgs) error {
		visited = append(visited, args.Rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(visited, []string{"kept", ""}) {
		t.Fatalf("visited = %#v, want %#v", visited, []string{"kept", ""})
	}
}

func TestWalkHonorsIgnoreDirectiveForUpdate(t *testing.T) {
	root := t.TempDir()
	writeWalkFile(t, filepath.Join(root, "pkg", "BUILD.bazel"), "# gazelle:ignore\n")

	repo, err := buildFrozenSnapshot(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.New()
	cfg.RepoRoot = root
	cfg.ValidBuildFileNames = []string{"BUILD.bazel", "BUILD"}

	wc := &Configurer{cliBuildFileNames: "BUILD.bazel,BUILD"}
	if err := wc.CheckFlags(nil, cfg); err != nil {
		t.Fatal(err)
	}

	updateByRel := make(map[string]bool)
	err = Walk(repo, cfg, []config.Configurer{wc}, func(args FuncArgs) error {
		updateByRel[args.Rel] = args.Update
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := updateByRel["pkg"]; got {
		t.Fatalf("pkg Update = %v, want false", got)
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

func TestWalkPackageDirRespectsExcludeDirective(t *testing.T) {
	root := t.TempDir()
	writeWalkFile(t, filepath.Join(root, "BUILD.bazel"), "# gazelle:exclude *_test.go\n")
	writeWalkFile(t, filepath.Join(root, "a.go"), "package root\n")
	writeWalkFile(t, filepath.Join(root, "a_test.go"), "package root\n")

	repo, err := buildFrozenSnapshot(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.New()
	cfg.RepoRoot = root
	cfg.ValidBuildFileNames = []string{"BUILD.bazel", "BUILD"}

	err = Walk(repo, cfg, []config.Configurer{&Configurer{}}, func(args FuncArgs) error {
		var names []string
		for _, file := range args.PackageDir.RegularFiles() {
			names = append(names, file.Name())
		}
		if !reflect.DeepEqual(names, []string{"BUILD.bazel", "a.go"}) {
			t.Fatalf("RegularFiles = %#v, want %#v", names, []string{"BUILD.bazel", "a.go"})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestConfigurerPreservesPreconfiguredBuildFileNames(t *testing.T) {
	cfg := config.New()
	cfg.RepoRoot = t.TempDir()
	cfg.ValidBuildFileNames = []string{"BUILD.custom", "BUILD.alt"}

	cr := &Configurer{}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	cr.RegisterFlags(fs, "fix", cfg)
	if err := cr.CheckFlags(fs, cfg); err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(cfg.ValidBuildFileNames, []string{"BUILD.custom", "BUILD.alt"}) {
		t.Fatalf("ValidBuildFileNames = %#v, want %#v", cfg.ValidBuildFileNames, []string{"BUILD.custom", "BUILD.alt"})
	}
}

func TestWalkIgnoresUnsupportedGenerationModeDirective(t *testing.T) {
	root := t.TempDir()
	writeWalkFile(t, filepath.Join(root, "BUILD.bazel"), "# gazelle:generation_mode update_only\n")

	repo, err := buildFrozenSnapshot(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.New()
	cfg.RepoRoot = root
	cfg.ValidBuildFileNames = []string{"BUILD.bazel", "BUILD"}

	var logBuf strings.Builder
	prevWriter := log.Writer()
	log.SetOutput(&logBuf)
	defer log.SetOutput(prevWriter)

	var update bool
	err = Walk(repo, cfg, []config.Configurer{&Configurer{}}, func(args FuncArgs) error {
		update = args.Update
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !update {
		t.Fatal("expected update_only to be ignored in vfsgazelle")
	}
	if !strings.Contains(logBuf.String(), "gazelle:generation_mode is not supported in vfsgazelle") {
		t.Fatalf("expected unsupported generation_mode warning, got %q", logBuf.String())
	}
}

func TestFilterChangesHonorsBazelIgnoreAndExclude(t *testing.T) {
	root := t.TempDir()
	writeWalkFile(t, filepath.Join(root, ".bazelignore"), "ignored\n")
	writeWalkFile(t, filepath.Join(root, "BUILD.bazel"), "# gazelle:exclude skipped/**\n")
	writeWalkFile(t, filepath.Join(root, "kept", "BUILD.bazel"), "")

	repo, err := buildFrozenSnapshot(root)
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.New()
	cfg.RepoRoot = root
	cfg.ValidBuildFileNames = []string{"BUILD.bazel", "BUILD"}

	cr := &Configurer{}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	cr.RegisterFlags(fs, "run", cfg)
	if err := cr.CheckFlags(fs, cfg); err != nil {
		t.Fatal(err)
	}

	got := FilterChanges(repo, cfg, []vfs.Change{
		{Path: "ignored/new.go", Kind: vfs.ChangeModify},
		{Path: "skipped/new.go", Kind: vfs.ChangeModify},
		{Path: "kept/new.go", Kind: vfs.ChangeModify},
	})
	want := []vfs.Change{{Path: "kept/new.go", Kind: vfs.ChangeModify}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("FilterChanges() = %#v, want %#v", got, want)
	}
}

func TestPromoteTraversalChangesForBuildExcludeChange(t *testing.T) {
	root := t.TempDir()
	writeWalkFile(t, filepath.Join(root, "pkg", "BUILD.bazel"), "# gazelle:exclude old/**\n")
	writeWalkFile(t, filepath.Join(root, "pkg", "child", "x.go"), "package child\n")

	repo, err := buildFrozenSnapshot(root)
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.New()
	cfg.RepoRoot = root
	cfg.ValidBuildFileNames = []string{"BUILD.bazel", "BUILD"}

	writeWalkFile(t, filepath.Join(root, "pkg", "BUILD.bazel"), "# gazelle:exclude new/**\n")

	got, fullRebuild, err := PromoteTraversalChanges(repo, cfg, []vfs.Change{
		{Path: "pkg/BUILD.bazel", Kind: vfs.ChangeModify},
	})
	if err != nil {
		t.Fatal(err)
	}
	if fullRebuild {
		t.Fatal("fullRebuild = true, want false")
	}
	want := []vfs.Change{{Path: "pkg", Kind: vfs.ChangeModify}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("PromoteTraversalChanges() = %#v, want %#v", got, want)
	}
}

func TestPromoteTraversalChangesForRootIgnoreChangeTriggersFullRebuild(t *testing.T) {
	root := t.TempDir()
	writeWalkFile(t, filepath.Join(root, "BUILD.bazel"), "# gazelle:ignore\n")

	repo, err := buildFrozenSnapshot(root)
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.New()
	cfg.RepoRoot = root
	cfg.ValidBuildFileNames = []string{"BUILD.bazel", "BUILD"}

	writeWalkFile(t, filepath.Join(root, "BUILD.bazel"), "")

	got, fullRebuild, err := PromoteTraversalChanges(repo, cfg, []vfs.Change{
		{Path: "BUILD.bazel", Kind: vfs.ChangeModify},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !fullRebuild {
		t.Fatal("fullRebuild = false, want true")
	}
	if got != nil {
		t.Fatalf("PromoteTraversalChanges() changes = %#v, want nil", got)
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
