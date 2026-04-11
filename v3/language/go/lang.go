/* Copyright 2018 The Bazel Authors. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

   http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package golang provides support for Go and Go proto rules. It generates
// go_library, go_binary, go_test, and go_proto_library rules.
//
// # Configuration
//
// Go rules support the flags -build_tags, -go_prefix, and -external.
// They also support the directives # gazelle:build_tags, # gazelle:prefix,
// and # gazelle:importmap_prefix. See
// https://github.com/bazelbuild/bazel-gazelle/blob/master/README.rst#directives
// for information on these.
//
// # Rule generation
//
// Currently, Gazelle generates rules for one Go package per directory. In
// general, we aim to support Go code which is compatible with "go build". If
// there are no buildable packages, Gazelle will delete existing rules with
// default names. If there are multiple packages, Gazelle will pick one that
// matches the directory name or will print an error if no such package is
// found.
//
// Gazelle names library and test rules somewhat oddly: go_default_library, and
// go_default_test. This is for historic reasons: before the importpath
// attribute was mandatory, import paths were inferred from label names. Even if
// we never support multiple packages in the future (we should), we should
// migrate away from this because it's surprising. Libraries should generally
// be named after their directories.
//
// # Dependency resolution
//
// Go libraries are indexed by their importpath attribute. Gazelle attempts to
// resolve libraries by import path using the index, filtered using the
// vendoring algorithm. If an import doesn't match any known library, Gazelle
// guesses a name for it, locally (if the import path is under the current
// prefix), or in an external repository or vendor directory (depending
// on external mode).
//
// Gazelle has special cases for import paths associated with proto Well
// Known Types and Google APIs. rules_go declares canonical rules for these.
package golang

import (
	"encoding/json"
	"go/parser"
	"go/token"
	"strings"

	"golang.org/x/mod/modfile"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/rule"
	"github.com/bazelbuild/bazel-gazelle/v3/internal/vfs"
	v3language "github.com/bazelbuild/bazel-gazelle/v3/language"
)

const goName = "go"

type goLang struct {
	// goPkgRels is a set of relative paths to directories containing buildable
	// Go code. If the value is false, it means the directory does not contain
	// buildable Go code, but it has a subdir which does.
	goPkgRels map[string]bool
}

func (*goLang) Name() string { return goName }

func NewLanguage() v3language.Language {
	return &goLang{goPkgRels: make(map[string]bool)}
}

func (*goLang) RegisterParsers(reg *vfs.Registry) error {
	if err := reg.Register(goFileParser{}, vfs.MatchExtension(".go")); err != nil {
		return err
	}
	return reg.Register(goModParser{}, vfs.MatchBasename("go.mod"))
}

func (gl *goLang) ConfigureRepo(c *config.Config, repo *vfs.Snapshot, rel string, f *rule.File) {
	withRepo(repo, func() {
		gl.Configure(c, rel, f)
	})
}

type goParsedFile struct {
	Package string   `json:"package"`
	Imports []string `json:"imports"`
}

type goFileParser struct{}

func (goFileParser) Key() string     { return "go/fileinfo-lite" }
func (goFileParser) Version() string { return "v1" }
func (goFileParser) Parse(path string, data []byte) (any, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, data, parser.ImportsOnly)
	if err != nil {
		return goParsedFile{}, nil
	}
	model := goParsedFile{Package: file.Name.Name}
	for _, imp := range file.Imports {
		model.Imports = append(model.Imports, strings.Trim(imp.Path.Value, `"`))
	}
	return model, nil
}
func (goFileParser) Encode(model any) ([]byte, error) { return json.Marshal(model) }
func (goFileParser) Decode(data []byte) (any, error) {
	var model goParsedFile
	err := json.Unmarshal(data, &model)
	return model, err
}

type goModInfo struct {
	ModulePath string `json:"module_path"`
}

type goModParser struct{}

func (goModParser) Key() string     { return "go/modfile" }
func (goModParser) Version() string { return "v1" }
func (goModParser) Parse(path string, data []byte) (any, error) {
	model := goModInfo{}
	file, err := modfile.ParseLax(path, data, nil)
	if err != nil {
		return model, err
	}
	if file.Module != nil {
		model.ModulePath = file.Module.Mod.Path
	}
	return model, nil
}
func (goModParser) Encode(model any) ([]byte, error) { return json.Marshal(model) }
func (goModParser) Decode(data []byte) (any, error) {
	var model goModInfo
	err := json.Unmarshal(data, &model)
	return model, err
}
