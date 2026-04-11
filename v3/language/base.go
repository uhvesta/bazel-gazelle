package language

import (
	"flag"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/label"
	"github.com/bazelbuild/bazel-gazelle/resolve"
	"github.com/bazelbuild/bazel-gazelle/rule"
	"github.com/bazelbuild/bazel-gazelle/v3/internal/vfs"
)

// BaseLang implements the minimum of the v3 language.Language interface.
//
// This is intended for composition by downstream v3 languages so they can
// implement the methods they care about incrementally.
type BaseLang struct{}

func (b *BaseLang) RegisterFlags(fs *flag.FlagSet, cmd string, c *config.Config) {}

func (b *BaseLang) CheckFlags(fs *flag.FlagSet, c *config.Config) error {
	return nil
}

func (b *BaseLang) KnownDirectives() []string {
	return nil
}

func (b *BaseLang) Configure(c *config.Config, rel string, f *rule.File) {}

func (b *BaseLang) Name() string {
	return "BaseLang"
}

func (b *BaseLang) RegisterParsers(reg *vfs.Registry) error {
	return nil
}

func (b *BaseLang) Kinds() map[string]rule.KindInfo {
	return nil
}

func (b *BaseLang) GenerateRules(args GenerateArgs) GenerateResult {
	return GenerateResult{}
}

func (b *BaseLang) Loads() []rule.LoadInfo {
	return nil
}

func (b *BaseLang) Fix(c *config.Config, f *rule.File) {}

func (b *BaseLang) Imports(args ImportsArgs) []resolve.ImportSpec {
	return nil
}

func (b *BaseLang) Embeds(args EmbedsArgs) []label.Label {
	return nil
}

func (b *BaseLang) Resolve(args ResolveArgs) {}

func (b *BaseLang) CrossResolve(args CrossResolveArgs) []resolve.FindResult {
	return nil
}

var _ Language = (*BaseLang)(nil)
