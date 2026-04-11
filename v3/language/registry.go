package language

import (
	"fmt"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/v3/internal/vfs"
)

// RegisterParsers registers parsers for all provided languages.
func RegisterParsers(reg *vfs.Registry, languages []Language) error {
	for _, lang := range languages {
		if err := lang.RegisterParsers(reg); err != nil {
			return fmt.Errorf("register parsers for %s: %w", lang.Name(), err)
		}
	}
	return nil
}

// Filter returns the subset of languages enabled by Config.Langs.
func Filter(c *config.Config, languages []Language) []Language {
	if c == nil || len(c.Langs) == 0 {
		return append([]Language(nil), languages...)
	}
	allowed := make(map[string]struct{}, len(c.Langs))
	for _, lang := range c.Langs {
		allowed[lang] = struct{}{}
	}
	var filtered []Language
	for _, lang := range languages {
		if _, ok := allowed[lang.Name()]; ok {
			filtered = append(filtered, lang)
		}
	}
	return filtered
}
