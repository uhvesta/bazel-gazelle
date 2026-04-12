package proto

import (
	"testing"

	"github.com/uhvesta/bazel-gazelle/v3/internal/vfs"
)

func TestRegisterParsers(t *testing.T) {
	reg := vfs.NewRegistry()
	lang := NewLanguage()
	if err := lang.RegisterParsers(reg); err != nil {
		t.Fatal(err)
	}
	parsers := reg.Match("foo.proto")
	if len(parsers) != 1 || parsers[0].Key() != "proto/fileinfo" {
		t.Fatalf("got parsers %#v", parsers)
	}
}
