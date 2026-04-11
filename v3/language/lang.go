package language

import (
	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/label"
	"github.com/bazelbuild/bazel-gazelle/resolve"
	"github.com/bazelbuild/bazel-gazelle/rule"
	"github.com/bazelbuild/bazel-gazelle/v3/internal/vfs"
)

// Language describes a VFS-native Gazelle language for the v3 pipeline.
//
// The intended lifecycle is:
//  1. languages register parsers against a mutable build snapshot
//  2. the v3 runner freezes the snapshot
//  3. GenerateRules, Imports, and Resolve run against the frozen snapshot
type Language interface {
	config.Configurer

	Name() string

	// RegisterParsers registers the file parsers a language needs with the VFS.
	RegisterParsers(reg *vfs.Registry) error

	Kinds() map[string]rule.KindInfo

	GenerateRules(args GenerateArgs) GenerateResult
	Loads() []rule.LoadInfo
	Fix(c *config.Config, f *rule.File)

	Imports(args ImportsArgs) []resolve.ImportSpec
	Embeds(args EmbedsArgs) []label.Label
	Resolve(args ResolveArgs)
}

// CrossResolver is an optional extension point for languages that need to
// influence dependency resolution for other languages.
type CrossResolver interface {
	CrossResolve(args CrossResolveArgs) []resolve.FindResult
}

// VFSConfigurer is an optional extension point for configurers or languages
// that need access to the frozen repo snapshot during Configure.
type VFSConfigurer interface {
	ConfigureRepo(c *config.Config, repo *vfs.Snapshot, rel string, f *rule.File)
}

// GenerateArgs contains arguments for Language.GenerateRules.
type GenerateArgs struct {
	Config     *config.Config
	Repo       *vfs.Snapshot
	PackageDir *vfs.Dir
	Dir        string
	Rel        string
	File       *rule.File
	GenFiles   []string
	OtherEmpty []*rule.Rule
	OtherGen   []*rule.Rule
}

// GenerateResult contains return values for Language.GenerateRules.
type GenerateResult struct {
	Gen         []*rule.Rule
	Empty       []*rule.Rule
	Imports     []interface{}
	RelsToIndex []string
}

// ImportsArgs contains arguments for Language.Imports.
type ImportsArgs struct {
	Config *config.Config
	Repo   *vfs.Snapshot
	Rule   *rule.Rule
	File   *rule.File
}

// EmbedsArgs contains arguments for Language.Embeds.
type EmbedsArgs struct {
	Repo *vfs.Snapshot
	Rule *rule.Rule
	From label.Label
}

// ResolveArgs contains arguments for Language.Resolve.
type ResolveArgs struct {
	Config  *config.Config
	Repo    *vfs.Snapshot
	Index   *resolve.RuleIndex
	Rule    *rule.Rule
	Imports interface{}
	From    label.Label
}

// CrossResolveArgs contains arguments for CrossResolver.CrossResolve.
type CrossResolveArgs struct {
	Config *config.Config
	Repo   *vfs.Snapshot
	Index  *resolve.RuleIndex
	Import resolve.ImportSpec
	Lang   string
}
