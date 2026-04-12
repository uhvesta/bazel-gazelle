package run

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

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
	Prepared    bool
	Timings     bool
	Cache       *vfs.Cache
	Snapshot    *vfs.Snapshot
	Changes     []vfs.Change
	Emit        func(*config.Config, *rule.File) error
	Repos       []repo.Repo
}

type phaseTiming struct {
	name     string
	duration time.Duration
}

type Result struct {
	Snapshot *vfs.Snapshot
	Cache    *vfs.Cache
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

func Run(opts Options) (*Result, error) {
	startTotal := time.Now()
	var timings []phaseTiming
	recordPhase := func(name string, start time.Time) {
		if !opts.Timings {
			return
		}
		timings = append(timings, phaseTiming{name: name, duration: time.Since(start)})
	}
	defer func() {
		if !opts.Timings {
			return
		}
		timings = append(timings, phaseTiming{name: "total", duration: time.Since(startTotal)})
		for _, phase := range timings {
			log.Printf("timing %-16s %s", phase.name, phase.duration)
		}
	}()

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
	if !opts.Prepared {
		phaseStart := time.Now()
		if err := initConfigurers(opts.Config, configurers); err != nil {
			return nil, err
		}
		recordPhase("init_config", phaseStart)
	}
	if err := loadKnownRepositories(opts.Config); err != nil {
		return nil, err
	}

	phaseStart := time.Now()
	registry, err := Registry(opts.Languages)
	if err != nil {
		return nil, err
	}
	recordPhase("register_parsers", phaseStart)

	phaseStart = time.Now()
	var buildRepo *vfs.BuildSnapshot
	if opts.Snapshot != nil {
		changes, fullRebuild, err := v3walk.PromoteTraversalChanges(opts.Snapshot, opts.Config, opts.Changes)
		if err != nil {
			return nil, err
		}
		changes = v3walk.FilterChanges(opts.Snapshot, opts.Config, changes)
		if len(changes) == 0 {
			recordPhase("build_vfs", phaseStart)
			recordPhase("prime_parsers", time.Now())
			recordPhase("freeze_vfs", time.Now())
			recordPhase("prepare_run", time.Now())
			recordPhase("walk_generate", time.Now())
			recordPhase("resolve", time.Now())
			recordPhase("emit", time.Now())
			return &Result{
				Snapshot: opts.Snapshot,
				Cache:    opts.Snapshot.Cache(),
			}, nil
		}
		if fullRebuild {
			buildRepo, err = vfs.Build(opts.Config.RepoRoot, vfs.BuildOptions{
				Cache:    opts.Snapshot.Cache(),
				Registry: registry,
			})
		} else {
			buildRepo, err = vfs.Patch(opts.Config.RepoRoot, opts.Snapshot, vfs.BuildOptions{
				Registry: registry,
			}, changes)
		}
	} else {
		buildRepo, err = vfs.Build(opts.Config.RepoRoot, vfs.BuildOptions{
			Cache:    opts.Cache,
			Registry: registry,
		})
	}
	if err != nil {
		return nil, err
	}
	recordPhase("build_vfs", phaseStart)
	if opts.Snapshot != nil && !buildRepo.Changed() {
		recordPhase("prime_parsers", time.Now())
		recordPhase("freeze_vfs", time.Now())
		recordPhase("prepare_run", time.Now())
		recordPhase("walk_generate", time.Now())
		recordPhase("resolve", time.Now())
		recordPhase("emit", time.Now())
		return &Result{
			Snapshot: opts.Snapshot,
			Cache:    opts.Snapshot.Cache(),
		}, nil
	}

	phaseStart = time.Now()
	if err := primeParsers(buildRepo); err != nil {
		return nil, err
	}
	recordPhase("prime_parsers", phaseStart)

	phaseStart = time.Now()
	repoSnapshot := buildRepo.Freeze()
	recordPhase("freeze_vfs", phaseStart)

	phaseStart = time.Now()
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
	recordPhase("prepare_run", phaseStart)

	var visits []visitRecord
	phaseStart = time.Now()
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
	recordPhase("walk_generate", phaseStart)

	if opts.Config.IndexLibraries {
		phaseStart = time.Now()
		ruleIndex.Finish()
		recordPhase("finish_index", phaseStart)
	}

	phaseStart = time.Now()
	for _, v := range visits {
		var wg sync.WaitGroup
		for i, r := range v.rules {
			wg.Add(1)
			go func(i int, r *rule.Rule) {
				defer wg.Done()
				from := label.New(opts.Config.RepoName, v.pkgRel, r.Name())
				if rslv := mrslv.Resolver(r, v.pkgRel); rslv != nil {
					rslv.Resolve(v.config, ruleIndex, nil, r, v.imports[i], from)
				}
			}(i, r)
		}
		wg.Wait()
		merger.MergeFile(v.file, v.empty, v.rules, merger.PostResolve, unionKindInfoMaps(kinds, v.mappedKindInfo), v.config.AliasMap)
	}
	recordPhase("resolve", phaseStart)

	phaseStart = time.Now()
	for _, v := range visits {
		merger.FixLoads(v.file, applyKindMappings(v.mappedKinds, loads))
		if opts.Emit != nil {
			if err := opts.Emit(v.config, v.file); err != nil {
				return nil, err
			}
		}
	}
	recordPhase("emit", phaseStart)

	return &Result{
		Snapshot: repoSnapshot,
		Cache:    repoSnapshot.Cache(),
	}, nil
}

func Registry(langs []v3language.Language) (*vfs.Registry, error) {
	registry := vfs.NewRegistry()
	if err := v3language.RegisterParsers(registry, langs); err != nil {
		return nil, err
	}
	return registry, nil
}

func vfsDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func loadKnownRepositories(c *config.Config) error {
	repoConfigPath := findRepoConfigPath(c.RepoRoot)
	repoConfigFile, err := rule.LoadWorkspaceFile(repoConfigPath, "")
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	repos, _, err := repo.ListRepositories(repoConfigFile)
	if err != nil {
		return err
	}
	c.Repos = repos
	return nil
}

func findRepoConfigPath(repoRoot string) string {
	candidates := []string{"WORKSPACE.bazel", "WORKSPACE", "REPO.bazel"}
	for _, name := range candidates {
		path := filepath.Join(repoRoot, name)
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path
		}
	}
	return filepath.Join(repoRoot, "WORKSPACE")
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
	type parseJob struct {
		file   vfs.File
		parser vfs.Parser
	}
	type parseResult struct {
		entry vfs.Entry
		err   error
	}

	var jobs []parseJob
	for _, file := range repo.Files() {
		for _, parser := range repo.MatchingParsers(file.Path) {
			if file.Content == nil {
				if _, hit, err := repo.Builder().CheckHash(file.Path, file.Hash, parser); err != nil {
					return fmt.Errorf("check parser %s for %s: %w", parser.Key(), file.Path, err)
				} else if hit {
					continue
				}
				content, err := repo.ReadFile(file.Path)
				if err != nil {
					return fmt.Errorf("read %s for parser %s: %w", file.Path, parser.Key(), err)
				}
				file.Content = content
				file.Hash = vfsDigest(content)
			} else if _, hit, err := repo.Builder().Check(file.Path, file.Content, parser); err != nil {
				return fmt.Errorf("check parser %s for %s: %w", parser.Key(), file.Path, err)
			} else if hit {
				continue
			}
			jobs = append(jobs, parseJob{file: file, parser: parser})
		}
	}
	if len(jobs) == 0 {
		return nil
	}

	workerCount := runtime.GOMAXPROCS(0)
	if workerCount < 1 {
		workerCount = 1
	}
	if workerCount > len(jobs) {
		workerCount = len(jobs)
	}

	jobCh := make(chan parseJob)
	resultCh := make(chan parseResult, workerCount)
	var wg sync.WaitGroup
	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobCh {
				model, err := job.parser.Parse(job.file.Path, job.file.Content)
				if err != nil {
					resultCh <- parseResult{err: fmt.Errorf("parse parser %s for %s: %w", job.parser.Key(), job.file.Path, err)}
					continue
				}
				encoded, err := job.parser.Encode(model)
				if err != nil {
					resultCh <- parseResult{err: fmt.Errorf("encode parser %s for %s: %w", job.parser.Key(), job.file.Path, err)}
					continue
				}
				resultCh <- parseResult{
					entry: vfs.Entry{
						Path:          job.file.Path,
						ParserKey:     job.parser.Key(),
						ParserVersion: job.parser.Version(),
						ContentHash:   job.file.Hash,
						ModelHash:     vfsDigest(encoded),
						EncodedModel:  encoded,
					},
				}
			}
		}()
	}
	go func() {
		for _, job := range jobs {
			jobCh <- job
		}
		close(jobCh)
		wg.Wait()
		close(resultCh)
	}()

	for result := range resultCh {
		if result.err != nil {
			return result.err
		}
		repo.Builder().StoreEntry(result.entry)
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
