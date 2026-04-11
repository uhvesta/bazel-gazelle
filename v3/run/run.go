package run

import (
	"flag"
	"fmt"
	"log"
	"path/filepath"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/label"
	"github.com/bazelbuild/bazel-gazelle/merger"
	"github.com/bazelbuild/bazel-gazelle/repo"
	"github.com/bazelbuild/bazel-gazelle/resolve"
	"github.com/bazelbuild/bazel-gazelle/rule"
	"github.com/bazelbuild/bazel-gazelle/v3/internal/vfs"
	v3language "github.com/bazelbuild/bazel-gazelle/v3/language"
	v3walk "github.com/bazelbuild/bazel-gazelle/v3/walk"
)

type Options struct {
	Config      *config.Config
	Languages   []v3language.Language
	Configurers []config.Configurer
	Cache       *vfs.Cache
	Emit        func(*config.Config, *rule.File) error
	Repos       []repo.Repo
}

type visitRecord struct {
	pkgRel  string
	config  *config.Config
	rules   []*rule.Rule
	imports []interface{}
	empty   []*rule.Rule
	file    *rule.File
}

var genericLoads = []rule.LoadInfo{
	{
		Name:    "@bazel_gazelle//:def.bzl",
		Symbols: []string{"gazelle"},
	},
}

func Run(opts Options) (*vfs.Cache, error) {
	if opts.Config == nil {
		return nil, fmt.Errorf("nil config")
	}
	if opts.Config.RepoRoot == "" {
		return nil, fmt.Errorf("config repo root must not be empty")
	}

	configurers := append([]config.Configurer{}, opts.Configurers...)
	configurers = append(configurers, &resolve.Configurer{})
	for _, lang := range opts.Languages {
		configurers = append(configurers, lang)
	}
	if err := initConfigurers(opts.Config, configurers); err != nil {
		return nil, err
	}

	registry := vfs.NewRegistry()
	if err := v3language.RegisterParsers(registry, opts.Languages); err != nil {
		return nil, err
	}

	buildRepo, err := vfs.Build(opts.Config.RepoRoot, vfs.BuildOptions{
		Cache:    opts.Cache,
		Registry: registry,
	})
	if err != nil {
		return nil, err
	}
	if err := primeParsers(buildRepo); err != nil {
		return nil, err
	}
	repoSnapshot := buildRepo.Freeze()

	kinds := make(map[string]rule.KindInfo)
	loads := append([]rule.LoadInfo(nil), genericLoads...)
	mrslv := newMetaResolver(repoSnapshot)
	exts := make([]interface{}, 0, len(opts.Languages))
	for _, lang := range opts.Languages {
		adapter := languageAdapter{repo: repoSnapshot, lang: lang}
		for kind, info := range lang.Kinds() {
			kinds[kind] = info
			mrslv.Add(kind, adapter)
		}
		loads = append(loads, lang.Loads()...)
		exts = append(exts, adapter)
	}
	ruleIndex := resolve.NewRuleIndex(mrslv.Resolver, exts...)

	var visits []visitRecord
	err = v3walk.Walk(repoSnapshot, opts.Config, configurers, func(args v3walk.FuncArgs) error {
		active := v3language.Filter(args.Config, opts.Languages)
		if args.File != nil {
			for _, lang := range active {
				lang.Fix(args.Config, args.File)
			}
		}

		var empty, gen []*rule.Rule
		var imports []interface{}
		for _, lang := range active {
			res := lang.GenerateRules(v3language.GenerateArgs{
				Config:       args.Config,
				Repo:         args.Repo,
				Dir:          args.Dir,
				Rel:          args.Rel,
				File:         args.File,
				Subdirs:      args.Subdirs,
				RegularFiles: args.RegularFiles,
				GenFiles:     args.GenFiles,
				OtherEmpty:   empty,
				OtherGen:     gen,
			})
			if len(res.Gen) != len(res.Imports) {
				return fmt.Errorf("%s: language %s generated %d rules but returned %d imports", args.Rel, lang.Name(), len(res.Gen), len(res.Imports))
			}
			empty = append(empty, res.Empty...)
			gen = append(gen, res.Gen...)
			imports = append(imports, res.Imports...)
		}

		f := args.File
		if f == nil && len(gen) == 0 {
			return nil
		}
		if f == nil {
			f = rule.EmptyFile(filepath.Join(args.Dir, args.Config.DefaultBuildFileName()), args.Rel)
			for _, r := range gen {
				r.Insert(f)
			}
		} else {
			merger.MergeFile(f, empty, gen, merger.PreResolve, kinds, args.Config.AliasMap)
		}

		visits = append(visits, visitRecord{
			pkgRel:  args.Rel,
			config:  args.Config,
			rules:   gen,
			imports: imports,
			empty:   empty,
			file:    f,
		})
		if args.Config.IndexLibraries {
			for _, r := range f.Rules {
				ruleIndex.AddRule(args.Config, r, f)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	if opts.Config.IndexLibraries {
		ruleIndex.Finish()
	}

	rc, cleanup := repo.NewRemoteCache(opts.Repos)
	defer func() {
		if cerr := cleanup(); cerr != nil {
			log.Printf("closing remote cache: %v", cerr)
		}
	}()

	for _, v := range visits {
		for i, r := range v.rules {
			from := label.New(opts.Config.RepoName, v.pkgRel, r.Name())
			if rslv := mrslv.Resolver(r, v.pkgRel); rslv != nil {
				rslv.Resolve(v.config, ruleIndex, rc, r, v.imports[i], from)
			}
		}
		merger.MergeFile(v.file, v.empty, v.rules, merger.PostResolve, kinds, v.config.AliasMap)
	}

	for _, v := range visits {
		merger.FixLoads(v.file, loads)
		if opts.Emit != nil {
			if err := opts.Emit(v.config, v.file); err != nil {
				return nil, err
			}
		}
	}

	return repoSnapshot.Cache(), nil
}

func initConfigurers(c *config.Config, cexts []config.Configurer) error {
	fs := flag.NewFlagSet("v3", flag.ContinueOnError)
	for _, cext := range cexts {
		cext.RegisterFlags(fs, "fix", c)
	}
	for _, cext := range cexts {
		if err := cext.CheckFlags(fs, c); err != nil {
			return err
		}
	}
	return nil
}

func primeParsers(repo *vfs.BuildSnapshot) error {
	for _, file := range repo.Files() {
		for _, parser := range repo.MatchingParsers(file.Path) {
			if _, err := repo.GetModel(file.Path, parser.Key()); err != nil {
				return fmt.Errorf("prime parser %s for %s: %w", parser.Key(), file.Path, err)
			}
		}
	}
	return nil
}

type metaResolver struct {
	builtins map[string]resolve.Resolver
}

func newMetaResolver(repo *vfs.Snapshot) *metaResolver {
	return &metaResolver{
		builtins: make(map[string]resolve.Resolver),
	}
}

func (mr *metaResolver) Add(kind string, resolver resolve.Resolver) {
	mr.builtins[kind] = resolver
}

func (mr *metaResolver) Resolver(r *rule.Rule, pkgRel string) resolve.Resolver {
	return mr.builtins[r.Kind()]
}

type languageAdapter struct {
	repo *vfs.Snapshot
	lang v3language.Language
}

func (a languageAdapter) Name() string { return a.lang.Name() }

func (a languageAdapter) Imports(c *config.Config, r *rule.Rule, f *rule.File) []resolve.ImportSpec {
	return a.lang.Imports(v3language.ImportsArgs{
		Config: c,
		Repo:   a.repo,
		Rule:   r,
		File:   f,
	})
}

func (a languageAdapter) Embeds(r *rule.Rule, from label.Label) []label.Label {
	return a.lang.Embeds(v3language.EmbedsArgs{
		Repo: a.repo,
		Rule: r,
		From: from,
	})
}

func (a languageAdapter) Resolve(c *config.Config, ix *resolve.RuleIndex, rc *repo.RemoteCache, r *rule.Rule, imports interface{}, from label.Label) {
	a.lang.Resolve(v3language.ResolveArgs{
		Config:  c,
		Repo:    a.repo,
		Index:   ix,
		Remote:  rc,
		Rule:    r,
		Imports: imports,
		From:    from,
	})
}

func (a languageAdapter) CrossResolve(c *config.Config, ix *resolve.RuleIndex, imp resolve.ImportSpec, lang string) []resolve.FindResult {
	cr, ok := a.lang.(v3language.CrossResolver)
	if !ok {
		return nil
	}
	return cr.CrossResolve(v3language.CrossResolveArgs{
		Config: c,
		Repo:   a.repo,
		Index:  ix,
		Import: imp,
		Lang:   lang,
	})
}
