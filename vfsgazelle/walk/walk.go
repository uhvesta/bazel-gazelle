package walk

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/uhvesta/bazel-gazelle/config"
	"github.com/uhvesta/bazel-gazelle/rule"
	"github.com/uhvesta/bazel-gazelle/vfsgazelle/internal/vfs"
	vfsgazellelanguage "github.com/uhvesta/bazel-gazelle/vfsgazelle/language"
)

var directiveRe = regexp.MustCompile(`^#\s*gazelle:(\w+)\s*(.*?)\s*$`)

// Func is called once per visited package during the walk.
type Func func(args FuncArgs) error

// FuncArgs contains the package-local view passed to a walk callback.
type FuncArgs struct {
	Repo       *vfs.Snapshot
	PackageDir *vfs.Dir
	Dir        string
	Rel        string
	Config     *config.Config
	Update     bool
	File       *rule.File
	GenFiles   []string
}

// FilterChanges returns the subset of changed paths that can affect the vfsgazelle
// walk according to the previous frozen snapshot and the current walk rules.
// This respects .bazelignore, REPO.bazel ignore_directories(), exclude,
// ignore, and directive_file.
func FilterChanges(repo *vfs.Snapshot, c *config.Config, changes []vfs.Change) []vfs.Change {
	if repo == nil || c == nil || len(changes) == 0 {
		return changes
	}
	filtered := make([]vfs.Change, 0, len(changes))
	for _, change := range changes {
		ok, err := shouldTrackPath(repo, c, change.Path)
		if err != nil {
			log.Printf("filtering changed path %s: %v", change.Path, err)
			ok = true
		}
		if ok {
			filtered = append(filtered, change)
		}
	}
	return filtered
}

// PromoteTraversalChanges rewrites BUILD-file changes that alter exclude/ignore
// directives into subtree rebuilds rooted at the containing package. If the
// root package changes these directives, fullRebuild is returned.
func PromoteTraversalChanges(repo *vfs.Snapshot, c *config.Config, changes []vfs.Change) ([]vfs.Change, bool, error) {
	if repo == nil || c == nil || len(changes) == 0 {
		return changes, false, nil
	}
	promoted := make([]vfs.Change, 0, len(changes))
	for _, change := range changes {
		rel := path.Clean(change.Path)
		switch rel {
		case ".bazelignore":
			changed, err := bazelIgnoreChanged(repo)
			if err != nil {
				return nil, false, err
			}
			if changed {
				return nil, true, nil
			}
			continue
		case "REPO.bazel":
			changed, err := repoIgnoreDirectoriesChanged(repo)
			if err != nil {
				return nil, false, err
			}
			if changed {
				return nil, true, nil
			}
			continue
		}
		if !isBuildFilePath(c, rel) {
			promoted = append(promoted, change)
			continue
		}
		changed, err := traversalDirectivesChanged(repo, rel)
		if err != nil {
			return nil, false, err
		}
		if !changed {
			promoted = append(promoted, change)
			continue
		}
		dir := path.Dir(rel)
		if dir == "." || dir == "" {
			return nil, true, nil
		}
		promoted = append(promoted, vfs.Change{Path: dir, Kind: vfs.ChangeModify})
	}
	return promoted, false, nil
}

func bazelIgnoreChanged(repo *vfs.Snapshot) (bool, error) {
	oldPaths, err := bazelIgnoreFromSnapshot(repo)
	if err != nil {
		return false, err
	}
	newPaths, err := loadBazelIgnore(repo.Root)
	if err != nil {
		return false, err
	}
	return !slices.Equal(sortedMapKeys(oldPaths), sortedMapKeys(newPaths)), nil
}

func repoIgnoreDirectoriesChanged(repo *vfs.Snapshot) (bool, error) {
	oldGlobs, err := repoIgnoreDirectoriesFromSnapshot(repo)
	if err != nil {
		return false, err
	}
	newGlobs, err := loadRepoDirectoryIgnore(repo.Root)
	if err != nil {
		return false, err
	}
	sort.Strings(oldGlobs)
	sort.Strings(newGlobs)
	return !slices.Equal(oldGlobs, newGlobs), nil
}

func bazelIgnoreFromSnapshot(repo *vfs.Snapshot) (map[string]struct{}, error) {
	data, err := repo.ReadFile(".bazelignore")
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return parseBazelIgnoreContent(data), nil
}

func repoIgnoreDirectoriesFromSnapshot(repo *vfs.Snapshot) ([]string, error) {
	data, err := repo.ReadFile("REPO.bazel")
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return parseRepoDirectoryIgnoreContent(repo.Root, data)
}

func parseBazelIgnoreContent(data []byte) map[string]struct{} {
	excludes := make(map[string]struct{})
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		ignore := strings.TrimSpace(scanner.Text())
		if ignore == "" || ignore[0] == '#' {
			continue
		}
		if strings.ContainsAny(ignore, "*?[") {
			continue
		}
		excludes[path.Clean(ignore)] = struct{}{}
	}
	return excludes
}

func parseRepoDirectoryIgnoreContent(repoRoot string, data []byte) ([]string, error) {
	return loadRepoDirectoryIgnoreFromData(repoRoot, data)
}

func sortedMapKeys(m map[string]struct{}) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// Walk traverses the whole repo snapshot in depth-first post-order.
//
// Configuration is applied in parent-before-child order by calling each
// Configurer with the BUILD file parsed from the frozen snapshot.
func Walk(repo *vfs.Snapshot, c *config.Config, cexts []config.Configurer, fn Func) error {
	if repo == nil {
		return fmt.Errorf("nil repo snapshot")
	}
	if c == nil {
		return fmt.Errorf("nil config")
	}
	if fn == nil {
		return nil
	}

	if getWalkConfig(c) == nil {
		c.Exts[walkName] = (&walkConfig{
			ignoreFilter:        newIgnoreFilter(c.RepoRoot),
			validBuildFileNames: append([]string(nil), c.ValidBuildFileNames...),
		})
	}
	knownDirectives := make(map[string]bool)
	for _, cext := range cexts {
		for _, d := range cext.KnownDirectives() {
			knownDirectives[d] = true
		}
	}

	_, err := walkDir(repo, c.Clone(), cexts, knownDirectives, "", fn)
	return err
}

type visitInfo struct {
	regularFiles []string
	subdirs      []string
}

func walkDir(repo *vfs.Snapshot, c *config.Config, cexts []config.Configurer, knownDirectives map[string]bool, rel string, fn Func) (visitInfo, error) {
	wc := getWalkConfig(c)
	if wc != nil && rel != "" && wc.isExcludedDir(rel) {
		return visitInfo{}, nil
	}
	entries, err := repo.ListDir(rel)
	if err != nil {
		return visitInfo{}, err
	}
	file, err := loadBuildFile(repo, c, rel, entries)
	if err != nil {
		return visitInfo{}, err
	}
	if err := expandDirectiveFiles(repo, file); err != nil {
		return visitInfo{}, err
	}
	checkDirectives(c, knownDirectives, file)
	for _, cext := range cexts {
		if vfsConfigurer, ok := cext.(vfsgazellelanguage.VFSConfigurer); ok {
			vfsConfigurer.ConfigureRepo(c, repo, rel, file)
		} else {
			cext.Configure(c, rel, file)
		}
	}
	wc = getWalkConfig(c)
	if wc != nil && wc.isExcludedDir(rel) {
		return visitInfo{}, nil
	}

	subdirs := make([]string, 0)
	regularFiles := make([]string, 0)
	for _, name := range entries {
		entryRel := path.Join(rel, name)
		if wc != nil && wc.isExcludedDir(entryRel) {
			continue
		}
		if _, err := repo.ListDir(entryRel); err == nil {
			subdirs = append(subdirs, name)
			continue
		}
		if wc != nil && wc.isExcludedFile(entryRel) {
			continue
		}
		if _, ok := repo.File(entryRel); ok {
			regularFiles = append(regularFiles, name)
		}
	}
	sort.Strings(subdirs)
	sort.Strings(regularFiles)

	vi := visitInfo{
		regularFiles: append([]string(nil), regularFiles...),
		subdirs:      append([]string(nil), subdirs...),
	}

	for _, subdir := range subdirs {
		childRel := path.Join(rel, subdir)
		child, err := walkDir(repo, c.Clone(), cexts, knownDirectives, childRel, fn)
		if err != nil {
			return visitInfo{}, err
		}
		_ = child
	}

	err = fn(FuncArgs{
		Repo:       repo,
		PackageDir: mustDir(repo, rel, subdirs, regularFiles),
		Dir:        filepath.Join(repo.Root, filepath.FromSlash(rel)),
		Rel:        rel,
		Config:     c,
		Update:     wc == nil || !wc.ignore,
		File:       file,
		GenFiles:   findGenFiles(wc, file),
	})
	return vi, err
}

func shouldTrackPath(repo *vfs.Snapshot, c *config.Config, rel string) (bool, error) {
	rel = path.Clean(rel)
	if rel == "." || rel == "" {
		return false, nil
	}
	cfg := c.Clone()
	if getWalkConfig(cfg) == nil {
		cfg.Exts[walkName] = (&walkConfig{
			ignoreFilter:        newIgnoreFilter(c.RepoRoot),
			validBuildFileNames: append([]string(nil), c.ValidBuildFileNames...),
		})
	}

	targetDir := path.Dir(rel)
	if targetDir == "." {
		targetDir = ""
	}
	parts := []string{}
	if targetDir != "" {
		parts = strings.Split(targetDir, "/")
	}
	curRel := ""
	for {
		wc := getWalkConfig(cfg)
		if wc != nil && curRel != "" && wc.isExcludedDir(curRel) {
			return false, nil
		}
		entries, err := repo.ListDir(curRel)
		if err != nil && !os.IsNotExist(err) {
			return false, err
		}
		file, err := loadBuildFile(repo, cfg, curRel, entries)
		if err != nil {
			return false, err
		}
		if file != nil {
			for _, d := range file.Directives {
				if d.Key == "directive_file" && path.Clean(path.Join(curRel, d.Value)) == rel {
					return true, nil
				}
			}
			if err := expandDirectiveFiles(repo, file); err != nil {
				return false, err
			}
		}
		cfg.Exts[walkName] = configureForWalk(getWalkConfig(cfg), curRel, file)
		cfg.ValidBuildFileNames = getWalkConfig(cfg).validBuildFileNames
		if curRel == targetDir {
			break
		}
		next := parts[0]
		parts = parts[1:]
		curRel = path.Join(curRel, next)
	}

	base := path.Base(rel)
	for _, name := range cfg.ValidBuildFileNames {
		if base == name {
			return true, nil
		}
	}
	wc := getWalkConfig(cfg)
	if wc != nil {
		if wc.ignore {
			return false, nil
		}
		if wc.isExcludedFile(rel) {
			return false, nil
		}
	}
	return true, nil
}

func isBuildFilePath(c *config.Config, rel string) bool {
	base := path.Base(rel)
	for _, name := range c.ValidBuildFileNames {
		if base == name {
			return true
		}
	}
	return false
}

func traversalDirectivesChanged(repo *vfs.Snapshot, rel string) (bool, error) {
	oldData, _ := repo.ReadFile(rel)
	oldDirectives, err := traversalDirectivesFromData(repo, rel, oldData)
	if err != nil {
		return false, err
	}

	newData, err := os.ReadFile(filepath.Join(repo.Root, filepath.FromSlash(rel)))
	if err != nil {
		if os.IsNotExist(err) {
			return len(oldDirectives) > 0, nil
		}
		return false, err
	}
	newDirectives, err := traversalDirectivesFromData(repo, rel, newData)
	if err != nil {
		return false, err
	}
	return !slices.Equal(oldDirectives, newDirectives), nil
}

func traversalDirectivesFromData(repo *vfs.Snapshot, rel string, data []byte) ([]string, error) {
	if len(data) == 0 {
		return nil, nil
	}
	pkg := path.Dir(rel)
	if pkg == "." {
		pkg = ""
	}
	f, err := rule.LoadData(filepath.Join(repo.Root, filepath.FromSlash(rel)), pkg, data)
	if err != nil {
		return nil, err
	}
	directives := make([]string, 0, len(f.Directives))
	for _, d := range f.Directives {
		if d.Key == "exclude" || d.Key == "ignore" {
			directives = append(directives, d.Key+"="+d.Value)
		}
	}
	return directives, nil
}

func mustDir(repo *vfs.Snapshot, rel string, subdirs []string, regularFiles []string) *vfs.Dir {
	dir, ok := repo.DirView(rel, subdirs, regularFiles)
	if !ok {
		panic(fmt.Sprintf("directory missing from snapshot: %s", rel))
	}
	return dir
}

func loadBuildFile(repo *vfs.Snapshot, c *config.Config, rel string, entries []string) (*rule.File, error) {
	for _, name := range c.ValidBuildFileNames {
		for _, entry := range entries {
			if entry != name {
				continue
			}
			buildRel := path.Join(rel, name)
			data, err := repo.ReadFile(buildRel)
			if err != nil {
				return nil, err
			}
			buildPath := filepath.Join(repo.Root, filepath.FromSlash(buildRel))
			return rule.LoadData(buildPath, rel, data)
		}
	}
	return nil, nil
}

func checkDirectives(c *config.Config, knownDirectives map[string]bool, f *rule.File) {
	if f == nil {
		return
	}
	for _, d := range f.Directives {
		if knownDirectives[d.Key] {
			continue
		}
		log.Printf("%s: unknown directive: gazelle:%s", f.Path, d.Key)
		if c.Strict {
			log.Fatal("Exit as strict mode is on")
		}
	}
}

func findGenFiles(wc *walkConfig, f *rule.File) []string {
	if f == nil {
		return nil
	}
	var genFiles []string
	for _, r := range f.Rules {
		for _, key := range []string{"out", "outs"} {
			if s := r.AttrString(key); s != "" {
				genFiles = append(genFiles, s)
			} else if ss := r.AttrStrings(key); len(ss) > 0 {
				genFiles = append(genFiles, ss...)
			}
		}
	}
	if wc != nil {
		filtered := genFiles[:0]
		for _, s := range genFiles {
			if !wc.isExcludedFile(path.Join(f.Pkg, s)) {
				filtered = append(filtered, s)
			}
		}
		genFiles = filtered
	}
	sort.Strings(genFiles)
	return genFiles
}

func expandDirectiveFiles(repo *vfs.Snapshot, f *rule.File) error {
	if f == nil {
		return nil
	}
	hasDirectiveFile := false
	for _, d := range f.Directives {
		if d.Key == "directive_file" {
			hasDirectiveFile = true
			break
		}
	}
	if !hasDirectiveFile {
		return nil
	}
	var expanded []rule.Directive
	for _, d := range f.Directives {
		if d.Key != "directive_file" {
			expanded = append(expanded, d)
			continue
		}
		rel := path.Clean(path.Join(f.Pkg, d.Value))
		data, err := repo.ReadFile(rel)
		if err != nil {
			return fmt.Errorf("%s: reading directive file %s: %w", f.Path, d.Value, err)
		}
		scanner := bufio.NewScanner(strings.NewReader(string(data)))
		for scanner.Scan() {
			line := scanner.Text()
			match := directiveRe.FindStringSubmatch(line)
			if match == nil {
				continue
			}
			if match[1] == "directive_file" {
				return fmt.Errorf("%s: directive_file in %s: recursive directive_file is not supported", f.Path, d.Value)
			}
			expanded = append(expanded, rule.Directive{Key: match[1], Value: match[2]})
		}
		if err := scanner.Err(); err != nil {
			return fmt.Errorf("%s: reading directive file %s: %w", f.Path, d.Value, err)
		}
	}
	f.Directives = expanded
	return nil
}
