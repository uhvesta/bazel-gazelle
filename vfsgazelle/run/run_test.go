package run

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/uhvesta/bazel-gazelle/config"
	"github.com/uhvesta/bazel-gazelle/resolve"
	"github.com/uhvesta/bazel-gazelle/rule"
	"github.com/uhvesta/bazel-gazelle/vfsgazelle/internal/vfs"
	v3language "github.com/uhvesta/bazel-gazelle/vfsgazelle/language"
	golang "github.com/uhvesta/bazel-gazelle/vfsgazelle/language/go"
)

type fakeModel struct {
	Export  string   `json:"export"`
	Imports []string `json:"imports"`
}

type fakeParser struct{}

func (*fakeParser) Key() string          { return "fake/model" }
func (*fakeParser) CacheVersion() string { return "v1" }
func (*fakeParser) Parse(path string, data []byte) (any, error) {
	var model fakeModel
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "export "):
			model.Export = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		case strings.HasPrefix(line, "import "):
			model.Imports = append(model.Imports, strings.TrimSpace(strings.TrimPrefix(line, "import ")))
		}
	}
	return model, nil
}
func (*fakeParser) Encode(model any) ([]byte, error) { return json.Marshal(model) }
func (*fakeParser) Decode(data []byte) (any, error) {
	var model fakeModel
	err := json.Unmarshal(data, &model)
	return model, err
}

type fakeLang struct {
	v3language.BaseLang
}

func (*fakeLang) Name() string { return "fake" }

func (*fakeLang) RegisterParsers(reg *vfs.Registry) error {
	return reg.Register(&fakeParser{}, vfs.MatchExtension(".foo"))
}

func (*fakeLang) RegisterFlags(fs *flag.FlagSet, cmd string, c *config.Config) {}
func (*fakeLang) CheckFlags(fs *flag.FlagSet, c *config.Config) error          { return nil }
func (*fakeLang) KnownDirectives() []string                                    { return nil }

func (*fakeLang) Kinds() map[string]rule.KindInfo {
	return map[string]rule.KindInfo{
		"proto_library": {
			MatchAny: true,
			NonEmptyAttrs: map[string]bool{
				"srcs": true,
			},
			MergeableAttrs: map[string]bool{
				"srcs": true,
			},
			ResolveAttrs: map[string]bool{
				"deps": true,
			},
		},
	}
}

func (*fakeLang) GenerateRules(args v3language.GenerateArgs) v3language.GenerateResult {
	var gen []*rule.Rule
	var imports []interface{}
	for _, file := range args.PackageDir.RegularFiles() {
		name := file.Name()
		if !strings.HasSuffix(name, ".foo") {
			continue
		}
		result, err := file.GetModel("fake/model")
		if err != nil {
			panic(err)
		}
		model := result.Model.(fakeModel)
		r := rule.NewRule("proto_library", strings.TrimSuffix(name, ".foo")+"_proto")
		r.SetAttr("srcs", []string{name})
		r.SetPrivateAttr("fake:export", model.Export)
		gen = append(gen, r)
		imports = append(imports, model.Imports)
	}
	return v3language.GenerateResult{Gen: gen, Imports: imports}
}

func (*fakeLang) Imports(args v3language.ImportsArgs) []resolve.ImportSpec {
	export, _ := args.Rule.PrivateAttr("fake:export").(string)
	if export == "" {
		return nil
	}
	return []resolve.ImportSpec{{Lang: "fake", Imp: export}}
}

func (*fakeLang) Resolve(args v3language.ResolveArgs) {
	raw, _ := args.Imports.([]string)
	if len(raw) == 0 {
		return
	}
	var deps []string
	for _, imp := range raw {
		matches := args.Index.FindRulesByImportWithConfig(args.Config, resolve.ImportSpec{Lang: "fake", Imp: imp}, "fake")
		if len(matches) == 0 {
			continue
		}
		deps = append(deps, matches[0].Label.Rel(args.From.Repo, args.From.Pkg).String())
	}
	if len(deps) > 0 {
		args.Rule.SetAttr("deps", deps)
	}
}

type lifecycleLang struct {
	v3language.BaseLang
	doneCalled    bool
	resolveSawDone bool
}

func (*lifecycleLang) Name() string { return "lifecycle" }

func (*lifecycleLang) Kinds() map[string]rule.KindInfo {
	return map[string]rule.KindInfo{
		"lifecycle_rule": {
			MatchAny: true,
			NonEmptyAttrs: map[string]bool{
				"srcs": true,
			},
			MergeableAttrs: map[string]bool{
				"srcs": true,
			},
		},
	}
}

func (*lifecycleLang) ApparentLoads(moduleToApparentName func(string) string) []rule.LoadInfo {
	repo := moduleToApparentName("gazelle")
	if repo == "" {
		repo = "bazel_gazelle"
	}
	return []rule.LoadInfo{{
		Name: "@" + repo + "//:defs.bzl",
		Symbols: []string{"lifecycle_rule"},
	}}
}

func (l *lifecycleLang) GenerateRules(args v3language.GenerateArgs) v3language.GenerateResult {
	if args.Rel != "" {
		return v3language.GenerateResult{}
	}
	r := rule.NewRule("lifecycle_rule", "root")
	r.SetAttr("srcs", []string{"BUILD.bazel"})
	return v3language.GenerateResult{
		Gen:     []*rule.Rule{r},
		Imports: []interface{}{nil},
	}
}

func (l *lifecycleLang) DoneGeneratingRules() {
	l.doneCalled = true
}

func (l *lifecycleLang) Resolve(args v3language.ResolveArgs) {
	l.resolveSawDone = l.doneCalled
}

func TestRunGeneratesIndexesAndResolvesWholeRepo(t *testing.T) {
	root := t.TempDir()
	writeRunFile(t, filepath.Join(root, "a", "a.foo"), "export a\n")
	writeRunFile(t, filepath.Join(root, "b", "b.foo"), "export b\nimport a\n")

	cfg := config.New()
	cfg.RepoRoot = root
	cfg.RepoName = "repo"
	cfg.ValidBuildFileNames = []string{"BUILD.bazel", "BUILD"}
	cfg.IndexLibraries = true

	emitted := make(map[string]string)
	result, err := Run(Options{
		Config:    cfg,
		Languages: []v3language.Language{&fakeLang{}},
		Emit: func(c *config.Config, f *rule.File) error {
			f.Sync()
			emitted[f.Pkg] = string(f.Format())
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.Cache == nil || result.Snapshot == nil {
		t.Fatal("expected frozen cache")
	}

	if got := emitted["a"]; !strings.Contains(got, "name = \"a_proto\"") {
		t.Fatalf("package a output missing rule:\n%s", got)
	}
	gotB := emitted["b"]
	if !strings.Contains(gotB, "name = \"b_proto\"") {
		t.Fatalf("package b output missing rule:\n%s", gotB)
	}
	if !strings.Contains(gotB, "\"//a:a_proto\"") && !strings.Contains(gotB, "\"//a\"") && !strings.Contains(gotB, "\"../a:a_proto\"") {
		t.Fatalf("package b output missing dep on a:\n%s", gotB)
	}
}

func TestRunWithGoLanguageGeneratesAndResolves(t *testing.T) {
	root := t.TempDir()
	writeRunFile(t, filepath.Join(root, "BUILD.bazel"), "# gazelle:prefix example.com/repo\n")
	writeRunFile(t, filepath.Join(root, "a", "a.go"), "package a\n")
	writeRunFile(t, filepath.Join(root, "b", "b.go"), "package b\nimport _ \"example.com/repo/a\"\n")

	cfg := config.New()
	cfg.RepoRoot = root
	cfg.RepoName = "repo"
	cfg.ValidBuildFileNames = []string{"BUILD.bazel", "BUILD"}
	cfg.IndexLibraries = true

	emitted := make(map[string]string)
	_, err := Run(Options{
		Config:    cfg,
		Languages: []v3language.Language{golang.NewLanguage()},
		Emit: func(c *config.Config, f *rule.File) error {
			f.Sync()
			emitted[f.Pkg] = string(f.Format())
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	gotA := emitted["a"]
	if !strings.Contains(gotA, "go_library(") {
		t.Fatalf("package a output missing go_library:\n%s", gotA)
	}
	if !strings.Contains(gotA, "importpath = \"example.com/repo/a\"") {
		t.Fatalf("package a output missing importpath:\n%s", gotA)
	}

	gotB := emitted["b"]
	if !strings.Contains(gotB, "go_library(") {
		t.Fatalf("package b output missing go_library:\n%s", gotB)
	}
	if !strings.Contains(gotB, "\"//a") && !strings.Contains(gotB, "\"../a") {
		t.Fatalf("package b output missing dep on a:\n%s", gotB)
	}
}

func TestRunCallsDoneGeneratingRulesBeforeResolveAndUsesApparentLoads(t *testing.T) {
	root := t.TempDir()
	writeRunFile(t, filepath.Join(root, "MODULE.bazel"), "module(name = \"repo\")\n")
	writeRunFile(t, filepath.Join(root, "BUILD.bazel"), "")

	cfg := config.New()
	cfg.RepoRoot = root
	cfg.RepoName = "repo"
	cfg.ValidBuildFileNames = []string{"BUILD.bazel", "BUILD"}
	cfg.IndexLibraries = true
	cfg.ModuleToApparentName = func(module string) string {
		if module == "gazelle" {
			return "custom_gazelle"
		}
		return ""
	}

	lang := &lifecycleLang{}
	emitted := make(map[string]string)
	_, err := Run(Options{
		Config:    cfg,
		Languages: []v3language.Language{lang},
		Emit: func(c *config.Config, f *rule.File) error {
			f.Sync()
			emitted[f.Pkg] = string(f.Format())
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !lang.doneCalled {
		t.Fatal("expected DoneGeneratingRules to be called")
	}
	if !lang.resolveSawDone {
		t.Fatal("expected Resolve to run after DoneGeneratingRules")
	}
	if got := emitted[""]; !strings.Contains(got, "@custom_gazelle//:defs.bzl") {
		t.Fatalf("expected apparent load label in output, got:\n%s", got)
	}
}

func TestRunWithGoLanguageUsesLocalRepoDeclarationsForExternalDeps(t *testing.T) {
	root := t.TempDir()
	writeRunFile(t, filepath.Join(root, "WORKSPACE"), `
load("@bazel_gazelle//:deps.bzl", "go_repository")

go_repository(
    name = "com_example_ext_pkg",
    importpath = "example.com/ext/pkg",
)
`)
	writeRunFile(t, filepath.Join(root, "BUILD.bazel"), "# gazelle:prefix example.com/repo\n")
	writeRunFile(t, filepath.Join(root, "b", "b.go"), "package b\nimport _ \"example.com/ext/pkg\"\n")

	cfg := config.New()
	cfg.RepoRoot = root
	cfg.RepoName = "repo"
	cfg.ValidBuildFileNames = []string{"BUILD.bazel", "BUILD"}
	cfg.IndexLibraries = true

	emitted := make(map[string]string)
	_, err := Run(Options{
		Config:    cfg,
		Languages: []v3language.Language{golang.NewLanguage()},
		Emit: func(c *config.Config, f *rule.File) error {
			f.Sync()
			emitted[f.Pkg] = string(f.Format())
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	gotB := emitted["b"]
	if !strings.Contains(gotB, "@com_example_ext_pkg//") {
		t.Fatalf("package b output missing external dep from repo declarations:\n%s", gotB)
	}
}

func TestRunSkipsAlgorithmWhenPatchedSnapshotIsUnchanged(t *testing.T) {
	root := t.TempDir()
	writeRunFile(t, filepath.Join(root, "BUILD.bazel"), "# gazelle:prefix example.com/repo\n")
	writeRunFile(t, filepath.Join(root, "a", "a.go"), "package a\n")

	cfg := config.New()
	cfg.RepoRoot = root
	cfg.RepoName = "repo"
	cfg.ValidBuildFileNames = []string{"BUILD.bazel", "BUILD"}
	cfg.IndexLibraries = true

	first, err := Run(Options{
		Config:    cfg,
		Languages: []v3language.Language{golang.NewLanguage()},
	})
	if err != nil {
		t.Fatal(err)
	}

	emits := 0
	second, err := Run(Options{
		Config:    cfg,
		Languages: []v3language.Language{golang.NewLanguage()},
		Snapshot:  first.Snapshot,
		Changes:   []vfs.Change{{Path: "a/a.go", Kind: vfs.ChangeModify}},
		Emit: func(c *config.Config, f *rule.File) error {
			emits++
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Snapshot != first.Snapshot {
		t.Fatal("expected unchanged rerun to reuse prior snapshot")
	}
	if emits != 0 {
		t.Fatalf("expected no emit on unchanged rerun, got %d", emits)
	}
}

func writeRunFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
