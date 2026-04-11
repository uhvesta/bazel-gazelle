package walk

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/bazelbuild/bazel-gazelle/config"
	gzflag "github.com/bazelbuild/bazel-gazelle/flag"
	"github.com/bazelbuild/bazel-gazelle/rule"
	bzl "github.com/bazelbuild/buildtools/build"
	"github.com/bmatcuk/doublestar/v4"
)

type generationModeType string

const (
	generationModeUpdate generationModeType = "update_only"
	generationModeCreate generationModeType = "create_and_update"
)

type walkConfig struct {
	ignoreFilter        *ignoreFilter
	excludes            []string
	ignore              bool
	validBuildFileNames []string
}

const walkName = "_v3_walk"

func getWalkConfig(c *config.Config) *walkConfig {
	wc, _ := c.Exts[walkName].(*walkConfig)
	return wc
}

func (wc *walkConfig) clone() *walkConfig {
	if wc == nil {
		return nil
	}
	wcCopy := *wc
	wcCopy.excludes = append([]string(nil), wc.excludes...)
	return &wcCopy
}

func (wc *walkConfig) isExcludedDir(p string) bool {
	return path.Base(p) == ".git" || wc.ignoreFilter.isDirectoryIgnored(p) || matchAnyGlob(wc.excludes, p)
}

func (wc *walkConfig) isExcludedFile(p string) bool {
	return wc.ignoreFilter.isFileIgnored(p) || matchAnyGlob(wc.excludes, p)
}

type Configurer struct {
	cliExcludes       []string
	cliBuildFileNames string
}

func (cr *Configurer) RegisterFlags(fs *flag.FlagSet, cmd string, c *config.Config) {
	fs.Var(&gzflag.MultiFlag{Values: &cr.cliExcludes}, "exclude", "pattern that should be ignored (may be repeated)")
	defaultBuildFileNames := c.ValidBuildFileNames
	if len(defaultBuildFileNames) == 0 {
		defaultBuildFileNames = config.DefaultValidBuildFileNames
	}
	fs.StringVar(&cr.cliBuildFileNames, "build_file_name", strings.Join(defaultBuildFileNames, ","), "comma-separated list of valid build file names")
}

func (cr *Configurer) CheckFlags(_ *flag.FlagSet, c *config.Config) error {
	if cr.cliBuildFileNames != "" {
		c.ValidBuildFileNames = strings.Split(cr.cliBuildFileNames, ",")
	}
	ignoreFilter := newIgnoreFilter(c.RepoRoot)
	c.Exts[walkName] = &walkConfig{
		ignoreFilter:        ignoreFilter,
		excludes:            append([]string(nil), cr.cliExcludes...),
		validBuildFileNames: append([]string(nil), c.ValidBuildFileNames...),
	}
	return nil
}

func (*Configurer) KnownDirectives() []string {
	return []string{"build_file_name", "directive_file", "generation_mode", "exclude", "follow", "ignore"}
}

func (*Configurer) Configure(c *config.Config, rel string, f *rule.File) {
	parent := getWalkConfig(c)
	if parent == nil {
		parent = &walkConfig{
			ignoreFilter:        newIgnoreFilter(c.RepoRoot),
			validBuildFileNames: append([]string(nil), c.ValidBuildFileNames...),
		}
	}
	c.Exts[walkName] = configureForWalk(parent, rel, f)
	c.ValidBuildFileNames = getWalkConfig(c).validBuildFileNames
}

func configureForWalk(parent *walkConfig, rel string, f *rule.File) *walkConfig {
	wc := parent.clone()
	wc.ignore = false
	if f == nil {
		return wc
	}
	for _, d := range f.Directives {
		switch d.Key {
		case "build_file_name":
			wc.validBuildFileNames = strings.Split(d.Value, ",")
		case "generation_mode":
			switch generationModeType(strings.TrimSpace(d.Value)) {
			case generationModeUpdate, generationModeCreate:
				log.Printf("//%s: gazelle:generation_mode is not supported in v3 and will be ignored", f.Pkg)
			default:
				log.Printf("unknown generation_mode %q in //%s", d.Value, f.Pkg)
			}
		case "exclude":
			p := path.Join(rel, d.Value)
			if err := checkPathMatchPattern(p); err != nil {
				log.Printf("the exclusion pattern is not valid %q: %s", p, err)
				continue
			}
			wc.excludes = append(wc.excludes, p)
		case "follow":
			log.Printf("//%s: gazelle:follow is not supported in v3 and will be ignored", f.Pkg)
		case "ignore":
			if d.Value != "" {
				log.Printf("the ignore directive does not take any arguments. Did you mean gazelle:exclude? in //%s '# gazelle:ignore %s'", f.Pkg, d.Value)
			}
			wc.ignore = true
		}
	}
	return wc
}

type ignoreFilter struct {
	ignoreDirectoryGlobs []string
	ignorePaths          map[string]struct{}
}

func newIgnoreFilter(repoRoot string) *ignoreFilter {
	bazelignorePaths, err := loadBazelIgnore(repoRoot)
	if err != nil {
		log.Printf("error loading .bazelignore: %v", err)
	}
	repoDirectoryIgnores, err := loadRepoDirectoryIgnore(repoRoot)
	if err != nil {
		log.Printf("error loading REPO.bazel ignore_directories(): %v", err)
	}
	return &ignoreFilter{
		ignorePaths:          bazelignorePaths,
		ignoreDirectoryGlobs: repoDirectoryIgnores,
	}
}

func (f *ignoreFilter) isDirectoryIgnored(p string) bool {
	if _, ok := f.ignorePaths[p]; ok {
		return true
	}
	return matchAnyGlob(f.ignoreDirectoryGlobs, p)
}

func (f *ignoreFilter) isFileIgnored(p string) bool {
	_, ok := f.ignorePaths[p]
	return ok
}

func loadBazelIgnore(repoRoot string) (map[string]struct{}, error) {
	file, err := filepath.Abs(filepath.Join(repoRoot, ".bazelignore"))
	if err != nil {
		return nil, err
	}
	f, err := os.Open(file)
	if errors.Is(err, os.ErrNotExist) {
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
	if errors.Is(err, os.ErrNotExist) {
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
			if err := checkPathMatchPattern(strExpr.Value); err != nil {
				log.Printf("the ignore_directories() pattern %q is not valid: %s", strExpr.Value, err)
				continue
			}
			ignoreDirectories = append(ignoreDirectories, strExpr.Value)
		}
		break
	}
	return ignoreDirectories, nil
}

func checkPathMatchPattern(pattern string) error {
	_, err := doublestar.Match(pattern, "x")
	return err
}

func matchAnyGlob(patterns []string, p string) bool {
	for _, x := range patterns {
		if doublestar.MatchUnvalidated(x, p) {
			return true
		}
	}
	return false
}
