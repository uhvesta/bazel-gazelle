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
		Root:     s.Root,
		cache:    cache,
		registry: s.registry,
		files:    files,
		dirs:     dirs,
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

// ReadFile returns the current content for path from the build snapshot.
func (s *BuildSnapshot) ReadFile(path string) ([]byte, error) {
	file, ok := s.files[cleanRepoPath(path)]
	if !ok {
		return nil, os.ErrNotExist
	}
	return append([]byte(nil), file.Content...), nil
}

// File returns the file metadata stored for path in the build snapshot.
func (s *BuildSnapshot) File(path string) (File, bool) {
	file, ok := s.files[cleanRepoPath(path)]
	if !ok {
		return File{}, false
	}
	file.Content = append([]byte(nil), file.Content...)
	return file, true
}

// ListDir returns the immediate entries in dir.
func (s *BuildSnapshot) ListDir(dir string) ([]string, error) {
	entries, ok := s.dirs[cleanRepoPath(dir)]
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
	file, ok := s.files[cleanRepoPath(path)]
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
	}
	return s.builder.Parse(file.Path, file.Content, parser)
}

// ReadFile returns the frozen content for path.
func (s *Snapshot) ReadFile(path string) ([]byte, error) {
	file, ok := s.files[cleanRepoPath(path)]
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

// Cache returns the frozen parsed-model cache for the snapshot.
func (s *Snapshot) Cache() *Cache {
	return s.cache
}

// Dir returns a directory handle for rel.
func (s *Snapshot) Dir(rel string) (*Dir, bool) {
	rel = cleanRepoPath(rel)
	if _, ok := s.dirs[rel]; !ok {
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
			if _, ok := s.files[fileRel]; ok {
				base.regularFileViews = append(base.regularFileViews, FileRef{repo: s, rel: fileRel})
			}
		}
	}
	return base, true
}

// File returns the file metadata stored for path in the frozen snapshot.
func (s *Snapshot) File(path string) (File, bool) {
	file, ok := s.files[cleanRepoPath(path)]
	if !ok {
		return File{}, false
	}
	file.Content = append([]byte(nil), file.Content...)
	return file, true
}

// ListDir returns the immediate entries in dir from the frozen snapshot.
func (s *Snapshot) ListDir(dir string) ([]string, error) {
	entries, ok := s.dirs[cleanRepoPath(dir)]
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
	file, ok := s.files[cleanRepoPath(path)]
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
	if _, ok := d.repo.dirs[rel]; !ok {
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

// RegularFiles returns the regular files directly contained in the directory.
func (d *Dir) RegularFiles() []FileRef {
	if d == nil || d.repo == nil {
		return nil
	}
	if d.filteredFiles {
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
