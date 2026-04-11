package run

import (
	"flag"
	"fmt"
	"log"
	"path/filepath"
	"strings"

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
	pkgRel         string
	config         *config.Config
	rules          []*rule.Rule
	imports        []interface{}
	empty          []*rule.Rule
	file           *rule.File
	mappedKinds    []config.MappedKind
	mappedKindInfo map[string]rule.KindInfo
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
	configurers = append(configurers, &v3walk.Configurer{})
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
		mrslv.AliasedKinds(args.Rel, args.Config.AliasMap)
		if !args.Update {
			if args.Config.IndexLibraries && args.File != nil {
				for _, r := range args.File.Rules {
					ruleIndex.AddRule(args.Config, r, args.File)
				}
			}
			return nil
		}
		if args.File != nil {
			for _, lang := range active {
				lang.Fix(args.Config, args.File)
			}
		}

		var empty, gen []*rule.Rule
		var imports []interface{}
		for _, lang := range active {
			res := lang.GenerateRules(v3language.GenerateArgs{
				Config:     args.Config,
				Repo:       args.Repo,
				PackageDir: args.PackageDir,
				Dir:        args.Dir,
				Rel:        args.Rel,
				File:       args.File,
				GenFiles:   args.GenFiles,
				OtherEmpty: empty,
				OtherGen:   gen,
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

		var (
			mappedKinds    []config.MappedKind
			mappedKindInfo = make(map[string]rule.KindInfo)
		)
		var allRules []*rule.Rule
		allRules = append(allRules, gen...)
		if f != nil {
			allRules = append(allRules, f.Rules...)
		}
		maybeRecordReplacement := func(ruleKind string) (*string, error) {
			repl, err := lookupMapKindReplacement(args.Config.KindMap, ruleKind)
			if err != nil {
				return nil, err
			}
			if repl == nil {
				return nil, nil
			}
			mappedKindInfo[repl.KindName] = kinds[ruleKind]
			mappedKinds = append(mappedKinds, *repl)
			mrslv.MappedKind(args.Rel, *repl)
			return &repl.KindName, nil
		}
		for _, r := range allRules {
			if replacementName, err := maybeRecordReplacement(r.Kind()); err != nil {
				return fmt.Errorf("looking up mapped kind: %w", err)
			} else if replacementName != nil {
				r.SetKind(*replacementName)
			}
		}
		for _, r := range empty {
			if repl, ok := args.Config.KindMap[r.Kind()]; ok {
				mappedKindInfo[repl.KindName] = kinds[r.Kind()]
				mappedKinds = append(mappedKinds, repl)
				mrslv.MappedKind(args.Rel, repl)
				r.SetKind(repl.KindName)
			}
		}

		if f == nil {
			f = rule.EmptyFile(filepath.Join(args.Dir, args.Config.DefaultBuildFileName()), args.Rel)
			for _, r := range gen {
				r.Insert(f)
			}
		} else {
			merger.MergeFile(f, empty, gen, merger.PreResolve, unionKindInfoMaps(kinds, mappedKindInfo), args.Config.AliasMap)
		}

		visits = append(visits, visitRecord{
			pkgRel:         args.Rel,
			config:         args.Config,
			rules:          gen,
			imports:        imports,
			empty:          empty,
			file:           f,
			mappedKinds:    mappedKinds,
			mappedKindInfo: mappedKindInfo,
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
		merger.MergeFile(v.file, v.empty, v.rules, merger.PostResolve, unionKindInfoMaps(kinds, v.mappedKindInfo), v.config.AliasMap)
	}

	for _, v := range visits {
		merger.FixLoads(v.file, applyKindMappings(v.mappedKinds, loads))
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
	builtins     map[string]resolve.Resolver
	mappedKinds  map[string][]config.MappedKind
	aliasedKinds map[string]map[string]string
}

func newMetaResolver(repo *vfs.Snapshot) *metaResolver {
	return &metaResolver{
		builtins:     make(map[string]resolve.Resolver),
		mappedKinds:  make(map[string][]config.MappedKind),
		aliasedKinds: make(map[string]map[string]string),
	}
}

func (mr *metaResolver) Add(kind string, resolver resolve.Resolver) {
	mr.builtins[kind] = resolver
}

func (mr *metaResolver) MappedKind(pkgRel string, kind config.MappedKind) {
	mr.mappedKinds[pkgRel] = append(mr.mappedKinds[pkgRel], kind)
}

func (mr *metaResolver) AliasedKinds(pkgRel string, aliasedKinds map[string]string) {
	mr.aliasedKinds[pkgRel] = aliasedKinds
}

func (mr *metaResolver) Resolver(r *rule.Rule, pkgRel string) resolve.Resolver {
	ruleKind := r.Kind()
	if wrappedKind, ok := mr.aliasedKinds[pkgRel][ruleKind]; ok {
		ruleKind = wrappedKind
	}
	for _, mappedKind := range mr.mappedKinds[pkgRel] {
		if mappedKind.KindName == ruleKind {
			ruleKind = mappedKind.FromKind
			break
		}
	}
	if ruleKind != r.Kind() {
		fromKindResolver := mr.builtins[ruleKind]
		if fromKindResolver == nil {
			return nil
		}
		return inverseMapKindResolver{
			fromKind: ruleKind,
			delegate: fromKindResolver,
		}
	}
	return mr.builtins[ruleKind]
}

type inverseMapKindResolver struct {
	fromKind string
	delegate resolve.Resolver
}

func (imkr inverseMapKindResolver) Name() string { return imkr.delegate.Name() }
func (imkr inverseMapKindResolver) Imports(c *config.Config, r *rule.Rule, f *rule.File) []resolve.ImportSpec {
	r = imkr.inverseMapKind(r)
	return imkr.delegate.Imports(c, r, f)
}
func (imkr inverseMapKindResolver) Embeds(r *rule.Rule, from label.Label) []label.Label {
	r = imkr.inverseMapKind(r)
	return imkr.delegate.Embeds(r, from)
}
func (imkr inverseMapKindResolver) Resolve(c *config.Config, ix *resolve.RuleIndex, rc *repo.RemoteCache, r *rule.Rule, imports interface{}, from label.Label) {
	r = imkr.inverseMapKind(r)
	imkr.delegate.Resolve(c, ix, rc, r, imports, from)
}
func (imkr inverseMapKindResolver) inverseMapKind(r *rule.Rule) *rule.Rule {
	rCopy := *r
	rCopy.SetKind(imkr.fromKind)
	return &rCopy
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

func lookupMapKindReplacement(kindMap map[string]config.MappedKind, kind string) (*config.MappedKind, error) {
	var mapped *config.MappedKind
	seenKinds := make(map[string]struct{})
	seenKindPath := []string{kind}
	for {
		replacement, ok := kindMap[kind]
		if !ok {
			break
		}
		seenKindPath = append(seenKindPath, replacement.KindName)
		if _, alreadySeen := seenKinds[replacement.KindName]; alreadySeen {
			return nil, fmt.Errorf("found loop of map_kind replacements: %s", strings.Join(seenKindPath, " -> "))
		}
		seenKinds[replacement.KindName] = struct{}{}
		mapped = &replacement
		if kind == replacement.KindName {
			break
		}
		kind = replacement.KindName
	}
	return mapped, nil
}

func unionKindInfoMaps(a, b map[string]rule.KindInfo) map[string]rule.KindInfo {
	if len(a) == 0 {
		return b
	}
	if len(b) == 0 {
		return a
	}
	result := make(map[string]rule.KindInfo, len(a)+len(b))
	for k, v := range a {
		result[k] = v
	}
	for k, v := range b {
		result[k] = v
	}
	return result
}

func applyKindMappings(mappedKinds []config.MappedKind, loads []rule.LoadInfo) []rule.LoadInfo {
	if len(mappedKinds) == 0 {
		return loads
	}
	mappedLoads := make([]rule.LoadInfo, len(loads))
	copy(mappedLoads, loads)
	for _, mappedKind := range mappedKinds {
		mappedLoads = appendOrMergeKindMapping(mappedLoads, mappedKind)
	}
	return mappedLoads
}

func appendOrMergeKindMapping(mappedLoads []rule.LoadInfo, mappedKind config.MappedKind) []rule.LoadInfo {
	for i, load := range mappedLoads {
		if load.Name == mappedKind.KindLoad {
			mappedLoads[i].Symbols = append(load.Symbols, mappedKind.KindName)
			return mappedLoads
		}
	}
	return append(mappedLoads, rule.LoadInfo{
		Name:    mappedKind.KindLoad,
		Symbols: []string{mappedKind.KindName},
	})
}
