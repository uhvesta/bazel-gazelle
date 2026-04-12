package vfs

import (
	"bufio"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/uhvesta/bazel-gazelle/config"
)

// StateFormat names the on-disk encoding used for persisted vfsgazelle state.
type StateFormat string

const (
	StateFormatGob  StateFormat = "gob"
	StateFormatJSON StateFormat = "json"
)

const (
	stateMagicGob     = "GZVFSGB1\n"
	stateMagicJSON    = "GZVFSJN1\n"
	oldStateMagicGob  = "GZV3GOB1\n"
	oldStateMagicJSON = "GZV3JSN1\n"
	metaMagicGob      = "GZVFSMG1\n"
	metaMagicJSON     = "GZVFSMJ1\n"
	cacheMagicGob     = "GZVFSKG1\n"
	cacheMagicJSON    = "GZVFSKJ1\n"
)

// ChangeKind describes the kind of filesystem change being patched.
type ChangeKind string

const (
	ChangeModify ChangeKind = "modify"
	ChangeDelete ChangeKind = "delete"
)

// Change describes one repo-relative path update applied during Patch.
type Change struct {
	Path string     `json:"path"`
	Kind ChangeKind `json:"kind"`
}

type persistedMetadata struct {
	Root                string              `json:"root"`
	ValidBuildFileNames []string            `json:"validBuildFileNames,omitempty"`
	Files               []File              `json:"files"`
	Dirs                map[string][]string `json:"dirs"`
}

type persistedState struct {
	Metadata persistedMetadata `json:"metadata"`
	Cache    Persisted         `json:"cache"`
}

var alwaysPersistContent = map[string]bool{
	".bazelignore":    true,
	"BUILD":           true,
	"BUILD.bazel":     true,
	"REPO.bazel":      true,
	"WORKSPACE":       true,
	"WORKSPACE.bazel": true,
}

func (s *Snapshot) Save(w io.Writer, format StateFormat) error {
	if s == nil {
		return fmt.Errorf("nil snapshot")
	}
	metadata, err := s.persistedMetadata()
	if err != nil {
		return err
	}
	persisted := persistedState{
		Metadata: metadata,
	}
	cachePersisted, err := s.cache.snapshotPersisted()
	if err != nil {
		return err
	}
	persisted.Cache = cachePersisted
	switch format {
	case "", StateFormatGob:
		if _, err := io.WriteString(w, stateMagicGob); err != nil {
			return err
		}
		return gob.NewEncoder(w).Encode(persisted)
	case StateFormatJSON:
		if _, err := io.WriteString(w, stateMagicJSON); err != nil {
			return err
		}
		return json.NewEncoder(w).Encode(persisted)
	default:
		return fmt.Errorf("unknown state format %q", format)
	}
}

// SaveMetadata writes only the snapshot metadata needed to reconstruct the
// tree, file hashes, and direct-read file contents.
func (s *Snapshot) SaveMetadata(w io.Writer, format StateFormat) error {
	metadata, err := s.persistedMetadata()
	if err != nil {
		return err
	}
	switch format {
	case "", StateFormatGob:
		if _, err := io.WriteString(w, metaMagicGob); err != nil {
			return err
		}
		return gob.NewEncoder(w).Encode(metadata)
	case StateFormatJSON:
		if _, err := io.WriteString(w, metaMagicJSON); err != nil {
			return err
		}
		return json.NewEncoder(w).Encode(metadata)
	default:
		return fmt.Errorf("unknown state format %q", format)
	}
}

// SaveCache writes only the parser-cache payload for a snapshot.
func (s *Snapshot) SaveCache(w io.Writer, format StateFormat) error {
	if s == nil || s.cache == nil {
		return fmt.Errorf("nil snapshot cache")
	}
	persisted, err := s.cache.snapshotPersisted()
	if err != nil {
		return err
	}
	switch format {
	case "", StateFormatGob:
		if _, err := io.WriteString(w, cacheMagicGob); err != nil {
			return err
		}
		return gob.NewEncoder(w).Encode(persisted)
	case StateFormatJSON:
		if _, err := io.WriteString(w, cacheMagicJSON); err != nil {
			return err
		}
		return json.NewEncoder(w).Encode(persisted)
	default:
		return fmt.Errorf("unknown state format %q", format)
	}
}

func (s *Snapshot) persistedMetadata() (persistedMetadata, error) {
	if s == nil {
		return persistedMetadata{}, fmt.Errorf("nil snapshot")
	}
	files := make([]File, 0, len(s.filePaths()))
	for _, filePath := range s.filePaths() {
		file, ok := s.lookupFile(filePath)
		if !ok {
			continue
		}
		content := file.Content
		if !s.shouldPersistContent(file.Path) {
			content = nil
		}
		files = append(files, File{
			Path:    file.Path,
			Content: append([]byte(nil), content...),
			Hash:    file.Hash,
		})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })

	dirs := make(map[string][]string)
	for _, dir := range s.dirPaths() {
		entries, ok := s.lookupDir(dir)
		if !ok {
			continue
		}
		dirs[dir] = append([]string(nil), entries...)
	}
	return persistedMetadata{
		Root:                s.Root,
		ValidBuildFileNames: append([]string(nil), s.validBuildFileNames...),
		Files:               files,
		Dirs:                dirs,
	}, nil
}

func (s *Snapshot) shouldPersistContent(rel string) bool {
	if s == nil {
		return false
	}
	if alwaysPersistContent[path.Base(rel)] {
		return true
	}
	if s.registry == nil {
		return true
	}
	return len(s.registry.Match(rel)) == 0
}

// LoadSnapshot decodes a persisted snapshot and parsed-model cache.
func LoadSnapshot(r io.Reader, registry *Registry) (*Snapshot, error) {
	br := bufio.NewReader(r)
	format, err := detectStateFormat(br)
	if err != nil {
		return nil, err
	}
	var persisted persistedState
	switch format {
	case StateFormatGob:
		if hasStateMagic(br, oldStateMagicGob) {
			if _, err := br.Discard(len(oldStateMagicGob)); err != nil {
				return nil, err
			}
		} else if _, err := br.Discard(len(stateMagicGob)); err != nil {
			return nil, err
		}
		if err := gob.NewDecoder(br).Decode(&persisted); err != nil {
			return nil, err
		}
	case StateFormatJSON:
		if hasStateMagic(br, stateMagicJSON) {
			if _, err := br.Discard(len(stateMagicJSON)); err != nil {
				return nil, err
			}
		} else if hasStateMagic(br, oldStateMagicJSON) {
			if _, err := br.Discard(len(oldStateMagicJSON)); err != nil {
				return nil, err
			}
		}
		if err := json.NewDecoder(br).Decode(&persisted); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unknown state format %q", format)
	}
	cacheEntries := make(map[cacheKey]Entry, len(persisted.Cache.Entries))
	for _, entry := range persisted.Cache.Entries {
		key := cacheKey{Path: entry.Path, ParserKey: entry.ParserKey}
		cacheEntries[key] = cloneEntry(entry)
	}
	return snapshotFromMetadata(persisted.Metadata, registry, &Cache{entries: cacheEntries}), nil
}

// LoadSnapshotMetadata decodes only snapshot metadata.
func LoadSnapshotMetadata(r io.Reader, registry *Registry) (*Snapshot, error) {
	br := bufio.NewReader(r)
	format, err := detectMetadataFormat(br)
	if err != nil {
		return nil, err
	}
	var metadata persistedMetadata
	switch format {
	case StateFormatGob:
		if _, err := br.Discard(len(metaMagicGob)); err != nil {
			return nil, err
		}
		if err := gob.NewDecoder(br).Decode(&metadata); err != nil {
			return nil, err
		}
	case StateFormatJSON:
		if _, err := br.Discard(len(metaMagicJSON)); err != nil {
			return nil, err
		}
		if err := json.NewDecoder(br).Decode(&metadata); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unknown metadata format %q", format)
	}
	return snapshotFromMetadata(metadata, registry, &Cache{entries: make(map[cacheKey]Entry)}), nil
}

// LoadCachePayload decodes only persisted parser-cache entries.
func LoadCachePayload(r io.Reader) (*Cache, error) {
	br := bufio.NewReader(r)
	format, err := detectCacheFormat(br)
	if err != nil {
		return nil, err
	}
	var persisted Persisted
	switch format {
	case StateFormatGob:
		if _, err := br.Discard(len(cacheMagicGob)); err != nil {
			return nil, err
		}
		if err := gob.NewDecoder(br).Decode(&persisted); err != nil {
			return nil, err
		}
	case StateFormatJSON:
		if _, err := br.Discard(len(cacheMagicJSON)); err != nil {
			return nil, err
		}
		if err := json.NewDecoder(br).Decode(&persisted); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unknown cache format %q", format)
	}
	cacheEntries := make(map[cacheKey]Entry, len(persisted.Entries))
	for _, entry := range persisted.Entries {
		key := cacheKey{Path: entry.Path, ParserKey: entry.ParserKey}
		cacheEntries[key] = cloneEntry(entry)
	}
	return &Cache{entries: cacheEntries}, nil
}

// AttachCache returns a shallow copy of snapshot with cache installed.
func (s *Snapshot) AttachCache(cache *Cache) *Snapshot {
	if s == nil {
		return nil
	}
	if cache == nil {
		cache = &Cache{entries: make(map[cacheKey]Entry)}
	}
	out := *s
	out.cache = cache
	return &out
}

// AttachCacheLoader returns a shallow copy of snapshot with a parser cache
// that begins loading immediately in the background and blocks only when
// parser-backed cache access actually needs it.
func (s *Snapshot) AttachCacheLoader(fn func() (*Cache, error)) *Snapshot {
	if s == nil {
		return nil
	}
	out := *s
	out.cache = newPendingCache(fn)
	return &out
}

func detectStateFormat(r *bufio.Reader) (StateFormat, error) {
	if hasStateMagic(r, stateMagicGob) {
		return StateFormatGob, nil
	}
	if hasStateMagic(r, stateMagicJSON) {
		return StateFormatJSON, nil
	}
	if hasStateMagic(r, oldStateMagicGob) {
		return StateFormatGob, nil
	}
	if hasStateMagic(r, oldStateMagicJSON) {
		return StateFormatJSON, nil
	}
	// Backward compatibility with older JSON snapshots that had no header.
	first, err := r.Peek(1)
	if err != nil {
		if err == io.EOF {
			return StateFormatJSON, nil
		}
		return "", err
	}
	if len(first) == 1 && (first[0] == '{' || first[0] == '[') {
		return StateFormatJSON, nil
	}
	return StateFormatGob, nil
}

func detectMetadataFormat(r *bufio.Reader) (StateFormat, error) {
	if hasStateMagic(r, metaMagicGob) {
		return StateFormatGob, nil
	}
	if hasStateMagic(r, metaMagicJSON) {
		return StateFormatJSON, nil
	}
	return "", fmt.Errorf("unknown metadata state format")
}

func detectCacheFormat(r *bufio.Reader) (StateFormat, error) {
	if hasStateMagic(r, cacheMagicGob) {
		return StateFormatGob, nil
	}
	if hasStateMagic(r, cacheMagicJSON) {
		return StateFormatJSON, nil
	}
	return "", fmt.Errorf("unknown cache state format")
}

func hasStateMagic(r *bufio.Reader, magic string) bool {
	b, err := r.Peek(len(magic))
	return err == nil && string(b) == magic
}

func snapshotFromMetadata(metadata persistedMetadata, registry *Registry, cache *Cache) *Snapshot {
	files := make(map[string]File, len(metadata.Files))
	for _, file := range metadata.Files {
		files[file.Path] = File{
			Path:    file.Path,
			Content: append([]byte(nil), file.Content...),
			Hash:    file.Hash,
		}
	}
	dirs := make(map[string][]string, len(metadata.Dirs))
	for dir, entries := range metadata.Dirs {
		dirs[cleanRepoPath(dir)] = append([]string(nil), entries...)
	}
	if cache == nil {
		cache = &Cache{entries: make(map[cacheKey]Entry)}
	}
	validBuildFileNames := append([]string(nil), metadata.ValidBuildFileNames...)
	if len(validBuildFileNames) == 0 {
		validBuildFileNames = append([]string(nil), config.DefaultValidBuildFileNames...)
	}
	return &Snapshot{
		Root:                metadata.Root,
		cache:               cache,
		registry:            registry,
		validBuildFileNames: validBuildFileNames,
		files:               files,
		deletedFiles:        make(map[string]struct{}),
		dirs:                dirs,
		deletedDirs:         make(map[string]struct{}),
	}
}

// Patch applies a changed-path set to a prior frozen snapshot and returns a
// new mutable build snapshot ready for parser priming and freezing.
func Patch(root string, prev *Snapshot, opts BuildOptions, changes []Change) (*BuildSnapshot, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if prev == nil {
		return Build(root, opts)
	}
	s := &BuildSnapshot{
		Root:                root,
		builder:             NewCacheBuilder(prev.cache),
		registry:            opts.Registry,
		validBuildFileNames: append([]string(nil), prev.validBuildFileNames...),
		base:                prev,
		files:               make(map[string]File),
		deletedFiles:        make(map[string]struct{}),
		dirs:                make(map[string][]string),
		deletedDirs:         make(map[string]struct{}),
	}
	if len(opts.ValidBuildFileNames) > 0 {
		s.validBuildFileNames = append([]string(nil), opts.ValidBuildFileNames...)
	}

	normalized := normalizeChanges(changes)
	for _, change := range normalized {
		if err := applyChange(s, change); err != nil {
			return nil, err
		}
	}
	for dir := range s.dirs {
		sort.Strings(s.dirs[dir])
	}
	return s, nil
}

func normalizeChanges(changes []Change) []Change {
	byPath := make(map[string]Change)
	for _, change := range changes {
		rel := cleanRepoPath(change.Path)
		if rel == "" {
			continue
		}
		kind := change.Kind
		if kind == "" {
			kind = ChangeModify
		}
		byPath[rel] = Change{Path: rel, Kind: kind}
	}
	out := make([]Change, 0, len(byPath))
	for _, change := range byPath {
		out = append(out, change)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func applyChange(s *BuildSnapshot, change Change) error {
	rel := cleanRepoPath(change.Path)
	abs := filepath.Join(s.Root, filepath.FromSlash(rel))
	info, err := os.Lstat(abs)
	if change.Kind == ChangeDelete || os.IsNotExist(err) {
		if removePath(s, rel) {
			s.changed = true
		}
		return nil
	}
	if err != nil {
		return err
	}
	if info.IsDir() {
		if removePath(s, rel) {
			s.changed = true
		}
		if err := addDirTree(s, abs, rel); err != nil {
			return err
		}
		s.changed = true
		return nil
	}
	if !info.Mode().IsRegular() {
		if removePath(s, rel) {
			s.changed = true
		}
		return nil
	}
	changed, err := upsertFileFromDisk(s, abs, rel)
	if changed {
		s.changed = true
	}
	return err
}

func addDirTree(s *BuildSnapshot, absDir, relDir string) error {
	cfg, err := inheritedTraversalConfig(s, relDir)
	if err != nil {
		return err
	}
	result, err := buildTree(s.Root, relDir, cfg)
	if err != nil {
		return err
	}
	for dir, entries := range result.dirs {
		s.dirs[dir] = append([]string(nil), entries...)
		delete(s.deletedDirs, dir)
	}
	for filePath, file := range result.files {
		s.files[filePath] = file
		delete(s.deletedFiles, filePath)
	}
	return nil
}

func upsertFileFromDisk(s *BuildSnapshot, absPath, rel string) (bool, error) {
	content, err := os.ReadFile(absPath)
	if err != nil {
		return false, err
	}
	hash := digest(content)
	if existing, ok := s.lookupFile(rel); ok && existing.Hash == hash {
		return false, nil
	}
	if err := ensureDirPath(s, path.Dir(rel)); err != nil {
		return false, err
	}
	s.files[rel] = File{
		Path:    rel,
		Content: content,
		Hash:    hash,
	}
	delete(s.deletedFiles, rel)
	parent := path.Dir(rel)
	if parent == "." {
		parent = ""
	}
	addDirEntry(s, parent, path.Base(rel))
	return true, nil
}

func ensureDirPath(s *BuildSnapshot, rel string) error {
	rel = cleanRepoPath(rel)
	if rel == "" {
		if _, ok := s.lookupDir(""); !ok {
			s.dirs[""] = nil
		}
		delete(s.deletedDirs, "")
		return nil
	}
	parent := path.Dir(rel)
	if parent == "." {
		parent = ""
	}
	if err := ensureDirPath(s, parent); err != nil {
		return err
	}
	if _, ok := s.lookupDir(rel); !ok {
		s.dirs[rel] = nil
	}
	delete(s.deletedDirs, rel)
	addDirEntry(s, parent, path.Base(rel))
	return nil
}

func addDirEntry(s *BuildSnapshot, dir, name string) {
	entries, _ := s.lookupDir(dir)
	entries = append([]string(nil), entries...)
	for _, existing := range entries {
		if existing == name {
			s.dirs[dir] = entries
			delete(s.deletedDirs, dir)
			return
		}
	}
	entries = append(entries, name)
	s.dirs[dir] = entries
	delete(s.deletedDirs, dir)
}

func removePath(s *BuildSnapshot, rel string) bool {
	if _, ok := s.lookupFile(rel); ok {
		delete(s.files, rel)
		s.deletedFiles[rel] = struct{}{}
		s.builder.DeletePath(rel)
		removeDirEntry(s, parentDir(rel), path.Base(rel))
		return true
	}
	if _, ok := s.lookupDir(rel); !ok {
		return false
	}
	var subdirs []string
	for _, dir := range s.dirPaths() {
		if dir == rel || pathHasPrefix(dir, rel) {
			subdirs = append(subdirs, dir)
		}
	}
	sort.Slice(subdirs, func(i, j int) bool { return len(subdirs[i]) > len(subdirs[j]) })
	for _, dir := range subdirs {
		delete(s.dirs, dir)
		s.deletedDirs[dir] = struct{}{}
	}
	for _, file := range s.filePaths() {
		if pathHasPrefix(file, rel) {
			delete(s.files, file)
			s.deletedFiles[file] = struct{}{}
		}
	}
	s.builder.DeleteSubtree(rel)
	removeDirEntry(s, parentDir(rel), path.Base(rel))
	return true
}

func removeDirEntry(s *BuildSnapshot, dir, name string) {
	entries, ok := s.lookupDir(dir)
	if !ok {
		return
	}
	updated := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry != name {
			updated = append(updated, entry)
		}
	}
	s.dirs[dir] = updated
}

func parentDir(rel string) string {
	parent := path.Dir(rel)
	if parent == "." {
		return ""
	}
	return parent
}

func pathHasPrefix(value, prefix string) bool {
	return value == prefix || len(prefix) > 0 && len(value) > len(prefix) && value[:len(prefix)] == prefix && value[len(prefix)] == '/'
}

func inheritedTraversalConfig(s *BuildSnapshot, rel string) (*traversalConfig, error) {
	cfg, err := newTraversalConfig(s.Root, s.validBuildFileNames)
	if err != nil {
		return nil, err
	}
	cur := ""
	for _, part := range strings.Split(cleanRepoPath(rel), "/") {
		if part == "" {
			continue
		}
		if err := applyTraversalConfigForDir(s, cfg, cur); err != nil {
			return nil, err
		}
		cur = cleanRepoPath(path.Join(cur, part))
	}
	return cfg, nil
}

func applyTraversalConfigForDir(s *BuildSnapshot, cfg *traversalConfig, rel string) error {
	entries, ok := s.lookupDir(rel)
	if !ok {
		return nil
	}
	buildName := ""
	for _, candidate := range cfg.validBuildFileNames {
		for _, entry := range entries {
			if entry == candidate {
				buildName = candidate
				break
			}
		}
		if buildName != "" {
			break
		}
	}
	if buildName == "" {
		return nil
	}
	buildRel := cleanRepoPath(path.Join(rel, buildName))
	buildFile, ok := s.lookupFile(buildRel)
	if !ok {
		return nil
	}
	directives, err := parseBuildDirectives(s.Root, rel, buildRel, buildFile.Content)
	if err != nil {
		return err
	}
	applyTraversalDirectives(cfg, rel, buildRel, directives)
	return nil
}
