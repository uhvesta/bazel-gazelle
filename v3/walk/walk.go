package walk

import (
	"fmt"
	"log"
	"path"
	"path/filepath"
	"sort"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/rule"
	"github.com/bazelbuild/bazel-gazelle/v3/internal/vfs"
)

type Func func(args FuncArgs) error

type FuncArgs struct {
	Repo         *vfs.Snapshot
	Dir          string
	Rel          string
	Config       *config.Config
	File         *rule.File
	Subdirs      []string
	RegularFiles []string
	GenFiles     []string
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

	knownDirectives := make(map[string]bool)
	for _, cext := range cexts {
		for _, d := range cext.KnownDirectives() {
			knownDirectives[d] = true
		}
	}

	return walkDir(repo, c.Clone(), cexts, knownDirectives, "", fn)
}

func walkDir(repo *vfs.Snapshot, c *config.Config, cexts []config.Configurer, knownDirectives map[string]bool, rel string, fn Func) error {
	entries, err := repo.ListDir(rel)
	if err != nil {
		return err
	}
	file, err := loadBuildFile(repo, c, rel, entries)
	if err != nil {
		return err
	}
	checkDirectives(c, knownDirectives, file)
	for _, cext := range cexts {
		cext.Configure(c, rel, file)
	}

	var subdirs, regularFiles []string
	for _, name := range entries {
		entryRel := path.Join(rel, name)
		if _, err := repo.ListDir(entryRel); err == nil {
			subdirs = append(subdirs, name)
			continue
		}
		if _, ok := repo.File(entryRel); ok {
			regularFiles = append(regularFiles, name)
		}
	}
	sort.Strings(subdirs)
	sort.Strings(regularFiles)

	for _, subdir := range subdirs {
		childRel := path.Join(rel, subdir)
		if err := walkDir(repo, c.Clone(), cexts, knownDirectives, childRel, fn); err != nil {
			return err
		}
	}

	return fn(FuncArgs{
		Repo:         repo,
		Dir:          filepath.Join(repo.Root, filepath.FromSlash(rel)),
		Rel:          rel,
		Config:       c,
		File:         file,
		Subdirs:      subdirs,
		RegularFiles: regularFiles,
		GenFiles:     findGenFiles(file),
	})
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

func findGenFiles(f *rule.File) []string {
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
	sort.Strings(genFiles)
	return genFiles
}
