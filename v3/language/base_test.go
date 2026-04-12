package language

import (
	"flag"
	"testing"

	"github.com/uhvesta/bazel-gazelle/config"
)

func TestBaseLangImplementsLanguage(t *testing.T) {
	var l Language = &BaseLang{}
	if l.Name() != "BaseLang" {
		t.Fatalf("Name() = %q, want %q", l.Name(), "BaseLang")
	}
}

func TestBaseLangConfigurerDefaults(t *testing.T) {
	lang := &BaseLang{}
	c := config.New()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)

	lang.RegisterFlags(fs, "fix", c)
	if err := lang.CheckFlags(fs, c); err != nil {
		t.Fatalf("CheckFlags() error = %v", err)
	}
	if got := lang.KnownDirectives(); got != nil {
		t.Fatalf("KnownDirectives() = %#v, want nil", got)
	}
}
