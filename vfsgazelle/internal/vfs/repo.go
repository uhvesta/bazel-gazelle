package vfs

import (
	"bufio"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"

	bzl "github.com/bazelbuild/buildtools/build"
	"github.com/bmatcuk/doublestar/v4"
	"github.com/uhvesta/bazel-gazelle/config"
	"github.com/uhvesta/bazel-gazelle/rule"
)

// PathMatcher reports whether a parser applies to a slash-separated repo path.
type PathMatcher func(path string) bool

// RegisteredParser associates a parser with one or more matchers.
type RegisteredParser struct {
	Parser   Parser
	Matchers []PathMatcher
}

// Registry stores the parsers available to a repo snapshot.
type Registry struct {
	byKey map[string]RegisteredParser
}

// NewRegistry returns an empty parser registry.
func NewRegistry() *Registry {
	return &Registry{byKey: make(map[string]RegisteredParser)}
}

// Register adds a parser to the registry along with the path matchers that
// determine which repo files it can parse.
func (r *Registry) Register(parser Parser, matchers ...PathMatcher) error {
	if parser == nil {
		return fmt.Errorf("nil parser")
	}
	if parser.Key() == "" {
		return fmt.Errorf("parser key must not be empty")
	}
	if _, ok := r.byKey[parser.Key()]; ok {
		return fmt.Errorf("parser already registered: %s", parser.Key())
	}
	r.byKey[parser.Key()] = RegisteredParser{
		Parser:   parser,
		Matchers: append([]PathMatcher(nil), matchers...),
	}
	return nil
}

// Parser returns the parser registered for key.
func (r *Registry) Parser(key string) (Parser, bool) {
	if r == nil {
		return nil, false
	}
	registered, ok := r.byKey[key]
	return registered.Parser, ok
}

// Match returns the parsers whose matchers apply to path.
func (r *Registry) Match(path string) []Parser {
	if r == nil {
		return nil
	}
	var parsers []Parser
	for _, registered := range r.byKey {
		if len(registered.Matchers) == 0 {
			parsers = append(parsers, registered.Parser)
			continue
		}
		for _, match := range registered.Matchers {
			if match != nil && match(path) {
				parsers = append(parsers, registered.Parser)
				break
			}
		}
	}
	sort.Slice(parsers, func(i, j int) bool {
		return parsers[i].Key() < parsers[j].Key()
	})
	return parsers
}

// BuildSnapshot is the mutable build-phase view of the repo.
//
// A coordinator owns the snapshot and its CacheBuilder during the build phase.
// Once parsing is complete, Freeze produces a read-only Snapshot and consumes
// the mutable maps instead of cloning them.
type BuildSnapshot struct {
	Root                string
	builder             *CacheBuilder
	registry            *Registry
	validBuildFileNames []string
	base                *Snapshot
	files               map[string]File
	deletedFiles        map[string]struct{}
	dirs                map[string][]string
	deletedDirs         map[string]struct{}
	changed             bool
}

// Snapshot is the frozen read-only view of the repo and parsed-model cache.
type Snapshot struct {
	Root                string
	cache               *Cache
	registry            *Registry
	validBuildFileNames []string
	base                *Snapshot
	files               map[string]File
	deletedFiles        map[string]struct{}
	dirs                map[string][]string
	deletedDirs         map[string]struct{}
}

// File describes one file stored in a snapshot.
type File struct {
	Path    string
	Content []byte
	Hash    string
}

// Dir is a read-only handle to a directory in a frozen repo snapshot.
type Dir struct {
	repo             *Snapshot
	rel              string
	subdirViews      []*Dir
	regularFileViews []FileRef
	filteredSubdirs  bool
	filteredFiles    bool
}

// FileRef is a lightweight handle to a file in a frozen repo snapshot.
type FileRef struct {
	repo *Snapshot
	rel  string
}

// BuildOptions configures cold builds and snapshot patches.
type BuildOptions struct {
	Cache               *Cache
	Registry            *Registry
	ValidBuildFileNames []string
}

type fileJob struct {
	absPath string
	rel     string
}

type fileResult struct {
	file File
	err  error
}

type traversalConfig struct {
	ignorePaths          map[string]struct{}
	ignoreDirectoryGlobs []string
	excludes             []string
	validBuildFileNames  []string
}

type buildTreeResult struct {
	files map[string]File
	dirs  map[string][]string
}

type childBuildResult struct {
	name   string
	result buildTreeResult
	err    error
}

var buildDirectiveRe = regexp.MustCompile(`^#\s*gazelle:(\w+)\s*(.*?)\s*$`)

// Build constructs a mutable snapshot for root by reading the repo from disk.
func Build(root string, opts BuildOptions) (*BuildSnapshot, error) {
	if root == "" {
		return nil, fmt.Errorf("root must not be empty")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}

	s := &BuildSnapshot{
		Root:                root,
		builder:             NewCacheBuilder(opts.Cache),
		registry:            opts.Registry,
		validBuildFileNames: append([]string(nil), opts.ValidBuildFileNames...),
		files:               make(map[string]File),
		deletedFiles:        make(map[string]struct{}),
		dirs:                make(map[string][]string),
		deletedDirs:         make(map[string]struct{}),
	}
	s.dirs[""] = nil

	cfg, err := newTraversalConfig(root, opts.ValidBuildFileNames)
	if err != nil {
		return nil, err
	}
	result, err := buildTree(root, "", cfg)
	if err != nil {
		return nil, err
	}
	s.files = result.files
	s.dirs = result.dirs
	return s, nil
}

func newTraversalConfig(root string, validBuildFileNames []string) (*traversalConfig, error) {
	if len(validBuildFileNames) == 0 {
		validBuildFileNames = config.DefaultValidBuildFileNames
	}
	ignorePaths, err := loadBazelIgnore(root)
	if err != nil {
		return nil, err
	}
	ignoreDirectoryGlobs, err := loadRepoDirectoryIgnore(root)
	if err != nil {
		return nil, err
	}
	return &traversalConfig{
		ignorePaths:          ignorePaths,
		ignoreDirectoryGlobs: ignoreDirectoryGlobs,
		validBuildFileNames:  append([]string(nil), validBuildFileNames...),
	}, nil
}

func (c *traversalConfig) clone() *traversalConfig {
	if c == nil {
		return nil
	}
	out := *c
	out.excludes = append([]string(nil), c.excludes...)
	out.validBuildFileNames = append([]string(nil), c.validBuildFileNames...)
	return &out
}

func (c *traversalConfig) isExcludedDir(p string) bool {
	if c == nil {
		return false
	}
	if path.Base(p) == ".git" {
		return true
	}
	if _, ok := c.ignorePaths[p]; ok {
		return true
	}
	for _, pat := range c.ignoreDirectoryGlobs {
		if doublestar.MatchUnvalidated(pat, p) {
			return true
		}
	}
	for _, pat := range c.excludes {
		if doublestar.MatchUnvalidated(pat, p) {
			return true
		}
	}
	return false
}

func (c *traversalConfig) isExcludedFile(p string) bool {
	if c == nil {
		return false
	}
	if _, ok := c.ignorePaths[p]; ok {
		return true
	}
	for _, pat := range c.excludes {
		if doublestar.MatchUnvalidated(pat, p) {
			return true
		}
	}
	return false
}

func buildTree(root, rel string, cfg *traversalConfig) (buildTreeResult, error) {
	if cfg != nil && rel != "" && cfg.isExcludedDir(rel) {
		return buildTreeResult{files: map[string]File{}, dirs: map[string][]string{}}, nil
	}
	absDir := filepath.Join(root, filepath.FromSlash(rel))
	entries, err := os.ReadDir(absDir)
	if err != nil {
		return buildTreeResult{}, err
	}
	buildName, buildData, err := loadBuildData(absDir, entries, cfg.validBuildFileNames)
	if err != nil {
		return buildTreeResult{}, err
	}
	nextCfg := cfg.clone()
	if buildName != "" {
		buildRel := cleanRepoPath(path.Join(rel, buildName))
		directives, err := parseBuildDirectives(root, rel, buildRel, buildData)
		if err != nil {
			return buildTreeResult{}, err
		}
		applyTraversalDirectives(nextCfg, rel, buildRel, directives)
	}
	if nextCfg != nil && rel != "" && nextCfg.isExcludedDir(rel) {
		return buildTreeResult{files: map[string]File{}, dirs: map[string][]string{}}, nil
	}

	result := buildTreeResult{
		files: make(map[string]File),
		dirs:  map[string][]string{rel: {}},
	}

	localFiles := make([]fileJob, 0)
	childEntries := make([]fs.DirEntry, 0)
	for _, entry := range entries {
		name := entry.Name()
		entryRel := cleanRepoPath(path.Join(rel, name))
		if entry.IsDir() {
			if nextCfg != nil && nextCfg.isExcludedDir(entryRel) {
				continue
			}
			childEntries = append(childEntries, entry)
			result.dirs[rel] = append(result.dirs[rel], name)
			continue
		}
		if !entry.Type().IsRegular() {
			continue
		}
		if buildName != "" && name == buildName {
			localFiles = append(localFiles, fileJob{absPath: filepath.Join(absDir, name), rel: entryRel})
			result.dirs[rel] = append(result.dirs[rel], name)
			continue
		}
		if nextCfg != nil && nextCfg.isExcludedFile(entryRel) {
			continue
		}
		localFiles = append(localFiles, fileJob{absPath: filepath.Join(absDir, name), rel: entryRel})
		result.dirs[rel] = append(result.dirs[rel], name)
	}

	files, err := readFiles(localFiles)
	if err != nil {
		return buildTreeResult{}, err
	}
	for path, file := range files {
		result.files[path] = file
	}

	if len(childEntries) > 0 {
		workerCount := runtime.GOMAXPROCS(0)
		if workerCount < 1 {
			workerCount = 1
		}
		if workerCount > len(childEntries) {
			workerCount = len(childEntries)
		}
		jobCh := make(chan fs.DirEntry)
		resultCh := make(chan childBuildResult, len(childEntries))
		var wg sync.WaitGroup
		for i := 0; i < workerCount; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for entry := range jobCh {
					childRel := cleanRepoPath(path.Join(rel, entry.Name()))
					childResult, err := buildTree(root, childRel, nextCfg)
					resultCh <- childBuildResult{name: entry.Name(), result: childResult, err: err}
				}
			}()
		}
		go func() {
			for _, entry := range childEntries {
				jobCh <- entry
			}
			close(jobCh)
			wg.Wait()
			close(resultCh)
		}()
		for child := range resultCh {
			if child.err != nil {
				return buildTreeResult{}, child.err
			}
			if len(child.result.dirs) == 0 && len(child.result.files) == 0 {
				result.dirs[rel] = removeString(result.dirs[rel], child.name)
				continue
			}
			for p, f := range child.result.files {
				result.files[p] = f
			}
			for d, ents := range child.result.dirs {
				result.dirs[d] = ents
			}
		}
	}

	for dir := range result.dirs {
		sort.Strings(result.dirs[dir])
	}
	return result, nil
}

func loadBuildData(absDir string, entries []fs.DirEntry, validBuildFileNames []string) (string, []byte, error) {
	for _, name := range validBuildFileNames {
		for _, entry := range entries {
			if entry.IsDir() || entry.Name() != name {
				continue
			}
			data, err := os.ReadFile(filepath.Join(absDir, name))
			if err != nil {
				return "", nil, err
			}
			return name, data, nil
		}
	}
	return "", nil, nil
}

func parseBuildDirectives(root, rel, buildRel string, data []byte) ([]rule.Directive, error) {
	buildPath := filepath.Join(root, filepath.FromSlash(buildRel))
	f, err := rule.LoadData(buildPath, rel, data)
	if err != nil {
		return nil, err
	}
	directives := append([]rule.Directive(nil), f.Directives...)
	for _, d := range f.Directives {
		if d.Key != "directive_file" {
			continue
		}
		directiveRel := cleanRepoPath(path.Join(rel, d.Value))
		directiveData, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(directiveRel)))
		if err != nil {
			return nil, fmt.Errorf("%s: reading directive file %s: %w", buildRel, d.Value, err)
		}
		scanner := bufio.NewScanner(strings.NewReader(string(directiveData)))
		for scanner.Scan() {
			match := buildDirectiveRe.FindStringSubmatch(scanner.Text())
			if match == nil {
				continue
			}
			if match[1] == "directive_file" {
				return nil, fmt.Errorf("%s: directive_file in %s: recursive directive_file is not supported", buildRel, d.Value)
			}
			directives = append(directives, rule.Directive{Key: match[1], Value: match[2]})
		}
		if err := scanner.Err(); err != nil {
			return nil, fmt.Errorf("%s: reading directive file %s: %w", buildRel, d.Value, err)
		}
	}
	return directives, nil
}

func applyTraversalDirectives(cfg *traversalConfig, rel string, buildRel string, directives []rule.Directive) {
	for _, d := range directives {
		switch d.Key {
		case "build_file_name":
			cfg.validBuildFileNames = strings.Split(d.Value, ",")
		case "exclude":
			p := path.Join(rel, d.Value)
			if _, err := doublestar.Match(p, "x"); err != nil {
				log.Printf("the exclusion pattern is not valid %q: %s", p, err)
				continue
			}
			cfg.excludes = append(cfg.excludes, p)
		case "ignore":
			if d.Value != "" {
				log.Printf("the ignore directive does not take any arguments. Did you mean gazelle:exclude? in %s '# gazelle:ignore %s'", buildRel, d.Value)
			}
		case "follow":
			log.Printf("%s: gazelle:follow is not supported in vfsgazelle and will be ignored", buildRel)
		case "generation_mode":
			log.Printf("%s: gazelle:generation_mode is not supported in vfsgazelle and will be ignored", buildRel)
		}
	}
}

func loadBazelIgnore(repoRoot string) (map[string]struct{}, error) {
	file, err := filepath.Abs(filepath.Join(repoRoot, ".bazelignore"))
	if err != nil {
		return nil, err
	}
	f, err := os.Open(file)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf(".bazelignore exists but couldn't be read: %v", err)
	}
	defer f.Close()
	excludes := make(map[string]struct{})
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		ignore := strings.TrimSpace(scanner.Text())
		if ignore == "" || ignore[0] == '#' {
			continue
		}
		if strings.ContainsAny(ignore, "*?[") {
			log.Printf("the .bazelignore exclusion pattern must not be a glob %s", ignore)
			continue
		}
		excludes[path.Clean(ignore)] = struct{}{}
	}
	return excludes, scanner.Err()
}

func loadRepoDirectoryIgnore(repoRoot string) ([]string, error) {
	repoFilePath := path.Join(repoRoot, "REPO.bazel")
	repoFileContent, err := os.ReadFile(repoFilePath)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("REPO.bazel exists but couldn't be read: %v", err)
	}
	ast, err := bzl.Parse(repoRoot, repoFileContent)
	if err != nil {
		return nil, fmt.Errorf("failed to parse REPO.bazel: %v", err)
	}
	var ignoreDirectories []string
	for _, expr := range ast.Stmt {
		call, isCall := expr.(*bzl.CallExpr)
		if !isCall {
			continue
		}
		inv, isIdentCall := call.X.(*bzl.Ident)
		if !isIdentCall || inv.Name != "ignore_directories" {
			continue
		}
		if len(call.List) != 1 {
			return nil, fmt.Errorf("REPO.bazel ignore_directories() expects one argument")
		}
		list, isList := call.List[0].(*bzl.ListExpr)
		if !isList {
			return nil, fmt.Errorf("REPO.bazel ignore_directories() unexpected argument type: %T", call.List[0])
		}
		for _, item := range list.List {
			strExpr, isStr := item.(*bzl.StringExpr)
			if !isStr {
				continue
			}
			if _, err := doublestar.Match(strExpr.Value, "x"); err != nil {
				log.Printf("the ignore_directories() pattern %q is not valid: %s", strExpr.Value, err)
				continue
			}
			ignoreDirectories = append(ignoreDirectories, strExpr.Value)
		}
		break
	}
	return ignoreDirectories, nil
}

func readFiles(jobs []fileJob) (map[string]File, error) {
	files := make(map[string]File, len(jobs))
	if len(jobs) == 0 {
		return files, nil
	}
	workerCount := runtime.GOMAXPROCS(0)
	if workerCount < 1 {
		workerCount = 1
	}
	if workerCount > len(jobs) {
		workerCount = len(jobs)
	}
	jobCh := make(chan fileJob)
	resultCh := make(chan fileResult, workerCount)
	var wg sync.WaitGroup
	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobCh {
				content, err := os.ReadFile(job.absPath)
				if err != nil {
					resultCh <- fileResult{err: err}
					continue
				}
				resultCh <- fileResult{
					file: File{
						Path:    job.rel,
						Content: content,
						Hash:    digest(content),
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
			return nil, result.err
		}
		files[result.file.Path] = result.file
	}
	return files, nil
}

func removeString(values []string, target string) []string {
	out := values[:0]
	for _, v := range values {
		if v != target {
			out = append(out, v)
		}
	}
	return out
}

func readFilesIntoSnapshot(s *BuildSnapshot, jobs []fileJob) error {
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

	jobCh := make(chan fileJob)
	resultCh := make(chan fileResult, workerCount)
	var wg sync.WaitGroup
	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobCh {
				content, err := os.ReadFile(job.absPath)
				if err != nil {
					resultCh <- fileResult{err: err}
					continue
				}
				resultCh <- fileResult{
					file: File{
						Path:    job.rel,
						Content: content,
						Hash:    digest(content),
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
		s.files[result.file.Path] = result.file
	}
	return nil
}

// Freeze consumes the mutable build snapshot and returns a frozen read-only
// snapshot that can be shared across the rest of the vfsgazelle pipeline.
func (s *BuildSnapshot) Freeze() *Snapshot {
	if s == nil {
		return &Snapshot{}
	}
	cache := s.builder.Freeze()
	files := s.files
	dirs := s.dirs
	s.builder = nil
	s.files = nil
	s.dirs = nil
	return &Snapshot{
		Root:                s.Root,
		cache:               cache,
		registry:            s.registry,
		validBuildFileNames: append([]string(nil), s.validBuildFileNames...),
		base:                s.base,
		files:               files,
		deletedFiles:        s.deletedFiles,
		dirs:                dirs,
		deletedDirs:         s.deletedDirs,
	}
}

// Builder returns the mutable parsed-model cache owned by the build snapshot.
func (s *BuildSnapshot) Builder() *CacheBuilder {
	return s.builder
}

// Changed reports whether the snapshot contents changed during a patch.
func (s *BuildSnapshot) Changed() bool {
	if s == nil {
		return false
	}
	return s.changed
}

// Files returns a stable snapshot of all build-phase files.
func (s *BuildSnapshot) Files() []File {
	files := make([]File, 0, len(s.filePaths()))
	for _, filePath := range s.filePaths() {
		file, ok := s.lookupFile(filePath)
		if !ok {
			continue
		}
		files = append(files, cloneFile(file))
	}
	return files
}

// ForEachFile visits each visible build-phase file in stable repo-relative order.
func (s *BuildSnapshot) ForEachFile(fn func(File) error) error {
	for _, filePath := range s.filePaths() {
		file, ok := s.lookupFile(filePath)
		if !ok {
			continue
		}
		if err := fn(file); err != nil {
			return err
		}
	}
	return nil
}

// ReadFile returns the current content for path from the build snapshot.
func (s *BuildSnapshot) ReadFile(path string) ([]byte, error) {
	file, ok := s.lookupFile(cleanRepoPath(path))
	if !ok {
		return nil, os.ErrNotExist
	}
	if file.Content == nil {
		return os.ReadFile(filepath.Join(s.Root, filepath.FromSlash(file.Path)))
	}
	return append([]byte(nil), file.Content...), nil
}

// File returns the file metadata stored for path in the build snapshot.
func (s *BuildSnapshot) File(path string) (File, bool) {
	file, ok := s.lookupFile(cleanRepoPath(path))
	if !ok {
		return File{}, false
	}
	return cloneFile(file), true
}

// ListDir returns the immediate entries in dir.
func (s *BuildSnapshot) ListDir(dir string) ([]string, error) {
	entries, ok := s.lookupDir(cleanRepoPath(dir))
	if !ok {
		return nil, os.ErrNotExist
	}
	return append([]string(nil), entries...), nil
}

// MatchingParsers returns the registered parsers that apply to path.
func (s *BuildSnapshot) MatchingParsers(path string) []Parser {
	return s.registry.Match(cleanRepoPath(path))
}

// GetModel returns a parser-backed semantic model for path during the build
// phase, parsing on demand when the cache cannot be reused.
func (s *BuildSnapshot) GetModel(path, parserKey string) (LookupResult, error) {
	file, ok := s.lookupFile(cleanRepoPath(path))
	if !ok {
		return LookupResult{}, os.ErrNotExist
	}
	parser, ok := s.registry.Parser(parserKey)
	if !ok {
		return LookupResult{}, fmt.Errorf("parser not registered: %s", parserKey)
	}
	if file.Content == nil {
		if result, hit, err := s.builder.CheckHash(file.Path, file.Hash, parser); err != nil {
			return LookupResult{}, err
		} else if hit {
			return result, nil
		}
		content, err := os.ReadFile(filepath.Join(s.Root, filepath.FromSlash(file.Path)))
		if err != nil {
			return LookupResult{}, err
		}
		file.Content = content
		file.Hash = digest(content)
		s.files[file.Path] = file
		delete(s.deletedFiles, file.Path)
	}
	return s.builder.Parse(file.Path, file.Content, parser)
}

// ReadFile returns the frozen content for path.
func (s *Snapshot) ReadFile(path string) ([]byte, error) {
	file, ok := s.lookupFile(cleanRepoPath(path))
	if !ok {
		return nil, os.ErrNotExist
	}
	if file.Content == nil {
		return os.ReadFile(filepath.Join(s.Root, filepath.FromSlash(file.Path)))
	}
	return append([]byte(nil), file.Content...), nil
}

// Files returns a stable snapshot of all frozen files.
func (s *Snapshot) Files() []File {
	files := make([]File, 0, len(s.filePaths()))
	for _, filePath := range s.filePaths() {
		file, ok := s.lookupFile(filePath)
		if !ok {
			continue
		}
		files = append(files, cloneFile(file))
	}
	return files
}

// Cache returns the frozen parsed-model cache for the snapshot.
func (s *Snapshot) Cache() *Cache {
	return s.cache
}

// Dir returns a directory handle for rel.
func (s *Snapshot) Dir(rel string) (*Dir, bool) {
	rel = cleanRepoPath(rel)
	if _, ok := s.lookupDir(rel); !ok {
		return nil, false
	}
	return &Dir{repo: s, rel: rel}, true
}

// DirView returns a directory handle with explicitly filtered child views.
// Walk uses this to present package-local file membership after excludes and
// ignores have been applied.
func (s *Snapshot) DirView(rel string, subdirs []string, regularFiles []string) (*Dir, bool) {
	base, ok := s.Dir(rel)
	if !ok {
		return nil, false
	}
	if subdirs != nil {
		base.filteredSubdirs = true
		base.subdirViews = make([]*Dir, 0, len(subdirs))
		for _, name := range subdirs {
			child, ok := s.Dir(path.Join(rel, name))
			if ok {
				base.subdirViews = append(base.subdirViews, child)
			}
		}
	}
	if regularFiles != nil {
		base.filteredFiles = true
		base.regularFileViews = make([]FileRef, 0, len(regularFiles))
		for _, name := range regularFiles {
			fileRel := cleanRepoPath(path.Join(rel, name))
			if _, ok := s.lookupFile(fileRel); ok {
				base.regularFileViews = append(base.regularFileViews, FileRef{repo: s, rel: fileRel})
			}
		}
	}
	return base, true
}

// File returns the file metadata stored for path in the frozen snapshot.
func (s *Snapshot) File(path string) (File, bool) {
	file, ok := s.lookupFile(cleanRepoPath(path))
	if !ok {
		return File{}, false
	}
	return cloneFile(file), true
}

// ListDir returns the immediate entries in dir from the frozen snapshot.
func (s *Snapshot) ListDir(dir string) ([]string, error) {
	entries, ok := s.lookupDir(cleanRepoPath(dir))
	if !ok {
		return nil, os.ErrNotExist
	}
	return append([]string(nil), entries...), nil
}

// MatchingParsers returns the registered parsers that apply to path.
func (s *Snapshot) MatchingParsers(path string) []Parser {
	return s.registry.Match(cleanRepoPath(path))
}

// GetModel returns a parser-backed semantic model for path from the frozen
// snapshot. Frozen snapshots never parse; they only decode cached models.
func (s *Snapshot) GetModel(path, parserKey string) (LookupResult, error) {
	file, ok := s.lookupFile(cleanRepoPath(path))
	if !ok {
		return LookupResult{}, os.ErrNotExist
	}
	parser, ok := s.registry.Parser(parserKey)
	if !ok {
		return LookupResult{}, fmt.Errorf("parser not registered: %s", parserKey)
	}
	var (
		result LookupResult
		hit    bool
		err    error
	)
	if file.Content == nil {
		result, hit, err = s.cache.GetHash(file.Path, file.Hash, parser)
	} else {
		result, hit, err = s.cache.Get(file.Path, file.Content, parser)
	}
	if err != nil {
		return LookupResult{}, err
	}
	if !hit {
		return LookupResult{}, fmt.Errorf("frozen cache miss for %s with parser %s", file.Path, parserKey)
	}
	return result, nil
}

// MatchExtension returns a matcher that applies to files ending in ext.
func MatchExtension(ext string) PathMatcher {
	return func(path string) bool {
		return strings.HasSuffix(path, ext)
	}
}

// MatchBasename returns a matcher that applies to files whose base name is name.
func MatchBasename(name string) PathMatcher {
	return func(path string) bool {
		return pathBase(path) == name
	}
}

func cleanRepoPath(p string) string {
	if p == "" || p == "." {
		return ""
	}
	p = filepath.ToSlash(p)
	p = path.Clean(p)
	if p == "." {
		return ""
	}
	return strings.TrimPrefix(p, "./")
}

func pathBase(p string) string {
	if p == "" {
		return ""
	}
	return path.Base(cleanRepoPath(p))
}

// Rel returns the repo-relative path for the directory.
func (d *Dir) Rel() string {
	if d == nil {
		return ""
	}
	return d.rel
}

// AbsPath returns the absolute path for the directory on disk.
func (d *Dir) AbsPath() string {
	if d == nil || d.repo == nil {
		return ""
	}
	return filepath.Join(d.repo.Root, filepath.FromSlash(d.rel))
}

// Name returns the base name of the directory.
func (d *Dir) Name() string {
	if d == nil {
		return ""
	}
	return pathBase(d.rel)
}

// Child returns the immediate child directory named name.
func (d *Dir) Child(name string) (*Dir, bool) {
	if d == nil || d.repo == nil {
		return nil, false
	}
	if d.filteredSubdirs {
		for _, child := range d.subdirViews {
			if child != nil && child.Name() == name {
				return child, true
			}
		}
		return nil, false
	}
	rel := cleanRepoPath(path.Join(d.rel, name))
	if _, ok := d.repo.lookupDir(rel); !ok {
		return nil, false
	}
	return &Dir{repo: d.repo, rel: rel}, true
}

// Subdirs returns the immediate child directories.
func (d *Dir) Subdirs() []*Dir {
	if d == nil || d.repo == nil {
		return nil
	}
	if d.filteredSubdirs {
		return append([]*Dir(nil), d.subdirViews...)
	}
	entries, ok := d.repo.lookupDir(d.rel)
	if !ok {
		return nil
	}
	var dirs []*Dir
	for _, name := range entries {
		childRel := cleanRepoPath(path.Join(d.rel, name))
		if _, ok := d.repo.lookupDir(childRel); ok {
			dirs = append(dirs, &Dir{repo: d.repo, rel: childRel})
		}
	}
	return dirs
}

// RegularFiles returns the regular files directly contained in the directory.
func (d *Dir) RegularFiles() []FileRef {
	if d == nil || d.repo == nil {
		return nil
	}
	if d.filteredFiles {
		return append([]FileRef(nil), d.regularFileViews...)
	}
	entries, ok := d.repo.lookupDir(d.rel)
	if !ok {
		return nil
	}
	var files []FileRef
	for _, name := range entries {
		fileRel := cleanRepoPath(path.Join(d.rel, name))
		if _, ok := d.repo.lookupFile(fileRel); ok {
			files = append(files, FileRef{repo: d.repo, rel: fileRel})
		}
	}
	return files
}

func cloneFile(file File) File {
	file.Content = append([]byte(nil), file.Content...)
	return file
}

func (s *BuildSnapshot) lookupFile(rel string) (File, bool) {
	if s == nil {
		return File{}, false
	}
	rel = cleanRepoPath(rel)
	if _, deleted := s.deletedFiles[rel]; deleted {
		return File{}, false
	}
	if file, ok := s.files[rel]; ok {
		return file, true
	}
	if s.base != nil {
		return s.base.lookupFile(rel)
	}
	return File{}, false
}

func (s *Snapshot) lookupFile(rel string) (File, bool) {
	if s == nil {
		return File{}, false
	}
	rel = cleanRepoPath(rel)
	if _, deleted := s.deletedFiles[rel]; deleted {
		return File{}, false
	}
	if file, ok := s.files[rel]; ok {
		return file, true
	}
	if s.base != nil {
		return s.base.lookupFile(rel)
	}
	return File{}, false
}

func (s *BuildSnapshot) lookupDir(rel string) ([]string, bool) {
	if s == nil {
		return nil, false
	}
	rel = cleanRepoPath(rel)
	if _, deleted := s.deletedDirs[rel]; deleted {
		return nil, false
	}
	if entries, ok := s.dirs[rel]; ok {
		return entries, true
	}
	if s.base != nil {
		return s.base.lookupDir(rel)
	}
	return nil, false
}

func (s *Snapshot) lookupDir(rel string) ([]string, bool) {
	if s == nil {
		return nil, false
	}
	rel = cleanRepoPath(rel)
	if _, deleted := s.deletedDirs[rel]; deleted {
		return nil, false
	}
	if entries, ok := s.dirs[rel]; ok {
		return entries, true
	}
	if s.base != nil {
		return s.base.lookupDir(rel)
	}
	return nil, false
}

func (s *BuildSnapshot) filePaths() []string {
	keys := make(map[string]struct{}, len(s.files))
	for key := range s.files {
		if _, deleted := s.deletedFiles[key]; !deleted {
			keys[key] = struct{}{}
		}
	}
	if s.base != nil {
		for _, key := range s.base.filePaths() {
			if _, deleted := s.deletedFiles[key]; deleted {
				continue
			}
			if _, ok := s.files[key]; ok {
				continue
			}
			keys[key] = struct{}{}
		}
	}
	return sortedKeys(keys)
}

func (s *Snapshot) filePaths() []string {
	keys := make(map[string]struct{}, len(s.files))
	for key := range s.files {
		if _, deleted := s.deletedFiles[key]; !deleted {
			keys[key] = struct{}{}
		}
	}
	if s.base != nil {
		for _, key := range s.base.filePaths() {
			if _, deleted := s.deletedFiles[key]; deleted {
				continue
			}
			if _, ok := s.files[key]; ok {
				continue
			}
			keys[key] = struct{}{}
		}
	}
	return sortedKeys(keys)
}

func (s *BuildSnapshot) dirPaths() []string {
	keys := make(map[string]struct{}, len(s.dirs))
	for key := range s.dirs {
		if _, deleted := s.deletedDirs[key]; !deleted {
			keys[key] = struct{}{}
		}
	}
	if s.base != nil {
		for _, key := range s.base.dirPaths() {
			if _, deleted := s.deletedDirs[key]; deleted {
				continue
			}
			if _, ok := s.dirs[key]; ok {
				continue
			}
			keys[key] = struct{}{}
		}
	}
	return sortedKeys(keys)
}

func (s *Snapshot) dirPaths() []string {
	keys := make(map[string]struct{}, len(s.dirs))
	for key := range s.dirs {
		if _, deleted := s.deletedDirs[key]; !deleted {
			keys[key] = struct{}{}
		}
	}
	if s.base != nil {
		for _, key := range s.base.dirPaths() {
			if _, deleted := s.deletedDirs[key]; deleted {
				continue
			}
			if _, ok := s.dirs[key]; ok {
				continue
			}
			keys[key] = struct{}{}
		}
	}
	return sortedKeys(keys)
}

func sortedKeys(m map[string]struct{}) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// Name returns the file's base name.
func (f FileRef) Name() string {
	return pathBase(f.rel)
}

// Rel returns the repo-relative path for the file.
func (f FileRef) Rel() string {
	return f.rel
}

// AbsPath returns the absolute path for the file on disk.
func (f FileRef) AbsPath() string {
	if f.repo == nil {
		return ""
	}
	return filepath.Join(f.repo.Root, filepath.FromSlash(f.rel))
}

// Read returns the file's content from the frozen snapshot.
func (f FileRef) Read() ([]byte, error) {
	if f.repo == nil {
		return nil, os.ErrNotExist
	}
	return f.repo.ReadFile(f.rel)
}

// GetModel returns a parser-backed semantic model for the file.
func (f FileRef) GetModel(parserKey string) (LookupResult, error) {
	if f.repo == nil {
		return LookupResult{}, os.ErrNotExist
	}
	return f.repo.GetModel(f.rel, parserKey)
}
