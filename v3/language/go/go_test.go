package golang

import (
	"testing"

	"github.com/bazelbuild/bazel-gazelle/v3/internal/vfs"
)

func TestRegisterParsers(t *testing.T) {
	reg := vfs.NewRegistry()
	lang := NewLanguage()
	if err := lang.RegisterParsers(reg); err != nil {
		t.Fatal(err)
	}
	parsers := reg.Match("foo.go")
	if len(parsers) != 1 || parsers[0].Key() != "go/fileinfo-lite" {
		t.Fatalf("got parsers %#v", parsers)
	}
}
