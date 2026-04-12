package language

import (
	"fmt"

	"github.com/uhvesta/bazel-gazelle/config"
	"github.com/uhvesta/bazel-gazelle/vfsgazelle/internal/vfs"
)

// RegisterParsers registers parsers for all provided languages.
// RegisterParsers asks each language to register its parser-backed VFS models.
func RegisterParsers(reg *vfs.Registry, languages []Language) error {
	for _, lang := range languages {
		if err := lang.RegisterParsers(reg); err != nil {
			return fmt.Errorf("register parsers for %s: %w", lang.Name(), err)
		}
	}
	return nil
}

// Filter returns the subset of languages enabled by Config.Langs.
// Filter returns the languages enabled for the current package config.
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
