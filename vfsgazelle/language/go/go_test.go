package golang

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
	parsers := reg.Match("foo.go")
	if len(parsers) != 1 || parsers[0].Key() != "go/fileinfo" {
		t.Fatalf("got parsers %#v", parsers)
	}
}

func TestApparentLoads(t *testing.T) {
	lang := NewLanguage().(*goLang)

	got := lang.ApparentLoads(func(module string) string {
		switch module {
		case "rules_go":
			return "custom_rules_go"
		case "gazelle":
			return "custom_gazelle"
		default:
			return ""
		}
	})

	want := []rule.LoadInfo{
		{
			Name: "@custom_rules_go//go:def.bzl",
			Symbols: []string{
				"cgo_library",
				"go_binary",
				"go_library",
				"go_prefix",
				"go_repository",
				"go_test",
				"go_tool_library",
			},
		},
		{
			Name: "@custom_rules_go//proto:def.bzl",
			Symbols: []string{
				"go_grpc_library",
				"go_proto_library",
			},
		},
		{
			Name: "@custom_gazelle//:deps.bzl",
			Symbols: []string{
				"go_repository",
			},
			After: []string{
				"go_rules_dependencies",
				"go_register_toolchains",
				"gazelle_dependencies",
			},
		},
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("ApparentLoads diff (-want +got):\n%s", diff)
	}
}
