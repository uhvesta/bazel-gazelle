package vfs

import (
	"bufio"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
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

type persistedSnapshot struct {
	Root  string              `json:"root"`
	Files []File              `json:"files"`
	Dirs  map[string][]string `json:"dirs"`
	Cache Persisted           `json:"cache"`
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
	files := make([]File, 0, len(s.files))
	for _, file := range s.files {
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

	dirs := make(map[string][]string, len(s.dirs))
	for dir, entries := range s.dirs {
		dirs[dir] = append([]string(nil), entries...)
	}

	persisted := persistedSnapshot{
		Root:  s.Root,
		Files: files,
		Dirs:  dirs,
		Cache: s.cache.Snapshot(),
	}
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
	var persisted persistedSnapshot
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
	files := make(map[string]File, len(persisted.Files))
	for _, file := range persisted.Files {
		files[file.Path] = File{
			Path:    file.Path,
			Content: append([]byte(nil), file.Content...),
			Hash:    file.Hash,
		}
	}
	dirs := make(map[string][]string, len(persisted.Dirs))
	for dir, entries := range persisted.Dirs {
		dirs[cleanRepoPath(dir)] = append([]string(nil), entries...)
	}
	return &Snapshot{
		Root:     persisted.Root,
		cache:    &Cache{entries: cacheEntries},
		registry: registry,
		files:    files,
		dirs:     dirs,
	}, nil
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

func hasStateMagic(r *bufio.Reader, magic string) bool {
	b, err := r.Peek(len(magic))
	return err == nil && string(b) == magic
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
		Root:     root,
		builder:  NewCacheBuilder(prev.cache),
		registry: opts.Registry,
		files:    make(map[string]File, len(prev.files)),
		dirs:     make(map[string][]string, len(prev.dirs)),
	}
	for key, file := range prev.files {
		s.files[key] = file
	}
	for dir, entries := range prev.dirs {
		s.dirs[dir] = entries
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
	if err := ensureDirPath(s, relDir); err != nil {
		return err
	}
	return filepath.WalkDir(absDir, func(absPath string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if absPath == absDir {
			return nil
		}
		rel, err := filepath.Rel(s.Root, absPath)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			return ensureDirPath(s, rel)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		_, err = upsertFileFromDisk(s, absPath, rel)
		return err
	})
}

func upsertFileFromDisk(s *BuildSnapshot, absPath, rel string) (bool, error) {
	content, err := os.ReadFile(absPath)
	if err != nil {
		return false, err
	}
	hash := digest(content)
	if existing, ok := s.files[rel]; ok && existing.Hash == hash {
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
		if _, ok := s.dirs[""]; !ok {
			s.dirs[""] = nil
		}
		return nil
	}
	parent := path.Dir(rel)
	if parent == "." {
		parent = ""
	}
	if err := ensureDirPath(s, parent); err != nil {
		return err
	}
	if _, ok := s.dirs[rel]; !ok {
		s.dirs[rel] = nil
	}
	addDirEntry(s, parent, path.Base(rel))
	return nil
}

func addDirEntry(s *BuildSnapshot, dir, name string) {
	entries := append([]string(nil), s.dirs[dir]...)
	for _, existing := range entries {
		if existing == name {
			s.dirs[dir] = entries
			return
		}
	}
	entries = append(entries, name)
	s.dirs[dir] = entries
}

func removePath(s *BuildSnapshot, rel string) bool {
	if _, ok := s.files[rel]; ok {
		delete(s.files, rel)
		s.builder.DeletePath(rel)
		removeDirEntry(s, parentDir(rel), path.Base(rel))
		return true
	}
	if _, ok := s.dirs[rel]; !ok {
		return false
	}
	var subdirs []string
	for dir := range s.dirs {
		if dir == rel || pathHasPrefix(dir, rel) {
			subdirs = append(subdirs, dir)
		}
	}
	sort.Slice(subdirs, func(i, j int) bool { return len(subdirs[i]) > len(subdirs[j]) })
	for _, dir := range subdirs {
		delete(s.dirs, dir)
	}
	for file := range s.files {
		if pathHasPrefix(file, rel) {
			delete(s.files, file)
		}
	}
	s.builder.DeleteSubtree(rel)
	removeDirEntry(s, parentDir(rel), path.Base(rel))
	return true
}

func removeDirEntry(s *BuildSnapshot, dir, name string) {
	entries, ok := s.dirs[dir]
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
