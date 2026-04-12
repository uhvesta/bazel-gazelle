package main

import (
	vfsgazellelanguage "github.com/uhvesta/bazel-gazelle/vfsgazelle/language"
	golang "github.com/uhvesta/bazel-gazelle/vfsgazelle/language/go"
	"github.com/uhvesta/bazel-gazelle/vfsgazelle/language/proto"
)

var languages = []vfsgazellelanguage.Language{
	proto.NewLanguage(),
	golang.NewLanguage(),
}
