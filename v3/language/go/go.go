package golang

import (
	"encoding/json"
	"flag"
	goparser "go/parser"
	"go/token"
	"strings"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/label"
	oldlanguage "github.com/bazelbuild/bazel-gazelle/language"
	oldgo "github.com/bazelbuild/bazel-gazelle/language/go"
	"github.com/bazelbuild/bazel-gazelle/resolve"
	"github.com/bazelbuild/bazel-gazelle/rule"
	"github.com/bazelbuild/bazel-gazelle/v3/internal/vfs"
	v3language "github.com/bazelbuild/bazel-gazelle/v3/language"
)

type language struct {
	delegate oldlanguage.Language
}

func NewLanguage() v3language.Language {
	return &language{delegate: oldgo.NewLanguage()}
}

func (l *language) RegisterFlags(fs *flag.FlagSet, cmd string, c *config.Config) {
	l.delegate.RegisterFlags(fs, cmd, c)
}
func (l *language) CheckFlags(fs *flag.FlagSet, c *config.Config) error {
	return l.delegate.CheckFlags(fs, c)
}
func (l *language) KnownDirectives() []string { return l.delegate.KnownDirectives() }
func (l *language) Configure(c *config.Config, rel string, f *rule.File) {
	l.delegate.Configure(c, rel, f)
}
func (l *language) Name() string { return l.delegate.Name() }
func (l *language) RegisterParsers(reg *vfs.Registry) error {
	return reg.Register(goFileParser{}, vfs.MatchExtension(".go"))
}
func (l *language) Kinds() map[string]rule.KindInfo { return l.delegate.Kinds() }
func (l *language) GenerateRules(args v3language.GenerateArgs) v3language.GenerateResult {
	res := l.delegate.GenerateRules(oldlanguage.GenerateArgs{
		Config:       args.Config,
		Dir:          args.Dir,
		Rel:          args.Rel,
		File:         args.File,
		Subdirs:      args.Subdirs,
		RegularFiles: args.RegularFiles,
		GenFiles:     args.GenFiles,
		OtherEmpty:   args.OtherEmpty,
		OtherGen:     args.OtherGen,
	})
	return v3language.GenerateResult{
		Gen:         res.Gen,
		Empty:       res.Empty,
		Imports:     res.Imports,
		RelsToIndex: res.RelsToIndex,
	}
}
func (l *language) Loads() []rule.LoadInfo             { return l.delegate.Loads() }
func (l *language) Fix(c *config.Config, f *rule.File) { l.delegate.Fix(c, f) }
func (l *language) Imports(args v3language.ImportsArgs) []resolve.ImportSpec {
	return l.delegate.Imports(args.Config, args.Rule, args.File)
}
func (l *language) Embeds(args v3language.EmbedsArgs) []label.Label {
	return l.delegate.Embeds(args.Rule, args.From)
}
func (l *language) Resolve(args v3language.ResolveArgs) {
	l.delegate.Resolve(args.Config, args.Index, args.Remote, args.Rule, args.Imports, args.From)
}
func (l *language) CrossResolve(args v3language.CrossResolveArgs) []resolve.FindResult {
	cr, ok := l.delegate.(resolve.CrossResolver)
	if !ok {
		return nil
	}
	return cr.CrossResolve(args.Config, args.Index, args.Import, args.Lang)
}

var _ v3language.Language = (*language)(nil)

type goParsedFile struct {
	Package string   `json:"package"`
	Imports []string `json:"imports"`
}

type goFileParser struct{}

func (goFileParser) Key() string     { return "go/fileinfo-lite" }
func (goFileParser) Version() string { return "v1" }
func (goFileParser) Parse(path string, data []byte) (any, error) {
	fset := token.NewFileSet()
	file, err := goparser.ParseFile(fset, path, data, goparser.ImportsOnly)
	if err != nil {
		return goParsedFile{}, nil
	}
	model := goParsedFile{Package: file.Name.Name}
	for _, imp := range file.Imports {
		model.Imports = append(model.Imports, strings.Trim(imp.Path.Value, `"`))
	}
	return model, nil
}
func (goFileParser) Encode(model any) ([]byte, error) { return json.Marshal(model) }
func (goFileParser) Decode(data []byte) (any, error) {
	var model goParsedFile
	err := json.Unmarshal(data, &model)
	return model, err
}
