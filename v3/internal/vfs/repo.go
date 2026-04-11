package vfs

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
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

func NewRegistry() *Registry {
	return &Registry{byKey: make(map[string]RegisteredParser)}
}

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

func (r *Registry) Parser(key string) (Parser, bool) {
	if r == nil {
		return nil, false
	}
	registered, ok := r.byKey[key]
	return registered.Parser, ok
}

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
	Root     string
	builder  *CacheBuilder
	registry *Registry
	files    map[string]File
	dirs     map[string][]string
	changed  bool
}

// Snapshot is the frozen read-only view of the repo and parsed-model cache.
type Snapshot struct {
	Root     string
	cache    *Cache
	registry *Registry
	files    map[string]File
	dirs     map[string][]string
}

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
}

// FileRef is a lightweight handle to a file in a frozen repo snapshot.
type FileRef struct {
	repo *Snapshot
	rel  string
}

type BuildOptions struct {
	Cache    *Cache
	Registry *Registry
}

type fileJob struct {
	absPath string
	rel     string
}

type fileResult struct {
	file File
	err  error
}

func Build(root string, opts BuildOptions) (*BuildSnapshot, error) {
	if root == "" {
		return nil, fmt.Errorf("root must not be empty")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}

	s := &BuildSnapshot{
		Root:     root,
		builder:  NewCacheBuilder(opts.Cache),
		registry: opts.Registry,
		files:    make(map[string]File),
		dirs:     make(map[string][]string),
	}
	s.dirs[""] = nil

	var jobs []fileJob
	err = filepath.WalkDir(root, func(absPath string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if absPath == root {
			return nil
		}

		rel, err := filepath.Rel(root, absPath)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		parent := path.Dir(rel)
		if parent == "." {
			parent = ""
		}
		base := path.Base(rel)
		s.dirs[parent] = append(s.dirs[parent], base)

		if d.IsDir() {
			if _, ok := s.dirs[rel]; !ok {
				s.dirs[rel] = nil
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		jobs = append(jobs, fileJob{absPath: absPath, rel: rel})
		return nil
	})
	if err != nil {
		return nil, err
	}

	if err := readFilesIntoSnapshot(s, jobs); err != nil {
		return nil, err
	}

	for dir := range s.dirs {
		sort.Strings(s.dirs[dir])
	}
	return s, nil
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
		Root:     s.Root,
		cache:    cache,
		registry: s.registry,
		files:    files,
		dirs:     dirs,
	}
}

func (s *BuildSnapshot) Builder() *CacheBuilder {
	return s.builder
}

func (s *BuildSnapshot) Changed() bool {
	if s == nil {
		return false
	}
	return s.changed
}

func (s *BuildSnapshot) Files() []File {
	files := make([]File, 0, len(s.files))
	for _, file := range s.files {
		files = append(files, File{
			Path:    file.Path,
			Content: append([]byte(nil), file.Content...),
			Hash:    file.Hash,
		})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files
}

func (s *BuildSnapshot) ReadFile(path string) ([]byte, error) {
	file, ok := s.files[cleanRepoPath(path)]
	if !ok {
		return nil, os.ErrNotExist
	}
	return append([]byte(nil), file.Content...), nil
}

func (s *BuildSnapshot) File(path string) (File, bool) {
	file, ok := s.files[cleanRepoPath(path)]
	if !ok {
		return File{}, false
	}
	file.Content = append([]byte(nil), file.Content...)
	return file, true
}

func (s *BuildSnapshot) ListDir(dir string) ([]string, error) {
	entries, ok := s.dirs[cleanRepoPath(dir)]
	if !ok {
		return nil, os.ErrNotExist
	}
	return append([]string(nil), entries...), nil
}

func (s *BuildSnapshot) MatchingParsers(path string) []Parser {
	return s.registry.Match(cleanRepoPath(path))
}

func (s *BuildSnapshot) GetModel(path, parserKey string) (LookupResult, error) {
	file, ok := s.files[cleanRepoPath(path)]
	if !ok {
		return LookupResult{}, os.ErrNotExist
	}
	parser, ok := s.registry.Parser(parserKey)
	if !ok {
		return LookupResult{}, fmt.Errorf("parser not registered: %s", parserKey)
	}
	return s.builder.Parse(file.Path, file.Content, parser)
}

func (s *Snapshot) ReadFile(path string) ([]byte, error) {
	file, ok := s.files[cleanRepoPath(path)]
	if !ok {
		return nil, os.ErrNotExist
	}
	return append([]byte(nil), file.Content...), nil
}

func (s *Snapshot) Files() []File {
	files := make([]File, 0, len(s.files))
	for _, file := range s.files {
		files = append(files, File{
			Path:    file.Path,
			Content: append([]byte(nil), file.Content...),
			Hash:    file.Hash,
		})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files
}

func (s *Snapshot) Cache() *Cache {
	return s.cache
}

func (s *Snapshot) Dir(rel string) (*Dir, bool) {
	rel = cleanRepoPath(rel)
	if _, ok := s.dirs[rel]; !ok {
		return nil, false
	}
	return &Dir{repo: s, rel: rel}, true
}

func (s *Snapshot) DirView(rel string, subdirs []string, regularFiles []string) (*Dir, bool) {
	base, ok := s.Dir(rel)
	if !ok {
		return nil, false
	}
	if subdirs != nil {
		base.subdirViews = make([]*Dir, 0, len(subdirs))
		for _, name := range subdirs {
			child, ok := s.Dir(path.Join(rel, name))
			if ok {
				base.subdirViews = append(base.subdirViews, child)
			}
		}
	}
	if regularFiles != nil {
		base.regularFileViews = make([]FileRef, 0, len(regularFiles))
		for _, name := range regularFiles {
			fileRel := cleanRepoPath(path.Join(rel, name))
			if _, ok := s.files[fileRel]; ok {
				base.regularFileViews = append(base.regularFileViews, FileRef{repo: s, rel: fileRel})
			}
		}
	}
	return base, true
}

func (s *Snapshot) File(path string) (File, bool) {
	file, ok := s.files[cleanRepoPath(path)]
	if !ok {
		return File{}, false
	}
	file.Content = append([]byte(nil), file.Content...)
	return file, true
}

func (s *Snapshot) ListDir(dir string) ([]string, error) {
	entries, ok := s.dirs[cleanRepoPath(dir)]
	if !ok {
		return nil, os.ErrNotExist
	}
	return append([]string(nil), entries...), nil
}

func (s *Snapshot) MatchingParsers(path string) []Parser {
	return s.registry.Match(cleanRepoPath(path))
}

func (s *Snapshot) GetModel(path, parserKey string) (LookupResult, error) {
	file, ok := s.files[cleanRepoPath(path)]
	if !ok {
		return LookupResult{}, os.ErrNotExist
	}
	parser, ok := s.registry.Parser(parserKey)
	if !ok {
		return LookupResult{}, fmt.Errorf("parser not registered: %s", parserKey)
	}
	result, hit, err := s.cache.Get(file.Path, file.Content, parser)
	if err != nil {
		return LookupResult{}, err
	}
	if !hit {
		return LookupResult{}, fmt.Errorf("frozen cache miss for %s with parser %s", file.Path, parserKey)
	}
	return result, nil
}

func MatchExtension(ext string) PathMatcher {
	return func(path string) bool {
		return strings.HasSuffix(path, ext)
	}
}

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

func (d *Dir) Rel() string {
	if d == nil {
		return ""
	}
	return d.rel
}

func (d *Dir) AbsPath() string {
	if d == nil || d.repo == nil {
		return ""
	}
	return filepath.Join(d.repo.Root, filepath.FromSlash(d.rel))
}

func (d *Dir) Name() string {
	if d == nil {
		return ""
	}
	return pathBase(d.rel)
}

func (d *Dir) Child(name string) (*Dir, bool) {
	if d == nil || d.repo == nil {
		return nil, false
	}
	if d.subdirViews != nil {
		for _, child := range d.subdirViews {
			if child != nil && child.Name() == name {
				return child, true
			}
		}
		return nil, false
	}
	rel := cleanRepoPath(path.Join(d.rel, name))
	if _, ok := d.repo.dirs[rel]; !ok {
		return nil, false
	}
	return &Dir{repo: d.repo, rel: rel}, true
}

func (d *Dir) Subdirs() []*Dir {
	if d == nil || d.repo == nil {
		return nil
	}
	if d.subdirViews != nil {
		return append([]*Dir(nil), d.subdirViews...)
	}
	entries, ok := d.repo.dirs[d.rel]
	if !ok {
		return nil
	}
	var dirs []*Dir
	for _, name := range entries {
		childRel := cleanRepoPath(path.Join(d.rel, name))
		if _, ok := d.repo.dirs[childRel]; ok {
			dirs = append(dirs, &Dir{repo: d.repo, rel: childRel})
		}
	}
	return dirs
}

func (d *Dir) RegularFiles() []FileRef {
	if d == nil || d.repo == nil {
		return nil
	}
	if d.regularFileViews != nil {
		return append([]FileRef(nil), d.regularFileViews...)
	}
	entries, ok := d.repo.dirs[d.rel]
	if !ok {
		return nil
	}
	var files []FileRef
	for _, name := range entries {
		fileRel := cleanRepoPath(path.Join(d.rel, name))
		if _, ok := d.repo.files[fileRel]; ok {
			files = append(files, FileRef{repo: d.repo, rel: fileRel})
		}
	}
	return files
}

func (f FileRef) Name() string {
	return pathBase(f.rel)
}

func (f FileRef) Rel() string {
	return f.rel
}

func (f FileRef) AbsPath() string {
	if f.repo == nil {
		return ""
	}
	return filepath.Join(f.repo.Root, filepath.FromSlash(f.rel))
}

func (f FileRef) Read() ([]byte, error) {
	if f.repo == nil {
		return nil, os.ErrNotExist
	}
	return f.repo.ReadFile(f.rel)
}

func (f FileRef) GetModel(parserKey string) (LookupResult, error) {
	if f.repo == nil {
		return LookupResult{}, os.ErrNotExist
	}
	return f.repo.GetModel(f.rel, parserKey)
}
