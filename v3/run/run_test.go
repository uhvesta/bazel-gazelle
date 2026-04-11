package run

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/resolve"
	"github.com/bazelbuild/bazel-gazelle/rule"
	"github.com/bazelbuild/bazel-gazelle/v3/internal/vfs"
	v3language "github.com/bazelbuild/bazel-gazelle/v3/language"
)

type fakeModel struct {
	Export  string   `json:"export"`
	Imports []string `json:"imports"`
}

type fakeParser struct{}

func (*fakeParser) Key() string     { return "fake/model" }
func (*fakeParser) Version() string { return "v1" }
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
	for _, name := range args.RegularFiles {
		if !strings.HasSuffix(name, ".foo") {
			continue
		}
		result, err := args.Repo.GetModel(pathJoin(args.Rel, name), "fake/model")
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
	cache, err := Run(Options{
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
	if cache == nil {
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

func pathJoin(parts ...string) string {
	var filtered []string
	for _, part := range parts {
		if part != "" {
			filtered = append(filtered, part)
		}
	}
	return strings.Join(filtered, "/")
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
