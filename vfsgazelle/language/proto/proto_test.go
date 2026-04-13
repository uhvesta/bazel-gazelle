package proto

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/uhvesta/bazel-gazelle/rule"
	"github.com/uhvesta/bazel-gazelle/vfsgazelle/internal/vfs"
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

func TestApparentLoads(t *testing.T) {
	lang := NewLanguage().(*protoLang)

	got := lang.ApparentLoads(func(module string) string {
		if module == "protobuf" {
			return "custom_protobuf"
		}
		return ""
	})

	want := []rule.LoadInfo{
		{
			Name: "@custom_protobuf//bazel:proto_library.bzl",
			Symbols: []string{
				"proto_library",
			},
		},
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("ApparentLoads diff (-want +got):\n%s", diff)
	}
}
