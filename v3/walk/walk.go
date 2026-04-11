package walk

import (
	"bufio"
	"fmt"
	"log"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/rule"
	"github.com/bazelbuild/bazel-gazelle/v3/internal/vfs"
	v3language "github.com/bazelbuild/bazel-gazelle/v3/language"
)

var directiveRe = regexp.MustCompile(`^#\s*gazelle:(\w+)\s*(.*?)\s*$`)

type Func func(args FuncArgs) error

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
		if vfsConfigurer, ok := cext.(v3language.VFSConfigurer); ok {
			vfsConfigurer.ConfigureRepo(c, repo, rel, file)
		} else {
			cext.Configure(c, rel, file)
		}
	}
	wc = getWalkConfig(c)
	if wc != nil && wc.isExcludedDir(rel) {
		return visitInfo{}, nil
	}

	var subdirs, regularFiles []string
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
